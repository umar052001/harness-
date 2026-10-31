//go:build environment_live

package environment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestLiveDatabaseRetainsDataAcrossRestart(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("rootless Podman is not installed")
	}
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(random[:])
	m := New(t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if err := m.requireRootless(ctx); err != nil {
		t.Fatal(err)
	}
	name := "ai-os-db-" + id
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := m.command(cleanup, "rm", "--force", name); err != nil {
			t.Logf("container cleanup: %v", err)
		}
		if _, err := m.command(cleanup, "volume", "rm", name); err != nil {
			t.Logf("volume cleanup: %v", err)
		}
	})
	db, err := m.Start(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if db.Status != "running" || db.Host != "127.0.0.1" || db.Port == "" {
		t.Fatalf("invalid service: %+v", db)
	}
	if _, err := m.Connection(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.command(ctx, "exec", name, "psql", "-U", "app", "-d", "app", "-v", "ON_ERROR_STOP=1", "-c", "CREATE TABLE checkpoint(value text); INSERT INTO checkpoint VALUES ('retained');"); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Connection(ctx, id); err == nil {
		t.Fatal("stopped database was offered to an agent")
	}
	if _, err := m.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	raw, err := m.command(ctx, "exec", name, "psql", "-U", "app", "-d", "app", "-Atc", "SELECT value FROM checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "retained" {
		t.Fatalf("data lost: %s", raw)
	}
}
