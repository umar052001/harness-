package router

import (
	"math"
	"strings"
	"testing"
)

func TestCheaperToolWinsWhenEquallyReliable(t *testing.T) {
	// Both tools are unproven, so they carry the same flat prior and the
	// decision falls to cost. This is the claim the whole system rests on:
	// same quality, less money.
	r := New(
		Tool{Name: "expensive", Cost: 0.40},
		Tool{Name: "cheap", Cost: 0.02},
	)
	d, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != "cheap" {
		t.Errorf("chose %q, want cheap", d.Tool)
	}
	if len(d.Considered) != 2 {
		t.Errorf("considered %d tools, want 2", len(d.Considered))
	}
}

func TestReliabilityBeatsCostOnceKnown(t *testing.T) {
	r := New(
		Tool{Name: "cheap", Cost: 0.02},
		Tool{Name: "reliable", Cost: 0.15},
	)
	// The cheap tool is unreliable, which only becomes visible through
	// history. Before any evidence, cost wins; that is correct.
	if d, _ := r.Choose(1.0); d.Tool != "cheap" {
		t.Fatalf("with no history, want cheap, got %q", d.Tool)
	}
	for i := 0; i < 40; i++ {
		if err := r.Record("cheap", false, 0.02); err != nil {
			t.Fatal(err)
		}
		if err := r.Record("reliable", true, 0.15); err != nil {
			t.Fatal(err)
		}
	}
	d, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != "reliable" {
		t.Errorf("after evidence, want reliable, got %q (%s)", d.Tool, d.Reason)
	}
	// It must have paid for itself: the score difference should exceed the
	// cost gap, otherwise the switch is not justified.
	reliable, cheap := d.Considered[0], d.Considered[1]
	if reliable.Score-cheap.Score <= 0.15-0.02 {
		t.Errorf("switch not justified: %s vs %s", reliable.Name, cheap.Name)
	}
}

func TestHigherValueTaskJustifiesCostlierTool(t *testing.T) {
	// Reliability comes from each tool's own declared track record, which is how a
	// tool with no run history yet is fairly judged.
	r := New(
		Tool{Name: "cheap", Cost: 0.01, PriorSuccess: 85, PriorFailure: 15},
		Tool{Name: "precise", Cost: 0.60, PriorSuccess: 99, PriorFailure: 1},
	)
	// A low-stakes task: the extra precision is not worth 60x the cost.
	if d, _ := r.Choose(1.0); d.Tool != "cheap" {
		d2, _ := r.Choose(1.0)
		t.Errorf("for a low-value task, want cheap, got %q (%s)", d2.Tool, d2.Reason)
	}
	// A high-stakes task: paying for reliability is correct. This is the reason
	// the reward is an input rather than a hard-coded preference, and it is what
	// "better results for the same budget" means in practice.
	if d, _ := r.Choose(5.0); d.Tool != "precise" {
		d2, _ := r.Choose(5.0)
		t.Errorf("for a high-value task, want precise, got %q (%s)", d2.Tool, d2.Reason)
	}
}

func TestDecisionIsDeterministic(t *testing.T) {
	r := New(
		Tool{Name: "a", Cost: 0.10},
		Tool{Name: "b", Cost: 0.10},
		Tool{Name: "c", Cost: 0.10},
	)
	first, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		d, err := r.Choose(1.0)
		if err != nil {
			t.Fatal(err)
		}
		if d.Tool != first.Tool {
			t.Fatalf("decision changed between identical calls: %q then %q", first.Tool, d.Tool)
		}
	}
	if first.Tool != "a" {
		t.Errorf("tie should break by name, got %q", first.Tool)
	}
}

// The lower bound exists to stop a tool looking reliable on a handful of lucky
// results. This is the mechanism, tested where it actually bites.
func TestLowerBoundProtectsAgainstSmallSamples(t *testing.T) {
	r := New(
		Tool{Name: "lucky", Cost: 0.01},
		Tool{Name: "proven", Cost: 0.05},
	)
	// 9 of 10 is a 90% hit rate, but on ten observations that is not enough to
	// bet on when the alternative has a real record.
	for i := 0; i < 9; i++ {
		if err := r.Record("lucky", true, 0.01); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Record("lucky", false, 0.01); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if err := r.Record("proven", true, 0.05); err != nil {
			t.Fatal(err)
		}
	}

	d, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != "proven" {
		t.Errorf("want proven, got %q (%s)", d.Tool, d.Reason)
	}
	for _, c := range d.Considered {
		if c.Name == "lucky" && c.LowerBound >= 0.9 {
			t.Errorf("lucky lower bound %.4f is not penalising the small sample", c.LowerBound)
		}
	}
}

// The opposite guard. A long, near-clean record must not be thrown away because
// of a single failure, or the router would oscillate and never settle on the
// cheap option that is in fact working.
func TestLongRecordIsNotOverturnedByOneFailure(t *testing.T) {
	r := New(
		Tool{Name: "usually-fine", Cost: 0.01},
		Tool{Name: "steady", Cost: 0.05},
	)
	for i := 0; i < 200; i++ {
		_ = r.Record("usually-fine", true, 0.01)
		_ = r.Record("steady", true, 0.05)
	}
	_ = r.Record("usually-fine", false, 0.01)

	d, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != "usually-fine" {
		t.Errorf("one failure in 201 should not cost a 200-record tool its place, got %q", d.Tool)
	}
	for _, c := range d.Considered {
		if c.Name == "usually-fine" && c.LowerBound < 0.95 {
			t.Errorf("lower bound %.4f over-discounted 200 successes", c.LowerBound)
		}
	}
}

func TestStatsAccumulate(t *testing.T) {
	r := New(Tool{Name: "t", Cost: 1})
	if err := r.Record("t", true, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := r.Record("t", false, 0.5); err != nil {
		t.Fatal(err)
	}
	s, ok := r.Stats("t")
	if !ok {
		t.Fatal("no stats for t")
	}
	if s.Successes != 1 || s.Failures != 1 || s.Spend != 1.0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	r := New(Tool{Name: "t"})
	if err := r.Record("other", true, 1); err == nil {
		t.Error("expected recording an unknown tool to fail")
	}
}

func TestEmptyRouterIsAnError(t *testing.T) {
	if _, err := New().Choose(1); err == nil {
		t.Error("expected an error from a router with no tools")
	}
}

func TestBetaLowerBoundIsSane(t *testing.T) {
	// No observations: a flat prior should sit low, not at 0.5 with no spread.
	if lb := betaLowerBound(1, 1, 0.9); lb < 0 || lb > 0.2 {
		t.Errorf("flat prior lower bound = %v, want a small value", lb)
	}
	// Many successes should approach the success rate without exceeding it.
	if lb := betaLowerBound(901, 101, 0.9); lb <= 0.8 || lb >= 0.91 {
		t.Errorf("lower bound for 900/1000 = %v, want between 0.8 and 0.91", lb)
	}
	// All failures should reach zero, never go negative.
	if lb := betaLowerBound(1, 51, 0.9); lb != 0 {
		t.Errorf("lower bound after all failures = %v, want 0", lb)
	}
	// Degenerate input must not produce NaN, which would break every comparison.
	for _, c := range [][2]float64{{0, 0}, {1, 0}, {0, 1}} {
		if lb := betaLowerBound(c[0], c[1], 0.9); math.IsNaN(lb) {
			t.Errorf("NaN for alpha,beta = %v", c)
		}
	}
}

func TestNormalQuantileKnownValues(t *testing.T) {
	// Spot-check against published quantiles; a wrong z would quietly shift
	// every lower bound in the system.
	cases := map[float64]float64{0.90: 1.2816, 0.95: 1.6449, 0.99: 2.3263}
	for p, want := range cases {
		got := normalQuantile(p)
		if math.Abs(got-want) > 1e-3 {
			t.Errorf("normalQuantile(%.2f) = %.4f, want ~%.4f", p, got, want)
		}
	}
	if normalQuantile(0) != 0 || normalQuantile(1) != 0 {
		t.Error("out-of-range probabilities should return 0")
	}
}

func TestConfidenceMustBeSane(t *testing.T) {
	r := New(Tool{Name: "t"})
	if r.WithConfidence(0.2) != r || r.WithConfidence(1.5) != r {
		t.Error("out-of-range confidence should be ignored, not applied")
	}
}

// The floor is the mechanism that stops "cheapest always wins". This is the
// behaviour the whole cost claim depends on.
func TestQualityFloorExcludesTheCheapToolWhenStakesAreHigh(t *testing.T) {
	r := New(
		Tool{Name: "cheap", Cost: 0.01, PriorSuccess: 85, PriorFailure: 15},
		Tool{Name: "precise", Cost: 0.60, PriorSuccess: 99, PriorFailure: 1},
	)

	low, err := r.Choose(1.0)
	if err != nil {
		t.Fatal(err)
	}
	if low.Tool != "cheap" {
		t.Errorf("routine task should use the cheap tool, got %q (%s)", low.Tool, low.Reason)
	}
	if !strings.Contains(low.Reason, "clears the") {
		t.Errorf("reason should record that the floor was met, got %q", low.Reason)
	}

	high, err := r.Choose(9.0)
	if err != nil {
		t.Fatal(err)
	}
	if high.Tool != "precise" {
		t.Errorf("consequential task should use the precise tool, got %q (%s)", high.Tool, high.Reason)
	}
	// The expensive tool must have been chosen because it cleared the bar, not
	// despite failing it.
	var precise Candidate
	for _, c := range high.Considered {
		if c.Name == "precise" {
			precise = c
		}
	}
	if !precise.MeetsFloor {
		t.Error("the chosen tool should have cleared the quality floor")
	}
}

func TestRequirementRisesWithTaskValue(t *testing.T) {
	r := New(Tool{Name: "t"})
	prev := 0.0
	for _, reward := range []float64{0.5, 1, 2, 4, 8, 12, 50, 1000} {
		got := r.Requirement(reward)
		if got < prev {
			t.Errorf("requirement fell as stakes rose: %.3f at %v after %.3f", got, reward, prev)
		}
		prev = got
	}
	if r.Requirement(1000) > 0.98+1e-9 {
		t.Errorf("requirement must saturate, got %.3f", r.Requirement(1000))
	}
	if r.Requirement(0.001) < 0.70-1e-9 {
		t.Errorf("a trivial task should need almost nothing, got %.3f", r.Requirement(0.001))
	}
}

// If nothing is good enough, the router must still act, and must say that it
// could not meet its own bar rather than quietly proceeding.
func TestFallsBackWhenNothingClearsTheFloor(t *testing.T) {
	r := New(
		Tool{Name: "mediocre", Cost: 0.01, PriorSuccess: 50, PriorFailure: 50},
	)
	d, err := r.Choose(50.0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Tool != "mediocre" {
		t.Errorf("expected a fallback rather than no action, got %q", d.Tool)
	}
	if !strings.Contains(d.Reason, "WARNING") {
		t.Errorf("the reason must flag that no tool met the bar, got %q", d.Reason)
	}
}
