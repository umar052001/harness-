package main

import (
	"context"
	"fmt"
	"os"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/runner"
)

func main() {
	hID := os.Getenv("H")
	if hID == "" {
		hID = "opencode"
	}
	bin := sbx.DefaultBinary()
	ws, _ := os.MkdirTemp("", "dbg-ws-")
	fmt.Println("sandbox:", bin, "ws:", ws)
	reg := harness.Load()
	r := runner.New(reg, bin)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	err := r.Run(ctx, runner.Request{
		HarnessID: hID, Prompt: "Reply with exactly: ONLINE", Workspace: ws, Timeout: 90 * time.Second,
	}, func(ev runner.Event) {
		fmt.Printf("[%d] %-18s %s\n", ev.Seq, ev.Kind, truncate(ev.Text, 300))
	})
	fmt.Println("ERR:", err)
	fmt.Println("WS kept at:", ws)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
