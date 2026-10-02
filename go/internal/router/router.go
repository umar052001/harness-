// Package router decides which tool to use for a task.
//
// The goal is narrower than "pick the best tool": it is to get a better result
// for less money than the obvious default. That makes the decision rule
// accountable, which is why it is written out here rather than delegated to a
// model at call time.
//
// The rule is deliberately cheap. It is arithmetic over a handful of numbers, so
// it runs in microseconds and costs nothing. Putting a language model in this
// path would add latency and cost to every single tool call, and would make the
// routing decision impossible to audit or reproduce - the opposite of what is
// needed when the claim is "this saved you money".
//
// Scoring, for a task worth `reward` if it succeeds:
//
//	requirement = quality floor implied by how much the task is worth
//	eligible    = tools whose lower confidence bound clears that floor
//	score       = lower_confidence_bound(success_probability) * reward - cost
//
// The lower bound rather than the mean is the important part. A tool that has
// succeeded 9 times out of 10 is not the same bet as one that has succeeded 900
// times out of 1000, and the mean hides that. Optimising the pessimistic end
// keeps the router from chasing a tool that happened to get lucky.
//
// The quality floor is what stops the cost comparison from running away. Without
// it, cheap always wins, because a cheaper tool that is usually right is usually
// the better bet. But "usually" is not good enough for a decision that is hard to
// undo, so the floor rises with the stakes and the expensive tool is used exactly
// when the cheap one is not good enough. That is the whole claim: pay more only
// where being wrong is expensive.
package router

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// Tool is a candidate the router may choose.
type Tool struct {
	// Name identifies the tool in decisions and in the outcome log.
	Name string

	// Cost is the expected spend for one call, in whatever unit the caller is
	// tracking. Keeping the unit abstract avoids pretending to know whether this
	// is dollars, tokens or credits.
	Cost float64

	// Reward is the value of a successful call. Defaults to 1 when unset, so
	// tools that only report success can be compared on cost alone.
	Reward float64

	// PriorSuccess and PriorFailure seed the estimate for a tool with no
	// history. A flat prior of 1/1 is deliberately uninformative: it lets a new
	// tool win on cost alone until evidence arrives, then move as evidence does.
	PriorSuccess float64
	PriorFailure float64
}

// reward normalises the value of a successful call.
func (t Tool) reward() float64 {
	if t.Reward <= 0 {
		return 1
	}
	return t.Reward
}

// prior normalises the Beta parameters, defaulting to a flat prior.
func (t Tool) prior() (alpha, beta float64) {
	a, b := t.PriorSuccess, t.PriorFailure
	if a <= 0 {
		a = 1
	}
	if b <= 0 {
		b = 1
	}
	return a, b
}

// Stats is the accumulated evidence about one tool.
type Stats struct {
	Successes int
	Failures  int
	Spend     float64
}

// Decision is the router's reasoning, kept so it can be shown and challenged.
type Decision struct {
	Tool   string
	Score  float64
	Reason string

	// Every candidate and its score, so the trade-off that was made is visible
	// rather than implied.
	Considered []Candidate
}

// Candidate is one tool as it scored for this decision.
type Candidate struct {
	Name  string
	Score float64

	// LowerBound is the conservative success probability used in the score.
	LowerBound float64
	Successes  int
	Failures   int
	Cost       float64

	// MeetsFloor records whether the tool cleared the task's quality
	// requirement, so a decision can be audited without recomputing it.
	MeetsFloor bool
}

// Router selects tools and records how they went.
type Router struct {
	mu    sync.RWMutex
	tools []Tool
	stats map[string]*Stats
	// confidence controls how pessimistic the lower bound is. Higher values look
	// further into the tail, which suits a task where a wrong answer is costly.
	confidence float64

	// qualityFloor and qualityCeil bound the reliability requirement, which is
	// interpolated from how much the task is worth.
	qualityFloor float64
	qualityCeil  float64
	// qualityStart is the task value at which the requirement begins to rise,
	// and qualitySpan is how far it takes to reach the ceiling. A task worth no
	// more than qualityStart is routine and only has to clear qualityFloor.
	qualityStart float64
	qualitySpan  float64
}

// New builds a router over the given tools.
func New(tools ...Tool) *Router {
	r := &Router{
		tools:        append([]Tool(nil), tools...),
		stats:        make(map[string]*Stats, len(tools)),
		confidence:   0.90,
		qualityFloor: 0.70,
		qualityCeil:  0.98,
		// Chosen so the requirement starts moving on a small task and saturates
		// on a large one. A task worth 1 unit is routine; a task worth 8 already
		// demands a tool that can promise better than 86%.
		qualityStart: 2.0,
		qualitySpan:  10.0,
	}
	for _, t := range tools {
		if _, ok := r.stats[t.Name]; !ok {
			r.stats[t.Name] = &Stats{}
		}
	}
	return r
}

// WithConfidence sets the confidence level used for the lower bound.
//
// Must be between 0.5 and 0.999; values outside that range do not correspond to
// a usable bound and are rejected rather than silently clamped.
func (r *Router) WithConfidence(c float64) *Router {
	if c <= 0.5 || c >= 1 {
		return r
	}
	r.confidence = c
	return r
}

// Requirement returns the success probability a tool must be able to promise
// before the router will use it for a task worth `reward`.
//
// A routine task does not need much: a cheap tool that is usually right is the
// right call, and insisting on more would just spend money for no benefit. A
// consequential task needs a lot, because a wrong result there costs more than
// the tool ever would.
//
// The floor is a linear ramp between the two, capped so that no amount of value
// makes the router demand a certainty no tool can offer - a system that refuses
// to act because nothing is perfect is not more careful, it is useless.
func (r *Router) Requirement(reward float64) float64 {
	if reward <= r.qualityStart || r.qualitySpan <= 0 {
		return r.qualityFloor
	}
	t := (reward - r.qualityStart) / r.qualitySpan
	if t > 1 {
		t = 1
	}
	return r.qualityFloor + t*(r.qualityCeil-r.qualityFloor)
}

// Choose picks the highest-scoring tool that is good enough for the task.
//
// Ties break towards the cheaper tool and then by name, so the same inputs
// always produce the same decision. A router that occasionally decides
// differently for identical inputs is impossible to test or to reason about
// after the fact.
func (r *Router) Choose(reward float64) (Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.tools) == 0 {
		return Decision{}, fmt.Errorf("router: no tools registered")
	}
	if reward <= 0 {
		reward = 1
	}

	required := r.Requirement(reward)

	var considered []Candidate
	var best Candidate
	var haveBest bool
	// Tracks the best tool overall, used only if nothing clears the floor.
	var fallback Candidate
	var haveFallback bool

	for _, t := range r.tools {
		st := r.stats[t.Name]
		if st == nil {
			st = &Stats{}
		}
		alpha := t.priorAlpha() + float64(st.Successes)
		beta := t.priorBeta() + float64(st.Failures)

		lb := betaLowerBound(alpha, beta, r.confidence)
		score := lb*reward - t.Cost

		c := Candidate{
			Name:       t.Name,
			Score:      score,
			LowerBound: lb,
			Successes:  st.Successes,
			Failures:   st.Failures,
			Cost:       t.Cost,
			MeetsFloor: lb >= required,
		}
		considered = append(considered, c)

		if !haveFallback || betterThan(c, fallback) {
			fallback, haveFallback = c, true
		}
		// Only tools that clear the floor are eligible to win. Without this the
		// cheap option would take every task, which is the failure mode the floor
		// exists to prevent.
		if c.MeetsFloor && (!haveBest || betterThan(c, best)) {
			best, haveBest = c, true
		}
	}

	if !haveBest {
		// Nothing is reliable enough for a task this consequential. Using the
		// best available is still the right move - refusing to act would leave the
		// task undone - but the reason says so plainly, because silently
		// downgrading a quality bar is exactly the kind of thing that should never
		// pass unmentioned.
		best, haveBest = fallback, haveFallback
	}
	if !haveBest {
		return Decision{}, fmt.Errorf("router: no tools registered")
	}

	sort.Slice(considered, func(i, j int) bool {
		return considered[i].Score > considered[j].Score
	})

	reason := fmt.Sprintf("lower bound %.3f x reward %.2f - cost %.4f = %.4f",
		best.LowerBound, reward, best.Cost, best.Score)
	if best.Successes+best.Failures == 0 {
		reason += " (no history yet: chosen on cost and prior)"
	}
	if best.MeetsFloor {
		reason += fmt.Sprintf(" [clears the %.2f requirement for a task worth %.1f]",
			required, reward)
	} else {
		reason += fmt.Sprintf(" [WARNING: no tool reaches the %.2f requirement; "+
			"used the best available]", required)
	}

	return Decision{Tool: best.Name, Score: best.Score, Reason: reason, Considered: considered}, nil
}

// betterThan reports whether a should replace b as the winner.
func betterThan(a, b Candidate) bool {
	// A margin, so a hair's difference in an estimate built from a handful of
	// trials does not flip the decision on noise.
	const epsilon = 1e-9
	if a.Score > b.Score+epsilon {
		return true
	}
	if b.Score > a.Score+epsilon {
		return false
	}
	if a.Cost != b.Cost {
		return a.Cost < b.Cost
	}
	return a.Name < b.Name
}

// Record notes the outcome of using a tool.
//
// The result is fed back immediately, which is what makes the router adapt
// within a session instead of only across runs.
func (r *Router) Record(name string, success bool, spend float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	st, ok := r.stats[name]
	if !ok {
		return fmt.Errorf("router: unknown tool %q", name)
	}
	if success {
		st.Successes++
	} else {
		st.Failures++
	}
	st.Spend += spend
	return nil
}

// Stats returns a copy of the evidence held for a tool.
func (r *Router) Stats(name string) (Stats, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.stats[name]
	if !ok {
		return Stats{}, false
	}
	return *st, true
}

// Tools lists the registered tool names.
func (r *Router) Tools() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Name)
	}
	return out
}

func (t Tool) priorAlpha() float64 { a, _ := t.prior(); return a }
func (t Tool) priorBeta() float64  { _, b := t.prior(); return b }

// betaLowerBound returns a conservative bound on the mean of a Beta(alpha,beta).
//
// Uses the closed-form normal approximation to the Beta quantile, which is
// accurate well past the handful of observations this system reasons about and
// costs nothing to evaluate. Guarded so a degenerate prior cannot produce NaN,
// which would make every comparison false and silently degrade the router to
// "first candidate wins".
func betaLowerBound(alpha, beta, confidence float64) float64 {
	if alpha <= 0 || beta <= 0 {
		return 0
	}
	n := alpha + beta
	if n < 2 {
		return alpha / n
	}
	mean := alpha / n
	variance := (alpha * beta) / (n * n * (n + 1))
	if variance <= 0 {
		return mean
	}
	// z for a one-sided bound at the requested confidence.
	z := normalQuantile(confidence)
	bound := mean - z*math.Sqrt(variance)
	// A single failed observation should be able to push the bound to zero, but
	// never below it: a probability cannot be negative.
	return math.Max(0, math.Min(1, bound))
}

// normalQuantile is the inverse standard normal CDF, by Acklam's rational
// approximation. Accurate to about 1e-9 over the range that matters here.
func normalQuantile(p float64) float64 {
	if p <= 0 || p >= 1 {
		return 0
	}
	a := [6]float64{
		-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00,
	}
	b := [5]float64{
		-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01,
	}
	c := [6]float64{
		-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00,
	}
	d := [4]float64{
		7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00,
	}
	const plow = 0.02425
	const phigh = 1 - plow

	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > phigh:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
		(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}
