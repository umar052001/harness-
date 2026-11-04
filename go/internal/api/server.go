// Package api serves build jobs, saved projects, previews, and app services.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/umar052001/ai-os/go/internal/environment"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/narrator"
	"github.com/umar052001/ai-os/go/internal/planner"
	"github.com/umar052001/ai-os/go/internal/runner"
)

// Runner is the slice of the runner the API needs. Keeping it an interface, and
// small, means the API can be tested against a stub without the Rust sandbox
// and without standing up a real model provider.
type Runner interface {
	Run(ctx context.Context, req runner.Request, emit func(runner.Event)) error
	Registry() *harness.Registry
}

// Options configures the server.
type Options struct {
	// SandboxBinary is the executor path, reported by /api/health.
	SandboxBinary string
	// Planner routes requests to an agent; nil disables planning.
	Planner *planner.Planner
	// Origins is the CORS allow-list. Kept explicit rather than "*" because the
	// stream carries sandbox enforcement evidence, and reflecting whatever
	// origin asks is a way to hand that to a page it was not meant for.
	Origins []string
	// WebDir serves the built frontend from disk. Empty disables static serving,
	// which is the mode tests and development use.
	WebDir string
	// WorkspacesDir is where run directories are created. Empty picks a
	// per-user default. Runs are durable here, which is what lets the frontend
	// show what a run produced after the agent has exited.
	WorkspacesDir string
	// MaxConcurrentRuns caps how many runs may be in flight at once. Zero picks
	// a default of one, which is the honest setting for a single-machine control
	// plane: two agents competing for disk and CPU look like one flaky agent.
	MaxConcurrentRuns int
	// WorkflowRoot enables the host-owned routed OpenCode workflow.
	WorkflowRoot string
}

// Server is an HTTP server for the ai-os control plane.
type Server struct {
	apps         environment.Apps
	environments *environment.Manager
	jobContext   context.Context
	cancelJobs   context.CancelFunc
	jobsMu       sync.Mutex
	jobs         sync.WaitGroup
	stopping     bool
	run          Runner
	sandboxBin   string
	planner      *planner.Planner
	origins      map[string]bool
	webDir       string
	heartbeat    time.Duration
	sandboxReady bool
	sandboxError string
	// store owns run workspaces. It is nil only when the root could not be
	// prepared, in which case runs still work but their output is temporary.
	store *WorkspaceStore
	// storeErr is why the store is unavailable, reported by /api/health so a
	// dashboard can say "no artifacts" instead of silently showing nothing.
	storeErr string
	// publishIndex records which runs have a live site address.
	publishIndex *publishIndex
	// active is the live half of run state: what is running now, as opposed to
	// what has finished and left a workspace behind.
	active       *runRegistry
	workflowRoot string
}

// New builds a server around a runner.
func New(run Runner, opt Options) *Server {
	origins := make(map[string]bool, len(opt.Origins))
	for _, o := range opt.Origins {
		if o != "" {
			origins[o] = true
		}
	}
	root := opt.WorkspacesDir
	if root == "" {
		root = defaultWorkspacesDir()
	}
	store, err := NewWorkspaceStore(root)
	s := &Server{
		run:          run,
		workflowRoot: opt.WorkflowRoot,
		sandboxBin:   opt.SandboxBinary,
		planner:      opt.Planner,
		origins:      origins,
		webDir:       opt.WebDir,
		heartbeat:    15 * time.Second,
		store:        store,
		storeErr:     errText(err),
		active:       newRunRegistry(opt.MaxConcurrentRuns),
	}
	if store != nil {
		s.publishIndex = newPublishIndex(store.Root())
		s.environments = environment.New(filepath.Join(store.Root(), ".environments"))
	}
	s.jobContext, s.cancelJobs = context.WithCancel(context.Background())
	if opt.SandboxBinary != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		output, probeErr := exec.CommandContext(ctx, opt.SandboxBinary, "probe").CombinedOutput()
		s.sandboxReady = probeErr == nil
		if probeErr != nil {
			s.sandboxError = strings.TrimSpace(string(output)) + ": " + probeErr.Error()
		}
	}
	return s
}

// defaultWorkspacesDir is where runs land when the operator names no root.
//
// Per-user and outside the system temp directory on purpose: /tmp is cleared on
// reboot and shared with every other user, and neither is a good home for
// generated work.
func defaultWorkspacesDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "harness++", "workspaces")
	}
	return filepath.Join(os.TempDir(), "harness-workspaces")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// elide shortens a prompt for the active-run listing. The registry is polled
// frequently and is not a place to carry a multi-kilobyte instruction; the first
// line is enough for a human to recognise which run they are waiting on.
func elide(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

// Handler returns the full HTTP handler: API routes, CORS, and the frontend
// when one is configured.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/harnesses", s.handleHarnesses)
	mux.HandleFunc("POST /api/run", s.handleRun)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/narrate", s.handleNarrate)
	mux.HandleFunc("GET /api/runs", s.handleRuns)
	mux.HandleFunc("GET /api/runs/active", s.handleActiveRuns)
	mux.HandleFunc("POST /api/runs/{id}/stop", s.handleStopRun)
	mux.HandleFunc("GET /api/workspace/{id}/database", s.handleDatabaseStatus)
	mux.HandleFunc("POST /api/workspace/{id}/database", s.handleDatabaseStart)
	mux.HandleFunc("DELETE /api/workspace/{id}/database", s.handleDatabaseStop)
	mux.HandleFunc("GET /api/workspace/{id}/app", s.handleApp)
	mux.HandleFunc("GET /api/workflow", s.handleWorkflow)
	mux.HandleFunc("POST /api/workspace/{id}/app", s.handleApp)
	mux.HandleFunc("DELETE /api/workspace/{id}/app", s.handleApp)
	mux.HandleFunc("GET /api/workspace/{id}", s.handleWorkspaceList)
	mux.HandleFunc("GET /api/workspace/{id}/events", s.handleWorkspaceEvents)
	mux.HandleFunc("GET /api/workspace/{id}/raw/{path...}", s.handleWorkspaceRaw)
	mux.HandleFunc("GET /api/workspace/{id}/archive", s.handleWorkspaceArchive)
	mux.HandleFunc("GET /api/workspace/{id}/publish", s.handlePublish)
	mux.HandleFunc("POST /api/workspace/{id}/publish", s.handlePublish)

	// A published run is addressed by hostname, and that has to be decided
	// before anything else looks at the path.
	//
	// staticHandler serves the built frontend for every path that is not an API
	// route, so it would answer "/" with the dashboard and the published site
	// would never be reached. The order here is the whole mechanism: hostname
	// first, then the dashboard's own routing.
	var routes http.Handler = mux
	if s.webDir != "" {
		routes = staticHandler(s.webDir, routes)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.publishedSite(w, r) {
			return
		}
		routes.ServeHTTP(w, r)
	})
	return s.withCORS(h)
}

// ---------------------------------------------------------------------------
// + type
// ---------------------------------------------------------------------------

type runRequest struct {
	AllowGPT    bool     `json:"allowGPT,omitempty"`
	ParentRunID string   `json:"parentRunId,omitempty"`
	HarnessID   string   `json:"harnessId"`
	Prompt      string   `json:"prompt"`
	Workspace   string   `json:"workspace,omitempty"`
	Models      []string `json:"models,omitempty"`
	AutoApprove bool     `json:"autoApprove,omitempty"`
	TimeoutSec  int      `json:"timeoutSec,omitempty"`
}

// handleRun starts a host-owned job and attaches its initial SSE observer.
// A disconnected observer can recover through run history and the event log.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req runRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.TimeoutSec < 0 || req.TimeoutSec > 1800 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "time limit must be 0–1800 seconds (0 means no overall limit)"})
		return
	}
	if len(req.Models) > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "choose one model per run"})
		return
	}
	if _, err := s.run.Registry().Lookup(req.HarnessID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace storage unavailable: " + s.storeErr})
		return
	}
	if req.HarnessID == "opencode" && s.workflowRoot != "" {
		if err := s.validateWorkflowRequest(req); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
	} else if req.HarnessID == "opencode" && (len(req.Models) == 0 || strings.TrimSpace(req.Models[0]) == "") {
		req.Models = []string{"opencode/big-pickle"}
	}
	if req.ParentRunID != "" {
		if _, err := s.store.Dir(req.ParentRunID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "parent run does not exist"})
			return
		}
	}

	// Take a concurrency slot before anything durable is created. Refusing here
	// rather than mid-stream means the client gets a plain 409 it can explain,
	// instead of a stream that opens and immediately dies.
	token, aerr := s.active.acquire(ActiveRun{
		HarnessID: req.HarnessID,
		Prompt:    elide(req.Prompt, 160),
		StartedAt: time.Now(),
	})
	if aerr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":  "a run or service operation is already in flight; wait for it to finish or stop the active run",
			"active": s.active.list(),
		})
		return
	}
	if !s.beginJob() {
		s.active.release(token)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "server is shutting down"})
		return
	}
	dispatched := false
	defer func() {
		if !dispatched {
			s.active.release(token)
			s.jobs.Done()
		}
	}()
	if req.ParentRunID != "" && s.apps.Status(req.ParentRunID).Active {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "stop the parent app before copying its files and database into a new attempt"})
		return
	}
	for _, live := range s.active.list() {
		if req.ParentRunID != "" && live.RunID == req.ParentRunID {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "wait for the parent run to stop before continuing"})
			return
		}
	}

	runReq := runner.Request{
		AllowGPT:    req.AllowGPT,
		HarnessID:   req.HarnessID,
		Prompt:      req.Prompt,
		Models:      req.Models,
		AutoApprove: req.AutoApprove,
	}

	// The workspace is created here rather than left to the runner's scratch
	// directory, because the scratch directory is deleted when the run ends and
	// that is precisely the output the user came for. The client is told the id
	// up front so it can list and preview files while the agent is still writing
	// them, not only afterwards.
	var runID string
	if s.store != nil {
		var id, dir string
		var serr error
		if req.ParentRunID == "" {
			id, dir, serr = s.store.Create()
		} else {
			id, dir, serr = s.store.Continue(r.Context(), req.ParentRunID)
			runReq.Prompt = "Continue the existing project in this workspace. Preserve existing functionality unless the request changes it. Agent history and dependency caches are not included; inspect the relevant source files first.\n\n" + req.Prompt
		}
		if serr != nil {
			http.Error(w, "workspace: "+serr.Error(), http.StatusInternalServerError)
			return
		}
		runID = id
		runReq.Workspace = dir
		defer func() {
			if !dispatched {
				_ = os.RemoveAll(dir)
				_ = os.Remove(filepath.Join(s.store.Root(), id+".json"))
				if path, err := s.store.eventLogPath(id); err == nil {
					_ = os.Remove(path)
				}
			}
		}()
	} else {
		runReq.Workspace = req.Workspace
	}
	if req.TimeoutSec > 0 {
		runReq.Timeout = time.Duration(req.TimeoutSec) * time.Second
	}
	// The registry entry stays without a run id until the workspace exists, then
	// is completed here. A poller that arrives in between sees a live run with an
	// empty id rather than no run at all, which is the truthful answer.
	if runID != "" {
		s.active.identify(token, runID, runReq.Workspace)
	}
	ctx, cancel := context.WithCancel(s.jobContext)
	if runReq.Timeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(s.jobContext, runReq.Timeout)
	}
	defer func() {
		if !dispatched {
			cancel()
		}
	}()
	s.active.setCancel(token, cancel)
	meta := RunMetadata{Prompt: req.Prompt, HarnessID: req.HarnessID, Status: "running", StartedAt: time.Now()}
	meta.ProjectID = runID
	meta.ParentRunID = req.ParentRunID
	if req.ParentRunID != "" {
		meta.ProjectID = s.store.metadata(req.ParentRunID).ProjectID
		if meta.ProjectID == "" {
			meta.ProjectID = req.ParentRunID
		}
	}
	if len(req.Models) > 0 {
		meta.Model = req.Models[0]
	} else if s.workflowRoot != "" && req.HarnessID == "opencode" {
		meta.Model = "Automatic · Gemma-first"
	}
	serviceCtx, serviceCancel := context.WithTimeout(ctx, 10*time.Second)
	runReq.DatabaseURL, err = s.environments.Connection(serviceCtx, meta.ProjectID)
	serviceCancel()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "project database: " + err.Error()})
		return
	}
	if runReq.DatabaseURL != "" {
		runReq.Prompt = "A project PostgreSQL database is available through DATABASE_URL in your environment. Read that variable in code; never print or hardcode the connection string. Preserve existing data and use migrations for schema changes.\n\n" + runReq.Prompt
	}
	if s.workflowRoot == "" || req.HarnessID != "opencode" {
		runReq.Prompt = environment.AppInstructions + runReq.Prompt
	}
	if err := s.store.saveMetadata(runID, meta); err != nil {
		http.Error(w, "could not save run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	logPath, _ := s.store.eventLogPath(runID)
	transcript, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		http.Error(w, "could not open transcript: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		if !dispatched {
			_ = transcript.Close()
		}
	}()
	var recordMu sync.Mutex
	record := func(payload []byte) {
		recordMu.Lock()
		defer recordMu.Unlock()
		if _, err := transcript.Write(append(payload, '\n')); err != nil {
			log.Printf("transcript %s: %v", runID, err)
			cancel()
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sink := &sseSink{w: w, flusher: flusher}
	defer sink.close()

	// A heartbeat keeps intermediaries from treating a silent agent as a dead
	// connection, and gives a reader the same signal a TCP keepalive does. It is
	// a comment per spec, invisible to an event parser.
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(s.heartbeat)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				sink.raw(": ping\n\n")
			case <-done:
				return
			}
		}
	}()

	// run.assigned comes before anything the runner emits so the frontend can
	// attach its artifacts panel to a directory that is about to start filling.
	if runID != "" {
		payload, merr := json.Marshal(map[string]any{
			"kind": "run.assigned", "runId": runID, "workspace": runReq.Workspace,
		})
		if merr == nil {
			sink.frame("run.assigned", string(payload))
			record(payload)
		}
	}

	jobDone := make(chan struct{})
	dispatched = true
	// This worker owns the run until terminal state and process cleanup complete.
	// The handler may return earlier; its closed sink drops subsequent live writes.
	go func() {
		defer s.jobs.Done()
		defer close(jobDone)
		defer s.active.release(token)
		defer cancel()
		defer transcript.Close()
		s.executeJob(ctx, runReq, runID, meta, record, sink)
	}()
	select {
	case <-jobDone:
	case <-r.Context().Done():
	}
}

// sseSink serialises writes to one event stream.
// Jobs outlive browser connections, so close must prevent late handler writes.
type sseSink struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	closed  bool
}

// frame writes one complete SSE frame and flushes it.
//
// The whole frame goes out in a single write. Building it in one string and
// locking once is what makes "the bytes of a frame land together" true by
// construction: the mutex is held for the entire frame, so a heartbeat cannot
// be interleaved between an event's name and its data, and a reader can never
// observe half an event.
func (s *sseSink) frame(event, data string) {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	if data != "" {
		b.WriteString("data: ")
		b.WriteString(data)
		b.WriteString("\n\n")
	} else {
		b.WriteByte('\n')
	}
	s.raw(b.String())
}

// raw writes arbitrary bytes, for comments such as the heartbeat.
func (s *sseSink) raw(b string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// A closed connection makes Write fail, which is not a problem; a recycled
	// writer makes it panic, which is.
	defer func() { _ = recover() }()
	_ = http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(s.w, b)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// close stops the sink. After this, every write is dropped.
func (s *sseSink) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// handleHealth reports whether the control plane can actually confine a run.
// "ok" means the server is up; sandboxdOk is the executor being present, which
// is the difference between a dashboard that renders and a run that can start.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	bin := s.sandboxBin
	body := map[string]any{
		"ok":           true,
		"sandboxd":     bin,
		"sandboxdOk":   s.sandboxReady,
		"sandboxError": s.sandboxError,
		"plannerOk":    s.planner != nil && s.planner.Configured(),
		"artifactsOk":  s.store != nil,
		"activeRuns":   len(s.active.list()),
		"maxRuns":      s.active.limit,
	}
	if s.store != nil {
		body["workspaces"] = s.store.Root()
	} else if s.storeErr != "" {
		body["artifactsError"] = s.storeErr
	}
	writeJSON(w, http.StatusOK, body)
}

// handleNarrate answers "what is happening right now" in one sentence.
//
// It always answers, even with no model key, because a status line that
// disappears when a provider is unconfigured is worse than a plain one. The
// response says which kind it is so the UI can be honest about it.
func (s *Server) handleNarrate(w http.ResponseWriter, r *http.Request) {
	var st narrator.Status
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&st); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	if strings.TrimSpace(st.Phase) == "" {
		st.Phase = "working"
	}
	sentence, source := narrator.Narrate(r.Context(), s.modelBackend(), st)
	writeJSON(w, http.StatusOK, map[string]any{
		"narration": sentence,
		"source":    source,
		"model":     source == narrator.SourceModel,
	})
}

// modelBackend returns the planner as a narrator.Backend, or a nil interface
// when there is no planner.
//
// The explicit conversion is the point: handing a nil *planner straight to an
// interface produces a non-nil interface holding a nil pointer, which looks
// configured to every `backend == nil` check and then panics on first use.
func (s *Server) modelBackend() narrator.Backend {
	if s.planner == nil {
		return nil
	}
	return s.planner
}

// handleRuns lists previous runs so output does not become unreachable when the
// user navigates away from the stream that produced it.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": s.storeErr})
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	runs := s.store.Runs(limit)
	live := map[string]bool{}
	for _, run := range s.active.list() {
		live[run.RunID] = true
	}
	for i := range runs {
		if runs[i].Status == "running" && !live[runs[i].ID] {
			runs[i].Status = "interrupted"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func mustJSONStatus(ok bool, status, reason string) []byte {
	raw, _ := json.Marshal(map[string]any{"ok": ok, "status": status, "error": reason})
	return raw
}

func (s *Server) handleStopRun(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("id")) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid run id"})
		return
	}
	if !s.active.cancel(r.PathValue("id")) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run is no longer active"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"stopping": true})
}

// handleActiveRuns answers "what is running right now", the question a list of
// finished workspaces cannot. The frontend polls it so a run started in another
// tab - or one this tab lost the stream for - is still visibly alive.
func (s *Server) handleActiveRuns(w http.ResponseWriter, r *http.Request) {
	active := s.active.list()
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active,
		"limit":  s.active.limit,
	})
}

// handleWorkspaceEvents replays the recorded transcript of a run.
//
// The live stream is only available while the run is happening; this is what
// makes the Logs view mean something for a run from yesterday. Events are
// returned as a JSON array in the same shape the stream delivered, so the
// frontend renders history and live output through one code path.
//
// A run with no transcript - one that predates this endpoint - answers with an
// empty list rather than 404, because "no events were recorded" and "no such
// run" are different facts and the UI shows them differently.
func (s *Server) handleWorkspaceEvents(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": s.storeErr})
		return
	}
	id := r.PathValue("id")
	raw, err := s.store.ReadEvents(id)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusOK, map[string]any{"events": []json.RawMessage{}})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	events := make([]json.RawMessage, 0, bytes.Count(raw, []byte{'\n'}))
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// A corrupt line is skipped rather than failing the whole history. A
		// half-written final line is the expected case: a run that was killed
		// mid-write leaves one. Losing that is fine; losing the transcript is
		// not.
		if !json.Valid(line) {
			continue
		}
		events = append(events, json.RawMessage(append([]byte(nil), line...)))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleWorkspaceList describes what a run produced.
func (s *Server) handleWorkspaceList(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": s.storeErr})
		return
	}
	listing, err := s.store.List(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

// handleWorkspaceRaw serves one file from a run's workspace.
//
// The response is framed as an opaque origin on purpose. These bytes are
// whatever an agent decided to write, and they are framed by the same origin
// that holds the control plane's API, so without this a generated page could
// script against the dashboard that is showing it.
//
// Two consequences of that isolation are handled here. A module script is
// fetched with CORS, so the sandboxed document cannot load its own bundle
// unless the response opts it in - hence Allow-Origin: null, which admits the
// sandboxed preview and nothing else. And a built site addresses its own root,
// which is not this origin, so references are rewritten to the workspace mount.
func (s *Server) handleWorkspaceRaw(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, s.storeErr, http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	rel := r.PathValue("path")
	f, _, err := s.store.Open(id, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	ct := contentTypeFor(rel)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	// Only an opaque origin - a sandboxed document - is admitted. A normal page
	// on another site cannot read a run's files through this header.
	w.Header().Set("Access-Control-Allow-Origin", "null")

	// `?download=1` hands the file over instead of showing it. Nothing is
	// rewritten - a download is not a preview, and rewriting the bytes would
	// mean saving back a file the agent wrote - and the sandbox CSP is dropped
	// because there is no document here to sandbox.
	if r.URL.Query().Get("download") == "1" {
		w.Header().Del("Content-Security-Policy")
		w.Header().Del("Access-Control-Allow-Origin")
		w.Header().Set("Content-Disposition", "attachment; filename=\""+downloadFilename(filepath.Base(rel))+"\"")
		http.ServeContent(w, r, filepath.Base(rel), time.Time{}, f)
		return
	}

	if !rewritableContentType(ct) {
		http.ServeContent(w, r, filepath.Base(rel), time.Time{}, f)
		return
	}

	// Rewriting means holding the file, so it is bounded rather than streamed.
	// A preview does not need Range support; a multi-gigabyte log does not need a
	// rewrite, and it is served by the branch above.
	raw, err := io.ReadAll(io.LimitReader(f, previewRewriteLimit+1))
	if err != nil || int64(len(raw)) > previewRewriteLimit {
		if _, serr := f.Seek(0, io.SeekStart); serr == nil {
			http.ServeContent(w, r, filepath.Base(rel), time.Time{}, f)
		}
		return
	}

	previewFile := ""
	if listing, lerr := s.store.List(id); lerr == nil {
		previewFile = listing.Preview
	}
	siteRoot := previewRootFor(rel, strings.HasPrefix(ct, "text/html"), previewFile)
	body := rewriteRootRelative(string(raw), siteRoot, mountFor(id))
	http.ServeContent(w, r, filepath.Base(rel), time.Time{}, strings.NewReader(body))
}

// previewRewriteLimit bounds how much of a text file is rewritten in memory.
const previewRewriteLimit = 8 << 20

// contentTypeFor maps an extension to a media type.
//
// The mapping is deliberately small and explicit. Guessing from content or
// defaulting everything to text/html would let a run serve an arbitrary file as
// a page in the dashboard's own origin.
func contentTypeFor(p string) string {
	switch strings.ToLower(pathExt(filepath.Base(p))) {
	case "html", "htm":
		return "text/html; charset=utf-8"
	case "css":
		return "text/css; charset=utf-8"
	case "js", "mjs", "cjs":
		return "text/javascript; charset=utf-8"
	case "json", "map":
		return "application/json"
	case "md", "txt", "log", "yaml", "yml", "toml", "lock", "cfg", "ini", "env", "sh":
		// Text, never text/html: a .md that a browser would parse as a page is a
		// file an agent wrote, and the only thing that should render it as a page
		// is the build output that was meant to be a page.
		return "text/plain; charset=utf-8"
	case "ts", "tsx", "jsx", "py", "go", "rs", "c", "h", "cpp", "hpp", "java", "rb", "php", "sql", "vue", "svelte":
		return "text/plain; charset=utf-8"
	case "svg":
		return "image/svg+xml"
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "ico":
		return "image/x-icon"
	case "woff2":
		return "font/woff2"
	case "woff":
		return "font/woff"
	case "ttf":
		return "font/ttf"
	case "otf":
		return "font/otf"
	case "pdf":
		return "application/pdf"
	case "wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}

// planRequest is the body of POST /api/plan.
type planRequest struct {
	Prompt string `json:"prompt"`
}

// handlePlan routes a request through the hosted planner. It is a one-shot
// endpoint, not a stream: the plan is a small JSON document and the caller
// needs it whole before it can decide what to run.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	if s.planner == nil || !s.planner.Configured() {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"error":     "planner is not configured: set OLLAMA_API_KEY",
		})
		return
	}
	var req planRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad json: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prompt is required"})
		return
	}
	plan, err := s.planner.Plan(r.Context(), req.Prompt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"available": true, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "plan": plan})
}

// handleHarnesses lists the registered agents for the picker.
func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"harnesses": s.run.Registry().Summaries(),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// withCORS applies the allow-list to API and static responses alike.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			u, err := url.Parse(origin)
			sameHost := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host == r.Host
			if !sameHost && !s.origins[origin] {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": "origin is not allowed to control runs"})
				return
			}
		}
		if origin != "" && s.origins[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// staticHandler serves the built frontend and falls back to index.html for any
// path that is not an API route, so the SPA owns its routing.
func staticHandler(dir string, api http.Handler) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		p := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}
