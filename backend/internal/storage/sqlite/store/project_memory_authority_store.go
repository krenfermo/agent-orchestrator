package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// project_memory_authority_store.go — the durable side of the P2-D authority
// axis (migration 0146).
//
// It is a separate file from project_memory_store.go for the same reason
// authority is a separate column from state: the two are different questions
// with different writers. project_memory_store.go is written by INDEXERS,
// which derive content and fence each other with generations. Everything here
// is written by VALIDATORS and by PROMOTION, which never touch content at all
// — they only ever move a fact between "AO can still prove this" and "AO can
// no longer prove this".
//
// Every method returns the number of rows it actually moved, and no method
// treats zero as an error. Zero is the normal, expected answer for a CAS that
// lost: a validation pass that woke up after a rebuild finds the rebuilt row
// at a newer generation and changes nothing, which is exactly right (P2-D §6).

// SetProjectMemoryItemAuthority moves one fact's licence, leaving its drift
// state and its content untouched.
//
// Generation-conditioned, and the fence here is the sharp one: a stale
// validation pass must not be able to mark a REBUILT row unprovable. A rebuild
// allocates a new generation, so the stale validator's write matches no row
// and reports applied=false rather than quietly winning.
func (s *Store) SetProjectMemoryItemAuthority(
	ctx context.Context, id string, generation int64,
	authority domain.MemoryAuthority, reason string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetProjectMemoryItemAuthority(ctx, gen.SetProjectMemoryItemAuthorityParams{
		Authority:       string(authority),
		AuthorityReason: reason,
		UpdatedAt:       now,
		ID:              id,
		Generation:      generation,
	})
	if err != nil {
		return false, fmt.Errorf("set project memory item authority for %s: %w", id, err)
	}
	return n > 0, nil
}

// ProjectMemoryPromotionProof is what licensed one fact's promotion to
// canonical, written in the same act that creates the canonical row.
//
// It is a struct rather than seven arguments because the seven have to travel
// together: a promotion authority without the commits it pins to is not proof
// of anything, and a caller able to pass one without the others would be able
// to write exactly the half-evidence this phase exists to eliminate.
type ProjectMemoryPromotionProof struct {
	// Authority is the workflow_mutation_provenance row id that proves the
	// work reached the repository. Empty means AO could not prove it — in
	// which case Authority below must be AuthorityUnprovable, and
	// SetProjectMemoryItemPromotionProof enforces nothing about that: the
	// decision belongs to the promotion path, which is the only place that
	// holds the durable rows to decide it.
	MutationProvenanceID string
	// VerifiedCommit is what verification passed on; IntegratedCommit is the
	// target-branch commit the work became part of. They are separate because
	// they license different things (P2-D §7).
	VerifiedCommit   string
	IntegratedCommit string
	// RepoIdentity is the durable identity of the repository the promotion was
	// observed against, so a later checkout of a DIFFERENT repository at the
	// same path cannot inherit this fact.
	RepoIdentity   domain.RepoIdentity
	ProvenanceKind domain.MemoryProvenanceKind
	// MemoryAuthority and Reason are the licence the promotion path concluded.
	MemoryAuthority domain.MemoryAuthority
	Reason          string
}

// SetProjectMemoryItemPromotionProof records what licensed one promoted fact.
func (s *Store) SetProjectMemoryItemPromotionProof(
	ctx context.Context, id string, generation int64, proof ProjectMemoryPromotionProof, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	authority := proof.MemoryAuthority
	if authority == "" {
		// A caller that names no authority gets the withholding one. The
		// default direction has to be "not served": a bug that forgets to set
		// this field must cost a re-derivation, never a fabricated licence.
		authority = domain.AuthorityUnprovable
	}
	n, err := s.qw.SetProjectMemoryItemPromotionProof(ctx, gen.SetProjectMemoryItemPromotionProofParams{
		PromotionAuthority: proof.MutationProvenanceID,
		VerifiedCommit:     proof.VerifiedCommit,
		IntegratedCommit:   proof.IntegratedCommit,
		RepoIdentity:       string(proof.RepoIdentity),
		ProvenanceKind:     string(proof.ProvenanceKind),
		Authority:          string(authority),
		AuthorityReason:    proof.Reason,
		UpdatedAt:          now,
		ID:                 id,
		Generation:         generation,
	})
	if err != nil {
		return false, fmt.Errorf("set project memory promotion proof for %s: %w", id, err)
	}
	return n > 0, nil
}

// ListProjectMemoryItemsByAuthority reads every fact of one repository holding
// one authority. It is the withheld-items read `ao memory validate` and
// `ao memory drift` report from.
func (s *Store) ListProjectMemoryItemsByAuthority(
	ctx context.Context, projectID domain.ProjectID, repoID string, authority domain.MemoryAuthority,
) ([]domain.ProjectMemoryItem, error) {
	rows, err := s.qr.ListProjectMemoryItemsByAuthority(ctx, gen.ListProjectMemoryItemsByAuthorityParams{
		ProjectID: string(projectID), RepoID: repoID, Authority: string(authority),
	})
	if err != nil {
		return nil, fmt.Errorf("list project memory items by authority %s: %w", authority, err)
	}
	return projectMemoryItemsFromRows(rows)
}

// CountProjectMemoryItemsByAuthority is the operator summary: how much of this
// repository's memory is currently being withheld, and under which licence.
func (s *Store) CountProjectMemoryItemsByAuthority(
	ctx context.Context, projectID domain.ProjectID, repoID string,
) (map[domain.MemoryAuthority]int64, error) {
	rows, err := s.qr.CountProjectMemoryItemsByAuthority(ctx, gen.CountProjectMemoryItemsByAuthorityParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if err != nil {
		return nil, fmt.Errorf("count project memory items by authority: %w", err)
	}
	out := make(map[domain.MemoryAuthority]int64, len(rows))
	for _, r := range rows {
		out[domain.MemoryAuthority(r.Authority)] = r.Total
	}
	return out, nil
}

// MarkProjectMemoryItemsUnprovableByRepoIdentity withholds every fact of one
// repository that was not derived under the identity now observed.
//
// This is the "different repository at the same path" case (P2-D §9), and it
// is deliberately unlike every other invalidation in this package:
//
//   - It is NOT scoped by path. No individual file is wrong; the premise that
//     these facts are about THIS repository is wrong.
//   - It is NOT generation-conditioned. A generation fence protects one pass's
//     work from another pass's; this is not a pass's opinion, it is a fact
//     about the checkout that holds for every generation at once.
//   - It withholds rows that recorded no identity at all, because "AO could
//     not tell which repository this came from" has never been permission to
//     serve it as this repository's.
//
// Passing an unknown (empty) observed identity is refused rather than executed.
// With an empty argument the statement would withhold every fact whose
// identity is non-empty — that is, all of them — which turns "AO could not
// read the repository's identity today" into "this project's memory is gone".
// The correct response to an unreadable identity is to prove nothing and
// change nothing.
func (s *Store) MarkProjectMemoryItemsUnprovableByRepoIdentity(
	ctx context.Context, projectID domain.ProjectID, repoID string,
	observed domain.RepoIdentity, reason string, now time.Time,
) (int64, error) {
	if !observed.Known() {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkProjectMemoryItemsUnprovableByRepoIdentity(
		ctx, gen.MarkProjectMemoryItemsUnprovableByRepoIdentityParams{
			Authority:        string(domain.AuthorityUnprovable),
			AuthorityReason:  reason,
			UpdatedAt:        now,
			ProjectID:        string(projectID),
			RepoID:           repoID,
			ObservedIdentity: string(observed),
		})
	if err != nil {
		return 0, fmt.Errorf("withhold project memory on repo identity change: %w", err)
	}
	return n, nil
}

// MarkLegacyProjectMemoryItemsUnprovable classifies the rows an upgraded
// install already had (P2-D §21).
//
// It never fabricates provenance and never deletes: a legacy row is withheld,
// labelled as legacy rather than as broken, and left available for a bounded
// rebuild. It is guarded on authority still being the default, so a row a
// later validation pass has already ruled on is never dragged back to legacy
// by a second sweep.
func (s *Store) MarkLegacyProjectMemoryItemsUnprovable(
	ctx context.Context, projectID domain.ProjectID, repoID, reason string, now time.Time,
) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkLegacyProjectMemoryItemsUnprovable(ctx, gen.MarkLegacyProjectMemoryItemsUnprovableParams{
		AuthorityReason: reason,
		UpdatedAt:       now,
		ProjectID:       string(projectID),
		RepoID:          repoID,
	})
	if err != nil {
		return 0, fmt.Errorf("classify legacy project memory: %w", err)
	}
	return n, nil
}

// SetProjectMemoryRelationAuthority moves one edge's licence.
func (s *Store) SetProjectMemoryRelationAuthority(
	ctx context.Context, id string, generation int64,
	authority domain.MemoryAuthority, reason string, now time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetProjectMemoryRelationAuthority(ctx, gen.SetProjectMemoryRelationAuthorityParams{
		Authority:       string(authority),
		AuthorityReason: reason,
		UpdatedAt:       now,
		ID:              id,
		Generation:      generation,
	})
	if err != nil {
		return false, fmt.Errorf("set project memory relation authority for %s: %w", id, err)
	}
	return n > 0, nil
}

// RetireProjectMemoryRelationsForNode withholds every edge touching one node,
// in both directions.
//
// It is what an item losing its licence does to the graph around it (P2-D
// §23). Edges are retired, never deleted: the record that two facts were once
// related is precisely what an operator reads when asking why a decision was
// made, and deleting it would make the audit trail depend on the facts still
// being current.
func (s *Store) RetireProjectMemoryRelationsForNode(
	ctx context.Context, projectID domain.ProjectID, repoID string,
	node domain.ProjectMemoryNode, reason string, now time.Time,
) (int64, error) {
	node = node.Normalized()
	if node.Kind == "" || node.Key == "" {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.MarkProjectMemoryRelationsUnprovableForNode(
		ctx, gen.MarkProjectMemoryRelationsUnprovableForNodeParams{
			Authority:       string(domain.AuthorityUnprovable),
			AuthorityReason: reason,
			UpdatedAt:       now,
			ProjectID:       string(projectID),
			RepoID:          repoID,
			NodeKind:        string(node.Kind),
			NodeKey:         node.Key,
		})
	if err != nil {
		return 0, fmt.Errorf("retire project memory relations for node %s: %w", node.String(), err)
	}
	return n, nil
}

// ProjectMemoryChangeMark is the instant one repository's memory last changed
// in any way that can alter what a reader is served.
//
// It exists for the pack cache, and it closes a hole P2-D made visible rather
// than one it created. The cache key is built from the memory GENERATION and
// the indexed commit, and both of those move only when an indexing pass runs.
// Every out-of-band demotion -- drift invalidation, `ao memory invalidate`, an
// authority pass, a promotion recording a refusal -- changes what a reader
// should be served WITHOUT touching either, so a pack cached moments earlier
// stayed reachable and kept serving a fact AO had just withheld.
//
// Every one of those writes moves `updated_at` on the row it touches, so the
// newest `updated_at` of the repository is exactly the right epoch: it advances
// on any change to what is servable, and on nothing else.
//
// A repository with no facts has no mark, and the zero time is a complete
// answer -- there is nothing to serve, so there is nothing to cache wrongly.
func (s *Store) ProjectMemoryChangeMark(
	ctx context.Context, projectID domain.ProjectID, repoID string,
) (time.Time, error) {
	last, err := s.qr.LatestProjectMemoryItemUpdatedAt(ctx, gen.LatestProjectMemoryItemUpdatedAtParams{
		ProjectID: string(projectID), RepoID: repoID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read project memory change mark: %w", err)
	}
	return last, nil
}

// DeregisterProjectMemoryRepo removes a repository memory entirely: its facts,
// its edges, its file ledger, its provenance index AND its registration.
//
// It is separate from PurgeProjectMemoryRepo, which empties a repository so the
// next pass can rebuild it and therefore KEEPS the index row that gives that
// pass its identity. Deregistration is for a "repository" that never was one --
// P2-E's worktree-minted memories -- where leaving the registration behind
// would keep it showing up in `ao memory status` as a repository of the project
// forever.
//
// The caller proves it is safe to call. This method deletes what it is told to.
func (s *Store) DeregisterProjectMemoryRepo(
	ctx context.Context, projectID domain.ProjectID, repoID string,
) error {
	if err := s.PurgeProjectMemoryRepo(ctx, projectID, repoID); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "deregister project memory repo", func(q *gen.Queries) error {
		if _, err := q.DeleteProjectMemorySourcesForRepo(ctx, gen.DeleteProjectMemorySourcesForRepoParams{
			ProjectID: string(projectID), RepoID: repoID,
		}); err != nil {
			return fmt.Errorf("delete sources: %w", err)
		}
		if _, err := q.DeleteProjectMemoryIndex(ctx, gen.DeleteProjectMemoryIndexParams{
			ProjectID: string(projectID), RepoID: repoID,
		}); err != nil {
			return fmt.Errorf("delete index row: %w", err)
		}
		return nil
	})
}
