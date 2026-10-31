package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/umar052001/ai-os/go/internal/environment"
	"github.com/umar052001/ai-os/go/internal/runner"
	"github.com/umar052001/ai-os/go/internal/workflow"
)

func (s *Server) handleWorkflow(w http.ResponseWriter, r *http.Request) {
	registry, err := workflow.LoadRegistry(filepath.Join(s.workflowRoot, "config/models.json"))
	if err != nil {
		writeJSON(w, 503, map[string]any{"error": err.Error()})
		return
	}
	var models []workflow.Model
	for _, m := range registry.Models {
		if m.Enabled {
			models = append(models, m)
		}
	}
	writeJSON(w, 200, map[string]any{"models": models, "router": registry.Router})
}

func (s *Server) validateWorkflowRequest(req runRequest) error {
	registry, err := workflow.LoadRegistry(filepath.Join(s.workflowRoot, "config/models.json"))
	if err != nil {
		return err
	}
	if len(req.Models) > 0 && req.Models[0] != "" {
		m, ok := registry.Model(req.Models[0])
		if !ok || !m.Enabled {
			return errors.New("model is unknown or disabled; choose Automatic, Gemma, or an enabled subscription GPT")
		}
		if m.Provider == "openai" && !req.AllowGPT {
			return errors.New("enable subscription GPT permission before selecting GPT")
		}
	}
	if !req.AutoApprove {
		return errors.New("enable Allow tools for routed builds; tools remain confined by sandboxd")
	}
	return nil
}

func (s *Server) runWorkflow(ctx context.Context, req runner.Request, project string, emit func(runner.Event)) error {
	model := ""
	if len(req.Models) > 0 {
		model = req.Models[0]
	}
	b, err := workflow.New(workflow.Options{
		Root: s.workflowRoot, Registry: filepath.Join(s.workflowRoot, "config/models.json"),
		Store: filepath.Join(s.store.Root(), ".workflow"), Workspace: req.Workspace,
		Binary: filepath.Join(s.workflowRoot, ".build/harness-build"), Sandbox: s.sandboxBin,
		Prompt: req.Prompt, Model: model, AllowGPT: req.AllowGPT, DatabaseURL: req.DatabaseURL, Timeout: req.Timeout, Emit: emit,
		Verify: func(ctx context.Context) error { return s.verifyWorkflowApp(ctx, project, req.Workspace) },
	})
	if err != nil {
		return err
	}
	return b.Run(ctx)
}

// Verify through the same confined runtime and route table used by Build & start.
// Verification services stop afterwards; generated code is not a running app.
func (s *Server) verifyWorkflowApp(ctx context.Context, project, workspace string) error {
	m, err := environment.LoadApp(workspace)
	if os.IsNotExist(err) {
		root, e := os.OpenRoot(workspace)
		if e != nil {
			return e
		}
		defer root.Close()
		if _, e := root.Stat("ai-os.app.json"); !os.IsNotExist(e) {
			return fmt.Errorf("runtime manifest: %w", err)
		}
		for _, name := range []string{"index.html", "public/index.html", "dist/index.html"} {
			if info, e := root.Stat(name); e == nil && info.Mode().IsRegular() {
				return nil
			}
		}
		return errors.New("no runtime manifest or static index.html; backend applications require ai-os.app.json")
	}
	if err != nil {
		return fmt.Errorf("runtime manifest: %w", err)
	}
	if len(m.Checks) == 0 {
		return errors.New("backend application requires functional checks in ai-os.app.json")
	}
	if m.Database == "postgres" {
		if _, err := s.environments.Start(ctx, project); err != nil {
			return err
		}
	}
	connection, err := s.environments.Connection(ctx, project)
	if err != nil {
		return err
	}
	var apps environment.Apps
	defer apps.Stop("")
	_, err = apps.Start(ctx, ctx, s.sandboxBin, "verification", workspace, connection)
	return err
}
