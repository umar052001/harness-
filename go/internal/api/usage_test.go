package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

type accountingRunner struct{ reg *harness.Registry }

func (r *accountingRunner) Registry() *harness.Registry { return r.reg }
func (r *accountingRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) error {
	for range 2 {
		emit(runner.Event{Kind: runner.KindUsage, Data: json.RawMessage(`{"usage":{"tokens":{"totalTokens":120,"inputTokens":100,"outputTokens":20},"cost":{"amount":0,"currency":"USD"}}}`)})
	}
	return nil
}

func TestUsageSurvivesServerRestart(t *testing.T) {
	root := t.TempDir()
	r := &accountingRunner{reg: harness.Load()}
	s := httptest.NewServer(api.New(r, api.Options{WorkspacesDir: root}).Handler())
	t.Cleanup(s.Close)
	res, err := http.Post(s.URL+"/api/run", "application/json", strings.NewReader(`{"harnessId":"opencode","prompt":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"totalTokens":120`) {
		t.Fatalf("usage missing from stream: %s", raw)
	}
	s.Close()
	restarted := httptest.NewServer(api.New(r, api.Options{WorkspacesDir: root}).Handler())
	defer restarted.Close()
	res, err = http.Get(restarted.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var history struct {
		Runs []api.Run `json:"runs"`
	}
	if err := json.NewDecoder(res.Body).Decode(&history); err != nil {
		t.Fatal(err)
	}
	if len(history.Runs) != 1 {
		t.Fatalf("runs: %+v", history.Runs)
	}
	u := history.Runs[0].Usage
	if u.Tokens == nil || *u.Tokens.Total != 120 || u.Cost == nil || u.Cost.Amount != 0 {
		t.Fatalf("accounting lost or double-counted: %+v", u)
	}
}
