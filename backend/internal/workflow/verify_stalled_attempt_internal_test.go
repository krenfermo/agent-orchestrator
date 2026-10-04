package workflow

// Regression suite for the SECOND half of the wf-0aadfcde incident (task 4 of
// wf-2f06ba48): a verification whose attempt had already FAILED, whose identity
// nothing could change, and which maybeVerify answered with a bare
//
//	return run, verifyStep, nil
//
// on every single pass. The durable state the diagnosis found:
//
//	workflow_attempts  wfa-verify-…  local-verify  outcome=failed
//	                   model = the CURRENT verification target key
//	workflow_steps     verify  state=waiting   (non-terminal, so re-entered)
//	workflow_runs      wf-0aadfcde  state=waiting
//	checkpoints        …no verify_fix_reentry after that attempt at all
//
// verifyAttemptID is a pure function of (step, target key, recovery generation,
// fix generation). None of those four inputs could move: no fix cycle was open,
// no recovery generation had been authorized, and the target key was pinned by
// the approval the review step pointed at. So every wake of the reconciler
// re-derived the same identity, found the same finished-and-failed row, and
// returned success without writing a checkpoint or making a transition. The run
// advertised a live verification for as long as the daemon stayed up.
//
// The invariant these tests pin: a failed verify attempt that cannot be retried
// under its own identity must produce a DURABLE transition — towards a valid
// recovery, or towards needs_attention — and never a silent success.

import (
	stdctx "context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// ---------------------------------------------------------------------------
// stubs: the verification never executes on this path, so these exist only to
// satisfy maybeVerify's "am I wired up at all" guard.
// ---------------------------------------------------------------------------

type stalledVerifier struct{ calls int }

func (s *stalledVerifier) Run(stdctx.Context, VerifyCommandRequest) (VerifyCommandExecution, error) {
	s.calls++
	return VerifyCommandExecution{}, nil
}

type stalledWorkspaceFacts struct{ obs ports.WorkspaceObservation }

func (s *stalledWorkspaceFacts) ObserveWorkspace(stdctx.Context, ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	return s.obs, nil
}

type stalledMessageSender struct{ calls int }

func (s *stalledMessageSender) Send(stdctx.Context, domain.SessionID, string, *ports.SpawnAttachment) error {
	s.calls++
	return nil
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

type stalledVerifyFixture struct {
	t         *testing.T
	ctx       stdctx.Context
	c         *Coordinator
	store     *sqlite.Store
	verifier  *stalledVerifier
	runID     string
	steps     map[domain.WorkflowStepKind]domain.WorkflowStep
	attempt   domain.WorkflowAttempt
	targetKey string
	now       time.Time
}

type stalledVerifyOptions struct {
	// fixReentry writes the verify_fix_reentry checkpoint finishVerifyFailure
	// writes when it parks a repairable failure for a fix worker.
	fixReentry bool
	// fixAttempt records a fix attempt AFTER that re-entry, which is what
	// "answered" means for unansweredVerifyFixReentry.
	fixAttempt bool
	// fixState overrides the fix step's state (default waiting).
	fixState domain.WorkflowStepState
	// verifyState overrides the verify step's state (default waiting).
	verifyState domain.WorkflowStepState
}

// newStalledVerifyFixture materializes the incident's durable state against a
// REAL sqlite store: every compare-and-swap the code under test relies on is the
// production statement, not a fake's approximation of it.
func newStalledVerifyFixture(t *testing.T, opts stalledVerifyOptions) *stalledVerifyFixture {
	t.Helper()
	st := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Date(2026, 9, 5, 11, 4, 0, 0, time.UTC)
	root := t.TempDir()

	if err := st.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: root, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}

	plan := VerificationPlan{
		Commands: []VerificationCommandCheck{
			{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
	}
	artifact := BuildPlanArtifact("proj-1", "ship the thing", policyVersionV1, plan)
	raw, err := MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}

	fixState := opts.fixState
	if fixState == "" {
		fixState = domain.WorkflowStepWaiting
	}
	verifyState := opts.verifyState
	if verifyState == "" {
		verifyState = domain.WorkflowStepWaiting
	}
	session, err := st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-1", Kind: domain.KindWorker,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: root},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid, reviewRunID := string(session.ID), "rr-e7cac283"
	runID := "wf-0aadfcde"
	run := domain.WorkflowRun{
		ID: runID, ProjectID: "proj-1", Objective: "ship the thing",
		State: domain.WorkflowRunWaiting, PolicyVersion: policyVersionV1,
		PolicySnapshot: `{"maxFixCycles":3}`, CreatedAt: now, UpdatedAt: now,
	}
	steps := []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: raw, CreatedAt: now, UpdatedAt: now},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid, CreatedAt: now, UpdatedAt: now},
		{ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, ReviewRunID: &reviewRunID, CreatedAt: now, UpdatedAt: now},
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: fixState, CreatedAt: now, UpdatedAt: now},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: verifyState, CreatedAt: now, UpdatedAt: now},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending, CreatedAt: now, UpdatedAt: now},
	}
	if _, _, err := st.CreateWorkflowRun(ctx, run, steps); err != nil {
		t.Fatal(err)
	}

	obs := ports.WorkspaceObservation{Path: root, Branch: "ao/wf", HeadSHA: "c0ffee"}
	reviewed := WorkspaceFingerprint(obs)

	// The approving review the verification is running under.
	if err := st.UpsertReview(ctx, domain.Review{
		ID: "review-e7cac283", SessionID: domain.SessionID(sid), ProjectID: "proj-1",
		Harness: domain.ReviewerClaudeCode, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertReviewRun(ctx, domain.ReviewRun{
		ID: reviewRunID, ReviewID: "review-e7cac283", SessionID: domain.SessionID(sid),
		Harness: domain.ReviewerClaudeCode, TriggerSource: domain.ReviewTriggerAuto,
		TargetSHA: reviewed, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// The authority pointer is set through the store, exactly as
	// recordReviewDispatchSuccess sets it: CreateWorkflowRun does not carry it.
	if ok, err := st.SetWorkflowStepReviewRun(ctx, "review", reviewRunID, now); err != nil || !ok {
		t.Fatalf("SetWorkflowStepReviewRun: %v ok=%v", err, ok)
	}

	// The work step's completion checkpoint: the worktree facts verify needs.
	workStepID := "work"
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-work", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "proj-1",
		SessionID: &sid, Branch: "ao/wf", WorktreePath: root, HeadSHA: "c0ffee",
		FingerprintAfter: reviewed, DurablePhase: "work_observed_completed",
		PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	targetKey := verificationTargetKey(reviewed, artifact.Verification)
	attemptID := verifyAttemptID("verify", targetKey, 0, 0)
	attempt, err := st.CreateWorkflowAttempt(ctx, attemptID, "verify", "local-verify", targetKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateWorkflowAttemptOutcome(ctx, attemptID, now.Add(300*time.Millisecond),
		domain.WorkflowAttemptFailed, domain.WorkflowErrorVerifyCommandFailed); err != nil {
		t.Fatal(err)
	}
	// The verify_result the failing execution persisted, exactly as
	// persistVerifyResult writes it.
	verifyStepID, aID := "verify", attemptID
	if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-verify-result", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, AttemptID: &aID,
		ProjectID: "proj-1", DurablePhase: verifyResultPhase, PayloadVersion: verifyResultVersion,
		RetryState: `{"version":"` + verifyResultVersion + `","passed":false,"targetKey":"` + targetKey +
			`","reviewedFingerprint":"` + reviewed + `","errorClass":"verify_command_failed"}`,
		NextAction: "verify_failed", CreatedAt: now.Add(300 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if opts.fixReentry {
		if _, err := st.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: "wfc-reentry", WorkflowRunID: runID, WorkflowStepID: &verifyStepID, AttemptID: &aID,
			ProjectID: "proj-1", DurablePhase: ReasonVerifyFixReentry, PayloadVersion: verifyResultVersion,
			RetryState: "{}", NextAction: "fix: verification failed", CreatedAt: now.Add(400 * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if opts.fixAttempt {
		if _, err := st.CreateWorkflowAttempt(ctx, "wfa-fix-1", "fix", "claude", "fix",
			now.Add(500*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}

	verifier := &stalledVerifier{}
	c := New(Deps{
		Store: st, ReviewRuns: st, WorkspaceFacts: &stalledWorkspaceFacts{obs: obs},
		Verifier: verifier, MessageSender: &stalledMessageSender{},
		Clock: func() time.Time { return now.Add(time.Second) },
		NewID: func() string { return "gen" },
	})

	f := &stalledVerifyFixture{
		t: t, ctx: ctx, c: c, store: st, verifier: verifier, runID: runID,
		attempt: attempt, targetKey: targetKey, now: now,
	}
	f.reload()
	return f
}

func (f *stalledVerifyFixture) reload() {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.runID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.steps = map[domain.WorkflowStepKind]domain.WorkflowStep{}
	for _, s := range steps {
		f.steps[s.Kind] = s
	}
}

func (f *stalledVerifyFixture) run() domain.WorkflowRun {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	return run
}

// verifyPass is one reconciler wake: exactly the call the cascade makes.
func (f *stalledVerifyFixture) verifyPass() {
	f.t.Helper()
	f.reload()
	if _, _, err := f.c.maybeVerify(f.ctx, f.run(),
		f.steps[domain.WorkflowStepWork], f.steps[domain.WorkflowStepReview],
		f.steps[domain.WorkflowStepVerify]); err != nil {
		f.t.Fatalf("maybeVerify: %v", err)
	}
	f.reload()
}

func (f *stalledVerifyFixture) checkpoints() []domain.WorkflowCheckpoint {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return cps
}

func (f *stalledVerifyFixture) countPhase(phase string) int {
	n := 0
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// the incident
// ---------------------------------------------------------------------------

// THE REGRESSION. A failed verify attempt whose identity nothing can change is
// re-entered on every wake. Before the fix this wrote nothing and moved nothing,
// forever; the run stayed `waiting` over a verification that had already failed.
func TestFailedVerifyAttemptWithNoRetryPathStopsInsteadOfLooping(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{})

	// The premise of the incident, asserted rather than assumed: the failed
	// attempt's model IS the target key this pass re-derives, so the identity
	// maybeVerify computes lands on exactly this finished row.
	if f.attempt.Model != f.targetKey {
		t.Fatalf("attempt model = %q, want the live target key %q", f.attempt.Model, f.targetKey)
	}

	before := len(f.checkpoints())
	f.verifyPass()

	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention: a failed verification nothing can retry must stop for a person", got)
	}
	if got := f.steps[domain.WorkflowStepVerify].State; !got.Terminal() {
		t.Fatalf("verify step = %q, want a terminal state: a non-terminal verify step is rendered as live work", got)
	}
	if len(f.checkpoints()) == before {
		t.Fatal("the pass wrote no durable record at all: the loop is invisible in the ledger")
	}
	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 1 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d, want exactly 1", got)
	}
	// The stop has to be readable as being about THIS attempt, or an operator
	// cannot tell which verification it is talking about.
	named := false
	for _, cp := range f.checkpoints() {
		if cp.DurablePhase == ReasonVerifyAttemptUnretryable && strings.Contains(cp.NextAction, f.attempt.ID) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the stop does not name attempt %s", f.attempt.ID)
	}
	if f.verifier.calls != 0 {
		t.Fatalf("the verifier ran %d times: a finished attempt must never be re-executed", f.verifier.calls)
	}
}

// Convergence: the stop is recorded ONCE however many times the reconciler
// wakes. A stop that re-records itself per poll is the ledger growth the
// wf-c4c84f52 incident produced.
func TestStalledVerifyAttemptStopIsRecordedExactlyOnce(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{})
	for i := 0; i < 5; i++ {
		f.verifyPass()
	}
	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 1 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d after 5 wakes, want exactly 1", got)
	}
}

// The one shape that legitimately rests: a verify_fix_reentry that no fix cycle
// has answered yet. Its identity WILL move — the fix delivery advances the fix
// generation — so this must stay exactly as quiet as it was.
func TestUnansweredFixReentryStillRestsQuietly(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{fixReentry: true})

	before := len(f.checkpoints())
	f.verifyPass()

	if got := f.run().State; got != domain.WorkflowRunWaiting {
		t.Fatalf("run = %q, want waiting: a fix cycle is still owed this verification", got)
	}
	if got := f.steps[domain.WorkflowStepVerify].State; got.Terminal() {
		t.Fatalf("verify step = %q, want non-terminal: its fix cycle has not run yet", got)
	}
	if got := len(f.checkpoints()); got != before {
		t.Fatalf("checkpoints = %d, want %d: an unanswered re-entry must write nothing", got, before)
	}
}

// A re-entry that HAS been answered — the fix cycle ran — and still left the
// verification identity unchanged is the same dead end as no re-entry at all:
// the fix delivered nothing the target key can see, so re-entering finds the
// same failed attempt forever.
func TestAnsweredFixReentryThatChangedNothingStops(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{fixReentry: true, fixAttempt: true})
	f.verifyPass()

	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention: the fix cycle ran and changed nothing verification can see", got)
	}
	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 1 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d, want exactly 1", got)
	}
}

// A permanently blocked fix cycle is not a reason to keep resting either: the
// re-entry is open, but the fix step is terminal so nothing can ever answer it.
func TestOpenReentryWithATerminalFixStepStops(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{
		fixReentry: true, fixState: domain.WorkflowStepFailed,
	})
	f.verifyPass()

	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention: the fix step that owed this re-entry is terminal", got)
	}
}

// A run already stopped for a NAMED reason keeps that reason. The verify step
// still has to leave its non-terminal state — it renders as live work — but
// nothing here overwrites a more specific explanation somebody else recorded.
func TestStalledVerifyDoesNotOverwriteAnExistingStop(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{})

	stepID := "fix"
	if _, err := f.store.CreateWorkflowCheckpoint(f.ctx, domain.WorkflowCheckpoint{
		ID: "wfc-existing-stop", WorkflowRunID: f.runID, WorkflowStepID: &stepID,
		ProjectID: "proj-1", DurablePhase: ReasonFixNoVerifiableChange,
		NextAction: "the fix worker left no change", PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: f.now.Add(600 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID,
		domain.WorkflowRunWaiting, domain.WorkflowRunRunning, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.UpdateWorkflowRunState(f.ctx, f.runID,
		domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, f.now); err != nil {
		t.Fatal(err)
	}

	f.verifyPass()

	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 0 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d, want 0: this run already had a named stop", got)
	}
	if got := f.steps[domain.WorkflowStepVerify].State; got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed: a finished verification must not render as live work", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention unchanged", got)
	}
}

// The race: several reconciler passes hit the same stalled attempt at once —
// a boot reconcile, a wake and a Board poll are genuinely concurrent in the
// daemon. The step transition is a compare-and-swap, so exactly one of them may
// own the decision, and exactly one stop may be recorded.
func TestConcurrentPassesProduceExactlyOneStalledVerifyStop(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{})

	run := f.run()
	work, review, verify := f.steps[domain.WorkflowStepWork],
		f.steps[domain.WorkflowStepReview], f.steps[domain.WorkflowStepVerify]

	const passes = 8
	errs := make(chan error, passes)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < passes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := f.c.maybeVerify(f.ctx, run, work, review, verify)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("maybeVerify: %v", err)
		}
	}

	f.reload()
	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 1 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d after %d concurrent passes, want exactly 1", got, passes)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention", got)
	}
	if got := f.steps[domain.WorkflowStepVerify].State; got != domain.WorkflowStepFailed {
		t.Fatalf("verify step = %q, want failed", got)
	}
}

// A restart changes nothing: the decision is derived from durable rows, so a
// fresh coordinator over the same store re-reaches it and records no second one.
func TestStalledVerifyStopSurvivesARestart(t *testing.T) {
	f := newStalledVerifyFixture(t, stalledVerifyOptions{})
	f.verifyPass()

	f.c = New(Deps{
		Store: f.store, ReviewRuns: f.store,
		WorkspaceFacts: &stalledWorkspaceFacts{}, Verifier: f.verifier,
		MessageSender: &stalledMessageSender{},
		Clock:         func() time.Time { return f.now.Add(2 * time.Second) },
		NewID:         func() string { return "restart" },
	})
	f.verifyPass()

	if got := f.countPhase(ReasonVerifyAttemptUnretryable); got != 1 {
		t.Fatalf("verify_attempt_unretryable checkpoints = %d after a restart, want exactly 1", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q after a restart, want needs_attention to stay put", got)
	}
}

// The stop must not take the operator recovery away. A stale approval is
// exactly the state this stop names, and RecoverUnprovableApprovedHead is the
// door out of it — discard the approval nobody can locate, ask for one fresh
// review of what is actually there. A new stop reason that was not in that
// door's accepted set would have made the dead end visible and unopenable.
func TestStalledVerifyStopStillOpensTheOperatorProvenanceRecovery(t *testing.T) {
	fx := newUnprovableProvenanceFixture(t)

	// The run is re-parked under the new reason, newer than the one the fixture
	// wrote, so stopReason resolves to it.
	verifyStepID := "wfs-verify"
	if _, err := fx.store.CreateWorkflowCheckpoint(fx.ctx, domain.WorkflowCheckpoint{
		ID: "cp-stop-unretryable", WorkflowRunID: fx.runID, WorkflowStepID: &verifyStepID,
		ProjectID: "p", NextAction: "verify attempt already failed and nothing can ask it again",
		DurablePhase: ReasonVerifyAttemptUnretryable, PayloadVersion: "v1",
		RetryState: "{}", CreatedAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := fx.c.RecoverUnprovableApprovedHead(fx.ctx, fx.runID)
	if err != nil {
		t.Fatalf("RecoverUnprovableApprovedHead on a verify_attempt_unretryable stop: %v", err)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want the stop cleared by the recovery", detail.Run.State)
	}
}

// The stop's own text must describe what actually happens. It used to promise
// an unconditional reopen on Continue; that is true only for a shape a recovery
// door can PROVE safe, and every other shape needs a person. Telling them
// otherwise sends them to press a button that correctly does nothing, with no
// explanation of why.
func TestUnretryableStopTextDoesNotPromiseAnUnconditionalReopen(t *testing.T) {
	action := attentionDispositions[ReasonVerifyAttemptUnretryable].HumanAction
	if strings.TrimSpace(action) == "" {
		t.Fatal("verify_attempt_unretryable has no human action")
	}
	if strings.Contains(action, "AO reopens the verification once per continue") {
		t.Fatalf("the stop still promises an unconditional reopen: %q", action)
	}
	// It has to say what Continue actually depends on, and what to do when the
	// condition does not hold.
	for _, want := range []string{"continue", "fresh review", "cancel"} {
		if !strings.Contains(strings.ToLower(action), want) {
			t.Fatalf("the stop text does not mention %q: %q", want, action)
		}
	}
}

// The stop names a recoverable stop reason, so an operator's Continue reaches
// verify_recovery.go's bounded reopen rather than a dead end — that reopen is
// the "valid recovery" half of the requirement, and it is what actually changes
// the attempt identity.
func TestStalledVerifyStopIsReachableByVerifyRecovery(t *testing.T) {
	if !recoverableVerifyStopReasons[ReasonVerifyAttemptUnretryable] {
		t.Fatal("verify_attempt_unretryable is not a recoverable verify stop, so Continue can never reopen it")
	}
	d, ok := attentionDispositions[ReasonVerifyAttemptUnretryable]
	if !ok {
		t.Fatal("verify_attempt_unretryable is not in the attention vocabulary, so the Board renders it as unclassified")
	}
	if d.HumanAction == "" {
		t.Fatal("verify_attempt_unretryable has no human action, so it cannot be a human decision")
	}
}
