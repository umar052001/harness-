package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectsModelThroughAdvertisedACPConfig(t *testing.T) {
	report := filepath.Join(t.TempDir(), "selection")
	client, err := Connect(t.Context(), os.Args[0], nil,
		append(os.Environ(), agentModeEnv+"=normal", "AI_OS_TEST_AGENT_REPORT="+report),
		Options{Cwd: t.TempDir(), Model: "opencode/big-pickle"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"value":"opencode/big-pickle"`) {
		t.Fatalf("model was not selected through configOptions: %s", raw)
	}
}
