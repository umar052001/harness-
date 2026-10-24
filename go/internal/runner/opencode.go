package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Configuration belongs to the run's synthetic home, never the operator's.
func prepareOpenCode(workspace, model string) error {
	dir := filepath.Join(workspace, ".home", ".config", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"model":   model, "update": "disable", "snapshots": false,
		"permissions": []map[string]string{
			{"action": "*", "resource": "*", "effect": "ask"},
			{"action": "subagent", "resource": "*", "effect": "deny"},
		},
		"agents": map[string]any{"build": map[string]any{"permissions": []map[string]string{
			{"action": "question", "resource": "*", "effect": "deny"},
		}}},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), raw, 0o600); err != nil {
		return fmt.Errorf("configure sandboxed OpenCode: %w", err)
	}
	return nil
}
