package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/umar052001/ai-os/go/internal/usage"
)

// RunMetadata is host-owned state, stored beside (not inside) the agent's workspace.
type RunMetadata struct {
	ProjectID   string       `json:"projectId,omitempty"`
	ParentRunID string       `json:"parentRunId,omitempty"`
	Usage       usage.Report `json:"usage"`
	Prompt      string       `json:"prompt,omitempty"`
	HarnessID   string       `json:"harnessId,omitempty"`
	Model       string       `json:"model,omitempty"`
	Status      string       `json:"status,omitempty"`
	StartedAt   time.Time    `json:"startedAt,omitempty"`
	FinishedAt  *time.Time   `json:"finishedAt,omitempty"`
	Error       string       `json:"error,omitempty"`
}

func (s *WorkspaceStore) saveMetadata(id string, m RunMetadata) error {
	if !validID(id) {
		return os.ErrInvalid
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, id+".json")
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func (s *WorkspaceStore) metadata(id string) RunMetadata {
	var m RunMetadata
	if validID(id) {
		raw, err := os.ReadFile(filepath.Join(s.root, id+".json"))
		if err == nil {
			_ = json.Unmarshal(raw, &m)
		}
	}
	return m
}
