// Command bench runs opt-in model evaluations. Reports contain measurements,
// not prompts, transcripts, or generated code.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/umar052001/ai-os/go/internal/benchmark"
	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
	"github.com/umar052001/ai-os/go/internal/usage"
)

type result struct {
	Version    int          `json:"version"`
	Case       string       `json:"case"`
	Model      string       `json:"model"`
	StartedAt  time.Time    `json:"startedAt"`
	DurationMS int64        `json:"durationMs"`
	Verified   bool         `json:"verified"`
	Error      string       `json:"error,omitempty"`
	Usage      usage.Report `json:"usage"`
}

func main() {
	model := flag.String("model", "opencode/big-pickle", "model to evaluate")
	which := flag.String("case", "", "case name (empty runs all)")
	repeat := flag.Int("repeat", 1, "attempts per case")
	limit := flag.Duration("timeout", 10*time.Minute, "time limit per attempt")
	bin := flag.String("sandboxd", sbx.DefaultBinary(), "sandboxd binary")
	flag.Parse()
	if *repeat < 1 || *repeat > 20 || *limit <= 0 || *limit > 30*time.Minute {
		fmt.Fprintln(os.Stderr, "repeat must be 1–20; timeout must be positive and at most 30m")
		os.Exit(2)
	}
	var cases []benchmark.Case
	for _, task := range benchmark.Cases() {
		if *which == "" || task.Name == *which {
			cases = append(cases, task)
		}
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "unknown case")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := runner.New(harness.Load(), *bin)
	failed := false
	for _, task := range cases {
		for range *repeat {
			if ctx.Err() != nil {
				os.Exit(1)
			}
			row := runCase(ctx, r, task, *model, *bin, *limit)
			if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			failed = failed || !row.Verified
		}
	}
	if failed {
		os.Exit(1)
	}
}

func runCase(ctx context.Context, r *runner.Runner, task benchmark.Case, model, bin string, limit time.Duration) result {
	row := result{Version: benchmark.Version, Case: task.Name, Model: model, StartedAt: time.Now()}
	err := func() error {
		ws, err := os.MkdirTemp("", "ai-os-bench-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(ws)
		for name, content := range task.Files {
			if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o600); err != nil {
				return err
			}
		}
		var mu sync.Mutex
		err = r.Run(ctx, runner.Request{HarnessID: "opencode", Models: []string{model}, Prompt: task.Prompt, Workspace: ws, AutoApprove: true, Timeout: limit}, func(ev runner.Event) {
			if ev.Kind != runner.KindUsage {
				return
			}
			var payload struct {
				Usage *usage.Report `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil && payload.Usage != nil {
				mu.Lock()
				row.Usage = *payload.Usage
				mu.Unlock()
			}
		})
		if err != nil {
			return err
		}
		if task.Name == "data-summary" {
			root, err := os.OpenRoot(ws)
			if err != nil {
				return err
			}
			defer root.Close()
			return benchmark.VerifySummary(root)
		}
		verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		stream, err := (&sbx.Client{Binary: bin}).Stream(verifyCtx, &sbx.StreamRequest{
			Workspace: ws, Stdio: sbx.StdioPipe, TimeoutMs: 10000,
			Argv: []string{"/usr/bin/python3", "-I", "-c", benchmark.VerifyTags, filepath.Join(ws, "tags.py")},
		})
		if err != nil {
			return err
		}
		defer stream.Close()
		for range stream.Events() {
		}
		if code := stream.Wait(verifyCtx); code != 0 {
			return fmt.Errorf("verification failed with exit code %d", code)
		}
		return verifyCtx.Err()
	}()
	row.DurationMS = time.Since(row.StartedAt).Milliseconds()
	row.Verified = err == nil
	if err != nil {
		row.Error = err.Error()
	}
	return row
}
