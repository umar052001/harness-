package environment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProject = "0123456789ab"

func TestRefusesRootfulRuntimeBeforeCreatingCredentials(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services")
	m := New(dir)
	m.command = func(context.Context, ...string) ([]byte, error) {
		return []byte(`{"host":{"security":{"rootless":false}}}`), nil
	}
	if _, err := m.Start(t.Context(), testProject); err == nil {
		t.Fatal("rootful runtime accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("state created before rootless check: %v", err)
	}
}

func TestConnectionIsScopedAndNotReturnedInStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, testProject+".env"), []byte("POSTGRES_PASSWORD=test-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(dir)
	m.command = func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"State":{"Status":"running"},"Config":{"Labels":{"io.ai-os.project":"0123456789ab"}},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32100"}]}}}]`), nil
	}
	connection, err := m.Connection(t.Context(), testProject)
	if err != nil {
		t.Fatal(err)
	}
	if connection != "postgresql://app:test-secret@127.0.0.1:32100/app?sslmode=disable" {
		t.Fatalf("unexpected connection: %s", connection)
	}
	if _, err := m.Status(t.Context(), "../../outside"); err == nil {
		t.Fatal("invalid project accepted")
	}
	if _, err := m.Status(t.Context(), "ffffffffffff"); err != nil {
		t.Fatal(err)
	}
	m.command = func(context.Context, ...string) ([]byte, error) {
		return []byte(`[{"State":{"Status":"running"},"Config":{"Labels":{"io.ai-os.project":"someone-else"}}}]`), nil
	}
	if _, err := m.Connection(t.Context(), testProject); err == nil {
		t.Fatal("foreign container accepted")
	}
}

func TestStartKeepsCredentialsOutOfArgumentsAndLimitsResources(t *testing.T) {
	m := New(t.TempDir())
	var runArgs []string
	m.command = func(ctx context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "volume", "exec":
			return nil, nil
		case "info":
			return []byte(`{"host":{"security":{"rootless":true}}}`), nil
		case "ps":
			return nil, nil
		case "run":
			runArgs = append([]string(nil), args...)
			return []byte("container-id"), nil
		case "inspect":
			return []byte(`[{"State":{"Status":"running"},"Config":{"Labels":{"io.ai-os.project":"0123456789ab"}},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"32100"}]}}}]`), nil
		}
		t.Fatalf("unexpected command: %v", args)
		return nil, nil
	}
	if _, err := m.Start(t.Context(), testProject); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runArgs, " ")
	for _, required := range []string{"--env-file", "127.0.0.1::5432", "--memory 512m", "--pids-limit 128", "--cap-drop ALL", "no-new-privileges"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("missing %s: %s", required, joined)
		}
	}
	raw, err := os.ReadFile(filepath.Join(m.root, testProject+".env"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(strings.Split(string(raw), "POSTGRES_PASSWORD=")[1])
	if len(password) != 64 || strings.Contains(joined, password) {
		t.Fatal("password missing or exposed in arguments")
	}
	info, err := os.Stat(filepath.Join(m.root, testProject+".env"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode: %v", info.Mode())
	}
}
