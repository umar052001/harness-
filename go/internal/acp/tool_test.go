package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

// These frames are verbatim from opencode 1.18.30. They are the reason the tool
// detail is extracted at all: the first frame of a call says only "bash", and
// the command appears in rawInput on a later frame.

// toolEvents runs frames through the real update path and collects what a
// control plane would receive.
func toolEvents(t *testing.T, frames ...string) []Event {
	t.Helper()
	var got []Event
	c := &Client{coll: &collector{}, tools: map[string]EventTool{}}
	c.onEvent = func(ev Event) {
		if ev.Type == EventToolCall || ev.Type == EventToolUpdate {
			got = append(got, ev)
		}
	}
	for _, f := range frames {
		c.onSessionUpdate(json.RawMessage(f))
	}
	return got
}

const (
	frameToolCallPending = `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1","title":"bash","kind":"execute","status":"pending","locations":[{"path":"/w"}],"rawInput":{"cwd":"/w"}}}`
	frameToolCallCommand = `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"in_progress","kind":"execute","title":"mkdir -p demo && ls demo","locations":[{"path":"/w"}],"rawInput":{"command":"mkdir -p demo && ls demo","workdir":"/w"}}}`
	frameToolCallDone    = `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed","title":"mkdir -p demo && ls demo","content":[{"type":"content","content":{"type":"text","text":"index.html\n"}}],"rawOutput":{"output":"index.html\n","metadata":{"exit":0}}}}`
)

func TestToolCallCarriesTheCommand(t *testing.T) {
	// The behaviour this exists for: the control plane must learn the command,
	// not just the name of the tool that ran it.
	got := toolEvents(t, frameToolCallPending, frameToolCallCommand, frameToolCallDone)
	if len(got) != 3 {
		t.Fatalf("collected %d tool events, want 3", len(got))
	}
	if got[0].Tool.Name != "bash" {
		t.Errorf("first frame name = %q, want bash", got[0].Tool.Name)
	}
	// The first frame carries no command yet and must not invent one.
	if got[0].Tool.Command != "" {
		t.Errorf("first frame command = %q, want empty", got[0].Tool.Command)
	}
	for _, ev := range got[1:] {
		// The title is now the command, so the remembered name has to survive it.
		if ev.Tool.Name != "bash" {
			t.Errorf("name = %q, want the remembered bash", ev.Tool.Name)
		}
		if ev.Tool.Command != "mkdir -p demo && ls demo" {
			t.Errorf("command = %q", ev.Tool.Command)
		}
	}
	if got[2].Tool.Status != "completed" {
		t.Errorf("status = %q", got[2].Tool.Status)
	}
	if got[2].Tool.Output != "index.html\n" {
		t.Errorf("output = %q", got[2].Tool.Output)
	}
	if len(got[2].Tool.Paths) != 1 || got[2].Tool.Paths[0] != "/w" {
		t.Errorf("paths = %v", got[2].Tool.Paths)
	}
}

func TestToolOutputIsBounded(t *testing.T) {
	big := strings.Repeat("x", 5000)
	frame := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"c2","status":"completed","title":"big","rawOutput":{"output":"` +
		big + `"}}}`

	got := toolEvents(t, frame)
	if len(got) != 1 {
		t.Fatalf("collected %d events", len(got))
	}
	if len(got[0].Tool.Output) > 1300 {
		t.Errorf("output is %d bytes; it must be bounded", len(got[0].Tool.Output))
	}
	if !got[0].Tool.Truncated {
		t.Error("truncated flag not set on a cut-off output")
	}
}

func TestToolCallCountsOncePerCall(t *testing.T) {
	// A single bash call arrives as three frames. Counting frames made one
	// command look like three tool calls, which then flows into cost
	// attribution and the action counter in the UI.
	c := &Client{coll: &collector{}, tools: map[string]EventTool{}}
	c.onEvent = func(Event) {}
	for _, f := range []string{frameToolCallPending, frameToolCallCommand, frameToolCallDone} {
		c.onSessionUpdate(json.RawMessage(f))
	}
	_, tools := c.coll.snapshot()
	if tools != 1 {
		t.Errorf("tool calls = %d, want 1 for one three-frame call", tools)
	}

	// A second, distinct call still counts, so the fix is not just suppression.
	c.onSessionUpdate(json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c2","title":"read"}}`))
	if _, tools = c.coll.snapshot(); tools != 2 {
		t.Errorf("tool calls = %d, want 2 after a second call", tools)
	}
}

func TestToolArgumentVariants(t *testing.T) {
	// Agents disagree on the argument field name. A read tool that reports its
	// path under "filePath" should still say what it is reading.
	cases := []struct {
		name, rawInput, want string
	}{
		{"read path", `{"filePath":"src/main.ts"}`, "src/main.ts"},
		{"glob pattern", `{"pattern":"**/*.ts"}`, "**/*.ts"},
		{"url", `{"url":"https://example.com"}`, "https://example.com"},
		{"description", `{"description":"search the web"}`, "search the web"},
		{"command", `{"command":"npm install"}`, "npm install"},
		{"nothing recognised", `{"weird":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c9","title":"some_tool","kind":"read","rawInput":` +
				tc.rawInput + `}}`
			got := toolEvents(t, frame)
			if len(got) != 1 {
				t.Fatalf("collected %d events", len(got))
			}
			if g := got[0].Tool.Command; g != tc.want {
				t.Errorf("command = %q, want %q", g, tc.want)
			}
		})
	}
}
