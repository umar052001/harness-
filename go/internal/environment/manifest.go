package environment

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

type AppStep struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd,omitempty"`
}

type AppService struct {
	AppStep
	Name       string `json:"name"`
	Route      string `json:"route"`
	HealthPath string `json:"healthPath,omitempty"`
}

// AppManifest describes executable project requirements, not a deployment claim.
// Commands run in the sandbox; the manifest cannot grant host paths or secrets.
type AppManifest struct {
	Version  int          `json:"version,omitempty"`
	Entry    string       `json:"entry,omitempty"` // Compatibility with the first Node-only contract.
	Database string       `json:"database,omitempty"`
	Build    []AppStep    `json:"build,omitempty"`
	Services []AppService `json:"services,omitempty"`
	Checks   []AppStep    `json:"checks,omitempty"`
}

var serviceName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

func LoadApp(workspace string) (AppManifest, error) {
	var m AppManifest
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return m, err
	}
	defer root.Close()
	f, err := root.OpenFile("ai-os.app.json", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return m, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16384 {
		return m, errors.New("app manifest must be a regular file under 16 KiB")
	}
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, errors.New("unexpected trailing app manifest data")
	}
	if m.Entry != "" {
		if len(m.Services) != 0 || len(m.Build) != 0 || len(m.Checks) != 0 || m.Version != 0 {
			return m, errors.New("legacy entry cannot be mixed with versioned services")
		}
		if !fs.ValidPath(m.Entry) || filepath.Ext(m.Entry) != ".js" {
			return m, errors.New("legacy entry must be a relative .js file")
		}
		info, err := root.Stat(m.Entry)
		if err != nil {
			return m, err
		}
		if !info.Mode().IsRegular() {
			return m, errors.New("app entry must be a regular file")
		}
		m.Services = []AppService{{Name: "web", Route: "/", HealthPath: "/", AppStep: AppStep{Cwd: filepath.Dir(m.Entry), Command: []string{"node", filepath.Base(m.Entry)}}}}
	} else if m.Version != 1 {
		return m, errors.New("app manifest version must be 1")
	}
	if m.Database != "" && m.Database != "postgres" {
		return m, errors.New("supported managed database: postgres")
	}
	if len(m.Services) == 0 || len(m.Services) > 4 || len(m.Build) > 8 || len(m.Checks) > 8 {
		return m, errors.New("app requires 1–4 services and at most 8 build/check steps")
	}
	validateStep := func(step AppStep) error {
		if len(step.Command) == 0 || len(step.Command) > 64 || step.Command[0] == "" {
			return errors.New("command must be a nonempty argument array (maximum 64 arguments)")
		}
		for _, arg := range step.Command {
			if strings.ContainsRune(arg, 0) {
				return errors.New("command contains a NUL byte")
			}
		}
		cwd := step.Cwd
		if cwd == "" {
			cwd = "."
		}
		if !fs.ValidPath(cwd) {
			return errors.New("cwd must stay inside the workspace")
		}
		info, err := root.Stat(cwd)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("cwd must be a directory")
		}
		return nil
	}
	names, routes := map[string]bool{}, map[string]bool{}
	for i := range m.Services {
		s := &m.Services[i]
		if !serviceName.MatchString(s.Name) || names[s.Name] || s.Name == "app" || s.Name == "database" {
			return m, errors.New("service names must be unique lowercase identifiers; app and database are reserved")
		}
		names[s.Name] = true
		if !strings.HasPrefix(s.Route, "/") || path.Clean(s.Route) != s.Route || strings.ContainsAny(s.Route, "?#\\") || routes[s.Route] {
			return m, errors.New("service routes must be unique, clean absolute paths")
		}
		routes[s.Route] = true
		if s.HealthPath == "" {
			s.HealthPath = "/"
		}
		u, err := url.Parse(s.HealthPath)
		if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(s.HealthPath, "/") || strings.HasPrefix(s.HealthPath, "//") || u.Fragment != "" {
			return m, errors.New("healthPath must be a local HTTP path")
		}
		if err := validateStep(s.AppStep); err != nil {
			return m, fmt.Errorf("service %s: %w", s.Name, err)
		}
	}
	if !routes["/"] {
		return m, errors.New("one service must own the root route /")
	}
	for _, steps := range [][]AppStep{m.Build, m.Checks} {
		for _, step := range steps {
			if err := validateStep(step); err != nil {
				return m, err
			}
		}
	}
	return m, nil
}
