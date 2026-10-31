//go:build runner_live

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
)

func TestLivePostgresClientWithoutHostConfig(t *testing.T) {
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	stream, err := (&sbx.Client{Binary: bin}).Stream(ctx, &sbx.StreamRequest{
		Workspace: t.TempDir(), Stdio: sbx.StdioPipe, TimeoutMs: 10000,
		Argv: []string{"psql", "--version"}, Env: []string{"PATH=" + RuntimePath()},
		ReadPaths: RuntimeReadPaths(), Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for range stream.Events() {
	}
	if code := stream.Wait(ctx); code != 0 {
		t.Fatalf("psql exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "PostgreSQL") {
		t.Fatalf("missing client version: %s", stdout.String())
	}
}
