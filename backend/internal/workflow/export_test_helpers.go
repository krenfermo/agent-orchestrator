package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// export_test_helpers.go — narrow windows onto production functions whose inputs
// a unit fixture cannot realistically stage.
//
// Everything here calls the real implementation; none of it reimplements one.

// ReviewCycleOfKeyForTest exposes the key-parsing fallback so a test can assert
// what it does NOT tell us about a replacement claim: those keys carry no cycle,
// so parsing one yields zero, and a budget recorded under a real cycle would
// read as untouched if recovery relied on it.
func ReviewCycleOfKeyForTest(key string) int {
	return reviewCycleOf(domain.WorkflowOutboxEntry{IdempotencyKey: key})
}

// ReviewLaunchBudgetRemainsForTest exercises the gate every claim-release path
// consults.
//
// Driving a REPLACEMENT claim into that gate through a full dispatch needs the
// whole replacement-authorization flow staged around it; the property under test
// — that the cycle comes from the durable record rather than from the claim's
// key — lives entirely inside this function, so it is exercised here directly.
func (c *Coordinator) ReviewLaunchBudgetRemainsForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) (bool, string, error) {
	return c.reviewLaunchBudgetRemains(ctx, run, step, entry)
}

// ReleaseReviewDispatchClaimForTest exercises the single choke point for
// dispatched -> pending, which applies the retry-budget gate itself.
//
// Reaching it with an exhausted budget through a full dispatch is not possible:
// claim-time allocation refuses the dispatch first, which is correct but hides
// what this gate independently guarantees — that a claim nothing will retry is
// not left advertising a retry.
func (c *Coordinator) ReleaseReviewDispatchClaimForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, why string,
) (domain.WorkflowStep, error) {
	return c.releaseReviewDispatchClaim(ctx, run, step, entry, why)
}

// ResumeReviewLaunchAfterFailureForTest exercises the human-resume protocol
// directly, so a test can place a ledger failure on the resume's OWN history
// read.
//
// Driving it through ContinueRun cannot isolate that read: an earlier
// checkpoint read inside the same call fails closed first, which is correct but
// masks whether this one does.
func (c *Coordinator) ResumeReviewLaunchAfterFailureForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) bool {
	observed, ok := c.observeFailedReviewLaunchGeneration(ctx, run.ID, step.ID, entry)
	if !ok {
		return false
	}
	return c.resumeReviewLaunchAfterFailure(ctx, run, step, entry, observed)
}

// ReviewLaunchGenerationForTest is one observed failed outbox generation, held
// opaquely so a test can capture what a caller SAW at one moment and hand it to
// a resume that runs much later — which is the whole shape of the race the
// binding exists to refuse.
type ReviewLaunchGenerationForTest struct {
	gen reviewLaunchGeneration
}

// RecordIDForTest is the launch-failure record this observation names.
func (g ReviewLaunchGenerationForTest) RecordIDForTest() string { return g.gen.RecordID }

// EpochForTest is the budget epoch the observed failure was produced in.
func (g ReviewLaunchGenerationForTest) EpochForTest() int { return g.gen.Epoch }

// ObserveFailedReviewLaunchGenerationForTest captures the failed generation a
// caller is looking at right now.
func (c *Coordinator) ObserveFailedReviewLaunchGenerationForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry,
) (ReviewLaunchGenerationForTest, bool) {
	gen, ok := c.observeFailedReviewLaunchGeneration(ctx, run.ID, step.ID, entry)
	return ReviewLaunchGenerationForTest{gen: gen}, ok
}

// ResumeReviewLaunchFromGenerationForTest resumes a launch failure against an
// observation taken EARLIER — a resume that was delayed while the world moved
// on. Nothing else can express the Codex race: the point is precisely that the
// observation and the resume are separated in time.
func (c *Coordinator) ResumeReviewLaunchFromGenerationForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, observed ReviewLaunchGenerationForTest,
) bool {
	return c.resumeReviewLaunchAfterFailure(ctx, run, step, entry, observed.gen)
}

// AllocateReviewLaunchAttemptForTest exposes claim-time allocation so a test can
// assert the NUMBER a fresh budget epoch hands out, not merely that one was
// handed out. Driving it through a dispatch reports only success or refusal,
// which cannot distinguish "the new epoch started over" from "it continued the
// superseded epoch's numbering and happened to be under the limit".
func (c *Coordinator) AllocateReviewLaunchAttemptForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	entry domain.WorkflowOutboxEntry, cycleNumber int,
) (int, error) {
	return c.allocateReviewLaunchAttempt(ctx, run, step, entry, cycleNumber)
}

// MaxReviewerLaunchAttemptsForTest is the retry budget the production gate
// applies, so a test asserts against the real limit rather than a copy of it
// that would keep passing if the limit changed.
const MaxReviewerLaunchAttemptsForTest = maxReviewerLaunchAttempts

// ReturnBranchLockFromRepairForTest exposes P1-D's branch-lock return so a test
// can drive the generation refusal directly.
//
// Reaching it through a real direct-branch repair would mean staging a project
// on a real checkout, a held lock, a repairable stop and two repair
// generations, and would still be testing the same one predicate. The
// production function is called; nothing is reimplemented.
func (c *Coordinator) ReturnBranchLockFromRepairForTest(ctx stdctx.Context, origin domain.WorkflowRun, intent domain.RepairIntent) error {
	return c.returnBranchLockFromRepair(ctx, origin, intent)
}

// PlacementIsCurrentForTest exposes P1-D's single staleness predicate.
//
// It is the one gate every authority-bearing operation passes through, so the
// six matrix rows about what a stale placement generation may NOT do are
// assertions about this function. Driving each of them through a full dispatch
// would stage six different launches to exercise one predicate, and would test
// the staging rather than the rule.
func (c *Coordinator) PlacementIsCurrentForTest(ctx stdctx.Context, run domain.WorkflowRun, generation int64) bool {
	return c.PlacementIsCurrent(ctx, placementScopeFor(run), generation)
}

// RequireCurrentPlacementForTest exposes the guard itself, which returns the
// record a caller was authorized against rather than merely a boolean.
func (c *Coordinator) RequireCurrentPlacementForTest(ctx stdctx.Context, run domain.WorkflowRun, generation int64) (domain.ExecutionPlacement, error) {
	return c.requireCurrentPlacement(ctx, run, generation)
}

// AdmitForTest exposes P1-D's unified admission gate.
//
// The gate is the single place capacity, branch authority, placement, provider
// eligibility and the lifecycle generation are combined, and its whole value is
// the REASON it returns. Driving each reason through a full dispatch would
// stage five different worlds to read one field, and several of the reasons
// (dependency, strategy) are decided by callers that never reach dispatch at
// all — so the gate is exercised directly, with the real store behind it.
func (c *Coordinator) AdmitForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	harness domain.AgentHarness, providerWaiting, dependenciesReady, strategyPermits bool,
) (domain.AdmissionDecision, error) {
	return c.Admit(ctx, AdmissionRequest{
		Run: run, Step: step, Harness: harness,
		ProviderWaiting: providerWaiting, DependenciesReady: dependenciesReady,
		StrategyPermits: strategyPermits,
		Capacity:        c.workerCapacityRequest(ctx, run, step),
	})
}

// RecordAdmissionWaitForTest exposes the parking half, so a test can assert
// that a refusal is durably classified and survives a restart.
func (c *Coordinator) RecordAdmissionWaitForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, decision domain.AdmissionDecision,
) error {
	_, err := c.recordAdmissionWait(ctx, run, step, decision)
	return err
}

// EnsureProviderAttemptForTest exposes the ledger's front door.
func (c *Coordinator) EnsureProviderAttemptForTest(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	placement domain.ExecutionPlacement, harness domain.AgentHarness,
) (domain.ProviderAttempt, bool, error) {
	return c.EnsureProviderAttempt(ctx, run, step, placement, harness, "")
}

// AdvanceProviderAttemptForTest exposes the CAS every attempt transition goes
// through, so a test can place an attempt in a state the production path would
// otherwise need a whole launch to reach.
func (c *Coordinator) AdvanceProviderAttemptForTest(
	ctx stdctx.Context, attempt domain.ProviderAttempt, next domain.ProviderAttemptState,
) bool {
	return c.advanceProviderAttempt(ctx, attempt, next, "", "", "", "")
}

// ProveNoMutationForTest exposes §H's proof assembly against a real store, so a
// test can assert which conditions a given world actually satisfies rather than
// asserting the AND of a struct it filled in itself.
func (c *Coordinator) ProveNoMutationForTest(
	ctx stdctx.Context, attempt domain.ProviderAttempt, placement domain.ExecutionPlacement, launchFingerprint string,
) MutationProof {
	return c.ProveNoMutation(ctx, attempt, placement, launchFingerprint)
}

// ReclassifyRepairWorkspaceStopForTest exercises the §12 guard directly.
//
// Reaching it through a real observation would mean staging a provider turn
// receipt, an empty worktree and a disproven repair checkout in one fixture --
// three independent subsystems, none of which is the property under test. What
// this guard guarantees lives entirely inside it: an empty turn is the WORKER's
// answer only when AO can show the worker held the artifact.
func (c *Coordinator) ReclassifyRepairWorkspaceStopForTest(
	ctx stdctx.Context, run domain.WorkflowRun, decision WorkStepDecision,
) WorkStepDecision {
	return c.reclassifyRepairWorkspaceStop(ctx, run, decision)
}

// RepairBaseRefForTest exposes the launch-time base decision: what a repair
// run's checkout must be cut from, and the refusal when AO cannot say.
//
// It is the one answer the incident turned on, and it is taken deep inside a
// dispatch that also needs a provider, a runtime and a capacity slot. Asking it
// directly is what lets a test assert the refusal a LEGACY repair run gets --
// one whose marker predates artifact authority, which is precisely the shape
// every repair created before this change carries.
func (c *Coordinator) RepairBaseRefForTest(
	ctx stdctx.Context, run domain.WorkflowRun,
) (string, domain.RepairArtifactAuthority, bool) {
	return c.repairBaseRef(ctx, run)
}

// AttentionDispositionForTest exposes the stop registry so a test can assert
// that a reason it produces is one AO can actually explain. Membership IS the
// definition of "AO knows what this stop is" (attention.go), so a reason absent
// from it is a run parked with nothing to tell a person.
func AttentionDispositionForTest(reason string) (AttentionDisposition, bool) {
	d, ok := attentionDispositions[reason]
	return d, ok
}
