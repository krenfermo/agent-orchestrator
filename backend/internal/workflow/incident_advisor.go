package workflow

import (
	stdctx "context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// incident_advisor.go — Checkpoint 8P-E.18's orchestration.
//
// Five entry points, in the order a person meets them:
//
//	OpenIncident            "what is going on"      (read + idempotent record)
//	IncidentPackFor         the bounded evidence     (read)
//	RequestIncidentDiagnosis  "investigate it"       (launches an isolated agent)
//	SubmitIncidentDiagnosis   the agent's answer     (validated, never trusted)
//	ExecuteIncidentAction     "do it"                (authorized, then executed)
//
// The separation between the last two is the whole security model in one
// sentence: the process that produces a proposal cannot be the process that
// carries it out. SubmitIncidentDiagnosis has no ability to execute anything,
// and ExecuteIncidentAction takes its instructions from the durable record
// rather than from whoever is calling it.

// ErrIncidentUnavailable is returned when a run has no incident to talk about —
// it is not stopped, or its stop is one AO handles by itself.
var ErrIncidentUnavailable = errors.New("workflow: run has no open incident")

// ErrIncidentStale is returned when the situation moved under an incident. The
// caller opens a fresh one; nothing is executed against evidence that no longer
// describes the run.
var ErrIncidentStale = errors.New("workflow: incident no longer matches the run's current stop")

// IncidentDiagnosisSubmission is what a Diagnostic Agent returns. It is parsed
// from JSON the agent wrote, so every field is untrusted input and is validated
// before any of it becomes durable.
type IncidentDiagnosisSubmission struct {
	IncidentID   string              `json:"incidentId"`
	PackDigest   string              `json:"packDigest"`
	Class        IncidentClass       `json:"classification"`
	Summary      string              `json:"summary"`
	WhatHappened string              `json:"whatHappened"`
	WhatIsStuck  string              `json:"whatIsStuck"`
	WhyStopped   string              `json:"whyAOStopped"`
	Evidence     []string            `json:"evidence"`
	Missing      []string            `json:"missingEvidence"`
	Risk         string              `json:"risk"`
	Options      []IncidentOption    `json:"options"`
	Action       *IncidentActionSpec `json:"proposedAction"`
}

// IncidentAgentLauncher is workflow's narrow port for starting an isolated
// agent about an incident. It is deliberately separate from ReviewerLauncher:
// a reviewer reads a worktree to judge code, while these agents read a pack to
// judge AO, and conflating them would let a diagnostic prompt inherit the
// reviewer's "post a PR review" contract.
type IncidentAgentLauncher interface {
	// LaunchDiagnostic starts a read-only agent over the pack. The
	// implementation must give it no write tooling; the prompt says so too, but
	// the prompt is not the enforcement.
	LaunchDiagnostic(ctx stdctx.Context, req IncidentAgentRequest) (IncidentAgentResult, error)
	// LaunchRepair starts a writing agent in an ISOLATED workspace — never the
	// user's checkout, never the run's own worktree.
	LaunchRepair(ctx stdctx.Context, req IncidentAgentRequest) (IncidentAgentResult, error)
}

// IncidentAgentRequest is one isolated agent launch.
type IncidentAgentRequest struct {
	IncidentID string
	RunID      string
	ProjectID  string
	// Prompt is the fully rendered instruction, pack included. The launcher
	// never adds evidence of its own.
	Prompt string
	// PackDigest is echoed into the agent's submission contract so a diagnosis
	// can be matched to the evidence it saw.
	PackDigest string
	// Harness is the provider AO selected for this role.
	Harness string
	// ReadOnly is true for diagnostics. A launcher that cannot honour it must
	// fail rather than launch a writable agent.
	ReadOnly bool
}

// IncidentAgentResult is the handle of a launched agent.
type IncidentAgentResult struct {
	SessionID string
	Harness   string
}

// ---- 1. open ----------------------------------------------------------------

// OpenIncident derives the run's current stop and returns the incident for it,
// recording one if this stop has not been seen before.
//
// It is idempotent by identity rather than by a check: the incident id is a
// function of the run and the stop signature, so asking twice about an
// unchanged stop returns the same incident, and asking after the situation
// moved returns a different one.
func (c *Coordinator) OpenIncident(ctx stdctx.Context, runID string) (Incident, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	if !ok {
		return Incident{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		return Incident{}, fmt.Errorf("%w: run is %s", ErrIncidentUnavailable, run.State)
	}
	reason, disp, ok := c.stopReason(ctx, run)
	if !ok || reason == "" {
		reason = unclassifiedStop
	}
	if disp.SelfRemediable {
		// AO is still working on this by itself. Offering a person an
		// investigation would invite them to intervene in something that is
		// already moving, which is the misreport the lifecycle work removed.
		return Incident{}, fmt.Errorf("%w: %s is self-remediable", ErrIncidentUnavailable, reason)
	}

	steps, err := c.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	detail := c.stopDetailFor(ctx, run, reason)
	signature := incidentSignature(reason, detail, steps)
	incidentID := incidentIDFor(runID, signature)

	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	if inc, found := foldIncident(incidentID, cps); found {
		inc.RunID = runID
		return inc, nil
	}

	rec := IncidentRecord{
		IncidentID: incidentID, StopReason: reason,
		StopDetail: detail, Signature: signature,
	}
	if err := c.writeIncidentRow(ctx, run, incidentOpenedPhase,
		fmt.Sprintf("incident_opened: %s", reason), rec); err != nil {
		return Incident{}, err
	}
	return Incident{
		ID: incidentID, RunID: runID, State: IncidentOpen, Signature: signature,
		StopReason: reason, StopDetail: detail,
		OpenedAt: c.clock(), UpdatedAt: c.clock(),
	}, nil
}

// stopDetailFor recovers the human sentence recorded with the current stop.
func (c *Coordinator) stopDetailFor(ctx stdctx.Context, run domain.WorkflowRun, reason string) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, run.ID)
	if err != nil {
		return ""
	}
	detail := ""
	var newest = struct{ at int64 }{}
	_ = newest
	for _, cp := range cps {
		if cp.DurablePhase == reason && strings.TrimSpace(cp.NextAction) != "" {
			detail = strings.TrimSpace(cp.NextAction)
		}
	}
	return detail
}

// ---- 2. the pack ------------------------------------------------------------

// IncidentPackFor builds the bounded evidence pack for an incident.
//
// Everything it gathers is something AO already observed for its own purposes.
// It opens no files, walks no directories and runs no git command of its own:
// the workspace facts come from the same ObserveWorkspace port the observation
// path uses, which is what keeps "do not read the repository indiscriminately"
// a structural property rather than an instruction in a prompt.
func (c *Coordinator) IncidentPackFor(ctx stdctx.Context, runID string) (Incident, IncidentContextPack, error) {
	inc, err := c.OpenIncident(ctx, runID)
	if err != nil {
		return Incident{}, IncidentContextPack{}, err
	}
	detail, err := c.GetRun(ctx, runID)
	if err != nil {
		return inc, IncidentContextPack{}, err
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return inc, IncidentContextPack{}, err
	}

	in := IncidentPackInput{
		Detail: detail, Signature: inc.Signature, IncidentID: inc.ID,
		StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		Checkpoints: cps,
	}
	if detail.Run.ParentWorkflowID != nil && *detail.Run.ParentWorkflowID != "" {
		in.MasterID = *detail.Run.ParentWorkflowID
		if master, ok, merr := c.store.GetWorkflowRun(ctx, in.MasterID); merr == nil && ok {
			in.MasterState = string(master.State)
		}
	}
	c.attachIncidentSessionFacts(ctx, detail, &in)
	c.attachIncidentReviewFacts(ctx, detail, &in)

	pack := BuildIncidentContextPack(in)
	pack.AttachBuiltAt(c.clock())
	return inc, pack, nil
}

// attachIncidentSessionFacts fills in the worker session and its workspace from
// the facts ports, never from the filesystem directly.
func (c *Coordinator) attachIncidentSessionFacts(ctx stdctx.Context, detail RunDetail, in *IncidentPackInput) {
	if c.sessionFacts == nil {
		return
	}
	var sessionID string
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepWork && sd.Step.SessionID != nil {
			sessionID = *sd.Step.SessionID
		}
	}
	if sessionID == "" {
		return
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		return
	}
	in.SessionID = sessionID
	in.SessionHarness = string(sess.Harness)
	in.SessionActivity = string(sess.Activity.State)
	in.SessionLastAt = sess.Activity.LastActivityAt
	in.SessionTerminated = sess.IsTerminated
	in.Branch = sess.Metadata.Branch
	in.WorktreePath = sess.Metadata.WorkspacePath

	if c.workspaceFacts == nil {
		return
	}
	if obs, ok := c.observeFixWorkspace(ctx, sess); ok {
		in.HeadSHA = obs.HeadSHA
		lines := make([]string, 0, len(obs.Changes))
		for _, ch := range obs.Changes {
			lines = append(lines, ch.Status+" "+ch.Path)
		}
		in.GitStatus = strings.Join(lines, "\n")
	}
}

// attachIncidentReviewFacts adds the newest reviewer verdict, when the stop is
// one a reviewer's findings could explain.
func (c *Coordinator) attachIncidentReviewFacts(ctx stdctx.Context, detail RunDetail, in *IncidentPackInput) {
	if c.reviewRuns == nil {
		return
	}
	for _, sd := range detail.Steps {
		if sd.Step.Kind != domain.WorkflowStepReview || sd.Step.ReviewRunID == nil {
			continue
		}
		rr, ok, err := c.reviewRuns.GetReviewRun(ctx, *sd.Step.ReviewRunID)
		if err != nil || !ok {
			return
		}
		in.ReviewVerdict = string(rr.Verdict)
		in.ReviewFindings = rr.Body
	}
}

// ---- 3. diagnose ------------------------------------------------------------

// RequestIncidentDiagnosis launches an isolated Diagnostic Agent.
//
// The durable row is written BEFORE the launch, for the same reason
// recordFixDispatchIntent is: a launch AO could not first write down is a
// launch AO does not make, and the row is what bounds the attempt count across
// a restart.
func (c *Coordinator) RequestIncidentDiagnosis(ctx stdctx.Context, runID string) (Incident, IncidentContextPack, error) {
	inc, pack, err := c.IncidentPackFor(ctx, runID)
	if err != nil {
		return inc, pack, err
	}
	if c.incidentAgents == nil {
		return inc, pack, errors.New("workflow: no incident agent launcher is configured")
	}
	if inc.Stale {
		return inc, pack, ErrIncidentStale
	}
	if !inc.CanDiagnose() {
		return inc, pack, fmt.Errorf("workflow: incident %s has used its %d diagnosis attempts", inc.ID, MaxIncidentDiagnoses)
	}
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		return inc, pack, err
	}

	// An outstanding generation is one whose agent may still answer. Launching
	// another would double-spend a provider on a question already being asked.
	generation, outstanding := c.diagnosisGeneration(inc, c.clock())
	if outstanding {
		inc.LaunchOutcome = IncidentAlreadyRunning
		return inc, pack, nil
	}

	// Provider selection through AO's ordinary routing — health, capacity,
	// profile eligibility and the operator's own priority list all apply, and a
	// shortage becomes the ordinary self-remediable wait.
	decision, err := c.selectIncidentDiagnosticProvider(ctx, run, c.incumbentHarnessFor(ctx, runID))
	if err != nil {
		return inc, pack, err
	}
	if decision.Waiting || decision.SelectedHarness == "" {
		c.recordIncidentCapacityWait(ctx, run, inc, decision)
		inc.LaunchOutcome = IncidentWaitingForCapacity
		return inc, pack, nil
	}

	// Claim the single-flight slot BEFORE anything is launched or recorded, so
	// a concurrent poll, a double click and a restarted daemon converge on one
	// launch instead of three.
	entry, claimed, err := c.claimIncidentLaunch(ctx, run, inc, generation)
	if err != nil {
		return inc, pack, err
	}
	if !claimed {
		inc.LaunchOutcome = IncidentAlreadyRunning
		return inc, pack, nil
	}

	// The durable request row is written before the launch, for the same reason
	// recordFixDispatchIntent is: a launch AO could not first write down is a
	// launch AO does not make, and this row is what bounds the attempt count
	// across a restart.
	rec := IncidentRecord{
		IncidentID: inc.ID, StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		Signature: inc.Signature, DiagnosisAttempt: generation,
		PackDigest: pack.Digest, Harness: string(decision.SelectedHarness),
	}
	if err := c.writeIncidentRow(ctx, run, incidentDiagnosingPhase,
		fmt.Sprintf("incident_diagnosis_requested: attempt %d of %d to %s over a %d-byte pack (~%d tokens)",
			generation, MaxIncidentDiagnoses, decision.SelectedHarness, pack.Bytes, pack.EstimatedTokens), rec); err != nil {
		c.releaseIncidentLaunch(ctx, run, inc, generation, entry, err)
		return inc, pack, err
	}

	res, err := c.incidentAgents.LaunchDiagnostic(ctx, IncidentAgentRequest{
		IncidentID: inc.ID, RunID: runID, ProjectID: run.ProjectID,
		Prompt: BuildIncidentDiagnosticPrompt(pack), PackDigest: pack.Digest,
		Harness: string(decision.SelectedHarness), ReadOnly: true,
	})
	if err != nil {
		// Nothing is running. Release the claim so the next generation can be
		// claimed rather than leaving the incident permanently unlaunchable.
		c.releaseIncidentLaunch(ctx, run, inc, generation, entry, err)
		return inc, pack, err
	}
	if c.log != nil {
		c.log.Info("workflow: diagnostic agent launched for an incident",
			"run", runID, "incident", inc.ID, "generation", generation,
			"harness", decision.SelectedHarness, "packBytes", pack.Bytes, "session", res.SessionID)
	}
	inc.State, inc.Diagnoses = IncidentDiagnosing, generation
	inc.LaunchOutcome = IncidentLaunched
	return inc, pack, nil
}

// ---- 4. accept the diagnosis ------------------------------------------------

// SubmitIncidentDiagnosis validates an agent's answer and records it.
//
// It cannot execute anything. That is not an oversight to be tidied up later —
// it is the separation of duties, and it is enforced by this function simply
// not having the ability.
func (c *Coordinator) SubmitIncidentDiagnosis(ctx stdctx.Context, runID string, sub IncidentDiagnosisSubmission) (Incident, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	if !ok {
		return Incident{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	inc, found := foldIncident(sub.IncidentID, cps)
	if !found {
		return Incident{}, fmt.Errorf("%w: incident %q", ErrNotFound, sub.IncidentID)
	}
	inc.RunID = runID
	if inc.State.Terminal() {
		return inc, fmt.Errorf("workflow: incident %s is already %s", inc.ID, inc.State)
	}

	// The submission must be about evidence AO actually handed over. Without
	// this, a diagnosis taken against one situation could be recorded against a
	// later, different one.
	expected := c.incidentPackDigestFor(cps, inc.ID)
	if expected != "" && sub.PackDigest != expected {
		return inc, fmt.Errorf("workflow: diagnosis is for pack %q but this incident's pack is %q", sub.PackDigest, expected)
	}
	if !sub.Class.Valid() {
		return inc, fmt.Errorf("workflow: %q is not one of AO's four incident classifications", sub.Class)
	}
	if strings.TrimSpace(sub.Summary) == "" {
		return inc, errors.New("workflow: a diagnosis must include a summary")
	}
	if sub.Class == IncidentUnsafeOrInsufficient && len(sub.Missing) == 0 {
		return inc, errors.New("workflow: an unsafe/insufficient diagnosis must name the evidence it lacks")
	}
	if sub.Class == IncidentHumanDecision && len(sub.Options) == 0 {
		return inc, errors.New("workflow: a human-decision diagnosis must offer at least one concrete option")
	}

	// The proposed action is resolved against the allow-list HERE, on the way
	// in, so an unrecognised verb never becomes durable in the first place.
	action := &IncidentActionSpec{Kind: IncidentActionNone}
	if sub.Action != nil {
		kind, _ := lookupIncidentAction(sub.Action.Kind)
		action = &IncidentActionSpec{Kind: kind, Reason: sub.Action.Reason, Detail: sub.Action.Detail}
	}

	rec := IncidentRecord{
		IncidentID: inc.ID, StopReason: inc.StopReason, StopDetail: inc.StopDetail,
		Signature: inc.Signature, Class: sub.Class, Summary: strings.TrimSpace(sub.Summary),
		WhatHappened: sub.WhatHappened, WhatIsStuck: sub.WhatIsStuck, WhyStopped: sub.WhyStopped,
		Evidence: sub.Evidence, MissingData: sub.Missing, Risk: sub.Risk,
		Options: sub.Options, Action: action,
		DiagnosisAttempt: inc.Diagnoses, PackDigest: sub.PackDigest,
	}
	phase := incidentDiagnosedPhase
	next := fmt.Sprintf("incident_diagnosed: %s — %s", sub.Class, oneLine(rec.Summary))
	if sub.Class == IncidentUnsafeOrInsufficient {
		phase = incidentRefusedPhase
		next = "incident_refused: " + oneLine(rec.Summary) + " (missing: " + strings.Join(sub.Missing, "; ") + ")"
	}
	if err := c.writeIncidentRow(ctx, run, phase, next, rec); err != nil {
		return inc, err
	}
	refreshed, _ := c.LoadIncident(ctx, runID, inc.ID)
	return refreshed, nil
}

// incidentPackDigestFor returns the digest of the pack this incident's newest
// diagnosis request was built from.
func (c *Coordinator) incidentPackDigestFor(cps []domain.WorkflowCheckpoint, incidentID string) string {
	digest := ""
	for _, cp := range cps {
		if cp.DurablePhase != incidentDiagnosingPhase {
			continue
		}
		rec, ok := decodeIncidentRecord(cp)
		if !ok || rec.IncidentID != incidentID {
			continue
		}
		digest = rec.PackDigest
	}
	return digest
}

// ---- 5. execute -------------------------------------------------------------

// ExecuteIncidentAction authorizes and then carries out the proposed action.
//
// approvedBy is the identity of the person who said yes; it is empty for an
// action AO is allowed to take by itself. authorizeIncidentAction decides which
// of those applies — this function never assumes.
func (c *Coordinator) ExecuteIncidentAction(ctx stdctx.Context, runID, incidentID, approvedBy string) (Incident, error) {
	run, ok, err := c.store.GetWorkflowRun(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	if !ok {
		return Incident{}, fmt.Errorf("%w: workflow run %q", ErrNotFound, runID)
	}
	inc, err := c.LoadIncident(ctx, runID, incidentID)
	if err != nil {
		return inc, err
	}
	if inc.State.Terminal() {
		return inc, fmt.Errorf("workflow: incident %s is already %s", inc.ID, inc.State)
	}
	if inc.Diagnosis == nil || inc.Diagnosis.Action == nil {
		return inc, errors.New("workflow: this incident has no diagnosed action to execute")
	}
	// The situation must still be the one that was diagnosed. This is the
	// check that makes a stale diagnosis harmless rather than dangerous.
	if err := c.assertIncidentFresh(ctx, run, inc); err != nil {
		return inc, err
	}
	if !inc.CanExecute() {
		return inc, fmt.Errorf("workflow: incident %s has used its %d execution attempts", inc.ID, maxIncidentActionExecutions)
	}

	class, action := inc.Diagnosis.Class, inc.Diagnosis.Action.Kind

	// A missing approval is a precondition, not a verdict. Recording it as a
	// refusal would burn the incident the first time someone pressed the button
	// before ticking the box, and the remedy for "you have not approved this
	// yet" is to approve it — not to open a new incident.
	if incidentActionNeedsApproval(class, action) && strings.TrimSpace(approvedBy) == "" {
		return inc, fmt.Errorf("workflow: %s requires explicit human approval before it can run", action)
	}

	allowed, why := authorizeIncidentAction(class, action, approvedBy)
	if !allowed {
		rec := IncidentRecord{
			IncidentID: inc.ID, Signature: inc.Signature, StopReason: inc.StopReason,
			Class: class, Action: inc.Diagnosis.Action, Note: why,
		}
		_ = c.writeIncidentRow(ctx, run, incidentRefusedPhase, "incident_refused: "+why, rec)
		return inc, fmt.Errorf("workflow: %s", why)
	}

	rec := IncidentRecord{
		IncidentID: inc.ID, Signature: inc.Signature, StopReason: inc.StopReason,
		Class: class, Action: inc.Diagnosis.Action, ApprovedBy: approvedBy,
	}
	if approvedBy != "" {
		if err := c.writeIncidentRow(ctx, run, incidentApprovedPhase,
			fmt.Sprintf("incident_action_approved: %s approved %s", approvedBy, action), rec); err != nil {
			return inc, err
		}
	}
	if err := c.writeIncidentRow(ctx, run, incidentExecutingPhase,
		fmt.Sprintf("incident_action_executing: %s", action), rec); err != nil {
		return inc, err
	}

	outcome, execErr := c.runIncidentAction(ctx, run, inc, action)
	rec.Outcome = outcome
	if execErr != nil {
		rec.Note = execErr.Error()
		_ = c.writeIncidentRow(ctx, run, incidentRefusedPhase, "incident_refused: action failed: "+execErr.Error(), rec)
		return inc, execErr
	}
	if err := c.writeIncidentRow(ctx, run, incidentResolvedPhase, "incident_resolved: "+outcome, rec); err != nil {
		return inc, err
	}
	return c.LoadIncident(ctx, runID, incidentID)
}

// runIncidentAction is the only place an incident action has an effect, and
// every branch delegates to a mechanism that already exists and is already
// bounded. There is no branch that shells out, edits a file, or writes to the
// database directly.
func (c *Coordinator) runIncidentAction(ctx stdctx.Context, run domain.WorkflowRun, inc Incident, kind IncidentActionKind) (string, error) {
	switch kind {
	case IncidentActionContinueRun:
		if _, err := c.ContinueRun(ctx, run.ID); err != nil {
			return "", err
		}
		return "continued the run through its ordinary continue path", nil
	case IncidentActionCancelRun:
		if _, err := c.CancelRun(ctx, run.ID); err != nil {
			return "", err
		}
		return "cancelled the run", nil
	case IncidentActionRepairAgent:
		if c.incidentAgents == nil {
			return "", errors.New("no incident agent launcher is configured")
		}
		res, err := c.incidentAgents.LaunchRepair(ctx, IncidentAgentRequest{
			IncidentID: inc.ID, RunID: run.ID, ProjectID: run.ProjectID,
			Prompt: BuildIncidentRepairPrompt(inc), ReadOnly: false,
		})
		if err != nil {
			return "", err
		}
		return "launched a repair agent in session " + res.SessionID, nil
	default:
		return "", fmt.Errorf("workflow: %q is not an executable incident action", kind)
	}
}

// assertIncidentFresh re-derives the run's current stop and refuses if it no
// longer matches what was diagnosed.
func (c *Coordinator) assertIncidentFresh(ctx stdctx.Context, run domain.WorkflowRun, inc Incident) error {
	if run.State != domain.WorkflowRunNeedsAttention {
		return fmt.Errorf("%w: the run is now %s", ErrIncidentStale, run.State)
	}
	reason, _, ok := c.stopReason(ctx, run)
	if !ok || reason == "" {
		reason = unclassifiedStop
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return err
	}
	if got := incidentSignature(reason, c.stopDetailFor(ctx, run, reason), steps); got != inc.Signature {
		return fmt.Errorf("%w: it was diagnosed against a different state", ErrIncidentStale)
	}
	return nil
}

// ---- shared -----------------------------------------------------------------

// LoadIncident folds one incident and marks it stale when the run's current
// stop no longer matches it.
func (c *Coordinator) LoadIncident(ctx stdctx.Context, runID, incidentID string) (Incident, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return Incident{}, err
	}
	inc, found := foldIncident(incidentID, cps)
	if !found {
		return Incident{}, fmt.Errorf("%w: incident %q", ErrNotFound, incidentID)
	}
	inc.RunID = runID
	if run, ok, rerr := c.store.GetWorkflowRun(ctx, runID); rerr == nil && ok && !inc.State.Terminal() {
		if ferr := c.assertIncidentFresh(ctx, run, inc); ferr != nil {
			inc.Stale = true
		}
	}
	return inc, nil
}

// writeIncidentRow appends one ledger row. Every incident transition goes
// through it, which is why the fold can be a pure function over checkpoints.
func (c *Coordinator) writeIncidentRow(ctx stdctx.Context, run domain.WorkflowRun, phase, nextAction string, rec IncidentRecord) error {
	payload, err := marshalIncidentRecord(rec)
	if err != nil {
		return err
	}
	_, err = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		ProjectID:      run.ProjectID,
		NextAction:     nextAction,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		RetryState:     payload,
		CreatedAt:      c.clock(),
	})
	return err
}
