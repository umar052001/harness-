package exec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RuntimePath resolves host-selected versions once. Mise shims need host configuration that
// sandboxed tools cannot read; actual tool binaries do not need that access.
func RuntimePath() string {
	path := os.Getenv("PATH")
	if !strings.Contains(filepath.ToSlash(path), "/mise/shims") {
		return path
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "mise", "env", "--json")
	cmd.WaitDelay = time.Second
	if raw, err := cmd.Output(); err == nil {
		var env map[string]string
		if json.Unmarshal(raw, &env) == nil && env["PATH"] != "" {
			path = env["PATH"]
		}
	}
	return withoutMiseShims(path)
}

func withoutMiseShims(path string) string {
	var dirs []string
	for _, dir := range filepath.SplitList(path) {
		if filepath.Base(dir) == "shims" && filepath.Base(filepath.Dir(dir)) == "mise" {
			continue
		}
		dirs = append(dirs, dir)
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

// RuntimeReadPaths grants installed toolchains without granting host configuration.
func RuntimeReadPaths() []string {
	// Chromium needs to list PCI devices even with GPU rendering disabled.
	candidates := []string{"/sys/bus/pci/devices"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "mise", "installs"),
			filepath.Join(home, ".nvm"),
			filepath.Join(home, ".volta"),
			filepath.Join(home, ".bun"),
		)
	}
	var dirs []string
	for _, d := range candidates {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			dirs = append(dirs, d)
		}
	}
	return dirs
}
