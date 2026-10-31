package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/umar052001/ai-os/go/internal/runner"
	"github.com/umar052001/ai-os/go/internal/usage"
)

// beginJob and Shutdown share a lock so shutdown cannot race a WaitGroup.Add.
func (s *Server) beginJob() bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.stopping {
		return false
	}
	s.jobs.Add(1)
	return true
}

// executeJob persists the same events sent to its optional live observer. The
// caller owns the worker lifetime; the runner returns only after agent cleanup.
func (s *Server) executeJob(ctx context.Context, req runner.Request, id string, meta RunMetadata, record func([]byte), sink *sseSink) {
	var usageMu sync.Mutex
	err := s.run.Run(ctx, req, func(ev runner.Event) {
		if ev.Kind == runner.KindUsage {
			var report struct {
				Usage *usage.Report `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &report) == nil && report.Usage != nil {
				usageMu.Lock()
				meta.Usage = *report.Usage
				usageMu.Unlock()
			}
		}
		if ev.Kind == runner.KindRunFinished {
			return
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		record(payload)
		sink.frame(ev.Kind, string(payload))
	})
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	finished := time.Now()
	meta.FinishedAt = &finished
	meta.Status = "completed"
	if err != nil {
		meta.Status, meta.Error = "failed", err.Error()
		if errors.Is(err, context.Canceled) {
			meta.Status, meta.Error = "cancelled", "Run stopped. Files already written are saved."
		}
		if errors.Is(err, context.DeadlineExceeded) {
			meta.Status, meta.Error = "timed_out", "Time limit reached. Files already written are saved."
		}
	}
	if saveErr := s.store.saveMetadata(id, meta); saveErr != nil {
		log.Printf("save run %s: %v", id, saveErr)
	}
	terminal, _ := json.Marshal(runner.Event{Kind: runner.KindRunFinished, At: finished, Text: meta.Status,
		Data: json.RawMessage(mustJSONStatus(err == nil, meta.Status, meta.Error))})
	record(terminal)
	sink.frame(runner.KindRunFinished, string(terminal))
	end := map[string]any{"ok": err == nil}
	if err != nil {
		end["error"] = meta.Error
		if errors.Is(err, context.DeadlineExceeded) {
			end["timeout"] = true
		}
	}
	if payload, err := json.Marshal(end); err == nil {
		sink.frame("stream.end", string(payload))
	}
}

// Shutdown stops accepting jobs, cancels owned work, and waits for process cleanup
// and terminal metadata to be written. Browser connections do not own jobs.
func (s *Server) Shutdown(ctx context.Context) error {
	s.jobsMu.Lock()
	s.stopping = true
	s.cancelJobs()
	s.jobsMu.Unlock()
	done := make(chan struct{})
	go func() { s.jobs.Wait(); s.apps.Stop(""); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
