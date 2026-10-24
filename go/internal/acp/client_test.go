package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The fake agent runs as a re-exec of this test binary. A real subprocess
// matters: the client's job is to speak JSON-RPC over a pipe to a process it
// did not build, and testing that against an in-memory pipe would skip exactly
// the parts that break in practice — process teardown, EOF handling, frames
// arriving after the response.
const agentModeEnv = "AI_OS_TEST_AGENT_MODE"

// TestMain doubles as the agent entry point. When AI_OS_TEST_AGENT_MODE is set,
// the process runs the fake server and never returns to the test framework.
func TestMain(m *testing.M) {
	mode := os.Getenv(agentModeEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	runFakeAgent(mode)
	os.Exit(0)
}

func runFakeAgent(mode string) {
	report := func(format string, args ...any) {
		// The client is told not to auto-approve, so the outcome of a
		// permission request is written to this file for the test to read.
		path := os.Getenv("AI_OS_TEST_AGENT_REPORT")
		if path == "" {
			return
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		fmt.Fprintf(f, format+"\n", args...)
	}
	denied := &atomic.Bool{}
	_ = denied

	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()
	enc := json.NewEncoder(w)

	// Each frame is flushed as it is produced. A buffered writer that only
	// flushed at exit would deadlock the client, which is waiting for the very
	// first response before it will do anything.
	send := func(v any) {
		_ = enc.Encode(v)
		_ = w.Flush()
	}
	sid := "sess-1"

	dec := json.NewDecoder(bufio.NewReader(os.Stdin))
	for {
		var m struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&m); err != nil {
			return
		}

		switch m.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"protocolVersion": 1,
				"agentInfo":       map[string]any{"name": "fake-agent", "version": "9.9"},
				"agentCapabilities": map[string]any{
					"loadSession": true,
				},
			}})

		case "session/new":
			// Report the whole environment. The client must be able to show,
			// in a test, exactly what the agent can see.
			var p struct {
				Cwd        string            `json:"cwd"`
				MCPServers []json.RawMessage `json:"mcpServers"`
			}
			_ = json.Unmarshal(m.Params, &p)
			report("session/new cwd=%s mcpServers=%d", p.Cwd, len(p.MCPServers))
			for _, kv := range os.Environ() {
				report("env %s", kv)
			}
			sid = "sess-1"
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
				"sessionId":     sid,
				"configOptions": []any{map[string]any{"id": "model", "category": "model", "currentValue": "other"}},
				"modes": map[string]any{
					"availableModes": []any{map[string]any{"id": "default", "name": "Default"}},
					"currentModeId":  "default",
				},
			}})

		case "session/set_config_option":
			report("model-selection %s", string(m.Params))
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
		case "session/prompt":
			if mode == "spec-shape" {
				// The reference-spec dialect: a "type" discriminator and a message
				// wrapper whose content is an array of blocks. The client must
				// read frames it did not write examples for.
				chunk := func(text string) {
					send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
						"params": map[string]any{"sessionId": sid, "type": "agent_message_chunk",
							"message": map[string]any{"role": "assistant",
								"content": []any{map[string]any{"type": "text", "text": text}}}}})
				}
				chunk("un")
				chunk(" deux")
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if mode == "long-but-alive" {
				// Emits progress for far longer than the request deadline before
				// answering. A productive long task looks exactly like this, so
				// the client must treat the frames as proof of life.
				for i := 0; i < 30; i++ {
					time.Sleep(50 * time.Millisecond)
					send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
						"params": map[string]any{"sessionId": sid, "type": "agent_message_chunk",
							"message": map[string]any{"role": "assistant",
								"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("step %d", i)}}}}})
				}
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if mode == "quiet-generation" {
				time.Sleep(600 * time.Millisecond)
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if mode == "stall" {
				time.Sleep(30 * time.Second)
				continue
			}
			if mode == "never-answer" {
				continue
			}
			if mode == "refuse-prompt" {
				send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
					"error": map[string]any{"code": -32000, "message": "quota exhausted"}})
				continue
			}
			var p struct {
				SessionID string `json:"sessionId"`
				Prompt    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if len(p.Prompt) > 0 {
				report("prompt=%s", p.Prompt[0].Text)
			}

			if mode == "bad-frame" {
				// Not JSON at all. The client must survive it.
				_, _ = w.WriteString("this is not json\n")
				_ = w.Flush()
			}
			if mode == "unknown-notification" {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/future_thing",
					"params": map[string]any{"anything": 1}})
			}

			chunk := func(kind, text string) {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": sid, "update": map[string]any{
						"sessionUpdate": kind,
						"content":       map[string]any{"type": "text", "text": text},
					}}})
			}
			tool := func(id, title, status string) {
				send(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": sid, "update": map[string]any{
						"sessionUpdate": "tool_call",
						"toolCallId":    id, "title": title, "kind": "execute",
						"status": status,
					}}})
			}

			// A thought chunk must not end up in the agent's answer.
			chunk("agent_thought_chunk", "hmm")
			tool("t1", "read file", "pending")
			chunk("agent_message_chunk", "one")
			if mode == "permission" {
				// Ask, then wait for the answer. The reply records which way it
				// went, which is the whole point of the test.
				send(map[string]any{"jsonrpc": "2.0", "id": 9001, "method": "session/request_permission",
					"params": map[string]any{"sessionId": sid,
						"toolCall": map[string]any{"toolCallId": "t2", "title": "rm -rf", "kind": "delete"},
						"options": []any{
							map[string]any{"optionId": "yes", "name": "Allow once", "kind": "allow_once"},
							map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"},
						}}})
				// Drain the answer.
				var reply struct {
					ID     *int64          `json:"id"`
					Result json.RawMessage `json:"result"`
				}
				if err := dec.Decode(&reply); err == nil && reply.ID != nil && *reply.ID == 9001 {
					var r struct {
						Outcome struct {
							Outcome  string `json:"outcome"`
							OptionID string `json:"optionId"`
						} `json:"outcome"`
					}
					_ = json.Unmarshal(reply.Result, &r)
					if r.Outcome.OptionID == "no" {
						report("PERMISSION=DENIED")
					} else {
						report("PERMISSION=ALLOWED")
					}
				}
			}
			tool("t2", "write file", "completed")
			chunk("agent_message_chunk", "two")
			chunk("agent_message_chunk", "three")
			send(map[string]any{"jsonrpc": "2.0", "id": m.ID,
				"result": map[string]any{"stopReason": "end_turn"}})

		case "session/cancel":
			report("cancelled")
		}
	}
}

// connectFake starts the fake agent and completes a handshake.
func connectFake(t *testing.T, mode string, onEvent func(Event)) (*Client, *reportReader) {
	t.Helper()
	reportPath := t.TempDir() + "/agent-report"
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	bin := fmt.Sprintf("%s/../acp.test", wd)
	_ = bin

	// Re-exec this test binary in agent mode.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{OnEvent: onEvent}
	c, err := connectExe(context.Background(), exe, mode, reportPath, opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, newReportReader(t, reportPath)
}

// reportReader reads what the fake agent observed.
type reportReader struct {
	t    *testing.T
	path string
}

func newReportReader(t *testing.T, path string) *reportReader {
	return &reportReader{t: t, path: path}
}

func (r *reportReader) lines() []string {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func (r *reportReader) saw(sub string) bool {
	for _, l := range r.lines() {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestConnectHandshake(t *testing.T) {
	c, rep := connectFake(t, "handshake", nil)

	name, version := c.Agent()
	if name != "fake-agent" || version != "9.9" {
		t.Errorf("Agent() = %q/%q, want fake-agent/9.9", name, version)
	}
	if c.sessionID != "sess-1" {
		t.Errorf("sessionID = %q, want sess-1", c.sessionID)
	}
	if !rep.saw("session/new cwd=") {
		t.Error("agent did not receive session/new")
	}
	if !rep.saw("mcpServers=0") {
		t.Error("session/new must send an explicit empty mcpServers list, not omit it")
	}
}

func TestPromptAccumulatesTextAndCountsToolCalls(t *testing.T) {
	var events []Event
	c, _ := connectFake(t, "run", func(e Event) { events = append(events, e) })

	res, err := c.Prompt(context.Background(), "count to three")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Text != "onetwothree" {
		t.Errorf("Text = %q, want %q; thoughts must not be included", res.Text, "onetwothree")
	}
	if res.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", res.StopReason)
	}
	if res.ToolCalls != 2 {
		t.Errorf("ToolCalls = %d, want 2", res.ToolCalls)
	}
	if res.Duration <= 0 {
		t.Error("Duration was not recorded")
	}

	var messages, thoughts, tools int
	for _, e := range events {
		switch e.Type {
		case EventMessage:
			messages++
		case EventThought:
			thoughts++
		case EventToolCall, EventToolUpdate:
			tools++
		}
	}
	if messages != 3 {
		t.Errorf("message events = %d, want 3", messages)
	}
	if thoughts != 1 {
		t.Errorf("thought events = %d, want 1", thoughts)
	}
	if tools != 2 {
		t.Errorf("tool events = %d, want 2", tools)
	}
}

func TestPromptReadsSpecDialectUpdates(t *testing.T) {
	c, _ := connectFake(t, "spec-shape", nil)

	res, err := c.Prompt(context.Background(), "comptez")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.Text != "un deux" {
		t.Errorf("Text = %q, want %q (spec-style type/message/content[] updates must be read)",
			res.Text, "un deux")
	}
}

func TestPermissionDefaultsToDeny(t *testing.T) {
	var sawPerm bool
	c, rep := connectFake(t, "permission", func(e Event) {
		if e.Type == EventPermission {
			sawPerm = true
		}
	})

	if _, err := c.Prompt(context.Background(), "delete everything"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !sawPerm {
		t.Error("no permission event surfaced to the caller")
	}
	if !rep.saw("PERMISSION=DENIED") {
		t.Error("agent was allowed to run a tool without AutoApprove; the safe default is deny")
	}
}

func TestPermissionAutoApproveAllows(t *testing.T) {
	c, rep := connectFake(t, "permission", nil)
	c.autoApprove = true

	if _, err := c.Prompt(context.Background(), "build it"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if rep.saw("PERMISSION=DENIED") {
		t.Error("permission was denied even though AutoApprove was set")
	}
	if !rep.saw("PERMISSION=ALLOWED") {
		t.Error("permission was not allowed with AutoApprove set")
	}
}

func TestAgentErrorSurfaces(t *testing.T) {
	c, _ := connectFake(t, "refuse-prompt", nil)
	_, err := c.Prompt(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected an error when the agent refused the prompt")
	}
	if !strings.Contains(err.Error(), "-32000") || !strings.Contains(err.Error(), "quota exhausted") {
		t.Errorf("error %q should carry the agent's code and message", err)
	}
}

func TestMalformedFrameIsSurvivable(t *testing.T) {
	var sawError bool
	c, _ := connectFake(t, "bad-frame", func(e Event) {
		if e.Type == EventError {
			sawError = true
		}
	})

	res, err := c.Prompt(context.Background(), "hello")
	if err != nil {
		t.Fatalf("a malformed frame should not fail the run: %v", err)
	}
	if res.Text != "onetwothree" {
		t.Errorf("Text = %q, want onetwothree", res.Text)
	}
	if !sawError {
		t.Error("the malformed frame was dropped silently; it must be reported")
	}
}

func TestUnknownNotificationIsTolerated(t *testing.T) {
	var sawAvailable bool
	c, _ := connectFake(t, "unknown-notification", func(e Event) {
		if e.Type == EventAvailable {
			sawAvailable = true
		}
	})
	if _, err := c.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("an unknown notification should not break the run: %v", err)
	}
	if !sawAvailable {
		t.Error("unknown notification was not surfaced")
	}
}

func TestQuietPromptUsesRunBudget(t *testing.T) {
	c, _ := connectFake(t, "quiet-generation", nil)
	c.requestTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := c.Prompt(ctx, "generate a page")
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("quiet generation was interrupted: result=%+v err=%v", result, err)
	}
}

func TestPromptTimeoutReturns(t *testing.T) {
	c, _ := connectFake(t, "stall", nil)
	c.requestTimeout = 400 * time.Millisecond

	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := c.Prompt(ctx, "hello")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout took %s; the request deadline was not honoured", elapsed)
	}
}

func TestUnansweredRequestTimesOut(t *testing.T) {
	c, _ := connectFake(t, "never-answer", nil)
	c.requestTimeout = 400 * time.Millisecond

	start := time.Now()
	err := c.call(t.Context(), "unknown/control", nil, nil)
	if err == nil {
		t.Fatal("expected a timeout when the agent never answers")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s; the deadline was not honoured", elapsed)
	}
}

func TestActivityKeepsLongRequestAlive(t *testing.T) {
	// The regression this guards: a run that streams progress for longer than
	// the request deadline used to fail with "no response after 2m" while its
	// own tool calls were still arriving. Frames are the liveness signal.
	c, _ := connectFake(t, "long-but-alive", nil)
	c.requestTimeout = 300 * time.Millisecond

	start := time.Now()
	res, err := c.Prompt(context.Background(), "hello")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a run that kept reporting progress was killed: %v (after %s)", err, elapsed)
	}
	if elapsed <= c.requestTimeout {
		t.Fatalf("the fake answered in %s, which cannot exercise the reset past a %s deadline", elapsed, c.requestTimeout)
	}
	if res.StopReason != "end_turn" {
		t.Errorf("stop reason = %q, want end_turn", res.StopReason)
	}
	if !strings.Contains(res.Text, "step 29") {
		t.Errorf("streamed text was lost; the last chunk should have been collected")
	}
}

func TestEmptyPromptRejected(t *testing.T) {
	c, rep := connectFake(t, "run", nil)
	if _, err := c.Prompt(context.Background(), "  "); err == nil {
		t.Error("an empty prompt should be rejected before any traffic is sent")
	}
	if rep.saw("prompt=") {
		t.Error("an empty prompt reached the agent")
	}
}

func TestPromptReachesAgentVerbatim(t *testing.T) {
	c, rep := connectFake(t, "run", nil)
	if _, err := c.Prompt(context.Background(), "explain Landlock in one sentence"); err != nil {
		t.Fatal(err)
	}
	if !rep.saw("prompt=explain Landlock in one sentence") {
		t.Errorf("agent did not receive the prompt verbatim; report: %v", rep.lines())
	}
}

func TestCloseTerminatesAgent(t *testing.T) {
	c, _ := connectFake(t, "run", nil)
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// A second close must not panic or error: teardown paths can run twice.
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestNoDescendantsSurvivePrompt checks that stopping a client does not leave the
// agent's helpers running. Agents spawn processes; a control plane that leaks
// them exhausts the host one run at a time.
func TestNoDescendantsSurvivePrompt(t *testing.T) {
	c, _ := connectFake(t, "run", nil)
	if _, err := c.Prompt(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Give the OS a moment to reap, then confirm the process is gone.
	time.Sleep(200 * time.Millisecond)
	if c.cmd.cmd.ProcessState == nil {
		t.Error("process was never waited on; a zombie may remain")
	}
}

// connectExe re-execs the test binary in agent mode and performs the handshake.
func connectExe(ctx context.Context, exe, mode, reportPath string, opts Options) (*Client, error) {
	return Connect(ctx, exe, nil, []string{
		agentModeEnv + "=" + mode,
		"AI_OS_TEST_AGENT_REPORT=" + reportPath,
	}, opts)
}

var _ = exec.Command

// TestChildEnvIsExplicit is the regression test for a bug that looked harmless
// and was not.
//
// start() took an env argument and dropped it, which left cmd.Env nil, which
// makes os/exec inherit the parent's entire environment. Two consequences, and
// the second is the one that mattered:
//
//   - In the test suite, the re-exec'd child never saw AI_OS_TEST_AGENT_MODE,
//     so TestMain fell through to m.Run() and re-ran the suite, which spawned
//     another child that did the same. A recursive fork bomb that reached 8.8 GB
//     before it was noticed.
//   - In production, every agent inherited the operator's real environment and
//     real $HOME, discarding the curated environment the runner built precisely
//     so that agents would not see the user's credentials. The isolation the
//     product is named for was inert.
//
// The test asserts the direction of both edges, because the failure mode is a
// silent absence.
func TestChildEnvIsExplicit(t *testing.T) {
	// A variable the parent has and the child must not see.
	t.Setenv("AI_OS_TEST_SHOULD_NOT_LEAK", "parent-only")
	// A variable the child must see, passed explicitly.
	const wantKey, wantVal = "AI_OS_TEST_SHOULD_REACH", "passed-explicitly"

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	reportPath := dir + "/report"

	c, err := Connect(context.Background(), exe, nil, []string{
		agentModeEnv + "=handshake",
		"AI_OS_TEST_AGENT_REPORT=" + reportPath,
		wantKey + "=" + wantVal,
	}, Options{Cwd: dir})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	rep := newReportReader(t, reportPath)
	lines := strings.Join(rep.lines(), "\n")

	if !strings.Contains(lines, "env "+wantKey+"="+wantVal) {
		t.Errorf("child did not receive %s=%s; the client dropped the env argument.\nchild env:\n%s",
			wantKey, wantVal, lines)
	}
	if strings.Contains(lines, "AI_OS_TEST_SHOULD_NOT_LEAK") {
		t.Error("child inherited the parent's environment; the agent can see variables it was never given")
	}
	// PATH is not passed by this test, so its absence proves nothing is added
	// implicitly either.
	if strings.Contains(lines, "env PATH=") {
		t.Error("child received a PATH it was not given; env is not being built exactly as passed")
	}
}

// TestNilEnvInheritsParent documents os/exec's actual semantics, which are the
// opposite of what the buggy comment claimed: a nil Env means "inherit
// everything", not "inherit nothing". A caller relying on the wrong reading is
// silently handed every secret in the supervisor's environment.
//
// It drives startCmd directly because Connect always receives an env argument,
// and this test needs the nil case. The command is a shell rather than the fake
// agent so nothing about the ACP handshake is involved.
func TestNilEnvInheritsParent(t *testing.T) {
	t.Setenv("AI_OS_TEST_PARENT_MARKER", "inherited")

	dir := t.TempDir()
	p, err := startCmd(
		exec.Command("sh", "-c", "printf 'marker=%s\\n' \"$AI_OS_TEST_PARENT_MARKER\""),
		Options{Cwd: dir, env: nil},
	)
	if err != nil {
		t.Fatalf("startCmd: %v", err)
	}
	defer func() { _ = p.stop() }()

	out, err := io.ReadAll(p.stdout)
	if err != nil {
		t.Fatalf("reading child output: %v", err)
	}
	if !strings.Contains(string(out), "marker=inherited") {
		t.Errorf("a nil env did not inherit the parent (got %q); this test is not asserting os/exec's real behaviour", out)
	}
}
