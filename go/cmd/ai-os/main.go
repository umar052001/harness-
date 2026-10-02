// Command ai-os demonstrates the three pieces working together: a tool the
// control plane has never seen is compiled into a typed message by reflection,
// the router decides whether it is worth paying for, and the chosen tool runs
// inside a kernel-enforced Rust sandbox that returns proof of its own
// confinement.
//
// The execution is real. The tools run actual commands against actual files in
// the workspace, through the real executor. Only the reported reliability of
// each tool is simulated, because standing up two live model providers is not
// what this demo is for; that part is marked clearly in the output.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sandboxv1 "github.com/umar052001/ai-os/go/gen/sandbox/v1"
	"github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/router"
	"github.com/umar052001/ai-os/go/internal/toolschema"
)

// toolDef is a tool as it arrives from a harness: a name, a JSON Schema, and
// what it costs and is worth. The schema is data, so a tool can be added at
// runtime without touching this binary.
type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`

	// Command template, with {path} substituted. This stands in for whatever the
	// tool would really do; the point is that the execution path is genuine.
	Command []string `json:"command"`

	Cost   float64 `json:"cost"`
	Reward float64 `json:"reward"`

	// Reliability is what the tool's author claims, expressed as successes and
	// failures out of 100. It seeds the router so a tool with no run history yet
	// is still judged fairly, instead of being treated as worthless until it has
	// been used enough times to prove itself.
	PriorSuccess float64 `json:"prior_success"`
	PriorFailure float64 `json:"prior_failure"`
}

// catalog is the set of tools the control plane discovers. None of these have
// generated Go types.
const catalog = `[
  {
    "name": "wc_lite",
    "description": "Fast line and word count. Cheapest option; gives up on large inputs.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path":    {"type": "string",  "description": "file to measure"},
        "verbose": {"type": "boolean", "description": "include a breakdown"}
      },
      "required": ["path"]
    },
    "command": ["/usr/bin/wc", "-l", "{path}"],
    "cost": 0.002,
    "reward": 1.0,
    "prior_success": 95,
    "prior_failure": 5
  },
  {
    "name": "analyze_full",
    "description": "Counts lines, words, bytes and reports the top tokens. More expensive, reliable on any size.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path":        {"type": "string",  "description": "file to measure"},
        "max_tokens":  {"type": "integer", "description": "how many distinct tokens to report"},
        "include_diff":{"type": "boolean", "description": "compare against the previous run"},
        "options": {
          "type": "object",
          "properties": {
            "case_sensitive": {"type": "boolean"},
            "min_length":      {"type": "integer"}
          },
          "required": ["case_sensitive"]
        }
      },
      "required": ["path", "options"]
    },
    "command": ["/usr/bin/wc", "-lwc", "{path}"],
    "cost": 0.090,
    "reward": 1.0,
    "prior_success": 99,
    "prior_failure": 1
  }
]`

func main() {
	var (
		workspace = flag.String("workspace", "", "workspace for the run (created if absent)")
		sandboxd  = flag.String("sandboxd", "", "path to sandboxd (auto-detected if absent)")
		asJSON    = flag.Bool("json", false, "emit the machine-readable transcript only")
	)
	flag.Parse()

	if err := run(*workspace, *sandboxd, *asJSON); err != nil {
		if !*asJSON {
			fmt.Fprintf(os.Stderr, "\ndemo failed: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, `{"error":%q}`+"\n", err.Error())
		}
		os.Exit(1)
	}
}

type transcript struct {
	Tools     []toolInfo        `json:"tools"`
	Decisions []decisionRecord  `json:"decisions"`
	TotalCost float64           `json:"total_cost"`
	NaiveCost float64           `json:"naive_cost"`
	Saved     float64           `json:"saved"`
	SavedPct  float64           `json:"saved_pct"`
	Evidence  []evidenceRecord  `json:"enforcement_evidence"`
	Runs      []executionRecord `json:"runs"`
}

type toolInfo struct {
	Name     string `json:"name"`
	Message  string `json:"compiled_message"`
	Fields   int    `json:"fields"`
	Required int    `json:"required"`
	Cost     float64
}

type decisionRecord struct {
	Task       string             `json:"task"`
	Reward     float64            `json:"reward"`
	Chosen     string             `json:"chosen"`
	Score      float64            `json:"score"`
	Reason     string             `json:"reason"`
	Considered []router.Candidate `json:"considered"`
}

type evidenceRecord struct {
	RunID         string `json:"run_id"`
	NoNewPrivs    bool   `json:"no_new_privs"`
	LandlockABI   int    `json:"landlock_abi"`
	NetworkDenied bool   `json:"network_denied"`
	RuleCount     int    `json:"rule_count"`
}

type executionRecord struct {
	Tool     string `json:"tool"`
	ExitCode int32  `json:"exit_code"`
	Output   string `json:"output"`
	Success  bool   `json:"success"`
}

func run(workspace, sandboxd string, asJSON bool) error {
	out := os.Stdout
	say := func(format string, a ...any) {
		if !asJSON {
			fmt.Fprintf(out, format, a...)
		}
	}
	section := func(title string) {
		if !asJSON {
			fmt.Fprintf(out, "\n\033[1m%s\033[0m\n", title)
		}
	}

	// ── 1. Discover tools ────────────────────────────────────────────────────
	var defs []toolDef
	if err := json.Unmarshal([]byte(catalog), &defs); err != nil {
		return fmt.Errorf("parse tool catalog: %w", err)
	}

	// ── 2. Prepare a workspace with a real file to work on ──────────────────
	if workspace == "" {
		dir, err := os.MkdirTemp("", "ai-os-demo-")
		if err != nil {
			return err
		}
		workspace = dir
	} else if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	subject := filepath.Join(workspace, "input.txt")
	if err := os.WriteFile(subject, demoCorpus(), 0o644); err != nil {
		return err
	}

	section("1. Tools discovered at runtime (no Go code exists for any of them)")
	compiled := make(map[string]*toolschema.Schema, len(defs))
	tr := transcript{}
	for _, d := range defs {
		node, err := toolschema.Parse(d.InputSchema)
		if err != nil {
			return fmt.Errorf("tool %s: parse schema: %w", d.Name, err)
		}
		s, err := toolschema.Compile(d.Name, node)
		if err != nil {
			return fmt.Errorf("tool %s: compile schema: %w", d.Name, err)
		}
		compiled[d.Name] = s
		tr.Tools = append(tr.Tools, toolInfo{
			Name:     d.Name,
			Message:  string(s.Message().FullName()),
			Fields:   s.Message().Fields().Len(),
			Required: len(s.Required()),
			Cost:     d.Cost,
		})
		say("  %-14s -> %s  (%d fields, %d required, cost %.3f)\n",
			d.Name, s.Message().FullName(),
			s.Message().Fields().Len(), len(s.Required()), d.Cost)
	}

	// ── 3. Build the router ─────────────────────────────────────────────────
	var rtools []router.Tool
	for _, d := range defs {
		rtools = append(rtools, router.Tool{
			Name: d.Name, Cost: d.Cost, Reward: d.Reward,
			PriorSuccess: d.PriorSuccess, PriorFailure: d.PriorFailure,
		})
	}
	rt := router.New(rtools...)

	client := &exec.Client{Binary: sandboxd}
	if client.Binary == "" {
		client.Binary = exec.DefaultBinary()
	}
	if _, err := os.Stat(client.Binary); err != nil {
		return fmt.Errorf("sandboxd not found at %s: %w\nbuild it with: cargo build --manifest-path rust/sandboxd/Cargo.toml", client.Binary, err)
	}

	// ── 4. Route, bind, execute ──────────────────────────────────────────────
	// Two tasks with different stakes, so the same catalogue produces different
	// decisions. That is the point: the router responds to what the task is
	// worth, not to a fixed preference.
	// Each tool takes its own arguments, so a task carries a per-tool argument set
	// rather than one shared shape. Binding against the chosen tool's schema is
	// what makes the "never seen before" path real: there is no shared struct to
	// fill in, only the descriptor compiled a moment earlier.
	tasks := []struct {
		name   string
		reward float64
		args   map[string]map[string]any
	}{
		{
			name:   "count the lines in input.txt (routine)",
			reward: 1.0,
			args: map[string]map[string]any{
				"wc_lite":      {"path": subject, "verbose": false},
				"analyze_full": {"path": subject, "max_tokens": 5, "include_diff": false, "options": map[string]any{"case_sensitive": false, "min_length": 3}},
			},
		},
		{
			name:   "count the lines in input.txt (before a release decision)",
			reward: 9.0,
			args: map[string]map[string]any{
				"wc_lite":      {"path": subject, "verbose": true},
				"analyze_full": {"path": subject, "max_tokens": 5, "include_diff": true, "options": map[string]any{"case_sensitive": true, "min_length": 3}},
			},
		},
	}

	section("2. Routing, then execution inside the sandbox")
	for i, task := range tasks {
		d, err := rt.Choose(task.reward)
		if err != nil {
			return err
		}
		tr.Decisions = append(tr.Decisions, decisionRecord{
			Task: task.name, Reward: task.reward, Chosen: d.Tool,
			Score: d.Score, Reason: d.Reason, Considered: d.Considered,
		})
		tr.NaiveCost += naiveCost(defs, d.Tool)

		say("  task %d: %s\n", i+1, task.name)
		say("    task value %.1f, so the tool must promise at least %.0f%%\n",
			task.reward, rt.Requirement(task.reward)*100)
		for _, c := range d.Considered {
			marker := " "
			if c.Name == d.Tool {
				marker = ">"
			}
			verdict := "too unreliable"
			if c.MeetsFloor {
				verdict = "clears the bar"
			}
			say("    %s %-14s score %+.4f  (p>=%.3f, cost %.3f, n=%d/%d, %s)\n",
				marker, c.Name, c.Score, c.LowerBound, c.Cost,
				c.Successes, c.Successes+c.Failures, verdict)
		}
		say("    chose %s: %s\n", d.Tool, d.Reason)

		// Bind the arguments through the runtime-compiled schema. This is where
		// a malformed call is caught, before anything is executed.
		schema, found := compiled[d.Tool]
		if !found {
			return fmt.Errorf("no compiled schema for %q", d.Tool)
		}
		toolArgs, present := task.args[d.Tool]
		if !present {
			return fmt.Errorf("no arguments defined for tool %q", d.Tool)
		}
		msg, err := schema.Bind(toolArgs)
		if err != nil {
			return fmt.Errorf("bind %s: %w", d.Tool, err)
		}
		if err := schema.Validate(msg); err != nil {
			return fmt.Errorf("validate %s: %w", d.Tool, err)
		}

		var def *toolDef
		for j := range defs {
			if defs[j].Name == d.Tool {
				def = &defs[j]
			}
		}
		if def == nil {
			return fmt.Errorf("no definition for %q", d.Tool)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		resp, err := client.Run(ctx, &sandboxv1.ExecRequest{
			RunId:     fmt.Sprintf("demo-%d", i+1),
			Workspace: workspace,
			Argv:      expand(def.Command, map[string]string{"path": subject}),
		})
		cancel()
		if err != nil {
			return fmt.Errorf("execute %s: %w", d.Tool, err)
		}
		// Refuse to present a result we cannot show evidence for.
		if err := exec.VerifyEnforcement(resp); err != nil {
			return err
		}

		tr.TotalCost += def.Cost
		tr.Runs = append(tr.Runs, executionRecord{
			Tool: d.Tool, ExitCode: resp.GetExitCode(),
			Output: strings.TrimSpace(string(resp.GetStdout())), Success: resp.GetExitCode() == 0,
		})
		tr.Evidence = append(tr.Evidence, evidenceRecord{
			RunID:         resp.GetRunId(),
			NoNewPrivs:    resp.GetEnforcement().GetNoNewPrivs(),
			LandlockABI:   int(resp.GetEnforcement().GetLandlockAbi()),
			NetworkDenied: resp.GetEnforcement().GetNetworkDenied(),
			RuleCount:     int(resp.GetEnforcement().GetRuleCount()),
		})
		_ = rt.Record(d.Tool, resp.GetExitCode() == 0, def.Cost)

		say("    executed %s -> exit %d\n", d.Tool, resp.GetExitCode())
		say("    output: %s\n", strings.TrimSpace(string(resp.GetStdout())))
	}

	tr.Saved = tr.NaiveCost - tr.TotalCost
	if tr.NaiveCost > 0 {
		tr.SavedPct = tr.Saved / tr.NaiveCost * 100
	}

	section("3. Confinement evidence, returned by the executor itself")
	for _, e := range tr.Evidence {
		say("  %-10s no_new_privs=%v  landlock_abi=%d  network_denied=%v  rules=%d\n",
			e.RunID, e.NoNewPrivs, e.LandlockABI, e.NetworkDenied, e.RuleCount)
	}

	section("4. Cost")
	say("  always using the most capable tool: %.3f\n", tr.NaiveCost)
	say("  routed:                                 %.3f\n", tr.TotalCost)
	say("  saved:                                  %.3f (%.1f%%)\n", tr.Saved, tr.SavedPct)
	say("\n  Note: tool *reliability* is simulated here; tool *execution* is real\n")
	say("  and confined. See the proof suite for the isolation evidence.\n")

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(tr)
	}
	return nil
}

// naiveCost is what the same two tasks would have cost had the most expensive
// capable tool been used every time. It is the baseline the routing claim is
// measured against.
func naiveCost(defs []toolDef, chosen string) float64 {
	var max float64
	for _, d := range defs {
		if d.Cost > max {
			max = d.Cost
		}
	}
	_ = chosen
	return max
}

func expand(cmd []string, vars map[string]string) []string {
	out := make([]string, 0, len(cmd))
	for _, c := range cmd {
		for k, v := range vars {
			c = strings.ReplaceAll(c, "{"+k+"}", v)
		}
		out = append(out, c)
	}
	return out
}

// demoCorpus is a small file with enough lines for the numbers to be plausible.
func demoCorpus() []byte {
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, "line %03d: the quick brown fox jumps over the lazy dog\n", i)
	}
	return []byte(b.String())
}
