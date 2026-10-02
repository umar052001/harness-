package runner

// Integration test for the full composition: a node-based ACP agent, started
// through the proxy relay, inside the Rust sandbox, driven by the real ACP
// client. Each layer has its own tests; this is the check that they actually
// fit together over stdio rather than over a pty, which was the failure mode
// that sent the first end-to-end attempt into a two-minute silence.
//
// The agent is a tiny fake that answers initialize, session/new and
// session/prompt, so the test is deterministic and needs no model or network.
// It skips when the sandbox or node is missing rather than failing, because a
// missing dependency is an environment fact, not a finding.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
)

const fakeAgentJS = `
// Tiny ACP server: answer handshake, session, and one prompt, streaming a
// single update before the result. Line-delimited JSON in and out; nothing else.
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false });
rl.on('line', (line) => {
  let m;
  try { m = JSON.parse(line); } catch { return; }
  const send = (result) => process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result }) + "\n");
  if (m.method === "initialize") {
    send({ protocolVersion: 1, agentInfo: { name: "FakeAgent", version: "0.0.1" } });
  } else if (m.method === "session/new") {
    send({ sessionId: "fake-session-1" });
  } else if (m.method === "session/prompt") {
    process.stdout.write(JSON.stringify({
      jsonrpc: "2.0", method: "session/update",
      params: { sessionId: m.params.sessionId, type: "agent_message_chunk",
                message: { role: "assistant", content: [{ type: "text", text: "hola desde el agente" }] } }
    }) + "\n");
    send({ stopReason: "end_turn" });
  }
});
`

func TestSandboxedACPFakeE2E(t *testing.T) {
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("sandboxd not built at %s: %v", bin, err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not on PATH: %v", err)
	}

	ws := t.TempDir()
	agent := filepath.Join(ws, "fake-agent.js")
	if err := os.WriteFile(agent, []byte(fakeAgentJS), 0o600); err != nil {
		t.Fatal(err)
	}

	// The harness is a manifest entry, like a real one: name, dialect, command.
	// The command is node and the argument is the fake agent inside the
	// workspace, which mirrors how a real harness would be reached.
	manDir := t.TempDir()
	manifest := `{"id":"fake-e2e","name":"Fake E2E","kind":"acp","command":"node","args":["` +
		agent + `"],"cost":"free","license":"mit","needsNetwork":""}`
	if err := os.WriteFile(filepath.Join(manDir, "fake.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := harness.Load()
	if err := reg.OverlayDir(manDir); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	h, err := reg.Lookup("fake-e2e")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}

	r := New(reg, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var events []Event
	var messages []string
	enforced := false
	var mu sync.Mutex
	seen := func() ([]Event, []string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return events, messages, enforced
	}
	em := &emitter{emit: func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
		if ev.Kind == KindMessage {
			messages = append(messages, ev.Text)
		}
		if ev.Kind == KindSandbox {
			t.Logf("[sandbox] %s", ev.Text)
			if strings.Contains(string(ev.Data), "landlock") || strings.Contains(ev.Text, "landlock") {
				enforced = true
			}
		}
	}}

	req := Request{
		HarnessID: "fake-e2e",
		Prompt:    "say hello",
		Workspace: ws,
		Timeout:   60 * time.Second,
	}
	err = r.runACP(ctx, h, req, ws, em)

	if err != nil {
		t.Fatalf("runACP: %v", err)
	}
	evs, msgs, en := seen()
	if !en {
		t.Errorf("no Landlock enforcement event reached the control plane; events=%d %s",
			len(evs), kindsOf(evs))
	}
	if len(msgs) == 0 || !strings.HasPrefix(strings.Join(msgs, "\n"), "hola") {
		t.Errorf("expected the fake agent's reply, got %q", msgs)
	}
	t.Logf("events seen: %s", kindsOf(evs))
	for _, ev := range evs {
		if ev.Kind != KindOutput {
			t.Logf("ev[%d] %s: %q", ev.Seq, ev.Kind, truncateOnly(ev.Text))
		}
	}
}
func truncateOnly(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func kindsOf(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Kind)
	}
	return out
}
