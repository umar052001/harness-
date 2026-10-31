package acp

import (
	"fmt"
	"strings"
)

// failureGuard stops repeated failed operations, not quiet generation or valid
// polling. It is owned by the single ACP reader goroutine.
type failureGuard struct {
	seen   map[string]bool
	counts map[string]int
}

func (g *failureGuard) observe(id string, t EventTool) error {
	if id == "" || g.seen[id] || (t.Status != "completed" && t.Status != "failed") {
		return nil
	}
	if len(g.seen) >= 128 {
		g.seen = nil
	}
	if g.seen == nil {
		g.seen = map[string]bool{}
	}
	g.seen[id] = true
	output := strings.ToLower(t.Output)
	key := ""
	switch {
	case t.Kind == "execute" && strings.Contains(output, "socket path too long"):
		key = "Chromium Unix socket path is too long"
	case t.Kind == "execute" && strings.Contains(output, "gpu process isn't usable"):
		key = "Chromium GPU process cannot start"
	case t.Status == "failed":
		key = t.Name + "\n" + t.Command + "\n" + t.Output
	default:
		// A successful edit is evidence of a changed approach. Mere log reads
		// must not hide repeated failures with the same underlying cause.
		if t.Kind == "edit" {
			g.counts = nil
		}
		return nil
	}
	if g.counts == nil {
		g.counts = map[string]int{}
	}
	// shortcut: retain 64 failure signatures, use a rolling history if varied loops occur.
	if len(g.counts) >= 64 && g.counts[key] == 0 {
		g.counts = map[string]int{}
	}
	g.counts[key]++
	if g.counts[key] >= 3 {
		return fmt.Errorf("stopped after 3 repeated tool failures without a successful edit: %s", key)
	}
	return nil
}
