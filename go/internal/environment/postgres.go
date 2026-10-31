// Package environment owns project services outside agent processes.
package environment

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const PostgresImage = "docker.io/library/postgres:17-alpine"
const projectLabel = "io.ai-os.project"

var projectID = regexp.MustCompile(`^[a-f0-9]{12}$`)

type Database struct {
	Status string `json:"status"`
	Host   string `json:"host,omitempty"`
	Port   string `json:"port,omitempty"`
	Name   string `json:"name,omitempty"`
	User   string `json:"user,omitempty"`
}

type Manager struct {
	root    string
	mu      sync.Mutex
	command func(context.Context, ...string) ([]byte, error)
}

func New(root string) *Manager {
	return &Manager{root: root, command: func(ctx context.Context, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}}
}

func (m *Manager) requireRootless(ctx context.Context) error {
	raw, err := m.command(ctx, "info", "--format", "json")
	if err != nil {
		return err
	}
	var info struct {
		Host struct {
			Security struct {
				Rootless bool `json:"rootless"`
			} `json:"security"`
		} `json:"host"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if !info.Host.Security.Rootless {
		return errors.New("project databases require rootless Podman")
	}
	return nil
}

func (m *Manager) Status(ctx context.Context, id string) (Database, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status(ctx, id)
}

func (m *Manager) status(ctx context.Context, id string) (Database, error) {
	if !projectID.MatchString(id) {
		return Database{}, errors.New("invalid project id")
	}
	if _, err := os.Stat(filepath.Join(m.root, id+".env")); os.IsNotExist(err) {
		return Database{Status: "not_created"}, nil
	} else if err != nil {
		return Database{}, err
	}
	raw, err := m.command(ctx, "inspect", "--type", "container", "ai-os-db-"+id)
	if err != nil {
		return Database{}, err
	}
	var rows []struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Ports map[string][]struct {
				HostIP   string `json:"HostIp"`
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return Database{}, err
	}
	if len(rows) != 1 || rows[0].Config.Labels[projectLabel] != id {
		return Database{}, errors.New("database container ownership does not match project")
	}
	db := Database{Status: rows[0].State.Status, Name: "app", User: "app"}
	ports := rows[0].NetworkSettings.Ports["5432/tcp"]
	if len(ports) == 1 && ports[0].HostIP == "127.0.0.1" {
		db.Host, db.Port = ports[0].HostIP, ports[0].HostPort
	}
	if db.Status == "running" && db.Port == "" {
		return Database{}, errors.New("database has no isolated loopback port")
	}
	return db, nil
}

func (m *Manager) Start(ctx context.Context, id string) (Database, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !projectID.MatchString(id) {
		return Database{}, errors.New("invalid project id")
	}
	if err := m.requireRootless(ctx); err != nil {
		return Database{}, err
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return Database{}, err
	}
	path := filepath.Join(m.root, id+".env")
	name := "ai-os-db-" + id
	volume, err := m.command(ctx, "volume", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return Database{}, err
	}
	if existing := strings.TrimSpace(string(volume)); existing != "" {
		if existing != name {
			return Database{}, errors.New("unexpected database volume listing")
		}
		raw, err := m.command(ctx, "volume", "inspect", name)
		if err != nil {
			return Database{}, err
		}
		var volumes []struct {
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal(raw, &volumes) != nil || len(volumes) != 1 || volumes[0].Labels[projectLabel] != id {
			return Database{}, errors.New("database volume ownership does not match project")
		}
		if _, err := os.Stat(path); err != nil {
			return Database{}, errors.New("existing database volume has no saved credentials; restore its credential file")
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			f.Close()
			os.Remove(path)
			return Database{}, err
		}
		_, writeErr := fmt.Fprintf(f, "POSTGRES_USER=app\nPOSTGRES_DB=app\nPOSTGRES_PASSWORD=%s\n", hex.EncodeToString(secret[:]))
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(path)
			return Database{}, errors.Join(writeErr, closeErr)
		}
	} else if !os.IsExist(err) {
		return Database{}, err
	}
	if strings.TrimSpace(string(volume)) == "" {
		if _, err := m.command(ctx, "volume", "create", "--label", projectLabel+"="+id, name); err != nil {
			return Database{}, err
		}
	}
	// List by exact name rather than interpreting every inspect error as absence.
	raw, err := m.command(ctx, "ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return Database{}, err
	}
	if strings.TrimSpace(string(raw)) == name {
		if _, err := m.status(ctx, id); err != nil {
			return Database{}, err
		}
		if _, err := m.command(ctx, "start", name); err != nil {
			return Database{}, err
		}
	} else if strings.TrimSpace(string(raw)) == "" {
		_, err := m.command(ctx, "run", "--detach", "--name", name, "--label", projectLabel+"="+id,
			"--env-file", path, "--publish", "127.0.0.1::5432", "--volume", name+":/var/lib/postgresql/data",
			"--memory", "512m", "--cpus", "1", "--pids-limit", "128", "--cap-drop", "ALL",
			"--security-opt", "no-new-privileges", "--user", "postgres",
			"--health-cmd", "pg_isready -U app -d app", "--health-interval", "5s",
			PostgresImage)
		if err != nil {
			return Database{}, err
		}
	} else {
		return Database{}, errors.New("unexpected database container listing")
	}
	ready, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if _, err := m.command(ready, "exec", name, "pg_isready", "-U", "app", "-d", "app"); err == nil {
			break
		}
		select {
		case <-ready.Done():
			return Database{}, fmt.Errorf("database did not become ready: %w", ready.Err())
		case <-tick.C:
		}
	}
	return m.status(ctx, id)
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.requireRootless(ctx); err != nil {
		return err
	}
	if _, err := m.status(ctx, id); err != nil {
		return err
	}
	_, err := m.command(ctx, "stop", "--time", "10", "ai-os-db-"+id)
	return err
}

// Connection is passed only to the owning project's agent, never to browser JSON.
func (m *Manager) Connection(ctx context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	db, err := m.status(ctx, id)
	if err != nil {
		return "", err
	}
	if db.Status == "not_created" {
		return "", nil
	}
	if db.Status != "running" {
		return "", errors.New("project database is stopped; start it before continuing")
	}
	raw, err := os.ReadFile(filepath.Join(m.root, id+".env"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if password, ok := strings.CutPrefix(line, "POSTGRES_PASSWORD="); ok && password != "" {
			u := url.URL{Scheme: "postgresql", User: url.UserPassword(db.User, password), Host: net.JoinHostPort(db.Host, db.Port), Path: "/" + db.Name, RawQuery: "sslmode=disable"}
			return u.String(), nil
		}
	}
	return "", errors.New("project database credentials are missing")
}
