package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// verified_state_test.go — the test matrix for the verified-tree race.
//
// Every case is named for the property it defends, and A is the incident
// itself: gate-7's run wf-a5cb8fa8 recorded a failed commit and parked a human
// at 04:10:40.245, then committed the identical work 47ms later.

// vsObserve is the observation the commit boundary and verify both take.
func vsObserve(t *testing.T, coord *Coordinator, worktree, branch string) ports.WorkspaceObservation {
	t.Helper()
	obs, err := coord.workspaceFacts.ObserveWorkspace(stdctx.Background(), ports.WorkspaceInfo{
		Path: worktree, Branch: branch, ProjectID: "p",
	})
	if err != nil {
		t.Fatalf("observe %s: %v", worktree, err)
	}
	return obs
}

// vsSeedVerified records a passing verification for the run, exactly as verify
// does: a verify_result checkpoint whose RetryState carries the VerifyResult.
func vsSeedVerified(t *testing.T, coord *Coordinator, run domain.WorkflowRun, step domain.WorkflowStep, result VerifyResult) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	stepID := step.ID
	if _, err := coord.store.CreateWorkflowCheckpoint(stdctx.Background(), domain.WorkflowCheckpoint{
		ID: "wfc-vs-" + coord.newID(), WorkflowRunID: run.ID, WorkflowStepID: &stepID,
		ProjectID: run.ProjectID, DurablePhase: verifyResultPhase, PayloadVersion: "v1",
		RetryState: string(raw), NextAction: "done", CreatedAt: coord.clock(),
	}); err != nil {
		t.Fatalf("seed verify result: %v", err)
	}
}

// vsBranch reads the run's execution branch from its frozen placement.
func vsBranch(t *testing.T, coord *Coordinator, run domain.WorkflowRun) string {
	t.Helper()
	recall, ok := coord.recallPlacement(stdctx.Background(), run)
	if !ok {
		t.Fatal("the fixture must have a recallable placement")
	}
	return recall.ExecutionBranch
}

// vsDrifts lists the run's reverification checkpoints.
func vsDrifts(t *testing.T, coord *Coordinator, runID string) []domain.WorkflowCheckpoint {
	t.Helper()
	cps, err := coord.store.ListWorkflowCheckpoints(stdctx.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []domain.WorkflowCheckpoint
	for _, cp := range cps {
		if cp.DurablePhase == verifiedStateReverificationPhase {
			out = append(out, cp)
		}
	}
	return out
}

// vsPhases lists every durable phase the run recorded, in order.
func vsPhases(t *testing.T, coord *Coordinator, runID string) []string {
	t.Helper()
	cps, err := coord.store.ListWorkflowCheckpoints(stdctx.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

// A — THE INCIDENT, as a regression. A tree that still holds exactly the
// verified deliverable, but whose observation differs from the recorded verdict,
// must NOT produce a commit failure. Before the fix this returned a plain error
// that completeVerifiedRun turned into autonomous_local_commit_failed and a
// parked run; the same work then committed on the next pass.
func TestVerifiedState_TransientMismatchIsNotACommitFailure(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{
		"src/features/shout.js": "export const shout = (n) => `${n.toUpperCase()}!`;\n",
	})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)

	// A verification that approved a DIFFERENT sample of this same worktree.
	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "3e1f79f00b5e7adcc89fe20e61d6dd765eded5934f7734a0c158472789dae431"
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: stale.Fingerprint, VerifiedState: &stale})

	err := coord.autonomousLocalCommit(ctx, run, step)
	if err == nil {
		t.Fatal("a tree that does not match its verification must not be committed as the verified result")
	}
	var drift *verifiedStateDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("the mismatch must be a verified-state drift, not a commit failure: %T %v", err, err)
	}
	// §5: nothing about this is a failure, so nothing may be recorded as one.
	for _, phase := range vsPhases(t, coord, run.ID) {
		if strings.HasSuffix(phase, "_failed") {
			t.Fatalf("a drift recorded a failure checkpoint %q", phase)
		}
	}
	head := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "status", "--porcelain"))
	if head == "" {
		t.Fatal("the drift path must leave the pending work uncommitted, not commit it anyway")
	}
}

// B — §3: verify RECORDS the canonical state, and the commit boundary reads
// that same record rather than re-deriving a root of its own.
func TestVerifiedState_ProducerAndConsumerShareOneRepresentation(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	branch := vsBranch(t, coord, run)
	state := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))

	if state.WorktreePath == "" {
		t.Fatal("a canonical verified state must name the root it was computed over")
	}
	if state.Fingerprint != WorkspaceFingerprint(vsObserve(t, coord, worktree, branch)) {
		t.Fatal("the canonical fingerprint must be the same digest every other reader uses")
	}
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: state.Fingerprint, VerifiedState: &state})

	got, ok := coord.canonicalVerifiedState(stdctx.Background(), run)
	if !ok {
		t.Fatal("the commit boundary must find the verification's canonical state")
	}
	if got.WorktreePath != state.WorktreePath || got.Fingerprint != state.Fingerprint {
		t.Fatalf("consumer read a different state than the producer wrote:\n want %+v\n got  %+v", state, got)
	}
}

// C — a tree that DOES match its verification commits, and the deliverable is
// what lands. The fix must not have made the boundary refuse honest work.
func TestVerifiedState_MatchingTreeStillCommits(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{
		"src/features/shout.js": "export const shout = (n) => `${n.toUpperCase()}!`;\n",
	})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	state := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: state.Fingerprint, VerifiedState: &state})

	if err := coord.autonomousLocalCommit(ctx, run, step); err != nil {
		t.Fatalf("a tree that matches its verification must commit: %v", err)
	}
	tree := e2eOutput(t, git(t), "-C", worktree, "ls-tree", "-r", "--name-only", "HEAD")
	if !strings.Contains(tree, "src/features/shout.js") {
		t.Fatalf("the verified deliverable is not in the commit; tree=%q", tree)
	}
	if len(vsDrifts(t, coord, run.ID)) != 0 {
		t.Fatal("a matching tree must not record a drift")
	}
}

// D — §5's diagnostic gap: the incident's failure record carried NEITHER
// fingerprint, which is why it could not be explained from its own record. A
// drift must put both sides and the named difference on the checkpoint.
func TestVerifiedState_DriftRecordsBothSidesAndNamesTheDifference(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "deadbeef"
	stale.HeadSHA = "0000000000000000000000000000000000000000"

	drift := &verifiedStateDriftError{verified: stale, observed: newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))}
	if err := coord.recordVerifiedStateDrift(ctx, run, step, drift); err != nil {
		t.Fatalf("record drift: %v", err)
	}
	recs := vsDrifts(t, coord, run.ID)
	if len(recs) != 1 {
		t.Fatalf("drift records = %d, want 1", len(recs))
	}
	cp := recs[0]
	if cp.FingerprintBefore != stale.Fingerprint || cp.FingerprintAfter == "" || cp.FingerprintAfter == cp.FingerprintBefore {
		t.Fatalf("both sides of the comparison must be on the record, got before=%q after=%q", cp.FingerprintBefore, cp.FingerprintAfter)
	}
	if !strings.Contains(cp.NextAction, "reverification_required") {
		t.Fatalf("the record must say reverification, not failure: %q", cp.NextAction)
	}
	if !strings.Contains(cp.NextAction, "head_sha") {
		t.Fatalf("the record must NAME what differed, got %q", cp.NextAction)
	}
}

// E — the difference is explained even when every named component agrees,
// which is the case where only file content moved.
func TestVerifiedState_DifferenceIsAlwaysExplained(t *testing.T) {
	a := VerifiedWorkspaceState{Fingerprint: "x", HeadSHA: "abc", Dirty: true, Changes: []string{"a.js:M"}}
	b := a
	b.Fingerprint = "y"
	if got := a.differenceFrom(b); !strings.Contains(got, "content") {
		t.Fatalf("an unexplained difference must still be described, got %q", got)
	}
	c := a
	c.Dirty = false
	if got := a.differenceFrom(c); !strings.Contains(got, "dirty") {
		t.Fatalf("want the dirty flag named, got %q", got)
	}
	d := a
	d.Changes = []string{"a.js:M", "b.js:??"}
	if got := a.differenceFrom(d); !strings.Contains(got, "b.js") {
		t.Fatalf("want the new path named, got %q", got)
	}
}

// F — §4: a drift invalidates the VERIFICATION. Recording one must advance the
// verification target key so the succeeded attempt can no longer be resumed and
// verify actually re-runs.
func TestVerifiedState_DriftAdvancesTheVerificationTarget(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	plan := VerificationPlan{Commands: []VerificationCommandCheck{}, Files: []VerificationFileCheck{}}
	reviewed := WorkspaceFingerprint(vsObserve(t, coord, worktree, branch))

	base := verificationTargetKey(reviewed, plan)
	if n := coord.verifiedStateDrifts(ctx, run.ID); n != 0 {
		t.Fatalf("drifts = %d, want 0 before any drift", n)
	}
	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "deadbeef"
	if err := coord.recordVerifiedStateDrift(ctx, run, step, &verifiedStateDriftError{verified: stale, observed: stale}); err != nil {
		t.Fatal(err)
	}
	drifts := coord.verifiedStateDrifts(ctx, run.ID)
	if drifts != 1 {
		t.Fatalf("drifts = %d, want 1", drifts)
	}
	advanced := verificationTargetKey(reviewed+"\ndrift:1", plan)
	if advanced == base {
		t.Fatal("a drift must advance the verification target key, or the stale attempt keeps driving completion")
	}
	// And the advance is authorized, so verify opens a new attempt rather than
	// declaring the target ambiguous.
	if !coord.verifyTargetAdvancedByVerifiedStateDrift(ctx, run.ID, domain.WorkflowAttempt{StartedAt: coord.clock().Add(-time.Minute)}) {
		t.Fatal("the target advance must be recognized as AO-authorized")
	}
}

// G — §6: exactly one winner. A run that drifted must not also hold a commit
// for the verdict it invalidated, and a run that committed must hold no drift.
func TestVerifiedState_NeverBothParksAndCommits(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "deadbeef"
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: stale.Fingerprint, VerifiedState: &stale})

	// Re-enter the boundary repeatedly, as the cascade would.
	for i := 0; i < 3; i++ {
		err := coord.autonomousLocalCommit(ctx, run, step)
		var drift *verifiedStateDriftError
		if !errors.As(err, &drift) {
			t.Fatalf("pass %d: want a drift while the verdict is stale, got %v", i, err)
		}
	}
	phases := vsPhases(t, coord, run.ID)
	committed, failed := 0, 0
	for _, p := range phases {
		switch {
		case p == autonomousLocalCommitPhase:
			committed++
		case strings.HasSuffix(p, "_failed"):
			failed++
		}
	}
	if committed != 0 {
		t.Fatalf("a run whose verdict is stale recorded %d commits", committed)
	}
	if failed != 0 {
		t.Fatalf("a drift recorded %d failure checkpoints", failed)
	}
}

// H — the bound. Re-verification is not a retry loop: after
// maxVerifiedStateDrifts a tree that will not hold still is a real incident and
// the run parks, honestly naming why.
func TestVerifiedState_ReverificationIsBounded(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "deadbeef"
	drift := &verifiedStateDriftError{verified: stale, observed: newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))}
	// The boundary only ever runs on a live run, which is the state the bound
	// has to park FROM.
	if _, err := coord.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunRunning, coord.clock()); err != nil {
		t.Fatalf("prepare a live run: %v", err)
	}
	run.State = domain.WorkflowRunRunning

	for i := 0; i < maxVerifiedStateDrifts; i++ {
		if _, _, err := coord.reverifyAfterVerifiedStateDrift(ctx, run, step, drift); err != nil {
			t.Fatalf("re-verification %d must be absorbed, got %v", i, err)
		}
	}
	if n := len(vsDrifts(t, coord, run.ID)); n != maxVerifiedStateDrifts {
		t.Fatalf("recorded drifts = %d, want %d", n, maxVerifiedStateDrifts)
	}
	gotRun, _, err := coord.reverifyAfterVerifiedStateDrift(ctx, run, step, drift)
	if err != nil {
		t.Fatalf("the bound must park the run, not error out: %v", err)
	}
	if gotRun.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the run parked once the bound is spent", gotRun.State)
	}
	if n := len(vsDrifts(t, coord, run.ID)); n != maxVerifiedStateDrifts {
		t.Fatalf("the bound must not record another re-verification, got %d", n)
	}
}

// I — a verification recorded before VerifiedState existed still governs the
// boundary through its digest alone. The fix must not silently stop checking
// older runs.
func TestVerifiedState_LegacyResultStillGovernsTheCommit(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	ctx := stdctx.Background()
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: "an-old-digest-with-no-state"})

	err := coord.autonomousLocalCommit(ctx, run, step)
	var drift *verifiedStateDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("a legacy verdict must still be enforced, got %v", err)
	}
	if drift.verified.WorktreePath != "" {
		t.Fatal("a legacy verdict names no root, so the consumer must fall back to the placement's")
	}
	// And a run with NO verification at all is not governed by this boundary.
	coord2, run2, step2, worktree2 := f5CommitFixture(t, map[string]string{"b.js": "export const b = 2;\n"})
	if err := coord2.autonomousLocalCommit(ctx, run2, step2); err != nil {
		t.Fatalf("a run with no verification must not be blocked by it: %v", err)
	}
	if status := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree2, "status", "--porcelain")); status != "" {
		t.Fatalf("that run's work should have been committed, still dirty: %q", status)
	}
	_ = worktree
}

// J — §7: ephemeral-artifact semantics are unchanged, and nothing broader is
// ignored. A cache directory must not drift a verified tree; a real source file
// must.
func TestVerifiedState_EphemeralArtifactsOnlyAreIgnored(t *testing.T) {
	coord, run, _, worktree := f5CommitFixture(t, map[string]string{"a.js": "export const a = 1;\n"})
	branch := vsBranch(t, coord, run)
	verified := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))

	if err := os.MkdirAll(filepath.Join(worktree, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "__pycache__", "x.cpython-311.pyc"), []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch)); after.Fingerprint != verified.Fingerprint {
		t.Fatalf("a known ephemeral artifact must not drift a verified tree:\n %s", verified.differenceFrom(after))
	}
	// A file that is merely NEW and untracked is not ephemeral, and must drift.
	if err := os.WriteFile(filepath.Join(worktree, "notes.md"), []byte("real work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch)); after.Fingerprint == verified.Fingerprint {
		t.Fatal("an ordinary untracked file must still drift the verified tree; the fix must not broadly ignore files")
	}
}

// A2 — the incident at the level it actually occurred. gate-7 recorded
// autonomous_local_commit_failed and parked wf-a5cb8fa8 for a human, then
// committed the same work 47ms later. Driving completeVerifiedRun with a stale
// verdict is that exact shape, and it must now produce a re-verification
// instead: no failure record, no parked run, and the work left intact.
func TestVerifiedState_CompleteVerifiedRunReverifiesInsteadOfParkingAHuman(t *testing.T) {
	coord, run, step, worktree := f5CommitFixture(t, map[string]string{
		"src/features/shout.js":   "export const shout = (n) => `${n.toUpperCase()}!`;\n",
		"test/unit/shout.test.js": "import { shout } from '../../src/features/shout.js';\n",
	})
	ctx := stdctx.Background()
	branch := vsBranch(t, coord, run)
	if _, err := coord.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunRunning, coord.clock()); err != nil {
		t.Fatalf("prepare a live run: %v", err)
	}
	run.State = domain.WorkflowRunRunning

	stale := newVerifiedWorkspaceState(vsObserve(t, coord, worktree, branch))
	stale.Fingerprint = "3e1f79f00b5e7adcc89fe20e61d6dd765eded5934f7734a0c158472789dae431"
	vsSeedVerified(t, coord, run, step, VerifyResult{Version: verifyResultVersion, Passed: true, PostFingerprint: stale.Fingerprint, VerifiedState: &stale})

	got, _, err := coord.CompleteVerifiedRunForTest(ctx, run, step)
	if err != nil {
		t.Fatalf("a drifted verdict must not error the cascade: %v", err)
	}
	if got.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("§4: a post-verify mismatch must not require a person on its first occurrence")
	}
	for _, phase := range vsPhases(t, coord, run.ID) {
		if phase == "autonomous_local_commit_failed" {
			t.Fatal("§5: a false failed checkpoint was recorded for a drift")
		}
		if phase == autonomousLocalCommitPhase {
			t.Fatal("§6: the run committed under the verdict it had just invalidated")
		}
	}
	if n := len(vsDrifts(t, coord, run.ID)); n != 1 {
		t.Fatalf("re-verification records = %d, want exactly 1", n)
	}
	if status := strings.TrimSpace(e2eOutput(t, git(t), "-C", worktree, "status", "--porcelain")); status == "" {
		t.Fatal("the deliverable must be left intact for the re-verification, not committed or discarded")
	}
}
