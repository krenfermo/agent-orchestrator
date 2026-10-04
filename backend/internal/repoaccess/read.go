package repoaccess

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// read.go — the only way an indexer reads a repository file.
//
// Contract (fail-closed; every refusal is a typed "skip", never a crash):
//
//   - the path must pass CheckPath: relative, no "..", not under an excluded
//     directory, not a secret -- all decided BEFORE anything is opened;
//   - no component may be a symbolic link. Not the file, not any parent
//     directory, not the first link of a chain, and not a dangling link. AO
//     does not follow repository symlinks even when they stay inside the
//     root: a link is a pointer the repository's author chose, and the
//     indexer's job is to describe the files the repository contains;
//   - the opened object must be a regular file within maxBytes;
//   - the open itself goes through os.Root, so even a component swapped for a
//     symlink between the Lstat checks and the open cannot escape the root
//     (os.Root refuses to resolve outside it). The residual race can only
//     redirect the read to another file INSIDE the root, which the indexer
//     could have read anyway.

// ReadConfined reads rel under root under the contract above. root must be an
// absolute directory; it may itself be reached through a symlink (a project
// registered at /tmp/x on macOS lives at /private/tmp/x) -- only components
// BELOW root are checked.
func ReadConfined(root, rel string, maxBytes int64) ([]byte, error) {
	f, clean, err := openConfined(root, rel, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("repoaccess: read %s: %w", clean, err)
	}
	if int64(len(data)) > maxBytes {
		// The file grew between fstat and read.
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, clean)
	}
	return data, nil
}

// StatConfined applies every check ReadConfined applies and returns the file's
// size without reading it. Useful for bounds checks before a read.
func StatConfined(root, rel string, maxBytes int64) (int64, error) {
	f, _, err := openConfined(root, rel, maxBytes)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func openConfined(root, rel string, maxBytes int64) (*os.File, string, error) {
	if !filepath.IsAbs(root) {
		return nil, "", fmt.Errorf("%w: root %q is not absolute", ErrEscapesRoot, root)
	}
	if err := CheckPath(rel); err != nil {
		return nil, "", err
	}
	clean, _ := cleanRel(rel)

	r, err := os.OpenRoot(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, clean, fmt.Errorf("%w: root %s", ErrNotExist, root)
		}
		return nil, clean, fmt.Errorf("repoaccess: open root %s: %w", root, err)
	}
	defer func() { _ = r.Close() }()

	// Lstat every prefix: a/, a/b/, a/b/file. A symlink anywhere refuses.
	segs := strings.Split(clean, "/")
	for i := range segs {
		prefix := strings.Join(segs[:i+1], "/")
		info, err := r.Lstat(filepath.FromSlash(prefix))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, clean, fmt.Errorf("%w: %s", ErrNotExist, clean)
			}
			return nil, clean, fmt.Errorf("repoaccess: lstat %s: %w", prefix, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, clean, fmt.Errorf("%w: %s", ErrSymlink, prefix)
		}
		last := i == len(segs)-1
		if !last && !info.IsDir() {
			return nil, clean, fmt.Errorf("%w: %s is not a directory", ErrNotRegular, prefix)
		}
		if last {
			if !info.Mode().IsRegular() {
				return nil, clean, fmt.Errorf("%w: %s", ErrNotRegular, clean)
			}
			if info.Size() > maxBytes {
				return nil, clean, fmt.Errorf("%w: %s", ErrTooLarge, clean)
			}
		}
	}

	f, err := r.Open(filepath.FromSlash(clean))
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil, clean, fmt.Errorf("%w: %s", ErrNotExist, clean)
		case strings.Contains(err.Error(), "escapes from parent"), strings.Contains(err.Error(), "path escapes"):
			return nil, clean, fmt.Errorf("%w: %s", ErrEscapesRoot, clean)
		}
		return nil, clean, fmt.Errorf("repoaccess: open %s: %w", clean, err)
	}
	// Re-check what was actually opened: the Lstat above and the open are two
	// system calls.
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, clean, fmt.Errorf("repoaccess: fstat %s: %w", clean, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, clean, fmt.Errorf("%w: %s", ErrNotRegular, clean)
	}
	if info.Size() > maxBytes {
		_ = f.Close()
		return nil, clean, fmt.Errorf("%w: %s", ErrTooLarge, clean)
	}
	return f, clean, nil
}
