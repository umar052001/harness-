// Package planner turns a user's request into a structured dispatch plan using
// a hosted LLM (Ollama Cloud, api.ollama.com).
//
// The plan is advice, not permission: each step names the harness that should
// run it and a rationale, and the control plane may refuse or reorder steps
// just as it would a tool call. The client here is deliberately small - it
// builds one chat completion and parses the JSON the model was asked to
// produce - because the interesting part of this system is what happens after
// the plan, not the HTTP call that makes it.
package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// DefaultBase is the Ollama Cloud OpenAI-compatible endpoint.
const DefaultBase = "https://api.ollama.com"

// Step is one unit of work in a plan.
type Step struct {
	// Harness is the harness id the step should run under.
	Harness string `json:"harness"`
	// Task is what to do, written for the agent.
	Task string `json:"task"`
	// Rationale is why this step exists, for a reviewer.
	Rationale string `json:"rationale,omitempty"`
	// DependsOn names steps that must finish first, by index.
	DependsOn []int `json:"dependsOn,omitempty"`
}

// Plan is the model's answer.
type Plan struct {
	// Summary is a one-line human summary of the approach.
	Summary string `json:"summary"`
	// Steps are ordered units of work.
	Steps []Step `json:"steps"`
}

// Planner calls the hosted model.
type Planner struct {
	// APIKey is the OLLAMA_API_KEY for api.ollama.com.
	APIKey string
	// Model is the hosted model id. Empty picks the default.
	Model string
	// Base overrides the endpoint; DefaultBase is used when empty.
	Base string
	// HTTP is the client; a zero Planner uses http.DefaultClient.
	HTTP *http.Client
}

// New builds a planner from configuration strings, tolerating an absent key:
// a planner without a key is not an error, it is simply unconfigured, and the
// API reports that state instead of failing every request.
func New(apiKey, model string) *Planner {
	return &Planner{APIKey: apiKey, Model: model}
}

// Configured reports whether a model call can actually be made.
//
// A nil Planner is unconfigured rather than a crash. It is stored in a nil-able
// field and compared against nil in several places, so "no key" has to include
// "no planner" or every caller needs its own guard.
func (p *Planner) Configured() bool { return p != nil && strings.TrimSpace(p.APIKey) != "" }

// ModelOrDefault returns the configured model or the default.
func (p *Planner) ModelOrDefault() string {
	if p.Model != "" {
		return p.Model
	}
	return "qwen3-coder:30b"
}

// systemPrompt asks the model to act as a dispatcher, not to solve the task
// itself. The control plane has the harnesses; the plan only routes.
const systemPrompt = `You are the planner of an agent dispatch system. The user has
asked for work. Produce a plan that divides that work into discrete steps.

Rules:
- Assign each step to exactly one of these harnesses: opencode, gemini, goose,
  kilocode, claude.
- Never write code or prose inside the plan yourself. Steps just describe the
  task for the assigned agent.
- Split work only when the steps are genuinely different (different concerns,
  different tools, different artifacts). One focused step is better than two
  overlapping ones.
- Between zero and three steps.
- Choose a single harness per step and write the rationale as one short sentence.

Respond only with JSON in this shape:
{"summary":"one line","steps":[{"harness":"opencode","task":"...","rationale":"...","dependsOn":[0]}]}`

// Plan asks the hosted model to route a request.
func (p *Planner) Plan(ctx context.Context, prompt string) (*Plan, error) {
	content, err := p.Complete(ctx, systemPrompt, prompt, true)
	if err != nil {
		return nil, err
	}
	var plan Plan
	if err := json.Unmarshal([]byte(stripFences(content)), &plan); err != nil {
		return nil, fmt.Errorf("planner: parse model json: %w", err)
	}
	return &plan, nil
}

// Complete performs one chat completion and returns the raw content.
//
// It is exported because the hosted model is useful for more than routing: the
// same key and endpoint also answer "what is happening right now" for the
// dashboard. Sharing the call keeps one HTTP client, one auth path and one
// error shape for everything that talks to the provider.
//
// jsonMode asks for a JSON object response; pass false for prose.
func (p *Planner) Complete(ctx context.Context, system, user string, jsonMode bool) (string, error) {
	if !p.Configured() {
		return "", ErrUnconfigured
	}
	body := map[string]any{
		"model": p.ModelOrDefault(),
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0.2,
		"stream":      false,
	}
	if jsonMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	base := p.Base
	if base == "" {
		base = DefaultBase
	}
	url := strings.TrimRight(base, "/") + "/v1/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("planner: call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("planner: api status %d", resp.StatusCode)
	}

	var chat struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&chat); err != nil {
		return "", fmt.Errorf("planner: decode: %w", err)
	}
	if len(chat.Choices) == 0 {
		return "", fmt.Errorf("planner: no choices in response")
	}
	return strings.TrimSpace(chat.Choices[0].Message.Content), nil
}

// stripFences removes a markdown code fence if the model wrapped its JSON.
//
// Models do this often enough that every JSON path needs it, and without it the
// failure mode is an unparseable response for a response that is correct.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// ErrUnconfigured is returned when no API key is available.
var ErrUnconfigured = fmt.Errorf("planner: OLLAMA_API_KEY is not set")
