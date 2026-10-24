package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// defaultRequestTimeout bounds a request that produced neither a response nor a
// frame. It is a liveness bound, not a duration bound: see Client.progress.
const defaultRequestTimeout = 2 * time.Minute

// message is one JSON-RPC frame in either direction.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	if len(e.Data) > 0 {
		return fmt.Sprintf("rpc %d: %s (%s)", e.Code, e.Message, truncate(string(e.Data), 200))
	}
	return fmt.Sprintf("rpc %d: %s", e.Code, e.Message)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// process owns the child and its stdio plumbing.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	reader io.Closer

	waitOnce sync.Once
	stopOnce sync.Once
}

// stop terminates the child and releases the pipes.
//
// The process group is signalled rather than the process alone, because agents
// spawn helpers. A helper holding the stdout pipe would keep the reader blocked
// long after the agent itself is gone, which is how a "finished" run turns into
// a leaked goroutine. The child leads its own group because it was spawned with
// Setpgid, so a negative pid reaches the entire tree.
//
// It always reports success. The caller asked for the agent to stop, so a
// SIGKILLed exit status is the intended outcome and not a failure; Wait is
// still called so the child is reaped and no zombie is left behind.
func (p *process) stop() error {
	p.stopOnce.Do(func() {
		// The stdin writer is closed first and unconditionally, including when
		// this client did not spawn the process. Under ConnectIO the peer is a
		// pipe whose far end belongs to somebody else, and closing the write
		// side is the only signal it gets that this conversation is over. Bailing
		// out early because there is no process to kill would leave an agent
		// waiting on a request that is never coming, and a Close that appears to
		// work while changing nothing.
		if p.stdin != nil {
			_ = p.stdin.Close()
		}
		if p.reader != nil {
			_ = p.reader.Close()
		}
		if p.cmd == nil || p.cmd.Process == nil {
			return
		}
		if p.cmd.Process.Pid > 0 {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		p.waitOnce.Do(func() { _ = p.cmd.Wait() })
	})
	return nil
}

// start launches the agent with stdio piped.
//
// env is folded into opts before the process is built, so there is exactly one
// place that decides what a child can see. Two sources for one setting is how
// the argument came to be accepted and dropped in the first place.
func start(command string, args, env []string, opts Options) (*process, error) {
	if env != nil {
		opts.env = env
	}
	return startCmd(exec.Command(command, args...), opts)
}

// ConnectIO drives an agent over pipes the caller already owns.
//
// It exists for the sandboxed case, where the agent's stdio is the sandbox
// process's stdio and this package cannot spawn anything itself. The handshake
// and the prompt behave identically to Connect; only the transport differs.
func ConnectIO(ctx context.Context, name string, stdin io.Reader, stdinW io.WriteCloser, stdout io.Reader, stderr io.Writer, opts Options) (*Client, error) {
	p := &process{
		stdin:  stdinW,
		stdout: bufio.NewReaderSize(stdout, 64*1024),
		cmd:    nil,
	}
	p.reader, _ = stdout.(io.Closer)
	_ = name
	return handshake(ctx, p, opts)
}

// startCmd wires and starts a command, returning the running process.
func startCmd(cmd *exec.Cmd, opts Options) (*process, error) {
	cmd.Dir = opts.Cwd
	cmd.Env = opts.env
	// Its own process group, so a timeout reaches helpers and a signal aimed at
	// the control plane does not hit a half-torn-down agent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 3 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("acp: stdout: %w", err)
	}
	// stderr is drained on its own goroutine through an os.Pipe rather than
	// Cmd.Stderr, because Cmd.Stderr writes straight to the sink with no
	// backpressure and a chatty agent would then block the whole agent.
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("acp: stderr pipe: %w", err)
	}

	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, fmt.Errorf("acp: starting %s: %w", cmd.Path, err)
	}
	// The parent must drop the write end or the stderr reader never sees EOF.
	_ = stderrW.Close()

	sink := opts.Stderr
	if sink == nil {
		sink = io.Discard
	}
	go func() {
		_, _ = io.Copy(sink, stderrR)
		_ = stderrR.Close()
	}()

	return &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), reader: stdout}, nil
}

// write sends one frame. The mutex matters because a request and a notification
// can be written concurrently, and a frame is only frame-atomic if it is not
// interleaved mid-line.
func (c *Client) write(m *message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("acp: encoding %s: %w", m.Method, err)
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.cmd.stdin.Write(raw); err != nil {
		return fmt.Errorf("acp: writing %s: %w", m.Method, err)
	}
	return nil
}

// readLoop is the single reader for the client's whole lifetime.
//
// It exists for the lifetime of the process rather than being started and
// stopped per request, for two reasons. First, a blocking pipe read cannot be
// cancelled: the earlier design spawned a goroutine per read and left one
// parked in ReadBytes after the response arrived, so the caller waited out its
// full deadline. Second, two concurrent readers on one bufio.Reader race, so
// the abandoned goroutine was a latent data race as well as a hang.
//
// Every frame arrives exactly once, here: responses are routed to the request
// that is waiting for them and notifications are dispatched.
func (c *Client) readLoop() {
	defer close(c.readDone)
	for {
		line, err := c.cmd.stdout.ReadBytes('\n')
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			// EOF, or the agent died. Pending requests are failed rather than
			// left to time out, so a crash surfaces as a prompt error and not as
			// a two-minute stall.
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				c.emit(Event{Type: EventError, Text: "acp: reading agent output: " + err.Error()})
			}
			c.failPending(err)
			return
		}
	}
}

// handleLine parses and routes one frame.
func (c *Client) handleLine(line []byte) {
	if len(trimSpace(line)) == 0 {
		return
	}
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		// A malformed frame is the agent's problem, not a reason to lose the
		// run. Report it and keep reading.
		c.emit(Event{Type: EventError, Text: "acp: unparseable frame: " + err.Error()})
		return
	}
	// Any frame counts as proof of life, before it is routed.
	c.noteProgress()
	if msg.Method == "" && len(msg.ID) > 0 {
		c.deliver(&msg)
		return
	}
	c.dispatch(&msg)
}

// noteProgress records that the agent said something.
//
// The send is non-blocking on a buffered channel: the reader goroutine is the
// only writer and must never block, because a blocked reader stops every pending
// request from being answered.
func (c *Client) noteProgress() {
	select {
	case c.progress <- struct{}{}:
	default:
	}
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// deliver hands a response to the request that is waiting for it.
//
// A response nobody is waiting for is dropped: an agent may legitimately
// answer something this client never asked, for instance an internal retry.
func (c *Client) deliver(m *message) {
	var id int64
	if err := json.Unmarshal(m.ID, &id); err != nil {
		return
	}
	c.pendingMu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()
	if ok {
		ch <- m
	}
}

// failPending releases every waiting request when the agent goes away.
func (c *Client) failPending(err error) {
	c.pendingMu.Lock()
	waiters := c.pending
	c.pending = map[int64]chan *message{}
	c.pendingMu.Unlock()
	for _, ch := range waiters {
		ch <- &message{Error: &rpcError{Code: -32099, Message: "agent exited: " + err.Error()}}
	}
}

// dispatch handles a server-initiated frame.
func (c *Client) dispatch(m *message) {
	switch m.Method {
	case "session/update":
		c.onSessionUpdate(m.Params)
	case "session/request_permission":
		c.onPermission(m)
	case "fs/read_text_file", "fs/write_text_file", "terminal/create",
		"terminal/output", "terminal/wait_for_exit", "terminal/kill", "terminal/release":
		// These are answered, not ignored, and the answer is "no".
		//
		// The client advertised none of them, so an agent asking is either
		// ignoring the handshake or has a stale idea of the protocol. Either way
		// it is waiting for a response: leaving the request unanswered holds its
		// tool call open until something else times out, so the run stalls rather
		// than failing. Method-not-found lets the agent decide what to do next.
		//
		// Implementing these would be easy and would be a security hole. They are
		// client-side capabilities, so serving them means serving host file
		// contents to an agent that is confined to a workspace.
		c.replyError(m, -32601, "ai-os client does not provide "+m.Method+
			": file and terminal access belong to the agent's own tools inside the sandbox")
	default:
		// Unknown requests must get a reply or the peer waits indefinitely.
		c.replyError(m, -32601, "unsupported method: "+m.Method)
		c.emit(Event{Type: EventAvailable, Text: m.Method, Raw: m.Params})
	}
}

// replyError answers a server-initiated request with a JSON-RPC error.
func (c *Client) replyError(m *message, code int, msg string) {
	if len(m.ID) == 0 {
		return
	}
	_ = c.write(&message{
		JSONRPC: "2.0",
		ID:      m.ID,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

// call performs one request/response round trip.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	c.writeMu.Lock()
	id := int64(c.nextID)
	c.nextID++
	c.writeMu.Unlock()

	ch := make(chan *message, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	rawID, err := json.Marshal(id)
	if err != nil {
		return fmt.Errorf("acp: encoding id: %w", err)
	}
	req := &message{JSONRPC: "2.0", ID: rawID, Method: method, Params: mustJSON(params)}
	if err := c.write(req); err != nil {
		return err
	}

	timeout := c.requestTimeout
	if timeout == 0 {
		timeout = defaultRequestTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	timeoutC := timer.C
	// ACP need not emit progress while generating tool arguments. Prompt owns
	// a total deadline, so silence alone is not evidence that its agent is stuck.
	if method == "session/prompt" {
		timer.Stop()
		timeoutC = nil
	}

	// The loop exists so an inbound frame can rearm the timer. A prompt that
	// streams progress for longer than the deadline is working, not stuck, so the
	// deadline measures silence rather than duration. Without this a productive
	// task was reported as "no response after 2m" while its own tool calls were
	// still arriving.
	for {
		select {
		case <-ctx.Done():
			// Close releases a blocked pipe writer as well as the process.
			_ = c.Close()
			return ctx.Err()
		case <-c.readDone:
			// The reader exited; failPending will deliver an error, but returning
			// here avoids waiting for the timer when no waiter was registered.
			select {
			case resp := <-ch:
				return finishCall(resp, out)
			default:
				return errors.New("agent exited before responding")
			}
		case <-timeoutC:
			// No "acp:" and no method name: every caller wraps this with the
			// method it called, and both prefixes at once read as a bug in the
			// error message the user is looking at.
			return fmt.Errorf("no response and no activity for %s", timeout)
		case <-c.progress:
			if timeoutC == nil {
				continue
			}
			if !timer.Stop() {
				// Already fired but not yet collected: drain so Reset cannot
				// leave a stale tick in the channel.
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case resp := <-ch:
			return finishCall(resp, out)
		}
	}
}

func finishCall(resp *message, out any) error {
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil || len(resp.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("acp: decoding result: %w", err)
	}
	return nil
}

// cancelNotification is the best-effort session/cancel notification.
//
// Per the protocol this is a notification, so there is no reply to wait for and
// no error worth surfacing.
func (c *Client) cancelNotification(ctx context.Context) error {
	_, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return c.write(&message{
		JSONRPC: "2.0",
		Method:  "session/cancel",
		Params:  mustJSON(map[string]any{"sessionId": c.sessionID, "reason": "cancelled"}),
	})
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		// Every call site passes a literal map of JSON-safe values, so this
		// cannot fail in practice. Returning null keeps a programming error from
		// becoming a panic in an agent's control flow.
		return json.RawMessage("null")
	}
	return raw
}

// emit forwards an event to the caller's sink.
func (c *Client) emit(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if c.onEvent == nil {
		return
	}
	c.onEvent(e)
}
