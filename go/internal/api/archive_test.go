package api_test

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
)

// A run's files can be read in the dashboard but not taken out of it, which
// makes the tool a place work goes to be looked at and then abandoned. These
// tests cover the two ways out: one file, and the whole project.

func TestDownloadsASingleFileByteForByte(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not valid HTML on its own: the preview path rewrites
	// root-relative references for display, and a download must not be touched.
	writeFile(t, filepath.Join(dir, "src", "app.ts"), "import './x.css'\nexport const n = 1\n")

	srv := httptest.NewServer(api.New(nil, api.Options{WorkspacesDir: w.Root()}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/workspace/" + id + "/raw/src/app.ts?download=1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="app.ts"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != "import './x.css'\nexport const n = 1\n" {
		t.Errorf("body was rewritten: %q", body)
	}
	// Without ?download=1 the same request is a preview and keeps its sandbox,
	// which is what lets an HTML file run safely in an iframe.
	res2, err := http.Get(srv.URL + "/api/workspace/" + id + "/raw/src/app.ts")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if got := res2.Header.Get("Content-Disposition"); got != "" {
		t.Errorf("a plain raw request became an attachment: %q", got)
	}
	if got := res2.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("preview lost its sandbox: %q", got)
	}
}

func TestArchivesTheWholeProject(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	writeFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")
	writeFile(t, filepath.Join(dir, "package.json"), "{}\n")
	writeFile(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "throw new Error('should not ship')\n")
	writeFile(t, filepath.Join(dir, ".home", "credentials"), "secret\n")

	srv := httptest.NewServer(api.New(nil, api.Options{WorkspacesDir: w.Root()}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/workspace/" + id + "/archive")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="ai-os-`+id+`.zip"` {
		t.Errorf("Content-Disposition = %q", got)
	}

	names, contents := readZip(t, res.Body)
	for _, want := range []string{"index.html", "src/main.ts", "package.json"} {
		if !names[want] {
			t.Errorf("archive is missing %s: %v", want, names)
		}
	}
	if contents["index.html"] != "<h1>hi</h1>" {
		t.Errorf("index.html = %q", contents["index.html"])
	}
	// The two things most likely to be shipped by accident: a dependency tree
	// the user can reinstall, and the run's synthetic home holding its config.
	for _, unwanted := range []string{"node_modules/left-pad/index.js", ".home/credentials"} {
		if names[unwanted] {
			t.Errorf("archive shipped %s", unwanted)
		}
	}
	for name := range names {
		if strings.HasPrefix(name, "INCOMPLETE-READ-ME.txt") {
			t.Errorf("a complete archive claimed to be incomplete")
		}
	}
}

func TestArchiveRootScopesTheDownload(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "src", "main.ts"), "export {}\n")
	writeFile(t, filepath.Join(dir, "dist", "index.html"), "<h1>built</h1>")

	srv := httptest.NewServer(api.New(nil, api.Options{WorkspacesDir: w.Root()}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/workspace/" + id + "/archive?root=dist")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Content-Disposition"); got != `attachment; filename="ai-os-`+id+`-dist.zip"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	names, _ := readZip(t, res.Body)
	// Entries keep their place in the project, so unpacking does not scatter
	// the build output across a downloads folder.
	if !names["dist/index.html"] {
		t.Errorf("archive is missing dist/index.html: %v", names)
	}
	if names["src/main.ts"] {
		t.Errorf("a scoped archive included source outside dist: %v", names)
	}
}

func TestArchiveRootCannotEscapeTheWorkspace(t *testing.T) {
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	secret := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(secret, []byte("not yours"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.New(nil, api.Options{WorkspacesDir: w.Root()}).Handler())
	defer srv.Close()

	for _, root := range []string{"..", "../", "../../etc", "dist/../../.."} {
		res, err := http.Get(srv.URL + "/api/workspace/" + id + "/archive?root=" + root)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("root=%q status %d, want 404", root, res.StatusCode)
		}
	}
}

func TestArchiveFollowsNoSymlinks(t *testing.T) {
	// A workspace can hold a link, and following one would let a download pull
	// in a file from anywhere on the disk. Skipping is the safe reading: the
	// link is not the work.
	w := newStore(t)
	id, dir, err := w.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>hi</h1>")
	target := filepath.Join(t.TempDir(), "elsewhere.txt")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	srv := httptest.NewServer(api.New(nil, api.Options{WorkspacesDir: w.Root()}).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/workspace/" + id + "/archive")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	names, contents := readZip(t, res.Body)
	if names["link.txt"] || contents["link.txt"] == "secret" {
		t.Errorf("archive followed a symlink out of the workspace: %v", contents)
	}
}

// readZip returns the entry names and their contents.
func readZip(t *testing.T, body io.Reader) (map[string]bool, map[string]string) {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	names := make(map[string]bool, len(zr.File))
	contents := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		contents[f.Name] = string(b)
	}
	return names, contents
}
