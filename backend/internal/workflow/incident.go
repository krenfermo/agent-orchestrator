package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident.go — Checkpoint 8P-E.18, the Incident Advisor's durable model.
//
// The problem this exists for is not a bug; it is what a person has to do when
// AO stops. Today a stopped run shows a reason code and a sentence, and every
// question past that sentence — what actually happened, what is frozen, what
// evidence AO holds, what is safe to do — is answered by copying SQL and
// prompts into other terminals. Three of the incidents this repository has
// already lived through (wf-6528a538, wf-57f90ff2, the stale provider
// capacity wait) were each diagnosed that way, by hand, twice.
//
// So: a stopped run can be handed to an isolated Diagnostic Agent along with a
// bounded pack of the evidence AO already holds, and that agent must come back
// with one of exactly four classifications and a proposed action. Nothing it
// proposes executes on its own say-so.
//
// # Why this has no schema of its own
//
// Every durable fact here is written as a workflow_checkpoints row, the same
// append-only ledger every other workflow fact uses. That is not a shortcut
// around a migration — it is the design:
//
//   - an incident is entirely a property of one run, and the ledger is already
//     keyed, ordered and ON DELETE CASCADE'd by run;
//   - the incident and the stop that caused it belong in ONE timeline. A
//     separate table would put the two halves of the same story in two places
//     and force every reader to join them;
//   - it is restart-safe by construction, because reconstruction is a fold over
//     rows that are already written before any side effect they describe;
//   - and it adds no migration, which matters concretely: AO's schema upgrades
//     run against a live database, and this feature must not require one while
//     a plan is mid-flight.
//
// The cost is that incidents cannot be queried across runs without scanning.
// Nothing in this feature needs that: every entry point starts from "this run
// has stopped".
//
// # What it may and may not do
//
// The Advisor never invents a mutation path. For an auto-recoverable incident
// the action it can propose is the one a person already has — POST /continue —
// so the entire "AO fixed it" surface is a mechanism that was already there,
// already bounded, and already evidence-checked. Anything beyond that is a
// Repair Agent, which is a separate agent, in a separate workspace, whose
// output goes through the ordinary independent review and deterministic verify.

// ---- classification ---------------------------------------------------------

// IncidentClass is the Diagnostic Agent's verdict. Exactly four values, and the
// boundaries between them are about EVIDENCE and BLAST RADIUS, not confidence.
type IncidentClass string

const (
	// IncidentAutoRecoverable means the stop has a known, bounded remedy that AO
	// already implements, and the evidence for it is present. The only action
	// this class may propose is one of the allow-listed recoveries in
	// incident_policy.go — never a code change, never a git operation.
	IncidentAutoRecoverable IncidentClass = "auto_recoverable"
	// IncidentRepairAO means the stop is caused by a defect in AO itself. The remedy
	// is a code change to this repository, which means a Repair Agent, an
	// independent reviewer and deterministic verification — never an inline fix
	// by whoever diagnosed it.
	IncidentRepairAO IncidentClass = "repair_ao"
	// IncidentHumanDecision means AO has the evidence and there is genuinely a choice
	// only a person can make (which of two acceptable outcomes, whether to spend
	// more budget, whether the reviewer is right). The Advisor presents concrete
	// options and takes none of them.
	IncidentHumanDecision IncidentClass = "human_decision_required"
	// IncidentUnsafeOrInsufficient means acting would be unsafe, or the evidence does
	// not support any conclusion. This is a first-class successful outcome of a
	// diagnosis, not a failure of one: naming the missing evidence is the whole
	// value, and it is what stops the other three classes from absorbing cases
	// they cannot support.
	IncidentUnsafeOrInsufficient IncidentClass = "unsafe_or_insufficient_evidence"
)

// Valid reports whether c is one of the four. Anything else from an agent is
// rejected outright rather than coerced — a classification AO does not
// recognise is not a classification.
func (c IncidentClass) Valid() bool {
	switch c {
	case IncidentAutoRecoverable, IncidentRepairAO, IncidentHumanDecision, IncidentUnsafeOrInsufficient:
		return true
	}
	return false
}

// RequiresHumanApproval reports whether a class may never execute automatically.
// Note that auto_recoverable is NOT exempt here: whether it needs approval is
// decided by the action's own risk (incident_policy.go), because "recoverable"
// describes the stop, not the blast radius of the remedy.
func (c IncidentClass) RequiresHumanApproval() bool {
	return c != IncidentAutoRecoverable
}

// ---- state machine ----------------------------------------------------------

// IncidentState is where one incident sits. Transitions are total and checked
// (see ValidIncidentTransition); there is no "unknown" state, because an
// incident is derived from rows that were written by the transitions themselves.
type IncidentState string

const (
	// IncidentOpen means AO has recorded that this run is stopped and a person may
	// ask about it. Nothing has been spent yet.
	IncidentOpen IncidentState = "open"
	// IncidentDiagnosing means a Diagnostic Agent is running against a context pack.
	IncidentDiagnosing IncidentState = "diagnosing"
	// IncidentDiagnosed means a classification and a proposed action exist.
	IncidentDiagnosed IncidentState = "diagnosed"
	// IncidentAwaitingApproval means the proposed action needs a person to say yes.
	IncidentAwaitingApproval IncidentState = "awaiting_approval"
	// IncidentExecuting means an approved (or auto-authorized) action is running.
	IncidentExecuting IncidentState = "executing"
	// IncidentResolved means the action ran and the run is no longer stopped on this.
	IncidentResolved IncidentState = "resolved"
	// IncidentRefused means AO declined to act, and said what evidence was missing.
	// Terminal for this incident; a person may open a new one.
	IncidentRefused IncidentState = "refused"
	// IncidentClosed means the stop went away on its own (someone continued the run,
	// the child recovered). Terminal, and it is why the Advisor re-derives the
	// live stop instead of trusting its own record.
	IncidentClosed IncidentState = "closed"
)

// Terminal reports whether an incident can still change.
func (s IncidentState) Terminal() bool {
	return s == IncidentResolved || s == IncidentRefused || s == IncidentClosed
}

// validIncidentTransitions is the whole state machine, written out so it can be
// read. A transition absent here cannot happen, and every write path goes
// through ValidIncidentTransition rather than assuming.
var validIncidentTransitions = map[IncidentState][]IncidentState{
	IncidentOpen:             {IncidentDiagnosing, IncidentClosed},
	IncidentDiagnosing:       {IncidentDiagnosed, IncidentRefused, IncidentClosed},
	IncidentDiagnosed:        {IncidentAwaitingApproval, IncidentExecuting, IncidentRefused, IncidentClosed},
	IncidentAwaitingApproval: {IncidentExecuting, IncidentRefused, IncidentClosed},
	IncidentExecuting:        {IncidentResolved, IncidentRefused, IncidentClosed},
}

// ValidIncidentTransition reports whether from -> to is allowed.
func ValidIncidentTransition(from, to IncidentState) bool {
	for _, allowed := range validIncidentTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ---- durable phases ---------------------------------------------------------
//
// These are the durable_phase values the incident ledger is folded from. They
// are deliberately NOT canonical attention reasons (attention.go): an incident
// describes a stop, it is never itself a reason a run is stopped, and
// ClassifyAttention must never surface one.

const (
	incidentOpenedPhase     = "incident_opened"
	incidentDiagnosingPhase = "incident_diagnosis_requested"
	incidentDiagnosedPhase  = "incident_diagnosed"
	incidentApprovedPhase   = "incident_action_approved"
	incidentExecutingPhase  = "incident_action_executing"
	incidentResolvedPhase   = "incident_resolved"
	incidentRefusedPhase    = "incident_refused"
	incidentClosedPhase     = "incident_closed"
)

// incidentPhases is the set folded by loadIncident, so an unrelated checkpoint
// can never be mistaken for incident history.
var incidentPhases = map[string]IncidentState{
	incidentOpenedPhase:     IncidentOpen,
	incidentDiagnosingPhase: IncidentDiagnosing,
	incidentDiagnosedPhase:  IncidentDiagnosed,
	incidentApprovedPhase:   IncidentAwaitingApproval,
	incidentExecutingPhase:  IncidentExecuting,
	incidentResolvedPhase:   IncidentResolved,
	incidentRefusedPhase:    IncidentRefused,
	incidentClosedPhase:     IncidentClosed,
	// A dispatched repair keeps the incident in `executing`: the work is
	// happening, and only the repair run reaching `completed` may resolve it.
	incidentRepairPhase: IncidentExecuting,
}

// incidentAuxiliaryPhases are incident-ledger rows that are NOT state
// transitions: they record something about an investigation without moving it.
// They are deliberately kept out of incidentPhases so foldIncident cannot read
// one as progress — a capacity wait means nothing started, and folding it into
// `diagnosing` would claim an agent was running when none was.
var incidentAuxiliaryPhases = map[string]bool{
	ReasonIncidentDiagnosisCapacityWait: true,
	incidentLaunchFailedPhase:           true,
	// The started row names the session a launch produced. It is deliberately
	// auxiliary: the incident is already `diagnosing` from the request row, and
	// folding this as a transition would make the state depend on whether a
	// best-effort observability write happened to land.
	incidentDiagnosisStartedPhase: true,
}

// incidentLaunchFailedPhase records a diagnostic launch that never produced a
// pane. It exists because the request row is written BEFORE the launch (so a
// launch AO could not write down is a launch it does not make), which means the
// row alone cannot distinguish "an agent is running" from "an agent failed to
// start". Without this, one transient spawn failure would leave the generation
// looking outstanding until it timed out, and the incident uninvestigable for
// fifteen minutes.
const incidentLaunchFailedPhase = "incident_diagnosis_launch_failed"

// isIncidentLedgerPhase reports whether a checkpoint belongs to the incident
// ledger rather than to the run's own progress.
//
// Every reader that folds "the newest checkpoint" into a run's derived state
// must skip these. An incident row is written the moment a person opens the
// "¿Qué hago?" modal, which makes it instantly the newest row on the run; if it
// counted, merely ASKING about a stop would rewrite the run's derived stop
// reason and lifecycle phase. Asking a question must not change the answer.
// isBookkeepingPhase reports whether a checkpoint records something ABOUT a run
// rather than something the run DID — and must therefore never be folded into
// the run's derived state.
//
// The incident ledger is one such set, for the reason described above. The
// attempt reaper writes the other: closing an attempt a restart abandoned is
// housekeeping on the run's history, not a step of it, and certainly not a
// stop. It is also written at the worst possible moment — on a parked run, from
// the very Continue that is about to ask why the run is parked — so if it
// counted as the newest checkpoint it would displace the real stop phase and
// leave the run unable to say why it stopped. That is exactly the
// wf-3220567f failure this file's guard already prevents once; a second phase
// with the same shape needs the same exclusion, not a second lesson.
//
// Every reader that folds "the newest checkpoint" into derived state uses this,
// not isIncidentLedgerPhase directly.
func isBookkeepingPhase(phase string) bool {
	return isIncidentLedgerPhase(phase) ||
		phase == attemptReapedPhase ||
		phase == verifySupersededPhase ||
		phase == verifyRaceReconciledPhase ||
		// The ambiguity gate's evidence row has exactly the shape this guard is
		// about: it is written at the same instant as the stop it is evidence
		// FOR, so if it counted it would displace that stop's own phase and the
		// run would once again be unable to say why it stopped — with, this
		// time, a checkpoint full of evidence sitting next to it saying nothing
		// about the decision. See ambiguous_worker_state.go.
		phase == AmbiguousWorkerStateEvidencePhase
}

func isIncidentLedgerPhase(phase string) bool {
	if _, ok := incidentPhases[phase]; ok {
		return true
	}
	return incidentAuxiliaryPhases[phase]
}

// ---- bounds -----------------------------------------------------------------

const (
	// MaxIncidentDiagnoses bounds Diagnostic Agent runs per incident. A second
	// opinion is occasionally worth it; a third is a loop. Exported so the API
	// can tell a person "1 of 2 used" instead of letting them discover the
	// limit by being refused.
	MaxIncidentDiagnoses = 2
	maxIncidentDiagnoses = MaxIncidentDiagnoses
	// MaxIncidentRepairs bounds Repair Agent runs per incident, hard, at one.
	// A repair that did not work is a new incident with new evidence, not a
	// retry of a guess.
	MaxIncidentRepairs = 1
	maxIncidentRepairs = MaxIncidentRepairs
	// maxIncidentActionExecutions bounds executions of an approved action.
	maxIncidentActionExecutions = 2
)

// ---- durable records --------------------------------------------------------

// IncidentRecord is the durable payload of one incident-ledger row. One type
// for every phase keeps the fold trivial and the JSON self-describing; fields
// irrelevant to a phase are simply absent.
type IncidentRecord struct {
	// IncidentID is the stable identity of this incident across its rows. It is
	// derived from the run and the stop signature (see incidentIDFor), so asking
	// "what do I do" twice about the same unchanged stop lands on the SAME
	// incident instead of opening a second one.
	IncidentID string `json:"incidentId"`
	// StopReason is the canonical attention reason this incident is about.
	StopReason string `json:"stopReason"`
	// StopDetail is the human sentence recorded with that stop.
	StopDetail string `json:"stopDetail,omitempty"`
	// Signature is the fingerprint of the stop this incident was opened for.
	// A stop that changes produces a different signature and therefore a
	// different incident, which is what keeps a stale diagnosis from being
	// applied to a situation it never saw.
	Signature string `json:"signature"`

	// Diagnosis fields (incident_diagnosed).
	Class        IncidentClass `json:"class,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	WhatHappened string        `json:"whatHappened,omitempty"`
	WhatIsStuck  string        `json:"whatIsStuck,omitempty"`
	WhyStopped   string        `json:"whyStopped,omitempty"`
	Evidence     []string      `json:"evidence,omitempty"`
	MissingData  []string      `json:"missingEvidence,omitempty"`
	Risk         string        `json:"risk,omitempty"`
	// Options are the concrete choices offered for human_decision_required.
	Options []IncidentOption `json:"options,omitempty"`
	// Action is the single thing the Advisor proposes doing.
	Action *IncidentActionSpec `json:"action,omitempty"`
	// DiagnosisAttempt is 1-based and bounded by maxIncidentDiagnoses.
	DiagnosisAttempt int `json:"diagnosisAttempt,omitempty"`
	// PackDigest ties a diagnosis to the exact evidence it was given, so a
	// diagnosis can never be applied to a pack it did not see.
	PackDigest string `json:"packDigest,omitempty"`
	// AgentSessionID / Harness identify the isolated agent that produced this.
	AgentSessionID string `json:"agentSessionId,omitempty"`
	Harness        string `json:"harness,omitempty"`
	// RoutingReasons records why routing chose (or could not choose) a
	// provider, so a capacity wait is explainable from the ledger alone.
	RoutingReasons []string `json:"routingReasons,omitempty"`

	// Repair linkage and audit — Checkpoint 8P-E.20. Together these answer, from
	// the ledger alone: who approved, which diagnosis authorised it, which
	// repair generation it was, which independent reviewer read it, what the
	// deterministic verification said, and what commit it landed at.
	RepairRunID      string `json:"repairRunId,omitempty"`
	RepairProjectID  string `json:"repairProjectId,omitempty"`
	RepairGeneration int    `json:"repairGeneration,omitempty"`
	ReviewerHarness  string `json:"reviewerHarness,omitempty"`
	VerifyResult     string `json:"verifyResult,omitempty"`
	FinalSHA         string `json:"finalSha,omitempty"`

	// Closure fields — Checkpoint 8P-E.21. ClosureCause names WHY the condition
	// stopped existing and Evidence is the minimum observation that justifies
	// saying so, because "AO stopped asking" must always be answerable.
	ClosureCause string `json:"closureCause,omitempty"`

	// Approval/execution fields.
	ApprovedBy string `json:"approvedBy,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Note       string `json:"note,omitempty"`
}

// IncidentOption is one concrete choice presented for a human decision. It is
// deliberately not executable: it names what the person would do, in AO's own
// vocabulary, so the modal is not a place where new mutation verbs are invented.
type IncidentOption struct {
	// ID is stable within a diagnosis so the UI can report which was chosen.
	ID string `json:"id"`
	// Label is the short imperative shown on the control.
	Label string `json:"label"`
	// Detail explains what happens and what it costs.
	Detail string `json:"detail"`
	// Consequence names what becomes true afterwards, including what is lost.
	Consequence string `json:"consequence,omitempty"`
}

// IncidentActionSpec is the single action a diagnosis proposes. `Kind` is
// checked against the allow-list in incident_policy.go before anything is
// offered to a person, so an agent cannot widen AO's action vocabulary by
// inventing a string.
type IncidentActionSpec struct {
	Kind   IncidentActionKind `json:"kind"`
	Reason string             `json:"reason"`
	// Detail is what the user is told the action will do.
	Detail string `json:"detail,omitempty"`
}

// ---- derived view -----------------------------------------------------------

// Incident is the folded, read-time view of one incident. It is never stored;
// it is rebuilt from the ledger on every read, which is what makes it correct
// after a restart without any recovery code.
type Incident struct {
	ID        string
	RunID     string
	State     IncidentState
	Signature string

	StopReason string
	StopDetail string

	// Diagnosis is the newest accepted diagnosis, if any.
	Diagnosis *IncidentDiagnosis
	// Diagnoses counts Diagnostic Agent runs already spent.
	Diagnoses int
	// Repairs counts Repair Agent runs already spent.
	Repairs int
	// Executions counts executions of an approved action already spent.
	Executions int
	// RepairRunID is the workflow run carrying the approved repair, and
	// ApprovedBy is who authorised it. Both are folded from the ledger so a
	// restart re-derives them rather than losing the link.
	RepairRunID string
	ApprovedBy  string

	// DiagnosticHarness is the provider the newest investigation was routed to.
	DiagnosticHarness string
	// DiagnosticSessionID is the agent session the newest launch actually
	// produced, and DiagnosisStartedAt when it was recorded. Both are what make
	// the investigation observable as a job rather than as a row — see
	// incident_diagnosis_job.go. Empty means no session has been recorded yet,
	// which is a different fact from "no agent is running".
	DiagnosticSessionID string
	DiagnosisStartedAt  time.Time
	// WaitingForCapacity is true when the newest thing that happened to this
	// incident was a capacity wait rather than a launch.
	WaitingForCapacity bool
	CapacityReasons    []string

	// ClosureCause/ClosureEvidence explain a CLOSED incident.
	ClosureCause    string
	ClosureEvidence []string

	// FailedGeneration is the highest diagnosis generation whose launch is
	// durably known to have failed. A generation at or below it is dead, not
	// outstanding.
	FailedGeneration int

	OpenedAt  time.Time
	UpdatedAt time.Time

	// LaunchOutcome is what the most recent RequestIncidentDiagnosis call
	// actually did. It is a per-call value, never folded from the ledger: the
	// difference between "your agent is running now" and "one was already
	// running" is about THIS request, not about the incident's history.
	LaunchOutcome IncidentLaunchOutcome

	// Stale marks an incident whose stop signature no longer matches the run's
	// current stop — the situation moved under it. A stale incident is never
	// acted on; the caller opens a fresh one.
	Stale bool
}

// IncidentDiagnosis is the read-time view of one accepted diagnosis.
type IncidentDiagnosis struct {
	Class        IncidentClass
	Summary      string
	WhatHappened string
	WhatIsStuck  string
	WhyStopped   string
	Evidence     []string
	Missing      []string
	Risk         string
	Options      []IncidentOption
	Action       *IncidentActionSpec
	Attempt      int
	PackDigest   string
	Harness      string
	At           time.Time
}

// CanDiagnose reports whether another Diagnostic Agent run is allowed.
func (i Incident) CanDiagnose() bool {
	return !i.State.Terminal() && !i.Stale && i.Diagnoses < maxIncidentDiagnoses
}

// CanRepair reports whether a Repair Agent run is allowed.
func (i Incident) CanRepair() bool {
	return !i.State.Terminal() && !i.Stale && i.Repairs < maxIncidentRepairs &&
		i.Diagnosis != nil && i.Diagnosis.Class == IncidentRepairAO
}

// CanExecute reports whether the proposed action may still be executed.
func (i Incident) CanExecute() bool {
	return !i.State.Terminal() && !i.Stale &&
		i.Executions < maxIncidentActionExecutions &&
		i.Diagnosis != nil && i.Diagnosis.Action != nil
}

// ---- identity ---------------------------------------------------------------

// incidentSignature fingerprints the stop an incident is about: the reason, the
// detail, and the durable positions of the run's steps.
//
// Including the step states is what makes the signature describe the SITUATION
// rather than just its label. Two runs stopped on fix_cycle_not_started with
// different steps frozen are different incidents and deserve different advice;
// and a run that moved on since a diagnosis was taken no longer matches it,
// which is exactly how a stale diagnosis is prevented from being executed.
func incidentSignature(reason, detail string, steps []domain.WorkflowStep) string {
	parts := make([]string, 0, len(steps)+2)
	parts = append(parts, "reason="+reason, "detail="+strings.TrimSpace(detail))
	ordered := append([]domain.WorkflowStep(nil), steps...)
	sort.Slice(ordered, func(a, b int) bool { return ordered[a].Ordinal < ordered[b].Ordinal })
	for _, s := range ordered {
		parts = append(parts, fmt.Sprintf("%s=%s", s.Kind, s.State))
	}
	return contentDigest(strings.Join(parts, "\n"))
}

// incidentIDFor derives an incident's stable id from the run and the signature,
// so re-asking about an unchanged stop is idempotent by construction rather
// than by a uniqueness check.
func incidentIDFor(runID, signature string) string {
	return "inc-" + contentDigest(runID + "\n" + signature)[:24]
}

// ---- fold -------------------------------------------------------------------

// foldIncident rebuilds an incident from its ledger rows, newest state wins.
// Rows for other incidents on the same run are ignored, which is what lets a
// run accumulate several incidents over its life without them interfering.
func foldIncident(incidentID string, cps []domain.WorkflowCheckpoint) (Incident, bool) {
	rows := make([]domain.WorkflowCheckpoint, 0, 8)
	for _, cp := range cps {
		// Auxiliary rows are collected too: they are not transitions, but they
		// carry facts the fold needs (a launch that failed, a capacity wait).
		// The state advance below is driven by incidentPhases alone, so an
		// auxiliary row can never move the incident.
		if !isIncidentLedgerPhase(cp.DurablePhase) {
			continue
		}
		var rec IncidentRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.IncidentID != incidentID {
			continue
		}
		rows = append(rows, cp)
	}
	if len(rows) == 0 {
		return Incident{}, false
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].CreatedAt.Before(rows[b].CreatedAt) })

	inc := Incident{ID: incidentID, State: IncidentOpen}
	for _, cp := range rows {
		var rec IncidentRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil {
			continue
		}
		next := incidentPhases[cp.DurablePhase]
		inc.RunID = cp.WorkflowRunID
		inc.UpdatedAt = cp.CreatedAt
		if inc.OpenedAt.IsZero() {
			inc.OpenedAt = cp.CreatedAt
		}
		if rec.Signature != "" {
			inc.Signature = rec.Signature
		}
		if rec.StopReason != "" {
			inc.StopReason = rec.StopReason
			inc.StopDetail = rec.StopDetail
		}
		switch cp.DurablePhase {
		case incidentDiagnosingPhase:
			inc.Diagnoses++
			inc.DiagnosticHarness = rec.Harness
			// A new generation supersedes the previous one's session: the row
			// naming this generation's session lands after it.
			inc.DiagnosticSessionID, inc.DiagnosisStartedAt = "", time.Time{}
			// A launch supersedes any earlier capacity wait: AO is no longer
			// waiting for a provider, it has one.
			inc.WaitingForCapacity, inc.CapacityReasons = false, nil
		case incidentDiagnosedPhase, incidentRefusedPhase:
			// A refusal IS a diagnosis, and folding it as one is load-bearing:
			// an unsafe/insufficient verdict's whole value is the evidence it
			// names as missing, and dropping that on the floor would leave the
			// modal showing a bare "refused" with nothing a person could read.
			// A refusal written by the EXECUTION path carries no classification
			// (it is a policy refusal, not an agent's answer), so it must not
			// overwrite a real one — hence the guard.
			if rec.Class == "" && inc.Diagnosis != nil {
				break
			}
			inc.Diagnosis = &IncidentDiagnosis{
				Class: rec.Class, Summary: rec.Summary,
				WhatHappened: rec.WhatHappened, WhatIsStuck: rec.WhatIsStuck, WhyStopped: rec.WhyStopped,
				Evidence: rec.Evidence, Missing: rec.MissingData, Risk: rec.Risk,
				Options: rec.Options, Action: rec.Action, Attempt: rec.DiagnosisAttempt,
				PackDigest: rec.PackDigest, Harness: rec.Harness, At: cp.CreatedAt,
			}
		case incidentDiagnosisStartedPhase:
			if rec.DiagnosisAttempt >= inc.Diagnoses {
				inc.DiagnosticSessionID = rec.AgentSessionID
				inc.DiagnosisStartedAt = cp.CreatedAt
				if rec.Harness != "" {
					inc.DiagnosticHarness = rec.Harness
				}
			}
		case ReasonIncidentDiagnosisCapacityWait:
			inc.WaitingForCapacity = true
			inc.CapacityReasons = rec.RoutingReasons
		case incidentClosedPhase:
			inc.ClosureCause = rec.ClosureCause
			inc.ClosureEvidence = rec.Evidence
		case incidentLaunchFailedPhase:
			if rec.DiagnosisAttempt > inc.FailedGeneration {
				inc.FailedGeneration = rec.DiagnosisAttempt
			}
		case incidentExecutingPhase:
			inc.Executions++
		case incidentRepairPhase:
			// The repair generation is counted HERE, from the row that records a
			// repair actually being dispatched, rather than from the generic
			// executing row. Counting both would spend the budget twice for one
			// repair.
			inc.Repairs++
			inc.RepairRunID = rec.RepairRunID
			inc.ApprovedBy = rec.ApprovedBy
		}
		// The state only advances along declared edges. A row describing a
		// transition the machine does not allow is a row from a future or
		// broken writer, and folding it would invent a state no code produced.
		if next != inc.State && ValidIncidentTransition(inc.State, next) {
			inc.State = next
		}
	}
	return inc, true
}
