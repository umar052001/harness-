//go:build environment_live

package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/umar052001/ai-os/go/internal/api"
	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
)

func TestAppProxyLifecycle(t *testing.T) {
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	root := t.TempDir()
	store, err := api.NewWorkspaceStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, dir, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"ai-os.app.json": `{"version":1,"services":[{"name":"api","command":["node","server.js"],"route":"/api"},{"name":"web","command":["node","web.js"],"route":"/"}],"checks":[{"command":["node","-e","fetch(process.env.APP_URL+'/api/value').then(async r=>{if(!r.ok || !['empty','saved'].includes(await r.text()))process.exit(1)}).catch(()=>process.exit(1))"]}]}`,
		"web.js":         `require('http').createServer((req,res)=>res.end('web')).listen(process.env.PORT,process.env.HOST);`,
		"server.js":      `const http=require('http'),fs=require('fs'); http.createServer((req,res)=>{if(req.url==='/api/value'){if(req.method==='POST')fs.writeFileSync('value.txt','saved'); res.end(fs.existsSync('value.txt')?fs.readFileSync('value.txt'):'empty');}else res.end('running');}).listen(process.env.PORT,process.env.HOST);`,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server := api.New(&accountingRunner{reg: harness.Load()}, api.Options{WorkspacesDir: root, SandboxBinary: bin})
	s := httptest.NewServer(server.Handler())
	defer s.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	request := func(method, path, host, origin string, want int) string {
		t.Helper()
		r, err := http.NewRequest(method, s.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			r.Host = host
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, body)
		}
		return string(body)
	}
	endpoint := "/api/workspace/" + id + "/app"
	request("POST", endpoint, "", "", 200)
	host := id + ".localhost"
	if got := request("POST", "/api/value", host, "http://"+host, 200); got != "saved" {
		t.Fatalf("write: %s", got)
	}
	request("POST", "/api/value", host, "http://other.localhost", 403)
	if got := request("GET", "/api/health", host, "", 200); got != "running" {
		t.Fatalf("app origin reached the control plane: %s", got)
	}
	request("DELETE", endpoint, "", "", 200)
	if got := request("GET", "/api/value", host, "", 503); !strings.Contains(got, "stopped") {
		t.Fatal(got)
	}
	request("POST", endpoint, "", "", 200)
	if got := request("GET", "/api/value", host, "", 200); got != "saved" {
		t.Fatalf("persistence: %s", got)
	}
	shutdown, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	request("GET", "/api/value", host, "", 503)
}
