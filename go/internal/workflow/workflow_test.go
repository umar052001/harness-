package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/umar052001/ai-os/go/internal/runner"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRegistry(t *testing.T) Registry {
	t.Helper()
	r, err := LoadRegistry("../../../config/models.json")
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func testPlan() Plan {
	return Plan{Problem: "Implement Counter", Requirements: []string{"zero value"}, Constraints: []string{"standard library"}, Assumptions: []string{"single-threaded"}, AcceptanceCriteria: []string{"negative input rejected"}, CodebaseFindings: []string{"counter.go declares Counter"}, Research: []string{"not needed"}, Sequence: []string{"implement and test"}, Validation: []string{"go test ./..."}, Quality: []string{"minimal readable code"}, Stages: []Stage{{TaskType: "implementation", Task: "Implement Counter", Model: "ollama/gemma4:cloud", Reason: "small deterministic change"}}}
}
func TestPlanValidation(t *testing.T) {
	r := testRegistry(t)
	for _, tc := range []struct {
		name  string
		edit  func(*Plan)
		valid bool
	}{
		{"valid", func(*Plan) {}, true},
		{"unknown model", func(p *Plan) { p.Stages[0].Model = "invented/model" }, false},
		{"disabled model", func(p *Plan) { p.Stages[0].Model = "ollama/kimi-k2.6:cloud" }, false},
		{"unapproved GPT", func(p *Plan) { p.Stages[0].Model = "openai/gpt-6.1-sol"; p.Stages[0].Escalation = "security" }, false},
		{"missing tests", func(p *Plan) { p.Validation = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPlan()
			tc.edit(&p)
			raw, _ := json.Marshal(p)
			_, err := ParsePlan("```json\n"+string(raw)+"\n```", r, false)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}

func TestFallbackPlanUsesActualHostEscalationReason(t *testing.T) {
	p := testPlan()
	p.Stages[0].Model = "openai/gpt-6.1-sol"
	raw, _ := json.Marshal(p)
	parsed, err := parseFallbackPlan(raw, testRegistry(t), "Ollama cloud request returned HTTP 429")
	if err != nil || !strings.Contains(parsed.Stages[0].Escalation, "HTTP 429") {
		t.Fatalf("plan=%+v error=%v", parsed, err)
	}
	p.Stages[0].Model = "openai/gpt-5.5"
	raw, _ = json.Marshal(p)
	if _, err := parseFallbackPlan(raw, testRegistry(t), "HTTP 429"); err == nil {
		t.Fatal("fallback enabled a disabled model")
	}
}
func TestFallbackRespectsAccessAndDoesNotLoop(t *testing.T) {
	r := testRegistry(t)
	if got := r.Fallback(r.Router, nil, false); got != "" {
		t.Fatalf("unapproved fallback %s", got)
	}
	if got := r.Fallback(r.Router, nil, true); got != "openai/gpt-6.1-sol" {
		t.Fatalf("fallback %s", got)
	}
	if got := r.Fallback(r.Router, []string{"openai/gpt-6.1-sol"}, true); got != "" {
		t.Fatalf("repeated fallback %s", got)
	}
}

func TestQuotaFailureSkippedInLaterStages(t *testing.T) {
	b := Build{Dir: t.TempDir(), registry: testRegistry(t), opt: Options{AllowGPT: true}}
	p := testPlan()
	p.Stages = append(p.Stages, p.Stages[0])
	var models []string
	err := b.runStages(t.Context(), p, func(_ context.Context, s Stage, _ Plan, _ string, _ bool) error {
		models = append(models, s.Model)
		if s.Model == "ollama/gemma4:cloud" {
			return errors.New("provider.quota: usage limit reached")
		}
		return nil
	})
	if err != nil || strings.Join(models, ",") != "ollama/gemma4:cloud,openai/gpt-6.1-sol,openai/gpt-6.1-sol" {
		t.Fatalf("models=%v error=%v", models, err)
	}
}
func TestAccountingPreservesUnknownAndCacheSemantics(t *testing.T) {
	r := testRegistry(t)
	gemma, _ := r.Model(r.Router)
	gpt, _ := r.Model("openai/gpt-6.1-sol")
	input, output, cache := int64(100), int64(20), int64(80)
	tokens := &Tokens{Input: &input, Output: &output, CacheRead: &cache}
	a := Account(gemma, tokens, "ollama-native")
	if a.EstimatedUSD == nil || *a.EstimatedUSD < 0.0000147 || *a.EstimatedUSD > 0.0000149 {
		t.Fatalf("estimate %+v", a)
	}
	if a := Account(gemma, tokens, "opencode-acp"); a.EstimatedUSD != nil || len(a.EstimatedUSDRange) != 2 {
		t.Fatal("ACP overlap hidden")
	}
	if a := Account(gpt, tokens, "opencode-acp"); a.EstimatedUSD != nil || len(a.EstimatedUSDRange) != 0 {
		t.Fatal("subscription assumed free")
	}
	if a := Account(gemma, nil, "opencode-acp"); a.Tokens != nil || a.EstimatedUSD != nil {
		t.Fatal("missing usage became zero")
	}
}
func TestCopyDoesNotFollowSourceSymlink(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "secret")); err != nil {
		t.Fatal(err)
	}
	if err := copySource(source, dest); err == nil {
		t.Fatal("copied external symlink")
	}
}
func TestPrivateWriteCannotEscapeThroughHome(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, ".home")); err != nil {
		t.Fatal(err)
	}
	b := Build{workspace: workspace}
	if err := b.writePrivate(filepath.Join(workspace, ".home/credential.json"), "secret"); err == nil {
		t.Fatal("write escaped workspace")
	}
}
func TestSummaryDoesNotInventUsage(t *testing.T) {
	r := testRegistry(t)
	m, _ := r.Model("openai/gpt-6.1-sol")
	a := Account(m, nil, "opencode-acp")
	b := Build{Dir: t.TempDir(), records: []Record{{Phase: "implementation", Model: m.ID, Accounting: &a}}}
	if err := b.summary(false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"input": null`) || !strings.Contains(string(raw), `"unknownCostCalls": 1`) {
		t.Fatalf("misleading summary: %s", raw)
	}
	if !strings.Contains(string(raw), `"knownCostEstimateUSDRange": null`) {
		t.Fatal("unknown cost became zero")
	}
}

func TestWorkflowEventsPersistUnknownAccounting(t *testing.T) {
	var kinds []string
	b := Build{Dir: t.TempDir(), opt: Options{Emit: func(e runner.Event) { kinds = append(kinds, e.Kind) }}}
	a := Accounting{Measurement: "unknown", Billing: "subscription"}
	if err := b.record(Record{Phase: "planning", Model: "openai/gpt-5.5", Accounting: &a, Result: "complete"}); err != nil {
		t.Fatal(err)
	}
	if err := b.summary(true); err != nil {
		t.Fatal(err)
	}
	if strings.Join(kinds, ",") != "workflow.record,workflow.summary" {
		t.Fatal(kinds)
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"unknownUsageCalls": 1`) {
		t.Fatal(string(raw))
	}
}

func TestDispatchRetriesProviderAndQualityFailures(t *testing.T) {
	for _, failure := range []string{"provider request failed", "acceptance output did not meet requirements"} {
		t.Run(failure, func(t *testing.T) {
			b := Build{Dir: t.TempDir(), registry: testRegistry(t), opt: Options{AllowGPT: true}}
			var models []string
			err := b.runStages(t.Context(), testPlan(), func(_ context.Context, s Stage, _ Plan, feedback string, final bool) error {
				models = append(models, s.Model)
				if !final {
					t.Fatal("last stage lost acceptance check after fallback")
				}
				if len(models) == 1 {
					return errors.New(failure)
				}
				if feedback != failure || s.Escalation != failure {
					t.Fatal("failure evidence not passed to fallback")
				}
				return nil
			})
			if err != nil || len(models) != 2 || models[1] != "openai/gpt-6.1-sol" {
				t.Fatalf("models=%v err=%v", models, err)
			}
		})
	}
}
func TestDispatchStopsAfterBoundedAttempts(t *testing.T) {
	b := Build{Dir: t.TempDir(), registry: testRegistry(t), opt: Options{AllowGPT: true}}
	calls := 0
	err := b.runStages(t.Context(), testPlan(), func(context.Context, Stage, Plan, string, bool) error { calls++; return errors.New("fails") })
	if err == nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestAutomaticPolicyUsesGemmaThenOnlySol(t *testing.T) {
	b := Build{registry: testRegistry(t), opt: Options{AllowGPT: true}}
	p := testPlan()
	p.Stages = append(p.Stages, p.Stages[0], p.Stages[0])
	for i := range p.Stages {
		p.Stages[i].Model = "openai/gpt-6-astra"
	}
	p = b.routePlan(p)
	for i, s := range p.Stages {
		want := "openai/gpt-6.1-sol"
		if i == 0 {
			want = b.registry.Router
		}
		if s.Model != want {
			t.Fatalf("stage %d: %s", i, s.Model)
		}
	}
	if m, _ := b.registry.Model("openai/gpt-6-astra"); m.Enabled {
		t.Fatal("Astra must stay disabled")
	}
}

func TestSolRepairsAreBoundedAndQuotaIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		calls         int
	}{
		{"validation", "functional check failed", 2},
		{"quota", "provider.quota", 1},
		{"authentication", "Authentication required", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Build{Dir: t.TempDir(), registry: testRegistry(t), opt: Options{AllowGPT: true}}
			p := testPlan()
			p.Stages[0].Model = "openai/gpt-6.1-sol"
			calls := 0
			err := b.runStages(t.Context(), p, func(_ context.Context, s Stage, _ Plan, feedback string, _ bool) error {
				calls++
				if s.Model != "openai/gpt-6.1-sol" {
					t.Fatal(s.Model)
				}
				if calls > 1 && feedback != tc.message {
					t.Fatal("missing repair feedback")
				}
				return errors.New(tc.message)
			})
			if err == nil || calls != tc.calls {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}
