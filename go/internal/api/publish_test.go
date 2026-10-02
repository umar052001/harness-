package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/runner"
)

// Publishing gives a run its own origin. The behaviour worth defending is that
// a built site is served exactly as a web server would serve it - same bytes,
// root-absolute references resolving natively, SPA routes falling back to the
// index - and that doing so does not widen what a generated site can reach.

func newPublishServer(t *testing.T) (*httptest.Server, *api.WorkspaceStore) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "workspaces")
	store, err := api.NewWorkspaceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(runner.NewUnsandboxed(nil), api.Options{
		WorkspacesDir: root,
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

// seedBuild writes the output shape a real Vite build produces, inside a project
// directory rather than at the workspace root, because that is the case that
// proves the site root is resolved rather than assumed.
func seedBuild(t *testing.T, store *api.WorkspaceStore) string {
	t.Helper()
	id, dir, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "portfolio", "dist", "index.html"),
		`<!doctype html><script type="module" src="/assets/index-abc.js"></script><link rel="stylesheet" href="/assets/index-abc.css">`)
	writeFile(t, filepath.Join(dir, "portfolio", "dist", "assets", "index-abc.js"), `console.log("app")`)
	writeFile(t, filepath.Join(dir, "portfolio", "dist", "assets", "index-abc.css"), `body{color:red}`)
	writeFile(t, filepath.Join(dir, "portfolio", "src", "main.tsx"), "export {}\n")
	writeFile(t, filepath.Join(dir, "portfolio", "node_modules", "react", "index.js"), "// noise")
	return id
}

func publish(t *testing.T, srv *httptest.Server, id string) string {
	t.Helper()
	res, err := http.Post(srv.URL+"/api/workspace/"+id+"/publish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("publish = %d, body %s", res.StatusCode, b)
	}
	var out struct {
		Site struct {
			Root  string   `json:"root"`
			Index string   `json:"index"`
			URLs  []string `json:"urls"`
		} `json:"site"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Site.Root != "portfolio/dist" {
		t.Errorf("published root = %q, want portfolio/dist", out.Site.Root)
	}
	if len(out.Site.URLs) == 0 || !strings.Contains(out.Site.URLs[0], id) {
		t.Errorf("urls = %v, want one carrying the run id", out.Site.URLs)
	}
	return out.Site.URLs[0]
}

// getHost issues a request with an explicit Host, which is how a published site
// is addressed: the run id lives in the hostname, not the path.
func getHost(t *testing.T, srv *httptest.Server, host, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func TestPublishServesBuildOutputAtItsOwnRoot(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)
	publish(t, srv, id)

	// A root-absolute asset reference resolves natively: this is the payoff of
	// giving the site its own origin rather than rewriting it behind a mount.
	res := getHost(t, srv, id+".localhost:8090", "/assets/index-abc.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("asset = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != `console.log("app")` {
		t.Errorf("asset body = %q, want the file verbatim", body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset content-type = %q, want javascript", ct)
	}

	res = getHost(t, srv, id+".localhost:8090", "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("index = %d, want 200", res.StatusCode)
	}
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), `src="/assets/index-abc.js"`) {
		t.Errorf("index was rewritten: %s", body)
	}
}

func TestPublishedSiteFallsBackToIndexForSPAPaths(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)
	publish(t, srv, id)

	res := getHost(t, srv, id+".localhost:8090", "/projects/some-slug")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SPA route = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "index-abc.js") {
		t.Errorf("SPA route did not serve the index: %s", body)
	}
}

// A missing asset answered with HTML looks like a corrupt bundle rather than a
// 404, which sends the reader looking in the wrong place.
func TestPublishedSiteDoesNotFallBackForMissingAssets(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)
	publish(t, srv, id)

	res := getHost(t, srv, id+".localhost:8090", "/assets/missing.js")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset = %d, want 404", res.StatusCode)
	}
}

// Publishing a site must not become a way to read arbitrary workspace files.
func TestPublishedSiteIsConfinedToItsSiteRoot(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)
	dir, derr := store.Dir(id)
	if derr != nil {
		t.Fatal(derr)
	}
	writeFile(t, filepath.Join(dir, "secret.txt"), "token=hunter2")
	publish(t, srv, id)

	for _, p := range []string{
		"/../secret.txt",
		"/assets/../../secret.txt",
		"/../../../../etc/passwd",
	} {
		res := getHost(t, srv, id+".localhost:8090", p)
		body, _ := io.ReadAll(res.Body)
		if strings.Contains(string(body), "hunter2") {
			t.Errorf("path %q escaped the site root: %s", p, body)
		}
	}
}

// The reason the site is addressed by hostname rather than mounted under
// /p/<runid>/: on its own origin it is only a website.
func TestPublishedSiteIsNotSameOriginWithTheDashboard(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)
	publish(t, srv, id)

	res := getHost(t, srv, id+".localhost:8090", "/")
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("published site is cross-origin readable: ACAO = %q", got)
	}
	// Scripts must run - that is what a published site is for - but only the
	// dashboard may frame it.
	if csp := res.Header.Get("Content-Security-Policy"); strings.Contains(csp, "sandbox") {
		t.Errorf("published site is sandboxed, so it cannot run: %q", csp)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors") {
		t.Errorf("published site may be framed by anyone: %q", csp)
	}
	// The dashboard has to be the one allowed to frame it, whichever loopback
	// spelling it was opened with - a browser calls those different origins,
	// and naming only one leaves the preview blank for half of all visits.
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp,
		"frame-ancestors 'self' http://localhost:8090 http://127.0.0.1:8090") {
		t.Errorf("frame-ancestors = %q, want both loopback spellings", csp)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff = %q", got)
	}
}

// A host naming a valid run that was never published must not be answered from
// the filesystem.
func TestUnpublishedHostIsNotServed(t *testing.T) {
	srv, store := newPublishServer(t)
	id := seedBuild(t, store)

	res := getHost(t, srv, id+".localhost:8090", "/")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unpublished host = %d, want 404", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "index-abc.js") {
		t.Errorf("unpublished run leaked its build: %s", body)
	}

	// The dashboard's own address is unaffected by any of this.
	res, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Errorf("dashboard health = %d, want 200", res.StatusCode)
	}
}

// A link is only useful if it survives a restart of the control plane.
func TestPublishPersistsAcrossRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	store, err := api.NewWorkspaceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	srv1 := httptest.NewServer(api.New(runner.NewUnsandboxed(nil), api.Options{WorkspacesDir: root}).Handler())
	id := seedBuild(t, store)
	publish(t, srv1, id)
	srv1.Close()

	srv2 := httptest.NewServer(api.New(runner.NewUnsandboxed(nil), api.Options{WorkspacesDir: root}).Handler())
	defer srv2.Close()
	res := getHost(t, srv2, id+".localhost:8090", "/assets/index-abc.js")
	if res.StatusCode != http.StatusOK {
		t.Errorf("after restart asset = %d, want 200", res.StatusCode)
	}
	res, err = http.Get(srv2.URL + "/api/workspace/" + id + "/publish")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), `"published":true`) {
		t.Errorf("status after restart = %s", b)
	}
}

// A published site has to be reachable even when the control plane is also
// serving its own frontend, because the dashboard claims "/" for itself and the
// routing order decides which one answers.
func TestPublishedSiteWinsOverTheDashboardFrontend(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	store, err := api.NewWorkspaceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	web := t.TempDir()
	writeFile(t, filepath.Join(web, "index.html"), "<h1>the dashboard</h1>")

	srv := httptest.NewServer(api.New(runner.NewUnsandboxed(nil), api.Options{
		WorkspacesDir: root,
		WebDir:        web,
	}).Handler())
	defer srv.Close()
	id := seedBuild(t, store)
	publish(t, srv, id)

	res := getHost(t, srv, id+".localhost:8090", "/")
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "the dashboard") {
		t.Errorf("published host was answered by the dashboard frontend: %s", body)
	}
	if !strings.Contains(string(body), "index-abc.js") {
		t.Errorf("published host did not serve the site: %s", body)
	}

	// And the dashboard itself is untouched on its own hostname.
	res, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ = io.ReadAll(res.Body)
	if !strings.Contains(string(body), "the dashboard") {
		t.Errorf("dashboard frontend = %s, want its own index", body)
	}
}

func TestPublishRejectsRunWithoutABuiltSite(t *testing.T) {
	srv, store := newPublishServer(t)
	id, dir, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "notes.md"), "# notes\n")

	res, err := http.Post(srv.URL+"/api/workspace/"+id+"/publish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("publish without a site = %d, want 422", res.StatusCode)
	}
	b, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(b), "no built site") {
		t.Errorf("body = %s", b)
	}
}

// TestPublishedSiteWithFlatWorkspaceRootServesItsIndex covers the shape a static
// site produces: index.html and its assets at the top of the workspace, which
// publishes with root "." rather than a build directory.
//
// Every other test here seeds portfolio/dist, so this was the one shape nothing
// exercised - and it is the one that answered 404 at its own address, because
// resolving "/" gave path.Join(".", "/") == "." and path.Ext(".") is "." , a
// truthy value that is not ".html", so the request was treated as a missing asset
// instead of the site root. The dashboard had already published it and was
// embedding it, so what the user saw at the end of a run was a 404 page.
func TestPublishedSiteWithFlatWorkspaceRootServesItsIndex(t *testing.T) {
	srv, store := newPublishServer(t)
	id, dir, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "index.html"),
		`<!doctype html><title>flat</title><link rel="stylesheet" href="/style.css">`)
	writeFile(t, filepath.Join(dir, "style.css"), `body{background:#000}`)
	writeFile(t, filepath.Join(dir, "README.md"), "# flat\n")

	res, err := http.Post(srv.URL+"/api/workspace/"+id+"/publish", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Site struct {
			Root string `json:"root"`
		} `json:"site"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if out.Site.Root != "." {
		t.Fatalf("published root = %q, want \".\"", out.Site.Root)
	}

	res = getHost(t, srv, id+".localhost:8090", "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("site root = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "<title>flat</title>") {
		t.Errorf("root body = %q, want the index verbatim", body)
	}

	// The root-absolute asset the index points at still resolves, which is the
	// whole point of giving the site its own origin.
	res = getHost(t, srv, id+".localhost:8090", "/style.css")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("asset = %d, want 200", res.StatusCode)
	}

	// An unknown route is still the SPA's business, not a 404.
	res = getHost(t, srv, id+".localhost:8090", "/about")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SPA route = %d, want 200", res.StatusCode)
	}

	// A missing asset is still a 404: answering it with the index would surface as
	// a parse error that reads like a corrupt bundle.
	res = getHost(t, srv, id+".localhost:8090", "/missing.js")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", res.StatusCode)
	}
}
