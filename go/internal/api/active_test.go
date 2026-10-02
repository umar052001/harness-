package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

// blockingRunner is a Runner whose run parks until the test releases it, so the
// concurrency guard can be observed while a run is genuinely in flight.
type blockingRunner struct {
	reg     *harness.Registry
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRunner) Registry() *harness.Registry { return b.reg }

func (b *blockingRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) error {
	b.once.Do(func() { close(b.started) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	emit(runner.Event{Kind: runner.KindMessage, Text: "done"})
	return nil
}

// TestConcurrentRunRejected proves the second run is refused with an actionable
// 409 while the first is live, and that the active endpoint reports the first.
//
// The bug this guards: before the registry, posting twice started two agents on
// one machine. Both slowed to a crawl and the user saw "ai-os sometimes works",
// with nothing anywhere saying two runs were competing.
func TestConcurrentRunRejected(t *testing.T) {
	br := &blockingRunner{
		reg:     harness.Load(),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	srv := api.New(br, api.Options{
		WorkspacesDir:     filepath.Join(t.TempDir(), "ws"),
		MaxConcurrentRuns: 1,
	})
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)

	first, err := http.Post(s.URL+"/api/run", "application/json",
		strings.NewReader(`{"harnessId":"opencode","prompt":"first"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(br.release)
		_, _ = io.Copy(io.Discard, first.Body)
		_ = first.Body.Close()
	}()
	if first.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(first.Body)
		t.Fatalf("first run status %d: %s", first.StatusCode, b)
	}
	<-br.started

	second, err := http.Post(s.URL+"/api/run", "application/json",
		strings.NewReader(`{"harnessId":"opencode","prompt":"second"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(second.Body)
		t.Fatalf("second run status %d, want 409: %s", second.StatusCode, b)
	}
	body, _ := io.ReadAll(second.Body)
	if !strings.Contains(string(body), "already in flight") {
		t.Errorf("409 body should explain why: %s", body)
	}

	active, err := http.Get(s.URL + "/api/runs/active")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Body.Close()
	got, _ := io.ReadAll(active.Body)
	if !strings.Contains(string(got), `"harnessId":"opencode"`) {
		t.Errorf("active run missing from /api/runs/active: %s", got)
	}
	if !strings.Contains(string(got), `"prompt":"first"`) {
		t.Errorf("active run should carry its prompt: %s", got)
	}
}
