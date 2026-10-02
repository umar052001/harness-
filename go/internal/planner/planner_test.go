package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPlanRequestShape pins the wire format: Bearer auth, the model's name in
// the body, the JSON-object response format, and the system prompt that makes
// the model a dispatcher rather than a solver.
func TestPlanRequestShape(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %q", r.URL.Path)
		}
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"summary\":\"one step\",\"steps\":[{\"harness\":\"opencode\",\"task\":\"do it\",\"rationale\":\"fast\"}]}"}}]}`))
	}))
	defer srv.Close()

	p := New("secret-key", "qwen3-coder:30b")
	p.Base = srv.URL

	plan, err := p.Plan(context.Background(), "fix the bug")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("auth %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"qwen3-coder:30b"`) {
		t.Errorf("body missing model: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"response_format"`) {
		t.Errorf("body missing response_format: %s", gotBody)
	}
	if !strings.Contains(gotBody, "agent dispatch system") {
		t.Errorf("body missing dispatcher system prompt: %s", gotBody)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Harness != "opencode" {
		t.Errorf("unexpected plan: %+v", plan)
	}
}

// TestPlanFencedJSON covers the case where the model wraps its JSON in code
// fences despite being told not to; the client should still parse it.
func TestPlanFencedJSON(t *testing.T) {
	type msg struct {
		Content string `json:"content"`
	}
	type choice struct {
		Message msg `json:"message"`
	}
	type chat struct {
		Choices []choice `json:"choices"`
	}
	body, err := json.Marshal(chat{[]choice{{Message: msg{Content: "```json\n" + `{"summary":"s","steps":[]}` + "\n```"}}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	p := New("k", "")
	p.Base = srv.URL
	plan, err := p.Plan(context.Background(), "x")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Summary != "s" {
		t.Errorf("summary %q", plan.Summary)
	}
}

func TestConfigured(t *testing.T) {
	if New("", "").Configured() {
		t.Error("empty key must not be configured")
	}
	if !New("k", "").Configured() {
		t.Error("set key must be configured")
	}
}

func TestUnconfigured(t *testing.T) {
	_, err := New("", "").Plan(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "OLLAMA_API_KEY") {
		t.Errorf("expected unconfigured error, got %v", err)
	}
}

var _ = json.RawMessage(nil)
