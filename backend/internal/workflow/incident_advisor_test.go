package workflow_test

// Checkpoint 8P-E.18 — the Incident Advisor, exercised against the stops this
// repository has actually been stranded on.
//
// The seven fixtures below are not invented shapes. Each is a stop AO really
// reached, and several of them cost a person an evening with a SQL client:
//
//	child_needs_attention            a parent mirroring a child that had recovered
//	waiting_for_capacity (stale)     a provider marked unavailable that was fine
//	verify_infrastructure_failed     go run from the wrong module root
//	verify_workspace_changed         the tree moved under an approval
//	dirty_worktree                   the user's own uncommitted changes
//	fix_cycle_not_started            wf-57f90ff2: judged by the previous cycle
//	fix_prompt_not_submitted         the same run: pasted into tmux copy-mode
//
// What is pinned here is not "the advice is good" — a language model produces
// that and it is not testable. It is everything AROUND the advice: that the
// evidence is bounded, that a classification AO does not recognise is refused,
// that nothing executes without the authorization the class demands, that a
// diagnosis cannot outlive the situation it was taken against, and that the
// diagnostician can never be the executor.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// ---- the seven real stops --------------------------------------------------

type advisorStopCase struct {
	name   string
	reason string
	detail string
}

var realIncidentStops = []advisorStopCase{
	{"child_needs_attention", workflowcore.ReasonChildNeedsAttention,
		"the running task stopped and needs a decision"},
	{"capacity_retry_exhausted", workflowcore.ReasonCapacityRetryExhausted,
		"every provider attempt reported no capacity"},
	{"verify_infrastructure_failed", workflowcore.ReasonVerifyInfraFailed,
		"verification infrastructure failure (wrong_module_root) running \"go build ./...\" in \".\""},
	{"verify_workspace_unattributable", workflowcore.ReasonVerifyWorkspaceUnattributable,
		"the worktree no longer matches what review approved"},
	{"dirty_worktree", "dirty_worktree",
		"the target repository has uncommitted changes"},
	{"fix_cycle_not_started", workflowcore.ReasonFixCycleNotStarted,
		"fix cycle 2 was delivered to worker session agent-orchestrator-29 11m15s ago and that session has produced no activity"},
	{"fix_prompt_not_submitted", workflowcore.ReasonFixPromptNotSubmitted,
		"fix cycle 2 reached worker session agent-orchestrator-29 but is still sitting unsubmitted in its composer"},
}

// Every real stop must produce an incident that can be opened, packed and
// explained — including the ones AO has no automatic answer for. A stop a
// person cannot ask about is the dead end this feature exists to remove.
func TestEveryRealStopProducesAnExplainableIncident(t *testing.T) {
	for _, tc := range realIncidentStops {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdvisorFixture(t, tc.reason, tc.detail)

			inc, pack, err := f.c.IncidentPackFor(context.Background(), f.runID)
			if err != nil {
				t.Fatalf("IncidentPackFor: %v", err)
			}
			if inc.StopReason != tc.reason {
				t.Fatalf("incident stop reason = %q, want %q", inc.StopReason, tc.reason)
			}
			if inc.State != workflowcore.IncidentOpen {
				t.Fatalf("incident state = %q, want open", inc.State)
			}
			if pack.Digest == "" || pack.Bytes == 0 {
				t.Fatal("pack is empty: there is nothing for anyone to diagnose")
			}
			if pack.Bytes > pack.MaxBytes {
				t.Fatalf("pack is %d bytes, over its own %d-byte budget", pack.Bytes, pack.MaxBytes)
			}
			rendered := pack.Render()
			for _, want := range []string{tc.reason, "Steps", "Recent checkpoints"} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("rendered pack is missing %q:\n%s", want, rendered)
				}
			}
			if !strings.Contains(rendered, "estimated") {
				t.Fatal("rendered pack does not disclose its own budget")
			}
		})
	}
}

// Asking twice about an unchanged stop is the same incident, not a second one.
func TestOpeningTheSameStopTwiceIsIdempotent(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	first, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}
	f.clk.Advance(time.Minute)
	second, err := f.c.OpenIncident(ctx, f.runID)
	if err != nil {
		t.Fatalf("OpenIncident (again): %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("incident ids %q != %q: re-asking opened a second incident", first.ID, second.ID)
	}
	if n := f.countCheckpointPhase("incident_opened"); n != 1 {
		t.Fatalf("incident_opened rows = %d, want exactly 1", n)
	}
}

// A run AO is still working on by itself is never offered for investigation.
func TestSelfRemediableStopsAreNotIncidents(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyFixReentry, "AO is handing the findings back to the fix worker")
	_, err := f.c.OpenIncident(context.Background(), f.runID)
	if !errors.Is(err, workflowcore.ErrIncidentUnavailable) {
		t.Fatalf("err = %v, want ErrIncidentUnavailable: AO is still working on this", err)
	}
}

// ---- the pack's budget ------------------------------------------------------

// The pack must stay inside its budget by dropping WHOLE low-priority sections,
// and must say which ones it dropped. A silently half-truncated diff is what
// invites a confident wrong diagnosis.
func TestPackDropsWholeSectionsAndDisclosesIt(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	f.workspaceFacts.obs.Changes = hugeChangeSet(4000)

	_, pack, err := f.c.IncidentPackFor(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("IncidentPackFor: %v", err)
	}
	if pack.Bytes > pack.MaxBytes {
		t.Fatalf("pack is %d bytes, over its %d-byte budget", pack.Bytes, pack.MaxBytes)
	}
	if pack.EstimatedTokens == 0 {
		t.Fatal("pack reports no token estimate")
	}
	// Whatever was dropped, the stop and the steps survive: a pack without
	// those is not a cheaper pack, it is a useless one.
	for _, must := range []string{"Stop", "Steps"} {
		found := false
		for _, s := range pack.Sections {
			if s.Title == must && !s.Dropped && s.Body != "" {
				found = true
			}
		}
		if !found {
			t.Fatalf("section %q was dropped or emptied; priority 1 must always survive", must)
		}
	}
	if len(pack.DroppedSections) > 0 && !strings.Contains(pack.Render(), "Evidence NOT included") {
		t.Fatal("sections were dropped and the agent was not told")
	}
}

// ---- diagnosis: validation --------------------------------------------------

// A classification AO does not recognise is refused outright. It is not coerced
// into the nearest valid one, because a wrong label routes a wrong action.
func TestUnknownClassificationIsRefused(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: "probably_fine", Summary: "looks ok to me",
	}, false)
	if inc.Diagnosis != nil {
		t.Fatal("an unrecognised classification was recorded as a diagnosis")
	}
}

// An "insufficient evidence" verdict must name what it lacked, or it is not a
// finding, just a shrug.
func TestInsufficientEvidenceMustNameWhatIsMissing(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyWorkspaceUnattributable, "the worktree no longer matches")
	f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentUnsafeOrInsufficient, Summary: "cannot tell",
	}, false)
}

// A human decision without options gives the person nothing to decide between.
func TestHumanDecisionMustOfferOptions(t *testing.T) {
	f := newAdvisorFixture(t, "dirty_worktree", "the target repository has uncommitted changes")
	f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentHumanDecision, Summary: "you have to choose",
	}, false)
}

// A diagnosis taken against a different pack is refused: it never saw this
// situation.
func TestDiagnosisForAnotherPackIsRefused(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()
	inc, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID)
	if err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	_, err = f.c.SubmitIncidentDiagnosis(ctx, f.runID, workflowcore.IncidentDiagnosisSubmission{
		IncidentID: inc.ID, PackDigest: "some-other-pack",
		Class: workflowcore.IncidentAutoRecoverable, Summary: "continue it",
	})
	if err == nil {
		t.Fatal("a diagnosis for a different pack was accepted")
	}
}

// An unrecognised proposed action degrades to "none" rather than becoming
// durable: an agent must not be able to widen AO's action vocabulary.
func TestUnknownProposedActionDegradesToNone(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentHumanDecision, Summary: "your call",
		Options: []workflowcore.IncidentOption{{ID: "a", Label: "Cancel", Detail: "stop the run"}},
		Action:  &workflowcore.IncidentActionSpec{Kind: "rm -rf /", Reason: "trust me"},
	}, true)
	if inc.Diagnosis.Action.Kind != workflowcore.IncidentActionNone {
		t.Fatalf("proposed action = %q, want it degraded to none", inc.Diagnosis.Action.Kind)
	}
}

// ---- authorization ----------------------------------------------------------

// Class A proposing the one bounded recovery may run without asking, because it
// can do no more than the button the person already has.
func TestAutoRecoverableContinueRunsWithoutApproval(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentAutoRecoverable, Summary: "the cycle was never picked up",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionContinueRun, Reason: "re-deliver it"},
	}, true)

	got, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "")
	if err != nil {
		t.Fatalf("ExecuteIncidentAction: %v", err)
	}
	if got.State != workflowcore.IncidentResolved {
		t.Fatalf("incident state = %q, want resolved", got.State)
	}
}

// Class B never runs on the agent's say-so. This is the self-approval boundary.
func TestRepairAgentRefusesWithoutHumanApproval(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentRepairAO, Summary: "AO resolves the module root wrongly",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionRepairAgent, Reason: "fix the resolver"},
	}, true)

	if _, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, ""); err == nil {
		t.Fatal("a Repair Agent launched without human approval")
	}
	if f.agents.repairs != 0 {
		t.Fatalf("repair launches = %d, want 0", f.agents.repairs)
	}

	got, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "morakurt@icloud.com")
	if err != nil {
		t.Fatalf("ExecuteIncidentAction with approval: %v", err)
	}
	if f.agents.repairs != 1 {
		t.Fatalf("repair launches = %d, want exactly 1", f.agents.repairs)
	}
	if got.State != workflowcore.IncidentResolved {
		t.Fatalf("incident state = %q, want resolved", got.State)
	}
	if n := f.countCheckpointPhase("incident_action_approved"); n != 1 {
		t.Fatalf("approval rows = %d, want exactly 1 durable record of who approved", n)
	}
}

// A classification cannot smuggle in an action from another one. AO believes
// the action, not the label.
func TestClassificationCannotSmuggleAForeignAction(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentAutoRecoverable, Summary: "trivial, just patch AO",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionRepairAgent, Reason: "small change"},
	}, true)

	if _, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "morakurt@icloud.com"); err == nil {
		t.Fatal("an auto_recoverable classification launched a Repair Agent")
	}
	if f.agents.repairs != 0 {
		t.Fatalf("repair launches = %d, want 0", f.agents.repairs)
	}
}

// Class D is a refusal, and a refusal is not a springboard: nothing executes.
func TestInsufficientEvidenceNeverExecutes(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyWorkspaceUnattributable, "the worktree no longer matches")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class:   workflowcore.IncidentUnsafeOrInsufficient,
		Summary: "cannot attribute the change",
		Missing: []string{"who moved HEAD", "the pre-approval diff"},
		Action:  &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionContinueRun, Reason: "just try"},
	}, true)

	if inc.State != workflowcore.IncidentRefused {
		t.Fatalf("incident state = %q, want refused", inc.State)
	}
	if _, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "morakurt@icloud.com"); err == nil {
		t.Fatal("AO acted on a diagnosis that reported insufficient evidence")
	}
}

// ---- staleness --------------------------------------------------------------

// A diagnosis must not outlive the situation it was taken against.
func TestDiagnosisIsNotExecutedAfterTheSituationMoves(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentAutoRecoverable, Summary: "re-deliver it",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionContinueRun, Reason: "safe"},
	}, true)

	// The stop changes underneath: a different reason entirely.
	f.parkAsFixStop(workflowcore.ReasonFixBudgetExhausted, "the reviewer still requests changes")

	if _, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "morakurt@icloud.com"); !errors.Is(err, workflowcore.ErrIncidentStale) {
		t.Fatalf("err = %v, want ErrIncidentStale", err)
	}
	loaded, err := f.c.LoadIncident(context.Background(), f.runID, inc.ID)
	if err != nil {
		t.Fatalf("LoadIncident: %v", err)
	}
	if !loaded.Stale {
		t.Fatal("the incident is not marked stale after its stop changed")
	}
}

// ---- bounds -----------------------------------------------------------------

// Diagnosis is bounded, and the bound is durable: a restart does not refill it.
func TestDiagnosisAttemptsAreBoundedAcrossRestart(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	// Each pass advances past the outstanding-generation window, so every ask
	// is genuinely eligible to launch and the only thing stopping it is the
	// budget — which is the property under test.
	for i := 0; i < 5; i++ {
		if i == 2 {
			f.c = f.newCoordinator()
		}
		f.clk.Advance(20 * time.Minute)
		_, _, _ = f.c.RequestIncidentDiagnosis(ctx, f.runID)
	}
	if f.agents.diagnostics != workflowcore.MaxIncidentDiagnoses {
		t.Fatalf("diagnostic launches = %d, want exactly %d however often it is asked",
			f.agents.diagnostics, workflowcore.MaxIncidentDiagnoses)
	}
}

// The Repair Agent is bounded at one. A repair that did not work is a new
// incident with new evidence, not a retry of a guess.
func TestRepairIsBoundedAtOne(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentRepairAO, Summary: "AO resolves the module root wrongly",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionRepairAgent, Reason: "fix it"},
	}, true)

	ctx := context.Background()
	if _, err := f.c.ExecuteIncidentAction(ctx, f.runID, inc.ID, "morakurt@icloud.com"); err != nil {
		t.Fatalf("first repair: %v", err)
	}
	for i := 0; i < 3; i++ {
		_, _ = f.c.ExecuteIncidentAction(ctx, f.runID, inc.ID, "morakurt@icloud.com")
	}
	if f.agents.repairs != 1 {
		t.Fatalf("repair launches = %d, want exactly 1", f.agents.repairs)
	}
}

// ---- separation of duties ---------------------------------------------------

// The diagnostic agent is launched read-only, and the repair agent is not the
// same launch. Both halves are asserted because the boundary is the feature.
func TestDiagnosticIsReadOnlyAndRepairIsSeparate(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	ctx := context.Background()
	if _, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID); err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	if !f.agents.lastDiagnosticReadOnly {
		t.Fatal("the diagnostic agent was not launched read-only")
	}
	if f.agents.repairs != 0 {
		t.Fatal("diagnosing launched a repair agent")
	}
	if strings.Contains(f.agents.lastDiagnosticPrompt, "implement") {
		t.Fatal("the diagnostic prompt asks the agent to implement something")
	}
	for _, must := range []string{"Do not modify anything", "Do not approve your own conclusion"} {
		if !strings.Contains(f.agents.lastDiagnosticPrompt, must) {
			t.Fatalf("diagnostic prompt is missing %q", must)
		}
	}
}

// The repair prompt must forbid the destructive operations outright and hand
// judgement to the independent reviewer.
func TestRepairPromptForbidsDestructiveWorkAndSelfApproval(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonVerifyInfraFailed, "verification infrastructure failure")
	inc := f.diagnoseWith(t, workflowcore.IncidentDiagnosisSubmission{
		Class: workflowcore.IncidentRepairAO, Summary: "AO resolves the module root wrongly",
		Action: &workflowcore.IncidentActionSpec{Kind: workflowcore.IncidentActionRepairAgent, Reason: "fix it"},
	}, true)
	if _, err := f.c.ExecuteIncidentAction(context.Background(), f.runID, inc.ID, "morakurt@icloud.com"); err != nil {
		t.Fatalf("ExecuteIncidentAction: %v", err)
	}
	for _, must := range []string{"no reset", "no stash", "no force", "NOT the reviewer", "Never modify the database directly"} {
		if !strings.Contains(f.agents.lastRepairPrompt, must) {
			t.Fatalf("repair prompt is missing %q:\n%s", must, f.agents.lastRepairPrompt)
		}
	}
}

// ---- fixture ----------------------------------------------------------------

type fakeIncidentAgents struct {
	diagnostics            int
	repairs                int
	lastDiagnosticPrompt   string
	lastRepairPrompt       string
	lastDiagnosticReadOnly bool
	err                    error
}

func (a *fakeIncidentAgents) LaunchDiagnostic(_ context.Context, req workflowcore.IncidentAgentRequest) (workflowcore.IncidentAgentResult, error) {
	if a.err != nil {
		return workflowcore.IncidentAgentResult{}, a.err
	}
	a.diagnostics++
	a.lastDiagnosticPrompt = req.Prompt
	a.lastDiagnosticReadOnly = req.ReadOnly
	return workflowcore.IncidentAgentResult{SessionID: "diag-session", Harness: "claude-code"}, nil
}

func (a *fakeIncidentAgents) LaunchRepair(_ context.Context, req workflowcore.IncidentAgentRequest) (workflowcore.IncidentAgentResult, error) {
	if a.err != nil {
		return workflowcore.IncidentAgentResult{}, a.err
	}
	a.repairs++
	a.lastRepairPrompt = req.Prompt
	return workflowcore.IncidentAgentResult{SessionID: "repair-session", Harness: "claude-code"}, nil
}

type advisorFixture struct {
	*fixRecoveryFixture
	agents *fakeIncidentAgents
}

// newIncidentFixture drives a run to a fix dispatch and then parks it on the
// given real stop, which is the durable shape every one of these incidents has.
func newAdvisorFixture(t *testing.T, reason, detail string) *advisorFixture {
	t.Helper()
	base := newFixRecoveryFixture(t)
	agents := &fakeIncidentAgents{}
	base.incidentAgents = agents
	base.c = base.newCoordinator()
	base.driveToFixDispatch()
	base.parkAsFixStop(reason, detail)
	return &advisorFixture{fixRecoveryFixture: base, agents: agents}
}

// diagnoseWith requests a diagnosis and submits one. wantAccepted says whether
// the submission is expected to be recorded; a rejected submission must leave
// no diagnosis behind.
func (f *advisorFixture) diagnoseWith(t *testing.T, sub workflowcore.IncidentDiagnosisSubmission, wantAccepted bool) workflowcore.Incident {
	t.Helper()
	ctx := context.Background()
	inc, pack, err := f.c.RequestIncidentDiagnosis(ctx, f.runID)
	if err != nil {
		t.Fatalf("RequestIncidentDiagnosis: %v", err)
	}
	sub.IncidentID = inc.ID
	if sub.PackDigest == "" {
		sub.PackDigest = pack.Digest
	}
	got, err := f.c.SubmitIncidentDiagnosis(ctx, f.runID, sub)
	if wantAccepted && err != nil {
		t.Fatalf("SubmitIncidentDiagnosis: %v", err)
	}
	if !wantAccepted && err == nil {
		t.Fatal("an invalid diagnosis was accepted")
	}
	if !wantAccepted {
		loaded, lerr := f.c.LoadIncident(ctx, f.runID, inc.ID)
		if lerr != nil {
			t.Fatalf("LoadIncident: %v", lerr)
		}
		return loaded
	}
	return got
}

// hugeChangeSet models a worktree far too large to fit in a pack, so the budget
// has something real to push back against.
func hugeChangeSet(n int) []ports.WorkspaceChange {
	out := make([]ports.WorkspaceChange, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ports.WorkspaceChange{
			Status: " M",
			Path:   fmt.Sprintf("backend/internal/generated/very/deep/package_%04d/file_with_a_long_name.go", i),
		})
	}
	return out
}

var _ = domain.WorkflowRunNeedsAttention

// A generation that is already launched is never launched again — by a 2s poll,
// by a double click, or by a daemon that restarted mid-flight. The outbox's
// unique idempotency key is what makes all three converge on one pane.
func TestConcurrentAsksLaunchExactlyOneDiagnosticAgent(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		if i == 3 {
			f.c = f.newCoordinator() // a restart mid-investigation
		}
		inc, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID)
		if err != nil {
			t.Fatalf("ask %d: %v", i+1, err)
		}
		if i > 0 && inc.LaunchOutcome != workflowcore.IncidentAlreadyRunning {
			t.Fatalf("ask %d outcome = %q, want already_running", i+1, inc.LaunchOutcome)
		}
	}
	if f.agents.diagnostics != 1 {
		t.Fatalf("diagnostic launches = %d, want exactly 1 across six asks and a restart", f.agents.diagnostics)
	}
}

// A launch that fails releases its claim, so the incident stays investigable
// rather than being wedged by a transient spawn failure.
func TestFailedLaunchDoesNotBurnTheIncident(t *testing.T) {
	f := newAdvisorFixture(t, workflowcore.ReasonFixCycleNotStarted, "the worker never started this cycle")
	ctx := context.Background()

	f.agents.err = errors.New("runtime refused to create a pane")
	if _, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID); err == nil {
		t.Fatal("expected the launch failure to be reported")
	}

	f.agents.err = nil
	f.clk.Advance(time.Minute)
	inc, _, err := f.c.RequestIncidentDiagnosis(ctx, f.runID)
	if err != nil {
		t.Fatalf("second attempt after a failed launch: %v", err)
	}
	if inc.LaunchOutcome != workflowcore.IncidentLaunched {
		t.Fatalf("outcome = %q, want launched", inc.LaunchOutcome)
	}
	if f.agents.diagnostics != 1 {
		t.Fatalf("successful launches = %d, want 1", f.agents.diagnostics)
	}
}
