package environment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppEntryValidatesLaunchManifest(t *testing.T) {
	for _, tc := range []struct {
		name, manifest string
		valid          bool
	}{
		{"node", `{"entry":"server.js"}`, true},
		{"text AI", `{"entry":"server.js","ai":"ollama"}`, true},
		{"unsupported AI", `{"entry":"server.js","ai":"image"}`, false},
		{"traversal", `{"entry":"../server.js"}`, false},
		{"absolute", `{"entry":"/tmp/server.js"}`, false},
		{"shell", `{"entry":"server.sh"}`, false},
		{"extra permissions", `{"entry":"server.js","readPaths":["/home"]}`, false},
		{"trailing", `{"entry":"server.js"}{}`, false},
		{"missing", `{"entry":"missing.js"}`, false},
		{"versioned", `{"version":1,"services":[{"name":"web","route":"/","command":["python3","server.py"]}]}`, true},
		{"root named app", `{"version":1,"services":[{"name":"app","route":"/","command":["node","server.js"]}]}`, true},
		{"nonroot named app", `{"version":1,"services":[{"name":"web","route":"/","command":["node","server.js"]},{"name":"app","route":"/api","command":["node","server.js"]}]}`, false},
		{"reserved database", `{"version":1,"services":[{"name":"database","route":"/","command":["node","server.js"]}]}`, false},
		{"duplicate name", `{"version":1,"services":[{"name":"app","route":"/","command":["node","server.js"]},{"name":"app","route":"/api","command":["node","server.js"]}]}`, false},
		{"uppercase name", `{"version":1,"services":[{"name":"App","route":"/","command":["node","server.js"]}]}`, false},
		{"duplicate route", `{"version":1,"services":[{"name":"web","route":"/","command":["node","server.js"]},{"name":"api","route":"/","command":["node","server.js"]}]}`, false},
		{"missing root", `{"version":1,"services":[{"name":"api","route":"/api","command":["node","server.js"]}]}`, false},
		{"remote health", `{"version":1,"services":[{"name":"web","route":"/","command":["node","server.js"],"healthPath":"http://example.com/"}]}`, false},
		{"cwd escape", `{"version":1,"services":[{"name":"web","route":"/","command":["node","server.js"],"cwd":".."}]}`, false},
		{"unknown database", `{"version":1,"database":"anything","services":[{"name":"web","route":"/","command":["node","server.js"]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "server.js"), []byte("// test"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "ai-os.app.json"), []byte(tc.manifest), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadApp(dir)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
