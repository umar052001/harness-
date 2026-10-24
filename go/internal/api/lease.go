package api

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockWorkspaceStore prevents two server processes from running independent
// concurrency guards against the same workspace store. The kernel releases it on exit.
func (s *Server) LockWorkspaceStore() (func(), error) {
	if s.store == nil {
		return nil, fmt.Errorf("workspace storage: %s", s.storeErr)
	}
	f, err := os.OpenFile(filepath.Join(s.store.Root(), ".server.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another ai-os server owns this workspace store: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
