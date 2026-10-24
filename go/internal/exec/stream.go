package exec

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	sandboxv1 "github.com/umar052001/ai-os/go/gen/sandbox/v1"
)

// StreamEvent is one line of the executor's NDJSON output.
//
// The fields are a flattened union of the four event shapes rather than a nested
// struct, because the alternative is a type switch at every call site for four
// types that are used in four different ways. Unused fields stay zero.
type StreamEvent struct {
	Type string `json:"type"`

	// run.started
	RunID        string   `json:"runId,omitempty"`
	Exec         []string `json:"exec,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	EffectiveABI int      `json:"effectiveAbi,omitempty"`
	Network      string   `json:"network,omitempty"`
	Workspace    string   `json:"workspace,omitempty"`
	GrantedPaths []string `json:"grantedPaths,omitempty"`
	RuleCount    int      `json:"ruleCount,omitempty"`

	// output
	Seq   int    `json:"seq,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	Data  string `json:"data,omitempty"`

	// enforcement
	Enforcement *sandboxv1.Enforcement `json:"enforcement,omitempty"`

	// exit
	ExitCode  int    `json:"exitCode,omitempty"`
	Signal    int    `json:"signal,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	DurationN string `json:"durationNs,omitempty"`
}

// Duration parses the protojson int64 string. The executor emits an int64 as a
// string because protojson does, and a nanosecond count does not survive a
// float64.
func (e StreamEvent) Duration() time.Duration {
	if e.DurationN == "" {
		return 0
	}
	ns, err := strconv.ParseInt(e.DurationN, 10, 64)
	if err != nil {
		return 0
	}
	return time.Duration(ns)
}

// StreamRequest starts a streaming execution.
type StreamRequest struct {
	// Workspace is the writable root.
	Workspace string
	// Cwd optionally overrides the working directory.
	Cwd string
	// Argv is the command. The first element is the program.
	Argv []string
	// Env is the child's environment, built explicitly.
	Env []string
	// ReadPaths are extra read+execute grants.
	ReadPaths []string
	// AllowNet lifts the network deny.
	AllowNet bool
	// TimeoutMs bounds the run; zero means no deadline.
	TimeoutMs int

	// Stdin, when set, is wired to the child in place of the executor's own
	// /dev/null. This is how an agent's JSON-RPC reaches the process that is
	// being confined.
	Stdin io.Reader
	// Stdout, when set, receives the child's stdout. In pty mode this moves the
	// executor's NDJSON to stderr, because stdout now belongs to the program
	// being run.
	Stdout io.Writer
	// Stderr, when set, receives the child's stderr.
	Stderr io.Writer

	// Stdio selects the executor's transport. Empty means pty.
	Stdio string
	// NetWhy is required with AllowNet, and is recorded by the executor in the
	// policy report, the attestation and the response file.
	NetWhy string
}

// Transport names, kept as constants because they are part of a command-line
// contract with a Rust binary and a typo here is a runtime failure, not a
// compile error.
const (
	StdioPTY  = "pty"
	StdioPipe = "pipe"
)

// Stream is a running sandboxed process.
//
// Events are delivered on a channel that is closed when the run ends, so a caller
// can range over it and be certain it has seen every event.
type Stream struct {
	cmd        *exec.Cmd
	events     chan StreamEvent
	eventsDone chan struct{}

	waitOnce    sync.Once
	waitCode    int
	stopOnce    sync.Once
	closed      chan struct{}
	closeOne    sync.Once
	stopping    chan struct{}
	processDone chan struct{}
	eventReader io.Closer
}

// Events returns the event channel. It is closed when the run ends.
func (s *Stream) Events() <-chan StreamEvent { return s.events }

// Stream starts a streaming execution.
func (c *Client) Stream(ctx context.Context, req *StreamRequest) (*Stream, error) {
	if len(req.Argv) == 0 {
		return nil, fmt.Errorf("exec: argv is empty")
	}
	if c.Binary == "" {
		return nil, fmt.Errorf("exec: sandbox binary is not set")
	}
	if req.Workspace == "" {
		return nil, fmt.Errorf("exec: workspace is required")
	}
	if err := os.MkdirAll(req.Workspace, 0o700); err != nil {
		return nil, fmt.Errorf("exec: creating workspace: %w", err)
	}

	args := []string{"exec", "--workspace", req.Workspace}
	if req.Cwd != "" {
		args = append(args, "--cwd", req.Cwd)
	}
	for _, p := range req.ReadPaths {
		args = append(args, "--read", p)
	}
	if req.AllowNet {
		// A network grant has to say what it is for. The executor refuses an
		// unlabelled one, which is the behaviour we want: this is the line of
		// code where a silent widening of the sandbox would otherwise live.
		args = append(args, "--allow-net")
		if req.NetWhy == "" {
			return nil, fmt.Errorf("exec: AllowNet needs NetWhy: a network grant has to say what it is for")
		}
		args = append(args, "--net-why", req.NetWhy)
	}
	if req.Stdio != "" {
		args = append(args, "--stdio", req.Stdio)
		// Pipe mode puts the event stream on the first extra file, which is fd 3
		// in the child. The number is not a preference: it is the contract
		// between this flag and cmd.ExtraFiles below, and the executor refuses to
		// start in pipe mode without it rather than guessing.
		if req.Stdio == StdioPipe {
			args = append(args, "--events-fd", "3")
		}
	}
	if req.TimeoutMs > 0 {
		args = append(args, "--timeout-ms", strconv.Itoa(req.TimeoutMs))
	}
	for _, e := range req.Env {
		args = append(args, "--env", e)
	}
	args = append(args, "--")
	args = append(args, req.Argv...)

	cmd := exec.Command(c.Binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	cmd.WaitDelay = 3 * time.Second

	s := &Stream{
		cmd:         cmd,
		events:      make(chan StreamEvent, 256),
		eventsDone:  make(chan struct{}),
		closed:      make(chan struct{}),
		stopping:    make(chan struct{}),
		processDone: make(chan struct{}),
	}

	// Where the NDJSON event stream comes from.
	//
	// Pipe mode gets a descriptor of its own, passed to the executor as fd 3.
	// The alternative - reading events out of the child's stderr and skipping
	// lines that do not parse - would mean a tool that happens to print JSON
	// becomes a source of fabricated enforcement evidence, and a tool that
	// prints nothing leaves the control plane with no idea whether the sandbox
	// is working.
	pipeMode := req.Stdio == StdioPipe
	var eventSrc io.Reader
	var errPipe io.ReadCloser

	if pipeMode {
		evR, evW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("exec: events pipe: %w", err)
		}
		// ExtraFiles[0] is fd 3 in the child.
		cmd.ExtraFiles = []*os.File{evW}
		eventSrc = evR
	} else if req.Stdout != nil {
		cmd.Stdout = req.Stdout
		p, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("exec: stderr: %w", err)
		}
		errPipe = p
		eventSrc = p
	} else {
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("exec: stdout: %w", err)
		}
		eventSrc = out
	}
	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	}
	// In pipe mode the child's stdout is genuinely the program's, not a channel
	// the executor is borrowing, so it is assigned the same way in both modes.
	if pipeMode {
		cmd.Stdout = req.Stdout
	}
	if req.Stderr != nil {
		cmd.Stderr = req.Stderr
	}

	if err := cmd.Start(); err != nil {
		if errPipe != nil {
			_ = errPipe.Close()
		}
		// Start consumed the write end; the read end still has to go.
		if pipeMode {
			for _, f := range cmd.ExtraFiles {
				_ = f.Close()
			}
			if c, ok := eventSrc.(io.Closer); ok {
				_ = c.Close()
			}
		}
		return nil, fmt.Errorf("exec: starting sandboxd: %w", err)
	}
	// The child owns its copy of fd 3 now. Closing the parent's write end is what
	// makes the event reader see EOF when the run ends, rather than hanging for
	// the lifetime of the process.
	if pipeMode {
		for _, f := range cmd.ExtraFiles {
			_ = f.Close()
		}
	}

	s.eventReader, _ = eventSrc.(io.Closer)
	go s.readEvents(eventSrc)
	go func() {
		_ = cmd.Wait()
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		s.waitOnce.Do(func() { s.waitCode = code })
		close(s.processDone)
		// The event reader closes the channel; waiting for it means the final
		// exit event is never dropped.
		<-s.eventsDone
		s.closeClosed()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()

	return s, nil
}

// readEvents parses NDJSON until the stream ends.
func (s *Stream) readEvents(src io.Reader) {
	if closer, ok := src.(io.Closer); ok {
		defer closer.Close()
	}
	defer close(s.events)
	defer close(s.eventsDone)
	sc := bufio.NewScanner(src)
	// The executor emits one line per event. A tool printing a very long line
	// would otherwise trip the default 64 KiB limit; 8 MiB is generous without
	// letting one event exhaust memory.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// The executor promises one JSON object per line. Breaking that
			// promise means the run is not trustworthy, so it is reported rather
			// than skipped.
			ev = StreamEvent{Type: "error"}
		}
		select {
		case s.events <- ev:
		case <-s.stopping:
			// Keep draining so a full event channel cannot block process teardown.
		}
	}
}

func (s *Stream) closeClosed() { s.closeOne.Do(func() { close(s.closed) }) }

// Close stops the run.
//
// The executor is sent SIGTERM rather than killed outright, because a SIGKILL in
// the middle of a run destroys the very evidence the caller may be waiting for:
// the enforcement and exit events are flushed only once the run is collected.
// The executor forwards the signal to the child's process group, ends the run,
// and closes its event pipe. SIGKILL is the escalation for an executor that does
// not go quietly.
func (s *Stream) Close() error {
	s.stopOnce.Do(func() {
		close(s.stopping)
		_ = s.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-s.processDone:
		case <-time.After(3 * time.Second):
			_ = s.cmd.Process.Kill()
			<-s.processDone
		}
		// A descendant may still hold a copied descriptor. Never await its EOF.
		if s.eventReader != nil {
			_ = s.eventReader.Close()
		}
		<-s.closed
	})
	return nil
}

// Wait blocks until the run has ended and returns its exit code.
func (s *Stream) Wait(ctx context.Context) int {
	select {
	case <-ctx.Done():
		return -1
	case <-s.closed:
		return s.waitCode
	}
}
