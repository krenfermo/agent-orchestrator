package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// workflow_provenance_store.go — typed access to migration 0133's durable
// dispatch checkpoints and mutation provenance, plus the verify attempt's
// deadline and review-target linkage.
//
// Both tables are append-only and there is deliberately no update path for
// either. A provenance record is evidence of a moment; editing one turns it
// into an assertion about a moment, which is a different and much weaker thing.
//
// Every read tolerates absence. A run created before 0133 has no rows in either
// table and no deadline or review target on its attempts, and reads back that
// way — an empty slice, a false ok, a nil pointer — never an error. Nothing is
// backfilled: fabricating provenance for a mutation nobody observed is exactly
// what these tables exist to make impossible.

// CreateWorkflowDispatchCheckpoint appends one dispatch-boundary record.
func (s *Store) CreateWorkflowDispatchCheckpoint(
	ctx context.Context,
	cp domain.WorkflowDispatchCheckpoint,
) (domain.WorkflowDispatchCheckpoint, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	evidence := cp.EvidenceJSON
	if evidence == "" {
		evidence = "{}"
	}
	row, err := s.qw.InsertWorkflowDispatchCheckpoint(ctx, gen.InsertWorkflowDispatchCheckpointParams{
		ID:             cp.ID,
		WorkflowRunID:  cp.WorkflowRunID,
		WorkflowStepID: stringPtrToNullString(cp.WorkflowStepID),
		AttemptID:      stringPtrToNullString(cp.AttemptID),
		CheckpointID:   stringPtrToNullString(cp.CheckpointID),
		Phase:          string(cp.Phase),
		IdempotencyKey: cp.IdempotencyKey,
		Harness:        cp.Harness,
		SessionID:      stringPtrToNullString(cp.SessionID),
		LaunchStage:    string(cp.LaunchStage),
		LaunchOutcome:  string(cp.LaunchOutcome),
		ErrorClass:     string(cp.ErrorClass),
		EvidenceJson:   evidence,
		Detail:         cp.Detail,
		// Migration 0134's launch evidence. Written exactly as observed: an
		// empty string is "the writer could not read this", and a nil
		// LaunchedAt is "the writer cannot say when the launch happened" —
		// neither is ever filled in from CreatedAt or from a neighbouring
		// field.
		Branch:               cp.Branch,
		WorktreePath:         cp.WorktreePath,
		BaseSha:              cp.BaseSHA,
		WorkspaceFingerprint: cp.WorkspaceFingerprint,
		RuntimeHandleID:      cp.RuntimeHandleID,
		RuntimeLaunchID:      cp.RuntimeLaunchID,
		AgentSessionID:       cp.AgentSessionID,
		LaunchedAt:           timePtrToNullTime(cp.LaunchedAt),
		CreatedAt:            cp.CreatedAt,
	})
	if err != nil {
		return domain.WorkflowDispatchCheckpoint{}, fmt.Errorf(
			"insert workflow dispatch checkpoint for run %s: %w", cp.WorkflowRunID, err)
	}
	return workflowDispatchCheckpointFromRow(row), nil
}

// ListWorkflowDispatchCheckpointsByRun lists a run's dispatch records oldest
// first. Empty for a run that predates migration 0133.
func (s *Store) ListWorkflowDispatchCheckpointsByRun(
	ctx context.Context,
	runID string,
) ([]domain.WorkflowDispatchCheckpoint, error) {
	rows, err := s.qr.ListWorkflowDispatchCheckpointsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow dispatch checkpoints for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowDispatchCheckpoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowDispatchCheckpointFromRow(row))
	}
	return out, nil
}

// ListWorkflowDispatchCheckpointsByStep lists one step's dispatch records
// oldest first.
func (s *Store) ListWorkflowDispatchCheckpointsByStep(
	ctx context.Context,
	stepID string,
) ([]domain.WorkflowDispatchCheckpoint, error) {
	rows, err := s.qr.ListWorkflowDispatchCheckpointsByStep(ctx, sql.NullString{String: stepID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list workflow dispatch checkpoints for step %s: %w", stepID, err)
	}
	out := make([]domain.WorkflowDispatchCheckpoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowDispatchCheckpointFromRow(row))
	}
	return out, nil
}

// GetLatestWorkflowDispatchCheckpointByStep returns the newest dispatch record
// for a step, ok=false when the step has none. Newest wins: a launch failure
// older than a successful dispatch has been superseded by a worker that
// actually launched.
func (s *Store) GetLatestWorkflowDispatchCheckpointByStep(
	ctx context.Context,
	stepID string,
) (domain.WorkflowDispatchCheckpoint, bool, error) {
	row, err := s.qr.GetLatestWorkflowDispatchCheckpointByStep(
		ctx, sql.NullString{String: stepID, Valid: true})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowDispatchCheckpoint{}, false, nil
	}
	if err != nil {
		return domain.WorkflowDispatchCheckpoint{}, false, fmt.Errorf(
			"get latest workflow dispatch checkpoint for step %s: %w", stepID, err)
	}
	return workflowDispatchCheckpointFromRow(row), true, nil
}

// RecordWorkflowMutationProvenance appends one mutation-provenance record,
// exactly once per boundary.
//
// "Exactly once" is a property of the BOUNDARY, not of the call. Two things
// routinely try to record the same moment twice, and neither is a bug:
//
//   - A completion callback delivered twice (the harness retried, the outbox
//     re-fired, a human pressed Continue).
//   - A daemon that died between observing the mutation and writing the row,
//     and re-observes the same mutation on restart.
//
// Both derive the same IdempotencyKey from the same facts (see
// domain.MutationIdempotencyKey), the partial unique index collapses the
// second write, and this method reads the surviving row back — so the caller
// always ends up holding the ONE row that describes the boundary, whether it
// wrote it or somebody else already had. A caller that could not derive a key
// leaves it empty and gets append-only behaviour, which is the honest
// fallback: two rows AO cannot prove are the same moment are two observations.
func (s *Store) RecordWorkflowMutationProvenance(
	ctx context.Context,
	p domain.WorkflowMutationProvenance,
) (domain.WorkflowMutationProvenance, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	evidence := p.EvidenceJSON
	if evidence == "" {
		evidence = "{}"
	}
	class := p.Class
	if class == "" {
		// An unattributed mutation is UNKNOWN, never a blank that later reads
		// as "no opinion was ever formed".
		class = domain.MutationUnknown
	}
	row, err := s.qw.InsertWorkflowMutationProvenance(ctx, gen.InsertWorkflowMutationProvenanceParams{
		ID:                p.ID,
		WorkflowRunID:     p.WorkflowRunID,
		WorkflowStepID:    stringPtrToNullString(p.WorkflowStepID),
		AttemptID:         stringPtrToNullString(p.AttemptID),
		TaskID:            p.TaskID,
		ProvenanceClass:   string(class),
		Harness:           p.Harness,
		SessionID:         stringPtrToNullString(p.SessionID),
		Branch:            p.Branch,
		WorktreePath:      p.WorktreePath,
		BaseSha:           p.BaseSHA,
		HeadSha:           p.HeadSHA,
		FingerprintBefore: p.FingerprintBefore,
		FingerprintAfter:  p.FingerprintAfter,
		Reason:            p.Reason,
		EvidenceJson:      evidence,
		ObservedAt:        timePtrToNullTime(p.ObservedAt),
		CreatedAt:         p.CreatedAt,

		ProjectID:                  p.ProjectID,
		RepoIdentity:               string(p.RepoIdentity),
		RepoPath:                   p.RepoPath,
		Placement:                  string(p.Placement),
		Boundary:                   string(p.Boundary),
		Generation:                 p.Generation,
		IntegrationTargetRef:       p.IntegrationTargetRef,
		IntegrationTargetBeforeSha: p.IntegrationTargetBeforeSHA,
		IntegrationTargetAfterSha:  p.IntegrationTargetAfterSHA,
		IntegrationMethod:          string(p.IntegrationMethod),
		IdempotencyKey:             p.IdempotencyKey,
	})
	if errors.Is(err, sql.ErrNoRows) {
		// ON CONFLICT DO NOTHING matched an existing row, so RETURNING yielded
		// nothing. The boundary is already recorded; hand back the row that
		// records it rather than a zero value the caller would read as "no
		// provenance exists".
		existing, found, gerr := s.getWorkflowMutationProvenanceByIdempotencyKeyLocked(ctx, p.IdempotencyKey)
		if gerr != nil {
			return domain.WorkflowMutationProvenance{}, gerr
		}
		if found {
			return existing, nil
		}
		return domain.WorkflowMutationProvenance{}, fmt.Errorf(
			"insert workflow mutation provenance for run %s: write was collapsed as a duplicate but no existing row could be read back",
			p.WorkflowRunID)
	}
	if err != nil {
		return domain.WorkflowMutationProvenance{}, fmt.Errorf(
			"insert workflow mutation provenance for run %s: %w", p.WorkflowRunID, err)
	}
	return workflowMutationProvenanceFromRow(row), nil
}

// GetWorkflowMutationProvenanceByIdempotencyKey reads the row describing one
// boundary, if AO has recorded it.
func (s *Store) GetWorkflowMutationProvenanceByIdempotencyKey(
	ctx context.Context, key string,
) (domain.WorkflowMutationProvenance, bool, error) {
	return s.getWorkflowMutationProvenanceByIdempotencyKeyLocked(ctx, key)
}

func (s *Store) getWorkflowMutationProvenanceByIdempotencyKeyLocked(
	ctx context.Context, key string,
) (domain.WorkflowMutationProvenance, bool, error) {
	if strings.TrimSpace(key) == "" {
		// An empty key is "this writer could not identify the boundary", and
		// it must never match the other rows that could not either.
		return domain.WorkflowMutationProvenance{}, false, nil
	}
	row, err := s.qr.GetWorkflowMutationProvenanceByIdempotencyKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowMutationProvenance{}, false, nil
	}
	if err != nil {
		return domain.WorkflowMutationProvenance{}, false, fmt.Errorf(
			"get workflow mutation provenance by idempotency key: %w", err)
	}
	return workflowMutationProvenanceFromRow(row), true, nil
}

// ListWorkflowMutationProvenanceByTask lists every boundary AO durably
// recorded for one planned task, oldest first. Empty for a task AO never
// observed a mutation for -- which is a complete answer, and the one that
// makes a promotion fail closed.
func (s *Store) ListWorkflowMutationProvenanceByTask(
	ctx context.Context, taskID string,
) ([]domain.WorkflowMutationProvenance, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, nil
	}
	rows, err := s.qr.ListWorkflowMutationProvenanceByTask(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("list workflow mutation provenance for task %s: %w", taskID, err)
	}
	out := make([]domain.WorkflowMutationProvenance, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowMutationProvenanceFromRow(row))
	}
	return out, nil
}

// GetLatestWorkflowMutationProvenanceByTaskBoundary returns the current state
// of one boundary for one task: the newest generation AO observed it at.
//
// Newest wins because a later observation supersedes an earlier one -- a
// re-integration after a repair is the case that matters -- and a promotion
// must be pinned to the last boundary AO actually saw, not the first.
func (s *Store) GetLatestWorkflowMutationProvenanceByTaskBoundary(
	ctx context.Context, taskID string, boundary domain.WorkflowMutationBoundary,
) (domain.WorkflowMutationProvenance, bool, error) {
	if strings.TrimSpace(taskID) == "" {
		return domain.WorkflowMutationProvenance{}, false, nil
	}
	row, err := s.qr.GetLatestWorkflowMutationProvenanceByTaskBoundary(
		ctx, gen.GetLatestWorkflowMutationProvenanceByTaskBoundaryParams{
			TaskID: taskID, Boundary: string(boundary),
		})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowMutationProvenance{}, false, nil
	}
	if err != nil {
		return domain.WorkflowMutationProvenance{}, false, fmt.Errorf(
			"get latest %s mutation provenance for task %s: %w", boundary, taskID, err)
	}
	return workflowMutationProvenanceFromRow(row), true, nil
}

// ListWorkflowMutationProvenanceByRun lists a run's provenance records oldest
// first. Empty for a run that predates migration 0133.
func (s *Store) ListWorkflowMutationProvenanceByRun(
	ctx context.Context,
	runID string,
) ([]domain.WorkflowMutationProvenance, error) {
	rows, err := s.qr.ListWorkflowMutationProvenanceByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list workflow mutation provenance for run %s: %w", runID, err)
	}
	out := make([]domain.WorkflowMutationProvenance, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowMutationProvenanceFromRow(row))
	}
	return out, nil
}

// ListWorkflowMutationProvenanceByStep lists one step's provenance records
// oldest first.
func (s *Store) ListWorkflowMutationProvenanceByStep(
	ctx context.Context,
	stepID string,
) ([]domain.WorkflowMutationProvenance, error) {
	rows, err := s.qr.ListWorkflowMutationProvenanceByStep(ctx, sql.NullString{String: stepID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("list workflow mutation provenance for step %s: %w", stepID, err)
	}
	out := make([]domain.WorkflowMutationProvenance, 0, len(rows))
	for _, row := range rows {
		out = append(out, workflowMutationProvenanceFromRow(row))
	}
	return out, nil
}

// GetLatestWorkflowMutationProvenanceByBranch returns who last changed one
// run's branch, ok=false when nothing was ever recorded for it. That false is
// the honest answer for a pre-0133 run and must not be read as "nobody changed
// it".
func (s *Store) GetLatestWorkflowMutationProvenanceByBranch(
	ctx context.Context,
	runID, branch string,
) (domain.WorkflowMutationProvenance, bool, error) {
	row, err := s.qr.GetLatestWorkflowMutationProvenanceByBranch(
		ctx, gen.GetLatestWorkflowMutationProvenanceByBranchParams{
			WorkflowRunID: runID,
			Branch:        branch,
		})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.WorkflowMutationProvenance{}, false, nil
	}
	if err != nil {
		return domain.WorkflowMutationProvenance{}, false, fmt.Errorf(
			"get latest workflow mutation provenance for run %s branch %s: %w", runID, branch, err)
	}
	return workflowMutationProvenanceFromRow(row), true, nil
}

// SetWorkflowAttemptDeadline records when an attempt must have concluded by.
// Reports whether a row was updated.
func (s *Store) SetWorkflowAttemptDeadline(
	ctx context.Context,
	attemptID string,
	deadline *time.Time,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetWorkflowAttemptDeadline(ctx, gen.SetWorkflowAttemptDeadlineParams{
		DeadlineAt: timePtrToNullTime(deadline),
		ID:         attemptID,
	})
	if err != nil {
		return false, fmt.Errorf("set workflow attempt %s deadline: %w", attemptID, err)
	}
	return rows > 0, nil
}

// SetWorkflowAttemptReviewTarget pins the reviewed artifact a verify attempt is
// judging, and reports whether this call is the one that pinned it.
//
// A second call with a different target is refused (ok=false), not applied: the
// point of the linkage is that a re-review cannot retarget a verification that
// is already in flight. Verifying different work means a new attempt.
func (s *Store) SetWorkflowAttemptReviewTarget(
	ctx context.Context,
	attemptID string,
	target domain.WorkflowReviewTarget,
) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.SetWorkflowAttemptReviewTarget(ctx, gen.SetWorkflowAttemptReviewTargetParams{
		ReviewTargetReviewRunID: stringPtrToNullString(target.ReviewRunID),
		ReviewTargetFingerprint: target.Fingerprint,
		ReviewTargetHeadSha:     target.HeadSHA,
		ID:                      attemptID,
	})
	if err != nil {
		return false, fmt.Errorf("set workflow attempt %s review target: %w", attemptID, err)
	}
	return rows > 0, nil
}

func workflowDispatchCheckpointFromRow(r gen.WorkflowDispatchCheckpoint) domain.WorkflowDispatchCheckpoint {
	return domain.WorkflowDispatchCheckpoint{
		ID:             r.ID,
		WorkflowRunID:  r.WorkflowRunID,
		WorkflowStepID: nullStringToPtr(r.WorkflowStepID),
		AttemptID:      nullStringToPtr(r.AttemptID),
		CheckpointID:   nullStringToPtr(r.CheckpointID),
		Phase:          domain.WorkflowDispatchPhase(r.Phase),
		IdempotencyKey: r.IdempotencyKey,
		Harness:        r.Harness,
		SessionID:      nullStringToPtr(r.SessionID),
		LaunchStage:    domain.WorkflowLaunchStage(r.LaunchStage),
		LaunchOutcome:  domain.WorkflowLaunchOutcome(r.LaunchOutcome),
		ErrorClass:     domain.WorkflowErrorClass(r.ErrorClass),
		EvidenceJSON:   r.EvidenceJson,
		Detail:         r.Detail,

		Branch:               r.Branch,
		WorktreePath:         r.WorktreePath,
		BaseSHA:              r.BaseSha,
		WorkspaceFingerprint: r.WorkspaceFingerprint,
		RuntimeHandleID:      r.RuntimeHandleID,
		RuntimeLaunchID:      r.RuntimeLaunchID,
		AgentSessionID:       r.AgentSessionID,
		// A pre-0134 row, and any row whose launch never happened, reads back
		// with a nil LaunchedAt rather than borrowing CreatedAt.
		LaunchedAt: nullTimeToPtr(r.LaunchedAt),
		CreatedAt:  r.CreatedAt,
	}
}

func workflowMutationProvenanceFromRow(r gen.WorkflowMutationProvenance) domain.WorkflowMutationProvenance {
	return domain.WorkflowMutationProvenance{
		ID:                r.ID,
		WorkflowRunID:     r.WorkflowRunID,
		WorkflowStepID:    nullStringToPtr(r.WorkflowStepID),
		AttemptID:         nullStringToPtr(r.AttemptID),
		TaskID:            r.TaskID,
		Class:             domain.WorkflowMutationClass(r.ProvenanceClass),
		Harness:           r.Harness,
		SessionID:         nullStringToPtr(r.SessionID),
		Branch:            r.Branch,
		WorktreePath:      r.WorktreePath,
		BaseSHA:           r.BaseSha,
		HeadSHA:           r.HeadSha,
		FingerprintBefore: r.FingerprintBefore,
		FingerprintAfter:  r.FingerprintAfter,
		Reason:            r.Reason,
		EvidenceJSON:      r.EvidenceJson,
		ObservedAt:        nullTimeToTimePtr(r.ObservedAt),
		CreatedAt:         r.CreatedAt,

		ProjectID:                  r.ProjectID,
		RepoIdentity:               domain.RepoIdentity(r.RepoIdentity),
		RepoPath:                   r.RepoPath,
		Placement:                  domain.WorkflowMutationPlacement(r.Placement),
		Boundary:                   domain.WorkflowMutationBoundary(r.Boundary),
		Generation:                 r.Generation,
		IntegrationTargetRef:       r.IntegrationTargetRef,
		IntegrationTargetBeforeSHA: r.IntegrationTargetBeforeSha,
		IntegrationTargetAfterSHA:  r.IntegrationTargetAfterSha,
		IntegrationMethod:          domain.WorkflowIntegrationMethod(r.IntegrationMethod),
		IdempotencyKey:             r.IdempotencyKey,
	}
}
