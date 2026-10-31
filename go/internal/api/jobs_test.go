package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
)

func TestDisconnectDoesNotCancelJobAndShutdownDoes(t *testing.T) {
	br := &blockingRunner{reg: harness.Load(), started: make(chan struct{}), release: make(chan struct{})}
	server := api.New(br, api.Options{WorkspacesDir: t.TempDir()})
	s := httptest.NewServer(server.Handler())
	defer s.Close()
	defer server.Shutdown(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.URL+"/api/run", strings.NewReader(`{"harnessId":"opencode","prompt":"keep working"}`))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-br.started
	cancel()
	res.Body.Close()
	res, err = http.Get(s.URL + "/api/runs/active")
	if err != nil {
		t.Fatal(err)
	}
	var live struct {
		Active []api.ActiveRun `json:"active"`
	}
	if err := json.NewDecoder(res.Body).Decode(&live); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(live.Active) != 1 {
		t.Fatalf("disconnect lost job: %+v", live)
	}
	shutdown, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(s.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"status":"cancelled"`) {
		t.Fatalf("shutdown did not persist outcome: %s", body)
	}
	res, err = http.Post(s.URL+"/api/run", "application/json", strings.NewReader(`{"harnessId":"opencode","prompt":"too late"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("accepted job during shutdown: %d", res.StatusCode)
	}
}
