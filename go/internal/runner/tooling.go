package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Tooling contains installed, operator-approved tools. It is not HTTP input.
type Tooling struct {
	OllamaURL   string   `json:"ollamaURL,omitempty"`
	GraphifyMCP string   `json:"graphifyMcp"`
	Ponytail    string   `json:"ponytail"`
	AuditPlugin string   `json:"auditPlugin"`
	ReadPaths   []string `json:"readPaths"`
}

func toolingPaths(t *Tooling) []string {
	if t == nil {
		return nil
	}
	return t.ReadPaths
}

func prepareTooling(ws string, t *Tooling) error {
	path := filepath.Join(ws, ".home", ".config", "opencode", "opencode.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	endpoint := t.OllamaURL
	if endpoint == "" {
		endpoint = "http://127.0.0.1:11434/v1"
	}
	config["providers"] = map[string]any{"ollama": map[string]any{
		"package":  "@opencode/ai/providers/openai-compatible",
		"settings": map[string]string{"baseURL": endpoint},
		"models": map[string]any{
			"gemma4:cloud":    map[string]any{"modelID": "gemma4:cloud", "capabilities": map[string]bool{"tools": true}},
			"kimi-k2.6:cloud": map[string]any{"modelID": "kimi-k2.6:cloud", "capabilities": map[string]bool{"tools": true}},
		},
	}, "openai": map[string]any{"settings": map[string]string{"reasoningEffort": "low"}}}
	config["plugins"] = []string{t.Ponytail, t.AuditPlugin}
	config["tool_output"] = map[string]int{"max_lines": 200, "max_bytes": 12000}
	config["compaction"] = map[string]any{"auto": true, "keep": map[string]int{"tokens": 8000}, "buffer": "15%"}
	config["warming"] = false
	config["mcp"] = map[string]any{"servers": map[string]any{"graphify": map[string]any{
		"type": "local", "command": []string{t.GraphifyMCP, "--graph", filepath.Join(ws, "graphify-out", "graph.json")},
	}}}
	raw, err = json.Marshal(config)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
