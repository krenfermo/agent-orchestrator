package workflow_test

// The terminal-session policy: which runtimes a finished workflow ends, and
// which it must leave exactly where they are.
//
// These tests exercise the POLICY only. The proofs it delegates to — ownership,
// incarnation, capacity authority, ABA — belong to runtimegc and are tested
// there against a fake runtime and, for the real thing, against a real tmux
// server. The split is deliberate: this file must be readable as a statement of
// what AO will and will not kill.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// recordingReclaimer captures every reclamation the policy asked for.
type recordingReclaimer struct {
	requests []workflowcore.TerminalRuntimeRequest
	err      error
	// refuse, when set, answers "not reclaimed" with a reason, modelling a
	// proof the sweeper declined to grant.
	refuse string
}

func (r *recordingReclaimer) ReclaimSessionRuntime(
	_ context.Context, req workflowcore.TerminalRuntimeRequest,
) (bool, string, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return false, "", r.err
	}
	if r.refuse != "" {
		return false, r.refuse, nil
	}
	return true, "ended", nil
}

func (r *recordingReclaimer) sessions() []string {
	out := make([]string, 0, len(r.requests))
	for _, req := range r.requests {
		out = append(out, string(req.SessionID))
	}
	return out
}

// ownedWorkerSession is a P1-D session record: AO created this exact
// incarnation for this launch, and the recorded token proves it.
func ownedWorkerSession(id, instance, launch string, terminated bool) domain.SessionRecord {
	return domain.SessionRecord{
		ID: domain.SessionID(id), ProjectID: "agent-orchestrator", IsTerminated: terminated,
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   id,
			RuntimeInstanceID: instance,
			RuntimeLaunchID:   launch,
			RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(domain.SessionID(id), launch),
		},
	}
}

// terminalPolicyFixture builds a run in `state` whose work step holds
// `session`, and returns the coordinator, the reclaimer and the run id.
func terminalPolicyFixture(
	t *testing.T, state domain.WorkflowRunState, sessions ...domain.SessionRecord,
) (*workflowcore.Coordinator, *fakeStore, *recordingReclaimer, string) {
	t.Helper()
	store := newFakeStore()
	now := time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC)
	runID := "wf-terminal"

	facts := newFakeSessionFacts()
	steps := []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}"},
	}
	for i, rec := range sessions {
		facts.put(rec)
		id := string(rec.ID)
		steps = append(steps, domain.WorkflowStep{
			ID: "work-" + id, WorkflowRunID: runID, Kind: domain.WorkflowStepWork,
			Ordinal: int64(i + 2), State: domain.WorkflowStepCompleted, SessionID: &id,
		})
	}
	store.steps[runID] = steps
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "agent-orchestrator", Objective: "terminal policy",
		State: state, PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}

	reclaimer := &recordingReclaimer{}
	clk := &fakeClock{t: now}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, SessionFacts: facts, TerminalRuntimes: reclaimer, Clock: clk.Now,
		NewID: func() string { return "tr" },
	})
	return c, store, reclaimer, runID
}

// reconcile drives the canonical reconciliation path a long-lived daemon uses.
func reconcile(t *testing.T, c *workflowcore.Coordinator, runID string) {
	t.Helper()
	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
}

// ---- 1-3: terminal states end the runtime ----------------------------------

func TestCompletedRunEndsItsOwnedWorkerRuntime(t *testing.T) {
	rec := ownedWorkerSession("agent-orchestrator-51", "$42", "launch-1", false)
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted, rec)

	reconcile(t, c, runID)

	if got := reclaimer.sessions(); len(got) != 1 || got[0] != "agent-orchestrator-51" {
		t.Fatalf("reclaimed %v, want exactly the completed run's worker", got)
	}
	// The identity handed down must be the recorded one, unmodified: the
	// incarnation is what the destroy is addressed to, and the token is what
	// authorizes it.
	req := reclaimer.requests[0]
	if req.InstanceID != "$42" || req.OwnerToken != rec.Metadata.RuntimeOwnerToken || req.LaunchID != "launch-1" {
		t.Fatalf("request = %+v, want the session's recorded runtime identity", req)
	}
	if req.WorkflowRunID != runID {
		t.Fatalf("request run = %q, want %q", req.WorkflowRunID, runID)
	}
}

func TestCancelledRunEndsItsOwnedWorkerRuntime(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCancelled,
		ownedWorkerSession("agent-orchestrator-52", "$43", "launch-1", false))

	reconcile(t, c, runID)

	if got := reclaimer.sessions(); len(got) != 1 {
		t.Fatalf("reclaimed %v, want the cancelled run's worker", got)
	}
}

func TestTerminallyFailedRunEndsItsOwnedWorkerRuntime(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunFailed,
		ownedWorkerSession("agent-orchestrator-53", "$44", "launch-1", false))

	reconcile(t, c, runID)

	if got := reclaimer.sessions(); len(got) != 1 {
		t.Fatalf("reclaimed %v, want the failed run's worker; a terminal failure has no bounded recovery that reuses it", got)
	}
}

// ---- 4: needs_attention preserves the runtime -------------------------------

// The single most important refusal in this file. A parked run is not a
// finished one: Resume, Repair and fix re-delivery all reuse the SAME session,
// so ending its runtime would turn a recoverable stop into an unrecoverable
// one.
func TestNeedsAttentionRunNeverEndsItsRuntimeAutomatically(t *testing.T) {
	c, store, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunNeedsAttention,
		ownedWorkerSession("agent-orchestrator-54", "$45", "launch-1", false))
	// Park it on a real, self-remediable reason, which is the case most likely
	// to want its session back.
	stepID := "work-agent-orchestrator-54"
	store.checkpoints[runID] = append(store.checkpoints[runID], domain.WorkflowCheckpoint{
		ID: "stop", WorkflowRunID: runID, WorkflowStepID: &stepID, ProjectID: "agent-orchestrator",
		DurablePhase: workflowcore.ReasonVerifyBudgetExhausted, RetryState: "{}",
		CreatedAt: time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC),
	})

	reconcile(t, c, runID)

	if got := reclaimer.sessions(); len(got) != 0 {
		t.Fatalf("ended %v for a run parked at needs_attention; its runtime may still be required for resume or repair", got)
	}
}

// Every attention reason, not just the convenient one. If AO ever learns which
// recoveries open a fresh generation, this test is where that becomes explicit
// rather than incidental.
func TestEveryAttentionReasonPreservesTheRuntime(t *testing.T) {
	for _, reason := range []string{
		workflowcore.ReasonVerifyBudgetExhausted,
		workflowcore.ReasonVerifyUnrepairable,
		workflowcore.ReasonVerifyFixUnavailable,
		workflowcore.ReasonFixBudgetExhausted,
		workflowcore.ReasonVerifyFixReentry,
		"dirty_worktree",
		"",
	} {
		t.Run(reason, func(t *testing.T) {
			c, store, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunNeedsAttention,
				ownedWorkerSession("agent-orchestrator-55", "$46", "launch-1", false))
			if reason != "" {
				stepID := "work-agent-orchestrator-55"
				store.checkpoints[runID] = append(store.checkpoints[runID], domain.WorkflowCheckpoint{
					ID: "stop", WorkflowRunID: runID, WorkflowStepID: &stepID, ProjectID: "agent-orchestrator",
					DurablePhase: reason, RetryState: "{}",
					CreatedAt: time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC),
				})
			}
			reconcile(t, c, runID)
			if got := reclaimer.sessions(); len(got) != 0 {
				t.Fatalf("ended %v for needs_attention/%q", got, reason)
			}
		})
	}
}

// ---- non-terminal states -----------------------------------------------------

func TestNonTerminalRunsNeverEndTheirRuntimes(t *testing.T) {
	for _, state := range []domain.WorkflowRunState{
		domain.WorkflowRunRunning, domain.WorkflowRunWaiting, domain.WorkflowRunPending,
	} {
		t.Run(string(state), func(t *testing.T) {
			c, _, reclaimer, runID := terminalPolicyFixture(t, state,
				ownedWorkerSession("agent-orchestrator-56", "$47", "launch-1", false))
			reconcile(t, c, runID)
			if got := reclaimer.sessions(); len(got) != 0 {
				t.Fatalf("ended %v for a %s run", got, state)
			}
		})
	}
}

// ---- 5-6: unknown and legacy ownership ---------------------------------------

// A session whose runtime identity was never recorded is not AO's to end. The
// policy still OFFERS it, because the proof belongs to the reclaimer and
// splitting a fail-closed rule across two layers is how one half forgets it —
// but the request carries the empty identity that makes the refusal certain,
// and the reclaimer's own tests pin that refusal.
func TestLegacySessionOffersNoOwnershipProof(t *testing.T) {
	legacy := domain.SessionRecord{
		ID: "agent-orchestrator-40", ProjectID: "agent-orchestrator",
		Metadata: domain.SessionMetadata{RuntimeHandleID: "agent-orchestrator-40"},
	}
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted, legacy)

	reconcile(t, c, runID)

	if len(reclaimer.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(reclaimer.requests))
	}
	req := reclaimer.requests[0]
	if req.InstanceID != "" || req.OwnerToken != "" {
		t.Fatalf("request = %+v, want the empty identity a legacy row actually holds; AO must never invent one", req)
	}
}

// ---- 11-12: idempotence and absence ------------------------------------------

// A session already recorded as terminated is not offered again, however many
// times the run is reconciled. This is what keeps a terminal run's every poll
// from being a destroy attempt.
func TestTerminalReclamationSkipsAlreadyTerminatedSessions(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted,
		ownedWorkerSession("agent-orchestrator-57", "$48", "launch-1", true))

	for i := 0; i < 3; i++ {
		reconcile(t, c, runID)
	}
	if got := reclaimer.sessions(); len(got) != 0 {
		t.Fatalf("offered %v for a session already recorded as terminated", got)
	}
}

// A session row that no longer exists converges to doing nothing — never to
// going and looking for the NAME on the runtime, which is the ABA-unsafe move
// the whole model excludes.
func TestTerminalReclamationConvergesWhenTheSessionRowIsGone(t *testing.T) {
	c, store, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted)
	missing := "agent-orchestrator-99"
	store.steps[runID] = append(store.steps[runID], domain.WorkflowStep{
		ID: "work-missing", WorkflowRunID: runID, Kind: domain.WorkflowStepWork,
		Ordinal: 9, State: domain.WorkflowStepCompleted, SessionID: &missing,
	})

	reconcile(t, c, runID)

	if got := reclaimer.sessions(); len(got) != 0 {
		t.Fatalf("offered %v for a session with no durable row", got)
	}
}

// ---- 15: one broken candidate does not block the others ----------------------

func TestOneFailingReclamationDoesNotBlockTheRest(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted,
		ownedWorkerSession("agent-orchestrator-60", "$60", "launch-1", false),
		ownedWorkerSession("agent-orchestrator-61", "$61", "launch-1", false),
		ownedWorkerSession("agent-orchestrator-62", "$62", "launch-1", false),
	)
	reclaimer.err = errors.New("the runtime would not answer")

	reconcile(t, c, runID)

	if got := len(reclaimer.requests); got != 3 {
		t.Fatalf("attempted %d reclamations, want all 3: one failure must not abort the run's other runtimes", got)
	}
}

// A refusal is not a failure, and it is not retried into a loop either.
func TestRefusedReclamationIsRecordedAndNotRetriedWithinAPass(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted,
		ownedWorkerSession("agent-orchestrator-63", "$63", "launch-1", false))
	reclaimer.refuse = "a held capacity claim is still paying for this runtime"

	reconcile(t, c, runID)

	if got := len(reclaimer.requests); got != 1 {
		t.Fatalf("requests = %d in one pass, want 1", got)
	}
}

// ---- multiple sessions --------------------------------------------------------

// A run with a worker and an independently-owned reviewer ends both, once each.
func TestTerminalRunEndsEveryRuntimeItOwnsExactlyOnce(t *testing.T) {
	c, _, reclaimer, runID := terminalPolicyFixture(t, domain.WorkflowRunCompleted,
		ownedWorkerSession("agent-orchestrator-70", "$70", "launch-1", false),
		ownedWorkerSession("review-agent-orchestrator-70", "$71", "launch-1", false),
	)

	reconcile(t, c, runID)

	got := reclaimer.sessions()
	if len(got) != 2 {
		t.Fatalf("reclaimed %v, want both the worker and the reviewer", got)
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("%s offered %d times in one pass, want 1", name, n)
		}
	}
}

// ---- no reclaimer wired --------------------------------------------------------

// The dependency is optional, like every other one here: without it a terminal
// run is simply left to the periodic sweep, and nothing panics or stalls.
func TestTerminalRunWithoutAReclaimerIsUnchanged(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC)
	runID := "wf-noreclaimer"
	sid := "agent-orchestrator-80"
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}"},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "agent-orchestrator", Objective: "no reclaimer",
		State: domain.WorkflowRunCompleted, PolicyVersion: "v1", PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	facts := newFakeSessionFacts()
	facts.put(ownedWorkerSession(sid, "$80", "launch-1", false))
	clk := &fakeClock{t: now}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, SessionFacts: facts, Clock: clk.Now, NewID: func() string { return "nr" },
	})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("a terminal run with no reclaimer wired failed to reconcile: %v", err)
	}
}

// ---- 10: provider attempts ----------------------------------------------------

// providerAttemptLedger serves a run's attempts.
type providerAttemptLedger struct {
	workflowcore.ProviderAttemptLedger
	attempts []domain.ProviderAttempt
}

func (l *providerAttemptLedger) ListProviderAttemptsForRun(_ context.Context, _ string) ([]domain.ProviderAttempt, error) {
	return l.attempts, nil
}

// A provider attempt that was superseded by a failover named a runtime of its
// own. When the obligation it belonged to is terminal, that runtime must not be
// left holding anything — §H — so it is offered for reclamation alongside the
// step's session.
func TestTerminalRunEndsASupersededProviderAttemptsRuntime(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC)
	runID := "wf-failover"
	worker, stale := "agent-orchestrator-90", "agent-orchestrator-89"

	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}"},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &worker},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "agent-orchestrator", Objective: "failover",
		State: domain.WorkflowRunCompleted, PolicyVersion: "v1", PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	facts := newFakeSessionFacts()
	facts.put(ownedWorkerSession(worker, "$90", "launch-2", false))
	facts.put(ownedWorkerSession(stale, "$89", "launch-1", false))

	reclaimer := &recordingReclaimer{}
	clk := &fakeClock{t: now}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, SessionFacts: facts, TerminalRuntimes: reclaimer, Clock: clk.Now,
		NewID: func() string { return "pa" },
		ProviderAttempts: &providerAttemptLedger{attempts: []domain.ProviderAttempt{
			// The superseded Codex attempt, whose runtime the step no longer names.
			{ID: "pa-1", WorkflowRunID: runID, Ordinal: 1, State: domain.ProviderAttemptSuperseded, RuntimeSessionID: stale},
			// The successor, which IS the step's session.
			{ID: "pa-2", WorkflowRunID: runID, Ordinal: 2, State: domain.ProviderAttemptCompleted, RuntimeSessionID: worker},
		}},
	})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, s := range reclaimer.sessions() {
		got[s] = true
	}
	if !got[stale] {
		t.Fatalf("reclaimed %v: a superseded provider attempt's runtime was left holding a live agent after its obligation ended",
			reclaimer.sessions())
	}
	if !got[worker] {
		t.Fatalf("reclaimed %v, want the successor's runtime too", reclaimer.sessions())
	}
	if len(reclaimer.requests) != 2 {
		t.Fatalf("requests = %d, want one per distinct session", len(reclaimer.requests))
	}
	// Each request must carry the identity of the session it names, so a stale
	// attempt can never authorize a destroy addressed to the successor's
	// incarnation.
	for _, req := range reclaimer.requests {
		switch req.SessionID {
		case domain.SessionID(stale):
			if req.InstanceID != "$89" {
				t.Fatalf("stale attempt request named incarnation %q, want $89", req.InstanceID)
			}
		case domain.SessionID(worker):
			if req.InstanceID != "$90" {
				t.Fatalf("successor request named incarnation %q, want $90", req.InstanceID)
			}
		}
	}
}

// A session named by both a step and a provider attempt is offered ONCE.
func TestTerminalRunDoesNotOfferTheSameRuntimeTwice(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 8, 29, 21, 30, 0, 0, time.UTC)
	runID := "wf-dedupe"
	worker := "agent-orchestrator-91"

	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: "{}"},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &worker},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "agent-orchestrator", Objective: "dedupe",
		State: domain.WorkflowRunCancelled, PolicyVersion: "v1", PolicySnapshot: "{}",
		CreatedAt: now, UpdatedAt: now,
	}
	facts := newFakeSessionFacts()
	facts.put(ownedWorkerSession(worker, "$91", "launch-1", false))

	reclaimer := &recordingReclaimer{}
	clk := &fakeClock{t: now}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, SessionFacts: facts, TerminalRuntimes: reclaimer, Clock: clk.Now,
		NewID: func() string { return "dd" },
		ProviderAttempts: &providerAttemptLedger{attempts: []domain.ProviderAttempt{
			{ID: "pa-1", WorkflowRunID: runID, Ordinal: 1, State: domain.ProviderAttemptCompleted, RuntimeSessionID: worker},
		}},
	})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(reclaimer.requests) != 1 {
		t.Fatalf("requests = %d, want 1: a session named by both a step and an attempt is one runtime", len(reclaimer.requests))
	}
}
