package workflow

// One authorized review question, one durable identity
// (reviewDispatchIdempotencyKey in review_dispatch.go).
//
// The cycle number was never an identity. It is derived from how many review
// runs have COMPLETED for a session and harness, so it advances only when a
// review finishes — and a newly authorized fresh-review generation therefore
// computed the SAME cycle as the one before it, hit that cycle's already
// acknowledged outbox row, and adopted the review run it belonged to. A new
// question answered with an old answer.
//
// wf-04e8309d: six completed cycles, cycleNumber 7, an acknowledged cycle7 row
// from an earlier dispatch, and an exceptional third fresh-review generation
// that was granted, consumed, and then served by the stale approval it existed
// to replace — re-emitting review_dispatched on every poll and never launching
// a reviewer.

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const idStep = "wfs-review"

// ---- 1/5. ordinary cycles keep exactly the identity they always had --------

// The backward-compatibility guarantee, asserted literally: with no fresh
// generation in force the key is byte-for-byte the historical one, so every
// acknowledged row already on disk stays adoptable by the path that wrote it.
func TestOrdinaryCycleKeyIsByteForByteUnchanged(t *testing.T) {
	for cycle := 1; cycle <= 8; cycle++ {
		legacy := reviewStepOutboxIdempotencyKey(idStep, cycle, domain.ReviewerCodex)
		got := reviewDispatchIdempotencyKey(idStep, cycle, domain.ReviewerCodex, "", 0)
		if got != legacy {
			t.Fatalf("cycle %d: key = %q, want the historical %q", cycle, got, legacy)
		}
	}
	// A purpose with no generation, and a generation with no purpose, are both
	// "not a fresh review" and must not change the key either.
	if got := reviewDispatchIdempotencyKey(idStep, 3, domain.ReviewerCodex, "integration", 0); got != reviewStepOutboxIdempotencyKey(idStep, 3, domain.ReviewerCodex) {
		t.Fatalf("a purpose with no generation changed the key: %q", got)
	}
	if got := reviewDispatchIdempotencyKey(idStep, 3, domain.ReviewerCodex, "", 2); got != reviewStepOutboxIdempotencyKey(idStep, 3, domain.ReviewerCodex) {
		t.Fatalf("a generation with no purpose changed the key: %q", got)
	}
}

// The same generation, asked for any number of times, is one identity — which
// is what makes 100 polls produce one reviewer rather than a hundred.
func TestSameGenerationIsOneStableIdentity(t *testing.T) {
	want := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)
	for i := 0; i < 100; i++ {
		if got := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3); got != want {
			t.Fatalf("poll %d produced a different identity: %q vs %q", i, got, want)
		}
	}
	if !strings.Contains(want, idStep) || !strings.Contains(want, string(domain.ReviewerCodex)) {
		t.Fatalf("the key does not identify the step and harness: %q", want)
	}
}

// ---- 2. a restart recomputes the SAME identity ----------------------------

// Restart-safety is the same property as poll-safety here: the key is a pure
// function of durable facts, so a new process resolves the identical string and
// adopts the review already in flight instead of launching a second one.
func TestRestartRecomputesTheSameIdentity(t *testing.T) {
	before := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)
	// Nothing in-process is carried across; recomputing from the same inputs is
	// exactly what a restarted daemon does.
	after := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)
	if before != after {
		t.Fatalf("a restart resolved a different identity: %q vs %q", after, before)
	}
}

// ---- 3/4. the next generation is a different question ---------------------

func TestEachGenerationGetsItsOwnIdentity(t *testing.T) {
	seen := map[string]int{}
	for gen := 1; gen <= 5; gen++ {
		// The cycle number deliberately does NOT move: that is the exact
		// condition under which the old code collapsed every generation onto
		// one key.
		key := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, gen)
		if prev, dup := seen[key]; dup {
			t.Fatalf("generation %d reuses generation %d's identity %q — it would adopt that generation's review", gen, prev, key)
		}
		seen[key] = gen
	}
	if len(seen) != 5 {
		t.Fatalf("distinct identities = %d, want 5", len(seen))
	}
}

// A generation must never resolve to a PREVIOUS generation's identity, which is
// the specific way an old review got adopted for a new question.
func TestAGenerationNeverResolvesToAnEarlierOne(t *testing.T) {
	for gen := 2; gen <= 6; gen++ {
		key := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, gen)
		for earlier := 1; earlier < gen; earlier++ {
			if key == reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, earlier) {
				t.Fatalf("generation %d collides with %d", gen, earlier)
			}
		}
		// Nor with the ordinary cycle key, whose row is very likely already
		// acknowledged — this is the wf-04e8309d collision itself.
		if key == reviewStepOutboxIdempotencyKey(idStep, 7, domain.ReviewerCodex) {
			t.Fatalf("generation %d collides with the ordinary cycle 7 key, whose row is already acknowledged", gen)
		}
	}
}

// ---- 7. a stale acknowledged row from an earlier cycle cannot block --------

// The failure in one assertion: cycle 7 is spent and acknowledged, a third
// generation is authorized, and it must not land on that row.
func TestStaleAcknowledgedCycleDoesNotBlockANewGeneration(t *testing.T) {
	spent := reviewStepOutboxIdempotencyKey(idStep, 7, domain.ReviewerCodex)
	fresh := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)
	if fresh == spent {
		t.Fatal("the new generation resolves to the spent cycle's key, so it would adopt that cycle's review run")
	}
	if !strings.HasPrefix(fresh, spent) {
		t.Fatalf("the fresh key %q should extend the cycle key %q, so the two stay legible as one lineage", fresh, spent)
	}
}

// ---- 6. different mechanisms are different questions ----------------------

// Each mechanism counts its generations independently, so a bare number would
// collapse an integration attempt 1 and a verify recovery generation 1 onto one
// review run. The purpose is what keeps them apart.
func TestDifferentPurposesWithTheSameGenerationDoNotCollide(t *testing.T) {
	purposes := []string{
		freshReviewPurposeVerifyRecovery,
		freshReviewPurposeIntegration,
		freshReviewPurposeBranchAdvance,
		freshReviewPurposeAmendment,
	}
	seen := map[string]string{}
	for _, p := range purposes {
		key := reviewDispatchIdempotencyKey(idStep, 4, domain.ReviewerCodex, p, 1)
		if prev, dup := seen[key]; dup {
			t.Fatalf("purpose %q collides with %q at generation 1: %q", p, prev, key)
		}
		seen[key] = p
	}
}

// The step and the harness remain part of the identity for a fresh generation,
// exactly as they are for an ordinary cycle.
func TestFreshIdentityStillSeparatesStepsAndHarnesses(t *testing.T) {
	a := reviewDispatchIdempotencyKey("wfs-a", 4, domain.ReviewerCodex, freshReviewPurposeIntegration, 1)
	b := reviewDispatchIdempotencyKey("wfs-b", 4, domain.ReviewerCodex, freshReviewPurposeIntegration, 1)
	if a == b {
		t.Fatal("two different review steps share one identity")
	}
	c := reviewDispatchIdempotencyKey("wfs-a", 4, domain.ReviewerClaudeCode, freshReviewPurposeIntegration, 1)
	if a == c {
		t.Fatal("two different harnesses share one identity")
	}
}

// ---- 8. concurrency -------------------------------------------------------

// Many goroutines resolving the same generation must all agree, and none may
// stray onto a neighbouring generation's identity.
func TestConcurrentResolutionAgreesOnOneIdentityPerGeneration(t *testing.T) {
	const racers = 100
	var wg sync.WaitGroup
	results := make(chan string, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	distinct := map[string]struct{}{}
	for r := range results {
		distinct[r] = struct{}{}
	}
	if len(distinct) != 1 {
		t.Fatalf("concurrent resolution produced %d identities, want 1", len(distinct))
	}
	for k := range distinct {
		if k == reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 2) {
			t.Fatal("concurrent resolution landed on the previous generation")
		}
	}
}

// ---- the target pin shares the dispatch identity --------------------------

// The second half of the same collision, one layer below the outbox key. The
// reviewed target is pinned so every retry of ONE question resolves the same
// workspace; it used to be pinned per cycle, and the cycle does not advance
// across generations — so a newly authorized generation was handed the target
// pinned by the previous one, and adoption by (session, target, harness) then
// bound the new question to the old review run.
func TestPinnedTargetIsScopedToTheDispatchIdentity(t *testing.T) {
	fx := newPinFixture(t)
	prevKey := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, "", 0)
	freshKey := reviewDispatchIdempotencyKey(idStep, 7, domain.ReviewerCodex, freshReviewPurposeIntegration, 3)

	fx.pin(prevKey, 7, "target-of-the-previous-question")

	// The previous question still resolves its own pinned target: retries and
	// restarts of it must stay stable.
	if got, ok := fx.coord.recordedReviewTarget(fx.ctx, fx.runID, idStep, prevKey, 7); !ok || got != "target-of-the-previous-question" {
		t.Fatalf("the previous question lost its pin: %q ok=%v", got, ok)
	}
	// The newly authorized generation must NOT inherit it.
	if got, ok := fx.coord.recordedReviewTarget(fx.ctx, fx.runID, idStep, freshKey, 7); ok {
		t.Fatalf("the new generation inherited the previous question's target %q — it would adopt that question's review run", got)
	}
	// Once it pins its own, that is what it resolves, stably.
	fx.pin(freshKey, 7, "target-of-the-new-question")
	for i := 0; i < 20; i++ {
		got, ok := fx.coord.recordedReviewTarget(fx.ctx, fx.runID, idStep, freshKey, 7)
		if !ok || got != "target-of-the-new-question" {
			t.Fatalf("the new generation's pin is unstable: %q ok=%v", got, ok)
		}
	}
}

// A row written before dispatch keys existed carries none, and must still
// resolve for the ordinary path that wrote it.
func TestLegacyPinWithoutADispatchKeyStillResolvesByCycle(t *testing.T) {
	fx := newPinFixture(t)
	fx.pinLegacy(4, "historical-target")
	got, ok := fx.coord.recordedReviewTarget(fx.ctx, fx.runID, idStep, "", 4)
	if !ok || got != "historical-target" {
		t.Fatalf("a historical pin stopped resolving: %q ok=%v", got, ok)
	}
}

// pinFixture is a minimal durable store holding review_target_observed rows.
type pinFixture struct {
	t     *testing.T
	coord *Coordinator
	store *sqlite.Store
	ctx   stdctx.Context
	runID string
	n     int
}

func newPinFixture(t *testing.T) *pinFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	runID := "wf-pin"
	steps := []domain.WorkflowStep{{ID: idStep, WorkflowRunID: runID, Kind: domain.WorkflowStepReview, Ordinal: 1,
		State: domain.WorkflowStepWaiting, ArtifactJSON: "{}", CreatedAt: now, UpdatedAt: now}}
	run := domain.WorkflowRun{ID: runID, ProjectID: "p", Objective: "o", State: domain.WorkflowRunRunning,
		PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateWorkflowRun(ctx, run, steps); err != nil {
		t.Fatal(err)
	}
	return &pinFixture{t: t, coord: New(Deps{Store: store, Projects: store,
		Clock: func() time.Time { return time.Now().UTC() }}), store: store, ctx: ctx, runID: runID}
}

func (fx *pinFixture) write(state map[string]any, target string) {
	fx.t.Helper()
	fx.n++
	raw, _ := json.Marshal(state)
	stepID := idStep
	if _, err := fx.store.CreateWorkflowCheckpoint(fx.ctx, domain.WorkflowCheckpoint{
		ID: fmt.Sprintf("wfc-pin-%d", fx.n), WorkflowRunID: fx.runID, WorkflowStepID: &stepID,
		ProjectID: "p", FingerprintAfter: target, RetryState: string(raw),
		DurablePhase: reviewTargetDurablePhase, PayloadVersion: "v1", CreatedAt: time.Now().UTC(),
	}); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *pinFixture) pin(dispatchKey string, cycle int, target string) {
	fx.write(map[string]any{"cycle": cycle, "dispatchKey": dispatchKey}, target)
}

func (fx *pinFixture) pinLegacy(cycle int, target string) {
	fx.write(map[string]any{"cycle": cycle}, target)
}
