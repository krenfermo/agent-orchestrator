package projectmemory

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// reader.go — reading back what a run's dispatches recorded about their context.
//
// The evidence tree is written per dispatch and filed under the run (see
// EvidenceRecord.RunKey). Reading it back is what turns "AO measured its own
// context" into something an operator can see: how much AO assembled, what it
// was made of, what project memory contributed, and what the router avoided
// sending.
//
// One honesty rule survives the round trip unchanged. These are AO-ASSEMBLED
// context sizes. AO does not observe what a coding harness reads inside the
// worktree after the prompt lands, so nothing here is, or may be reported as,
// the provider's input token count — see docs/p2-project-memory-audit.md §1 and
// the read model in internal/service/usage.

// DirSource reads evidence records back out of a DirSink's tree.
type DirSource struct{ root string }

// NewDirSource opens a reader over an evidence directory.
func NewDirSource(dir string) (*DirSource, error) {
	if err := ValidateEvidenceDir(dir); err != nil {
		return nil, err
	}
	return &DirSource{root: filepath.Clean(dir)}, nil
}

// NewDefaultDirSource reads the standard evidence location under AO's data dir.
func NewDefaultDirSource() (*DirSource, error) {
	dir, err := EvidenceDir()
	if err != nil {
		return nil, err
	}
	return NewDirSource(dir)
}

// Root is where this source reads from.
func (s *DirSource) Root() string { return s.root }

// ListForRun returns every evidence record filed under one run, oldest first.
//
// A run with no evidence returns an empty slice and no error: a run that
// predates the baseline harness, or one whose dispatches recorded nothing, is
// an ordinary state and must be reported as an absence rather than as an error
// or as a set of zeroes.
//
// A single unreadable or malformed file is SKIPPED rather than failing the
// whole read. The alternative — one corrupt file making a run's whole context
// story unavailable — trades a partial answer for none at all, and the caller
// is told how many were skipped so a partial answer is visibly partial.
func (s *DirSource) ListForRun(_ stdctx.Context, runKey string) ([]EvidenceRecord, int, error) {
	key := sanitizePathSegment(runKey)
	if key == "" {
		return nil, 0, fmt.Errorf("%w: run key %q has no usable path segment", ErrEvidencePath, runKey)
	}
	dir := filepath.Join(s.root, key)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read evidence dir: %w", err)
	}
	records := make([]EvidenceRecord, 0, len(entries))
	skipped := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // AO's own data dir
		if rerr != nil {
			skipped++
			continue
		}
		var record EvidenceRecord
		if json.Unmarshal(raw, &record) != nil {
			skipped++
			continue
		}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if !records[i].GeneratedAt.Equal(records[j].GeneratedAt) {
			return records[i].GeneratedAt.Before(records[j].GeneratedAt)
		}
		return records[i].RecordID < records[j].RecordID
	})
	return records, skipped, nil
}
