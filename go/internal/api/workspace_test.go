package api_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
)

// The workspace store is the only thing standing between a browser and whatever
// an agent decided to write, so most of these tests are about what it refuses.

func TestWorkspaceListsWhatTheAgentWrote(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	writeFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")

	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 2 {
		t.Errorf("total = %d, want 2: %+v", l.Total, l.Files)
	}
	kinds := map[string]string{}
	for _, f := range l.Files {
		kinds[f.Path] = f.Kind
	}
	if kinds["index.html"] != "html" || kinds["src/main.ts"] != "ts" {
		t.Errorf("kinds = %v", kinds)
	}
	if l.Preview != "index.html" {
		t.Errorf("preview = %q, want index.html", l.Preview)
	}
}

func TestWorkspaceSkipsDependencyTrees(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	// A scaffolded project has tens of thousands of these. Listing them would
	// bury the two files a person cares about and cost seconds per run.
	writeFile(t, filepath.Join(dir, "node_modules", "react", "index.js"), "// noise")
	writeFile(t, filepath.Join(dir, ".git", "config"), "[core]\n")

	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Total != 1 || l.Files[0].Path != "index.html" {
		t.Errorf("a dependency tree leaked into the listing: %+v", l.Files)
	}
}

func TestWorkspaceRejectsTraversal(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(filepath.Dir(dir), "secret.txt"), "not yours")

	for _, rel := range []string{"../../secret.txt", "/../../secret.txt", "..", "sub/../../secret.txt"} {
		if _, _, err := w.Open(id, rel); err == nil {
			t.Errorf("Open(%q) succeeded; it must be refused", rel)
		}
	}
	// A hostile id must not escape either: the id is joined onto the root, so an
	// unchecked one is a path traversal of its own.
	for _, bad := range []string{"..", "../..", "a/b", "0123456789abZ", "", "short"} {
		if _, err := w.Dir(bad); err == nil {
			t.Errorf("Dir(%q) succeeded; it must be refused", bad)
		}
	}
}

func TestWorkspaceRefusesSymlinkEscape(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "host file")
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// There is no "../" anywhere in this request, so cleaning the string is not
	// enough: an agent can plant a symlink and a naive server will serve the host
	// file. Resolving and re-checking containment is the whole guard.
	if _, _, err := w.Open(id, "link.txt"); err == nil {
		t.Error("a symlink out of the workspace was served")
	}
	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range l.Files {
		if f.Path == "link.txt" {
			t.Error("a symlink out of the workspace appeared in the listing")
		}
	}
}

func TestWorkspacePrefersBuiltOutputForPreview(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	// The shape a Vite scaffold leaves behind: a source index.html that cannot
	// render on its own, plus a built one that can.
	writeFile(t, filepath.Join(dir, "index.html"), `<script type="module" src="/src/main.jsx"></script>`)
	writeFile(t, filepath.Join(dir, "dist", "index.html"), "<h1>built</h1>")

	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Preview != "dist/index.html" {
		t.Errorf("preview = %q, want dist/index.html", l.Preview)
	}
}

func TestRunProducesPreviewableArtifacts(t *testing.T) {
	// The end-to-end claim: after a real run through the API, the workspace is
	// still there and can be listed and served. This is the behaviour that was
	// missing entirely - the runner used to delete the output with its scratch
	// directory, so a user asked for a site and had nothing to look at.
	s := newTestServer(t)
	id := runOnce(t, s)

	res, err := http.Get(s.URL + "/api/workspace/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("listing status %d", res.StatusCode)
	}
	var listing api.Listing
	if err := json.NewDecoder(res.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if listing.Preview != "index.html" {
		t.Errorf("preview = %q, want index.html", listing.Preview)
	}
	paths := map[string]bool{}
	for _, f := range listing.Files {
		paths[f.Path] = true
	}
	if !paths["index.html"] || !paths["src/main.js"] {
		t.Errorf("listing is missing what the agent wrote: %+v", listing.Files)
	}
	if paths["node_modules/left-pad/index.js"] {
		t.Error("the dependency tree was listed")
	}

	page, err := http.Get(s.URL + "/api/workspace/" + id + "/raw/index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	if page.StatusCode != 200 {
		t.Fatalf("raw status %d", page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "fake") {
		t.Errorf("served page = %q", body)
	}
	// These bytes are agent-authored and are framed by the same origin as the
	// dashboard. Without an opaque origin a generated page can script against
	// the API that is displaying it.
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("content-security-policy = %q, want a sandbox directive", csp)
	}
	if ct := page.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if page.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on agent-authored content")
	}
	// A sandboxed document has an opaque origin, so its own module scripts need
	// CORS to load. "null" admits exactly that and nothing else - a page on
	// another site cannot read a run's files through it.
	if got := page.Header.Get("Access-Control-Allow-Origin"); got != "null" {
		t.Errorf("access-control-allow-origin = %q, want null so the preview's own bundle can load", got)
	}
}

func TestWorkspaceEndpointsReportNotFound(t *testing.T) {
	s := newTestServer(t)
	for _, path := range []string{
		"/api/workspace/deadbeefdead",
		"/api/workspace/nope/raw/index.html",
		"/api/workspace/../../etc/passwd",
	} {
		res, err := http.Get(s.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestRunsListsPreviousWork(t *testing.T) {
	s := newTestServer(t)
	id := runOnce(t, s)

	res, err := http.Get(s.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Runs []api.Run `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Runs) != 1 || got.Runs[0].ID != id {
		t.Fatalf("runs = %+v, want the one run %s", got.Runs, id)
	}
	if got.Runs[0].Files != 2 {
		t.Errorf("files = %d, want 2", got.Runs[0].Files)
	}
}

func TestNarrateAlwaysAnswers(t *testing.T) {
	// No planner is configured on this server, so this is the degraded path: the
	// status line still has to say something useful.
	s := newTestServer(t)
	body := `{"phase":"working","harness":"OpenCode","step":"Running: npm install","tools":3,"elapsedSec":42}`
	res, err := http.Post(s.URL+"/api/narrate", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Narration string `json:"narration"`
		Source    string `json:"source"`
		Model     bool   `json:"model"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Narration == "" {
		t.Fatal("narration is empty; the endpoint must always answer")
	}
	if got.Source != "local" || got.Model {
		t.Errorf("source = %q model = %v, want local", got.Source, got.Model)
	}
	if !strings.Contains(got.Narration, "npm install") {
		t.Errorf("narration %q should name the current action", got.Narration)
	}
}

// runOnce drives one run against the fake agent and returns the workspace id the
// stream assigned.
//
// The stream is drained to the end before returning. Closing the response body
// mid-stream cancels the run, and a cancelled run leaves an empty workspace - so
// reading one frame and hanging up would test nothing about artifacts.
func runOnce(t *testing.T, s *httptest.Server) string {
	t.Helper()
	res, err := http.Post(s.URL+"/api/run", "application/json",
		strings.NewReader(`{"harnessId":"api-fake","prompt":"make me a page"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event, data := "", ""
	runID, ended := "", false
	for sc.Scan() {
		switch line := sc.Text(); {
		case line == "":
			switch event {
			case "run.assigned":
				var got struct {
					RunID string `json:"runId"`
				}
				if err := json.Unmarshal([]byte(data), &got); err != nil {
					t.Fatal(err)
				}
				runID = got.RunID
			case "stream.end":
				var got struct {
					OK bool `json:"ok"`
				}
				if err := json.Unmarshal([]byte(data), &got); err != nil {
					t.Fatal(err)
				}
				ended = got.OK
			}
			event, data = "", ""
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if !ended {
		t.Fatal("the run did not finish ok")
	}
	if runID == "" {
		t.Fatal("the stream never assigned a workspace; there would be nothing to preview")
	}
	return runID
}

func TestWorkspaceHidesAgentStateButKeepsProjectDotfiles(t *testing.T) {
	// This is what a confined run actually leaves behind: with HOME redirected
	// into the workspace, the agent's npm cache, logs and session database sit
	// beside the deliverable. Listing them made a one-file run look like a
	// twenty-eight-file one.
	s := newStore(t)
	id, dir, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	_ = dir
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	writeFile(t, filepath.Join(dir, "README.md"), "# readme")
	writeFile(t, filepath.Join(dir, ".env.example"), "KEY=")
	writeFile(t, filepath.Join(dir, ".home/.npm/_cacache/content-v2/blob"), "binary-ish")
	writeFile(t, filepath.Join(dir, ".home/.local/share/opencode/log/run.log"), "log")
	writeFile(t, filepath.Join(dir, ".sandbox-tmp/lib.so"), "so")
	writeFile(t, filepath.Join(dir, ".github/workflows/ci.yml"), "on: push")

	listing, err := s.List(id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range listing.Files {
		got[f.Path] = true
	}
	for _, want := range []string{"index.html", "README.md", ".env.example", ".github/workflows/ci.yml"} {
		if !got[want] {
			t.Errorf("listing is missing %s: %+v", want, listing.Files)
		}
	}
	for _, unwanted := range []string{".home/.npm/_cacache/content-v2/blob", ".home/.local/share/opencode/log/run.log", ".sandbox-tmp/lib.so"} {
		if got[unwanted] {
			t.Errorf("agent state was listed as output: %s", unwanted)
		}
	}
	if listing.Preview != "index.html" {
		t.Errorf("preview = %q, want index.html", listing.Preview)
	}
}

// A scaffolder invents a new scratch directory on every version, and a
// throwaway index.html inside one gets listed, chosen as the preview, and then
// fails to load - which is how a run that built a working app still showed a
// broken preview. An unknown dot-directory has to be treated as scratch.
func TestWorkspaceHidesUnknownDotDirectories(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".scaffold", "index.html"), "<h1>scratch</h1>")
	writeFile(t, filepath.Join(dir, ".next-scratch-7", "index.html"), "<h1>scratch</h1>")
	writeFile(t, filepath.Join(dir, "dist", "index.html"), "<h1>real</h1>")
	// Project dot-directories are output, not scratch.
	writeFile(t, filepath.Join(dir, ".github", "workflows", "ci.yml"), "name: ci\n")
	writeFile(t, filepath.Join(dir, ".vscode", "settings.json"), "{}\n")
	// A dotfile a person wrote is a project file.
	writeFile(t, filepath.Join(dir, ".env.example"), "TOKEN=\n")

	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, f := range l.Files {
		paths[f.Path] = true
	}
	for _, hidden := range []string{".scaffold/index.html", ".next-scratch-7/index.html"} {
		if paths[hidden] {
			t.Errorf("scratch file %q is listed as output", hidden)
		}
	}
	for _, kept := range []string{"dist/index.html", ".github/workflows/ci.yml", ".vscode/settings.json", ".env.example"} {
		if !paths[kept] {
			t.Errorf("%q was hidden but is a project file", kept)
		}
	}
	if l.Preview != "dist/index.html" {
		t.Errorf("preview = %q, want dist/index.html", l.Preview)
	}
}

// Even if a scratch index.html reaches the listing, the preview must pass it
// over: framing an agent's throwaway file is how the dashboard showed a blank
// page on a run that had built a real site.
func TestPreviewNeverPointsIntoAScratchDirectory(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>root</h1>")
	writeFile(t, filepath.Join(dir, ".cache", "index.html"), "<h1>scratch</h1>")

	l, err := w.List(id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Preview != "index.html" {
		t.Errorf("preview = %q, want index.html", l.Preview)
	}
}

func newStore(t *testing.T) *api.WorkspaceStore {
	t.Helper()
	w, err := api.NewWorkspaceStore(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
