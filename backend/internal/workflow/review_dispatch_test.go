package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeReviewRuns is a hand-rolled fake for workflowcore.ReviewRuns: no real
// sqlite review_store call in unit tests, but the same natural-key dedupe
// semantics (unique on session+prUrl+targetSha+harness).
type fakeReviewRuns struct {
	reviews map[string]domain.Review    // by "sessionID|harness"
	runs    map[string]domain.ReviewRun // by run id

	// insertCalls counts InsertReviewRun invocations, used to assert a
	// review is created exactly once across repeated dispatch calls.
	insertCalls int
	// beforeCancelRunning fires inside CancelRunningReviewRunsBySessionAndHarness,
	// immediately before its CAS-guarded update, so a test can make a normal
	// verdict win that exact race rather than approximately.
	beforeCancelRunning func()
	// cancelErr makes the cancellation itself fail, so a test can prove AO never
	// acts on a cancellation it could not prove.
	cancelErr error
	// beforeGetReviewRun fires on each read, so a test can change authority
	// between an adopter's gate read and its own revalidation — the exact window
	// the revalidation exists to close.
	beforeGetReviewRun func(id string)
	// forceDuplicate makes the next InsertReviewRun return
	// domain.ErrDuplicateReviewRun instead of inserting, so tests can drive
	// the dedupe-fallback path deterministically.
	forceDuplicate       bool
	rowCreateErr         error
	afterInsertReviewRun func(reviewRunID string)
}

func newFakeReviewRuns() *fakeReviewRuns {
	return &fakeReviewRuns{
		reviews: map[string]domain.Review{},
		runs:    map[string]domain.ReviewRun{},
	}
}

func reviewKey(id domain.SessionID, harness domain.ReviewerHarness) string {
	return string(id) + "|" + string(harness)
}

func naturalKey(id domain.SessionID, prURL, targetSHA string, harness domain.ReviewerHarness) string {
	return string(id) + "|" + prURL + "|" + targetSHA + "|" + string(harness)
}

func (f *fakeReviewRuns) GetReviewRun(_ context.Context, id string) (domain.ReviewRun, bool, error) {
	if hook := f.beforeGetReviewRun; hook != nil {
		hook(id)
	}
	r, ok := f.runs[id]
	return r, ok, nil
}

func (f *fakeReviewRuns) GetReviewBySessionAndHarness(_ context.Context, id domain.SessionID, harness domain.ReviewerHarness) (domain.Review, bool, error) {
	// rowCreateErr models the review ROW failing to be created — the window in
	// which a launch attempt dies before any review run id exists.
	if f.rowCreateErr != nil {
		err := f.rowCreateErr
		f.rowCreateErr = nil
		return domain.Review{}, false, err
	}
	r, ok := f.reviews[reviewKey(id, harness)]
	return r, ok, nil
}

func (f *fakeReviewRuns) UpsertReview(_ context.Context, r domain.Review) error {
	f.reviews[reviewKey(r.SessionID, r.Harness)] = r
	return nil
}

func (f *fakeReviewRuns) InsertReviewRun(_ context.Context, run domain.ReviewRun) error {
	f.insertCalls++
	// afterInsertReviewRun fires once the identity exists and before anything is
	// launched — the window in which a dispatch can lose its authorization with
	// a review row already on disk.
	if f.afterInsertReviewRun != nil {
		hook := f.afterInsertReviewRun
		f.afterInsertReviewRun = nil
		defer hook(run.ID)
	}
	if f.forceDuplicate {
		f.forceDuplicate = false
		return domain.ErrDuplicateReviewRun
	}
	key := naturalKey(run.SessionID, run.PRURL, run.TargetSHA, run.Harness)
	// Mirrors migration 0014's partial unique index: rows closed out as
	// "failed" are durable diagnostics, not idempotency winners, so a retry of
	// the same target after a failed reviewer launch can insert a fresh run.
	if _, exists := f.byNaturalKey(key, true); exists {
		return domain.ErrDuplicateReviewRun
	}
	f.runs[run.ID] = run
	return nil
}

// byNaturalKey returns the newest run matching the natural key, mirroring the
// real query's `ORDER BY created_at DESC LIMIT 1`. excludeFailed mirrors the
// unique index's `status NOT IN ('failed','cancelled')` predicate (insert
// dedupe only): a run AO closed out never wins idempotency over its own
// replacement.
func (f *fakeReviewRuns) byNaturalKey(key string, excludeFailed bool) (domain.ReviewRun, bool) {
	var newest domain.ReviewRun
	found := false
	for _, r := range f.runs {
		if naturalKey(r.SessionID, r.PRURL, r.TargetSHA, r.Harness) != key {
			continue
		}
		// The real index is `status NOT IN ('failed','cancelled') AND (status =
		// 'running' OR verdict NOT IN ('','changes_requested'))`. `cancelled`
		// was missing here, and that gap hid the wf-756988ae retry: after a
		// stall closed a run out as cancelled, the replacement's insert deduped
		// onto the very run it was replacing, so the step was rebound to the
		// dead review and observed straight into reviewer_launch_failed. In the
		// real store that insert succeeds.
		if excludeFailed &&
			(r.Status == domain.ReviewRunFailed || r.Status == domain.ReviewRunCancelled) {
			continue
		}
		if !found || r.CreatedAt.After(newest.CreatedAt) {
			newest = r
			found = true
		}
	}
	return newest, found
}

func (f *fakeReviewRuns) GetReviewRunBySessionPRSHAAndHarness(_ context.Context, id domain.SessionID, prURL, targetSHA string, harness domain.ReviewerHarness) (domain.ReviewRun, bool, error) {
	r, ok := f.byNaturalKey(naturalKey(id, prURL, targetSHA, harness), false)
	return r, ok, nil
}

// ListReviewRunsBySession backs Checkpoint 8D's cycle-number derivation.
// Sorted newest-first, matching the real store's `ORDER BY created_at DESC`
// (review.sql) — Checkpoint 8P-D.3's reviewerHarnessForStep relies on that
// ordering to find the most recent non-cancelled cycle.
func (f *fakeReviewRuns) ListReviewRunsBySession(_ context.Context, id domain.SessionID) ([]domain.ReviewRun, error) {
	var out []domain.ReviewRun
	for _, r := range f.runs {
		if r.SessionID == id {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// CancelRunningReviewRunsBySessionAndHarness backs Checkpoint 8P-D.3's
// reviewer-capacity stall recovery, mirroring the real store's CAS guard
// (status='running' AND verdict=”).
func (f *fakeReviewRuns) CancelRunningReviewRunsBySessionAndHarness(_ context.Context, id domain.SessionID, harness domain.ReviewerHarness, body string) (int64, error) {
	if hook := f.beforeCancelRunning; hook != nil {
		// Deterministic interleaving point: a competing submit runs HERE, after
		// the stall path decided to cancel and before the CAS-guarded update
		// takes effect. Cleared first so a hook cannot recurse.
		f.beforeCancelRunning = nil
		hook()
	}
	if f.cancelErr != nil {
		return 0, f.cancelErr
	}
	var n int64
	for rid, r := range f.runs {
		if r.SessionID == id && r.Harness == harness && r.Status == domain.ReviewRunRunning && r.Verdict == "" {
			r.Status = domain.ReviewRunCancelled
			r.Body = body
			f.runs[rid] = r
			n++
		}
	}
	return n, nil
}

// MarkReviewRunSupersededBy mirrors the real store's write-once guard: it names
// the replacement that took authority over a closed-out run.
func (f *fakeReviewRuns) MarkReviewRunSupersededBy(_ context.Context, id, supersededBy string) (bool, error) {
	r, ok := f.runs[id]
	if !ok || id == supersededBy {
		return false, nil
	}
	if r.SupersededBy != "" {
		// Mirrors the real store: already holding THIS replacement is an
		// idempotent replay (a crash between the supersede and the rebind), and
		// must succeed so the replay can finish the rebind. Holding a DIFFERENT
		// replacement is a genuinely lost race.
		return r.SupersededBy == supersededBy, nil
	}
	r.SupersededBy = supersededBy
	f.runs[id] = r
	return true, nil
}

// RecordLateReviewVerdict mirrors the real store's guard: a run AO closed out
// without a verdict, and no late verdict recorded yet. It is what makes a
// reviewer that answered after AO stopped listening testable here.
func (f *fakeReviewRuns) RecordLateReviewVerdict(
	id string, verdict domain.ReviewVerdict, body string, at time.Time,
) bool {
	r, ok := f.runs[id]
	if !ok || !r.TerminalWithoutVerdict() || r.LateVerdict.Valid() {
		return false
	}
	stamp := at
	r.LateVerdict, r.LateVerdictBody, r.LateVerdictAt = verdict, body, &stamp
	f.runs[id] = r
	return true
}

// UpdateReviewRunResult mirrors the real store's CAS guard (status='running'):
// it is how review_launch_recovery.go closes out a review_run whose reviewer
// never launched, and it must be a no-op against anything already terminal.
func (f *fakeReviewRuns) UpdateReviewRunResult(_ context.Context, id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, body, githubReviewID string, autoInjectReview bool) (bool, error) {
	r, ok := f.runs[id]
	if !ok || r.Status != domain.ReviewRunRunning {
		return false, nil
	}
	r.Status = status
	r.Verdict = verdict
	r.Body = body
	r.GithubReviewID = githubReviewID
	r.AutoInjectReview = autoInjectReview
	f.runs[id] = r
	return true, nil
}

// fakeMessageSender is a hand-rolled fake for workflowcore.MessageSender (no
// real session_manager.Send in unit tests).
type fakeMessageSender struct {
	calls   int
	lastID  domain.SessionID
	lastMsg string
	err     error
}

func (f *fakeMessageSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.calls++
	f.lastID = id
	f.lastMsg = message
	return f.err
}

// setStatus lets tests simulate the real `ao review submit` CLI call landing
// out-of-band (as it does for real: Claude's own process calls the daemon's
// HTTP endpoint directly, never through workflow's own write path).
func (f *fakeReviewRuns) setStatus(id string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict) {
	r := f.runs[id]
	r.Status = status
	r.Verdict = verdict
	f.runs[id] = r
}

// fakeReviewerLauncher is a hand-rolled fake for workflowcore.ReviewerLauncher
// (no real Claude Code process/pane in unit tests).
type fakeReviewerLauncher struct {
	// externalLive models the world OUTSIDE AO: which deterministic reviewer
	// identities currently exist. It survives "restarts" (the coordinator is
	// rebuilt, this map is not), which is what makes an orphaned reviewer
	// observable at all.
	externalLive map[string]bool
	probeCalls   int
	cancelCalls  int
	// probeUnknown makes the probe answer "AO cannot tell", the one reply that
	// must never be read as absence.
	probeUnknown bool
	// probeErrorsOnce models a transient probe failure over a reviewer that is
	// genuinely alive — the case in which launching would duplicate it.
	probeErrorsOnce bool
	// foreign marks identities that exist but are NOT AO's reviewer: a name
	// collision or a stale shell. They may never be adopted or destroyed.
	foreign map[string]bool
	// beforeLaunch fires immediately before the external launch, so a test can
	// change durable state in the window between the review identity being
	// created and the reviewer existing.
	beforeLaunch func()
	// externalExited models AO's own reviewer whose process has exited while its
	// session lingers.
	externalExited map[string]bool
	// instances maps a reviewer name to the incarnation currently behind it.
	// Replacing the value models a NAME changing hands.
	instances   map[string]string
	instanceSeq int
	// ownedWithoutInstance models a launcher that reports a live reviewer it
	// cannot pin to an incarnation — the one answer a confirmation must refuse.
	ownedWithoutInstance bool
	// strictOwnership makes CancelReviewer behave exactly as the production
	// launcher does: an identity AO cannot PROVE it owns is refused, with the
	// deterministic marker that tells the wake scheduler retrying is pointless.
	// Without it this fake would happily destroy a session production would
	// never touch, and every test built on it would be testing the fake.
	strictOwnership bool
	// afterProbe fires once an observation has been produced, so a test can
	// replace the session in the window a second look-up would fall into.
	afterProbe     func()
	preflightCalls int
	launchCalls    int
	preflightErr   error
	launchErr      error
	lastPrompt     string
	lastReq        workflowcore.ReviewerLaunchRequest
}

func (f *fakeReviewerLauncher) Preflight(_ context.Context, _ domain.ReviewerHarness, _ string) error {
	f.preflightCalls++
	return f.preflightErr
}

func (f *fakeReviewerLauncher) Launch(_ context.Context, req workflowcore.ReviewerLaunchRequest) (workflowcore.ReviewerLaunchResult, error) {
	f.launchCalls++
	f.lastPrompt = req.Prompt
	f.lastReq = req
	if hook := f.beforeLaunch; hook != nil {
		f.beforeLaunch = nil
		hook()
	}
	if f.externalLive == nil {
		f.externalLive = map[string]bool{}
	}
	identity := f.ReviewerIdentity(req)
	f.externalLive[identity] = true
	if f.instances == nil {
		f.instances = map[string]string{}
	}
	f.instanceSeq++
	f.instances[identity] = "$" + strconv.Itoa(f.instanceSeq)
	if f.launchErr != nil {
		return workflowcore.ReviewerLaunchResult{}, f.launchErr
	}
	// The handle IS the deterministic identity, exactly as the production
	// launcher returns it (the real runtimes derive their handle from the
	// session id they are given). A fake that invented its own handle would let
	// probe and cancel address something that does not exist.
	// The instance the "runtime" just created travels back, exactly as the real
	// launcher returns tmux's `$N`.
	return workflowcore.ReviewerLaunchResult{HandleID: identity, InstanceID: f.instances[identity]}, nil
}

// newCoordinatorWithReview wires a coordinator with 8B's work-step deps plus
// 8C's review deps, all fakes.
func newCoordinatorWithReview(spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts, workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns, launcher *fakeReviewerLauncher) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	// The store's conditional release has to be able to see the run's late
	// verdict, exactly as the real single-statement UPDATE does.
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

// newCoordinatorWithReviewAndMessages is newCoordinatorWithReview plus the fix
// step's message transport, so a test can observe the prompt a fix cycle
// actually delivers.
func newCoordinatorWithReviewAndMessages(
	spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts,
	workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns,
	launcher *fakeReviewerLauncher, messages workflowcore.MessageSender,
) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	store.reviewRuns = reviewRuns
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		MessageSender:    messages,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

// completeWorkStep drives a run all the way through StartRun and a
// successful work-step completion (idle worker + dirty worktree evidence),
// returning the run detail once the work step is completed and the review
// step is ready/pending. Shared setup for the review-dispatch tests below.
func completeWorkStep(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, runID string) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	detail, err := c.StartRun(ctx, runID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs.Dirty = true
	clk.Advance(10 * time.Second)
	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	_ = store
	return got
}

// Test: work completed enables review dispatch; before completion,
// ContinueRun is a no-op that never creates a review run.
func TestContinueRunNoOpBeforeWorkCompletes(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, _, _ := newCoordinatorWithReview(spawner, sessionFacts, &fakeWorkspaceFacts{}, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if reviewRuns.insertCalls != 0 {
		t.Fatalf("InsertReviewRun calls = %d, want 0 before work completes", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0 before work completes", launcher.launchCalls)
	}
	if reviewStepFrom(got).Step.State != domain.WorkflowStepPending {
		t.Fatalf("review step state = %q, want pending", reviewStepFrom(got).Step.State)
	}
}

// Test: review is created exactly once, harness is codex (Checkpoint 8L:
// ExecutionRouter's default cross-provider reviewer independence — the
// worker for this normal-complexity "ship the thing" objective routes to
// claude-code by default, so the reviewer routes to the opposite provider,
// codex), and repeated ContinueRun calls are idempotent (no duplicate
// review_run, no duplicate reviewer launch).
func TestContinueRunDispatchesReviewExactlyOnceAndIdempotent(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want running", review.Step.State)
	}
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after dispatch")
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want 1", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", launcher.launchCalls)
	}
	run, ok := reviewRuns.runs[*review.Step.ReviewRunID]
	if !ok {
		t.Fatalf("review run %s not found", *review.Step.ReviewRunID)
	}
	if run.Harness != domain.ReviewerCodex {
		t.Fatalf("review run harness = %q, want codex", run.Harness)
	}
	if run.AutoInjectReview {
		t.Fatalf("review run AutoInjectReview = true, want false (no-fix-loop guardrail)")
	}

	// Repeated ContinueRun calls must not create a second review or launch.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second ContinueRun: %v", err)
	}
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("third ContinueRun: %v", err)
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls after repeated ContinueRun = %d, want still 1", reviewRuns.insertCalls)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls after repeated ContinueRun = %d, want still 1", launcher.launchCalls)
	}
}

// Test: the launched prompt instructs a plain git status/diff and the exact
// `ao review submit` single-run form, never a PR-centric review.
func TestContinueRunPromptIsWorkflowOwnedNotPRCentric(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if launcher.lastPrompt == "" {
		t.Fatalf("no prompt captured")
	}
	for _, forbidden := range []string{"gh api", "pulls/", "PR base branch", "--reviews"} {
		if contains(launcher.lastPrompt, forbidden) {
			t.Errorf("prompt contains forbidden PR-centric text %q:\n%s", forbidden, launcher.lastPrompt)
		}
	}
	for _, required := range []string{"git status", "git diff", "ao review submit", "--verdict"} {
		if !contains(launcher.lastPrompt, required) {
			t.Errorf("prompt missing required text %q", required)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// Test: approved verdict -> next_action "verify"; changes_requested -> "fix".
// Also asserts zero Spawn calls happen from the changes_requested path (no
// fix loop) and that ContinueRun never dispatches anything toward fix.
func TestReviewVerdictDrivesNextAction(t *testing.T) {
	cases := []struct {
		name       string
		verdict    domain.ReviewVerdict
		status     domain.ReviewRunStatus
		wantStep   domain.WorkflowStepState
		wantRun    domain.WorkflowRunState
		wantAction string
	}{
		{"approved", domain.VerdictApproved, domain.ReviewRunComplete, domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify"},
		// 8C->8D revision: changes_requested now rests the review step at
		// "waiting" (non-terminal), not "completed" — a terminal state would
		// make a second review cycle for the same step impossible. See
		// fix_dispatch_test.go for the full review<->fix cycling coverage.
		{"changes_requested", domain.VerdictChangesRequested, domain.ReviewRunComplete, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting, "fix"},
		{"delivered_approved", domain.VerdictApproved, domain.ReviewRunDelivered, domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionFacts := newFakeSessionFacts()
			spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
			workspaceFacts := &fakeWorkspaceFacts{}
			reviewRuns := newFakeReviewRuns()
			launcher := &fakeReviewerLauncher{}
			c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
			ctx := context.Background()

			created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
			got, err := c.ContinueRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("ContinueRun: %v", err)
			}
			reviewRunID := *reviewStepFrom(got).Step.ReviewRunID

			// Simulate the real `ao review submit` CLI call landing out of
			// band (as it does for real — this is a live HTTP write into the
			// review_run row, not anything workflow's own code calls).
			reviewRuns.setStatus(reviewRunID, tc.status, tc.verdict)
			clk.Advance(time.Second)

			final, err := c.GetRun(ctx, created.Run.ID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			review := reviewStepFrom(final)
			if review.Step.State != tc.wantStep {
				t.Fatalf("review step state = %q, want %q", review.Step.State, tc.wantStep)
			}
			if final.Run.State != tc.wantRun {
				t.Fatalf("run state = %q, want %q", final.Run.State, tc.wantRun)
			}
			if final.NextAction != tc.wantAction {
				t.Fatalf("next action = %q, want %q", final.NextAction, tc.wantAction)
			}
			// No fix loop: zero Spawn calls happened beyond the original work
			// step dispatch (exactly 1, from StartRun).
			if spawner.calls != 1 {
				t.Fatalf("spawner calls = %d, want exactly 1 (no fix-loop spawn)", spawner.calls)
			}
		})
	}
}

// Test: failed/terminal review_run status -> run needs_attention.
func TestReviewRunFailedNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunFailed, domain.VerdictNone)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepFailed {
		t.Fatalf("review step state = %q, want failed", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}

// Test: ambiguous (invalid verdict while complete) -> run needs_attention,
// never silently approved.
func TestReviewRunCompleteWithInvalidVerdictIsAmbiguous(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictNone)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}

// Test: a review run genuinely still running, fresh (under the staleness
// threshold) stays running/waiting — recovery/read must not force it to
// needs_attention prematurely.
func TestReviewStillRunningFreshStaysRunning(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(time.Minute) // well under the 30-minute staleness threshold

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state = %q, want still running", reviewStepFrom(final).Step.State)
	}
	if final.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want not needs_attention while review is genuinely fresh", final.Run.State)
	}
}

// TestReviewerCapacityStall_ScopedAgentHealthEvent is Checkpoint 8P-D.3's
// per-(user,profile) isolation proof: a reviewer capacity stall recorded for
// one user's connection must land under that exact (harness,user,profile)
// key and must never appear under any other user's key, mirroring
// TestCapacityScope_CrossUserIsolation's own proof for the underlying
// primitive — this test instead proves handleReviewerCapacityStall (the new
// caller) actually threads resolveRuntimeEnv's scope through, using
// fakeStore's real scoped/legacy key separation (workflow_test.go) rather
// than re-testing the primitive itself.
func TestReviewerCapacityStall_ScopedAgentHealthEvent(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	owner, profileID := domain.UserID("user-a"), domain.ProviderProfileID("profile-a-codex")
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, Clock: clk.Now,
		RuntimeIsolation: &fakeRuntimeIsolation{owner: owner, profileID: profileID},
		NewID:            func() string { idSeq++; return fmt.Sprintf("id%d", idSeq) },
	})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after dispatch")
	}
	sessionID := domain.SessionID(*workStepFrom(got).Step.SessionID)

	clk.Advance(1 * time.Second)
	sessionFacts.put(domain.SessionRecord{
		ID: sessionID, ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: clk.Now()}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	clk.Advance(25 * time.Second) // past reviewerStallGrace (20s), nowhere near reviewStalenessThreshold

	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	scopedA, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, owner, profileID)
	if err != nil || !ok {
		t.Fatalf("GetAgentHealthScoped(user-a): ok=%v err=%v", ok, err)
	}
	if scopedA.State != domain.AgentHealthCooldown || scopedA.FailureClass != domain.WorkflowErrorCapacityExhausted {
		t.Fatalf("scoped health for user-a = %+v, want cooldown/capacity_exhausted", scopedA)
	}

	// A different user's identical (harness) connection must never see it.
	if _, ok, err := store.GetAgentHealthScoped(ctx, domain.HarnessCodex, "user-b", "profile-b-codex"); err != nil || ok {
		t.Fatalf("GetAgentHealthScoped(user-b): ok=%v err=%v, want not found (never leaked from user-a)", ok, err)
	}
}

// Test: a review run stuck at "running" past the staleness threshold ->
// waiting/needs_attention, never silently assumed approved.
func TestReviewStaleRunningNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	clk.Advance(31 * time.Minute)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(final).Step.State)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", final.Run.State)
	}
}

// ReviewerIdentity/ProbeReviewer/CancelReviewer make this fake a
// workflowcore.ReviewerEnsurer, mirroring the production launcher: the identity
// is derived purely from the review run id, so it is knowable before the launch
// and askable afterwards.
func (f *fakeReviewerLauncher) ReviewerIdentity(req workflowcore.ReviewerLaunchRequest) string {
	return "workflow-review-" + req.RunID
}

func (f *fakeReviewerLauncher) ProbeReviewer(_ context.Context, ref workflowcore.ReviewerRef) (workflowcore.ReviewerObservation, error) {
	handleID := ref.HandleID
	f.probeCalls++
	if f.afterProbe != nil {
		hook := f.afterProbe
		f.afterProbe = nil
		defer hook()
	}
	// A LIVE SESSION ALWAYS HAS AN INCARNATION. The real runtime learns `$N` at
	// creation and reports it with every observation, so a fake that could
	// answer "owned, instance unknown" would be expressing a state production
	// cannot reach — and would hide the refusal that state is supposed to
	// trigger. Tests that want that state set ownedWithoutInstance explicitly.
	if !f.ownedWithoutInstance && (f.externalLive[handleID] || f.externalExited[handleID]) {
		if f.instances == nil {
			f.instances = map[string]string{}
		}
		if f.instances[handleID] == "" {
			f.instanceSeq++
			f.instances[handleID] = "$" + strconv.Itoa(f.instanceSeq)
		}
	}
	// A ref that carries an INSTANCE addresses that exact incarnation. If the
	// name now holds a different one, the answer is about neither — which is the
	// property persisting the instance exists to give.
	if ref.Known() && f.instances[handleID] != ref.InstanceID {
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceAbsent}, nil
	}
	if f.probeErrorsOnce {
		f.probeErrorsOnce = false
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceUnknown}, errors.New("probe transport failed")
	}
	if f.probeUnknown {
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceUnknown}, nil
	}
	if f.foreign[handleID] {
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceForeign, InstanceID: f.instances[handleID]}, nil
	}
	// Ours, but finished: the session name is still taken while the process
	// behind it is gone. Modelled separately from `owned` because production
	// distinguishes them, and a fake that collapsed them would hide exactly the
	// adopt-a-corpse bug this classification exists to prevent.
	if f.externalExited[handleID] {
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceExited, InstanceID: f.instances[handleID]}, nil
	}
	if f.externalLive[handleID] {
		return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceOwned, InstanceID: f.instances[handleID]}, nil
	}
	return workflowcore.ReviewerObservation{Presence: workflowcore.ReviewerPresenceAbsent}, nil
}

// CancelReviewer terminates only what AO owns, exactly as the production
// launcher does: a foreign session is refused, never destroyed.
func (f *fakeReviewerLauncher) CancelReviewer(_ context.Context, ref workflowcore.ReviewerRef) error {
	handleID := ref.HandleID
	f.cancelCalls++
	if f.strictOwnership && (f.probeUnknown || f.foreign[handleID] ||
		(!f.externalLive[handleID] && !f.externalExited[handleID])) {
		// Production's refusal, verbatim in shape: proof of ownership or nothing
		// happens, and the refusal is marked deterministic.
		return fmt.Errorf("%w: reviewer %s cannot be proven AO's own", workflowcore.ErrUnrecoverable, handleID)
	}
	if ref.Known() && f.instances[handleID] != ref.InstanceID {
		// The incarnation AO verified is gone; whatever holds the name now is
		// not it, and must not be destroyed.
		return nil
	}
	if f.foreign[handleID] {
		return errors.New("refusing to terminate a session AO does not own")
	}
	delete(f.externalLive, handleID)
	delete(f.externalExited, handleID)
	delete(f.instances, handleID)
	return nil
}
