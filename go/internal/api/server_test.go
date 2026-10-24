package api_test

// Test for the HTTP surface: the harness catalogue, health, and the run stream
// as server-sent events. The run goes through an unsandboxed runner driving a
// tiny node ACP server, so the test needs no Rust binary and no model
// provider; what it proves is the framing - SSE framing, event names, stream
// end, CORS - not the sandbox itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/planner"
	"github.com/umar052001/ai-os/go/internal/runner"
)

const apiFakeAgentJS = `
const fs = require('fs');
const path = require('path');
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin, output: process.stdout, terminal: false });
let cwd = process.cwd();
rl.on('line', (line) => {
  let m; try { m = JSON.parse(line); } catch { return; }
  const send = (r) => process.stdout.write(JSON.stringify({ jsonrpc: "2.0", id: m.id, result: r }) + "\n");
  if (m.method === "initialize") send({ protocolVersion: 1, agentInfo: { name: "FakeAgent", version: "0.0.1" } });
  else if (m.method === "session/new") {
    if (m.params && m.params.cwd) cwd = m.params.cwd;
    send({ sessionId: "api-fake-1" });
  }
  else if (m.method === "session/prompt") {
    // Write real files into the session cwd. The whole point of the workspace
    // endpoints is that an agent produces something inspectable, so the fake
    // agent has to actually produce something.
    fs.mkdirSync(path.join(cwd, "src"), { recursive: true });
    fs.writeFileSync(path.join(cwd, "index.html"), "<h1>fake</h1>");
    fs.writeFileSync(path.join(cwd, "src", "main.js"), "console.log(1)\n");
    fs.mkdirSync(path.join(cwd, "node_modules", "left-pad"), { recursive: true });
    fs.writeFileSync(path.join(cwd, "node_modules", "left-pad", "index.js"), "// noise");
    process.stdout.write(JSON.stringify({ jsonrpc: "2.0", method: "session/update",
      params: { sessionId: m.params.sessionId, type: "agent_message_chunk",
        message: { role: "assistant", content: [{ type: "text", text: "api reply ok" }] } } }) + "\n");
    send({ stopReason: "end_turn" });
  }
});
`

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not on PATH: %v", err)
	}
	return newTestServerWithPlanner(t, nil)
}

// newTestServerWithPlanner is like newTestServer but with an explicit planner.
func newTestServerWithPlanner(t *testing.T, pl *planner.Planner) *httptest.Server {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not on PATH: %v", err)
	}

	ws := t.TempDir()
	agent := filepath.Join(ws, "fake-agent.js")
	if err := os.WriteFile(agent, []byte(apiFakeAgentJS), 0o600); err != nil {
		t.Fatal(err)
	}
	manDir := t.TempDir()
	manifest := `{"id":"api-fake","name":"API Fake","kind":"acp","command":"node","args":["` +
		agent + `"],"cost":"free","license":"mit"}`
	if err := os.WriteFile(filepath.Join(manDir, "api-fake.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := harness.Load()
	if err := reg.OverlayDir(manDir); err != nil {
		t.Fatal(err)
	}

	run := runner.NewUnsandboxed(reg)
	srv := api.New(run, api.Options{
		SandboxBinary: "/nonexistent/sandboxd",
		Planner:       pl,
		Origins:       []string{"http://localhost:5173"},
		// A temp root, so a test run never writes workspaces into the operator's
		// real data directory.
		WorkspacesDir: filepath.Join(t.TempDir(), "workspaces"),
	})
	s := httptest.NewServer(srv.Handler())
	t.Cleanup(s.Close)
	return s
}

// TestHealth checks shape including the planner readiness field.
func TestHealth(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Get(s.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var got struct {
		OK         bool   `json:"ok"`
		Sandboxd   string `json:"sandboxd"`
		SandboxdOK bool   `json:"sandboxdOk"`
		PlannerOK  bool   `json:"plannerOk"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Sandboxd == "" {
		t.Errorf("health shape wrong: %+v", got)
	}
	if got.SandboxdOK {
		t.Errorf("sandboxdOk should be false for the stub binary, got %+v", got)
	}
	if got.PlannerOK {
		t.Errorf("plannerOk should be false with no planner, got %+v", got)
	}
}

func TestHarnesses(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Get(s.URL + "/api/harnesses")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Harnesses []harness.Summary `json:"harnesses"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, h := range got.Harnesses {
		ids[h.ID] = true
	}
	if !ids["api-fake"] {
		t.Errorf("fake harness missing from catalogue: %+v", got.Harnesses)
	}
	if !ids["opencode"] {
		t.Errorf("opencode missing from catalogue: %+v", got.Harnesses)
	}
}

// TestRunStream drives the fake agent through a real HTTP POST and reads the
// response as an SSE frame stream, asserting type order and the final end.
func TestRunStream(t *testing.T) {
	s := newTestServer(t)
	body := strings.NewReader(`{"harnessId":"api-fake","prompt":"hi","timeoutSec":60}`)
	req, err := http.NewRequest(http.MethodPost, s.URL+"/api/run", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:5173")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content type %q", ct)
	}
	if oc := res.Header.Get("Access-Control-Allow-Origin"); oc != "http://localhost:5173" {
		t.Errorf("cors header %q", oc)
	}

	type sse struct {
		event string
		data  map[string]any
	}
	var frames []sse
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var curEvent string
	var curData string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if curEvent != "" && curData != "" {
				frames = append(frames, sse{curEvent, decodeData(t, curData)})
			}
			curEvent, curData = "", ""
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			curData = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	ok, firstMsg := false, false
	var kinds []string
	for _, f := range frames {
		if k, _ := f.data["kind"].(string); k != "" {
			kinds = append(kinds, k)
		}
		if f.event == "message" {
			if txt, _ := f.data["text"].(string); strings.Contains(txt, "api reply ok") {
				firstMsg = true
			}
		}
		if f.event == "stream.end" {
			if v, _ := f.data["ok"].(bool); v {
				ok = true
			}
		}
	}

	if !firstMsg {
		t.Errorf("no agent message event seen; frames=%d kinds=%v", len(frames), kinds)
	}
	if !ok {
		t.Errorf("stream did not end ok; frames=%d kinds=%v", len(frames), kinds)
	}
	if len(frames) == 0 {
		t.Fatalf("no SSE frames received")
	}
	// run.assigned leads the stream: the frontend needs the workspace id before
	// the first agent event in order to attach its artifacts panel.
	if kinds[0] != "run.assigned" {
		t.Errorf("first event should be run.assigned, got %v", kinds)
	}
	for _, k := range kinds {
		if !knownKind(k) {
			t.Errorf("unexpected event kind %q in %v", k, kinds)
		}
	}
}

// TestRunTranscriptPersistedAndReplayed covers the history half of the Logs
// view: a run's events are written to disk as they stream, and a later request
// gets the same sequence back.
//
// The bug this guards is the one the UI actually had - transcripts existed only
// in the tab that watched the run, so selecting yesterday's run showed an empty
// Logs pane with no way to tell "nothing happened" from "nothing was kept".
func TestRunTranscriptPersistedAndReplayed(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Post(s.URL+"/api/run", "application/json",
		strings.NewReader(`{"harnessId":"api-fake","prompt":"hi","timeoutSec":60}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// Drain the stream and remember the workspace id it announced.
	var runID string
	var live []string
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var curEvent, curData string
	flush := func() {
		if curEvent == "" || curData == "" {
			return
		}
		if curEvent == "run.assigned" {
			var m map[string]any
			if json.Unmarshal([]byte(curData), &m) == nil {
				runID, _ = m["runId"].(string)
			}
		}
		if strings.HasPrefix(curEvent, "run.") || curEvent == "message" || curEvent == "output" {
			live = append(live, curEvent)
		}
		curEvent, curData = "", ""
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event: "):
			curEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			curData = strings.TrimPrefix(line, "data: ")
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if runID == "" {
		t.Fatal("stream never announced a run id")
	}

	res2, err := http.Get(s.URL + "/api/workspace/" + runID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != 200 {
		t.Fatalf("events status %d", res2.StatusCode)
	}
	var got struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(res2.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) == 0 {
		t.Fatal("transcript is empty after a completed run")
	}
	// The first recorded event is the server's own run.assigned, and the kinds
	// replayed must include the agent's message - proving this is the whole
	// stream, not just its framing.
	var kinds []string
	for _, ev := range got.Events {
		k, _ := ev["kind"].(string)
		kinds = append(kinds, k)
	}
	if kinds[0] != "run.assigned" {
		t.Errorf("transcript should open with run.assigned, got %v", kinds)
	}
	sawMessage := false
	for _, k := range kinds {
		if k == "message" {
			sawMessage = true
		}
	}
	if !sawMessage {
		t.Errorf("transcript missing the agent message: %v", kinds)
	}
	if len(got.Events) < len(live) {
		t.Errorf("transcript has %d events, live stream had %d", len(got.Events), len(live))
	}
}

// A run with no transcript answers with an empty list, not an error, so the
// client can render "no activity recorded" instead of a failure.
func TestRunEventsUnknownRunIsEmpty(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Get(s.URL + "/api/workspace/000000000000/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var got struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 0 {
		t.Errorf("expected no events, got %d", len(got.Events))
	}
}

func decodeData(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad sse data %q: %v", s, err)
	}
	return m
}

func knownKind(k string) bool {
	switch k {
	case "run.started", "harness.selected", "sandbox.enforcement", "message",
		"thought", "tool.call", "tool.update", "permission", "output",
		"error", "plan", "usage", "run.finished", "stream.end", "run.assigned":
		return true
	}
	return false
}

// TestPlanUnconfigured: without a key the endpoint says so plainly rather than
// 500ing, so the frontend can show "planner not configured".
func TestPlanUnconfigured(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Post(s.URL+"/api/plan", "application/json", strings.NewReader(`{"prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Available bool   `json:"available"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Errorf("plan should not be available without a key: %+v", got)
	}
	if !strings.Contains(got.Error, "OLLAMA_API_KEY") {
		t.Errorf("error should mention the key: %+v", got)
	}
}

// TestPlanEndpoint runs a housed fake model and checks the plan comes straight
// through the API.
func TestPlanEndpoint(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"two steps\",\"steps\":[{\"harness\":\"gemini\",\"task\":\"scan\",\"rationale\":\"eyes\"},{\"harness\":\"opencode\",\"task\":\"fix\",\"rationale\":\"hands\",\"dependsOn\":[0]}]}"}}]}`))
	}))
	defer model.Close()

	pl := planner.New("k", "m")
	pl.Base = model.URL
	s := newTestServerWithPlanner(t, pl)

	res, err := http.Post(s.URL+"/api/plan", "application/json", strings.NewReader(`{"prompt":"tidy up"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got struct {
		Available bool           `json:"available"`
		Plan      map[string]any `json:"plan"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Available {
		t.Errorf("plan should be available: %+v", got)
	}
	steps, _ := got.Plan["steps"].([]any)
	if len(steps) != 2 {
		t.Errorf("expected 2 steps, got %+v", got.Plan)
	}
	if got.Plan["summary"] != "two steps" {
		t.Errorf("summary %v", got.Plan["summary"])
	}
}
func TestRunBadPrompt(t *testing.T) {
	s := newTestServer(t)
	res, err := http.Post(s.URL+"/api/run", "application/json", strings.NewReader(`{"harnessId":"api-fake","prompt":"  "}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	// Empty prompt is rejected before any run starts: plain 400, not a stream.
	if res.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
}

// Invalid harnesses fail before a workspace or event stream is created.
func TestRunUnknownHarness(t *testing.T) {
	s := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/api/run",
		strings.NewReader(`{"harnessId":"nope","prompt":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 before stream creation, got %d: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "unknown harness") {
		t.Errorf("expected the harness error on the stream, got:\n%s", body)
	}
}

// TestCORSPreflight asserts the allow-list reflects the configured origin and
// rejects one that was not granted.
func TestCORSPreflight(t *testing.T) {
	s := newTestServer(t)
	req, _ := http.NewRequest(http.MethodOptions, s.URL+"/api/run", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if sc := res.Header.Get("Access-Control-Allow-Origin"); sc != "http://localhost:5173" {
		t.Errorf("allowed origin %q", sc)
	}
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status %d", res.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodOptions, s.URL+"/api/run", nil)
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if oc := res.Header.Get("Access-Control-Allow-Origin"); oc != "" {
		t.Errorf("unexpected allow origin for disallowed client: %q", oc)
	}
}
