// Package workflow coordinates bounded, sandboxed OpenCode stages.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

type Price struct {
	Currency  string   `json:"currency"`
	Input     *float64 `json:"inputPerMillion"`
	CacheRead *float64 `json:"cacheReadPerMillion"`
	Output    *float64 `json:"outputPerMillion"`
	Billing   string   `json:"billing"`
}
type Model struct {
	ID              string            `json:"id"`
	Provider        string            `json:"provider"`
	ProviderModelID string            `json:"providerModelId"`
	Enabled         bool              `json:"enabled"`
	OpenWeights     *bool             `json:"openWeights"`
	Tasks           []string          `json:"tasks"`
	Capabilities    []string          `json:"capabilities"`
	Limitations     []string          `json:"limitations"`
	Limits          map[string]*int64 `json:"limits"`
	Priority        int               `json:"priority"`
	Fallback        []string          `json:"fallback"`
	Price           Price             `json:"price"`
}
type Registry struct {
	Version int    `json:"version"`
	Router  string `json:"router"`
	Policy  struct {
		MaxStages   int `json:"maxStages"`
		MaxAttempts int `json:"maxAttemptsPerStage"`
	} `json:"policy"`
	Models []Model `json:"models"`
}

func LoadRegistry(path string) (Registry, error) {
	var r Registry
	raw, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.Version != 1 || r.Policy.MaxStages < 1 || r.Policy.MaxStages > 3 || r.Policy.MaxAttempts < 1 || r.Policy.MaxAttempts > 3 {
		return r, errors.New("invalid registry policy")
	}
	ids := map[string]bool{}
	for _, m := range r.Models {
		if m.Price.Currency != "USD" {
			return r, errors.New("registry price currency must be USD; no implicit conversion")
		}
		for _, rate := range []*float64{m.Price.Input, m.Price.Output, m.Price.CacheRead} {
			if rate != nil && *rate < 0 {
				return r, errors.New("negative model price")
			}
		}
		if m.ID != m.Provider+"/"+m.ProviderModelID || ids[m.ID] {
			return r, errors.New("invalid or duplicate model ID")
		}
		ids[m.ID] = true
	}
	for _, m := range r.Models {
		for _, fallback := range m.Fallback {
			if !ids[fallback] {
				return r, fmt.Errorf("unregistered fallback %s", fallback)
			}
		}
	}
	if router, ok := r.Model(r.Router); !ok || !router.Enabled || router.Provider != "ollama" {
		return r, errors.New("enabled Ollama router required")
	}
	return r, nil
}
func (r Registry) Model(id string) (Model, bool) {
	for _, m := range r.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
func (r Registry) Fallback(id string, tried []string, allowGPT bool) string {
	m, _ := r.Model(id)
	for _, id := range m.Fallback {
		m, ok := r.Model(id)
		if ok && m.Enabled && !slices.Contains(tried, id) && (allowGPT || m.Provider != "openai") {
			return id
		}
	}
	return ""
}

type Stage struct {
	TaskType   string `json:"taskType"`
	Task       string `json:"task"`
	Model      string `json:"model"`
	Reason     string `json:"reason"`
	Escalation string `json:"escalation,omitempty"`
}
type Plan struct {
	Problem            string   `json:"problem"`
	Requirements       []string `json:"requirements"`
	Constraints        []string `json:"constraints"`
	Assumptions        []string `json:"assumptions"`
	AcceptanceCriteria []string `json:"acceptanceCriteria"`
	CodebaseFindings   []string `json:"codebaseFindings"`
	Research           []string `json:"research"`
	Sequence           []string `json:"sequence"`
	Validation         []string `json:"validation"`
	Quality            []string `json:"quality"`
	Stages             []Stage  `json:"stages"`
}

func ParsePlan(content string, r Registry, allowGPT bool) (Plan, error) {
	var p Plan
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```json\n") && strings.HasSuffix(content, "```") {
		content = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(content, "```json\n"), "```"))
	}
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return p, err
	}
	if p.Problem == "" || len(p.Stages) < 1 || len(p.Stages) > r.Policy.MaxStages {
		return p, errors.New("missing problem or invalid stage count")
	}
	for _, items := range [][]string{p.Requirements, p.Constraints, p.Assumptions, p.AcceptanceCriteria, p.CodebaseFindings, p.Research, p.Sequence, p.Validation, p.Quality} {
		if len(items) == 0 {
			return p, errors.New("engineering plan missing a required field")
		}
	}
	for _, s := range p.Stages {
		m, ok := r.Model(s.Model)
		if !ok || !m.Enabled || !slices.Contains(m.Tasks, s.TaskType) || s.Task == "" || s.Reason == "" {
			return p, errors.New("stage must select an enabled suitable model and explain the task and route")
		}
		if m.Provider == "openai" && (!allowGPT || s.Escalation == "") {
			return p, errors.New("GPT requires explicit permission and escalation justification")
		}
	}
	return p, nil
}
