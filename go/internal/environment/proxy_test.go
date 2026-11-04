package environment

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAppProxySlowResponseAndCancellation(t *testing.T) {
	t.Parallel()
	started, cancelled := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		// Regression: text generation must survive the old ten-second deadline.
		select {
		case <-time.After(11 * time.Second):
			io.WriteString(w, "story ready")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	proxy := httptest.NewServer(NewAppProxy(upstream.URL))
	defer proxy.Close()
	client := &http.Client{Timeout: 20 * time.Second}
	res, err := client.Get(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 || string(body) != "story ready" {
		t.Fatalf("slow response: status=%d body=%q err=%v", res.StatusCode, body, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/cancel", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if res, err := client.Do(req); err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive cancellation")
	}
	<-done
}
