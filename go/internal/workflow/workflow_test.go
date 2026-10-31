package workflow

import (
	"context"
	"encoding/json"
	"errors"
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
	if err == nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
