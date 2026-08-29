package workflow_test

// Regression suite for incident wf-170b16ce (task 1 of wf-e1339e29), the first
// real production Autonomous workflow after P1 GO.
//
// What the operator saw: "Task 1 of 8 — Verification — AO está verificando el
// resultado — local-verify", unchanged for 49 minutes, while a process
// inspection found no `go test`, no `go vet`, no verifier child of any kind.
//
// What the durable state actually said:
//
//	workflow_attempts  wfa-verify-1892df…  local-verify
//	                   started 21:17:02.726  finished 21:17:03.024
//	                   outcome failed  error_class verify_artifact_missing
//	workflow_steps     verify  state=waiting     (never running, never terminal)
//	workflow_runs      wf-170b16ce  state=waiting
//	checkpoints        verify_started -> verify_result -> verify_fix_reentry
//	                   "fix: verification failed (verify_artifact_missing) —
//	                    handing findings back to the fix worker (cycle 1 of 3)"
//	workflow_steps     fix  state=pending        (never dispatched)
//
// So there was no stuck verifier and no lost process. Local verification is a
// synchronous in-daemon `cmd.Run()` with a bounded timeout (daemon's
// workflowVerifyRunner) — it cannot outlive the call that started it. The
// verification had finished, and failed, in 298 milliseconds. Three defects
// stacked into the appearance of a hung verifier:
//
//  1. FALSE NEGATIVE. The task's only change was `docs/p2-project-memory-audit.md`
//     at the repository root. Its one command, `gofmt -l .`, resolved into the
//     Go module at `backend/`, so the spec's path context became "backend" and
//     the file check was evaluated at `backend/docs/p2-project-memory-audit.md`.
//     The artifact existed exactly where the plan said it did.
//
//  2. UNDISPATCHABLE RE-ENTRY. The review policy had SKIPPED review for a
//     docs-only change, so the review step completed with no review_run.
//     finishVerifyFailure parked the run at `waiting` with a verify_fix_reentry
//     checkpoint; maybeDispatchVerifyFix returned immediately on
//     `reviewStep.ReviewRunID == nil`. Nothing else in the cascade acts on a
//     waiting verify step, so the re-entry could never be answered by anything.
//
//  3. NO CONVERGENCE. Parking was unconditional. A verify step that rests
//     non-terminal is rendered as live work, so the run advertised a running
//     verification forever, with nothing running and nothing scheduled.
//
// These tests pin all three, and the invariant that covers gaps like them:
// a verify step may rest in a non-terminal state only while some path can still
// move it.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- fixture ---------------------------------------------------------------

// reentryOptions selects the two shapes the incident turned on: whether the
// review step carries a real review run or a durably recorded policy SKIP, and
// what state the fix step is in.
type reentryOptions struct {
	// reviewSkippedByPolicy makes the review step complete with NO review run
	// and a `review_policy_decision` checkpoint recording ReviewSkipped — the
	// production shape of a docs-only change.
	reviewSkippedByPolicy bool
	// fixState overrides the fix step's state (default: waiting).
	fixState domain.WorkflowStepState
}

// reentryFixture is incidentFixture's shape with the review authority and the
// fix step made selectable. It deliberately reuses the same fake wiring so a
// difference between the two suites is a difference in the code under test.
func reentryFixture(
	t *testing.T, root string, plan workflowcore.VerificationPlan,
	runner workflowcore.VerifyRunner, opts reentryOptions,
) (*workflowcore.Coordinator, *fakeStore, *fakeMessageSender, string) {
	t.Helper()
	store := newFakeStore()
	now := time.Date(2026, 8, 29, 21, 17, 2, 0, time.UTC)
	runID, sid, reviewID := "wf-170b16ce", "agent-orchestrator-51", "review-170b16ce"

	artifact := workflowcore.BuildPlanArtifact("agent-orchestrator", "audit context/memory sources", "v1", plan)
	raw, err := workflowcore.MarshalPlanArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	fixState := opts.fixState
	if fixState == "" {
		fixState = domain.WorkflowStepWaiting
	}
	reviewStep := domain.WorkflowStep{
		ID: "review", WorkflowRunID: runID, Kind: domain.WorkflowStepReview,
		Ordinal: 3, State: domain.WorkflowStepCompleted,
	}
	if !opts.reviewSkippedByPolicy {
		reviewStep.ReviewRunID = &reviewID
	}
	store.steps[runID] = []domain.WorkflowStep{
		{ID: "plan", WorkflowRunID: runID, Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, ArtifactJSON: raw},
		{ID: "work", WorkflowRunID: runID, Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &sid},
		reviewStep,
		{ID: "fix", WorkflowRunID: runID, Kind: domain.WorkflowStepFix, Ordinal: 4, State: fixState},
		{ID: "verify", WorkflowRunID: runID, Kind: domain.WorkflowStepVerify, Ordinal: 5, State: domain.WorkflowStepPending},
		{ID: "advance", WorkflowRunID: runID, Kind: domain.WorkflowStepAdvance, Ordinal: 6, State: domain.WorkflowStepPending},
	}
	store.runs[runID] = domain.WorkflowRun{
		ID: runID, ProjectID: "agent-orchestrator", Objective: "audit context/memory sources",
		State: domain.WorkflowRunWaiting, PolicyVersion: "v1", PolicySnapshot: `{"maxFixCycles":3}`,
		CreatedAt: now, UpdatedAt: now,
	}

	approved := cleanObservation(root)
	workStepID, reviewStepID := "work", "review"
	store.checkpoints[runID] = []domain.WorkflowCheckpoint{{
		ID: "work-cp", WorkflowRunID: runID, WorkflowStepID: &workStepID, ProjectID: "agent-orchestrator",
		SessionID: &sid, Branch: "feature", WorktreePath: root,
		FingerprintAfter: workflowcore.WorkspaceFingerprint(approved), CreatedAt: now,
	}}

	reviews := newFakeReviewRuns()
	if opts.reviewSkippedByPolicy {
		// The durable proof maybeVerify already demands for this shape, written
		// exactly as review_policy_dispatch.go writes it. The phase string is
		// spelled out rather than imported: it is a durable contract, and a
		// rename that broke reading production rows should fail here.
		decision, derr := json.Marshal(workflowcore.ReviewPolicyDecision{
			PolicyVersion: "v1",
			Decision:      workflowcore.ReviewSkipped,
			EvaluatedAt:   now,
		})
		if derr != nil {
			t.Fatal(derr)
		}
		store.checkpoints[runID] = append(store.checkpoints[runID], domain.WorkflowCheckpoint{
			ID: "review-policy-cp", WorkflowRunID: runID, WorkflowStepID: &reviewStepID,
			ProjectID: "agent-orchestrator", DurablePhase: "review_policy_decision",
			RetryState: string(decision), PayloadVersion: "v1", CreatedAt: now,
		})
	} else {
		reviews.runs[reviewID] = domain.ReviewRun{
			ID: reviewID, SessionID: domain.SessionID(sid), Harness: domain.ReviewerClaudeCode,
			Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
			TargetSHA: workflowcore.WorkspaceFingerprint(approved),
		}
	}

	facts := newFakeSessionFacts()
	facts.put(domain.SessionRecord{
		ID: domain.SessionID(sid), ProjectID: "agent-orchestrator",
		Activity: domain.Activity{State: domain.ActivityActive},
		Metadata: domain.SessionMetadata{Branch: "feature", WorkspacePath: root},
	})

	sender := &fakeMessageSender{}
	clk := &fakeClock{t: now}
	ids := 0
	c := workflowcore.New(workflowcore.Deps{
		Store: store, ReviewRuns: reviews, WorkspaceFacts: &mutableWorkspaceFacts{obs: approved},
		SessionFacts: facts, Verifier: runner, MessageSender: sender,
		Clock: clk.Now, NewID: func() string { ids++; return fmt.Sprintf("rx%d", ids) },
	})
	return c, store, sender, runID
}

// docsWorktree is the incident's repository shape: a Go module below the root,
// and the task's only artifact at the ROOT — the mirror image of wf-6528a538.
func docsWorktree(t *testing.T) string {
	t.Helper()
	return writeWorktree(t, map[string]string{
		"backend/go.mod":                  "module x\n",
		"backend/main.go":                 "package main\n",
		"docs/p2-project-memory-audit.md": "# audit\n",
		"README.md":                       "# repo\n",
	})
}

// docsPlan is the incident's verification spec: one Go command whose working
// directory resolves into the module, and one repository-root file artifact.
func docsPlan() workflowcore.VerificationPlan {
	return workflowcore.VerificationPlan{
		Commands: []workflowcore.VerificationCommandCheck{
			{Command: "go", Args: []string{"build", "./..."}, WorkingDirectory: ".", RequiredExitCode: 0, RetrySafe: true},
		},
		Files: []workflowcore.VerificationFileCheck{
			{Path: "docs/p2-project-memory-audit.md", Exists: true},
		},
	}
}

func reentryStepState(t *testing.T, store *fakeStore, runID, stepID string) domain.WorkflowStepState {
	t.Helper()
	for _, s := range store.steps[runID] {
		if s.ID == stepID {
			return s.State
		}
	}
	t.Fatalf("step %q not found", stepID)
	return ""
}

func reentryHasPhase(store *fakeStore, runID, phase string) bool {
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == phase {
			return true
		}
	}
	return false
}

func reentryCountPhase(store *fakeStore, runID, phase string) int {
	n := 0
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == phase {
			n++
		}
	}
	return n
}

// ---- A: the false negative that started the incident ------------------------

// A repository-root artifact is not missing because the spec's commands happen
// to run in a subdirectory. This is the exact check that failed in production.
func TestVerifyFindsRepoRootArtifactWhenCommandsResolveIntoAModule(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	c, store, sender, runID := reentryFixture(t, root, docsPlan(), runner, reentryOptions{})

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for an artifact that was exactly where the plan said", sender.calls)
	}
	res := latestVerifyResult(t, store, runID)
	if !res.Passed {
		t.Fatalf("verification failed on a present artifact: %+v", res.Checks)
	}
	// The durable record must say which reading actually satisfied the check,
	// so an operator can see the namespace question was asked and answered.
	if got := onlyFileCheck(t, res).ResolvedPath; got != "docs/p2-project-memory-audit.md" {
		t.Fatalf("resolved path = %q, want the repository-root reading that exists", got)
	}
}

// The wf-6528a538 direction is unchanged: an artifact that lives in the
// commands' module is still found there, and still recorded as such.
func TestVerifyStillPrefersTheCommandNamespaceWhenTheArtifactIsThere(t *testing.T) {
	root := incidentWorktree(t)
	runner := goModuleRunner(root)
	c, store, sender, runID := reentryFixture(t, root,
		postrunqaPlan("internal/postrunqa/classify.go"), runner, reentryOptions{})

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times", sender.calls)
	}
	if got := onlyFileCheck(t, latestVerifyResult(t, store, runID)).ResolvedPath; got != "backend/internal/postrunqa/classify.go" {
		t.Fatalf("resolved path = %q, want the command namespace that holds the file", got)
	}
}

// A genuinely missing artifact is still a genuine failure, in both readings.
func TestVerifyStillFailsWhenTheArtifactIsInNeitherNamespace(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	c, store, _, runID := reentryFixture(t, root, plan, runner, reentryOptions{})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	res := latestVerifyResult(t, store, runID)
	if res.Passed {
		t.Fatal("a missing required artifact passed verification")
	}
	if res.ErrorClass != domain.WorkflowErrorVerifyArtifactMissing {
		t.Fatalf("errorClass = %q, want %q", res.ErrorClass, domain.WorkflowErrorVerifyArtifactMissing)
	}
	// Reported against the namespace, exactly as before the fallback existed.
	if got := onlyFileCheck(t, res).ResolvedPath; got != "backend/docs/never-written.md" {
		t.Fatalf("resolved path = %q, want the namespace reading for a file in neither", got)
	}
}

// "Must be absent" has to mean absent in BOTH readings, or the fallback would
// let a check whose whole purpose is to prove a file is gone pass on a file
// that is still there under the other interpretation.
func TestVerifyAbsenceCheckFailsWhenTheArtifactExistsInEitherNamespace(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	// Absent under the command namespace (backend/docs/…), present at the root.
	plan.Files[0] = workflowcore.VerificationFileCheck{Path: "docs/p2-project-memory-audit.md", Exists: false}
	c, store, _, runID := reentryFixture(t, root, plan, runner, reentryOptions{})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	res := latestVerifyResult(t, store, runID)
	if res.Passed {
		t.Fatal("an absence check passed while the artifact sat at the repository root")
	}
	if res.ErrorClass != domain.WorkflowErrorVerifyArtifactMismatch {
		t.Fatalf("errorClass = %q, want %q", res.ErrorClass, domain.WorkflowErrorVerifyArtifactMismatch)
	}
	if got := onlyFileCheck(t, res).ResolvedPath; got != "docs/p2-project-memory-audit.md" {
		t.Fatalf("resolved path = %q, want the reading that proves the file is still there", got)
	}
}

// ---- B: the re-entry that could never be answered ---------------------------

// The incident's own shape: review SKIPPED by policy, verification fails
// repairably, budget remains. The fix cycle must be dispatched.
func TestVerifyFixReentryDispatchesWhenReviewWasSkippedByPolicy(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md" // a real, repairable failure
	c, store, sender, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{reviewSkippedByPolicy: true})

	// Two polls, deliberately. The cascade dispatches the verify-driven fix
	// (step 5) BEFORE it runs verification (step 6), so the poll that discovers
	// the failure is the one that records the re-entry, and the next one is the
	// one that answers it. That is the same one-poll lag the review-driven fix
	// cycle has, and it is fine precisely because the run is left in a state a
	// later poll acts on — which is the whole difference from the incident.
	for i := 0; i < 2; i++ {
		if _, err := c.GetRun(context.Background(), runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if !reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixReentry) {
		t.Fatal("no verify_fix_reentry checkpoint: the repairable failure did not open a fix cycle")
	}
	if sender.calls != 1 {
		t.Fatalf("fix prompts delivered = %d, want exactly 1: a policy-skipped review must still be able to repair", sender.calls)
	}
	if got := stepState(t, store, runID, "fix"); got != domain.WorkflowStepRunning {
		t.Fatalf("fix step = %q, want running", got)
	}
	// And the run must never be resting on an attention stop that says the
	// cycle was impossible, because it was not.
	if reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixUnavailable) {
		t.Fatal("recorded verify_fix_unavailable for a cycle it went on to dispatch")
	}
}

// One fix per re-entry, however often the run is polled. This is the property
// that stops a 2-second read poll from re-delivering the same findings.
func TestVerifyFixReentryUnderSkippedReviewIsDispatchedExactlyOnce(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	c, store, sender, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{reviewSkippedByPolicy: true})

	for i := 0; i < 4; i++ {
		if _, err := c.GetRun(context.Background(), runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if sender.calls != 1 {
		t.Fatalf("fix prompts delivered = %d across four polls, want exactly 1", sender.calls)
	}
	if got := reentryCountPhase(store, runID, workflowcore.ReasonVerifyFixReentry); got != 1 {
		t.Fatalf("verify_fix_reentry checkpoints = %d, want 1", got)
	}
}

// ---- C: the invariant — never park on a promise AO cannot keep --------------

// A repairable failure with budget left, whose fix cycle is structurally
// impossible, must CONVERGE. Before this it took the re-entry branch anyway and
// rested at `waiting` on a verify step the UI renders as live work.
func TestVerifyConvergesWhenTheFixCycleCanNeverRun(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	// A terminal fix step is a permanent blocker: no later poll can revive it.
	c, store, sender, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{fixState: domain.WorkflowStepCancelled})

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State == domain.WorkflowRunWaiting {
		t.Fatal("run rests at waiting on a fix cycle that can never be dispatched — the incident's shape")
	}
	if detail.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run = %q, want needs_attention (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if got := stepState(t, store, runID, "verify"); got == domain.WorkflowStepWaiting {
		t.Fatal("verify step rests at waiting: it would still render as a running verification")
	}
	if reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixReentry) {
		t.Fatal("wrote a verify_fix_reentry nothing could ever answer")
	}
	if !reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixUnavailable) {
		t.Fatalf("no verify_fix_unavailable stop recorded; phases: %v", reentryAllPhases(store, runID))
	}
	if sender.calls != 0 {
		t.Fatalf("delivered %d fix prompts through a terminal fix step", sender.calls)
	}
}

// The stop must name the blocker rather than blame a budget that was never the
// constraint — raising maxFixCycles would change nothing here.
func TestVerifyFixUnavailableStopNamesTheBlockerNotTheBudget(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	c, store, _, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{fixState: domain.WorkflowStepFailed})

	if _, err := c.GetRun(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if reentryHasPhase(store, runID, workflowcore.ReasonVerifyBudgetExhausted) {
		t.Fatal("blamed the fix budget for a stop the budget had nothing to do with")
	}
	var detail string
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase == workflowcore.ReasonVerifyFixUnavailable {
			detail = cp.NextAction
		}
	}
	if detail == "" {
		t.Fatalf("verify_fix_unavailable stop carries no explanation; phases: %v", reentryAllPhases(store, runID))
	}
}

// Repeated reconciliation of a converged run is idempotent: one stop, not one
// per poll. The write storm this guards against is the wf-04e8309d shape.
func TestVerifyFixUnavailableStopIsRecordedOnce(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	c, store, _, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{fixState: domain.WorkflowStepCancelled})

	for i := 0; i < 3; i++ {
		if _, err := c.GetRun(context.Background(), runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if got := reentryCountPhase(store, runID, workflowcore.ReasonVerifyFixUnavailable); got != 1 {
		t.Fatalf("verify_fix_unavailable checkpoints = %d across three polls, want 1", got)
	}
}

// ---- D: a passing verification is unaffected by any of the above ------------

func TestVerifyStillCompletesUnderAPolicySkippedReview(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	c, store, sender, runID := reentryFixture(t, root, docsPlan(), runner,
		reentryOptions{reviewSkippedByPolicy: true})

	detail, err := c.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	if sender.calls != 0 {
		t.Fatalf("fix worker dispatched %d times for a passing verification", sender.calls)
	}
	if reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixReentry) {
		t.Fatal("opened a fix cycle for a verification that passed")
	}
}

func reentryAllPhases(store *fakeStore, runID string) []string {
	var out []string
	for _, cp := range store.checkpoints[runID] {
		if cp.DurablePhase != "" {
			out = append(out, cp.DurablePhase)
		}
	}
	return out
}

// ---- E: the incident shape, end to end -------------------------------------

// The exact production combination that stalled wf-170b16ce for 49 minutes,
// driven through the coordinator with nothing stubbed out but the runtime:
//
//	a docs-only change at the repository root
//	+ a review the policy SKIPPED (so no review_run exists)
//	+ a verification whose one command resolves into the Go module
//
// Before the fix this reached `waiting` with a verify_fix_reentry no code path
// could answer, and stayed there. It must now simply finish.
func TestTheP2AIncidentShapeCompletesInsteadOfStalling(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	c, store, sender, runID := reentryFixture(t, root, docsPlan(), runner,
		reentryOptions{reviewSkippedByPolicy: true})

	// Poll the way a long-lived daemon does. The incident survived 52 of these.
	var detail workflowcore.RunDetail
	for i := 0; i < 5; i++ {
		var err error
		detail, err = c.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if detail.Run.State == domain.WorkflowRunWaiting {
		t.Fatalf("the run is still waiting after five polls — the incident shape still stalls (steps: %s)",
			stepStates(detail))
	}
	if detail.Run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run = %q, want completed (steps: %s)", detail.Run.State, stepStates(detail))
	}
	// The verification passed, so no fix cycle was opened and no worker was
	// asked to repair a file that was never missing.
	if sender.calls != 0 {
		t.Fatalf("dispatched %d fix prompts for an artifact that was exactly where the plan said", sender.calls)
	}
	if reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixReentry) {
		t.Fatal("opened a fix cycle for a verification that passed")
	}
	// And no verify step is left resting in a state the UI renders as live work.
	for _, s := range detail.Steps {
		if s.Step.Kind == domain.WorkflowStepVerify && s.Step.State == domain.WorkflowStepWaiting {
			t.Fatal("the verify step rests at waiting: it would still render as 'Verificando' forever")
		}
	}
}

// The same shape, but with a verification that genuinely fails. It must reach a
// fix cycle rather than parking — the other half of the incident, where the
// re-entry was written and could never be answered.
func TestTheP2AIncidentShapeReachesAFixCycleWhenVerificationTrulyFails(t *testing.T) {
	root := docsWorktree(t)
	runner := goModuleRunner(root)
	plan := docsPlan()
	plan.Files[0].Path = "docs/never-written.md"
	c, store, sender, runID := reentryFixture(t, root, plan, runner,
		reentryOptions{reviewSkippedByPolicy: true})

	for i := 0; i < 5; i++ {
		if _, err := c.GetRun(context.Background(), runID); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if !reentryHasPhase(store, runID, workflowcore.ReasonVerifyFixReentry) {
		t.Fatal("a repairable failure under a policy-skipped review opened no fix cycle")
	}
	if sender.calls != 1 {
		t.Fatalf("fix prompts = %d across five polls, want exactly 1", sender.calls)
	}
	if got := reentryStepState(t, store, runID, "fix"); got != domain.WorkflowStepRunning {
		t.Fatalf("fix step = %q after five polls, want running: the re-entry was never answered", got)
	}
}
