//go:build acp_live

// Command acp-live drives a real agent through the ACP client.
//
// It exists because a fake agent cannot tell you whether a real one works. Every
// assumption the unit tests bake in — that an agent answers initialize, that
// session/new returns an id, that notifications are framed the way the protocol
// says — is an assumption about real software, and this is the check.
//
// Build and run it explicitly; the tag keeps it out of normal builds and test
// runs so CI never depends on a provider key.
//
//	go test -tags acp_live -run TestLiveGemini -v ./internal/acp/
package acp_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/umar052001/ai-os/go/internal/acp"
)

// liveEnv is one agent under test.
type liveEnv struct {
	id      string
	binary  string
	needsAt string // env var that must be set, empty when the agent needs no key
}

var liveEnvs = []liveEnv{
	{id: "gemini", binary: "gemini", needsAt: ""},
	{id: "opencode", binary: "opencode", needsAt: ""},
}

func TestLiveAgents(t *testing.T) {
	// -live runs the real prompt. Without it only the handshake is checked, which
	// is fast and needs no provider quota.
	runPrompt := os.Getenv("AI_OS_ACP_LIVE_PROMPT") != ""

	for _, env := range liveEnvs {
		t.Run(env.id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			var events []acp.Event
			c, err := acp.Connect(ctx, env.binary, acpArgs(env.id), nil, acp.Options{
				Cwd:     t.TempDir(),
				OnEvent: func(e acp.Event) { events = append(events, e) },
				Stderr:  testWriter{t},
				// A live model can take a while; this is not the 2-minute default.
				RequestTimeout: 5 * time.Minute,
			})
			if err != nil {
				t.Skipf("%s is not usable here: %v", env.binary, err)
			}
			defer func() { _ = c.Close() }()

			name, version := c.Agent()
			t.Logf("connected to %s %s", name, version)

			if !runPrompt {
				t.Logf("set AI_OS_ACP_LIVE_PROMPT=1 to send a real prompt")
				return
			}

			res, err := c.Prompt(ctx, "Reply with exactly the word: ONLINE")
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if !strings.Contains(strings.ToUpper(res.Text), "ONLINE") {
				t.Errorf("agent did not answer as asked; text = %q", res.Text)
			}
			t.Logf("answer: %q in %s, %d tool calls, %d events",
				res.Text, res.Duration.Round(time.Millisecond), res.ToolCalls, len(events))
		})
	}
}

func acpArgs(id string) []string {
	switch id {
	case "gemini":
		return []string{"--acp"}
	case "opencode":
		return []string{"acp"}
	}
	return nil
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("[agent] %s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
