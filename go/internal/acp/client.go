// Package acp is a minimal Agent Client Protocol client.
//
// The protocol is JSON-RPC 2.0 over stdio. An agent is a server: it answers
// requests and pushes notifications as work progresses. This package implements
// the subset needed to run one prompt to completion, and no more. The subset is
// deliberately small because the protocol is negotiated at runtime — an agent
// advertises what it can do in the initialize result, and a client that only
// relies on advertised capabilities keeps working as agents diverge.
//
// The alternative was a generated client from the ACP schema. That would be more
// code and would still need every hand-written part below, because the
// interesting behaviour is in the streaming and permission paths, not in method
// signatures.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/umar052001/ai-os/go/internal/usage"
)

// protocolVersion is the ACP version this client implements.
const protocolVersion = 1

// defaultPromptTimeout bounds a prompt when the caller supplies no deadline.
const defaultPromptTimeout = 10 * time.Minute

// Event is something the agent reported as a run progressed.
//
// The type is deliberately coarse. A UI wants to know "the agent said this",
// "the agent called a tool", "the agent asked permission" — not a variant per
// ACP notification kind. Raw payloads are carried alongside so nothing is lost
// and the frontend can narrow later without a protocol change.
type Event struct {
	Usage *usage.Report `json:"usage,omitempty"`
	// Type is one of the Event* constants.
	Type string `json:"type"`
	// Text is the human-visible message or tool detail, when there is one.
	Text string `json:"text,omitempty"`
	// ToolCallID and ToolTitle identify a tool invocation.
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolTitle  string `json:"toolTitle,omitempty"`
	ToolStatus string `json:"toolStatus,omitempty"`
	ToolKind   string `json:"toolKind,omitempty"`
	// Tool is what the call actually did: the tool's name, its argument, the
	// files it touched and its result. A client that renders only ToolTitle shows
	// "bash" for a command, which is the information a waiting user least needs.
	Tool EventTool `json:"tool,omitempty"`
	// Raw is the untouched update payload.
	Raw json.RawMessage `json:"raw,omitempty"`
	// At is when the event was received.
	At time.Time `json:"at"`
}

// EventTool is the interpreted content of one tool call.
//
// Every field is best-effort. The protocol does not require any of them and
// vendors spell them differently, so a missing field costs detail rather than
// the event: the control plane still knows a tool was called, and the raw
// payload is still carried.
type EventTool struct {
	// Name is the tool: bash, read, edit, webfetch.
	Name string `json:"name,omitempty"`
	// Kind is the protocol's coarse category: execute, edit, read, search.
	Kind string `json:"kind,omitempty"`
	// Status is pending, in_progress, completed or failed.
	Status string `json:"status,omitempty"`
	// Command is the tool's primary argument: the shell command, the path, the
	// pattern, the URL. This is what a user actually wants to read.
	Command string `json:"command,omitempty"`
	// Paths are the files the call declared it touches.
	Paths []string `json:"paths,omitempty"`
	// Output is the head of the result, bounded.
	Output string `json:"output,omitempty"`
	// Truncated says Output was cut short.
	Truncated bool `json:"truncated,omitempty"`
}

// Event types emitted by this package.
const (
	EventMessage    = "message"
	EventThought    = "thought"
	EventToolCall   = "tool_call"
	EventToolUpdate = "tool_update"
	EventPermission = "permission"
	EventPlan       = "plan"
	EventAvailable  = "available"
	EventError      = "error"
	EventDone       = "done"
	EventUsage      = "usage"
)

// Result is the outcome of a prompt.
type Result struct {
	Usage usage.Report `json:"usage"`
	// StopReason is the agent's own classification, for example "end_turn".
	StopReason string `json:"stopReason,omitempty"`
	// Text is the concatenation of all agent messages, which is the answer a
	// planner would consume.
	Text string `json:"text,omitempty"`
	// ToolCalls counts tool invocations, for the UI and for cost attribution.
	ToolCalls int `json:"toolCalls"`
	// Duration is wall-clock time for the prompt.
	Duration time.Duration `json:"-"`
}

// Options configures a session.
type Options struct {
	// Cwd is the workspace the agent may act in.
	Cwd string
	// Prompt is the user request.
	Prompt string
	// Model is an optional model id passed to the agent when it supports one.
	Model string
	// AutoApprove decides how tool permission requests are answered. The zero
	// value denies everything, which is the safe default: an agent asking to run
	// a command is the exact moment a human or a policy should see it.
	AutoApprove bool
	// OnEvent receives every event as it arrives. It is called from the reader
	// goroutine, so it must not block for long.
	OnEvent func(Event)
	// Stderr collects the agent's diagnostics. Agents put useful warnings here.
	Stderr io.Writer
	// env is the child's environment. A nil value makes os/exec inherit the
	// parent's whole environment, which is not what the runner wants: it passes a
	// curated set with a sandboxed HOME precisely to keep the agent out of the
	// operator's real one.
	env []string
	// RequestTimeout bounds idle control requests. Prompts use their context deadline.
	RequestTimeout time.Duration
	// OnClose runs when the client is closed, after the agent transport has been
	// released. It is how a caller ties the client's lifetime to something the
	// client does not own, such as the supervisor that confined the agent.
	OnClose func() error
}

// Client is a connection to one agent process.
//
// A Client is used for a single prompt and then discarded. Reuse is possible but
// not needed here, and a fresh process per run means one run's state cannot leak
// into the next, which matters more than the cost of a spawn.
type Client struct {
	cmd *process
	// writeMu guards both frame writes and the request id counter, so a
	// notification cannot interleave mid-line with a request.
	writeMu sync.Mutex
	nextID  int

	// sessionID and the agent identity come out of the handshake.
	sessionID    string
	agentName    string
	agentVersion string

	// onClose releases whatever the transport was built out of but does not own.
	onClose func() error

	// pending holds responses waiting for the request that asked for them,
	// keyed by JSON-RPC id. The single read goroutine fills it; callers drain it.
	pendingMu sync.Mutex
	pending   map[int64]chan *message
	// readDone closes when the reader exits, which is how callers learn the
	// agent is gone without waiting for a timeout.
	readDone chan struct{}

	// progress is nudged whenever a frame arrives from the agent.
	//
	// It exists because "has not answered yet" and "has died" are different
	// failures. session/prompt on a real task stays open for many minutes while
	// the agent streams tool calls and messages; the response only arrives at the
	// end. Treating that silence as a timeout killed runs that were visibly still
	// working, so a waiting request treats inbound frames as proof of life.
	progress chan struct{}

	// tools remembers what each toolCallId was first announced with: the tool's
	// name and the paths it declared. The protocol spreads a call's identity
	// across its frames - the first says "bash", the second retitles it with the
	// command, and the last often carries neither - so without this the details
	// are lost exactly when they are most useful.
	tools map[string]EventTool

	// onEvent is the caller's sink, called from the reader goroutine.
	onEvent func(Event)
	// stderr receives the agent's diagnostics.
	stderr io.Writer
	// requestTimeout bounds one request that produced no response and no
	// activity. See progress for why activity resets it.
	requestTimeout time.Duration
	// autoApprove decides permission requests; the default is deny.
	autoApprove bool

	// coll accumulates the current prompt's answer. The reader goroutine writes
	// it while Prompt reads it after the response, so the handshake is the
	// happens-before edge.
	coll *collector
}

// collector accumulates one prompt's output.
type collector struct {
	usage     usage.Report
	mu        sync.Mutex
	text      strings.Builder
	toolCalls int
	// seen deduplicates tool updates by call id.
	//
	// ACP reports a tool call as a stream of frames: the call arrives first,
	// then a status change, then output, then completion. Counting frames makes
	// one `npm install` look like four tool calls, which then feeds cost
	// attribution and the UI. An empty id is counted rather than dropped,
	// because an unidentifiable frame is still evidence that something ran.
	seen map[string]bool
}

func (c *collector) addText(s string) {
	c.mu.Lock()
	c.text.WriteString(s)
	c.mu.Unlock()
}

func (c *collector) addTool(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != "" {
		if c.seen == nil {
			c.seen = make(map[string]bool)
		}
		if c.seen[id] {
			return
		}
		c.seen[id] = true
	}
	c.toolCalls++
}

func (c *collector) snapshot() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String(), c.toolCalls
}

// Connect starts the agent and performs the ACP handshake.
//
// It returns once initialize and session/new have both completed, so a caller
// that gets a non-nil error knows no session exists. The returned Client owns a
// running process and must be closed.
func Connect(ctx context.Context, command string, args []string, env []string, opts Options) (*Client, error) {
	p, err := start(command, args, env, opts)
	if err != nil {
		return nil, err
	}
	return handshake(ctx, p, opts)
}

// handshake performs the ACP initialize and session/new exchange on a running
// transport. It is shared by Connect and ConnectIO so both paths cannot drift.
func handshake(ctx context.Context, p *process, opts Options) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	c := &Client{
		cmd:            p,
		pending:        map[int64]chan *message{},
		readDone:       make(chan struct{}),
		progress:       make(chan struct{}, 1),
		tools:          map[string]EventTool{},
		onEvent:        opts.OnEvent,
		stderr:         opts.Stderr,
		requestTimeout: opts.RequestTimeout,
		autoApprove:    opts.AutoApprove,
		onClose:        opts.OnClose,
		coll:           &collector{},
	}
	// One reader for the process lifetime. Starting it before the handshake is
	// what lets initialize and session/new work through the same code path as a
	// prompt.
	go c.readLoop()

	// initialize advertises the filesystem and terminal capabilities this client
	// actually implements. Advertising a capability and then not serving it is
	// worse than not advertising it: the agent will route work to a method that
	// returns an error.
	var initResult struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"clientInfo": map[string]any{
			"name":    "ai-os",
			"version": "0.1.0",
		},
		"clientCapabilities": map[string]any{
			// Deliberately false, and this is a confinement decision rather than
			// an unfinished feature. In ACP these are *client* capabilities: the
			// agent asks the client to do the reading and writing. The client here
			// is the control plane, running on the host, outside the sandbox. So
			// answering an fs request would mean reading host files for an agent
			// that is only supposed to see its workspace, which is a sandbox escape
			// wearing the costume of a feature. The agent does its own file I/O
			// through tools, inside the sandbox, where it belongs.
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
	}, &initResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	if initResult.ProtocolVersion != 0 && initResult.ProtocolVersion != protocolVersion {
		c.Close()
		return nil, fmt.Errorf("acp: agent speaks protocol v%d, this client implements v%d", initResult.ProtocolVersion, protocolVersion)
	}

	// session/new. mcpServers is an empty list rather than omitted: agents differ
	// in whether they accept a missing key, and an empty list means "no extra
	// servers" unambiguously.
	var sessionResult struct {
		SessionID     string `json:"sessionId"`
		ConfigOptions []struct {
			ID           string `json:"id"`
			Category     string `json:"category"`
			CurrentValue string `json:"currentValue"`
		} `json:"configOptions"`
	}
	if err := c.call(ctx, "session/new", map[string]any{
		"cwd":        opts.Cwd,
		"mcpServers": []any{},
	}, &sessionResult); err != nil {
		c.Close()
		return nil, fmt.Errorf("acp: session/new: %w", err)
	}
	if sessionResult.SessionID == "" {
		c.Close()
		return nil, errors.New("acp: session/new returned no sessionId")
	}
	c.sessionID = sessionResult.SessionID
	if opts.Model != "" {
		method := "session/set_model"
		params := map[string]any{"sessionId": c.sessionID, "modelId": opts.Model}
		for _, option := range sessionResult.ConfigOptions {
			if option.Category == "model" || option.ID == "model" {
				method = "session/set_config_option"
				params = map[string]any{"sessionId": c.sessionID, "configId": option.ID, "value": opts.Model}
				break
			}
		}
		if err := c.call(ctx, method, params, nil); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("select model %s: %w", opts.Model, err)
		}
	}
	c.agentName = initResult.AgentInfo.Name
	c.agentVersion = initResult.AgentInfo.Version
	return c, nil
}

// Agent identifies the connected agent, for the UI and for logs.
func (c *Client) Agent() (name, version string) { return c.agentName, c.agentVersion }

// Close terminates the agent and releases the transport.
//
// The client is the only thing that knows the conversation is over, so anything
// the transport was built out of has to be released here rather than left to the
// caller. OnClose exists because the process is not always the client's to kill:
// under the sandbox the agent is a Node relay, and the supervisor that actually
// confines the agent has to be stopped as well or it outlives the conversation.
func (c *Client) Close() error {
	err := c.cmd.stop()
	if c.onClose != nil {
		if hookErr := c.onClose(); hookErr != nil && err == nil {
			err = hookErr
		}
	}
	return err
}

// Prompt sends the prompt and streams events until the agent stops.
//
// The returned text is the agent's answer with all tool chatter excluded, which
// is what a planner needs to consume. Events carry the rest.
func (c *Client) Prompt(ctx context.Context, prompt string) (Result, error) {
	if strings.TrimSpace(prompt) == "" {
		return Result{}, errors.New("acp: prompt is empty")
	}
	// A prompt is bounded even if the caller supplied no deadline: an agent that
	// never returns a stop reason would otherwise hold a slot forever.
	//
	// The bound only applies when the caller has none. Wrapping an existing
	// deadline would silently clamp it - a caller asking for twenty minutes got
	// ten, and the request then died at the shorter limit while the agent was
	// still working. One deadline, owned by the caller, is the only version of
	// this that cannot surprise anyone.
	runCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, defaultPromptTimeout)
		defer cancel()
	}

	start := time.Now()

	// The request/response is a single blocking call. Events keep flowing during
	// it because the reader goroutine is independent, which is the whole reason
	// for that split.
	var promptResult struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	callErr := c.call(runCtx, "session/prompt", map[string]any{
		"sessionId": c.sessionID,
		"prompt":    []map[string]any{{"type": "text", "text": prompt}},
	}, &promptResult)

	// Every notification that preceded the response has already been handled:
	// the reader is the only writer to coll and it processes frames in order, so
	// the response being delivered is the happens-before edge.
	text, tools := c.coll.snapshot()
	var tokens *usage.Tokens
	if json.Unmarshal(promptResult.Usage, &tokens) != nil {
		tokens = nil
	}
	out := Result{
		Usage:      c.coll.usageSnapshot(tokens),
		StopReason: promptResult.StopReason,
		Text:       text,
		ToolCalls:  tools,
		Duration:   time.Since(start),
	}
	if callErr != nil {
		return out, fmt.Errorf("acp: session/prompt: %w", callErr)
	}
	return out, nil
}
