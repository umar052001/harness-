package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// WorkspaceStore owns the directories runs are allowed to work in.
//
// It exists because of a specific failure: the runner deletes its scratch
// directory when a run ends, which is right for a throwaway sandbox and useless
// for a product. A user asks for a portfolio site and then has nothing to look
// at, because the only copy was thrown away the instant the agent stopped. So
// runs get a durable directory under a server-owned root, and this file is the
// only thing that can hand out a path inside it.
type WorkspaceStore struct {
	root string
}

// WorkspaceFile is one entry in a listing.
type WorkspaceFile struct {
	Path    string    `json:"path"`
	Kind    string    `json:"kind"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`
}

// Listing is the answer to "what did the agent actually make".
type Listing struct {
	ID    string          `json:"id"`
	Dir   string          `json:"dir"`
	Files []WorkspaceFile `json:"files"`
	// Preview is the best HTML entry to frame, empty when the run produced no
	// page. It is computed here rather than in the browser because deciding what
	// counts as the site is a judgement about where agents put build output.
	Preview string `json:"preview"`
	// Total and Truncated describe what the walk stopped at. A run that installed
	// a dependency tree has a million files and the interesting ones are a
	// handful, so the walk is bounded and says so rather than silently lying.
	Total     int   `json:"total"`
	Truncated bool  `json:"truncated"`
	Bytes     int64 `json:"bytes"`
}

// Run is one workspace in the store's history.
type Run struct {
	RunMetadata
	ID      string    `json:"id"`
	Dir     string    `json:"dir"`
	ModTime time.Time `json:"modTime"`
	Files   int       `json:"files"`
}

const (
	// maxListedFiles bounds a listing. A dependency tree is noise: nobody
	// previews node_modules, and walking it costs seconds per run.
	maxListedFiles = 2000
	// maxListedBytes bounds how much of one file the listing will read when
	// classifying it.
	maxListedBytes = 4 << 20
	// maxPreviewBytes bounds what the raw endpoint will serve. A generated site
	// is kilobytes; a stray multi-gigabyte artifact is not something to stream
	// into a browser tab.
	maxPreviewBytes = 8 << 20
)

// skipDirs are directories a run can create that are never worth listing.
// Dependency trees and VCS metadata are both large, both machine-generated, and
// both useless in a preview.
var skipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	".next":        true,
	".nuxt":        true,
	".svelte-kit":  true,
	".turbo":       true,
	".cache":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	"target":       true,
}

// agentStateDirs are the bookkeeping directories an agent creates for itself
// when it runs confined.
//
// With HOME redirected into the workspace, an agent's own configuration, logs,
// session database and package cache land next to the work. A run that wrote a
// single two-byte file came back reporting twenty-eight, and the list was almost
// entirely npm cache objects - the actual output was the least visible thing on
// screen. Skipping them keeps the artifact list about the artifact.
//
// These are listed by name rather than by a blanket dot-prefix rule, at any
// depth: `.github` and `.vscode` are project files a person wants to see, and a
// rule for hidden directories would swallow them along with the state.
var agentStateDirs = map[string]bool{
	".home":        true,
	".sandbox-tmp": true,
	".npm":         true,
	".opencode":    true,
	".config":      true,
	".local":       true,
	".state":       true,
	".cargo":       true,
	".rustup":      true,
	".gradle":      true,
	".m2":          true,
}

// projectDotDirs are the dot-directories that hold work rather than an agent's
// scratch space.
//
// The blocklist above only knows what it has already been bitten by, and a
// scaffolder invents a new scratch directory on every version: a run that wrote
// a working app also left a `.scaffold/` behind holding a throwaway index.html,
// which the preview then chose and the browser then failed to load. Inverting to
// an allowlist means an unknown dot-directory is treated as the agent's own by
// default, and a new scaffolder cannot leak into the artifact list.
//
// Only directories are matched. A dotfile a person wrote - .env.example,
// .gitignore - is a project file and stays visible.
var projectDotDirs = map[string]bool{
	".github":       true,
	".vscode":       true,
	".idea":         true,
	".devcontainer": true,
	".cursor":       true,
	".claude":       true,
}

// skipDirName reports whether a directory is noise rather than output.
func skipDirName(name string) bool {
	if skipDirs[name] || agentStateDirs[name] {
		return true
	}
	if strings.HasPrefix(name, ".") && !projectDotDirs[name] {
		return true
	}
	return false
}

// NewWorkspaceStore prepares root, creating it if needed.
func NewWorkspaceStore(root string) (*WorkspaceStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	// Resolve once. Every later containment check compares against this real
	// path, so a root that is itself a symlink cannot make the prefix test
	// silently fail and hand out the whole filesystem.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	return &WorkspaceStore{root: real}, nil
}

// Root is the directory every workspace lives under.
func (s *WorkspaceStore) Root() string { return s.root }

// Create makes a new workspace and returns its id and path.
//
// The caller owns the directory's lifetime: the store never cleans up, because
// the point is that the output outlives the run.
func (s *WorkspaceStore) Create() (id, dir string, err error) {
	for attempt := 0; attempt < 4; attempt++ {
		id, err = newID()
		if err != nil {
			return "", "", err
		}
		dir = filepath.Join(s.root, id)
		// 0o700: the files in here are whatever an agent decided to write, and
		// they are served back over HTTP to whoever can reach this API.
		if err = os.Mkdir(dir, 0o700); err == nil {
			return id, dir, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("workspace: %w", err)
		}
	}
	return "", "", errors.New("workspace: could not find a free id")
}

// eventLogPath is the sidecar transcript for a run.
//
// It sits next to the workspace rather than inside it, for two reasons. The
// event stream is the control plane's record, not an artifact the agent wrote,
// so it should not appear in the file listing, the archive, or the preview; and
// a run that is still being written must not be able to read or overwrite its
// own transcript through the workspace path handlers.
func (s *WorkspaceStore) eventLogPath(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("workspace: invalid id")
	}
	return filepath.Join(s.root, id+".events.jsonl"), nil
}

// AppendEvents adds already-marshalled JSON events to a run's transcript.
//
// The caller marshals because it is the same bytes that go out on the SSE
// stream: persisting the transcript and serving the stream then cannot drift.
// A failed append is returned rather than logged and swallowed, because a
// transcript that silently stops is worse than one that never started.
func (s *WorkspaceStore) AppendEvents(id string, lines [][]byte) error {
	path, err := s.eventLogPath(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("workspace events: %w", err)
	}
	defer f.Close()
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if _, err := f.Write(line); err != nil {
			return fmt.Errorf("workspace events: %w", err)
		}
		if _, err := f.Write([]byte{'\n'}); err != nil {
			return fmt.Errorf("workspace events: %w", err)
		}
	}
	return nil
}

// ReadEvents returns a run's transcript as newline-delimited JSON.
//
// A run with no transcript returns an error matching fs.ErrNotExist, so the
// handler can tell "this run predates persistence" apart from "the store is
// broken" and answer with an empty transcript rather than a server error.
func (s *WorkspaceStore) ReadEvents(id string) ([]byte, error) {
	path, err := s.eventLogPath(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// newID is a short random id.
//
// Short because a user pastes this into a URL, random because the id is the
// only thing standing between a caller and someone else's run.
func newID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// validID rejects anything that is not exactly a lowercase hex id.
//
// This is the guard that matters most in the file, because the id is joined onto
// the root: without it, ".." is a path traversal and "a/b" reaches outside.
func validID(id string) bool {
	if len(id) != 12 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// Dir resolves an id to its directory.
func (s *WorkspaceStore) Dir(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("workspace: unknown run %q", id)
	}
	dir := filepath.Join(s.root, id)
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("workspace: unknown run %q", id)
	}
	return dir, nil
}

// resolve maps a caller-supplied relative path to a real file inside the
// workspace.
//
// It rejects the syntactic tricks (absolute, .., empty) and then the semantic
// one that a naive implementation forgets: an agent can write a symlink
// pointing at /etc/shadow, and a server that only cleans the string will happily
// serve it. Resolving and re-checking containment is what makes the preview
// endpoint safe to expose.
func (s *WorkspaceStore) resolve(id, rel string) (string, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return "", err
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel != "" {
		clean := filepath.Clean(rel)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", errors.New("workspace: path escapes the run directory")
		}
		rel = clean
		for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
			if agentStateDirs[segment] {
				return "", errors.New("workspace: agent state is not a project artifact")
			}
		}
	}
	full := filepath.Join(dir, rel)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("workspace: %s: %w", rel, err)
	}
	if resolved != dir && !strings.HasPrefix(resolved, dir+string(filepath.Separator)) {
		return "", errors.New("workspace: path escapes the run directory")
	}
	resolvedRel, err := filepath.Rel(dir, resolved)
	if err != nil {
		return "", err
	}
	for _, segment := range strings.Split(filepath.ToSlash(resolvedRel), "/") {
		if agentStateDirs[segment] {
			return "", errors.New("workspace: agent state is not a project artifact")
		}
	}
	return resolved, nil
}

// List describes a workspace's contents.
func (s *WorkspaceStore) List(id string) (Listing, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return Listing{}, err
	}
	out := Listing{ID: id, Dir: dir, Files: []WorkspaceFile{}}

	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry is not worth failing the whole listing over.
			return nil
		}
		if d.IsDir() {
			if p != dir && skipDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		// A symlink is skipped rather than resolved. The raw endpoint refuses one
		// that points outside the run, so listing it would advertise a file that
		// cannot be opened, and following it into the listing would cost a stat
		// per link for no benefit.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// The file-level check only covers names that are directories in practice.
		// Skipping a regular file called ".local" would hide a dotfile a person
		// wrote, and nothing produces one by accident.
		if skipDirs[filepath.Base(p)] {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		out.Total++
		if len(out.Files) >= maxListedFiles {
			out.Truncated = true
			return filepath.SkipAll
		}
		info, ierr := d.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		out.Bytes += size
		out.Files = append(out.Files, WorkspaceFile{
			Path:    filepath.ToSlash(rel),
			Kind:    classify(filepath.ToSlash(rel)),
			Size:    size,
			ModTime: info.ModTime(),
		})
		return nil
	})
	if walkErr != nil {
		return Listing{}, walkErr
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	out.Preview = pickPreview(out.Files)
	return out, nil
}

// Runs lists the known workspaces, most recently touched first, so a user can
// find the output of a run they have scrolled away from.
func (s *WorkspaceStore) Runs(limit int) []Run {
	if limit <= 0 {
		limit = 20
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return []Run{}
	}
	out := make([]Run, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		r := Run{ID: e.Name(), Dir: filepath.Join(s.root, e.Name())}
		if info, err := e.Info(); err == nil {
			r.ModTime = info.ModTime()
		}
		r.RunMetadata = s.metadata(e.Name())
		if !r.StartedAt.IsZero() {
			r.ModTime = r.StartedAt
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	if len(out) > limit {
		out = out[:limit]
	}
	// Only walk the requested page, not every historical dependency tree.
	for i := range out {
		if l, err := s.List(out[i].ID); err == nil {
			out[i].Files = len(l.Files)
		}
	}
	return out
}

// Open returns a file's contents plus its size, refusing anything that is not a
// readable regular file inside the workspace.
//
// The file is an *os.File rather than an io.Reader because the handler serves it
// with http.ServeContent, which needs to seek in order to honour Range requests.
// A preview that cannot be reloaded or partially fetched is a worse preview.
func (s *WorkspaceStore) Open(id, rel string) (*os.File, int64, error) {
	full, err := s.resolve(id, rel)
	if err != nil {
		return nil, 0, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return nil, 0, fmt.Errorf("workspace: %w", err)
	}
	if fi.IsDir() {
		return nil, 0, errors.New("workspace: not a file")
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, errors.New("workspace: not a regular file")
	}
	if fi.Size() > maxPreviewBytes {
		return nil, 0, fmt.Errorf("workspace: file is larger than %d bytes", maxPreviewBytes)
	}
	dir, err := s.Dir(id)
	if err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, 0, err
	}
	defer root.Close()
	relative, err := filepath.Rel(dir, full)
	if err != nil {
		return nil, 0, err
	}
	// OpenRoot prevents a symlink swap between validation and opening the file.
	f, err := root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("workspace: %w", err)
	}
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || actual.Size() > maxPreviewBytes {
		_ = f.Close()
		return nil, 0, errors.New("workspace: not a previewable regular file")
	}
	return f, actual.Size(), nil
}

// classify buckets a path so the frontend can pick an icon and a syntax mode
// without shipping a table of its own.
func classify(path string) string {
	switch strings.ToLower(pathExt(path)) {
	case "html", "htm":
		return "html"
	case "css":
		return "css"
	case "js", "mjs", "cjs":
		return "js"
	case "jsx", "ts", "tsx":
		return "ts"
	case "json":
		return "json"
	case "md", "txt", "csv":
		return "text"
	case "svg", "png", "jpg", "jpeg", "gif", "webp", "ico", "avif":
		return "image"
	case "woff", "woff2", "ttf", "otf", "eot":
		return "font"
	case "pdf":
		return "pdf"
	default:
		return "file"
	}
}

func pathExt(p string) string {
	i := strings.LastIndex(p, ".")
	if i < 0 {
		return ""
	}
	return p[i+1:]
}

// pickPreview chooses the file a user most likely wants to look at.
//
// Agents scaffold a Vite app with an index.html that cannot render on its own and
// put the real page in dist/ or build/ once they build, so a built directory wins
// over a source one even when the source file is at the top level.
func pickPreview(files []WorkspaceFile) string {
	best := ""
	bestScore := -1
	for _, f := range files {
		if f.Kind != "html" || inHiddenTree(f.Path) {
			continue
		}
		score := 100 - strings.Count(f.Path, "/")*5
		if hasBuildDir(f.Path) {
			score += 50
		}
		if base := pathBase(f.Path); base == "index.html" {
			score += 25
		}
		if score > bestScore {
			best, bestScore = f.Path, score
		}
	}
	return best
}

// inHiddenTree reports whether a path sits inside a dot-directory that is not a
// known project directory.
//
// The preview is chosen from the listing, which already skips these, so this is
// a second line of defence: an index.html inside an agent's scratch space is not
// something to frame in the dashboard, and if one ever reaches here it should be
// passed over rather than rendered.
func inHiddenTree(rel string) bool {
	segs := strings.Split(rel, "/")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs[:len(segs)-1] {
		if strings.HasPrefix(seg, ".") && !projectDotDirs[seg] {
			return true
		}
	}
	return false
}

// buildDirs are where agents put the page that actually renders.
var buildDirs = map[string]bool{
	"dist": true, "build": true, "out": true, "public": true,
	"_site": true, ".next": true,
}

// hasBuildDir reports whether any directory segment is a build output directory.
//
// The segments are compared rather than the string: "dist/index.html" contains
// no "/dist/" and a substring test would silently never fire, which is exactly
// the bug this replaced.
func hasBuildDir(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if buildDirs[strings.ToLower(seg)] {
			return true
		}
	}
	return false
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
