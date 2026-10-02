package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Registry is a set of declared harnesses.
//
// It is loaded in two layers. The built-in manifests ship in the binary, so a
// fresh clone has a working set. A directory of manifests can then be overlaid
// to add, replace, or remove entries without a rebuild, which is how a new
// agent is added: write a JSON file.
type Registry struct {
	byID  map[string]*Harness
	order []string
}

// builtins are the manifests compiled into the binary. Kept in Go rather than
// as embedded files so the defaults are reviewable in one place and cannot go
// missing from a build that forgets to copy assets.
var builtins = []Harness{
	{
		NeedsNetwork: "model provider over HTTPS",
		ID:           "opencode", Name: "OpenCode", Kind: KindACP,
		Command: "opencode", Args: []string{"acp"},
		ModelFlags: []ModelFlag{{Flag: "--model"}},
		Cost:       CostFreeTier, License: "MIT",
		InstalledBy: "npm i -g opencode-ai",
		Notes:       "Free Zen models without an API key. Best default.",
	},
	{
		NeedsNetwork: "Google Generative Language API over HTTPS",
		ID:           "gemini", Name: "Gemini CLI", Kind: KindACP,
		Command: "gemini", Args: []string{"--acp"},
		ModelFlags: []ModelFlag{{Flag: "--model"}},
		// Gemini refuses to run at all in a directory it has not been told to
		// trust, and in a sandbox every workspace is a fresh throwaway path, so
		// without this the agent hangs before its first token. The workspace is
		// disposable and kernel-confined, which is exactly the condition this
		// variable asserts.
		Env:  map[string]string{"GEMINI_CLI_TRUST_WORKSPACE": "true"},
		Cost: CostFreeTier, License: "Apache-2.0",
		InstalledBy: "npm i -g @google/gemini-cli",
		Notes:       "Generous free tier. Needs the trust flag to run headless.",
	},
	{
		NeedsNetwork: "model provider over HTTPS",
		ID:           "kilocode", Name: "KiloCode", Kind: KindACP,
		Command: "kilo", Args: []string{"acp"},
		ModelFlags: []ModelFlag{{Flag: "--model"}},
		Cost:       CostFreeTier, License: "Apache-2.0",
		InstalledBy: "npm i -g @kilocode/cli",
		Notes:       "Open-source fork lineage; many provider integrations.",
	},
	{
		NeedsNetwork: "model provider over HTTPS",
		ID:           "goose", Name: "goose", Kind: KindACP,
		Command: "goose", Args: []string{"acp"},
		ModelFlags: []ModelFlag{{Flag: "--model"}},
		Cost:       CostFree, License: "Apache-2.0",
		InstalledBy: "curl -fsSL https://github.com/block/goose/releases/download/stable/download_cli.sh | bash",
		Notes:       "Local-model friendly, 70+ extensions.",
	},
	{
		NeedsNetwork: "Anthropic API over HTTPS",
		ID:           "claude", Name: "Claude Code", Kind: KindCLI,
		Command: "claude", PromptStdin: true,
		ModelFlags: []ModelFlag{{Flag: "--model"}},
		Cost:       CostMetered, License: "Commercial",
		InstalledBy: "npm i -g @anthropic-ai/claude-code",
		Notes:       "No ACP; driven as a one-shot prompt over stdin.",
	},
}

// Load builds the default registry.
func Load() *Registry {
	r := &Registry{byID: map[string]*Harness{}}
	for i := range builtins {
		h := builtins[i]
		if err := h.Validate(); err != nil {
			// A builtin that fails validation is a programming error, and a
			// control plane that starts with a silently dropped agent is worse
			// than one that refuses to start.
			panic(fmt.Sprintf("harness: builtin manifest: %v", err))
		}
		r.byID[h.ID] = &h
	}
	r.order = sortedIDs(r.byID)
	return r
}

// OverlayDir merges every *.json manifest in dir over the registry.
//
// A manifest with the same id replaces the existing entry, which is how an
// operator points a builtin at a different command or turns a builtin off by
// replacing it with an empty command. dir may be empty, in which case the
// builtins stand alone.
func (r *Registry) OverlayDir(dir string) error {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("harness: reading overlay %s: %w", dir, err)
	}
	var added bool
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		h, err := loadManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if _, exists := r.byID[h.ID]; !exists {
			r.order = append(r.order, h.ID)
		}
		r.byID[h.ID] = h
		added = true
	}
	if added {
		sort.Strings(r.order)
	}
	return nil
}

// Lookup returns a harness by id.
func (r *Registry) Lookup(id string) (*Harness, error) {
	h, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("harness: unknown harness %q (known: %s)", id, strings.Join(r.order, ", "))
	}
	return h, nil
}

// IDs returns the registered ids in order.
func (r *Registry) IDs() []string { return append([]string(nil), r.order...) }

// Len reports how many harnesses are registered.
func (r *Registry) Len() int { return len(r.byID) }

func execLookPath(name string) (string, error) { return exec.LookPath(name) }
