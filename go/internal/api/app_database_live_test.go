//go:build environment_live

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/umar052001/ai-os/go/internal/api"
	sbx "github.com/umar052001/ai-os/go/internal/exec"
	"github.com/umar052001/ai-os/go/internal/harness"
)

func TestAppDeclaredPostgres(t *testing.T) {
	bin := sbx.DefaultBinary()
	if _, err := os.Stat(bin); err != nil {
		t.Skip("sandboxd is not built")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("Podman is not installed")
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
		"ai-os.app.json": `{"version":1,"database":"postgres","build":[{"command":["python3","migrate.py"]}],"services":[{"name":"web","command":["node","server.js"],"route":"/","healthPath":"/health"}],"checks":[{"command":["python3","check.py"]}]}`,
		"migrate.py": `import subprocess,os
subprocess.run(['psql',os.environ['DATABASE_URL'],'-v','ON_ERROR_STOP=1','-c',"CREATE TABLE IF NOT EXISTS app_check(value text); INSERT INTO app_check SELECT 'retained' WHERE NOT EXISTS (SELECT 1 FROM app_check);"],check=True)
`,
		"server.js": `const http=require('http'),cp=require('child_process');http.createServer((req,res)=>{const p=cp.spawnSync('psql',[process.env.DATABASE_URL,'-Atc','SELECT value FROM app_check']);res.writeHead(p.status===0?200:500);res.end(p.status===0?p.stdout:'database unavailable');}).listen(process.env.PORT,process.env.HOST);`,
		"check.py": `import urllib.request,os
assert urllib.request.urlopen(os.environ['APP_URL']).read().strip()==b'retained'
`,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, args := range [][]string{{"rm", "--force", "ai-os-db-" + id}, {"volume", "rm", "ai-os-db-" + id}} {
			if out, err := exec.CommandContext(ctx, "podman", args...).CombinedOutput(); err != nil {
				t.Errorf("cleanup: %v: %s", err, out)
			}
		}
	})
	// A fresh control plane must reattach the same database and rerun idempotent
	// migrations/checks, rather than equating saved code with a running app.
	for range 2 {
		server := api.New(&accountingRunner{reg: harness.Load()}, api.Options{WorkspacesDir: root, SandboxBinary: bin})
		s := httptest.NewServer(server.Handler())
		func() {
			defer s.Close()
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := server.Shutdown(ctx); err != nil {
					t.Error(err)
				}
			}()
			res, err := http.Post(s.URL+"/api/workspace/"+id+"/app", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			raw, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Status       string
				Verification string
				ChecksPassed int
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatalf("response: %s: %v", raw, err)
			}
			if res.StatusCode != 200 || status.Status != "ready" || status.Verification != "passed" || status.ChecksPassed != 1 {
				t.Fatalf("database-backed app did not verify: %s", raw)
			}
		}()
	}
}
