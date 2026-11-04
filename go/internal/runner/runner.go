// Package runner drives a harness and streams what it does.
//
// It is the layer that turns a harness manifest into a running agent, and it is
// the only place that knows about both harnesses and the sandbox. Everything
// above it — the API, the frontend — works in terms of Run events, so adding a
// harness or changing how one is invoked does not ripple upward.
package runner

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/umar052001/ai-os/go/internal/acp"
	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
)

// Event kinds streamed to the frontend. These are the control plane's own types,
// not either language's wire types: the frontend should not have to know that
// ACP calls its chunks "agent_message_chunk" or that sandboxd calls them "output".
const (
	KindRunStarted  = "run.started"
	KindHarness     = "harness.selected"
	KindSandbox     = "sandbox.enforcement"
	KindMessage     = "message"
	KindThought     = "thought"
	KindToolCall    = "tool.call"
	KindToolUpdate  = "tool.update"
	KindPermission  = "permission"
	KindOutput      = "output"
	KindError       = "error"
	KindPlan        = "plan"
	KindUsage       = "usage"
	KindRunFinished = "run.finished"
)

// Event is one thing that happened, addressed to the frontend.
type Event struct {
	Seq  int             `json:"seq"`
	Kind string          `json:"kind"`
	At   time.Time       `json:"at"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Request is a run to perform.
type Request struct {
	AllowGPT bool `json:"allowGPT,omitempty"`
	// Tooling is host-controlled configuration for the opt-in routed workflow.
	Tooling *Tooling `json:"-"`
	// DatabaseURL is supplied by the host service manager, never by HTTP input.
	DatabaseURL string `json:"-"`
	// HarnessID selects the agent.
	HarnessID string `json:"harnessId"`
	// Prompt is the user's request.
	Prompt string `json:"prompt"`
	// Workspace is the directory the agent may act in.
	Workspace string `json:"workspace,omitempty"`
	// Models optionally pins models for the harness.
	Models []string `json:"models,omitempty"`
	// AutoApprove lets the agent act without asking. The UI must make this
	// explicit, because it is the difference between a reviewer and a
	// rubber stamp.
	AutoApprove bool `json:"autoApprove,omitempty"`
	// Timeout optionally bounds the run; zero leaves duration to the caller.
	Timeout time.Duration `json:"-"`
	// SandboxBinary is the executor path; empty uses the located default.
	SandboxBinary string `json:"-"`
}

// Runner performs runs.
type Runner struct {
	path     string
	registry *harness.Registry
	sandbox  *sbx.Client
	// sandboxed decides whether the agent runs under sandboxd. Tests turn it off
	// because a unit test should not need the Rust binary, and production leaves
	// it on because an unsandboxed agent is not the product.
	sandboxed bool
}

// New builds a runner.
func New(reg *harness.Registry, sandboxBinary string) *Runner {
	return &Runner{registry: reg, sandbox: &sbx.Client{Binary: sandboxBinary}, sandboxed: true, path: sbx.RuntimePath()}
}

// NewUnsandboxed builds a runner that does not confine the agent.
//
// It exists for tests and for the explicit case where an operator has decided
// to trust an agent on their own machine. It is never the default, because the
// default failing open would make the sandbox a suggestion.
func NewUnsandboxed(reg *harness.Registry) *Runner {
	return &Runner{registry: reg, sandboxed: false}
}

// Registry exposes the harness set for the API.
func (r *Runner) Registry() *harness.Registry { return r.registry }

// Run executes a request, calling emit for every event until the run ends.
//
// emit may be called from several goroutines and must be safe for concurrent
// use. The run is finished when Run returns, so a caller can stop by cancelling
// the context.
func (r *Runner) Run(ctx context.Context, req Request, emit func(Event)) error {
	if strings.TrimSpace(req.Prompt) == "" {
		return errors.New("runner: prompt is empty")
	}
	h, err := r.registry.Lookup(req.HarnessID)
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	if req.Timeout > 0 {
		cancel()
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	defer cancel()

	em := &emitter{emit: emit}
	defer em.finish()
	if h.ID == "opencode" && len(req.Models) == 0 {
		req.Models = []string{"opencode/big-pickle"}
	}
	em.send(KindHarness, h.Name, map[string]any{
		"id": h.ID, "kind": h.Kind, "cost": h.Cost, "license": h.License,
		"command": h.Command, "args": h.Args,
	})

	ws := req.Workspace
	if ws == "" {
		ws, err = os.MkdirTemp("", "ai-os-run-")
		if err != nil {
			return fmt.Errorf("runner: workspace: %w", err)
		}
		defer func() { _ = os.RemoveAll(ws) }()
	}
	// The resolved workspace is the authoritative one for the rest of the run:
	// childEnv and the proxy both derive paths from req.Workspace, so handing
	// them a local variable while the request still says "" would send the agent
	// to the system temp directory instead of the run's directory.
	req.Workspace = ws
	if h.ID == "opencode" {
		if err := prepareOpenCode(ws, req.Models[0]); err != nil {
			return err
		}
		if req.Tooling != nil {
			if err := prepareTooling(ws, req.Tooling); err != nil {
				return err
			}
		}
	}

	switch h.Kind {
	case harness.KindACP:
		return r.runACP(runCtx, h, req, ws, em)
	case harness.KindCLI:
		return r.runCLI(runCtx, h, req, ws, em)
	default:
		return fmt.Errorf("runner: harness %q has unsupported kind %q", h.ID, h.Kind)
	}
}

// runACP drives an agent that speaks the Agent Client Protocol.
func (r *Runner) runACP(ctx context.Context, h *harness.Harness, req Request, ws string, em *emitter) error {
	em.send(KindRunStarted, "starting "+h.Name, map[string]any{
		"harness": h.ID, "workspace": ws, "sandboxed": r.sandboxed,
	})

	env := r.childEnv(h, req)
	client, drain, err := r.connect(ctx, h, req, ws, env, em)
	if err != nil {
		em.send(KindError, err.Error(), nil)
		return err
	}
	defer func() {
		_ = client.Close()
		// A run is not finished while the sandbox can still be talking: the
		// enforcement and exit events reach the control plane before this returns,
		// not whenever a goroutine happened to get scheduled. The bound exists
		// because a grandchild holding the event pipe open must not turn a done
		// run into a hang.
		if drain != nil {
			select {
			case <-drain():
			case <-time.After(3 * time.Second):
			}
		}
		// The agent's own pipes are copied by goroutines os/exec owns and that
		// closing the client does not join, so the door is shut only now, once
		// every event this run owes has been sent.
		em.finish()
	}()

	name, version := client.Agent()
	em.send(KindHarness, fmt.Sprintf("connected to %s %s", name, version), map[string]any{
		"agent": name, "version": version,
	})

	res, err := client.Prompt(ctx, req.Prompt)
	if res.Text != "" {
		// A final message event so a client that missed the stream still shows
		// the answer.
		em.send(KindMessage, res.Text, map[string]any{"final": true})
	}
	em.send(KindUsage, "", map[string]any{
		"usage":     res.Usage,
		"toolCalls": res.ToolCalls, "stopReason": res.StopReason,
		"durationMs": res.Duration.Milliseconds(),
	})
	if err != nil {
		// A deadline is reported in the runner's own terms. "context deadline
		// exceeded" is the true error and useless to a person: it names neither
		// what was cut short nor the budget, so the user cannot tell a hang from
		// a task that legitimately needed more time.
		if errors.Is(err, context.DeadlineExceeded) {
			em.send(KindError, fmt.Sprintf("run stopped after its %s time limit", req.Timeout), map[string]any{
				"type": "timeout", "timeoutSec": int(req.Timeout.Seconds()),
			})
		} else {
			em.send(KindError, err.Error(), nil)
		}
		em.send(KindRunFinished, "failed", map[string]any{"ok": false})
		return err
	}
	em.send(KindRunFinished, "done", map[string]any{"ok": true, "stopReason": res.StopReason})
	return nil
}

// connect starts the agent, inside the sandbox when one is configured.
//
// It returns the client, plus a drain function the caller should await after
// closing: it blocks until every sandbox event (enforcement, exit) has been
// delivered, so a finished run really is finished.
func (r *Runner) connect(ctx context.Context, h *harness.Harness, req Request, ws string, env []string, em *emitter) (*acp.Client, func() <-chan struct{}, error) {
	args := h.Argv("", req.Models)
	model := ""
	if h.ID == "opencode" && len(req.Models) > 0 {
		model = req.Models[0]
	}

	if !r.sandboxed {
		client, err := acp.Connect(ctx, h.Command, args, env, acp.Options{
			Cwd: ws, AutoApprove: req.AutoApprove, OnEvent: em.acpSink(),
			RequestTimeout: 90 * time.Second, Model: model,
		})
		if err != nil {
			return nil, nil, err
		}
		return client, func() <-chan struct{} {
			done := make(chan struct{})
			close(done)
			return done
		}, nil
	}

	// The pipes are created before the sandbox is launched because exec.Cmd fixes
	// a process's stdio at Start; there is no way to attach them afterwards.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return nil, nil, err
	}

	// Inherited pipes already carry ACP unchanged; a Node relay adds no value.
	argv := append([]string{h.Command}, args...)

	// The agent's own diagnostics are forwarded as output events rather than
	// parked in a buffer. A failed run is exactly when a user needs to see what
	// the agent said, and "the error is in a variable nobody printed" is how a
	// two-minute timeout turns into an afternoon of guessing. It is not mixed into
	// the ACP stream - it is a separate event, labelled as the agent's stderr.
	agentStderr := &lineEmitter{emit: em.outputLine("agent-stderr")}

	stream, err := r.sandbox.Stream(ctx, &sbx.StreamRequest{
		Workspace: ws,
		TimeoutMs: int(req.Timeout.Milliseconds()),
		Argv:      argv,
		Env:       env,
		// Network is granted only when the harness manifest says it needs it, and
		// the grant is made for a stated reason that the executor records. A local
		// harness that never phones home does not get egress just because it is an
		// agent: the default stays deny, and NeedsNetwork is the harness's own
		// account of what it is for.
		AllowNet: h.NeedsNetwork != "",
		NetWhy:   netWhy(h),
		// /proc is not convenience, it is a hard runtime requirement: Bun (which
		// opencode is built on) aborts with a silent SIGABRT when procfs is
		// unreadable. A narrower grant than /proc does not help - the rule binds
		// to the sandbox supervisor's own pid directory, so a descendant that
		// resolves its *own* /proc/<pid> does not match it and the runtime dies
		// anyway. The grant is read-only; the agent can observe no more than it
		// could unconfined, and exposes nothing outside its own namespace.
		ReadPaths: append(append(append(sbx.RuntimeReadPaths(), "/proc"), sbx.ResolverReadPaths()...), toolingPaths(req.Tooling)...),
		// Pipe transport, not a pty. ACP is a JSON-RPC dialect over stdio, and a
		// pty would echo every frame back at the peer, rewrite newlines, and
		// line-buffer a protocol that has no lines to buffer on.
		Stdio: sbx.StdioPipe,
		// The agent's JSON-RPC reaches the confined process through these pipes.
		// The executor's NDJSON goes to its own descriptor (fd 3) so nothing the
		// executor says can land in the protocol stream.
		Stdin:  stdinR,
		Stdout: stdoutW,
		Stderr: agentStderr,
	})
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, nil, fmt.Errorf("runner: starting sandboxed agent: %w", err)
	}
	// Only the child may keep these ends open, otherwise an agent crash has no EOF.
	_ = stdinR.Close()
	_ = stdoutW.Close()

	// Enforcement evidence is reported before the prompt, not after: a run that
	// ends with a violation report is a run that was already unsafe.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range stream.Events() {
			switch ev.Type {
			case "run.started", "enforcement":
				// The ruleset is already built and asserted before the child
				// runs, so the enforcement evidence is reported before the
				// prompt, not after: a run that ends with a violation report is
				// a run that was already unsafe. The executor emits
				// "run.started" as soon as it is known; "enforcement" covers a
				// finished run's attested summary too.
				em.send(KindSandbox, "landlock ruleset enforced", ev)
			case "output":
				// The agent's own diagnostics and any shell output. Sent as
				// output rather than parsed, because the sandbox does not know
				// which bytes are protocol and which are chatter.
				if txt := decodeChunk(ev.Data); txt != "" {
					em.send(KindOutput, txt, map[string]any{"stream": "agent-stderr"})
				}
			case "exit":
				em.send(KindSandbox, fmt.Sprintf("agent exited with code %d", ev.ExitCode), ev)
			}
		}
	}()

	client, err := acp.ConnectIO(ctx, h.Name, stdinR, stdinW, stdoutR, agentStderr, acp.Options{
		Cwd: ws, AutoApprove: req.AutoApprove, OnEvent: em.acpSink(),
		RequestTimeout: 90 * time.Second, Model: model,
		OnClose: func() error { return stream.Close() },
	})
	if err != nil {
		_ = stream.Close()
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, nil, err
	}

	// The client owns the conversation, so the client closing has to stop the
	// executor too. Without this the sandbox outlives it: the process the ACP
	// client is attached to is the Node relay, so tearing that down orphans
	// sandboxd, which leaves the real agent running with nobody reading its
	// protocol and the event reader blocked on a pipe nobody will close.
	//
	// Cancelling the run reaches the same place through the context Stream was
	// given, and both are wired: a caller that stops because the agent finished
	// is a different path from a caller that stops because the user cancelled.
	go func() {
		<-ctx.Done()
		_ = stream.Close()
	}()

	return client, func() <-chan struct{} { return drained }, nil
}

// netWhy records why a harness needs network egress.
//
// It is derived from the harness rather than from the run, because it is a fact
// about the harness: an ACP agent talks to a model provider over the network,
// and the reason has to survive into the transcript and the attestation so a
// later reader sees which grants were opened and on whose judgement.
func netWhy(h *harness.Harness) string {
	if h.NeedsNetwork == "" {
		return h.ID + " harness: model provider access"
	}
	return h.ID + " harness: " + h.NeedsNetwork
}

// lineEmitter turns a byte stream into line events as it arrives.
//
// A child's stderr is not line-framed, so partial writes are buffered until a
// newline rather than emitted as fragments that would each become their own log
// line. Writes are not necessarily serialised by os/exec, so the buffer is
// guarded.
type lineEmitter struct {
	emit func(string)
	mu   sync.Mutex
	cur  []byte
}

func (l *lineEmitter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if line := strings.TrimRight(string(l.cur), "\r"); line != "" {
				l.emit(line)
			}
			l.cur = l.cur[:0]
			continue
		}
		l.cur = append(l.cur, b)
		if len(l.cur) >= 64*1024 {
			l.emit(string(l.cur))
			l.cur = l.cur[:0]
		}
	}
	return len(p), nil
}

// runCLI drives a one-shot agent: the prompt goes in, text comes out.
func (r *Runner) runCLI(ctx context.Context, h *harness.Harness, req Request, ws string, em *emitter) error {
	em.send(KindRunStarted, "starting "+h.Name, map[string]any{
		"harness": h.ID, "workspace": ws, "sandboxed": r.sandboxed,
	})

	// A PromptStdin harness takes the request on fd 0, not in argv: argv is world
	// readable through the process table, and a prompt is the part of this system
	// most likely to contain something the user did not mean to publish.
	argv := h.Argv(req.Prompt, req.Models)
	stdin := strings.NewReader("")
	if h.PromptStdin {
		stdin = strings.NewReader(req.Prompt)
	}
	if r.sandboxed {
		output := &lineEmitter{emit: em.outputLine("stdout")}
		stream, err := r.sandbox.Stream(ctx, &sbx.StreamRequest{
			Workspace: ws, Argv: append([]string{h.Command}, argv...),
			Env: r.childEnv(h, req), Stdio: sbx.StdioPipe,
			Stdin: stdin, Stdout: output, Stderr: &lineEmitter{emit: em.outputLine("stderr")},
			AllowNet: h.NeedsNetwork != "", NetWhy: netWhy(h),
			ReadPaths: append(append(sbx.RuntimeReadPaths(), "/proc"), sbx.ResolverReadPaths()...),
			TimeoutMs: int(req.Timeout.Milliseconds()),
		})
		if err != nil {
			return err
		}
		defer stream.Close()
		for ev := range stream.Events() {
			if ev.Type == "run.started" || ev.Type == "enforcement" {
				em.send(KindSandbox, "sandbox policy", ev)
			}
		}
		code := stream.Wait(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if code != 0 {
			return fmt.Errorf("agent exited with code %d", code)
		}
		em.send(KindRunFinished, "done", map[string]any{"ok": true})
		return nil
	}
	cmd := exec.CommandContext(ctx, h.Command, argv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Dir = ws
	cmd.Env = r.childEnv(h, req)
	cmd.Stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		em.send(KindError, err.Error(), nil)
		return fmt.Errorf("runner: starting %s: %w", h.Command, err)
	}

	var text strings.Builder
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			text.WriteString(line)
			text.WriteString("\n")
			em.send(KindOutput, line, map[string]any{"stream": "stdout"})
		}
		done <- sc.Err()
	}()

	readErr := <-done
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		em.send(KindError, waitErr.Error(), nil)
		em.send(KindRunFinished, "failed", map[string]any{"ok": false})
		return waitErr
	}
	em.send(KindMessage, strings.TrimSpace(text.String()), map[string]any{"final": true})
	em.send(KindRunFinished, "done", map[string]any{"ok": true})
	return nil
}

// childEnv builds the agent's environment.
//
// It is built from nothing rather than inherited. The sandbox does not pass the
// supervisor's environment through, and neither does this: an agent that
// inherits the operator's shell also inherits every token in it, which is the
// difference between a tool with access to a model and a tool with access to
// everything.
func (r *Runner) childEnv(h *harness.Harness, req Request) []string {
	home := filepath.Join(reqWorkspace(req), ".home")
	path := r.path
	if path == "" {
		path = os.Getenv("PATH")
	}
	env := []string{
		"PATH=" + path,
		"HOME=" + home,
		// Tools do not all honour HOME: XDG directories are resolved
		// independently and would otherwise fall back to the wrapped library's
		// own default, which is often the launching user's real home - exactly
		// the directory the sandbox is meant to keep agent state out of. Pinning
		// them under the synthetic home keeps logs, config, and caches inside the
		// writable workspace regardless of which directory resolver the agent
		// uses.
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"TERM=xterm-256color",
		"NO_COLOR=1",
		"CI=1",
	}
	// A dedicated home keeps agent state out of the operator's real one, and out
	// of a shared directory when several runs overlap.
	if h.Env != nil {
		keys := make([]string, 0, len(h.Env))
		for k := range h.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+h.Env[k])
		}
	}
	if req.DatabaseURL != "" {
		env = append(env, "DATABASE_URL="+req.DatabaseURL)
	}
	return env
}

func reqWorkspace(req Request) string {
	if req.Workspace != "" {
		return req.Workspace
	}
	return os.TempDir()
}

// decodeChunk turns a base64 output payload into text for display.
func decodeChunk(b64 string) string {
	if b64 == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(raw), "\r\n")
}

// emitter assigns sequence numbers and marshals payloads.
type emitter struct {
	mu   sync.Mutex
	seq  int
	emit func(Event)
	done bool
}

// finish stops the emitter accepting anything further.
//
// The agent's stdout and stderr are copied by goroutines that os/exec owns and
// nobody waits for: closing the client returns while they can still be mid-read.
// Anything they emit afterwards lands after the run's own last event, so the
// transcript ends with out-of-order noise, and - because the control plane has
// usually already closed the HTTP response by then - it is written to a
// recycled http.ResponseWriter, which takes the process down. Refusing the
// events is the only place that can be relied on to know the run is over.
func (e *emitter) finish() {
	e.mu.Lock()
	e.done = true
	e.mu.Unlock()
}

func (e *emitter) send(kind, text string, data any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	e.seq++
	ev := Event{Seq: e.seq, Kind: kind, At: time.Now(), Text: text}
	if data != nil {
		if raw, err := json.Marshal(data); err == nil {
			ev.Data = raw
		}
	}
	emit := e.emit
	if emit != nil {
		emit(ev)
	}
}

// acpSink adapts ACP events onto the emitter.
// outputLine returns a sink for one named stream.
func (e *emitter) outputLine(stream string) func(string) {
	return func(line string) { e.send(KindOutput, line, map[string]any{"stream": stream}) }
}

func (e *emitter) acpSink() func(acp.Event) {
	return func(ev acp.Event) {
		switch ev.Type {
		case acp.EventUsage:
			e.send(KindUsage, "", map[string]any{"usage": ev.Usage})
		case acp.EventMessage:
			e.send(KindMessage, ev.Text, nil)
		case acp.EventThought:
			e.send(KindThought, ev.Text, nil)
		case acp.EventToolCall, acp.EventToolUpdate:
			kind := KindToolCall
			if ev.Type == acp.EventToolUpdate {
				kind = KindToolUpdate
			}
			// The tool detail is forwarded whole. A client that only gets the
			// title learns that "bash" happened, which tells a waiting user
			// nothing about what the agent is doing to their files.
			e.send(kind, ev.Text, map[string]any{
				"id": ev.ToolCallID, "title": ev.ToolTitle,
				"status": ev.ToolStatus, "toolKind": ev.ToolKind,
				"tool": ev.Tool.Name, "command": ev.Tool.Command,
				"paths": ev.Tool.Paths, "output": ev.Tool.Output,
				"truncated": ev.Tool.Truncated,
			})
		case acp.EventPermission:
			e.send(KindPermission, ev.Text, map[string]any{
				"status": ev.ToolStatus,
				"id":     ev.ToolCallID, "title": ev.ToolTitle, "toolKind": ev.ToolKind,
				"tool": ev.Tool.Name, "command": ev.Tool.Command,
			})
		case acp.EventPlan:
			e.send(KindPlan, ev.Text, nil)
		case acp.EventError:
			e.send(KindError, ev.Text, nil)
		default:
			if ev.Text != "" {
				e.send(KindOutput, ev.Text, map[string]any{"stream": "agent"})
			}
		}
	}
}
