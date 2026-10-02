// Package narrator turns a run's machine events into one sentence a person can
// read.
//
// It exists because the raw stream is not a status. A run emits typed events
// that a control plane needs - sandbox enforcement, tool calls, usage counters,
// stderr - and that is exactly the wrong shape to show someone waiting. The
// question a user actually has is "what is happening right now", and the honest
// answer is usually knowable locally from the phase, the tool being run and the
// elapsed time.
//
// So the local answer is the primary one and is always available. A hosted model
// can replace it with something better written, but never gates it: if the key
// is missing, the provider is slow, or the model returns nonsense, the caller
// still gets a sentence. Degrading to the previous behaviour is the whole design.
package narrator

import (
	"context"
	"fmt"
	"strings"
)

// Status is the small, already-interpreted summary a caller has.
type Status struct {
	// Phase is one of the coarse states the UI shows: starting, working,
	// reviewing, done, failed.
	Phase string `json:"phase"`
	// Harness is the agent's display name.
	Harness string `json:"harness,omitempty"`
	// Step is a short description of the current action, such as
	// "Running: npm install". Empty means the agent is between actions.
	Step string `json:"step,omitempty"`
	// Tools is how many tool calls have happened.
	Tools int `json:"tools"`
	// Files is how many files the run has written.
	Files int `json:"files"`
	// ElapsedSec is how long the run has been going.
	ElapsedSec int `json:"elapsedSec"`
	// LastMessage is the agent's most recent sentence.
	LastMessage string `json:"lastMessage,omitempty"`
	// Error is set when the run failed.
	Error string `json:"error,omitempty"`
}

// Source says where a sentence came from.
const (
	// SourceLocal is the deterministic sentence built here.
	SourceLocal = "local"
	// SourceModel is a sentence written by the hosted model.
	SourceModel = "model"
)

// Backend is the model call the narrator needs. *planner.Planner satisfies it;
// so does a test double.
type Backend interface {
	Configured() bool
	Complete(ctx context.Context, system, user string, jsonMode bool) (string, error)
}

// narratePrompt asks for prose, not JSON: this value is rendered directly, and a
// parse step between the model and the screen would be one more way to show a
// user an error instead of a status.
const narratePrompt = `You are the status narrator for a coding agent control plane.
You are given the current state of a run. Reply with ONE sentence, at most 18
words, in the present tense, saying what the agent is doing right now.

Rules:
- Describe the current action, not the history and not the goal.
- No preamble, no quotes, no emoji, no trailing period style guide.
- If it failed, say what failed in plain words.
- Never invent files, commands or results that are not in the state.`

// Narrate returns a sentence describing the run.
//
// It always succeeds. A model error is not an error for the caller: the local
// sentence is returned instead, and the caller can see which one it got from the
// returned source.
func Narrate(ctx context.Context, backend Backend, st Status) (sentence, source string) {
	local := Local(st)
	if backend == nil || !backend.Configured() {
		return local, SourceLocal
	}
	payload := render(st)
	out, err := backend.Complete(ctx, narratePrompt, payload, false)
	if err != nil {
		return local, SourceLocal
	}
	out = clean(out)
	// A model that returns nothing useful is worse than the local sentence, so
	// an empty or overlong answer is treated as a failure and falls back.
	if out == "" || len(out) > 240 {
		return local, SourceLocal
	}
	return out, SourceModel
}

// Local builds the sentence without a model.
//
// It is written to be specific where the run is specific - naming the command
// being run beats saying "working" - and honest about what it does not know.
func Local(st Status) string {
	switch st.Phase {
	case "failed":
		if st.Error != "" {
			return trim(st.Error, 90)
		}
		return "The run failed before the agent reported a reason."
	case "done":
		if st.Files > 0 {
			return fmt.Sprintf("Done. %s in the workspace.", capitalise(pluralFiles(st.Files)))
		}
		return "Done. The agent finished without writing any files."
	case "starting":
		if st.Harness != "" {
			return fmt.Sprintf("Starting %s in a confined workspace.", st.Harness)
		}
		return "Starting the agent in a confined workspace."
	case "reviewing":
		return "The agent is reviewing what it has done so far."
	}
	// working
	if st.Step != "" {
		return trim(st.Step, 90)
	}
	if st.LastMessage != "" {
		return trim(st.LastMessage, 90)
	}
	if st.Tools > 0 {
		return fmt.Sprintf("The agent is working. %d tool calls so far.", st.Tools)
	}
	if st.Harness != "" {
		return fmt.Sprintf("%s is thinking about the request.", st.Harness)
	}
	return "The agent is thinking about the request."
}

func pluralFiles(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// render is the model-facing state.
func render(st Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phase: %s\n", st.Phase)
	if st.Harness != "" {
		fmt.Fprintf(&b, "agent: %s\n", st.Harness)
	}
	if st.Step != "" {
		fmt.Fprintf(&b, "current action: %s\n", st.Step)
	}
	if st.LastMessage != "" {
		fmt.Fprintf(&b, "agent's latest words: %s\n", truncate(st.LastMessage, 200))
	}
	fmt.Fprintf(&b, "tool calls so far: %d\n", st.Tools)
	fmt.Fprintf(&b, "files written: %d\n", st.Files)
	fmt.Fprintf(&b, "elapsed: %s\n", humanDuration(st.ElapsedSec))
	if st.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", truncate(st.Error, 200))
	}
	return b.String()
}

// clean normalises a model's sentence: no quotes, no markdown, one line.
func clean(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'`")
	s = strings.TrimPrefix(s, "**")
	s = strings.TrimSuffix(s, "**")
	// Models sometimes answer in more than one line; the first is the sentence.
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.Trim(s, "\"'` "))
}

func humanDuration(sec int) string {
	switch {
	case sec <= 0:
		return "under a second"
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm %ds", sec/60, sec%60)
	default:
		return fmt.Sprintf("%dh %dm", sec/3600, (sec%3600)/60)
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "\u2026"
}

// trim keeps a locally built sentence inside the display budget.
func trim(s string, n int) string { return truncate(s, n) }
