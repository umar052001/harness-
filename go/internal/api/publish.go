package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Publishing: turning a run's build output into a real, shareable URL.
//
// The dashboard preview frames a run through /api/workspace/<id>/raw/..., which
// forces a rewriting proxy: the built site believes it lives at the root of its
// own domain, and every root-absolute reference has to be remapped to reach it.
// That is workable but it is not a website. There is no address to hand anyone,
// and the rewrites mean the site is only correct in the one place it is framed.
//
// Publishing gives a run its own origin instead. The built site is mounted at the
// root of <runid>.<host>, so "/assets/index-abc.js" resolves correctly on its
// own, with no rewriting at all, and the URL is one a person can open, bookmark,
// or paste into a message.
//
// The distinct origin is the security argument, not a convenience. A published
// site is untrusted generated code that executes with scripts enabled. Mounted
// under the control plane's own origin - /p/<runid>/ - it would be same-origin
// with the dashboard: able to read the API responses, read the DOM, and act as
// the operator. On its own origin it can only do what any website can do, which
// is the same trust level as opening the preview in a new tab. That is why the
// site is addressed by hostname and not by path.

// Published is a run's site, resolved and ready to be served.
type Published struct {
	RunID string `json:"runId"`
	// Root is the site root relative to the workspace: the directory whose
	// index.html is the site. For a Vite build inside a project it is
	// "portfolio/dist", not "portfolio".
	Root string `json:"root"`
	// Index is the entry document relative to Root.
	Index string    `json:"index"`
	At    time.Time `json:"at"`
	// URLs are the addresses this site answers on, most specific first.
	URLs []string `json:"urls"`
}

// publishIndex remembers which runs have been published.
//
// It is written next to the workspaces rather than kept in memory, so a link
// keeps working across a restart of the control plane. The workspaces are the
// durable artefact; a link to one of them is just as durable.
type publishIndex struct {
	mu   sync.Mutex
	path string
}

func newPublishIndex(root string) *publishIndex {
	return &publishIndex{path: filepath.Join(root, ".published.json")}
}

func (p *publishIndex) load() map[string]Published {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]Published{}
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return out
	}
	// A corrupt index is not fatal: it costs the operator their links, not their
	// files, and publishing again restores it.
	_ = json.Unmarshal(raw, &out)
	return out
}

func (p *publishIndex) get(id string) (Published, bool) {
	all := p.load()
	pub, ok := all[id]
	return pub, ok
}

func (p *publishIndex) put(pub Published) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := map[string]Published{}
	if raw, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(raw, &all)
	}
	all[pub.RunID] = pub
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// publish resolves a run's built site and records it.
//
// It requires a preview entry: that is the store's existing judgement about
// which file is the site's entry point, and a run that produced only source or
// only a document has nothing to serve. Publishing is idempotent, and
// re-publishing after a rebuild picks up the new output, which is what makes it
// usable as the "ship it" step rather than a one-shot.
func (s *Server) publish(id string) (Published, error) {
	if s.store == nil {
		return Published{}, errors.New("workspace store unavailable")
	}
	listing, err := s.store.List(id)
	if err != nil {
		return Published{}, err
	}
	if listing.Preview == "" {
		return Published{}, fmt.Errorf("run %s produced no built site: there is no index.html to publish", id)
	}
	return Published{
		RunID: id,
		Root:  dirOf(listing.Preview),
		Index: path.Base(listing.Preview),
		At:    time.Now().UTC(),
	}, nil
}

func dirOf(rel string) string {
	if d := path.Dir(rel); d != "" && d != "." {
		return d
	}
	return "."
}

// siteURL is the address a published run is reachable on, derived from the
// request rather than configured: the control plane may be bound to any
// interface or port, and substituting the first label of the host it was reached
// on is what keeps the link correct without the server knowing its own name.
func siteURL(host, id string) string {
	hostname := host
	port := ""
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		hostname, port = host[:i], host[i:]
	}
	// Asking for the address of a site that is already being served must not
	// stack another run id onto it: the host is already the answer.
	if label, _, ok := strings.Cut(hostname, "."); ok && validID(label) {
		return "http://" + host
	}
	// A loopback address is normalised to localhost. {runid}.127.0.0.1 does not
	// resolve anywhere, so a link built from it is dead on arrival, while every
	// {runid}.localhost name resolves to the loopback in a browser without a
	// hosts entry. Deriving the link from the interface the operator happened to
	// connect on must not decide whether the link works.
	if hostname == "127.0.0.1" || hostname == "::1" || hostname == "[::1]" {
		hostname = "localhost"
	}
	if hostname == "" {
		return "http://" + id + ".localhost" + port
	}
	return "http://" + id + "." + hostname + port
}

// handlePublish is the API half of publishing: report what is published, or
// publish the run's site and report the addresses it is now live on.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.Error(w, "workspace: unknown run", http.StatusNotFound)
		return
	}
	if s.publishIndex == nil {
		http.Error(w, "publishing unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodGet {
		pub, ok := s.publishIndex.get(id)
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"published": false})
			return
		}
		pub.URLs = []string{siteURL(r.Host, id)}
		writeJSON(w, http.StatusOK, map[string]any{"published": true, "site": pub})
		return
	}

	pub, err := s.publish(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	pub.URLs = []string{siteURL(r.Host, id)}
	if err := s.publishIndex.put(pub); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"published": true, "site": pub})
}

// runFromHost extracts a run id addressed as a subdomain.
//
// Only a syntactically valid run id is considered, and the caller then requires
// that run to be published, so an unrecognised or unpublished host falls through
// to normal routing instead of being answered from the filesystem.
func runFromHost(host string) (string, bool) {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	label, _, ok := strings.Cut(host, ".")
	if !ok || !validID(label) {
		return "", false
	}
	return label, true
}

// handlePublishedSite serves a published run at its own origin.
func (s *Server) handlePublishedSite(w http.ResponseWriter, r *http.Request, id string, pub Published) {
	if s.store == nil {
		http.Error(w, s.storeErr, http.StatusServiceUnavailable)
		return
	}
	// The requested path is resolved through the workspace store, so it gets the
	// same traversal and symlink checks as every other file read: publishing a
	// site does not widen what is readable.
	rel := strings.TrimPrefix(r.URL.Path, "/")

	// The site root resolves to the index directly.
	//
	// It cannot go through the join and the extension test below: for a published
	// root of "." - which is every run that wrote index.html at the top level,
	// including this project's own demo - path.Join(".", "/") is "." and
	// path.Ext(".") is ".", a truthy value that is not ".html". The request was
	// therefore classified as a missing asset and answered 404, at an address the
	// dashboard had just published and was already embedding. A root of "dist" or
	// "portfolio" gave path.Ext("") == "" and worked, which is why the bug only
	// ever appeared on the flat case.
	isRoot := rel == "" || rel == "."
	target := path.Join(pub.Root, path.Clean("/"+rel))
	if isRoot {
		target = path.Join(pub.Root, pub.Index)
	}

	ct := contentTypeFor(target)
	f, _, err := s.store.Open(id, target)
	if err != nil && !isRoot {
		// A single-page app owns its own routing: an unknown path is a route, not
		// a missing file. Missing assets are left missing, because answering a
		// request for a script with HTML produces a parse error that reads like
		// a corrupt bundle rather than a 404. The test is on the requested path,
		// never on the joined one, for the reason above.
		if ext := path.Ext(rel); ext != "" && ext != ".html" {
			http.NotFound(w, r)
			return
		}
		target = path.Join(pub.Root, pub.Index)
		ct = contentTypeFor(target)
		f, _, err = s.store.Open(id, target)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// The site runs: no sandbox directive, because that is the whole point of
	// publishing. What is constrained instead is who may frame it - only the
	// control plane, so the dashboard can embed it and nothing else can.
	w.Header().Set("Content-Security-Policy", "frame-ancestors "+dashboardAncestors(r.Host, id))
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Revalidate rather than cache indefinitely: the point of re-publishing is
	// that the same address starts serving new output.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, filepath.Base(target), time.Time{}, f)
}

// dashboardAncestors is the frame-ancestors list for a published site: the
// control plane, and nothing else.
//
// The dashboard is this server's base host, which is the host with the run's
// label removed - naming the published host instead would forbid the dashboard
// from framing what it just published, and the preview would silently go blank.
//
// Both loopback spellings are listed because a browser treats http://localhost
// and http://127.0.0.1 as different origins, and which one the operator has open
// is not something the published response can know. Allowing only one of them
// produces a blank preview for half of all visits, which is a bad trade for a
// cosmetic difference between two names for this machine.
func dashboardAncestors(host, id string) string {
	base := host
	if label, _, ok := strings.Cut(host, "."); ok && label == id {
		base = host[len(label)+1:]
	}
	parts := []string{"'self'", "http://" + base}
	if _, port, ok := strings.Cut(base, "localhost:"); ok {
		parts = append(parts, "http://127.0.0.1:"+port)
	} else if _, port, ok := strings.Cut(base, "127.0.0.1:"); ok {
		parts = append(parts, "http://localhost:"+port)
	}
	return strings.Join(parts, " ")
}

// publishedSite reports the published record for the run addressed by this
// request's host, if any.
func (s *Server) publishedSite(w http.ResponseWriter, r *http.Request) bool {
	id, ok := runFromHost(r.Host)
	if !ok {
		return false
	}
	pub, published := s.publishIndex.get(id)
	if !published {
		http.Error(w, "this address names a run that has not been published; open the dashboard to publish it", http.StatusNotFound)
		return true
	}
	s.handlePublishedSite(w, r, id, pub)
	return true
}
