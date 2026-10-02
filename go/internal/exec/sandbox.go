// Package exec drives the Rust executor from Go.
//
// The boundary between the two languages is a single generated contract:
// sandbox.v1.ExecRequest and sandbox.v1.ExecResponse. The executor writes the
// response as protojson, this package decodes it with the generated type, and a
// field that does not line up is a decode error rather than something that
// quietly goes unnoticed.
package exec

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	sandboxv1 "github.com/umar052001/ai-os/go/gen/sandbox/v1"
)

// Client runs commands through sandboxd.
type Client struct {
	// Binary is the path to the sandboxd executable.
	Binary string
}

// DefaultBinary locates sandboxd next to a standard build output.
func DefaultBinary() string {
	wd, err := os.Getwd()
	if err != nil {
		return "sandboxd"
	}
	// Walk up looking for rust/sandboxd/target/debug/sandboxd, so the demo
	// works from either the repository root or the go module.
	for dir := wd; ; {
		candidate := filepath.Join(dir, "rust", "sandboxd", "target", "debug", "sandboxd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "sandboxd"
}

// Run executes a request and returns the executor's attested response.
//
// The response is decoded into the generated type, so a caller gets a typed
// result with the enforcement evidence attached rather than a log line to trust.
func (c *Client) Run(ctx context.Context, req *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
	if len(req.GetArgv()) == 0 {
		return nil, fmt.Errorf("exec: argv is empty")
	}
	if c.Binary == "" {
		return nil, fmt.Errorf("exec: no sandboxd binary configured")
	}
	if _, err := os.Stat(c.Binary); err != nil {
		return nil, fmt.Errorf("exec: sandboxd not found at %s: %w", c.Binary, err)
	}

	// The response goes to its own file so the executor's human-readable
	// transcript and the child's own output stay separate.
	respFile, err := os.CreateTemp("", "sandboxd-resp-*.json")
	if err != nil {
		return nil, fmt.Errorf("exec: create response file: %w", err)
	}
	defer os.Remove(respFile.Name())
	if err := respFile.Close(); err != nil {
		return nil, fmt.Errorf("exec: close response file: %w", err)
	}

	args := []string{
		"run",
		"--workspace", req.GetWorkspace(),
		"--response", respFile.Name(),
		"--run-id", req.GetRunId(),
	}
	if req.GetNetwork() == sandboxv1.NetworkPolicy_NETWORK_POLICY_ALLOW {
		args = append(args, "--allow-net")
	}
	if req.GetCwd() != "" {
		args = append(args, "--cwd", req.GetCwd())
	}
	for _, g := range req.GetExtraRead() {
		args = append(args, "--read", g.GetPath())
	}
	// Environment pairs are passed through the proto map, sorted for a stable
	// command line.
	for _, k := range sortedKeys(req.GetEnv()) {
		args = append(args, "--env", k+"="+req.GetEnv()[k])
	}
	args = append(args, "--")
	args = append(args, req.GetArgv()...)

	cmd := exec.CommandContext(ctx, c.Binary, args...)

	// The child's own output is captured rather than inherited, so a caller can
	// decide what to do with it. The executor's report goes to stderr and is
	// kept only if the run fails, for the error message.
	var childOut, execReport strings.Builder
	cmd.Stdout = &childOut
	cmd.Stderr = &execReport

	runErr := cmd.Run()

	raw, readErr := os.ReadFile(respFile.Name())
	if readErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("exec: %w\n%s", runErr, tail(execReport.String(), 800))
		}
		return nil, fmt.Errorf("exec: read response: %w", readErr)
	}
	// An empty response file means the executor refused before running anything,
	// which is the fail-closed path. Surface why rather than returning a zero
	// value that would look like a successful run.
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("exec: executor produced no response (refused to run)\n%s",
			tail(execReport.String(), 800))
	}

	var resp sandboxv1.ExecResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("exec: response is not a valid ExecResponse: %w\n%s",
			err, tail(string(raw), 400))
	}
	// The child's output is authoritative; the response's stdout is whatever the
	// pty saw, which includes carriage returns from terminal emulation.
	if len(resp.GetStdout()) == 0 && childOut.Len() > 0 {
		resp.Stdout = []byte(childOut.String())
	}
	return &resp, nil
}

// VerifyEnforcement refuses a response that cannot be trusted to have run
// confined.
//
// The caller decides whether to require this. The reasoning behind requiring it
// anyway: a run that did not attest is a run whose isolation is unknown, and
// acting on its output is the exact mistake this system exists to prevent.
func VerifyEnforcement(resp *sandboxv1.ExecResponse) error {
	e := resp.GetEnforcement()
	if e == nil {
		return fmt.Errorf("run %s: no enforcement evidence was returned", resp.GetRunId())
	}
	if !e.GetNoNewPrivs() {
		return fmt.Errorf("run %s: no_new_privs was not set", resp.GetRunId())
	}
	if e.GetLandlockAbi() == 0 {
		return fmt.Errorf("run %s: no Landlock ABI was reported", resp.GetRunId())
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort keeps the import list short and the map is tiny.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	return "..." + strings.TrimSpace(s[len(s)-n:])
}

// Timeout wraps a context with a deadline appropriate for a tool call.
func Timeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
