package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// importOpenAI uses the supported V2 credential API, not the obsolete auth.json
// migration file. The subprocess output is captured in memory and never logged.
func (b *Build) importOpenAI(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "opencode", "api", "get", "/api/credential")
	raw, err := cmd.Output()
	if err != nil {
		return errors.New("cannot obtain existing OpenCode credential through its API")
	}
	var result struct {
		Data []struct {
			IntegrationID string          `json:"integrationID"`
			Active        bool            `json:"active"`
			Value         json.RawMessage `json:"value"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return errors.New("invalid OpenCode credential response")
	}
	for _, entry := range result.Data {
		if entry.IntegrationID != "openai" || !entry.Active {
			continue
		}
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(entry.Value, &kind) != nil || kind.Type != "oauth" {
			continue
		}
		path := filepath.Join(b.home, "openai-import.json")
		if err := b.writePrivate(path, map[string]any{"integrationID": "openai", "label": "harness isolated build", "value": entry.Value}); err != nil {
			return err
		}
		defer os.Remove(path)
		_, err := b.command(ctx, []string{b.binary, "-credential-worker", path})
		return err
	}
	return errors.New("no active OpenAI OAuth credential; connect through OpenCode /connect")
}

// CredentialWorker runs a short-lived private OpenCode server inside sandboxd,
// stores only the selected OAuth credential using the public API, then joins it.
// The resulting private OpenCode DB is not shared with the operator's service.
func CredentialWorker(ctx context.Context, path string) error {
	var body []byte
	if path != ":purge" {
		var err error
		body, err = os.ReadFile(path)
		if err != nil {
			return errors.New("credential handoff unavailable")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(child, "opencode", "serve", "--hostname", "127.0.0.1", "--port", fmt.Sprint(port))
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	password := hex.EncodeToString(secret[:])
	// Supported by OpenCode 2.0.32 ServerProcess/Env.password. Keep the private
	// server authenticated; the generated password never reaches logs or argv.
	cmd.Env = append(os.Environ(), "OPENCODE_PASSWORD="+password)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { cancel(); cmd.Wait() }()
	url := fmt.Sprintf("http://127.0.0.1:%d/api/credential", port)
	client := &http.Client{Timeout: time.Second}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		method := http.MethodPost
		if path == ":purge" {
			method = http.MethodGet
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth("opencode", password)
		res, err := client.Do(req)
		// V2 briefly accepts authenticated HTTP before its storage services are
		// ready. A 503 has not admitted the operation; retry it within the worker
		// deadline, without retrying model inference or other response statuses.
		if err == nil && res.StatusCode == http.StatusServiceUnavailable {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-tick.C:
				continue
			}
		}
		if err == nil {
			if path == ":purge" {
				defer res.Body.Close()
				if res.StatusCode != 200 {
					return fmt.Errorf("private credential listing returned HTTP %d", res.StatusCode)
				}
				var entries struct {
					Data []struct {
						ID            string `json:"id"`
						IntegrationID string `json:"integrationID"`
					} `json:"data"`
				}
				if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&entries); err != nil {
					return errors.New("invalid private credential response")
				}
				for _, entry := range entries.Data {
					if entry.IntegrationID == "openai" {
						r, e := http.NewRequestWithContext(ctx, http.MethodDelete, url+"/"+entry.ID, nil)
						if e != nil {
							return e
						}
						r.SetBasicAuth("opencode", password)
						deleted, e := client.Do(r)
						if e != nil {
							return errors.New("private credential deletion failed")
						}
						deleted.Body.Close()
						if deleted.StatusCode != 204 {
							return fmt.Errorf("credential deletion HTTP %d", deleted.StatusCode)
						}
					}
				}
				fmt.Println("private OpenAI credentials removed")
				return nil
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				return fmt.Errorf("private credential import returned HTTP %d", res.StatusCode)
			}
			fmt.Println("credential imported through private OpenCode API")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}
