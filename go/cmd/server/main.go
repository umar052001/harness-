// Command server exposes the harness++ control plane over HTTP.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/umar052001/ai-os/go/internal/api"
	"github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
	"github.com/umar052001/ai-os/go/internal/planner"
	"github.com/umar052001/ai-os/go/internal/runner"
)

func main() {
	var (
		addr         = flag.String("addr", "127.0.0.1:8091", "listen address")
		webdir       = flag.String("web", "", "built frontend directory to serve (optional)")
		sandboxd     = flag.String("sandboxd", "", "path to sandboxd (default: located binary)")
		overlay      = flag.String("harness-overlay", "", "directory of harness manifests to overlay")
		origins      = flag.String("origins", defaultOrigins, "comma-separated CORS allow-list")
		workRoot     = flag.String("workspaces", "", "where run workspaces are created (default: ~/.local/share/harness++/workspaces)")
		workflowRoot = flag.String("workflow-root", "", "harness++ checkout for routed builds (default: binary parent)")
	)
	flag.Parse()
	host, _, err := net.SplitHostPort(*addr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		log.Fatal("-addr must use a loopback IP, for example 127.0.0.1:8091; remote access is not authenticated")
	}
	if *workflowRoot == "" {
		binary, err := os.Executable()
		if err != nil {
			log.Fatal(err)
		}
		*workflowRoot = filepath.Dir(filepath.Dir(binary))
	}

	if *sandboxd == "" {
		*sandboxd = exec.DefaultBinary()
	}
	if _, err := os.Stat(*sandboxd); err != nil {
		log.Fatalf("sandboxd not found at %s: %v\nbuild it with: cargo build --manifest-path rust/sandboxd/Cargo.toml", *sandboxd, err)
	}

	reg := harness.Load()
	if *overlay != "" {
		if err := reg.OverlayDir(*overlay); err != nil {
			log.Fatalf("harness overlay: %v", err)
		}
	}

	run := runner.New(reg, *sandboxd)
	srv := api.New(run, api.Options{
		WorkflowRoot:  *workflowRoot,
		SandboxBinary: *sandboxd,
		// The planner is optional: it only exists when OLLAMA_API_KEY does, and
		// the API reports it as unconfigured instead of pretending otherwise.
		// The key is read from the environment rather than a flag so it never
		// appears in a process list or a history file.
		Planner: planner.New(os.Getenv("OLLAMA_API_KEY"), os.Getenv("OLLAMA_MODEL")),
		Origins: splitCSV(*origins),
		WebDir:  *webdir,
		// Run workspaces are durable on purpose: they are what the dashboard
		// previews, so a run's output has to outlive the run itself.
		WorkspacesDir: *workRoot,
	})

	unlock, err := srv.LockWorkspaceStore()
	if err != nil {
		log.Fatal(err)
	}
	defer unlock()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           api.LocalHandler(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			log.Printf("job shutdown: %v", err)
		}
		_ = httpSrv.Shutdown(shut)
	}()

	log.Printf("harness++ control plane on %s (sandboxd %s)", *addr, *sandboxd)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
	<-shutdownDone
}

const defaultOrigins = "http://localhost:5173,http://127.0.0.1:5173"

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
