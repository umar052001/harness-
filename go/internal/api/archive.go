package api

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Handing the work back is the point of a durable workspace.
//
// A run's files are visible in the dashboard one at a time, which is enough to
// read them and not enough to keep them: there is no way to take the work out
// of the tool. Two routes close that gap - a single file with `?download=1` on
// the raw handler, and this handler, which streams the whole project as a zip.

// archiveLimits bound what a single download may cost.
//
// A run that installed a dependency tree has a million files in it, and
// archiving that would hang the request and fill a disk for no benefit:
// `node_modules` is excluded anyway, and what is left is source a person can
// actually use. The caps are the backstop for a workspace that is large for
// other reasons, and reaching one is reported inside the archive rather than
// silently truncating it.
const (
	archiveMaxFiles     = 20_000
	archiveMaxBytes     = 256 << 20 // 256 MiB
	archiveMaxFileBytes = 8 << 20   // 8 MiB: past this it is not source
)

// archiveSkip are directory and file names never worth shipping.
//
// The dot-directories are the run's own machinery: the sandbox scratch space,
// the synthetic home holding the agent's config and caches. None of it is the
// work, and all of it is noise the user would have to delete on arrival.
var archiveSkip = map[string]bool{
	"node_modules":   true,
	".git":           true,
	".sandbox-tmp":   true,
	".home":          true,
	".cache":         true,
	".next":          true,
	".turbo":         true,
	"__pycache__":    true,
	".DS_Store":      true,
	".venv":          true,
	"venv":           true,
	"target":         true, // Rust build output: gigabytes, fully reproducible
	".gradle":        true,
	".pytest_cache":  true,
	".svelte-kit":    true,
	".astro":         true,
	".vercel":        true,
	".netlify":       true,
	"coverage":       true,
	"npm-debug.log":  true,
	"yarn-error.log": true,
}

// handleWorkspaceArchive streams a run's project as a zip.
//
// The archive is written straight to the response as it is built rather than
// assembled in memory or spooled to a temporary file, so a large download costs
// no memory and leaves nothing behind when the client disconnects halfway.
func (s *Server) handleWorkspaceArchive(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, s.storeErr, http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")

	// `root` scopes the download to part of the project, which is what makes
	// "give me the build output" and "give me the source" both one click.
	root := strings.Trim(r.URL.Query().Get("root"), "/")
	dir, err := s.store.Dir(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	scanRoot := dir
	if root != "" {
		// resolve, not filepath.Join: this rejects `..` and absolute paths, so
		// the parameter cannot reach outside the workspace.
		scanRoot, err = s.store.resolve(id, root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		info, statErr := os.Stat(scanRoot)
		if statErr != nil || !info.IsDir() {
			http.Error(w, "root is not a directory in this workspace", http.StatusNotFound)
			return
		}
	}

	name := archiveName(id, root)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	zw := zip.NewWriter(w)
	writeArchive(zw, scanRoot, dir)
	// Close flushes the central directory. If it fails the status line is long
	// gone, so there is nothing left to say but end the response: the client
	// sees a truncated archive and a closed connection.
	_ = zw.Close()
}

// writeArchive copies everything under root into the zip, and records what it
// had to leave out.
//
// `prefix` is the workspace directory: entries are named relative to it, so a
// download of `dist` arrives as `dist/...` rather than as a pile of loose files.
func writeArchive(zw *zip.Writer, root, prefix string) (added int, skipped []string) {
	confined, openErr := os.OpenRoot(prefix)
	if openErr != nil {
		return 0, []string{"workspace unavailable"}
	}
	defer confined.Close()
	var (
		files    int
		bytesOut int64
		capped   bool
	)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtree: record it and carry on rather than failing
			// the whole download over one permission problem.
			skipped = append(skipped, relOrBase(p, prefix)+" (unreadable)")
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()

		if d.IsDir() {
			if p != root && (archiveSkip[name] || skipDirName(name)) {
				return fs.SkipDir
			}
			return nil
		}
		// Symlinks are skipped rather than followed. Following one would let a
		// workspace hand out a file from outside itself - the same reason the
		// file handlers refuse to resolve through a link.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if archiveSkip[name] {
			return nil
		}
		if files >= archiveMaxFiles || bytesOut >= archiveMaxBytes {
			capped = true
			return fs.SkipAll
		}

		info, ierr := d.Info()
		if ierr != nil {
			skipped = append(skipped, relOrBase(p, prefix)+" (no metadata)")
			return nil
		}
		if info.Size() > archiveMaxFileBytes {
			skipped = append(skipped, fmt.Sprintf("%s (%.1f MiB)", relOrBase(p, prefix), float64(info.Size())/(1<<20)))
			return nil
		}

		if bytesOut+info.Size() > archiveMaxBytes {
			capped = true
			return fs.SkipAll
		}
		if err := addFile(zw, confined, relOrBase(p, prefix), info); err != nil {
			skipped = append(skipped, relOrBase(p, prefix)+" ("+err.Error()+")")
			return nil
		}
		files++
		bytesOut += info.Size()
		added++
		return nil
	})
	if capped {
		skipped = append(skipped, "the rest of the project")
	}
	if err != nil {
		skipped = append(skipped, err.Error())
	}

	// The omissions go inside the archive. A silently short download is the
	// kind of thing a person only discovers when their project will not build.
	if len(skipped) > 0 {
		note := strings.Join(append([]string{
			"This archive was written by ai-os and is incomplete.",
			"",
			fmt.Sprintf("%d file(s) included. %d omitted:", added, len(skipped)),
			"",
		}, bulletize(skipped)...), "\n")
		if w, err := zw.Create("INCOMPLETE-READ-ME.txt"); err == nil {
			_, _ = io.WriteString(w, note)
		}
	}
	return added, skipped
}

// addFile streams one file into the archive under a fixed timestamp.
//
// The timestamp is the workspace's, not the file's, and it is deliberate: zip
// stores second-resolution local time, so a per-file modtime would make the
// archive differ on every export and show up as noise in version control.
func addFile(zw *zip.Writer, root *os.Root, inArchive string, info fs.FileInfo) error {
	src, err := root.OpenFile(inArchive, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	actual, err := src.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}

	hdr := &zip.FileHeader{
		Name:     filepath.ToSlash(inArchive),
		Method:   zip.Deflate,
		Modified: info.ModTime().Truncate(time.Second),
	}
	dst, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	// Copy the listed size only: a file growing during export cannot bypass caps.
	_, err = io.CopyN(dst, src, info.Size())
	return err
}

// archiveName builds a download filename a person can find again later.
func archiveName(id, root string) string {
	if root == "" {
		return "ai-os-" + id + ".zip"
	}
	// A root of `dist` becomes `ai-os-<id>-dist.zip`, so two downloads from the
	// same run do not overwrite each other in ~/Downloads.
	return "ai-os-" + id + "-" + sanitizeForFilename(root) + ".zip"
}

// sanitizeForFilename keeps a root name safe to put in a header.
func sanitizeForFilename(s string) string {
	return safeFilename(s, '-')
}

// downloadFilename keeps a file's name safe to put in a header, dots included,
// so `App.tsx` downloads as `App.tsx` and not as `App-tsx`.
func downloadFilename(s string) string {
	return safeFilename(s, '_')
}

// safeFilename strips everything that is not plainly printable and cannot be
// misread as a path or as header syntax. Dots survive only where a file name
// legitimately has them.
func safeFilename(s string, filler rune) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune(filler)
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." || len(out) > 80 {
		return "file"
	}
	return out
}

// relOrBase names a path relative to the workspace, falling back to the base
// name when the path is somehow not under it.
func relOrBase(p, prefix string) string {
	rel, err := filepath.Rel(prefix, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(p)
	}
	return filepath.ToSlash(rel)
}

func bulletize(items []string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, "  - "+it)
	}
	return out
}
