package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

func TestContinuePreservesOldVersionAndExcludesAgentState(t *testing.T) {
	s, err := api.NewWorkspaceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, dir, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"src/main.go": "original", "dist/index.html": "preview", ".env.example": "PORT=8080",
		".home/opencode.db": "private", "node_modules/package/index.js": "dependency",
	} {
		writeFile(t, filepath.Join(dir, name), data)
	}
	_, next, err := s.Continue(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"src/main.go", "dist/index.html", ".env.example"} {
		if _, err := os.Stat(filepath.Join(next, name)); err != nil {
			t.Fatalf("lost %s: %v", name, err)
		}
	}
	for _, name := range []string{".home", "node_modules"} {
		if _, err := os.Stat(filepath.Join(next, name)); !os.IsNotExist(err) {
			t.Fatalf("inherited %s: %v", name, err)
		}
	}
	writeFile(t, filepath.Join(next, "src/main.go"), "changed")
	raw, _ := os.ReadFile(filepath.Join(dir, "src/main.go"))
	if string(raw) != "original" {
		t.Fatal("continuation modified the checkpoint")
	}
}

type continuationRunner struct{ reg *harness.Registry }

func (r *continuationRunner) Registry() *harness.Registry { return r.reg }
func (r *continuationRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) error {
	path := filepath.Join(req.Workspace, "version.txt")
	if strings.Contains(req.Prompt, "second version") {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != "first" {
			return fmt.Errorf("prior source missing: %s, %v", raw, err)
		}
		return os.WriteFile(path, []byte("second"), 0o600)
	}
	return os.WriteFile(path, []byte("first"), 0o600)
}

func TestContinueAPIPreservesProjectLineage(t *testing.T) {
	server := api.New(&continuationRunner{reg: harness.Load()}, api.Options{WorkspacesDir: t.TempDir()})
	s := httptest.NewServer(server.Handler())
	defer s.Close()
	defer server.Shutdown(t.Context())
	post := func(body string) {
		t.Helper()
		res, err := http.Post(s.URL+"/api/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 || !strings.Contains(string(raw), `"status":"completed"`) {
			t.Fatalf("run failed: %s", raw)
		}
	}
	history := func() []api.Run {
		t.Helper()
		res, err := http.Get(s.URL + "/api/runs")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result struct {
			Runs []api.Run `json:"runs"`
		}
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result.Runs
	}
	post(`{"harnessId":"opencode","prompt":"first version"}`)
	first := history()[0]
	post(fmt.Sprintf(`{"harnessId":"opencode","prompt":"second version","parentRunId":%q}`, first.ID))
	runs := history()
	if len(runs) != 2 {
		t.Fatalf("runs: %d", len(runs))
	}
	second := runs[0]
	if second.ParentRunID != first.ID || second.ProjectID != first.ProjectID || second.ID == first.ID {
		t.Fatalf("lineage lost: %+v", second)
	}
	raw, _ := os.ReadFile(filepath.Join(first.Dir, "version.txt"))
	if string(raw) != "first" {
		t.Fatal("original version overwritten")
	}
}

func TestContinueRejectsSymlinksAndRemovesPartialCopy(t *testing.T) {
	root := t.TempDir()
	s, err := api.NewWorkspaceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, dir, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "a.txt"), "keep me")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "z.txt")); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Continue(t.Context(), id)
	if err == nil {
		t.Fatal("accepted symlink")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != id {
		t.Fatalf("partial checkpoint leaked: %+v", entries)
	}
}
