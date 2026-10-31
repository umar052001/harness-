//go:build workflow_live

package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Entitlement is tested through the same isolated ACP and credential handoff as
// real builds. Catalog entries and zero subscription prices are not access proof.
func TestLiveSubscriptionProbe(t *testing.T) {
	model := os.Getenv("HARNESS_PROBE_MODEL")
	if os.Getenv("HARNESS_WORKFLOW_LIVE") != "1" || model == "" {
		t.Skip("opt in and select HARNESS_PROBE_MODEL; consumes subscription quota")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Root: root, Store: filepath.Join(home, ".local/share/harness++/validations"), Source: filepath.Join(root, "examples/counter"), Registry: filepath.Join(root, "config/models.json"), Prompt: "Read the graph then reply ONLINE without editing files or running tests. This is only an inference and plugin-access probe.", Check: []string{"true"}, AllowGPT: true, Binary: filepath.Join(root, ".build/harness-build")})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("report:", b.Dir)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if _, err := b.graph(ctx, "Counter"); err != nil {
		t.Fatal(err)
	}
	err = b.agent(ctx, Stage{TaskType: "testing", Model: model, Task: "Use graphify query_graph, then reply ONLINE. No edits or shell commands.", Reason: "operator-authorized entitlement probe", Escalation: "verify a smaller subscription candidate"}, testPlan(), "none")
	if e := b.summary(err == nil); e != nil {
		t.Error(e)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestLivePlanningFallback(t *testing.T) {
	if os.Getenv("HARNESS_WORKFLOW_LIVE") != "1" {
		t.Skip("opt in; consumes subscription quota")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(Options{Root: root, Store: filepath.Join(home, ".local/share/harness++/validations"), Source: filepath.Join(root, "examples/counter"), Registry: filepath.Join(root, "config/models.json"), Prompt: "Plan implementation of the Counter in the source, preserving its existing tests. No application runtime is needed for this Go library.", Check: []string{"go", "test", "./..."}, AllowGPT: true, Binary: filepath.Join(root, ".build/harness-build")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	graph, err := b.graph(ctx, "Counter")
	if err != nil {
		t.Fatal(err)
	}
	// A nonexistent native alias rejects before paid inference, exercising the
	// planning failure path without changing the operator's real Ollama daemon.
	for i := range b.registry.Models {
		if b.registry.Models[i].ID == b.registry.Router {
			b.registry.Models[i].ID = "ollama/harness-missing-model"
			b.registry.Models[i].ProviderModelID = "harness-missing-model"
			b.registry.Router = b.registry.Models[i].ID
			break
		}
	}
	p, err := b.plan(ctx, b.opt.Prompt, graph)
	if e := b.summary(err == nil); e != nil {
		t.Error(e)
	}
	t.Log("report:", b.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Stages {
		if s.Model == b.registry.Router {
			t.Fatal("routed back to failed planner")
		}
	}
}
