//go:build runner_live

package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestLiveChromiumInLongWorkspace(t *testing.T) {
	chromium, err := exec.LookPath("chromium")
	if err != nil {
		t.Skip("Chromium is not installed")
	}
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	ws := filepath.Join(t.TempDir(), strings.Repeat("long-workspace-", 8))
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	stream, err := (&sbx.Client{Binary: bin}).Stream(ctx, &sbx.StreamRequest{
		Workspace: ws, Stdio: sbx.StdioPipe, TimeoutMs: 15000,
		Argv:      []string{chromium, "--headless=new", "--no-sandbox", "--disable-dev-shm-usage", "--disable-gpu", "--no-first-run", "--user-data-dir=" + filepath.Join(ws, "profile"), "--dump-dom", "data:text/html,<h1>HARNESS_BROWSER_OK</h1>"},
		Env:       []string{"PATH=" + RuntimePath(), "HOME=" + ws, "XDG_CONFIG_HOME=" + ws},
		ReadPaths: append(RuntimeReadPaths(), "/proc"), Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for range stream.Events() {
	}
	if code := stream.Wait(ctx); code != 0 {
		t.Fatalf("Chromium exited %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "<h1>HARNESS_BROWSER_OK</h1>") {
		t.Fatalf("missing rendered DOM: %s", stdout.String())
	}
}
