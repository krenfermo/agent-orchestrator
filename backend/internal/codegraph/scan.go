package codegraph

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scan.go — which files AO will look at, and how it proves it may.
//
// Both indexers in this package share it. That is the point: the walk decides
// what a full pass admits and readCandidate decides what a diff-named path
// may be read as, and if those two ever disagreed, a path the walk refuses
// could still enter the graph through an incremental update that named it
// directly.

// scanner is the filesystem half of an indexer.
type scanner struct {
	extractors  extractorSet
	skipDirs    map[string]bool
	maxFileSize int64
}

// newScanner returns the default admission policy: the registered extractors,
// the standard skipped directories, and the standard file-size cap.
func newScanner() scanner {
	return scanner{
		extractors:  newExtractorSet(DefaultExtractors()),
		skipDirs:    nameSet(defaultSkipDirs),
		maxFileSize: defaultMaxFileSize,
	}
}

// indexable reports whether a path is one this scanner will read, and returns
// the extractor that claims it.
func (s scanner) indexable(rel string) (Extractor, bool) {
	extractor, ok := s.extractors.find(rel)
	if !ok {
		return nil, false
	}
	// Section 28: a file whose content is a secret by convention is never
	// opened. A configuration KEY can still enter the graph -- from the code
	// that reads it -- but its value cannot.
	if DeniedPath(rel) {
		return nil, false
	}
	return extractor, true
}

// readCandidate reads a project-relative file if it is one the indexer will
// consider: an existing regular file within the size cap. ok=false means "not
// indexable", which is a normal outcome, not an error.
func (s scanner) readCandidate(root, rel string) (data []byte, ok bool, err error) {
	abs, exists, err := s.resolve(root, rel)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("codegraph: stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() || info.Size() > s.maxFileSize {
		return nil, false, nil
	}
	data, err = os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("codegraph: read %s: %w", rel, err)
	}
	return data, true, nil
}

// resolve turns a project-relative path into an absolute one, refusing any
// path that does not genuinely live beneath the project root. A diff is data
// from outside the process; neither a "../../etc/passwd" entry nor a
// "linked/secret.go" entry that reaches another checkout through a symlinked
// directory inside this one may make the indexer read — or file under this
// project's graph — a file the caller did not ask about.
//
// Lexical cleaning alone cannot decide this: it sees no ".." in
// "linked/secret.go", and os.Lstat only ever inspects the final component. So
// the parent directory is symlink-resolved and proven to be inside the
// canonical root before anything is opened. The final component is left
// unresolved on purpose — readCandidate's Lstat then sees a symlink for what
// it is and declines it as a non-regular file.
//
// exists=false means the path (or a directory on the way to it) is simply not
// there, which is a normal outcome for a stale or partially-applied diff, not
// an error.
func (s scanner) resolve(root, rel string) (abs string, exists bool, err error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("%w: path %q escapes the project root", ErrProjectRoot, rel)
	}

	candidate := filepath.Join(root, clean)
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("codegraph: resolve %s: %w", rel, err)
	}
	if !containedIn(root, parent) {
		return "", false, fmt.Errorf("%w: path %q leaves the project root through a symlink", ErrProjectRoot, rel)
	}
	return filepath.Join(parent, filepath.Base(candidate)), true, nil
}

// containedIn reports whether path is root itself or sits beneath it. Both
// must already be absolute and symlink-resolved for the answer to mean
// anything.
func containedIn(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// walk collects every indexable project-relative path under root.
func (s scanner) walk(ctx context.Context, root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable directory is skipped rather than fatal: one
			// permission-denied subtree should not cost the whole index.
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && s.skipDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativize %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		if _, supported := s.extractors.find(rel); !supported {
			return nil
		}
		if DeniedPath(rel) {
			// Section 28: a file whose content is a secret by convention is
			// never opened. A configuration KEY can still enter the graph --
			// from the code that reads it -- but its value cannot.
			return nil
		}
		found = append(found, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codegraph: walk %s: %w", root, err)
	}
	sort.Strings(found)
	return found, nil
}
