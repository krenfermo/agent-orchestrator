package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Recovery for a run already parked on a worker-blocked stop that the evidence
// rule in worker_progress.go would no longer produce.
//
// The incident (wf-57f90ff2, task 4 of master wf-872e7f57): a Codex worker was
// visibly executing commands and running tests. Codex's PermissionRequest hook
// had latched the session at waiting_input — Codex installs no resolving hook,
// so nothing before the turn's Stop could clear it — and AO read that latch as
// proof a person was needed. It scraped the pane, found no question (there was
// none), manufactured a human_required question row with empty text, and parked
// the run. The worker finished its turn unattended two minutes later.
//
// The rows that incident left behind are all still individually well-formed, so
// nothing sweeps them up on its own:
//
//	workflow_runs      needs_attention, newest stop = worker_blocked
//	workflow_steps     work = waiting, session_id set
//	workflow_questions one OPEN row, human_required, no text, no choices
//
// and the question row is load-bearing in the worst way: hasOpenQuestion sees
// it and refuses to dispatch, so the run cannot move even after the classifier
// is fixed.
//
// This is the generic way out. It keys off the shape — an evidence-free
// question, plus a worker-blocked stop that no surviving evidence supports —
// never off a run id, a harness, or an error string. Under the fixed detector
// no such row is ever written again, so in practice it only ever touches
// history.

const (
	// workerBlockedRecheckPhase is the durable record of one recheck: what was
	// retired, and what AO concluded. Not a canonical attention reason — it
	// records a resume, not a stop.
	workerBlockedRecheckPhase = "worker_blocked_recheck"

	// maxWorkerBlockedRechecks bounds how many times one work step's
	// worker-blocked stop may be reopened this way, however many times Continue
	// is pressed. Same reasoning as every other bound in this package: if the
	// stop keeps coming back, the answer is not to keep reopening it.
	maxWorkerBlockedRechecks = 3
)

// workerBlockedRecheck is the decoded form of the recheck checkpoint's
// RetryState.
type workerBlockedRecheck struct {
	Generation        int `json:"generation"`
	RetiredQuestions  int `json:"retiredQuestions"`
	RemainingEvidence int `json:"remainingEvidence"`
}

// resumeFalseWorkerBlocked re-examines a run parked on ReasonWorkerBlocked and,
// when no surviving evidence supports that stop, returns the work step to
// observation.
//
// ContinueRun is its only caller, for the same reason resumeStaleVerifyFailure
// and resumeWorkerLaunchAfterFailure are: THIS call is a person saying "look
// again". Read-time polling never un-parks a human-owned stop, however often it
// re-derives the same state.
//
// It refuses everything that is not exactly this situation. The run must be
// parked, its newest canonical stop must be ReasonWorkerBlocked specifically,
// and the work step must be resting at `waiting` with a session attached. If —
// after retiring the evidence-free question rows that are not evidence under
// provenHumanInputRequest's rule — any real question survives, the stop is
// genuine and is left exactly where it is.
//
// Nothing is decided about the worker here. The step goes back to `running`,
// which is the state in which observeWorkStep evaluates it, and the ordinary
// fact-based rules then classify it from scratch: still working, finished,
// terminated, genuinely blocked, or ambiguous. That is deliberate — this
// function's job is to stop AO answering from a stale conclusion, not to
// substitute a new one.
func (c *Coordinator) resumeFalseWorkerBlocked(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
) (domain.WorkflowRun, domain.WorkflowStep, bool, error) {
	if run.State != domain.WorkflowRunNeedsAttention || run.State.Terminal() {
		return run, step, false, nil
	}
	if step.Kind != domain.WorkflowStepWork || step.State != domain.WorkflowStepWaiting || step.SessionID == nil {
		return run, step, false, nil
	}
	if reason, ok := c.latestCanonicalStopReason(ctx, run.ID); !ok || reason != ReasonWorkerBlocked {
		return run, step, false, nil
	}
	generation := c.workerBlockedRecheckCount(ctx, run.ID, step.ID) + 1
	if generation > maxWorkerBlockedRechecks {
		return run, step, false, nil
	}

	// Retire the rows that only ever recorded "the session said needs-input",
	// then ask whether anything AO actually observed being asked is left.
	retired := c.retireUnevidencedQuestions(ctx, run.ID)
	proven, err := c.provenHumanInputRequest(ctx, run.ID, step.ID)
	if err != nil {
		return run, step, false, err
	}
	if proven {
		// A real question is open on this step. The stop is correct; leave it.
		return run, step, false, nil
	}

	stepID := step.ID
	state, _ := json.Marshal(workerBlockedRecheck{Generation: generation, RetiredQuestions: retired})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		NextAction: fmt.Sprintf(
			"worker_blocked_recheck: no observed question supports this worker-blocked stop (generation %d/%d, %d evidence-free question(s) retired) — returning the work step to observation",
			generation, maxWorkerBlockedRechecks, retired),
		DurablePhase:   workerBlockedRecheckPhase,
		PayloadVersion: "v1",
		RetryState:     string(state),
		CreatedAt:      c.clock(),
	}); err != nil {
		return run, step, false, err
	}

	now := c.clock()
	if !domain.ValidWorkflowStepTransition(step.State, domain.WorkflowStepRunning) {
		return run, step, false, nil
	}
	if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepRunning, now); err != nil {
		return run, step, false, err
	}
	step.State = domain.WorkflowStepRunning

	// Un-park, narrowly: only the worker-blocked stop this function just
	// disproved. A run stopped for anything else never reached here (the reason
	// check above), so no unrelated attention can be cleared by this path.
	run = c.unparkRun(ctx, run, ReasonWorkerBlocked,
		"no question AO actually observed supports the worker-blocked stop; the work step returns to observation")
	if c.log != nil {
		c.log.Info("workflow: reopened a worker-blocked stop with no surviving evidence",
			"run", run.ID, "step", step.ID, "generation", generation, "retiredQuestions", retired)
	}
	return run, step, true, nil
}

// workerBlockedRecheckCount counts how many times this step's worker-blocked
// stop has already been rechecked.
func (c *Coordinator) workerBlockedRecheckCount(ctx stdctx.Context, runID, stepID string) int {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return maxWorkerBlockedRechecks // failing to read is never a licence to reopen
	}
	count := 0
	for _, cp := range cps {
		if cp.WorkflowStepID != nil && *cp.WorkflowStepID == stepID && cp.DurablePhase == workerBlockedRecheckPhase {
			count++
		}
	}
	return count
}
