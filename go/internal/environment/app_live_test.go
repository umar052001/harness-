//go:build environment_live

package environment

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sbx "github.com/umar052001/ai-os/go/internal/exec"
)

func appFixture(t *testing.T, m AppManifest, files map[string]string) (string, string) {
	t.Helper()
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	dir := t.TempDir()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	files["ai-os.app.json"] = string(raw)
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, bin
}

func TestLiveMultiServiceVerification(t *testing.T) {
	m := AppManifest{Version: 1, AI: "ollama", Build: []AppStep{{Command: []string{"python3", "-c", "open('built','w').write('ok')"}}}, Services: []AppService{
		{Name: "api", Route: "/api", HealthPath: "/health", AppStep: AppStep{Command: []string{"python3", "api.py"}}},
		{Name: "web", Route: "/", AppStep: AppStep{Command: []string{"node", "web.js"}}},
	}, Checks: []AppStep{{Command: []string{"python3", "check.py"}}}}
	dir, bin := appFixture(t, m, map[string]string{
		"api.py": `import http.server,os,sqlite3,json
db=sqlite3.connect('data.db')
db.execute('CREATE TABLE IF NOT EXISTS items(id INTEGER PRIMARY KEY,value TEXT)')
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200);self.end_headers();self.wfile.write(json.dumps(db.execute('SELECT value FROM items ORDER BY id').fetchall()).encode())
 def do_POST(self):
  value=self.rfile.read(int(self.headers['Content-Length'])).decode()
  db.execute('INSERT OR REPLACE INTO items VALUES(1,?)',(value,));db.commit()
  self.send_response(201);self.end_headers()
http.server.HTTPServer((os.environ['HOST'],int(os.environ['PORT'])),Handler).serve_forever()
`,
		"web.js": `const http=require('http');http.createServer((req,res)=>res.end('web')).listen(process.env.PORT,process.env.HOST);`,
		"check.py": `import urllib.request,os,json
assert open('built').read()=='ok'
assert os.environ['OLLAMA_BASE_URL']=='http://127.0.0.1:11434'
assert os.environ['OLLAMA_MODEL']=='gemma4:cloud'
base=os.environ['APP_URL']
assert urllib.request.urlopen(base).read()==b'web'
if os.path.exists('checked'):
 assert json.load(urllib.request.urlopen(base+'/api/items'))==[['retained']]
urllib.request.urlopen(urllib.request.Request(base+'/api/items',data=b'retained'))
assert json.load(urllib.request.urlopen(base+'/api/items'))==[['retained']]
open('checked','w').write('yes')
`,
	})
	var apps Apps
	defer apps.Stop("")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for range 2 {
		address, err := apps.Start(ctx, t.Context(), bin, "project", dir, "")
		if err != nil {
			t.Fatal(err)
		}
		status := apps.Status("project")
		if status.Status != "ready" || status.Verification != "passed" || status.ChecksPassed != 1 {
			t.Fatalf("status: %+v", status)
		}
		if apps.Address("project", "/api/items") == address || apps.Address("project", "/apiculture") != address {
			t.Fatal("route boundary or selection incorrect")
		}
		apps.Stop("project")
		if apps.Status("project").Active {
			t.Fatal("processes survived stop")
		}
		// The next start exercises compatibility with generated root names;
		// its unchanged checks must still reach the API through APP_URL.
		m.Services[1].Name = "app"
		raw, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dir, "ai-os.app.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A failed functional check must tear down every service and never publish readiness.
	m.Checks = []AppStep{{Command: []string{"python3", "-c", "raise SystemExit(9)"}}}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "ai-os.app.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := apps.Start(ctx, t.Context(), bin, "project", dir, ""); err == nil {
		t.Fatal("failing check passed")
	}
	status := apps.Status("project")
	if status.Status != "failed" || status.Verification != "failed" || status.Active || apps.Address("project") != "" {
		t.Fatalf("false readiness: %+v", status)
	}
	for _, p := range apps.processes {
		u, _ := url.Parse(p.address)
		c, err := net.DialTimeout("tcp", u.Host, time.Second)
		if err == nil {
			c.Close()
			t.Fatal("failed app left a listener alive")
		}
	}
}

func TestLiveGoBuildAndReadiness(t *testing.T) {
	m := AppManifest{Version: 1, Build: []AppStep{{Command: []string{"go", "build", "-o", "server", "server.go"}}}, Services: []AppService{{Name: "web", Route: "/", HealthPath: "/health", AppStep: AppStep{Command: []string{"./server"}}}}}
	dir, bin := appFixture(t, m, map[string]string{"server.go": `package main
import("net/http";"os")
func main(){http.HandleFunc("/",func(w http.ResponseWriter,r *http.Request){w.Write([]byte("go-ready"))});if err:=http.ListenAndServe(os.Getenv("HOST")+":"+os.Getenv("PORT"),nil);err!=nil{panic(err)}}`})
	var apps Apps
	defer apps.Stop("")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	address, err := apps.Start(ctx, t.Context(), bin, "go-project", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "go-ready" {
		t.Fatalf("unexpected response: %s", raw)
	}
	if apps.Status("go-project").Verification != "not_run" {
		t.Fatal("readiness was misreported as functional verification")
	}
}

func TestLiveStartupFailuresAreBounded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		build  []AppStep
		script string
	}{
		{"build failure", []AppStep{{Command: []string{"python3", "-c", "import sys; print('build diagnostic',file=sys.stderr);sys.exit(2)"}}}, `require('http').createServer((q,s)=>s.end('ok')).listen(process.env.PORT,process.env.HOST)`},
		{"unhealthy", nil, `require('http').createServer((q,s)=>{s.writeHead(503);s.end('not ready')}).listen(process.env.PORT,process.env.HOST)`},
		{"build timeout", []AppStep{{Command: []string{"python3", "-c", "import time;time.sleep(60)"}}}, `process.exit(1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := AppManifest{Version: 1, Build: tc.build, Services: []AppService{{Name: "web", Route: "/", AppStep: AppStep{Command: []string{"node", "server.js"}}}}}
			dir, bin := appFixture(t, m, map[string]string{"server.js": tc.script})
			var apps Apps
			defer apps.Stop("")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if _, err := apps.Start(ctx, t.Context(), bin, "test", dir, ""); err == nil {
				t.Fatal("startup failure accepted")
			}
			status := apps.Status("test")
			if status.Status != "failed" || status.Active || status.Verification == "passed" {
				t.Fatalf("bad outcome: %+v", status)
			}
			if tc.name == "build failure" && !strings.Contains(status.Error, "build diagnostic") {
				t.Fatalf("diagnostics lost: %+v", status)
			}
		})
	}
}

func TestLiveServiceExitStopsPeers(t *testing.T) {
	m := AppManifest{Version: 1, Services: []AppService{
		{Name: "web", Route: "/", AppStep: AppStep{Command: []string{"node", "server.js"}}},
		{Name: "api", Route: "/api", AppStep: AppStep{Command: []string{"node", "server.js"}}},
	}}
	dir, bin := appFixture(t, m, map[string]string{"server.js": `require('http').createServer((q,s)=>{s.end('ok',()=>{if(q.url==='/exit')process.exit(1)})}).listen(process.env.PORT,process.env.HOST)`})
	var apps Apps
	defer apps.Stop("")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	address, err := apps.Start(ctx, t.Context(), bin, "test", dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := http.Get(address + "/exit"); err == nil {
		res.Body.Close()
	}
	for _, p := range apps.processes {
		select {
		case <-p.done:
		case <-ctx.Done():
			t.Fatal("service peer survived exit")
		}
	}
	if status := apps.Status("test"); status.Status != "failed" || status.Active {
		t.Fatalf("bad exit state: %+v", status)
	}
}

func TestLiveRuntimeDNS(t *testing.T) {
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	err := runAppStep(ctx, t.Context(), bin, t.TempDir(), []string{"PATH=/usr/bin:/bin"}, AppStep{Command: []string{"node", "-e", `require('dns').lookup('registry.npmjs.org',(e,a)=>{if(e)throw e;if(!a)process.exit(1)})`}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLiveManagedToolchainInAppBuild(t *testing.T) {
	home := t.TempDir()
	tools := filepath.Join(home, ".local", "share", "mise", "installs", "fixture", "bin")
	if err := os.MkdirAll(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "harness-test-tool"), []byte("#!/bin/sh\nprintf managed > built\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	m := AppManifest{Version: 1, Build: []AppStep{{Command: []string{"harness-test-tool"}}}, Services: []AppService{{Name: "web", Route: "/", AppStep: AppStep{Command: []string{"node", "server.js"}}}}}
	dir, bin := appFixture(t, m, map[string]string{"server.js": `require('http').createServer((q,s)=>s.end('ok')).listen(process.env.PORT,process.env.HOST)`})
	var apps Apps
	defer apps.Stop("")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := apps.Start(ctx, t.Context(), bin, "managed-tools", dir, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "built"))
	if err != nil || string(data) != "managed" {
		t.Fatalf("managed build output=%q err=%v", data, err)
	}
}
