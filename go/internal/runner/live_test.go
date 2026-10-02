//go:build runner_live

// Command runner-live performs a real run through the real sandbox.
//
// It is the only check that the three layers actually compose: the harness
// manifest, the ACP client, and the Rust executor. Each has unit tests; none of
// them proves that a node-based agent can start inside a Landlock ruleset, be
// spoken to over stdio, and answer a prompt.
//
//	go test -tags runner_live -run TestLiveRun -v ./internal/runner/
package runner_test

import (
	"context"
	"os"
	"testing"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

func TestLiveRun(t *testing.T) {
	sandboxBin := sbx.DefaultBinary()
	if _, err := os.Stat(sandboxBin); err != nil {
		t.Skipf("sandboxd not built: %v", err)
	}

	harnessID := os.Getenv("AI_OS_HARNESS")
	if harnessID == "" {
		harnessID = "opencode"
	}
	prompt := os.Getenv("AI_OS_PROMPT")
	if prompt == "" {
		prompt = "Reply with exactly the word: ONLINE"
	}

	reg := harness.Load()
	if err := reg.OverlayDir(os.Getenv("AI_OS_HARNESS_DIR")); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	h, err := reg.Lookup(harnessID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	t.Logf("harness %s: %s %v (installed=%v)", h.ID, h.Command, h.Args, h.InstalledBy != "")

	// Report the sandbox enforcement events separately: the run may succeed
	// while confinement was never applied, and that is the failure worth seeing.
	enforced := false
	var kinds []string

	r := runner.New(reg, sandboxBin)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	err = r.Run(ctx, runner.Request{
		HarnessID: harnessID,
		Prompt:    prompt,
		Workspace: t.TempDir(),
		Timeout:   3 * time.Minute,
	}, func(ev runner.Event) {
		kinds = append(kinds, ev.Kind)
		if ev.Kind == runner.KindSandbox {
			t.Logf("[sandbox] %s %s", ev.Text, string(ev.Data))
			if bytesContain([]byte(ev.Text+string(ev.Data)), "landlock") {
				enforced = true
			}
		}
		if ev.Kind == runner.KindMessage {
			t.Logf("[message] %s", truncate(ev.Text, 200))
		}
		if ev.Kind == runner.KindError {
			t.Logf("[error] %s", ev.Text)
		}
		if ev.Kind == runner.KindToolCall {
			t.Logf("[tool] %s", ev.Text)
		}
	})

	t.Logf("events: %v", kinds)
	t.Logf("sandbox evidence seen: %v", enforced)
	if err != nil {
		t.Errorf("run: %v", err)
	}
}

func bytesContain(raw []byte, sub string) bool {
	return len(raw) > 0 && (len(sub) == 0 || indexOf(string(raw), sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
