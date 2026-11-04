package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

func TestLocalHandlerRejectsForeignHosts(t *testing.T) {
	h := LocalHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		host string
		want int
	}{
		{"127.0.0.1:8091", 204}, {"[::1]:8091", 204}, {"localhost:8091", 204},
		{"abcdef123456.localhost:8091", 204}, {"evil.test:8091", 403},
		{"localhost.evil.test", 403}, {"evil.localhost:8091", 403}, {"0.0.0.0:8091", 403},
	} {
		t.Run(tc.host, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/runs", nil)
			r.Host = tc.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d", w.Code, tc.want)
			}
		})
	}
}

type completedRunner struct{}

func (completedRunner) Run(context.Context, runner.Request, func(runner.Event)) error { return nil }
func (completedRunner) Registry() *harness.Registry                                   { return harness.Load() }

func TestFailedOutcomeWriteCannotReportSuccess(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.Root(), id+".json.tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: store, run: completedRunner{}}
	w := httptest.NewRecorder()
	s.executeJob(t.Context(), runner.Request{}, id, RunMetadata{}, func([]byte) {}, &sseSink{w: w, flusher: w})
	if text := w.Body.String(); !strings.Contains(text, `"ok":false`) || strings.Contains(text, `"ok":true`) || !strings.Contains(text, "save run outcome") {
		t.Fatalf("incorrect terminal result: %s", text)
	}
}

func TestUnavailableStorePublishedHost(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "http://abcdef123456.localhost/", nil)
	w := httptest.NewRecorder()
	if !s.publishedSite(w, r) || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; unavailable storage should not panic or serve dashboard", w.Code)
	}
}

func TestCORSAllowsServiceStop(t *testing.T) {
	s := &Server{origins: map[string]bool{"http://localhost:5173": true}}
	r := httptest.NewRequest("OPTIONS", "/api/workspace/abcdef123456/app", nil)
	r.Header.Set("Origin", "http://localhost:5173")
	r.Header.Set("Access-Control-Request-Method", "DELETE")
	w := httptest.NewRecorder()
	s.withCORS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("preflight reached routes") })).ServeHTTP(w, r)
	if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "DELETE") {
		t.Fatalf("stop preflight: %d %v", w.Code, w.Header())
	}
}

func TestPromptSummaryKeepsUnicode(t *testing.T) {
	got := elide("Build 世界 app", 8)
	if !utf8.ValidString(got) || got != "Build 世界…" {
		t.Fatalf("summary=%q", got)
	}
}

func TestWorkspaceAliasCannotExposeAgentState(t *testing.T) {
	store, err := NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, dir, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".home"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".home", "credential.json"), []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".home/credential.json", filepath.Join(dir, "public.json")); err != nil {
		t.Fatal(err)
	}
	f, _, err := store.Open(id, "public.json")
	if err == nil {
		f.Close()
		t.Fatal("alias exposed private agent state")
	}
}
