package api

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// errRunsBusy is returned when a run is refused because the server is already at
// its concurrency ceiling.
var errRunsBusy = errors.New("another run is already in flight")

// ActiveRun is a run that is currently occupying the control plane.
//
// This exists because "is something running right now?" was previously
// unanswerable. Workspaces on disk only say what has already happened, so a user
// who started a run, closed the tab and came back could not tell a long agent
// from a crashed one. The registry is the live half of that picture; the
// workspace store is the historical half.
type ActiveRun struct {
	RunID     string    `json:"runId"`
	HarnessID string    `json:"harnessId"`
	Prompt    string    `json:"prompt"`
	StartedAt time.Time `json:"startedAt"`
	Workspace string    `json:"workspace,omitempty"`
}

// runRegistry tracks in-flight runs and enforces a concurrency ceiling.
//
// The ceiling is a resource guard, not a UX preference. Every run is a sandboxed
// agent with its own package manager and node runtime; letting them stack up
// turns one slow run into a machine that has stopped responding, and the symptom
// - "ai-os sometimes works and sometimes does not" - points at the wrong layer.
// Serialising them makes the failure mode honest: the second caller is told the
// first is still going, with enough detail to wait for it.
type runRegistry struct {
	mu      sync.Mutex
	limit   int
	runs    map[string]*ActiveRun
	order   []string
	seq     int
	cancels map[string]context.CancelFunc
}

func newRunRegistry(limit int) *runRegistry {
	if limit < 1 {
		limit = 1
	}
	return &runRegistry{limit: limit, runs: map[string]*ActiveRun{}, cancels: map[string]context.CancelFunc{}}
}

// acquire reserves a slot, returning an opaque token used to release it.
//
// A token rather than the run id because the run id does not exist yet: the
// workspace has to be created after the slot is taken, or a refused run leaves
// an empty directory behind for every rejected request.
func (r *runRegistry) acquire(ar ActiveRun) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.runs) >= r.limit {
		return "", errRunsBusy
	}
	r.seq++
	token := strconv.Itoa(r.seq)
	cp := ar
	r.runs[token] = &cp
	r.order = append(r.order, token)
	return token, nil
}

// identify attaches the run id and workspace once the workspace exists.
func (r *runRegistry) identify(token, runID, workspace string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ar, ok := r.runs[token]; ok {
		ar.RunID = runID
		ar.Workspace = workspace
	}
}

// release frees a slot. It is safe to call more than once.
func (r *runRegistry) release(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.runs[token]; !ok {
		return
	}
	delete(r.runs, token)
	delete(r.cancels, token)
	for i, t := range r.order {
		if t == token {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// list returns the in-flight runs oldest first, so a caller polls the browser
// in the order they started them rather than in map order.
func (r *runRegistry) list() []ActiveRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ActiveRun, 0, len(r.runs))
	for _, t := range r.order {
		if ar, ok := r.runs[t]; ok {
			out = append(out, *ar)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// busy reports whether the ceiling is currently reached.
func (r *runRegistry) busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs) >= r.limit
}

func (r *runRegistry) setCancel(token string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[token] = cancel
}

func (r *runRegistry) cancel(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for token, run := range r.runs {
		if run.RunID == id && r.cancels[token] != nil {
			r.cancels[token]()
			return true
		}
	}
	return false
}
