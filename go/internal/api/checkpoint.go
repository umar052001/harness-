package api

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

const checkpointMaxEntries = 5000
const checkpointMaxBytes = 100 << 20

// Continue creates a new attempt from saved project files. Previous output stays
// unchanged, and agent homes and dependency caches are never inherited.
func (s *WorkspaceStore) Continue(ctx context.Context, parent string) (id, dir string, err error) {
	source, err := s.Dir(parent)
	if err != nil {
		return "", "", err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	id, dir, err = s.Create()
	if err != nil {
		return "", "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	entries := 0
	var bytes int64
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if entry.IsDir() && skipDirName(entry.Name()) {
			return fs.SkipDir
		}
		entries++
		if entries > checkpointMaxEntries {
			return fmt.Errorf("project checkpoint exceeds %d entries", checkpointMaxEntries)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("checkpoint cannot copy symlink %q; replace it with a project file", path)
		}
		target := filepath.Join(dir, path)
		if entry.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		n, err := copyCheckpointFile(root, path, target, checkpointMaxBytes-bytes)
		bytes += n
		return err
	})
	return id, dir, err
}

func copyCheckpointFile(root *os.Root, path, target string, remaining int64) (int64, error) {
	source, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("checkpoint requires a regular file: %s", path)
	}
	if info.Size() > remaining {
		return 0, fmt.Errorf("project checkpoint exceeds %d bytes", checkpointMaxBytes)
	}
	dest, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600|info.Mode().Perm()&0o100)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.CopyN(dest, source, info.Size())
	closeErr := dest.Close()
	if copyErr != nil {
		return n, copyErr
	}
	return n, closeErr
}
