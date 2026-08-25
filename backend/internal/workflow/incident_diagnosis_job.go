package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_diagnosis_job.go — the investigation as an observable background job.
//
// The incident: an Incident Advisor investigation showed "Investigando" while
// the agent it had launched was, in fact, sitting at an interactive trust
// prompt and would never answer. Everything AO reported about that job was
// derived from ONE fact — a durable row saying a launch had been requested —
// and that row cannot tell "running" from "waiting for a person" from "dead".
//
// Two things follow, and this file is both of them.
//
// First: the job was already durable and already independent of the UI. It is
// a workflow_checkpoints row plus a real agent session; closing the modal has
// never affected it, and reopening it re-derives everything from the ledger.
// What was missing was not persistence — it was STATUS. So the launch now also
// records the session it started (incidentDiagnosisStartedPhase), which is what
// makes every fact below readable at all.
//
// Second: the status is derived from the SESSION, not from the row. queued,
// starting, running, waiting_for_provider, waiting_for_user, completed and
// failed are seven different things a person does seven different things
// about, and the one the incident was actually in — waiting_for_user — is
// exactly the one a row-only view cannot express.

// incidentDiagnosisStartedPhase records a diagnostic launch that actually
// produced a session. It is an auxiliary row, not a transition: the incident is
// already `diagnosing` from the request row, and this adds the identity of the
// agent doing it. Without it the session id is only ever in a log line, and a
// log line cannot be read back after a restart.
const incidentDiagnosisStartedPhase = "incident_diagnosis_started"

// IncidentDiagnosisState is the job vocabulary. Seven values, closed, each with
// a different thing a person does about it.
type IncidentDiagnosisState string

const (
	// DiagnosisQueued: AO intends to investigate and has not started.
	DiagnosisQueued IncidentDiagnosisState = "queued"
	// DiagnosisStarting: a launch was requested and no session has been
	// recorded yet — the window in which a launch can still fail.
	DiagnosisStarting IncidentDiagnosisState = "starting"
	// DiagnosisRunning: an agent session exists and is doing something.
	DiagnosisRunning IncidentDiagnosisState = "running"
	// DiagnosisWaitingForProvider: AO wants to run and no provider has
	// capacity. Self-remediable; a wake is pending.
	DiagnosisWaitingForProvider IncidentDiagnosisState = "waiting_for_provider"
	// DiagnosisWaitingForUser: the agent is blocked on an interactive prompt —
	// a trust dialog, a permission dialog, a login. This is the state the
	// incident was actually in while AO reported "Investigando", and it is the
	// whole reason this vocabulary exists.
	DiagnosisWaitingForUser IncidentDiagnosisState = "waiting_for_user"
	// DiagnosisCompleted: a diagnosis (including a refusal, which is a
	// first-class answer) has been recorded.
	DiagnosisCompleted IncidentDiagnosisState = "completed"
	// DiagnosisFailed: the launch is durably known to have failed, or the
	// session ended without answering.
	DiagnosisFailed IncidentDiagnosisState = "failed"
)

// IncidentDiagnosisJob is the render-ready view of one investigation. Every
// field is a fact AO holds; a fact AO cannot read is left zero rather than
// guessed, and the UI must render a zero as "unknown", never as "none".
type IncidentDiagnosisJob struct {
	State IncidentDiagnosisState `json:"state"`
	// Attempt is the diagnosis generation, 1-based, and Max the bound.
	Attempt int `json:"attempt,omitempty"`
	Max     int `json:"max,omitempty"`

	// Agent identity: the session AO launched and the provider it went to.
	SessionID string `json:"sessionId,omitempty"`
	Harness   string `json:"harness,omitempty"`

	// StartedAt is when the launch was recorded; ElapsedSeconds is measured
	// from it against the caller's clock, so "how long has this been going" is
	// answered without the UI running a timer of its own.
	StartedAt      time.Time `json:"startedAt,omitempty"`
	ElapsedSeconds int       `json:"elapsedSeconds,omitempty"`

	// LastActivityAt is the agent session's newest activity reading, and
	// LastSignalAt the first/most recent hook signal AO received from it. They
	// are different facts and both matter: activity can come from the pane
	// while the hook pipeline is silent, which is precisely how a stuck startup
	// looks.
	LastActivityAt time.Time `json:"lastActivityAt,omitempty"`
	LastSignalAt   time.Time `json:"lastSignalAt,omitempty"`

	// BlockingInteraction names what the agent is waiting for, when AO can
	// tell. Empty means AO does not know — never "nothing".
	BlockingInteraction string `json:"blockingInteraction,omitempty"`

	// CapacityReasons explains waiting_for_provider in routing's own words, and
	// NextEvaluationAt when AO will look again.
	CapacityReasons  []string   `json:"capacityReasons,omitempty"`
	NextEvaluationAt *time.Time `json:"nextEvaluationAt,omitempty"`
}

// DeriveIncidentDiagnosisJob projects an incident's investigation onto the job
// vocabulary, reading the agent session for everything the ledger cannot say.
//
// Order matters and is the order the outcomes are decided in: an answer that
// exists ends the job whatever the session now reads, and a launch that is
// durably known to have failed is failed however recently it was requested.
func (c *Coordinator) DeriveIncidentDiagnosisJob(ctx stdctx.Context, inc Incident) IncidentDiagnosisJob {
	job := IncidentDiagnosisJob{
		State:   DiagnosisQueued,
		Attempt: inc.Diagnoses,
		Max:     MaxIncidentDiagnoses,
		Harness: inc.DiagnosticHarness,
	}
	if inc.Diagnosis != nil && inc.Diagnosis.Attempt >= inc.Diagnoses && inc.Diagnoses > 0 {
		job.State = DiagnosisCompleted
		job.StartedAt = inc.Diagnosis.At
		return job
	}
	if inc.Diagnoses > 0 && inc.FailedGeneration >= inc.Diagnoses {
		job.State = DiagnosisFailed
		return job
	}
	if inc.WaitingForCapacity {
		job.State = DiagnosisWaitingForProvider
		job.CapacityReasons = inc.CapacityReasons
		job.NextEvaluationAt = c.nextIncidentEvaluation(ctx, inc.RunID)
		return job
	}
	if inc.Diagnoses == 0 {
		return job
	}

	job.SessionID = inc.DiagnosticSessionID
	job.StartedAt = inc.DiagnosisStartedAt
	if job.StartedAt.IsZero() {
		job.StartedAt = inc.UpdatedAt
	}
	if !job.StartedAt.IsZero() {
		if elapsed := c.clock().Sub(job.StartedAt); elapsed > 0 {
			job.ElapsedSeconds = int(elapsed / time.Second)
		}
	}
	// No session recorded yet: the launch is in the window between the durable
	// request and the session it produces. That is `starting`, not `running` —
	// claiming an agent is working when AO has not seen one is the same
	// overstatement this file exists to remove, one step earlier.
	if job.SessionID == "" || c.sessionFacts == nil {
		job.State = DiagnosisStarting
		return job
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(job.SessionID))
	if err != nil || !found {
		job.State = DiagnosisStarting
		return job
	}
	job.Harness = string(sess.Harness)
	job.LastActivityAt = sess.Activity.LastActivityAt
	job.LastSignalAt = sess.FirstSignalAt
	if !sess.TurnCompletedAt.IsZero() && sess.TurnCompletedAt.After(job.LastSignalAt) {
		job.LastSignalAt = sess.TurnCompletedAt
	}

	switch {
	case sess.IsTerminated || sess.Activity.State == domain.ActivityExited:
		// The agent is gone and no diagnosis was recorded. That is a failed
		// investigation, and saying so is what lets a person spend the next
		// generation instead of watching a dead job forever.
		job.State = DiagnosisFailed
		job.BlockingInteraction = "the diagnostic agent's session ended without submitting an answer"
	case sess.Activity.State == domain.ActivityBlocked:
		// `blocked` is proof: it is only ever entered from a permission dialog
		// AO correlated to a tool, and it clears the moment that tool resolves.
		job.State = DiagnosisWaitingForUser
		job.BlockingInteraction = "the diagnostic agent has a permission dialog open"
	case sess.Activity.State == domain.ActivityWaitingInput:
		job.State = DiagnosisWaitingForUser
		job.BlockingInteraction = "the diagnostic agent reported it is waiting for input"
	case sess.FirstSignalAt.IsZero() && !job.StartedAt.IsZero() &&
		c.clock().Sub(job.StartedAt) > incidentDiagnosisStartupGrace:
		// Alive, but the hook pipeline has never said anything and the startup
		// grace is gone. This is the wf-f0efac7e shape exactly: a session that
		// exists, a UI that says "Investigando", and an agent parked on a trust
		// prompt that fires no hook. AO cannot prove which prompt, so it does
		// not name one — it names the fact.
		job.State = DiagnosisWaitingForUser
		job.BlockingInteraction = "the diagnostic agent has produced no signal since it started, which is what an unanswered interactive startup prompt (trust, permissions, login) looks like"
	default:
		job.State = DiagnosisRunning
	}
	return job
}

// incidentDiagnosisStartupGrace is how long a launched diagnostic agent may
// produce no signal at all before AO stops calling it "running". It mirrors the
// work step's own startup grace deliberately: the two answer the same question
// about the same providers, and they must not disagree about how long a normal
// cold start takes.
const incidentDiagnosisStartupGrace = workStepFirstSignalTimeout

// recordIncidentDiagnosisStarted writes the auxiliary row that names the agent
// session a launch actually produced.
//
// Best-effort: the investigation is running whether or not AO managed to write
// down which session is doing it, and failing to record that must not undo a
// launch. What it costs when it fails is observability, which is why it is
// logged rather than swallowed.
func (c *Coordinator) recordIncidentDiagnosisStarted(
	ctx stdctx.Context, run domain.WorkflowRun, inc Incident, generation int, res IncidentAgentResult, packDigest string,
) {
	rec := IncidentRecord{
		IncidentID: inc.ID, StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		Signature: inc.Signature, DiagnosisAttempt: generation,
		PackDigest: packDigest, Harness: res.Harness, AgentSessionID: res.SessionID,
	}
	if err := c.writeIncidentRow(ctx, run, incidentDiagnosisStartedPhase,
		"incident_diagnosis_started: session "+res.SessionID+" on "+res.Harness, rec); err != nil && c.log != nil {
		c.log.Warn("workflow: recording the diagnostic agent's session failed",
			"run", run.ID, "incident", inc.ID, "session", res.SessionID, "err", err)
	}
}
