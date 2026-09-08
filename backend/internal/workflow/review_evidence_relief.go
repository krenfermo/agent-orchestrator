package workflow

import (
	stdctx "context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// review_evidence_relief.go — deciding, once and durably, whether AO's own
// evidence may lower a change's review floor, and completing a review step that
// earned no reviewer.
//
// The policy itself is pure and lives in domain (EvaluateReviewEvidenceRelief).
// This file is the part that touches durable state: it gathers the four facts
// the policy needs, writes the answer down next to the evidence it stands on,
// and — when the answer is "no reviewer" — walks the review step to completed
// through a door that says exactly which door it was.

// resolveReviewEvidenceRelief evaluates and records whether observed evidence
// lowers this review step's floor.
//
// Called at cycle 1 only, between the evidence pass and the depth decision that
// reads its answer. A failure to persist is returned rather than swallowed: a
// review that skipped a reviewer for a reason AO cannot produce afterwards is
// precisely the thing this record exists to make impossible.
func (c *Coordinator) resolveReviewEvidenceRelief(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	workStep, reviewStep domain.WorkflowStep,
	policyDecision ReviewPolicyDecision,
	tier domain.ReviewRiskTier,
	evidence domain.PreReviewEvidence,
	reviewTargetFingerprint string,
) (domain.ReviewEvidenceRelief, domain.PreReviewEvidence, error) {
	// The report is re-read here only for its ADMISSIONS. Whether it existed and
	// whether it contradicted AO's observations are already stamped on the
	// evidence row by collectPreReviewEvidence, which is the single writer of
	// that record — recomputing them here would be a second opinion about a
	// fact already on disk.
	report, _ := c.workReportForStep(ctx, run.ID, workStep.ID)

	relief := domain.EvaluateReviewEvidenceRelief(domain.ReviewEvidenceReliefInput{
		Tier:                    tier,
		ReviewTargetFingerprint: reviewTargetFingerprint,
		Evidence:                evidence,
		Report:                  report,
		HasReviewBlockingReason: hasEvidenceReliefBlockingReason(policyDecision),
		IsChildRun:              run.ParentWorkflowID != nil,
		Now:                     c.clock(),
	})

	payload, err := json.Marshal(relief)
	if err != nil {
		return relief, evidence, err
	}
	stepID := reviewStep.ID
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     "review_evidence_relief: " + string(relief.Reason),
		DurablePhase:   reviewEvidenceReliefPhase,
		PayloadVersion: domain.PreReviewEvidenceVersion,
		CreatedAt:      relief.DecidedAt,
	})
	return relief, evidence, err
}

// reviewEvidenceReliefForStep re-reads a recorded relief decision. Used by
// Verify's skip door and by the read model; ok=false means "cannot prove a
// reviewer was legitimately skipped", which every caller must treat as a
// refusal rather than a default.
func (c *Coordinator) reviewEvidenceReliefForStep(ctx stdctx.Context, runID, reviewStepID string) (domain.ReviewEvidenceRelief, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return domain.ReviewEvidenceRelief{}, false
	}
	var latest *domain.WorkflowCheckpoint
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID || cp.DurablePhase != reviewEvidenceReliefPhase {
			continue
		}
		if latest == nil || cp.CreatedAt.After(latest.CreatedAt) {
			latest = cp
		}
	}
	if latest == nil {
		return domain.ReviewEvidenceRelief{}, false
	}
	var relief domain.ReviewEvidenceRelief
	if err := json.Unmarshal([]byte(latest.RetryState), &relief); err != nil {
		return domain.ReviewEvidenceRelief{}, false
	}
	return relief, true
}

// applyReviewSkippedByEvidence advances a review step to completed WITHOUT a
// reviewer, because the depth resolved to `none` on evidence AO observed.
//
// It mirrors applyReviewPolicySkip's transitions exactly — the same CAS walk
// through ready->running->completed, the same run-state move — and differs in
// the one way that matters: it writes its OWN durable phase, carrying the
// evidence key and the relief reason.
//
// That separation is the whole point. reviewPolicySkippedPhase means "the paths
// alone said no reviewer was needed". This means "AO ran the checks, watched
// them pass, and the policy allowed the reviewer to be skipped on that basis".
// A reader must be able to tell those apart forever, and a decision taken on
// evidence must name the evidence.
//
// It never fabricates a verdict. No review_run is created, nothing is marked
// approved, and the run's own Verify step still executes on its own authority.
func (c *Coordinator) applyReviewSkippedByEvidence(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	relief domain.ReviewEvidenceRelief,
	evidence domain.PreReviewEvidence,
	depth domain.ReviewDepthDecision,
) (domain.WorkflowStep, error) {
	now := c.clock()
	for _, transition := range []domain.WorkflowStepState{domain.WorkflowStepReady, domain.WorkflowStepRunning, domain.WorkflowStepCompleted} {
		if reviewStep.State == transition {
			continue
		}
		if _, err := c.store.UpdateWorkflowStepState(ctx, reviewStep.ID, reviewStep.State, transition, now); err != nil {
			return reviewStep, err
		}
		reviewStep.State = transition
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, now); err != nil {
			return reviewStep, err
		}
	}

	// The record a person reads to answer "why did nobody review this?". It
	// carries the decision, the evidence it stood on and the exact tree that
	// evidence was taken at, so the answer is checkable rather than asserted.
	payload, err := json.Marshal(reviewSkippedByEvidenceRecord{
		PolicyVersion:     domain.PreReviewEvidenceVersion,
		Reason:            string(relief.Reason),
		EvidenceTargetKey: evidence.TargetKey,
		EvidenceStatus:    string(evidence.Status),
		Fingerprint:       evidence.Fingerprint,
		CommandsExecuted:  evidence.ExecutedCommandCount,
		RiskTier:          string(depth.RiskTier),
		RequestedDepth:    string(depth.Requested),
		EffectiveDepth:    string(depth.Effective),
	})
	if err != nil {
		return reviewStep, err
	}
	stepID := reviewStep.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     "verify",
		DurablePhase:   reviewSkippedByEvidencePhase,
		PayloadVersion: domain.PreReviewEvidenceVersion,
		CreatedAt:      now,
	}); err != nil {
		return reviewStep, err
	}
	return reviewStep, nil
}

// reviewSkippedByEvidenceRecord is the payload of the skip checkpoint. A named
// type rather than a map so the shape is greppable and stays stable.
type reviewSkippedByEvidenceRecord struct {
	PolicyVersion     string `json:"policyVersion"`
	Reason            string `json:"reason"`
	EvidenceTargetKey string `json:"evidenceTargetKey"`
	EvidenceStatus    string `json:"evidenceStatus"`
	Fingerprint       string `json:"fingerprint"`
	CommandsExecuted  int    `json:"commandsExecuted"`
	RiskTier          string `json:"riskTier"`
	RequestedDepth    string `json:"requestedDepth"`
	EffectiveDepth    string `json:"effectiveDepth"`
}

// reviewSkippedByEvidence reports whether this review step was completed
// through the evidence door. It is what Verify consults to know that a review
// step with no review_run is nonetheless legitimately complete.
func (c *Coordinator) reviewSkippedByEvidence(ctx stdctx.Context, runID, reviewStepID string) (reviewSkippedByEvidenceRecord, bool) {
	checkpoints, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return reviewSkippedByEvidenceRecord{}, false
	}
	for i := range checkpoints {
		cp := &checkpoints[i]
		if cp.WorkflowStepID == nil || *cp.WorkflowStepID != reviewStepID || cp.DurablePhase != reviewSkippedByEvidencePhase {
			continue
		}
		var rec reviewSkippedByEvidenceRecord
		if err := json.Unmarshal([]byte(cp.RetryState), &rec); err != nil {
			continue
		}
		return rec, true
	}
	return reviewSkippedByEvidenceRecord{}, false
}

// resolveReviewDepthDecisionWithRelief resolves and durably records the depth
// this review step will run at, with phase 2's evidence relief applied to the
// floor.
//
// It is phase 1's resolveReviewDepthDecision with one input added, and it keeps
// that function's contract exactly: the tier and its reasons come from the
// ReviewPolicyDecision AO already persisted (one risk evaluation, never two),
// the answer is written as its own checkpoint before anything acts on it, and a
// failure to persist is returned rather than swallowed.
func (c *Coordinator) resolveReviewDepthDecisionWithRelief(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	reviewStep domain.WorkflowStep,
	tier domain.ReviewRiskTier,
	riskReasons []string,
	relief domain.ReviewEvidenceRelief,
) (domain.ReviewDepthDecision, error) {
	snapshot := policyForRun(run).EffectiveReviewDepthPolicy()
	decision := domain.ResolveReviewDepthWithEvidence(snapshot, tier, riskReasons, relief, c.clock())
	if err := c.persistReviewDepthDecision(ctx, run, reviewStep, decision, reviewDepthDecisionPhase); err != nil {
		return decision, err
	}
	return decision, nil
}

// DecodeReviewEvidenceReliefForTest exposes the relief decode path to the
// external workflow_test package, mirroring DecodeReviewPolicyDecisionForTest.
func DecodeReviewEvidenceReliefForTest(retryState string) (domain.ReviewEvidenceRelief, bool) {
	var relief domain.ReviewEvidenceRelief
	if err := json.Unmarshal([]byte(retryState), &relief); err != nil || relief.PolicyVersion == "" {
		return domain.ReviewEvidenceRelief{}, false
	}
	return relief, true
}

// evidenceReliefBlockingReasons are the ReviewPolicy reasons that a passing
// test run does not answer.
//
// The set is the whole judgement of phase 2's policy, so it is worth stating
// why each member is here. All of these produce a STANDARD tier — they are not
// sensitive paths, which the tier gate already handles — but each one describes
// a doubt about the QUESTION rather than about the code, and running the suite
// green resolves none of them:
//
//   - insufficient_verify: the plan's own checks are too thin to prove
//     anything. Their passing is therefore worth correspondingly little, and
//     relieving a review on them would be circular.
//   - ambiguous_acceptance: there are no usable acceptance criteria. A test run
//     cannot tell AO whether the change did what somebody wanted.
//   - large_or_multi_module: a change wide enough that one bounded look is
//     already a stretch. Evidence does not make it narrower.
//   - prior_provider_attempts: the worker needed more than one attempt to get
//     here. That is a fact about this delivery a reviewer should weigh.
//   - no_changed_files: AO observed no change at all. Passing tests on an
//     unchanged tree prove the tree, not the work.
//
// Deliberately ABSENT is default_conservative, which is the reason ReviewPolicy
// attaches to ordinary code it found nothing specific to worry about. That case
// — nothing suspicious, and then the checks ran and passed — is exactly the one
// phase 2 exists to make proportional.
var evidenceReliefBlockingReasons = map[ReviewReason]struct{}{
	ReasonInsufficientVerify:    {},
	ReasonAmbiguousAcceptance:   {},
	ReasonLargeOrMultiModule:    {},
	ReasonPriorProviderAttempts: {},
	ReasonNoChangedFiles:        {},
}

// hasEvidenceReliefBlockingReason reports whether a decision carries any reason
// from that set.
//
// A decision with NO reasons at all also blocks: an unexplained decision is not
// evidence that a change is cheap, which is the same rule ReviewRiskTierFor
// applies when it reads an empty reason list as high risk.
func hasEvidenceReliefBlockingReason(decision ReviewPolicyDecision) bool {
	if len(decision.Reasons) == 0 {
		return true
	}
	for _, r := range decision.Reasons {
		if _, blocking := evidenceReliefBlockingReasons[r]; blocking {
			return true
		}
	}
	return false
}
