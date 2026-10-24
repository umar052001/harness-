package exec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStreamCancellationReapsWithAbandonedConsumer(t *testing.T) {
	script := filepath.Join(t.TempDir(), "executor")
	// Fill the event channel before sleeping. Cancellation must not require a reader.
	raw := "#!/bin/sh\ni=0; while [ $i -lt 600 ]; do printf '{\"type\":\"output\"}\\n' >&3; i=$((i+1)); done\nexec sleep 60\n"
	if err := os.WriteFile(script, []byte(raw), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := (&Client{Binary: script}).Stream(ctx, &StreamRequest{Workspace: t.TempDir(), Argv: []string{"ignored"}, Stdio: StdioPipe})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("cleanup blocked on an abandoned event consumer")
	}
	if s.cmd.ProcessState == nil {
		t.Fatal("Close returned before the executor was reaped")
	}
	if code := s.Wait(t.Context()); code == 0 {
		t.Fatalf("cancelled executor reported success: %d", code)
	}
}
