package domain

import "time"

// pre_review_evidence.go — the vocabulary of "AO ran the checks itself, before
// anybody reviewed anything", and the one rule that lets that buy a cheaper
// review.
//
// Phase 1 made review depth proportional to a change's risk, and it could not
// make `none` reachable for ordinary code. The reason was structural rather
// than cautious: review runs BEFORE verify, so at the moment AO chose a depth
// the only thing it knew about the change was which paths it touched. `none`
// was therefore only ever effective where ReviewPolicy already skipped the
// reviewer outright — docs, or a single file with an exact-content check.
//
// This file closes that gap. If AO runs the task's own planned verification
// itself, at the exact tree about to be reviewed, through the same authorized
// runtime Verify uses, then at decision time AO holds something it never held
// before: a deterministic, durable, re-checkable answer to "does this change
// actually work". That answer — and ONLY that answer, never the worker's
// account of it — is allowed to lower the floor for ordinary code by one step.
//
// The rule is deliberately narrow, and every clause is a refusal:
//
//   - it applies to the `standard` tier only. `high` is untouched, and so is
//     an unrecognised tier, which still floors at deep;
//   - it requires checks that AO OBSERVED, that ACTUALLY RAN, and that ALL
//     PASSED;
//   - it requires the evidence to be about THIS tree — same fingerprint;
//   - it refuses on any confession by the worker, on any unaddressed
//     criterion, and on anything AO could not read.
//
// A failure, a timeout, an exhausted budget, an unavailable runtime and an
// unattributable result are all the same answer here: no relief. None of them
// is evidence that a change is safe, and treating "AO could not tell" as
// "nothing to see" is the exact mistake the whole review-depth model exists to
// prevent.

// PreReviewEvidenceVersion identifies the shape of a durable evidence record
// and the policy that reads it. Bump it when either changes.
const PreReviewEvidenceVersion = "pre-review-evidence/v1"

// PreReviewEvidenceStatus is the provenance of one evidence record as a whole.
// It mirrors EvidenceStatus's discipline in evidence_snapshot.go: silence is
// never converted into a fact.
type PreReviewEvidenceStatus string

const (
	// PreReviewEvidenceObserved — AO executed the planned checks itself and
	// read their exit codes. The only status that can support relief.
	PreReviewEvidenceObserved PreReviewEvidenceStatus = "observed"
	// PreReviewEvidenceFailed — AO executed the checks and at least one did
	// not pass. A real, useful, durable fact, and never a reason to review
	// less.
	PreReviewEvidenceFailed PreReviewEvidenceStatus = "failed"
	// PreReviewEvidenceUnavailable — AO could not execute the checks: no
	// verification runtime wired, a working directory it could not resolve, a
	// command it refused to run. Says which, in Note.
	PreReviewEvidenceUnavailable PreReviewEvidenceStatus = "unavailable"
	// PreReviewEvidenceUnattributed — results exist but AO cannot tie them to
	// the tree under review, because the workspace moved between running them
	// and reading them. Never resolved in favour of the most plausible tree.
	PreReviewEvidenceUnattributed PreReviewEvidenceStatus = "unattributed"
	// PreReviewEvidenceNotPlanned — the task declares no executable
	// verification at all. Distinct from unavailable: nothing failed, there
	// was simply nothing to run, and a change nobody can check automatically
	// is precisely a change a person should look at.
	PreReviewEvidenceNotPlanned PreReviewEvidenceStatus = "not_planned"
	// PreReviewEvidenceNotNeeded — AO did not run the checks because nothing
	// about this review could have changed if it had.
	//
	// Reachable on exactly one route: a change whose review floor is ALREADY
	// deep, either because its risk tier demands a full independent pass or
	// because the run asked for one. There, max(requested, floor) is deep
	// whatever the evidence says, the deep prompt does not render evidence at
	// all, and Verify runs the same plan itself minutes later on its own
	// authority. Running it early would buy one reuse when the tree holds still
	// and cost one whole wasted suite when a fix cycle moves it.
	//
	// It is its own status rather than `not_planned` or `unavailable` because
	// it is neither: there was a plan, AO could have run it, and it chose not
	// to for a reason a reader should be able to see. Like every other
	// non-observed status it can never support relief — and on this route it
	// could not have anyway.
	PreReviewEvidenceNotNeeded PreReviewEvidenceStatus = "not_needed"
	// PreReviewEvidenceTimedOut — AO started the checks and stopped waiting.
	// Explicitly its own status rather than folded into failed, because a
	// timeout says nothing about whether the work passed.
	PreReviewEvidenceTimedOut PreReviewEvidenceStatus = "timed_out"
)

// Observed reports whether s is the one status that means AO actually watched
// every planned check succeed.
func (s PreReviewEvidenceStatus) Observed() bool { return s == PreReviewEvidenceObserved }

// Valid reports whether s is a recognised status.
func (s PreReviewEvidenceStatus) Valid() bool {
	switch s {
	case PreReviewEvidenceObserved, PreReviewEvidenceFailed, PreReviewEvidenceUnavailable,
		PreReviewEvidenceUnattributed, PreReviewEvidenceNotPlanned, PreReviewEvidenceTimedOut,
		PreReviewEvidenceNotNeeded:
		return true
	default:
		return false
	}
}

// evidenceStatusSeverity ranks how serious a status is, so a record covering
// several checks reports the WORST thing that happened rather than the last.
// `observed` is the only rank that means "nothing went wrong".
//
// The order above `observed` is deliberate: a plain failure is the most
// informative outcome (AO knows what the code did), a timeout says less, an
// unavailable runtime says nothing about the code at all, and `unattributed` is
// the most serious because AO cannot even say which tree the results describe.
func evidenceStatusSeverity(s PreReviewEvidenceStatus) int {
	switch s {
	case PreReviewEvidenceObserved:
		return 0
	case PreReviewEvidenceNotNeeded:
		// Nothing went wrong; AO deliberately did not look.
		return 1
	case PreReviewEvidenceNotPlanned:
		return 2
	case PreReviewEvidenceFailed:
		return 3
	case PreReviewEvidenceTimedOut:
		return 4
	case PreReviewEvidenceUnavailable:
		return 5
	case PreReviewEvidenceUnattributed:
		return 6
	default:
		// A status this build does not recognise is the most serious thing
		// there is, for the same reason an unclassified risk reason is high.
		return 7
	}
}

// DeeperEvidenceFailure returns whichever of a and b is the more serious
// outcome. It is how one pass over several checks collapses to one status
// without a later check's success ever erasing an earlier one's failure.
func DeeperEvidenceFailure(a, b PreReviewEvidenceStatus) PreReviewEvidenceStatus {
	if evidenceStatusSeverity(b) > evidenceStatusSeverity(a) {
		return b
	}
	return a
}

// PreReviewCheckRecord is one executed check, recorded so the result can be
// re-checked rather than merely asserted: what was asked, where, what came
// back, and how long it took.
type PreReviewCheckRecord struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	Command string `json:"command,omitempty"`
	// Directory is worktree-relative, "." for the root.
	Directory string `json:"directory,omitempty"`
	// ExitCode is a pointer so "did not run" is distinguishable from "exited 0".
	ExitCode         *int   `json:"exitCode,omitempty"`
	RequiredExitCode int    `json:"requiredExitCode"`
	Passed           bool   `json:"passed"`
	TimedOut         bool   `json:"timedOut,omitempty"`
	DurationMS       int64  `json:"durationMs,omitempty"`
	StdoutTail       string `json:"stdoutTail,omitempty"`
	StderrTail       string `json:"stderrTail,omitempty"`
	FailureReason    string `json:"failureReason,omitempty"`
}

// PreReviewEvidence is the durable record of one pre-review evidence pass.
//
// TargetKey is what makes it reusable: it is verificationTargetKey over the
// fingerprint AND the narrowed plan that actually ran, so a record can only
// ever be matched against a later execution of the identical plan on the
// identical tree.
type PreReviewEvidence struct {
	Version string                  `json:"version"`
	Status  PreReviewEvidenceStatus `json:"status"`
	// Note explains a non-observed status in terms a person can act on.
	Note string `json:"note,omitempty"`
	// Fingerprint is AO's content fingerprint of the worktree the checks ran
	// against, read immediately before execution.
	Fingerprint string `json:"fingerprint"`
	// FingerprintAfter is the same reading taken immediately AFTER execution.
	// When it differs from Fingerprint the checks describe a tree that no
	// longer exists, and the record is unattributed however well it went.
	FingerprintAfter string `json:"fingerprintAfter,omitempty"`
	// TargetKey identifies (fingerprint, narrowed plan). Reuse compares this
	// and nothing looser.
	TargetKey string `json:"targetKey"`
	// PlanCommandCount and PlanFileCheckCount describe the plan as narrowed.
	PlanCommandCount   int `json:"planCommandCount"`
	PlanFileCheckCount int `json:"planFileCheckCount"`
	// ExecutedCommandCount is how many commands actually ran. Zero with an
	// observed status means the plan declared none, which is not proof of
	// anything and is why relief additionally requires this to be positive.
	ExecutedCommandCount int                    `json:"executedCommandCount"`
	Checks               []PreReviewCheckRecord `json:"checks,omitempty"`
	// Scope records how the plan was narrowed, so a reader can see AO did not
	// run a full suite for three lines — and can see when it did.
	Scope *VerifyScopeSummary `json:"scope,omitempty"`
	// DurationMS is the wall-clock cost of the whole pass, for the phase-cost
	// view P5-A phase 4 will build.
	DurationMS int64     `json:"durationMs"`
	StartedAt  time.Time `json:"startedAt"`
	DecidedAt  time.Time `json:"decidedAt"`
	// ReportRecorded says whether a worker report existed at decision time.
	// The report's CONTENT is stored separately; this is the fact policy reads.
	ReportRecorded bool `json:"reportRecorded,omitempty"`
	// ReportContradicted is set when the worker claimed a command passed that
	// AO observed failing. It is recorded because a worker that misreports its
	// own test results is a fact about the change worth carrying, and it
	// independently blocks relief.
	ReportContradicted bool `json:"reportContradicted,omitempty"`
}

// VerifyScopeSummary is the narrowing decision, flattened into domain so this
// record stays free of internal/workflow's types.
type VerifyScopeSummary struct {
	PolicyVersion string   `json:"policyVersion,omitempty"`
	Scope         string   `json:"scope,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
	Transforms    []string `json:"transforms,omitempty"`
}

// Recorded reports whether e is a real record rather than a zero value.
func (e PreReviewEvidence) Recorded() bool {
	return e.Version != "" && e.Status.Valid()
}

// AllPassed reports whether every recorded check passed and at least one
// command actually ran.
func (e PreReviewEvidence) AllPassed() bool {
	if e.ExecutedCommandCount == 0 {
		return false
	}
	for _, c := range e.Checks {
		if !c.Passed {
			return false
		}
	}
	return true
}

// ReviewEvidenceReliefReason is a stable code explaining why relief was or was
// not granted, so a decision taken today stays explainable after the rules
// change.
type ReviewEvidenceReliefReason string

const (
	// ReliefGranted — the floor was lowered by observed, passing evidence.
	ReliefGranted ReviewEvidenceReliefReason = "granted_observed_evidence"
	// ReliefDeniedTier — the tier is not `standard`.
	ReliefDeniedTier ReviewEvidenceReliefReason = "denied_risk_tier"
	// ReliefDeniedNoEvidence — nothing durable to read.
	ReliefDeniedNoEvidence ReviewEvidenceReliefReason = "denied_no_evidence"
	// ReliefDeniedNotObserved — the evidence exists but AO did not observe a
	// clean pass (failed, timed out, unavailable, unattributed, not planned).
	ReliefDeniedNotObserved ReviewEvidenceReliefReason = "denied_evidence_not_observed"
	// ReliefDeniedFingerprint — the evidence is about a different tree.
	ReliefDeniedFingerprint ReviewEvidenceReliefReason = "denied_fingerprint_mismatch"
	// ReliefDeniedNoCommands — nothing executable actually ran.
	ReliefDeniedNoCommands ReviewEvidenceReliefReason = "denied_no_executed_commands"
	// ReliefDeniedWorkerAdmission — the worker itself reported a failure, a
	// limitation, a risk, or an unaddressed criterion.
	ReliefDeniedWorkerAdmission ReviewEvidenceReliefReason = "denied_worker_admission"
	// ReliefDeniedContradiction — the worker claimed a pass AO observed failing.
	ReliefDeniedContradiction ReviewEvidenceReliefReason = "denied_report_contradiction"
	// ReliefDeniedBlockingReason — ReviewPolicy recorded a reason a passing
	// test run does not answer (thin verification, ambiguous criteria, a large
	// change, prior worker attempts, or nothing observed to have changed).
	ReliefDeniedBlockingReason ReviewEvidenceReliefReason = "denied_blocking_review_reason"
	// ReliefDeniedChildRun — the run executes one step of a parent's plan, and
	// the parent's scrutiny is not the child's to lower.
	ReliefDeniedChildRun ReviewEvidenceReliefReason = "denied_child_run"
	// ReliefNotRequested — the run did not ask for a depth the floor was
	// blocking, so no relief was needed and none was evaluated.
	ReliefNotRequested ReviewEvidenceReliefReason = "not_requested"
)

// ReviewEvidenceRelief is the durable result of asking "may observed evidence
// lower this change's review floor by one step?".
type ReviewEvidenceRelief struct {
	PolicyVersion string                     `json:"policyVersion"`
	Granted       bool                       `json:"granted"`
	Reason        ReviewEvidenceReliefReason `json:"reason"`
	// EvidenceTargetKey is the evidence record this relief stands on, so the
	// decision names its own proof instead of merely claiming one existed.
	EvidenceTargetKey string    `json:"evidenceTargetKey,omitempty"`
	DecidedAt         time.Time `json:"decidedAt"`
}

// ReviewEvidenceReliefInput is everything EvaluateReviewEvidenceRelief needs.
// A struct rather than eight positional arguments because every field is a
// refusal condition and a reader has to be able to see them all at once.
type ReviewEvidenceReliefInput struct {
	Tier ReviewRiskTier
	// ReviewTargetFingerprint is the tree the review is about. Evidence taken
	// on any other tree is not about this change.
	ReviewTargetFingerprint string
	Evidence                PreReviewEvidence
	// Report is the worker's declaration. Read only for admissions, only in
	// the deepening direction.
	Report WorkReport
	// HasReviewBlockingReason is true when ReviewPolicy recorded a reason that
	// a passing test run does not answer.
	//
	// It is deliberately NOT "the decision was REQUIRED". ReviewPolicy resolves
	// to REQUIRED for essentially all ordinary code — that is what
	// ReasonDefaultConservative means, and it is precisely the disproportionate
	// case phase 2 exists to make proportional. Treating REQUIRED itself as a
	// mandate would deny relief to every change it was built for.
	//
	// What DOES block is a reason whose worry green tests cannot settle: a plan
	// whose verification is too thin to prove anything, acceptance criteria
	// nobody can judge against, a change too large for one bounded look, a
	// worker that needed several attempts to get here, or no observed change at
	// all. Those are questions for a reviewer, and a passing suite is not an
	// answer to any of them. See evidenceReliefBlockingReasons.
	HasReviewBlockingReason bool
	// IsChildRun is true when this run was created as a child of another run —
	// which today means a task executing one step of a master's plan.
	//
	// A child never earns relief, and the reason is the master guarantee rather
	// than anything about the child's own risk. A master run asks for a full
	// independent review, but a master does not itself deliver code: its
	// children do. If each child could skip its reviewer on green tests, a
	// master workstream would consist entirely of unreviewed work while still
	// reporting that it required deep review — which is exactly the guarantee
	// phase 1 froze and phase 2 must not quietly spend.
	//
	// The child's own strategy cannot express this: children are created as
	// task-strategy runs, whose default request is `none`. So the fact that
	// matters is parentage, and it is read here directly.
	IsChildRun bool
	Now        time.Time
}

// EvaluateReviewEvidenceRelief is the whole phase-2 policy, and it is pure:
// same inputs, same answer, forever.
//
// It answers exactly one question — may the floor for THIS change drop from
// light to none — and it answers "no" for every reason it can find before it
// answers "yes". The order of the checks is the order of their severity, so
// the recorded reason names the most serious objection rather than the last
// one evaluated.
func EvaluateReviewEvidenceRelief(in ReviewEvidenceReliefInput) ReviewEvidenceRelief {
	out := ReviewEvidenceRelief{
		PolicyVersion:     PreReviewEvidenceVersion,
		DecidedAt:         in.Now,
		EvidenceTargetKey: in.Evidence.TargetKey,
	}
	deny := func(r ReviewEvidenceReliefReason) ReviewEvidenceRelief {
		out.Granted = false
		out.Reason = r
		return out
	}

	// The tier gate first. `high` is non-degradable and an unrecognised tier
	// is treated as high everywhere else in this model, so only an explicit
	// `standard` may be relieved. `low` needs no relief: its floor is already
	// none.
	if in.Tier != ReviewRiskStandard {
		return deny(ReliefDeniedTier)
	}
	// A reason a passing suite does not answer. Distinct from the tier gate
	// above: these are standard-risk changes whose doubt is about the QUESTION
	// rather than about the paths, and running the tests does not resolve it.
	if in.HasReviewBlockingReason {
		return deny(ReliefDeniedBlockingReason)
	}
	// A master's work is done by its children. Relieving them would spend the
	// master guarantee without anybody choosing to.
	if in.IsChildRun {
		return deny(ReliefDeniedChildRun)
	}
	if !in.Evidence.Recorded() {
		return deny(ReliefDeniedNoEvidence)
	}
	if !in.Evidence.Status.Observed() {
		return deny(ReliefDeniedNotObserved)
	}
	// The evidence must be about the tree under review, and it must still have
	// been about it when the run finished. Both readings are compared: a tree
	// that moved DURING the checks makes them describe something that no
	// longer exists.
	if in.ReviewTargetFingerprint == "" || in.Evidence.Fingerprint != in.ReviewTargetFingerprint {
		return deny(ReliefDeniedFingerprint)
	}
	if in.Evidence.FingerprintAfter != "" && in.Evidence.FingerprintAfter != in.Evidence.Fingerprint {
		return deny(ReliefDeniedFingerprint)
	}
	if in.Evidence.ExecutedCommandCount == 0 {
		return deny(ReliefDeniedNoCommands)
	}
	if !in.Evidence.AllPassed() {
		return deny(ReliefDeniedNotObserved)
	}
	if in.Evidence.ReportContradicted {
		return deny(ReliefDeniedContradiction)
	}
	// A worker's confession is the one part of its report policy believes,
	// and it believes it only here, where believing it costs a fuller review.
	if in.Report.Recorded() &&
		(in.Report.AdmitsAnyFailure() || len(in.Report.UnaddressedCriteria()) > 0) {
		return deny(ReliefDeniedWorkerAdmission)
	}

	out.Granted = true
	out.Reason = ReliefGranted
	return out
}

// EvidenceRelievedMinimumDepth is MinimumDepth with phase 2's one exception
// applied. It is the ONLY place the phase-1 floor may be lowered, and it can
// lower it by exactly one step, for exactly one tier.
//
// Everything else — high, unrecognised, and every denied relief — returns
// precisely what phase 1 returned, which is what keeps every phase-1 guarantee
// true by construction rather than by re-testing.
func EvidenceRelievedMinimumDepth(tier ReviewRiskTier, relief ReviewEvidenceRelief) ReviewDepth {
	floor := tier.MinimumDepth()
	if !relief.Granted {
		return floor
	}
	// Defence in depth: relief is only ever honoured against the exact floor it
	// was designed to lower. If a future edit made MinimumDepth(standard)
	// return something else, this refuses rather than lowering whatever it
	// finds.
	if tier == ReviewRiskStandard && floor == ReviewDepthLight {
		return ReviewDepthNone
	}
	return floor
}

// ReviewDepthReasonEvidenceRelieved is the explanation code for a depth that
// only became reachable because AO ran the checks itself. It is deliberately
// distinct from explicit_request and strategy_default: a reader must be able to
// tell that this review was cheap BECAUSE of evidence, and go and read it.
const ReviewDepthReasonEvidenceRelieved ReviewDepthReason = "relieved_by_pre_review_evidence"

// ResolveReviewDepthWithEvidence is ResolveReviewDepth with phase 2's floor
// relief applied.
//
//	effective = max(requested, EvidenceRelievedMinimumDepth(tier, relief))
//
// The shape is identical to phase 1's and so is every guarantee that followed
// from it. A request can still only ever DEEPEN a review; the single thing
// phase 2 changes is that, for ordinary code whose checks AO ran and watched
// pass, the floor a request is measured against can be one step lower.
//
// Passing a zero-valued (ungranted) relief makes this function behave exactly
// as ResolveReviewDepth, which is what lets every existing call site keep its
// meaning.
func ResolveReviewDepthWithEvidence(
	snapshot ReviewDepthPolicySnapshot,
	tier ReviewRiskTier,
	riskReasons []string,
	relief ReviewEvidenceRelief,
	now time.Time,
) ReviewDepthDecision {
	requested := snapshot.Requested
	source := snapshot.Source
	if !requested.Valid() {
		requested = ReviewDepthDeep
		source = ReviewDepthRecovered
	}
	if !source.Recorded() {
		source = ReviewDepthPolicy
	}

	decision := ReviewDepthDecision{
		PolicyVersion: ReviewDepthPolicyVersion,
		Requested:     requested,
		RiskTier:      tier,
		RiskReasons:   riskReasons,
		DecidedAt:     now,
	}

	floor := EvidenceRelievedMinimumDepth(tier, relief)
	effective := DeeperOf(requested, floor)
	decision.Effective = effective

	switch {
	case effective != requested:
		decision.Source = ReviewDepthClamped
		decision.Reason = ReviewDepthReasonClampedByRiskTier
	case source == ReviewDepthRecovered:
		decision.Source = ReviewDepthRecovered
		decision.Reason = ReviewDepthReasonLegacyDefault
	case relief.Granted && effective == ReviewDepthNone:
		// The one new outcome: the request was honoured at a depth the risk
		// tier alone would have refused. Record WHY, and record it as the
		// evidence's doing rather than the requester's.
		decision.Source = source
		decision.Reason = ReviewDepthReasonEvidenceRelieved
	case source == ReviewDepthExplicit:
		decision.Source = ReviewDepthExplicit
		decision.Reason = ReviewDepthReasonExplicitRequest
	default:
		decision.Source = ReviewDepthPolicy
		decision.Reason = ReviewDepthReasonStrategyDefault
	}
	return decision
}
