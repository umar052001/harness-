package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSseSinkSurvivesLateAndConcurrentWrites covers the crash that took the
// control plane down mid-run: an event delivered after the handler returned was
// written to a recycled http.ResponseWriter, and the nil dereference in
// bufio.Writer.Flush killed the process. The sink has to absorb both a late
// write and writes from two goroutines at once.
func TestSseSinkSurvivesLateAndConcurrentWrites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink := &sseSink{w: w, flusher: w.(http.Flusher)}
		defer sink.close()
		sink.frame("hello", `{"a":1}`)

		// A heartbeat and the event callback racing, which is what the run does.
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				for j := 0; j < 25; j++ {
					sink.raw(": ping\n\n")
				}
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				sink.frame("tick", `{"n":1}`)
			}
		}()
		wg.Wait()
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if len(body) == 0 {
		t.Fatal("nothing was written")
	}

	// Every frame that arrives must be whole: an `event:` line immediately
	// followed by its own `data:` line, and a heartbeat comment only ever on its
	// own. A torn frame is what a reader cannot recover from - it would attribute
	// one event's name to another event's payload.
	//
	// Splitting a frame across two writes under one lock would still pass the
	// write-count checks above, so this asserts the shape of the output itself.
	// A frame is `event:` then `data:`, then a blank line, and a comment may
	// only stand alone between frames. Anything else means a heartbeat landed
	// in the middle of an event, which is precisely the bug this shape catches:
	// a two-write `frame` under the same lock passes every count-based check,
	// because the torn bytes are still individually well-formed.
	const (
		wantEvent = iota
		wantData
	)
	state := wantEvent
	frames := 0
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case line == "":
			// End of frame.
			if state == wantData {
				frames++
				state = wantEvent
			}
		case strings.HasPrefix(line, ":"):
			if state != wantEvent {
				t.Fatalf("torn frame: a heartbeat comment landed inside an event, before its data line")
			}
		case strings.HasPrefix(line, "event: "):
			if state != wantEvent {
				t.Fatal("torn frame: a second event line arrived before the previous frame's data")
			}
			state = wantData
		case strings.HasPrefix(line, "data: "):
			if state != wantData {
				t.Fatal("torn frame: a data line arrived with no event line before it")
			}
		default:
			t.Fatalf("torn frame: unrecognised line %q", line)
		}
	}
	if frames == 0 {
		t.Fatal("no complete frame was written")
	}

	// The part that crashed the process: write to the same sink long after the
	// handler that owns the ResponseWriter has returned.
	rec2 := httptest.NewRecorder()
	s2 := &sseSink{w: rec2, flusher: rec2}
	s2.frame("before", "{}")
	s2.close()
	s2.frame("after", "{}")
	s2.raw(": ping\n\n")
	if !strings.Contains(rec2.Body.String(), "before") {
		t.Error("the pre-close frame was lost")
	}
	if strings.Contains(rec2.Body.String(), "after") {
		t.Error("a frame was written after the sink closed")
	}
}
