package narrator

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeBackend stands in for the hosted model.
type fakeBackend struct {
	configured bool
	reply      string
	err        error
	gotSystem  string
	gotUser    string
	jsonMode   bool
}

func (f *fakeBackend) Configured() bool { return f.configured }

func (f *fakeBackend) Complete(_ context.Context, system, user string, jsonMode bool) (string, error) {
	f.gotSystem, f.gotUser, f.jsonMode = system, user, jsonMode
	return f.reply, f.err
}

func TestLocalSentenceNamesTheAction(t *testing.T) {
	// The local sentence is the primary output, so it has to be specific. "The
	// agent is working" for a run that is visibly running npm install is the kind
	// of technically-correct status nobody can act on.
	got := Local(Status{Phase: "working", Harness: "OpenCode", Step: "Running: npm install", Tools: 4})
	if !strings.Contains(got, "npm install") {
		t.Errorf("Local = %q, want it to name the command", got)
	}
}

func TestLocalSentenceForEachPhase(t *testing.T) {
	cases := []struct {
		name string
		st   Status
		want string
	}{
		{"starting", Status{Phase: "starting", Harness: "OpenCode"}, "OpenCode"},
		{"reviewing", Status{Phase: "reviewing"}, "reviewing"},
		{"done with files", Status{Phase: "done", Files: 3}, "3 files"},
		{"done without files", Status{Phase: "done"}, "without writing"},
		{"failed", Status{Phase: "failed", Error: "acp: no response"}, "no response"},
		{"working no action", Status{Phase: "working", Tools: 2}, "2 tool calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Local(tc.st)
			if !strings.Contains(got, tc.want) {
				t.Errorf("Local(%+v) = %q, want it to mention %q", tc.st, got, tc.want)
			}
			if len(got) > 120 {
				t.Errorf("Local(%+v) = %q, longer than a status line should be", tc.st, got)
			}
		})
	}
}

func TestNarrateFallsBackWithoutAModel(t *testing.T) {
	// No backend at all. A status line that vanishes when a key is missing is a
	// worse product than a plain one.
	got, source := Narrate(context.Background(), nil, Status{Phase: "working", Step: "Running: tsc"})
	if source != SourceLocal {
		t.Errorf("source = %q, want local", source)
	}
	if !strings.Contains(got, "tsc") {
		t.Errorf("got %q", got)
	}
}

func TestNarrateFallsBackWhenTheModelFails(t *testing.T) {
	b := &fakeBackend{configured: true, err: errors.New("503 from provider")}
	got, source := Narrate(context.Background(), b, Status{Phase: "working", Step: "Running: tsc"})
	if source != SourceLocal {
		t.Errorf("source = %q, want local when the provider fails", source)
	}
	if !strings.Contains(got, "tsc") {
		t.Errorf("got %q", got)
	}
}

func TestNarrateFallsBackOnRubbish(t *testing.T) {
	// A model that returns an empty or rambling answer is worse than the local
	// sentence, so both are treated as failures.
	for _, reply := range []string{"", "   ", strings.Repeat("word ", 200)} {
		b := &fakeBackend{configured: true, reply: reply}
		got, source := Narrate(context.Background(), b, Status{Phase: "working", Step: "Running: tsc"})
		if source != SourceLocal {
			t.Errorf("reply %q: source = %q, want local", reply[:min(len(reply), 20)], source)
		}
		if !strings.Contains(got, "tsc") {
			t.Errorf("reply %q: got %q", reply[:min(len(reply), 20)], got)
		}
	}
}

func TestNarrateUsesTheModelWhenItAnswers(t *testing.T) {
	b := &fakeBackend{configured: true, reply: `"Installing the project's dependencies."`}
	got, source := Narrate(context.Background(), b, Status{Phase: "working", Step: "Running: npm install"})
	if source != SourceModel {
		t.Fatalf("source = %q, want model", source)
	}
	if got != "Installing the project's dependencies." {
		t.Errorf("got %q, want the quotes stripped", got)
	}
	if b.jsonMode {
		t.Error("narration asked for JSON; this value is rendered directly")
	}
	// The model is told what it may say, so it is not asked to invent one.
	for _, want := range []string{"phase: working", "current action: Running: npm install", "tool calls so far:"} {
		if !strings.Contains(b.gotUser, want) {
			t.Errorf("payload is missing %q:\n%s", want, b.gotUser)
		}
	}
}

func TestCleanKeepsOneLine(t *testing.T) {
	got := clean("Line one.\nLine two.")
	if got != "Line one." {
		t.Errorf("clean = %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
