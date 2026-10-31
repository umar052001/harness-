package environment

import (
	"net/url"
	"strings"
	"sync"
)

// commandOutput retains a bounded diagnostic tail, not an unbounded process log.
type commandOutput struct {
	mu   sync.Mutex
	tail []byte
}

func (o *commandOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	const limit = 8192
	if len(p) > limit {
		p = p[len(p)-limit:]
	}
	o.tail = append(o.tail, p...)
	if len(o.tail) > limit {
		o.tail = append([]byte(nil), o.tail[len(o.tail)-limit:]...)
	}
	return n, nil
}

func (o *commandOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.TrimSpace(string(o.tail))
}

func redactConnection(text, connection string) string {
	if connection == "" {
		return text
	}
	text = strings.ReplaceAll(text, connection, "[database connection redacted]")
	if u, err := url.Parse(connection); err == nil && u.User != nil {
		if password, ok := u.User.Password(); ok && password != "" {
			text = strings.ReplaceAll(text, password, "[redacted]")
		}
	}
	return text
}
