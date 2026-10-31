package environment

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
)

type AppStatus struct {
	Active       bool   `json:"active"`
	Status       string `json:"status"`
	Verification string `json:"verification"`
	ChecksPassed int    `json:"checksPassed"`
	ChecksTotal  int    `json:"checksTotal"`
	Error        string `json:"error,omitempty"`
}

type appProcess struct {
	service AppService
	address string
	done    chan struct{}
	output  *commandOutput
}

// Apps owns one multi-service project independently of coding jobs. op serializes
// lifecycle operations; mu permits status polling while builds/checks execute.
type Apps struct {
	op          sync.Mutex
	mu          sync.Mutex
	id          string
	status      AppStatus
	processes   []appProcess
	cancel      context.CancelFunc
	gatewayDone chan struct{}
}

func (a *Apps) Status(id string) AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.id == "" || (id != "" && id != a.id) {
		return AppStatus{Status: "stopped", Verification: "not_run"}
	}
	status := a.status
	for _, p := range a.processes {
		select {
		case <-p.done:
		default:
			status.Active = true
		}
	}
	if status.Status == "ready" {
		for _, p := range a.processes {
			select {
			case <-p.done:
				status.Status, status.Error = "failed", "service "+p.service.Name+" exited"
			default:
			}
		}
	}
	return status
}

func (a *Apps) update(change func(*AppStatus)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	change(&a.status)
}

// Address returns the ready root service, or the longest matching route.
func (a *Apps) Address(id string, requestPath ...string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if (id != "" && a.id != id) || a.status.Status != "ready" {
		return ""
	}
	path := "/"
	if len(requestPath) > 0 {
		path = requestPath[0]
	}
	for _, p := range a.processes {
		select {
		case <-p.done:
			return ""
		default:
		}
	}
	return routeAddress(a.processes, path)
}

func routeAddress(processes []appProcess, path string) string {
	address, length := "", 0
	for _, p := range processes {
		route := p.service.Route
		if (route == "/" || path == route || strings.HasPrefix(path, route+"/")) && len(route) > length {
			address, length = p.address, len(route)
		}
	}
	return address
}

func newAppGateway(processes []appProcess) *http.Server {
	return &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, _ := url.Parse(routeAddress(processes, r.URL.Path))
		proxy := httputil.NewSingleHostReverseProxy(target)
		proxy.Transport = &http.Transport{ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
		proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { http.Error(w, "service unavailable", 502) }
		proxy.ServeHTTP(w, r)
	})}
}

func (a *Apps) Stop(id string) {
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	if id != "" && id != a.id {
		a.mu.Unlock()
		return
	}
	cancel, processes := a.cancel, a.processes
	gatewayDone := a.gatewayDone
	a.status.Status = "stopped"
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, p := range processes {
		<-p.done
	}
	if gatewayDone != nil {
		<-gatewayDone
	}
}

func (a *Apps) Start(ctx context.Context, lifetime context.Context, bin, id, workspace, databaseURL string) (address string, err error) {
	a.op.Lock()
	defer a.op.Unlock()
	a.mu.Lock()
	for _, p := range a.processes {
		select {
		case <-p.done:
		default:
			a.mu.Unlock()
			return "", errors.New("stop the running app before starting or rebuilding an app")
		}
	}
	if a.cancel != nil {
		a.cancel()
	}
	if a.gatewayDone != nil {
		<-a.gatewayDone
		a.gatewayDone = nil
	}
	a.id, a.processes = id, nil
	a.status = AppStatus{Status: "building", Verification: "not_run"}
	a.mu.Unlock()
	m, err := LoadApp(workspace)
	if err != nil {
		a.update(func(s *AppStatus) { s.Status = "failed"; s.Error = err.Error() })
		return "", err
	}
	runCtx, cancel := context.WithTimeout(lifetime, 24*time.Hour)
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()
	var processes []appProcess
	var gatewayDone chan struct{}
	defer func() {
		if err != nil {
			cancel()
			for _, p := range processes {
				<-p.done
			}
			if gatewayDone != nil {
				<-gatewayDone
			}
			if safe := redactConnection(err.Error(), databaseURL); safe != err.Error() {
				err = errors.New(safe)
			}
			a.update(func(s *AppStatus) { s.Status = "failed"; s.Error = err.Error() })
		}
	}()
	if m.Database == "postgres" && databaseURL == "" {
		return "", errors.New("manifest requires PostgreSQL but no connection was supplied")
	}
	// Reserve all service ports together, then release each just before launch.
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			l.Close()
		}
	}()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + workspace, "HOST=127.0.0.1", "NODE_ENV=production", "GOTOOLCHAIN=local"}
	if databaseURL != "" {
		env = append(env, "DATABASE_URL="+databaseURL)
	}
	var reserved []appProcess
	for _, service := range m.Services {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			return "", e
		}
		listeners = append(listeners, l)
		url := "http://" + l.Addr().String()
		reserved = append(reserved, appProcess{service: service, address: url})
		// APP_URL always belongs to the gateway, including a root service named app.
		if service.Name != "app" {
			env = append(env, strings.ToUpper(service.Name)+"_URL="+url)
		}
		if service.Route == "/" {
			address = url
		}
	}
	// Checks use the same route table as the published app, including /api on a
	// separate backend. Testing the frontend port alone would miss that wiring.
	gatewayListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	env = append(env, "APP_URL=http://"+gatewayListener.Addr().String())
	gateway := newAppGateway(reserved)
	gatewayDone = make(chan struct{})
	a.mu.Lock()
	a.gatewayDone = gatewayDone
	a.mu.Unlock()
	go func() { <-runCtx.Done(); gateway.Close() }()
	go func() { defer close(gatewayDone); gateway.Serve(gatewayListener) }()
	for i, step := range m.Build {
		if err = runAppStep(ctx, runCtx, bin, workspace, env, step); err != nil {
			return "", fmt.Errorf("build step %d: %w", i+1, err)
		}
	}
	a.update(func(s *AppStatus) { s.Status = "starting"; s.ChecksTotal = len(m.Checks) })
	for i, p := range reserved {
		port := listeners[i].Addr().(*net.TCPAddr).Port
		listeners[i].Close()
		serviceEnv := append(append([]string(nil), env...), "PORT="+strconv.Itoa(port))
		p.output = &commandOutput{}
		stream, e := startAppProcess(runCtx, bin, workspace, serviceEnv, p.service.AppStep, p.output)
		if e != nil {
			return "", fmt.Errorf("start %s: %w", p.service.Name, e)
		}
		p.done = make(chan struct{})
		processes = append(processes, p)
		go func(p appProcess) {
			for range stream.Events() {
			}
			stream.Wait(runCtx)
			stream.Close()
			a.update(func(s *AppStatus) {
				if s.Status == "ready" {
					s.Status = "failed"
					s.Error = redactConnection("service "+p.service.Name+" exited: "+p.output.String(), databaseURL)
				}
			})
			cancel() // A required service exiting tears down its peers as well.
			close(p.done)
		}(p)
	}
	a.mu.Lock()
	a.processes = append([]appProcess(nil), processes...)
	a.mu.Unlock()
	for _, p := range processes {
		if err = waitAppReady(ctx, runCtx, p); err != nil {
			return "", fmt.Errorf("service %s: %w", p.service.Name, err)
		}
	}
	if len(m.Checks) > 0 {
		a.update(func(s *AppStatus) { s.Status = "checking"; s.Verification = "running" })
	}
	for i, step := range m.Checks {
		if err = runAppStep(ctx, runCtx, bin, workspace, env, step); err != nil {
			a.update(func(s *AppStatus) { s.Verification = "failed" })
			return "", fmt.Errorf("check %d: %w", i+1, err)
		}
		a.update(func(s *AppStatus) { s.ChecksPassed++ })
	}
	for _, p := range processes {
		select {
		case <-p.done:
			return "", fmt.Errorf("service %s exited during startup checks", p.service.Name)
		default:
		}
	}
	if err = runCtx.Err(); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	a.update(func(s *AppStatus) {
		s.Status = "ready"
		if len(m.Checks) > 0 {
			s.Verification = "passed"
		}
	})
	return address, nil
}

func startAppProcess(ctx context.Context, bin, workspace string, env []string, step AppStep, output io.Writer) (*sbx.Stream, error) {
	return (&sbx.Client{Binary: bin}).Stream(ctx, &sbx.StreamRequest{
		Workspace: workspace, Cwd: filepath.Join(workspace, step.Cwd), Argv: step.Command, Env: env,
		ReadPaths: sbx.ResolverReadPaths(),
		AllowNet:  true, NetWhy: "build, serve and verify the local project and its database", Stdio: sbx.StdioPipe, Stdout: output, Stderr: output,
	})
}

func runAppStep(ctx, lifetime context.Context, bin, workspace string, env []string, step AppStep) error {
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	output := &commandOutput{}
	stream, err := startAppProcess(bounded, bin, workspace, env, step, output)
	if err != nil {
		return err
	}
	defer stream.Close()
	for range stream.Events() {
	}
	code := stream.Wait(bounded)
	if bounded.Err() != nil {
		return bounded.Err()
	}
	if code != 0 {
		return fmt.Errorf("command exited with code %d: %s", code, output.String())
	}
	return nil
}

func waitAppReady(ctx, lifetime context.Context, p appProcess) error {
	ready, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(lifetime, cancel)
	defer stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		req, err := http.NewRequestWithContext(ready, http.MethodGet, p.address+p.service.HealthPath, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode >= 200 && res.StatusCode < 300 {
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("process exited before readiness: %s", p.output.String())
		case <-ready.Done():
			return fmt.Errorf("HTTP readiness: %w; %s", ready.Err(), p.output.String())
		case <-tick.C:
		}
	}
}
