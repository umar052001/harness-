// Package harness describes the coding agents the control plane can drive.
//
// The design constraint is that adding a harness must not require writing Go.
// Every agent is declared as a manifest: a command to run, how to speak to it,
// and what it costs. Two dialects are supported because real agents split along
// that line and no more:
//
//   - KindACP: the agent speaks the Agent Client Protocol over stdio, which
//     OpenCode, Gemini CLI, KiloCode and goose all do. One client implementation
//     covers every agent that has ever implemented the protocol, including ones
//     written after this file.
//   - KindCLI: the agent reads a prompt and writes to stdout. Anything with a
//     non-interactive flag works, which is the long tail that has no ACP.
//
// A third dialect is deliberately absent. Rather than accumulate per-vendor
// flags, a dialect that nobody implements should be added when someone has two
// agents that genuinely need it.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is the wire dialect a harness speaks.
type Kind string

const (
	// KindACP is the Agent Client Protocol: JSON-RPC 2.0 over stdio.
	KindACP Kind = "acp"
	// KindCLI is a one-shot command that consumes a prompt and writes to stdout.
	KindCLI Kind = "cli"
)

// CostClass is a coarse hint about what a harness costs to run. It is a hint,
// not a price: the router still consults the model catalogue, and this exists so
// an operator can filter to free agents without querying a pricing service.
type CostClass string

const (
	// CostFree runs entirely on the operator's machine.
	CostFree CostClass = "free"
	// CostFreeTier has a provider-backed free allowance.
	CostFreeTier CostClass = "free-tier"
	// CostMetered bills per token or per request.
	CostMetered CostClass = "metered"
)

// ModelFlag is how a harness is told which model to use. Harnesses disagree on
// the spelling, so the registry names the intent once and the adapter expands it.
type ModelFlag struct {
	// Flag is the switch itself, for example "--model".
	Flag string `json:"flag"`
	// Joined renders one value. A space means "--model x"; "=" means "--model=x".
	// Empty renders as a repeated "--flag x" for multi-value models.
	Joined string `json:"joined,omitempty"`
	// Repeat renders one flag per value, for harnesses that take a list.
	Repeat bool `json:"repeat,omitempty"`
}

// Harness is one declared agent.
type Harness struct {
	// ID is the stable key used by the API and the frontend. Lowercase.
	ID string `json:"id"`
	// Name is what a human sees.
	Name string `json:"name"`
	// Kind is the wire dialect, one of KindACP or KindCLI.
	Kind Kind `json:"kind"`
	// Command is the executable to run. Resolved against PATH inside the sandbox.
	Command string `json:"command"`
	// Args precede any prompt, for example ["--acp"].
	Args []string `json:"args,omitempty"`
	// ModelFlags expand into argv when a model is selected. Most harnesses want
	// one flag; listing them keeps the adapter from special-casing vendors.
	ModelFlags []ModelFlag `json:"modelFlags,omitempty"`
	// Env is added to the child environment. Values are manifest data, not
	// secrets: the sandbox does not inherit the supervisor's environment, so a
	// harness needing an API key gets it from a --env grant at the call site.
	Env map[string]string `json:"env,omitempty"`
	// Cost is the coarse class.
	Cost CostClass `json:"cost"`
	// License is the agent's license, which matters when shipping a product.
	License string `json:"license,omitempty"`
	// InstalledBy is how the operator gets it. Recorded so a manifest can be
	// honest about the fact that the binary may not exist yet.
	InstalledBy string `json:"installedBy,omitempty"`
	// Notes is a short human note, shown in the UI.
	Notes string `json:"notes,omitempty"`
	// PromptFlag is where the prompt goes for KindCLI. Empty means stdin.
	PromptFlag string `json:"promptFlag,omitempty"`
	// PromptStdin sends the prompt on stdin for KindCLI instead of argv, which
	// is what most agents want: argv is visible in the process table.
	PromptStdin bool `json:"promptStdin,omitempty"`
	// NeedsNetwork is the harness's own account of what it reaches for. A cloud
	// agent has to talk to a model provider, and saying so in the manifest keeps
	// the sandbox's egress grant traceable to something a reviewer can read,
	// rather than to a hard-coded string in the runner.
	NeedsNetwork string `json:"needsNetwork,omitempty"`
}

// Validate checks a manifest. A malformed manifest is rejected at load time
// rather than at spawn time, so a bad entry cannot become a surprising runtime
// failure for a user who selected it from a dropdown.
func (h *Harness) Validate() error {
	if h.ID == "" {
		return fmt.Errorf("harness: id is required")
	}
	if h.ID != strings.ToLower(h.ID) {
		return fmt.Errorf("harness %q: id must be lowercase", h.ID)
	}
	if strings.ContainsAny(h.ID, " \t/\\") {
		return fmt.Errorf("harness %q: id must not contain spaces or path separators", h.ID)
	}
	if h.Name == "" {
		return fmt.Errorf("harness %q: name is required", h.ID)
	}
	if h.Command == "" {
		return fmt.Errorf("harness %q: command is required", h.ID)
	}
	switch h.Kind {
	case KindACP, KindCLI:
	case "":
		return fmt.Errorf("harness %q: kind is required (one of %q, %q)", h.ID, KindACP, KindCLI)
	default:
		return fmt.Errorf("harness %q: unknown kind %q (want %q or %q)", h.ID, h.Kind, KindACP, KindCLI)
	}
	switch h.Cost {
	case CostFree, CostFreeTier, CostMetered, "":
	default:
		return fmt.Errorf("harness %q: unknown cost class %q", h.ID, h.Cost)
	}
	for i, mf := range h.ModelFlags {
		if mf.Flag == "" {
			return fmt.Errorf("harness %q: modelFlags[%d] has no flag", h.ID, i)
		}
	}
	if h.Kind == KindCLI && h.PromptFlag == "" && !h.PromptStdin {
		// Not fatal: an agent that reads stdin without being told to is legal,
		// and the adapter simply does not write the prompt itself.
	}
	return nil
}

// Argv builds the command line for a prompt, applying model flags.
//
// Model values are joined into a single argument because every current agent
// takes one value per flag, and combining them is the adapter's job, not the
// caller's.
func (h *Harness) Argv(prompt string, models []string) []string {
	argv := make([]string, 0, len(h.Args)+len(h.ModelFlags)*2+1)
	argv = append(argv, h.Args...)
	for _, mf := range h.ModelFlags {
		switch {
		case mf.Repeat:
			for _, m := range models {
				argv = append(argv, mf.Flag, m)
			}
		case len(models) == 0:
			// No model requested: emit nothing rather than an empty value. An
			// empty --model is not always rejected, and an agent that accepts
			// "" may quietly select something expensive.
		case mf.Joined == "=":
			argv = append(argv, mf.Flag+"="+strings.Join(models, ","))
		case mf.Joined == " ":
			// Space-separated is still two argv entries. Folding them into one
			// string hands the harness a literal "--model gpt-4" as the flag,
			// which it will not match against anything.
			argv = append(argv, mf.Flag, strings.Join(models, ","))
		default:
			argv = append(argv, mf.Flag, strings.Join(models, ","))
		}
	}
	// The prompt is only placed on argv when the harness wants it there. A
	// PromptStdin harness gets nothing: putting the prompt in argv as well would
	// show the user's entire request in the process table, which is the reason
	// PromptStdin exists.
	if h.Kind == KindCLI && h.PromptFlag != "" {
		argv = append(argv, h.PromptFlag, prompt)
	}
	return argv
}

// Summary is the JSON shape the frontend consumes. It is a separate type so the
// UI never receives an executable path it could be tricked into rendering as
// trusted, and so internal fields can change without a UI change.
type Summary struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      Kind      `json:"kind"`
	Cost      CostClass `json:"cost"`
	License   string    `json:"license,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	Installed bool      `json:"installed"`
	// Command is reported for transparency. The frontend renders it as text.
	Command string `json:"command"`
}

// Summaries renders the registry for the API, ordered by id.
func (r *Registry) Summaries() []Summary {
	out := make([]Summary, 0, len(r.order))
	for _, id := range r.order {
		h := r.byID[id]
		s := Summary{
			ID: h.ID, Name: h.Name, Kind: h.Kind, Cost: h.Cost,
			License: h.License, Notes: h.Notes, Command: h.Command,
		}
		if p, err := lookPath(h.Command); err == nil {
			s.Installed = true
			s.Command = p
		}
		out = append(out, s)
	}
	return out
}

// lookPath is exec.LookPath, injected so tests can claim a binary exists.
var lookPath = func(name string) (string, error) { return execLookPath(name) }

// loadManifest reads and validates one manifest file.
func loadManifest(path string) (*Harness, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("harness: reading %s: %w", path, err)
	}
	var h Harness
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("harness: parsing %s: %w", path, err)
	}
	if err := h.Validate(); err != nil {
		return nil, fmt.Errorf("harness: %s: %w", filepath.Base(path), err)
	}
	return &h, nil
}

// sortedIDs is used by both the builtin and overlay loaders.
func sortedIDs(m map[string]*Harness) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
