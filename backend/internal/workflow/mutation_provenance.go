package workflow

import (
	stdctx "context"
	"encoding/json"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
)

// mutation_provenance.go — the production writer migration 0133 never got
// (P2-D §4, §5, §6).
//
// The table `workflow_mutation_provenance` has existed since 0133, with a
// schema, a store method, and — in production — zero rows. Everything that
// wanted to know "which workflow/task produced this change" read
// `workflow_checkpoints.retry_state` JSON instead, filtered on a durable_phase
// string, and json-decoded whatever the Go struct happened to look like on the
// day it was written. That works for the verification path, which is standing
// in the workspace it is asking about. It does not work for a memory
// promotion, which asks the same question from a different subsystem, minutes
// or hours later, about a branch that has since moved.
//
// So this file writes the rows. Three properties are load-bearing, and each is
// a specific failure P2-D's audit found:
//
//   - **Boundaries, not writes.** AO records the five moments at which the
//     answer to "may this become project knowledge" can change (see
//     domain.WorkflowMutationBoundary), not every mutation of every file. A
//     row per write syscall would be a transcript; a row per boundary is
//     evidence.
//   - **Generation-conditioned.** A stale worker, reviewer or repair callback
//     that wakes up after a newer generation has recorded the same boundary
//     must not append a row that later reads as the current one. The CAS is
//     `recordMutationProvenance`'s refusal below, and it is checked against
//     the durable row rather than against anything in memory.
//   - **Exactly once.** Every row carries an idempotency key derived from the
//     facts of the boundary itself, so a duplicate completion callback and a
//     daemon that died between the mutation and the row produce ONE row. See
//     domain.MutationIdempotencyKey and the partial unique index in 0146.
//
// Everything here is best-effort at the call sites, exactly like the memory
// writes: a run that did its work and integrated it must not fail because an
// evidence row could not be appended. What it must NOT be is silently skipped
// — a promotion that finds no provenance refuses to promote, so a missing row
// costs a withheld fact rather than a fabricated one, which is the correct
// direction for this whole subsystem.

// MutationProvenance is the slice of the provenance store this package writes
// and reads.
//
// Declared at the consumer and deliberately four methods wide, following the
// same local-narrow-interface pattern as TaskMemory and evidence_snapshot.go's
// provenance reader. A nil MutationProvenance disables recording, and every
// call site treats that as the ordinary "this daemon does not have it" state
// rather than as a degraded mode — with the one consequence that promotions
// which need a proof will not find one, and will withhold.
type MutationProvenance interface {
	// RecordWorkflowMutationProvenance appends one boundary record, exactly
	// once per idempotency key. It returns the row that describes the boundary
	// whether this call wrote it or an earlier one already had.
	RecordWorkflowMutationProvenance(ctx stdctx.Context, p domain.WorkflowMutationProvenance) (domain.WorkflowMutationProvenance, error)
	// GetLatestWorkflowMutationProvenanceByTaskBoundary reads the newest
	// record of one boundary for one task.
	GetLatestWorkflowMutationProvenanceByTaskBoundary(ctx stdctx.Context, taskID string, boundary domain.WorkflowMutationBoundary) (domain.WorkflowMutationProvenance, bool, error)
	// ListWorkflowMutationProvenanceByTask reads every boundary of one task,
	// oldest first.
	ListWorkflowMutationProvenanceByTask(ctx stdctx.Context, taskID string) ([]domain.WorkflowMutationProvenance, error)
	// ListWorkflowMutationProvenanceByRun reads every boundary of one run.
	ListWorkflowMutationProvenanceByRun(ctx stdctx.Context, runID string) ([]domain.WorkflowMutationProvenance, error)
}

// mutationBoundary is everything one call site knows about the moment it is
// recording. It is a struct rather than a long argument list because the
// fields have to be filled in from several different durable reads at each
// call site, and a positional call would make it easy to write a boundary with
// the source SHA in the target's slot — which is a lie the schema cannot catch.
type mutationBoundary struct {
	run  domain.WorkflowRun
	step *domain.WorkflowStep
	// taskID is the planned task, empty for a run with no decomposition. A
	// boundary with no task can still be recorded (it is real evidence about
	// the run) but it can never license a memory promotion, because promotion
	// is keyed by task.
	taskID string

	boundary  domain.WorkflowMutationBoundary
	placement domain.WorkflowMutationPlacement
	class     domain.WorkflowMutationClass
	// generation fences the write. It is the attempt/integration generation
	// the caller is acting under, never a clock and never a counter this file
	// keeps.
	generation int64

	repoPath     string
	repoIdentity domain.RepoIdentity
	branch       string
	worktreePath string
	sessionID    string
	harness      string

	baseSHA string
	headSHA string

	fingerprintBefore string
	fingerprintAfter  string

	integrationTargetRef       string
	integrationTargetBeforeSHA string
	integrationTargetAfterSHA  string
	integrationMethod          domain.WorkflowIntegrationMethod

	reason   string
	evidence map[string]any
}

// idempotencyKey derives the boundary's identity from the boundary's own
// facts.
//
// The target SHA participates for an integration and is empty otherwise, which
// is what keeps a re-integration onto a moved target a DIFFERENT boundary from
// the first one while a duplicated callback about the same integration stays
// the same boundary.
func (b mutationBoundary) idempotencyKey() string {
	target := b.integrationTargetAfterSHA
	return domain.MutationIdempotencyKey(b.run.ID, b.taskID, b.boundary, b.generation, b.headSHA, target)
}

// recordMutationProvenance appends one boundary record.
//
// It is generation-safe in the strong sense: before writing it reads the
// newest durable record of the SAME (task, boundary) and refuses if that
// record is at a newer generation. That check is what stops a stale callback
// from appending a row that a later "newest wins" read would treat as current
// — and it has to be a read of the durable row rather than of anything this
// process remembers, because the stale caller is frequently a different
// process, or the same one after a restart.
//
// The refusal is not an error. A stale callback is a normal event, and telling
// its caller it failed would make several best-effort paths log noise about
// something that worked correctly. It returns (zero, false, nil).
func (c *Coordinator) recordMutationProvenance(
	ctx stdctx.Context, b mutationBoundary,
) (domain.WorkflowMutationProvenance, bool, error) {
	if c.mutationProvenance == nil || !b.boundary.Valid() {
		return domain.WorkflowMutationProvenance{}, false, nil
	}

	// Generation fence. Only meaningful for a boundary that names a task,
	// because that is the granularity the "newest wins" reads use.
	if b.taskID != "" {
		latest, found, err := c.mutationProvenance.GetLatestWorkflowMutationProvenanceByTaskBoundary(ctx, b.taskID, b.boundary)
		if err != nil {
			return domain.WorkflowMutationProvenance{}, false, err
		}
		if found && latest.Generation > b.generation {
			if c.log != nil {
				c.log.Info("workflow: refused a stale mutation-provenance write",
					"task", b.taskID, "boundary", string(b.boundary),
					"writerGeneration", b.generation, "storedGeneration", latest.Generation)
			}
			return domain.WorkflowMutationProvenance{}, false, nil
		}
	}

	evidence := "{}"
	if len(b.evidence) > 0 {
		if raw, err := json.Marshal(b.evidence); err == nil {
			evidence = string(raw)
		}
	}
	class := b.class
	if class == "" {
		class = domain.MutationUnknown
	}
	now := c.clock()
	rec := domain.WorkflowMutationProvenance{
		ID:            "wmp-" + c.newID(),
		WorkflowRunID: b.run.ID,
		TaskID:        b.taskID,
		Class:         class,
		Harness:       b.harness,
		Branch:        b.branch,
		WorktreePath:  b.worktreePath,
		BaseSHA:       b.baseSHA,
		HeadSHA:       b.headSHA,

		FingerprintBefore: b.fingerprintBefore,
		FingerprintAfter:  b.fingerprintAfter,
		Reason:            b.reason,
		EvidenceJSON:      evidence,
		// ObservedAt is the boundary's own instant, which for every caller here
		// is the moment AO read the durable fact it is recording. It is set
		// rather than left nil precisely because these callers DID observe it;
		// the nil case is reserved for a reconstruction that honestly cannot
		// say when the mutation happened.
		ObservedAt: &now,
		CreatedAt:  now,

		ProjectID:                  b.run.ProjectID,
		RepoIdentity:               b.repoIdentity,
		RepoPath:                   b.repoPath,
		Placement:                  b.placement,
		Boundary:                   b.boundary,
		Generation:                 b.generation,
		IntegrationTargetRef:       b.integrationTargetRef,
		IntegrationTargetBeforeSHA: b.integrationTargetBeforeSHA,
		IntegrationTargetAfterSHA:  b.integrationTargetAfterSHA,
		IntegrationMethod:          b.integrationMethod,
		IdempotencyKey:             b.idempotencyKey(),
	}
	if b.step != nil {
		id := b.step.ID
		rec.WorkflowStepID = &id
	}
	if s := strings.TrimSpace(b.sessionID); s != "" {
		rec.SessionID = &s
	}

	stored, err := c.mutationProvenance.RecordWorkflowMutationProvenance(ctx, rec)
	if err != nil {
		return domain.WorkflowMutationProvenance{}, false, err
	}
	return stored, true, nil
}

// recordMutationBoundary is the best-effort wrapper every call site uses. It
// logs and swallows, because no boundary this file records is a boundary whose
// failure should undo the thing that happened.
//
// It reports only WHETHER a row was recorded, not which one. The row itself is
// read back later, from the durable store, by whichever promotion needs it --
// which is the point: a proof built from a value this process happened to be
// holding would be a proof about this process rather than about the repository.
func (c *Coordinator) recordMutationBoundary(ctx stdctx.Context, b mutationBoundary) bool {
	_, ok, err := c.recordMutationProvenance(ctx, b)
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: could not record mutation provenance",
				"run", b.run.ID, "task", b.taskID, "boundary", string(b.boundary), "err", err)
		}
		return false
	}
	return ok
}

// mutationPlacementFor renders this run's execution placement into the
// provenance vocabulary. It goes through the same resolver the placement path
// itself uses, so "was this direct branch" cannot get two answers in one
// process (see task_memory.go's workIsIntegrated for the other reader).
func (c *Coordinator) mutationPlacementFor(ctx stdctx.Context, run domain.WorkflowRun) domain.WorkflowMutationPlacement {
	scope := placementScopeFor(run)
	// P3-A §7: the FROZEN record wins when there is one. Project configuration
	// answers "how does this project usually work", which is the wrong question
	// for a run whose placement was decided once and explicitly -- and reading
	// the mutable answer here is how a direct-branch run gets its mutations
	// filed as if they had happened in a worktree.
	if c.placementEnabled() {
		if live, found, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID); err == nil && found && live.Type.IsKnown() {
			if live.Type == domain.PlacementDirectBranch {
				return domain.MutationPlacementDirectBranch
			}
			return domain.MutationPlacementIsolatedWorktree
		}
	}
	if c.projectExecutionModeFor(ctx, run, scope).DirectBranch() {
		return domain.MutationPlacementDirectBranch
	}
	return domain.MutationPlacementIsolatedWorktree
}

// integrationMethodOf maps the Integration Coordinator's chosen strategy onto
// the provenance vocabulary.
//
// The mapping collapses four strategies into three methods, and the collapse
// is the point rather than a loss: what a promotion has to know is whether
// ANCESTRY is a valid proof of integration for this operation, and that is
// true of fast-forward and rebase-fast-forward (the source, as rebased, IS the
// target) and false of cherry-pick (same content, different SHAs). A strategy
// this build does not recognise maps to the empty method, which
// AncestryProves() reports false for — so an unknown strategy is proven only
// by the recorded target SHAs, never by an ancestry check that might not
// apply.
func integrationMethodOf(s integration.Strategy) domain.WorkflowIntegrationMethod {
	switch s {
	case integration.StrategyFastForward, integration.StrategyRebaseFastForward:
		return domain.IntegrationFastForward
	case integration.StrategyCherryPick:
		return domain.IntegrationCherryPick
	case integration.StrategyMergeCommit:
		return domain.IntegrationMerge
	case integration.StrategyNoOp:
		// Nothing was forwarded: the work was already on the target ref, which
		// is what direct-branch execution looks like from inside the
		// integration coordinator.
		return domain.IntegrationDirectCommit
	default:
		return ""
	}
}
