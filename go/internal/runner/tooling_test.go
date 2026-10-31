package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolingUsesPinnedPluginsAndBoundedContext(t *testing.T) {
	ws := t.TempDir()
	if err := prepareOpenCode(ws, "ollama/gemma4:cloud"); err != nil {
		t.Fatal(err)
	}
	if err := prepareTooling(ws, &Tooling{Ponytail: "/approved/ponytail", AuditPlugin: "/approved/audit", GraphifyMCP: "/approved/graphify-mcp"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".home/.config/opencode/opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Plugins    []string
		Warming    bool
		ToolOutput struct {
			MaxBytes int `json:"max_bytes"`
		} `json:"tool_output"`
		Compaction struct {
			Auto bool
			Keep struct{ Tokens int }
		}
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Plugins) != 2 || config.Plugins[0] != "/approved/ponytail" || config.Warming || config.ToolOutput.MaxBytes != 12000 || !config.Compaction.Auto || config.Compaction.Keep.Tokens != 8000 {
		t.Fatalf("config: %s", raw)
	}
}
