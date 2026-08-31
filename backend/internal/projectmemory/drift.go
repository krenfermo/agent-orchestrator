package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// drift.go — proving, or failing to prove, that a stored fact still holds.
//
// The indexing passes keep memory in step with the repository as they see it
// change. Drift detection is the independent check that answers the question
// no pass can: has the repository moved in a way no pass observed — a file
// edited outside AO, a checkout swapped underneath it, a branch reset?
//
// It is built on one asymmetry, and the asymmetry is the whole safety
// argument: **a fact can be disproved cheaply and can never be proved by this
// check.** Recomputing the digest of an item's source paths either matches —
// in which case the fact is exactly as good as it was — or does not, in which
// case the fact is demonstrably about a version of the file that is gone. So
// the detector only ever demotes. It never promotes a stale item back to
// valid, because a matching digest does not show that the *derivation* is
// still right; only a re-derivation does, and that is the indexer's job.
//
// Items with no source paths (the repository overview, a module census) cannot
// be checked this way: their provenance is the whole tree, and recomputing it
// is a full pass. They are reported as unverifiable rather than silently
// counted as confirmed — see DriftReport.Unverifiable.

// DriftRequest asks for one drift check over one repository.
type DriftRequest struct {
	ProjectID domain.ProjectID
	// RepoPath is the repository root to check against.
	RepoPath string
	// Commit is the commit the working tree is at, recorded in the reasons
	// the check writes so an operator can see when a fact went stale.
	Commit string
	// Apply writes the demotions. With Apply false the check is a dry run:
	// it reports exactly what it would do and changes nothing, which is what
	// `ao memory status --verify` uses.
	Apply bool
	// MaxChecks bounds the work. Zero means DefaultDriftChecks. The bound
	// exists because this runs on operator command and on a wake, and an
	// unbounded verify over a large repository is a verify nobody runs.
	MaxChecks int
}

// DefaultDriftChecks bounds one drift pass.
const DefaultDriftChecks = 2000

// DriftFinding is one fact whose provenance no longer holds.
type DriftFinding struct {
	ItemID string
	Key    domain.ProjectMemoryKey
	// From and To are the state transition the check would apply, or applied.
	From domain.ProjectMemoryState
	To   domain.ProjectMemoryState
	// Reason is the operator-readable explanation, and is what gets written
	// into the item's StateReason.
	Reason string
	// Applied reports whether the demotion was written. It is false for a dry
	// run, and also for a demotion the store refused because a newer
	// generation had already moved the row.
	Applied bool
}

// DriftReport is what one check found.
type DriftReport struct {
	RepoID string
	// Checked counts items whose provenance the check could evaluate.
	Checked int
	// Confirmed counts items whose source digests still match.
	Confirmed int
	// Unverifiable counts valid items with no path-anchored provenance —
	// aggregates whose source is the whole tree. They are left alone, and
	// counted here so the report never implies they were confirmed.
	Unverifiable int
	// Truncated reports that MaxChecks stopped the pass short, which means
	// "no drift found" covers only what was checked.
	Truncated bool
	Findings  []DriftFinding
}

// Drifted reports whether anything was found to have moved.
func (r DriftReport) Drifted() bool { return len(r.Findings) > 0 }

// Detector checks stored provenance against a repository on disk.
type Detector struct {
	repo Repository
	now  func() time.Time
	// hash lets a test substitute the file hasher. Production always uses the
	// same content hash the indexer writes, which is the point: two different
	// hashes would make every fact look drifted.
	hash func(path string) (string, error)
}

// NewDetector builds a drift detector over a durable repository.
func NewDetector(repo Repository) *Detector {
	return &Detector{
		repo: repo,
		now:  func() time.Time { return time.Now().UTC() },
		hash: hashFileContent,
	}
}

// Check evaluates every currently-valid fact whose provenance names a path.
func (d *Detector) Check(ctx context.Context, req DriftRequest) (DriftReport, error) {
	repoPath, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		return DriftReport{}, err
	}
	repoID := domain.ProjectMemoryRepoID(repoPath)
	report := DriftReport{RepoID: repoID}

	limit := req.MaxChecks
	if limit <= 0 {
		limit = DefaultDriftChecks
	}

	items, err := d.repo.ListProjectMemoryItemsByState(ctx, req.ProjectID, repoID, domain.MemoryStateValid)
	if err != nil {
		return report, err
	}

	// Hash each distinct path once. A convention item and the instruction item
	// it was lifted out of share a source file, and hashing it twice would
	// double the cost of the check for no new information.
	digests := map[string]string{}
	missing := map[string]bool{}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if len(item.SourcePaths) == 0 || item.SourceDigest == "" {
			report.Unverifiable++
			continue
		}
		if report.Checked >= limit {
			report.Truncated = true
			break
		}
		report.Checked++

		finding, ok, err := d.evaluate(repoPath, item, digests, missing, req.Commit)
		if err != nil {
			return report, err
		}
		if !ok {
			report.Confirmed++
			continue
		}
		if req.Apply {
			applied, err := d.repo.MarkProjectMemoryItemState(ctx, item.ID, item.Generation,
				finding.To, finding.Reason, d.now())
			if err != nil {
				return report, err
			}
			finding.Applied = applied
		}
		report.Findings = append(report.Findings, finding)
	}

	sort.Slice(report.Findings, func(i, j int) bool { return report.Findings[i].ItemID < report.Findings[j].ItemID })
	return report, nil
}

// evaluate decides whether one item's provenance still holds.
func (d *Detector) evaluate(
	repoPath string, item domain.ProjectMemoryItem,
	digests map[string]string, missing map[string]bool, commit string,
) (DriftFinding, bool, error) {
	pairs := make(map[string]string, len(item.SourcePaths))
	var gone []string

	for _, rel := range item.SourcePaths {
		if missing[rel] {
			gone = append(gone, rel)
			continue
		}
		digest, cached := digests[rel]
		if !cached {
			abs, ok := confinedPath(repoPath, rel)
			if !ok {
				// A stored source path that escapes the repository root is not
				// evidence of anything. Treating it as missing fails closed.
				missing[rel] = true
				gone = append(gone, rel)
				continue
			}
			h, err := d.hash(abs)
			switch {
			case errors.Is(err, os.ErrNotExist):
				missing[rel] = true
				gone = append(gone, rel)
				continue
			case err != nil:
				return DriftFinding{}, false, fmt.Errorf("hash %s: %w", rel, err)
			}
			digest = h
			digests[rel] = h
		}
		pairs[rel] = digest
	}

	if len(gone) > 0 {
		return DriftFinding{
			ItemID: item.ID, Key: item.Key,
			From: item.State, To: domain.MemoryStateInvalidated,
			Reason: fmt.Sprintf("source %s no longer present at %s",
				strings.Join(gone, ", "), orNone(commit)),
		}, true, nil
	}
	if domain.MemorySourceDigest(pairs) == item.SourceDigest {
		return DriftFinding{}, false, nil
	}
	return DriftFinding{
		ItemID: item.ID, Key: item.Key,
		From: item.State, To: domain.MemoryStateStale,
		Reason: fmt.Sprintf("source content moved since this fact was derived at %s",
			orNone(item.SourceCommit)),
	}, true, nil
}

// InvalidatePaths marks everything derived from the named paths as no longer
// authoritative, without deriving anything.
//
// It is the operator's `ao memory invalidate` and the safety valve a caller
// reaches for when it knows the repository moved but cannot say how: marking
// too much stale costs a re-derivation, and marking too little costs a wrong
// answer handed to an agent.
func (d *Detector) InvalidatePaths(
	ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (int64, error) {
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		return 0, err
	}
	repoID := domain.ProjectMemoryRepoID(canonical)
	if strings.TrimSpace(reason) == "" {
		reason = "invalidated by operator request"
	}
	var total int64
	for _, rel := range domain.NormalizeMemorySourcePaths(paths) {
		items, _, err := d.repo.InvalidateProjectMemoryByPath(ctx, projectID, repoID, rel,
			domain.MemoryStateStale, reason, d.now())
		if err != nil {
			return total, err
		}
		total += items
	}
	return total, nil
}

// hashFileContent computes the same digest the indexer writes. It exists as a
// named function rather than an inline read so there is exactly one definition
// of "this file's content hash" in the package.
func hashFileContent(abs string) (string, error) {
	content, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the repository root by confinedPath
	if err != nil {
		return "", err
	}
	return hashBytes(content), nil
}

// confinedPath resolves a repo-relative path against the repository root and
// refuses anything that escapes it. Stored provenance is data, and data that
// says "../../etc/passwd" must not become a read.
func confinedPath(root, rel string) (string, bool) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", false
	}
	abs := filepath.Join(root, clean)
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", false
	}
	return abs, true
}
