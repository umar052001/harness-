package api

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkflowRequestPermissions(t *testing.T) {
	s := &Server{workflowRoot: "../../.."}
	for _, tc := range []struct {
		name, model       string
		gpt, tools, valid bool
	}{
		{"automatic", "", false, true, true},
		{"gemma", "ollama/gemma4:cloud", false, true, true},
		{"subscription", "openai/gpt-6.1-sol", true, true, true},
		{"operator-disabled Astra", "openai/gpt-6-astra", true, true, false},
		{"catalog-only unsupported subscription", "openai/gpt-5.5", true, true, false},
		{"unapproved subscription", "openai/gpt-5.5", false, true, false},
		{"exhausted free model", "opencode/big-pickle", true, true, false},
		{"unknown", "unknown/model", true, true, false},
		{"tools off", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := runRequest{AllowGPT: tc.gpt, AutoApprove: tc.tools}
			if tc.model != "" {
				req.Models = []string{tc.model}
			}
			if err := s.validateWorkflowRequest(req); (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
		})
	}
}

func TestWorkflowHostVerificationRejectsMissingOrInvalidApp(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name, manifest, index string
		valid                 bool
	}{
		{"empty", "", "", false},
		{"static", "", "<h1>Hello</h1>", true},
		{"bad manifest", "{}", "<h1>must not bypass manifest</h1>", false},
		{"missing service directory", `{"version":1,"services":[{"name":"app","route":"/","cwd":"missing","command":["node","server.js"]}]}`, "<h1>must not bypass manifest</h1>", false},
		{"no checks", `{"version":1,"services":[{"name":"app","route":"/","command":["node","server.js"]}]}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range map[string]string{"ai-os.app.json": tc.manifest, "index.html": tc.index} {
				if content != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.verifyWorkflowApp(t.Context(), "project", dir); (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
		})
	}
}
