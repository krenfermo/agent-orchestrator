package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

type fakeStore struct {
	runs  map[string]domain.WorkflowRun
	steps map[string][]domain.WorkflowStep

	// attempts, checkpoints, and outbox back Checkpoint 8B's dispatch/
	// observation methods. Keyed the same way the real store's queries are.
	attempts map[string][]domain.WorkflowAttempt // by workflow_step_id
	// attemptMu makes the compare-and-swap below atomic under the concurrency
	// tests, which is the whole property it exists to model.
	attemptMu   sync.Mutex
	checkpoints map[string][]domain.WorkflowCheckpoint // by workflow_run_id, oldest first
	outbox      map[string]domain.WorkflowOutboxEntry  // by idempotency_key

	// healthEvents backs Checkpoint 8H's minimal agent health, append-only
	// per harness, oldest first (mirrors agent_health_events).
	healthEvents map[string][]domain.AgentHealthEvent
	// scopedHealthEvents backs Checkpoint 8P-C's per-(user,profile) health,
	// keyed by "harness|userID|profileID".
	scopedHealthEvents map[string][]domain.AgentHealthEvent
	// owners backs Checkpoint 8P-C's runOwner lookup (mirrors
	// GetWorkflowRunOwner). nil/missing entry means unowned.
	owners map[string]domain.UserID

	// dispatchCheckpoints and mutationProvenance back migration 0133's two
	// append-only tables (workflowcore.ProvenanceStore). Both are slices
	// because both are append-only in the real store, and "the sequence of
	// rows IS the history" is the property the readers depend on.
	dispatchCheckpoints []domain.WorkflowDispatchCheckpoint
	mutationProvenance  []domain.WorkflowMutationProvenance

	// dispatchWriteErr injects a storage failure into one dispatch-boundary
	// write, so a test can drive the two phases of the dispatch state machine
	// that are DEFINED by a failed durable write: an intent that cannot be
	// recorded (nothing is launched) and a confirmation that cannot be recorded
	// (launched, unconfirmed). Nil means every write succeeds.
	dispatchWriteErr func(domain.WorkflowDispatchCheckpoint) error

	// checkpointWriteErr injects a storage failure into one checkpoint write, so
	// a test can prove what does NOT happen when a durable record AO requires
	// cannot be written. Nil means every write succeeds.
	checkpointWriteErr func(domain.WorkflowCheckpoint) error

	// stepStateWriteErr injects a compare-and-swap failure before a workflow
	// step transition. Late-verdict recovery uses it to prove that a failed
	// transition cannot be mistaken for completed adoption.
	stepStateWriteErr func(id string, expected, next domain.WorkflowStepState) error

	// listStepsErr injects a storage failure into the step lookup, so a test
	// can prove what still happens when the bookkeeping AFTER a durable state
	// transition fails (Checkpoint 8P-E13A.1).
	listStepsErr error

	// listStepsErrFor injects a storage failure scoped to ONE run, so a test can
	// prove that a run AO cannot reconcile does not take every other run down
	// with it (the stale-runtime-pane regression: boot reconciliation used to
	// abort on the first failure and leave the rest of the fleet unrecovered).
	listStepsErrFor map[string]error

	// reviewRuns lets the fake honour ReleaseWorkflowStepReviewRunIfNoLateVerdict's
	// real condition (the run's late verdict) rather than pretending it away.
	// Nil means "no late verdicts exist", which is true for every fixture that
	// wires no review runs at all.
	reviewRuns *fakeReviewRuns

	// beforeCheckpointInsert and beforeReleaseReviewRun are deterministic
	// interleaving points. They run INSIDE the store call, immediately before it
	// takes effect, which is how the concurrency tests here reproduce an exact
	// racing order without a goroutine, a sleep or a timing assumption.
	beforeCheckpointInsert func(cp domain.WorkflowCheckpoint)
	// beforeStepStateWrite fires inside the AUTHORITY-GUARDED step transition,
	// immediately before it takes effect — the interleaving point at which a
	// replacement can steal the step from an adopter that is mid-flight.
	beforeStepStateWrite func(stepID string)
	// beforeOutboxCAS fires inside the outbox compare-and-swap, immediately
	// before it takes effect — the interleaving point at which a second
	// dispatcher can claim the pending dispatch out from under this one.
	beforeOutboxCAS func(id string, expected, next domain.WorkflowOutboxStatus)
	// checkpointListErr makes the durable ledger unreadable. Every decision that
	// depends on evidence must fail CLOSED when it is set: an unreadable ledger
	// is the absence of proof, never a substitute for it.
	checkpointListErr error
	// checkpointListErrAfter lets a test place the failure at a precise point in
	// a decision, so the branch under test is the one that meets it rather than
	// some earlier read that never gets that far.
	checkpointListErrAfter int
	checkpointListCalls    int
	// checkpointListErrOnce fails exactly ONE read. It isolates a single
	// decision's evidence lookup from the reads around it, so a fail-open bug in
	// one place cannot be masked by a fail-closed guard in the next.
	checkpointListErrOnce error
	// outboxCASErr fails the outbox compare-and-swap, modelling a crash between
	// a durable decision and the transition it authorises.
	outboxCASErr           error
	beforeReleaseReviewRun func(stepID, reviewRunID string)

	// checkpointClaims mirrors migration 0135's partial UNIQUE indexes on the
	// review authority claim and completed-adoption receipt phases.
	checkpointClaims map[string]bool
	// claimMu makes the claim check-and-insert atomic, which is the whole
	// property the single-winner tests exercise.
	claimMu sync.Mutex

	seq int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		runs:               map[string]domain.WorkflowRun{},
		steps:              map[string][]domain.WorkflowStep{},
		attempts:           map[string][]domain.WorkflowAttempt{},
		checkpoints:        map[string][]domain.WorkflowCheckpoint{},
		outbox:             map[string]domain.WorkflowOutboxEntry{},
		healthEvents:       map[string][]domain.AgentHealthEvent{},
		scopedHealthEvents: map[string][]domain.AgentHealthEvent{},
		owners:             map[string]domain.UserID{},
		checkpointClaims:   map[string]bool{},
	}
}

func (f *fakeStore) RecordAgentHealthEvent(_ context.Context, ev domain.AgentHealthEvent) (domain.AgentHealthEvent, error) {
	key := string(ev.Harness)
	f.healthEvents[key] = append(f.healthEvents[key], ev)
	if ev.UserID != "" && ev.ProviderProfileID != "" {
		scopedKey := string(ev.Harness) + "|" + string(ev.UserID) + "|" + string(ev.ProviderProfileID)
		f.scopedHealthEvents[scopedKey] = append(f.scopedHealthEvents[scopedKey], ev)
	}
	return ev, nil
}

func (f *fakeStore) GetAgentHealth(_ context.Context, harness domain.AgentHarness) (domain.AgentHealthEvent, bool, error) {
	list := f.healthEvents[string(harness)]
	if len(list) == 0 {
		return domain.AgentHealthEvent{}, false, nil
	}
	return list[len(list)-1], true, nil
}

func (f *fakeStore) GetAgentHealthScoped(_ context.Context, harness domain.AgentHarness, userID domain.UserID, profileID domain.ProviderProfileID) (domain.AgentHealthEvent, bool, error) {
	key := string(harness) + "|" + string(userID) + "|" + string(profileID)
	list := f.scopedHealthEvents[key]
	if len(list) == 0 {
		return domain.AgentHealthEvent{}, false, nil
	}
	return list[len(list)-1], true, nil
}

func (f *fakeStore) GetWorkflowRunOwner(_ context.Context, id string) (*domain.UserID, error) {
	owner, ok := f.owners[id]
	if !ok {
		return nil, nil
	}
	return &owner, nil
}

func (f *fakeStore) SetWorkflowRunOwner(_ context.Context, id string, owner domain.UserID) (bool, error) {
	if _, ok := f.runs[id]; !ok {
		return false, nil
	}
	f.owners[id] = owner
	return true, nil
}

func (f *fakeStore) UpdateWorkflowRunPolicySnapshot(_ context.Context, id, policySnapshot string, now time.Time) (bool, error) {
	run, ok := f.runs[id]
	if !ok {
		return false, nil
	}
	run.PolicySnapshot = policySnapshot
	run.UpdatedAt = now
	f.runs[id] = run
	return true, nil
}

func (f *fakeStore) CreateWorkflowRun(_ context.Context, run domain.WorkflowRun, steps []domain.WorkflowStep) (domain.WorkflowRun, []domain.WorkflowStep, error) {
	f.runs[run.ID] = run
	f.steps[run.ID] = append([]domain.WorkflowStep{}, steps...)
	return run, steps, nil
}

func (f *fakeStore) GetWorkflowRun(_ context.Context, id string) (domain.WorkflowRun, bool, error) {
	run, ok := f.runs[id]
	return run, ok, nil
}

func (f *fakeStore) ListWorkflowRuns(_ context.Context, projectID string) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if projectID == "" || run.ProjectID == projectID {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeStore) ListNonTerminalWorkflowRuns(_ context.Context) ([]domain.WorkflowRun, error) {
	var out []domain.WorkflowRun
	for _, run := range f.runs {
		if !run.State.Terminal() {
			out = append(out, run)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateWorkflowRunState(_ context.Context, id string, expected, next domain.WorkflowRunState, now time.Time) (bool, error) {
	run, ok := f.runs[id]
	if !ok || run.State != expected || !domain.ValidWorkflowRunTransition(expected, next) {
		return false, nil
	}
	run.State = next
	run.UpdatedAt = now
	if next == domain.WorkflowRunCompleted {
		run.CompletedAt = &now
	}
	if next == domain.WorkflowRunCancelled {
		run.CancelledAt = &now
	}
	f.runs[id] = run
	return true, nil
}

func (f *fakeStore) ListWorkflowSteps(_ context.Context, runID string) ([]domain.WorkflowStep, error) {
	if f.listStepsErr != nil {
		return nil, f.listStepsErr
	}
	if err := f.listStepsErrFor[runID]; err != nil {
		return nil, err
	}
	return append([]domain.WorkflowStep{}, f.steps[runID]...), nil
}

func (f *fakeStore) UpdateWorkflowStepState(_ context.Context, id string, expected, next domain.WorkflowStepState, now time.Time) (bool, error) {
	if f.stepStateWriteErr != nil {
		if err := f.stepStateWriteErr(id, expected, next); err != nil {
			return false, err
		}
	}
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != id {
				continue
			}
			if step.State != expected || !domain.ValidWorkflowStepTransition(expected, next) {
				return false, nil
			}
			step.State = next
			step.UpdatedAt = now
			if next.Terminal() {
				step.CompletedAt = &now
			}
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

// ReopenFailedWorkflowStep mirrors the real store's compare-and-swap: expected
// state pinned to `failed`, completed_at cleared, false when no row matched.
func (f *fakeStore) ReopenFailedWorkflowStep(_ context.Context, stepID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.State != domain.WorkflowStepFailed {
				return false, nil
			}
			step.State = domain.WorkflowStepReady
			step.UpdatedAt = now
			step.CompletedAt = nil
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

// ReopenCompletedWorkflowStep mirrors the real store's compare-and-swap:
// expected state pinned to `completed`, next state `waiting`, completed_at
// cleared, false when no row matched.
func (f *fakeStore) ReopenCompletedWorkflowStep(_ context.Context, stepID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.State != domain.WorkflowStepCompleted {
				return false, nil
			}
			step.State = domain.WorkflowStepWaiting
			step.UpdatedAt = now
			step.CompletedAt = nil
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) ListWorkflowAttempts(_ context.Context, stepID string) ([]domain.WorkflowAttempt, error) {
	return append([]domain.WorkflowAttempt{}, f.attempts[stepID]...), nil
}

func (f *fakeStore) UpdateWorkflowStepArtifact(_ context.Context, stepID, artifactJSON string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			step.ArtifactJSON = artifactJSON
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) SetWorkflowStepReviewRun(_ context.Context, stepID, reviewRunID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			rid := reviewRunID
			step.ReviewRunID = &rid
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) UpdateWorkflowStepSession(_ context.Context, stepID, sessionID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i, step := range steps {
			if step.ID != stepID {
				continue
			}
			if step.SessionID != nil {
				return false, nil
			}
			sid := sessionID
			step.SessionID = &sid
			step.UpdatedAt = now
			f.steps[runID][i] = step
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) CreateWorkflowAttempt(_ context.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, error) {
	f.seq++
	attempt := domain.WorkflowAttempt{
		ID:             id,
		WorkflowStepID: stepID,
		AttemptNumber:  int64(len(f.attempts[stepID]) + 1),
		Harness:        harness,
		Model:          model,
		StartedAt:      startedAt,
	}
	f.attempts[stepID] = append(f.attempts[stepID], attempt)
	return attempt, nil
}

// ClaimOpenWorkflowAttempt mirrors the store's atomic read-then-insert: the
// step's open attempt if it has one, a new row otherwise.
func (f *fakeStore) ClaimOpenWorkflowAttempt(ctx context.Context, id, stepID, harness, model string, startedAt time.Time) (domain.WorkflowAttempt, bool, error) {
	f.attemptMu.Lock()
	defer f.attemptMu.Unlock()
	list := f.attempts[stepID]
	if len(list) > 0 && list[len(list)-1].Outcome == "" {
		return list[len(list)-1], false, nil
	}
	attempt, err := f.CreateWorkflowAttempt(ctx, id, stepID, harness, model, startedAt)
	return attempt, err == nil, err
}

// StartWorkflowStepForSession mirrors the store's session-predicated
// ready -> running transition.
func (f *fakeStore) StartWorkflowStepForSession(ctx context.Context, id, sessionID string, now time.Time) (bool, error) {
	for runID, steps := range f.steps {
		for i := range steps {
			if steps[i].ID != id {
				continue
			}
			if steps[i].State != domain.WorkflowStepReady ||
				steps[i].SessionID == nil || *steps[i].SessionID != sessionID {
				return false, nil
			}
			return f.UpdateWorkflowStepState(ctx, id, domain.WorkflowStepReady, domain.WorkflowStepRunning, now)
		}
		_ = runID
	}
	return false, nil
}

// AcknowledgeWorkflowOutboxDispatch mirrors the store's generation-fenced
// dispatched -> acknowledged transition.
func (f *fakeStore) AcknowledgeWorkflowOutboxDispatch(_ context.Context, id string, expected domain.WorkflowOutboxStatus, now time.Time, dispatchGeneration string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, expected, domain.WorkflowOutboxAcknowledged)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != expected || entry.DispatchGeneration != dispatchGeneration {
			return false, nil
		}
		t := now
		entry.Status = domain.WorkflowOutboxAcknowledged
		entry.AcknowledgedAt = &t
		entry.ErrorClass = ""
		entry.FailureGeneration = ""
		entry.DispatchGeneration = ""
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

func (f *fakeStore) GetLatestWorkflowAttempt(_ context.Context, stepID string) (domain.WorkflowAttempt, bool, error) {
	list := f.attempts[stepID]
	if len(list) == 0 {
		return domain.WorkflowAttempt{}, false, nil
	}
	return list[len(list)-1], true, nil
}

// ClaimWorkflowAttemptOutcome is the fake's compare-and-swap: it concludes the
// attempt only when nothing has, which is what lets a test run two verify
// executions against one attempt and assert that exactly one of them may act.
func (f *fakeStore) ClaimWorkflowAttemptOutcome(_ context.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) (bool, error) {
	f.attemptMu.Lock()
	defer f.attemptMu.Unlock()
	for stepID, list := range f.attempts {
		for i, a := range list {
			if a.ID != attemptID {
				continue
			}
			if a.FinishedAt != nil {
				return false, nil
			}
			t := finishedAt
			a.FinishedAt = &t
			a.Outcome = outcome
			a.ErrorClass = errorClass
			f.attempts[stepID][i] = a
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeStore) UpdateWorkflowAttemptOutcome(_ context.Context, attemptID string, finishedAt time.Time, outcome domain.WorkflowAttemptOutcome, errorClass domain.WorkflowErrorClass) error {
	for stepID, list := range f.attempts {
		for i, a := range list {
			if a.ID != attemptID {
				continue
			}
			if !finishedAt.IsZero() {
				t := finishedAt
				a.FinishedAt = &t
			} else {
				a.FinishedAt = nil
			}
			a.Outcome = outcome
			a.ErrorClass = errorClass
			f.attempts[stepID][i] = a
			return nil
		}
	}
	return nil
}

func (f *fakeStore) CreateWorkflowCheckpoint(_ context.Context, cp domain.WorkflowCheckpoint) (domain.WorkflowCheckpoint, error) {
	if f.checkpointWriteErr != nil {
		if err := f.checkpointWriteErr(cp); err != nil {
			return domain.WorkflowCheckpoint{}, err
		}
	}
	if hook := f.beforeCheckpointInsert; hook != nil {
		// Deterministic interleaving point: a competing writer runs HERE, after
		// this caller decided to insert and before the insert takes effect.
		// Cleared first so a hook that re-enters the same path cannot recurse.
		f.beforeCheckpointInsert = nil
		hook(cp)
	}
	// Migration 0135's partial UNIQUE indexes, modelled.
	if key, claimed := checkpointClaimKey(cp); claimed {
		f.claimMu.Lock()
		if f.checkpointClaims[key] {
			f.claimMu.Unlock()
			return domain.WorkflowCheckpoint{}, fmt.Errorf(
				"fake store: %w", domain.ErrDuplicateWorkflowCheckpoint)
		}
		f.checkpointClaims[key] = true
		f.claimMu.Unlock()
	}
	f.claimMu.Lock()
	f.checkpoints[cp.WorkflowRunID] = append(f.checkpoints[cp.WorkflowRunID], cp)
	f.claimMu.Unlock()
	return cp, nil
}

// checkpointClaimKey returns the uniqueness key for the one constrained
// checkpoint phase, and whether this row is constrained at all.
func checkpointClaimKey(cp domain.WorkflowCheckpoint) (string, bool) {
	// Migration 0136: the human budget reset is single-winner per (step, epoch),
	// with the epoch carried in head_sha.
	if cp.DurablePhase == "reviewer_launch_human_retry" {
		if cp.WorkflowStepID == nil || cp.HeadSHA == "" {
			return "", false
		}
		return cp.DurablePhase + "|" + *cp.WorkflowStepID + "|" + cp.HeadSHA, true
	}
	if (cp.DurablePhase != "review_authority_rebind" &&
		cp.DurablePhase != "review_late_verdict_adopted") ||
		cp.WorkflowStepID == nil || cp.ReviewRunID == nil {
		return "", false
	}
	return cp.DurablePhase + "|" + *cp.WorkflowStepID + "|" + *cp.ReviewRunID, true
}

// RebindWorkflowStepReviewRunFrom mirrors the real compare-and-swap: the pointer
// moves only when it currently holds exactly `expected` ("" meaning unset).
func (f *fakeStore) RebindWorkflowStepReviewRunFrom(
	_ context.Context, stepID, expected, predecessor, next string, now time.Time,
) (bool, error) {
	f.claimMu.Lock()
	defer f.claimMu.Unlock()
	// Mirrors the real statement's third guard: a run carrying a LATE verdict may
	// not be replaced out from under the outcome adoption is applying for it. An
	// on-time verdict is the ordinary cycle and is replaceable.
	if predecessor != "" && f.reviewRuns != nil {
		if r, ok := f.reviewRuns.runs[predecessor]; ok && r.LateVerdict.Valid() {
			return false, nil
		}
	}
	for runID, steps := range f.steps {
		for i := range steps {
			if steps[i].ID != stepID {
				continue
			}
			if steps[i].State.Terminal() {
				// A resolved review step is not replaceable, whatever its
				// pointer says.
				return false, nil
			}
			cur := ""
			if steps[i].ReviewRunID != nil {
				cur = *steps[i].ReviewRunID
			}
			if cur != expected {
				return false, nil
			}
			if next == "" {
				steps[i].ReviewRunID = nil
			} else {
				id := next
				steps[i].ReviewRunID = &id
			}
			steps[i].UpdatedAt = now
			f.steps[runID] = steps
			return true, nil
		}
	}
	return false, nil
}

// UpdateWorkflowStepStateIfReviewRun mirrors the real single-statement guarded
// transition: state AND authority pointer are checked with the write, so the
// fake cannot admit an interleaving the database would refuse.
func (f *fakeStore) UpdateWorkflowStepStateIfReviewRun(
	_ context.Context, stepID string, expected, next domain.WorkflowStepState,
	reviewRunID string, now time.Time,
) (bool, error) {
	if !domain.ValidWorkflowStepTransition(expected, next) {
		return false, nil
	}
	if hook := f.beforeStepStateWrite; hook != nil {
		f.beforeStepStateWrite = nil
		hook(stepID)
	}
	// Same injected-failure surface as the unguarded writer: adoption now uses
	// THIS method, so a test that injects a transition failure must still see it.
	if f.stepStateWriteErr != nil {
		if err := f.stepStateWriteErr(stepID, expected, next); err != nil {
			return false, err
		}
	}
	f.claimMu.Lock()
	defer f.claimMu.Unlock()
	for runID, steps := range f.steps {
		for i := range steps {
			if steps[i].ID != stepID {
				continue
			}
			if steps[i].State != expected ||
				steps[i].ReviewRunID == nil || *steps[i].ReviewRunID != reviewRunID {
				return false, nil
			}
			steps[i].State = next
			steps[i].UpdatedAt = now
			if next.Terminal() {
				t := now
				steps[i].CompletedAt = &t
			}
			f.steps[runID] = steps
			return true, nil
		}
	}
	return false, nil
}

// ReleaseWorkflowStepReviewRunIfNoLateVerdict mirrors the real single-statement
// UPDATE: the pointer is cleared only while it still names reviewRunID AND that
// run has no late verdict. The two conditions are evaluated under one lock, so
// the fake cannot admit an interleaving the database would refuse.
func (f *fakeStore) ReleaseWorkflowStepReviewRunIfNoLateVerdict(
	_ context.Context, stepID, reviewRunID string, now time.Time,
) (bool, error) {
	if hook := f.beforeReleaseReviewRun; hook != nil {
		f.beforeReleaseReviewRun = nil
		hook(stepID, reviewRunID)
	}
	f.claimMu.Lock()
	defer f.claimMu.Unlock()
	if f.reviewRuns != nil {
		if r, ok := f.reviewRuns.runs[reviewRunID]; ok && r.LateVerdict.Valid() {
			return false, nil
		}
	}
	for runID, steps := range f.steps {
		for i := range steps {
			if steps[i].ID != stepID {
				continue
			}
			if steps[i].ReviewRunID == nil || *steps[i].ReviewRunID != reviewRunID {
				return false, nil
			}
			steps[i].ReviewRunID = nil
			steps[i].UpdatedAt = now
			f.steps[runID] = steps
			return true, nil
		}
	}
	return false, nil
}

// ListWorkflowRunIDsByCheckpointPhase mirrors the real query: runs carrying a
// checkpoint of this phase, terminal ones included.
func (f *fakeStore) ListWorkflowRunIDsByCheckpointPhase(_ context.Context, phase string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for runID, cps := range f.checkpoints {
		for _, cp := range cps {
			if cp.DurablePhase == phase && !seen[runID] {
				seen[runID] = true
				out = append(out, runID)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeStore) ListWorkflowCheckpoints(_ context.Context, runID string) ([]domain.WorkflowCheckpoint, error) {
	f.checkpointListCalls++
	if f.checkpointListErrOnce != nil {
		err := f.checkpointListErrOnce
		f.checkpointListErrOnce = nil
		return nil, err
	}
	if f.checkpointListErr != nil && f.checkpointListCalls > f.checkpointListErrAfter {
		return nil, f.checkpointListErr
	}
	return append([]domain.WorkflowCheckpoint{}, f.checkpoints[runID]...), nil
}

func (f *fakeStore) GetLatestWorkflowCheckpointByStep(_ context.Context, stepID string) (domain.WorkflowCheckpoint, bool, error) {
	var latest domain.WorkflowCheckpoint
	found := false
	for _, list := range f.checkpoints {
		for _, cp := range list {
			if cp.WorkflowStepID == nil || *cp.WorkflowStepID != stepID {
				continue
			}
			if !found || cp.CreatedAt.After(latest.CreatedAt) {
				latest = cp
				found = true
			}
		}
	}
	return latest, found, nil
}

func (f *fakeStore) EnqueueWorkflowOutboxEntry(_ context.Context, entry domain.WorkflowOutboxEntry) (domain.WorkflowOutboxEntry, bool, error) {
	if existing, ok := f.outbox[entry.IdempotencyKey]; ok {
		return existing, false, nil
	}
	entry.Status = domain.WorkflowOutboxPending
	f.outbox[entry.IdempotencyKey] = entry
	return entry, true, nil
}

func (f *fakeStore) ListWorkflowOutboxByRun(_ context.Context, runID string) ([]domain.WorkflowOutboxEntry, error) {
	out := []domain.WorkflowOutboxEntry{}
	for _, entry := range f.outbox {
		if entry.WorkflowRunID == runID {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeStore) UpdateWorkflowOutboxStatus(_ context.Context, id string, expected, next domain.WorkflowOutboxStatus, now time.Time, errorClass string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, expected, next)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != expected {
			return false, nil
		}
		entry.Status = next
		switch next {
		case domain.WorkflowOutboxDispatched:
			t := now
			entry.DispatchedAt = &t
		case domain.WorkflowOutboxAcknowledged:
			t := now
			entry.AcknowledgedAt = &t
		case domain.WorkflowOutboxFailed:
			t := now
			entry.FailedAt = &t
		}
		entry.ErrorClass = errorClass
		// Modelled exactly as the SQL does it: any transition through here
		// clears BOTH tokens, so neither a failure identity nor a claim can be
		// inherited by a state it does not describe.
		entry.FailureGeneration = ""
		entry.DispatchGeneration = ""
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

// FailWorkflowOutboxWithGeneration models the generation-stamping failure: the
// status change and the stamp are one operation, never two.
// ClaimWorkflowOutboxDispatch models the ownership claim: pending ->
// dispatched, stamping the token of the dispatch that took it.
func (f *fakeStore) ClaimWorkflowOutboxDispatch(_ context.Context, id string, now time.Time, dispatchGeneration string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, domain.WorkflowOutboxPending, domain.WorkflowOutboxDispatched)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != domain.WorkflowOutboxPending {
			return false, nil
		}
		t := now
		entry.Status = domain.WorkflowOutboxDispatched
		entry.DispatchedAt = &t
		entry.ErrorClass = ""
		entry.FailureGeneration = ""
		entry.DispatchGeneration = dispatchGeneration
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

// ReleaseDispatchedWorkflowOutboxGeneration models the ownership-conditioned
// release: dispatched -> pending for the EXACT claim that holds the row.
func (f *fakeStore) ReleaseDispatchedWorkflowOutboxGeneration(_ context.Context, id, errorClass, dispatchGeneration string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, domain.WorkflowOutboxDispatched, domain.WorkflowOutboxPending)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != domain.WorkflowOutboxDispatched || entry.DispatchGeneration != dispatchGeneration {
			return false, nil
		}
		entry.Status = domain.WorkflowOutboxPending
		entry.DispatchedAt = nil
		entry.ErrorClass = errorClass
		entry.FailureGeneration = ""
		entry.DispatchGeneration = ""
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

func (f *fakeStore) FailWorkflowOutboxWithGeneration(_ context.Context, id string, expected domain.WorkflowOutboxStatus, now time.Time, errorClass, generation, dispatchGeneration string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, expected, domain.WorkflowOutboxFailed)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		// The ownership half of the predicate, exactly as in the SQL: a caller
		// that no longer holds the claim changes nothing.
		if entry.Status != expected || entry.DispatchGeneration != dispatchGeneration {
			return false, nil
		}
		t := now
		entry.Status = domain.WorkflowOutboxFailed
		entry.FailedAt = &t
		entry.ErrorClass = errorClass
		entry.FailureGeneration = generation
		entry.DispatchGeneration = ""
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

// ReopenFailedWorkflowOutboxGeneration models the generation-conditioned CAS.
// The generation is part of the predicate, exactly as in the SQL: a row that
// failed again under the same id and status does NOT match.
func (f *fakeStore) ReopenFailedWorkflowOutboxGeneration(_ context.Context, id, errorClass, generation string) (bool, error) {
	if f.outboxCASErr != nil {
		return false, f.outboxCASErr
	}
	if hook := f.beforeOutboxCAS; hook != nil {
		f.beforeOutboxCAS = nil
		hook(id, domain.WorkflowOutboxFailed, domain.WorkflowOutboxPending)
	}
	for key, entry := range f.outbox {
		if entry.ID != id {
			continue
		}
		if entry.Status != domain.WorkflowOutboxFailed || entry.FailureGeneration != generation {
			return false, nil
		}
		entry.Status = domain.WorkflowOutboxPending
		entry.FailedAt = nil
		entry.ErrorClass = errorClass
		entry.FailureGeneration = ""
		f.outbox[key] = entry
		return true, nil
	}
	return false, nil
}

func newCoordinator() (*workflowcore.Coordinator, *fakeStore) {
	store := newFakeStore()
	clock := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store,
		Clock: func() time.Time { return clock },
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store
}

func TestCreateRunSeedsSixLinearSteps(t *testing.T) {
	c, _ := newCoordinator()
	ctx := context.Background()

	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if detail.Run.State != domain.WorkflowRunPending {
		t.Fatalf("run state = %q, want pending", detail.Run.State)
	}
	if len(detail.Steps) != 6 {
		t.Fatalf("steps = %d, want 6", len(detail.Steps))
	}
	wantKinds := []domain.WorkflowStepKind{
		domain.WorkflowStepPlan, domain.WorkflowStepWork, domain.WorkflowStepReview,
		domain.WorkflowStepFix, domain.WorkflowStepVerify, domain.WorkflowStepAdvance,
	}
	for i, sd := range detail.Steps {
		if sd.Step.Kind != wantKinds[i] {
			t.Errorf("step %d kind = %q, want %q", i, sd.Step.Kind, wantKinds[i])
		}
		if sd.Step.Ordinal != int64(i+1) {
			t.Errorf("step %d ordinal = %d, want %d", i, sd.Step.Ordinal, i+1)
		}
		if i == 0 {
			if sd.Step.State != domain.WorkflowStepReady {
				t.Errorf("first step state = %q, want ready", sd.Step.State)
			}
			if sd.Step.DependsOnStepID != nil {
				t.Errorf("first step depends_on = %v, want nil", sd.Step.DependsOnStepID)
			}
			continue
		}
		if sd.Step.State != domain.WorkflowStepPending {
			t.Errorf("step %d state = %q, want pending", i, sd.Step.State)
		}
		if sd.Step.DependsOnStepID == nil || *sd.Step.DependsOnStepID != detail.Steps[i-1].Step.ID {
			t.Errorf("step %d depends_on = %v, want %q", i, sd.Step.DependsOnStepID, detail.Steps[i-1].Step.ID)
		}
	}
}

func TestCreateRunRejectsEmptyObjective(t *testing.T) {
	c, _ := newCoordinator()
	if _, err := c.CreateRun(context.Background(), "proj-1", ""); !errors.Is(err, workflowcore.ErrInvalid) {
		t.Fatalf("CreateRun with empty objective: err=%v, want ErrInvalid", err)
	}
}

func TestCancelRunCascadesToNonTerminalSteps(t *testing.T) {
	c, store := newCoordinator()
	ctx := context.Background()
	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID

	cancelled, err := c.CancelRun(ctx, runID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", cancelled.Run.State)
	}
	for _, sd := range cancelled.Steps {
		if sd.Step.State != domain.WorkflowStepCancelled {
			t.Errorf("step %q state = %q, want cancelled", sd.Step.ID, sd.Step.State)
		}
	}
	_ = store
}

func TestCancelRunOnAlreadyTerminalRunIsRejectedNotSilent(t *testing.T) {
	c, store := newCoordinator()
	ctx := context.Background()
	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID

	// Force the run straight to completed (bypassing the coordinator, as a
	// completed run would be in production once execution exists).
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now()); err != nil {
		t.Fatalf("force running: %v", err)
	}
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunRunning, domain.WorkflowRunCompleted, time.Now()); err != nil {
		t.Fatalf("force completed: %v", err)
	}

	if _, err := c.CancelRun(ctx, runID); !errors.Is(err, workflowcore.ErrAlreadyTerminal) {
		t.Fatalf("CancelRun on completed run: err=%v, want ErrAlreadyTerminal", err)
	}
	// The run must not have been mutated back toward running.
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state after rejected cancel = %q, want completed (unchanged)", got.Run.State)
	}
}

func TestListRunsFiltersByProject(t *testing.T) {
	c, _ := newCoordinator()
	ctx := context.Background()
	if _, err := c.CreateRun(ctx, "proj-a", "a"); err != nil {
		t.Fatalf("create a: %v", err)
	}

	runs, err := c.ListRuns(ctx, "proj-a")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ProjectID != "proj-a" {
		t.Fatalf("runs = %+v", runs)
	}

	empty, err := c.ListRuns(ctx, "proj-nonexistent")
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("runs for unrelated project = %+v, want empty", empty)
	}
}
