package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Planning gets one authorized fallback as well as implementation; exhausting
// Ollama's allowance must not strand an otherwise usable subscription build.
func (b *Build) plan(ctx context.Context, raw, graph string) (Plan, error) {
	p, err := b.planGemma(ctx, raw, graph)
	if err == nil || ctx.Err() != nil {
		return p, err
	}
	next := b.registry.Fallback(b.registry.Router, nil, b.opt.AllowGPT)
	if next == "" {
		return p, err
	}
	// Do not route implementation back to the planner that just exhausted its
	// allowance or failed twice. This affects this build only, not the registry.
	for i := range b.registry.Models {
		if b.registry.Models[i].ID == b.registry.Router {
			b.registry.Models[i].Enabled = false
		}
	}
	if e := b.record(Record{Phase: "fallback", Model: next, Reason: err.Error(), Result: "planning fallback after Gemma failure"}); e != nil {
		return p, e
	}
	root, e := os.OpenRoot(b.workspace)
	if e != nil {
		return p, e
	}
	defer root.Close()
	const file = ".harness-plan.json"
	if e := root.Remove(file); e != nil && !os.IsNotExist(e) {
		return p, e
	}
	registry, _ := json.Marshal(b.registry)
	task := fmt.Sprintf("%s\nWrite only your JSON answer to %s. Do not implement the application during this planning turn. User request: %s\nGraphify context: %s\nEligible registry (GPT authorized): %s", planningPrompt, file, raw, graph, registry)
	stage := Stage{TaskType: "planning", Task: task, Model: next, Reason: "Gemma planning failed", Escalation: err.Error()}
	if e := b.agent(ctx, stage, Plan{Problem: raw}, err.Error()); e != nil {
		return p, e
	}
	f, e := root.Open(file)
	if e != nil {
		return p, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return p, e
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return p, errors.New("fallback plan must be a regular file under 64 KiB")
	}
	content, e := io.ReadAll(io.LimitReader(f, 65537))
	if e != nil {
		return p, e
	}
	p, e = parseFallbackPlan(content, b.registry, err.Error())
	if e != nil {
		return p, e
	}
	return p, writeJSON(b.Dir+"/plan.json", p)
}

func parseFallbackPlan(content []byte, registry Registry, failure string) (Plan, error) {
	var proposed Plan
	if err := json.Unmarshal(content, &proposed); err != nil {
		return proposed, err
	}
	// The host already has a factual escalation reason. Do not spend another
	// model turn just to have the planner repeat that reason in every GPT stage.
	for i := range proposed.Stages {
		m, ok := registry.Model(proposed.Stages[i].Model)
		if ok && m.Provider == "openai" && proposed.Stages[i].Escalation == "" {
			proposed.Stages[i].Escalation = "primary planner unavailable: " + failure
		}
	}
	raw, err := json.Marshal(proposed)
	if err != nil {
		return proposed, err
	}
	return ParsePlan(string(raw), registry, true)
}
