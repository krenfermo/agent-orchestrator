package codegraph

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/repoaccess"
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
	// Frente 3 / 3B: the shared repository-access policy decides, from the
	// path alone and before anything is opened, whether the file is a secret
	// or lives under a directory AO never indexes. A configuration KEY can
	// still enter the graph -- from the code that reads it -- but its value
	// cannot.
	if repoaccess.CheckPath(rel) != nil {
		return nil, false
	}
	for _, seg := range strings.Split(path.Dir(rel), "/") {
		if s.skipDirs[seg] {
			return nil, false
		}
	}
	return extractor, true
}

// readCandidate reads a project-relative file if it is one the indexer will
// consider: an existing regular file within the size cap, reached without
// traversing a symlink. ok=false means "not indexable", which is a normal
// outcome, not an error.
//
// A path that escapes the root -- lexically, or through a symlink at any
// component -- is an ERROR (ErrProjectRoot), not a skip: such a path can only
// reach here from a diff, which is data from outside the process, and a diff
// naming one is refused loudly rather than half-applied. The full walk never
// hands one over (walk pre-filters symlinks), so a tracked symlink costs the
// full build nothing.
func (s scanner) readCandidate(root, rel string) (data []byte, ok bool, err error) {
	data, err = repoaccess.ReadConfined(root, rel, s.maxFileSize)
	switch {
	case err == nil:
		return data, true, nil
	case errors.Is(err, repoaccess.ErrEscapesRoot), errors.Is(err, repoaccess.ErrSymlink):
		return nil, false, fmt.Errorf("%w: %w", ErrProjectRoot, err)
	case repoaccess.IsRefusal(err):
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("codegraph: read %s: %w", rel, err)
	}
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
//
// Frente 3 / 3B: the candidate set is repoaccess.ListEligible -- in a git
// repository the TRACKED files, so an agent's linked worktree under
// `.claude/worktrees/`, ignored build output or a nested checkout can never be
// indexed as this project's code (MEDUSA served 24.7% of its symbols from one
// before this). Each candidate is then proven readable under the confined-read
// contract (no symlink at any component, regular file, within the size cap)
// without being read, so the build loop only ever reads admitted paths.
func (s scanner) walk(ctx context.Context, root string) ([]string, error) {
	listing, err := repoaccess.ListEligible(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("codegraph: list %s: %w", root, err)
	}
	found := make([]string, 0, len(listing.Paths))
	for _, rel := range listing.Paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := s.indexable(rel); !ok {
			continue
		}
		if _, err := repoaccess.StatConfined(root, rel, s.maxFileSize); err != nil {
			if repoaccess.IsRefusal(err) {
				continue
			}
			return nil, fmt.Errorf("codegraph: stat %s: %w", rel, err)
		}
		found = append(found, rel)
	}
	sort.Strings(found)
	return found, nil
}
