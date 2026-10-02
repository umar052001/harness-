package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sandboxv1 "github.com/umar052001/ai-os/go/gen/sandbox/v1"
)

func newWorkspace(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	return ws
}

func client(t *testing.T) *Client {
	t.Helper()
	bin := DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("sandboxd not built at %s; run cargo build first", bin)
	}
	return &Client{Binary: bin}
}

func TestRunReturnsAttestedResponse(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-basic",
		Workspace: ws,
		Argv:      []string{"/bin/echo", "hello from go"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.GetRunId() != "it-basic" {
		t.Errorf("runId = %q, want it-basic", resp.GetRunId())
	}
	if got := resp.GetExitCode(); got != 0 {
		t.Errorf("exitCode = %d, want 0; stdout=%q", got, resp.GetStdout())
	}
	if !strings.Contains(string(resp.GetStdout()), "hello from go") {
		t.Errorf("stdout = %q, want it to contain the echo output", resp.GetStdout())
	}

	// The whole point of the contract: enforcement evidence travels with the
	// result, decoded into a typed message rather than scraped from a log.
	if err := VerifyEnforcement(resp); err != nil {
		t.Errorf("enforcement evidence missing: %v", err)
	}
	if resp.GetEnforcement().GetLandlockAbi() == 0 {
		t.Error("landlock ABI should be reported")
	}
	if resp.GetEnforcement().GetRuleCount() == 0 {
		t.Error("rule count should be non-zero for a real policy")
	}
	if !resp.GetEnforcement().GetNetworkDenied() {
		t.Error("network should be reported as denied")
	}
}

func TestExitCodePropagates(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId: "it-exit", Workspace: ws, Argv: []string{"/bin/sh", "-c", "exit 42"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := resp.GetExitCode(); got != 42 {
		t.Errorf("exitCode = %d, want 42", got)
	}
}

func TestSignalIsReported(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId: "it-sig", Workspace: ws, Argv: []string{"/bin/sh", "-c", "kill -TERM $$"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.GetSignal() != 15 {
		t.Errorf("signal = %d, want 15", resp.GetSignal())
	}
	if got := resp.GetExitCode(); got != 143 {
		t.Errorf("exitCode = %d, want 143 (128+15)", got)
	}
}

// The escape case, driven from Go. If this ever succeeds, the whole design is
// wrong, so it is asserted directly rather than merely logged.
func TestEscapeIsRefused(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-escape",
		Workspace: ws,
		Argv:      []string{"/bin/cat", secret},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.GetExitCode() == 0 {
		t.Fatalf("read outside the workspace succeeded: %q", resp.GetStdout())
	}
	if !strings.Contains(strings.ToLower(string(resp.GetStdout())), "permission denied") {
		t.Errorf("expected a permission denial, got: %q", resp.GetStdout())
	}
}

func TestNetworkIsDenied(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-net",
		Workspace: ws,
		Argv: []string{
			"/usr/bin/python3", "-c",
			"import socket\ns=socket.socket()\ntry:\n    s.connect(('127.0.0.1',1))\n    print('CONNECTED')\nexcept OSError as e:\n    print('BLOCKED', e.errno)\n",
		},
	})
	if err != nil {
		t.Skipf("python3 unavailable in this environment: %v", err)
	}
	if strings.Contains(string(resp.GetStdout()), "CONNECTED") {
		t.Fatalf("network egress succeeded: %q", resp.GetStdout())
	}
	if !strings.Contains(string(resp.GetStdout()), "BLOCKED") {
		t.Errorf("expected the connect to be blocked, got: %q", resp.GetStdout())
	}
}

func TestEnvOverrideReachesChild(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-env",
		Workspace: ws,
		Argv:      []string{"/bin/sh", "-c", "printf %s \"$AI_OS_RUN_ID\""},
		Env:       map[string]string{"AI_OS_RUN_ID": "from-proto"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(resp.GetStdout()), "from-proto") {
		t.Errorf("env did not reach the child: %q", resp.GetStdout())
	}
}

func TestExtraReadGrantWorks(t *testing.T) {
	c := client(t)
	ws := newWorkspace(t)
	extra := t.TempDir()
	target := filepath.Join(extra, "tool.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho granted\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-grant",
		Workspace: ws,
		Argv:      []string{target},
		ExtraRead: []*sandboxv1.Grant{{Path: extra}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(string(resp.GetStdout()), "granted") {
		t.Errorf("extra_read grant did not take effect: %q", resp.GetStdout())
	}
}

func TestRefusedRunIsAnError(t *testing.T) {
	c := client(t)
	ctx, cancel := Timeout(context.Background(), 30*time.Second)
	defer cancel()

	// A workspace that does not exist must fail loudly. Treating it as an
	// ordinary zero exit is how a typo becomes a silent policy bypass.
	_, err := c.Run(ctx, &sandboxv1.ExecRequest{
		RunId:     "it-refused",
		Workspace: filepath.Join(t.TempDir(), "nope"),
		Argv:      []string{"/bin/true"},
	})
	if err == nil {
		t.Fatal("expected an error for a missing workspace")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error should explain the missing workspace, got: %v", err)
	}
}

func TestEmptyArgvIsRejected(t *testing.T) {
	c := client(t)
	if _, err := c.Run(context.Background(), &sandboxv1.ExecRequest{Workspace: t.TempDir()}); err == nil {
		t.Fatal("expected an error for empty argv")
	}
}
