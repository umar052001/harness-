package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
)

func TestStopPersistsTerminalStateAndReleasesSlot(t *testing.T) {
	br := &blockingRunner{reg: harness.Load(), started: make(chan struct{}), release: make(chan struct{})}
	server := api.New(br, api.Options{WorkspacesDir: t.TempDir()})
	s := httptest.NewServer(server.Handler())
	defer s.Close()
	res, err := http.Post(s.URL+"/api/run", "application/json", strings.NewReader(`{"harnessId":"opencode","prompt":"cancel me"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	<-br.started
	active, err := http.Get(s.URL + "/api/runs/active")
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Active []api.ActiveRun `json:"active"`
	}
	if err := json.NewDecoder(active.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	active.Body.Close()
	if len(body.Active) != 1 {
		t.Fatalf("active: %+v", body)
	}
	id := body.Active[0].RunID
	stop, err := http.Post(s.URL+"/api/runs/"+id+"/stop", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop.Body.Close()
	if stop.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: %d", stop.StatusCode)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"status":"cancelled"`) {
		t.Fatalf("missing terminal state: %s", raw)
	}
	history, err := http.Get(s.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer history.Body.Close()
	var result struct {
		Runs []api.Run `json:"runs"`
	}
	if err := json.NewDecoder(history.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Runs) != 1 || result.Runs[0].Status != "cancelled" || result.Runs[0].Model != "opencode/big-pickle" {
		t.Fatalf("durable result: %+v", result.Runs)
	}
	replay, err := http.Get(s.URL + "/api/workspace/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	raw, _ = io.ReadAll(replay.Body)
	if !strings.Contains(string(raw), `"status":"cancelled"`) {
		t.Fatalf("replay lost cancellation: %s", raw)
	}
	active, err = http.Get(s.URL + "/api/runs/active")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Body.Close()
	raw, _ = io.ReadAll(active.Body)
	if !strings.Contains(string(raw), `"active":[]`) {
		t.Fatalf("slot not released: %s", raw)
	}
}

func TestRejectsUnboundedTimeoutAndForeignOrigin(t *testing.T) {
	s := newTestServer(t)
	for _, body := range []string{`{"harnessId":"api-fake","prompt":"x","timeoutSec":-1}`, `{"harnessId":"api-fake","prompt":"x","timeoutSec":9223372036854775807}`} {
		res, err := http.Post(s.URL+"/api/run", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("timeout accepted: %s status %d", body, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", s.URL+"/api/run", strings.NewReader(`{"harnessId":"api-fake","prompt":"x"}`))
	req.Header.Set("Origin", "http://hostile.example")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatalf("foreign origin accepted: %d", res.StatusCode)
	}
}

func TestOnlyOneServerOwnsWorkspaceStore(t *testing.T) {
	root := t.TempDir()
	first := api.New(&blockingRunner{reg: harness.Load()}, api.Options{WorkspacesDir: root})
	second := api.New(&blockingRunner{reg: harness.Load()}, api.Options{WorkspacesDir: root})
	unlock, err := first.LockWorkspaceStore()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := second.LockWorkspaceStore(); err == nil {
		release()
		t.Fatal("second server acquired the same store")
	}
}
