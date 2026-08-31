package projectmemory

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// digests.go — answering "did these documents change" without reading them
// (P2-B §5).
//
// The P2-A audit found the clearest piece of repeated work AO controls: the
// plan-reuse assessment rebuilds the whole planner context — six files read
// from disk with their bodies, up to 48 KiB each — purely to compute digests,
// which it then compares against digests the stored manifest already holds.
// And it is not a rare path: the assessment is deliberately re-derived on
// demand and is reached from an HTTP read a poll or a page load can hit freely.
//
// Project memory already knows those digests. The indexing pass hashes every
// admitted file and records `(path, content_digest, size_bytes, generation,
// indexed_commit)` in the digest ledger. When memory is current for the
// repository, the ledger can answer the drift question outright and the six
// reads never happen.
//
// Two conditions gate it, and both are necessary:
//
//   - **The ledger must be current for the commit being asked about.** A
//     digest recorded at an older commit describes an older file; using it
//     would let a changed document read as unchanged, which is the one error
//     that would make plan reuse unsound rather than merely slow.
//   - **The file must be small enough that the two digests mean the same
//     thing.** The planner truncates a document at 48 KiB before hashing it,
//     while the ledger hashes the whole file. For a file at or under the cap
//     the two are the same SHA-256 of the same bytes; above it they are not,
//     and such a file is reported as unknown so the caller reads it.
//
// Anything the ledger cannot answer is returned as unknown rather than
// guessed. The caller then reads exactly those paths — "if there is drift,
// read only what is affected".

// DocumentDigests is what the ledger could say about a set of paths.
type DocumentDigests struct {
	// Known maps a repo-relative path to the SHA-256 of its content, for
	// paths the ledger could answer.
	Known map[string]string
	// Unknown names the paths the caller must still read: absent from the
	// ledger, or too large for the digests to be comparable.
	Unknown []string
	// Generation and IndexedCommit are the provenance of the answer.
	Generation    int64
	IndexedCommit string
	// Usable reports whether the ledger was current enough to be consulted at
	// all. When false, Known is empty and every path is Unknown.
	Usable bool
	// Reason explains an unusable ledger, for operator output.
	Reason string
}

// DigestsFor answers what the digest ledger knows about a set of paths at a
// commit.
//
// It never returns an error: a repository with no memory, an unreadable store
// and a moved checkout all produce Usable=false with a reason, and the caller
// falls back to reading the files exactly as it did before P2-B.
func (s *Service) DigestsFor(
	ctx context.Context, projectID domain.ProjectID, repoPath, commit string,
	paths []string, maxComparableBytes int64,
) DocumentDigests {
	out := DocumentDigests{Known: map[string]string{}, Unknown: append([]string(nil), paths...)}

	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		out.Reason = "the repository path could not be resolved"
		return out
	}
	if commit == "" {
		// Without a commit AO cannot prove the ledger describes the tree in
		// front of it, so it does not claim to.
		out.Reason = "the checkout reported no commit to prove the ledger against"
		return out
	}
	repoID := domain.ProjectMemoryRepoID(canonical)

	state, found, err := s.repo.GetProjectMemoryIndexState(ctx, projectID, repoID)
	switch {
	case err != nil:
		out.Reason = "the project memory index could not be read"
		return out
	case !found || state.IndexedCommit == "":
		out.Reason = "this repository has not completed a project-memory index"
		return out
	case state.IndexedCommit != commit:
		out.Reason = "project memory is indexed at " + state.IndexedCommit + ", not at " + commit
		return out
	case state.Phase != domain.IndexPhaseIdle:
		out.Reason = "an indexing pass is in flight, so the ledger is mid-update"
		return out
	}
	out.Generation, out.IndexedCommit = state.Generation, state.IndexedCommit

	ledger, err := s.repo.ListProjectMemoryFiles(ctx, projectID, repoID)
	if err != nil {
		out.Reason = "the digest ledger could not be read"
		return out
	}
	byPath := make(map[string]int64, len(ledger))
	digest := make(map[string]string, len(ledger))
	for _, f := range ledger {
		byPath[f.Path] = f.Size
		digest[f.Path] = f.Digest
	}

	out.Usable = true
	out.Unknown = nil
	for _, p := range paths {
		key := normalizePath(p)
		size, present := byPath[key]
		switch {
		case !present:
			// Not in the ledger: excluded by the index bounds, or genuinely
			// absent. Either way the caller must look for itself.
			out.Unknown = append(out.Unknown, p)
		case maxComparableBytes > 0 && size > maxComparableBytes:
			// Above the caller's truncation cap the two hashes describe
			// different byte ranges and must not be compared.
			out.Unknown = append(out.Unknown, p)
		default:
			out.Known[key] = digest[key]
		}
	}
	return out
}

// DigestOf returns the ledger's digest for one path, if it has one.
func (d DocumentDigests) DigestOf(path string) (string, bool) {
	if !d.Usable {
		return "", false
	}
	digest, ok := d.Known[normalizePath(path)]
	return digest, ok
}

// Resolved reports how many of the requested paths the ledger answered, which
// is the measurement that proves this optimisation did anything.
func (d DocumentDigests) Resolved() int { return len(d.Known) }
