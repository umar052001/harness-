//go:build workflow_live

package workflow

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These opt-in checks consume real model/account quota. Faults are injected at
// the provider request and acceptance boundaries; fallback executes real GPT.
func TestLiveFallback(t *testing.T) {
	if os.Getenv("HARNESS_WORKFLOW_LIVE") != "1" {
		t.Skip("set HARNESS_WORKFLOW_LIVE=1 to spend real account quota")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"provider-error", "quality-failure"} {
		t.Run(mode, func(t *testing.T) {
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatal(err)
			}
			b, err := New(Options{Root: root, Store: filepath.Join(home, ".local/share/harness++/validations"), Source: filepath.Join(root, "examples/counter"), Registry: filepath.Join(root, "config/models.json"), Prompt: "Implement Counter", Check: []string{"go", "test", "./..."}, AllowGPT: true, Timeout: 15 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			passed := false
			t.Cleanup(func() {
				if err := b.summary(passed); err != nil {
					t.Error(err)
				}
			})
			t.Log("usage report:", filepath.Join(b.Dir, "summary.json"))
			b.binary = filepath.Join(root, ".build/harness-build")
			b.tooling.ReadPaths = append(b.tooling.ReadPaths, b.binary)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Minute)
			defer cancel()
			attempt := 0
			var rejected atomic.Int32
			failedProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/chat/completions") {
					rejected.Add(1)
					w.WriteHeader(403)
					w.Write([]byte(`{"error":{"message":"injected provider request failure","type":"permission_error"}}`))
					return
				}
				w.Write([]byte(`{"models":[]}`))
			}))
			defer failedProvider.Close()
			err = b.runStages(ctx, testPlan(), func(ctx context.Context, s Stage, p Plan, feedback string, final bool) error {
				attempt++
				if attempt == 1 && mode == "provider-error" {
					b.tooling.OllamaURL = failedProvider.URL + "/v1"
					defer func() { b.tooling.OllamaURL = "" }()
					if _, err := b.graph(ctx, s.Task); err != nil {
						return err
					}
					return b.agent(ctx, s, p, feedback)
				}
				if attempt == 1 && mode == "quality-failure" {
					if _, err := b.graph(ctx, s.Task); err != nil {
						return err
					}
					if err := b.agent(ctx, s, p, feedback); err != nil {
						return err
					}
					// Simulate an inadequate implementation without weakening the
					// operator's tests. The real acceptance command must reject it.
					if err := os.WriteFile(filepath.Join(b.workspace, "counter.go"), []byte("package counter\n\ntype Counter struct{}\n"), 0600); err != nil {
						return err
					}
					_, err := b.command(ctx, b.opt.Check)
					if err == nil {
						return errors.New("fault injection did not fail acceptance")
					}
					return err
				}
				if err := b.runStage(ctx, s, p, feedback, final); err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if attempt != 2 {
				t.Fatalf("expected bounded fallback, attempts=%d", attempt)
			}
			if mode == "provider-error" && rejected.Load() == 0 {
				t.Fatal("injected failure did not reach the model HTTP request")
			}
			if err := b.summary(true); err != nil {
				t.Fatal(err)
			}
			passed = true
		})
	}
}
