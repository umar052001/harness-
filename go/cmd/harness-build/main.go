// Command harness-build plans, routes and verifies OpenCode builds in sandboxd.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/umar052001/ai-os/go/internal/workflow"
)

func main() {
	home, _ := os.UserHomeDir()
	root := flag.String("root", ".", "harness++ checkout")
	prompt := flag.String("prompt", "", "raw request file")
	source := flag.String("source", "", "copy this project; never edit the source")
	store := flag.String("store", filepath.Join(home, ".local/share/harness++/builds"), "private build/report store")
	registry := flag.String("registry", "", "registry JSON (default config/models.json)")
	check := flag.String("check", "", "acceptance command as a JSON argument array")
	allowGPT := flag.Bool("allow-gpt", false, "permit existing ChatGPT OAuth escalation")
	timeout := flag.Duration("timeout", 25*time.Minute, "whole-build deadline, at most 30m")
	cloud := flag.String("cloud-worker", "", "internal sandboxed planning worker")
	credential := flag.String("credential-worker", "", "internal sandboxed credential import")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	if *cloud != "" || *credential != "" {
		worker, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if *cloud != "" {
			err = workflow.CloudWorker(worker, *cloud)
		} else {
			err = workflow.CredentialWorker(worker, *credential)
		}
	} else {
		var argv []string
		if *prompt == "" || json.Unmarshal([]byte(*check), &argv) != nil || len(argv) == 0 {
			fmt.Fprintln(os.Stderr, "-prompt and -check '[\"command\",\"argument\"]' required")
			os.Exit(2)
		}
		raw, e := os.ReadFile(*prompt)
		err = e
		if *registry == "" {
			*registry = filepath.Join(*root, "config/models.json")
		}
		if err == nil {
			var build *workflow.Build
			build, err = workflow.New(workflow.Options{Root: *root, Store: *store, Source: *source, Registry: *registry, Prompt: string(raw), Check: argv, AllowGPT: *allowGPT, Timeout: *timeout})
			if err == nil {
				err = build.Run(ctx)
				fmt.Println("Report:", filepath.Join(build.Dir, "summary.json"))
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-build:", err)
		os.Exit(1)
	}
}
