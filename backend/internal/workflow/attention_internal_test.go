package workflow

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestHumanDecisionAlwaysCarriesReasonAndAction is Checkpoint 8P-E.13 Phase 2's
// invariant, asserted over the entire canonical vocabulary rather than over a
// hand-picked sample: for EVERY registered reason, and for every attempt error
// class AO knows about, ClassifyAttention either declines to call it a human
// decision or supplies both a reason and a concrete action.
//
// This is the test that makes "no generic needs_attention dead end" a property
// of the code rather than a promise. A future stop reason added to the registry
// without an action cannot silently become an unanswerable "Te necesita" — it
// either gets an action or it is classified ao_internal.
func TestHumanDecisionAlwaysCarriesReasonAndAction(t *testing.T) {
	assert := func(t *testing.T, label string, v AttentionVerdict) {
		t.Helper()
		if v.Attention != AttentionHuman {
			return
		}
		if v.Reason == "" {
			t.Fatalf("%s: human_decision with an empty reason", label)
		}
		if v.Action == "" {
			t.Fatalf("%s: human_decision with an empty action — nothing for the user to do", label)
		}
	}

	for reason := range attentionDispositions {
		d := RunDetail{
			Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
			LatestCheckpointPhase: reason,
		}
		assert(t, "reason "+reason, ClassifyAttention(d, nil, PhaseNeedsAttention))
	}

	for class := range attentionErrorClasses {
		finished := domain.WorkflowAttempt{ID: "a1", ErrorClass: class}
		d := RunDetail{
			Run:   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
			Steps: []StepDetail{{Step: domain.WorkflowStep{ID: "s1"}, Attempts: []domain.WorkflowAttempt{finished}}},
		}
		assert(t, "class "+string(class), ClassifyAttention(d, nil, PhaseNeedsAttention))
	}

	// The two cases that produced the dead end this checkpoint exists to
	// remove: nothing recorded at all, and a durable_phase that names what AO
	// was doing rather than why it stopped.
	for _, phase := range []string{"", "review_observed", "worker_observed_idle", "fix_observed_waiting"} {
		d := RunDetail{
			Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
			LatestCheckpointPhase: phase,
		}
		v := ClassifyAttention(d, nil, PhaseNeedsAttention)
		if v.Attention == AttentionHuman {
			t.Fatalf("durable_phase %q became a human decision; it names an activity, not a stop", phase)
		}
		assert(t, "non-reason phase "+phase, v)
	}
}

// TestEveryRegisteredReasonIsEitherSelfRemediableOrActionable guards the
// registry itself. An entry that is neither — no action and not self-remediable
// — would classify as ao_internal and quietly become a run nobody drives and
// nobody is told about: the dead end wearing a different label.
func TestEveryRegisteredReasonIsEitherSelfRemediableOrActionable(t *testing.T) {
	for reason, disp := range attentionDispositions {
		if !disp.SelfRemediable && disp.HumanAction == "" {
			t.Fatalf("reason %q is neither self-remediable nor actionable: nothing would ever move this run", reason)
		}
		if disp.SelfRemediable && disp.HumanAction != "" {
			t.Fatalf("reason %q is self-remediable but also carries a human action; it must be one or the other", reason)
		}
	}
	for class, disp := range attentionErrorClasses {
		if !disp.SelfRemediable && disp.HumanAction == "" {
			t.Fatalf("error class %q is neither self-remediable nor actionable", class)
		}
	}
}

// TestSelfRemediableStopsAreNeverHumanDecisions pins the other half of the
// separation: a stop AO is actively retrying must never interrupt anyone, and
// must report the phase of what AO is actually doing rather than the durable
// run state's flat "needs_attention".
func TestSelfRemediableStopsAreNeverHumanDecisions(t *testing.T) {
	for reason, disp := range attentionDispositions {
		if !disp.SelfRemediable {
			continue
		}
		d := RunDetail{
			Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
			LatestCheckpointPhase: reason,
		}
		v := ClassifyAttention(d, nil, PhaseNeedsAttention)
		if v.Attention != AttentionInternal {
			t.Fatalf("reason %q classified %q, want ao_internal", reason, v.Attention)
		}
		if v.Phase == "" || v.Phase == PhaseNeedsAttention {
			t.Fatalf("reason %q derived phase %q; a stop AO is handling must say what it is doing", reason, v.Phase)
		}
	}
}

// TestOpenHumanQuestionOutranksEverything: a question addressed to the user is
// the one carrier that always wins, and it always carries its own action.
func TestOpenHumanQuestionOutranksEverything(t *testing.T) {
	d := RunDetail{
		Run:                   domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunNeedsAttention},
		LatestCheckpointPhase: ReasonPlannerRetryScheduled,
	}
	q := []domain.WorkflowQuestion{{State: domain.QuestionStateHumanRequired, QuestionText: "Which database should the migration target?"}}
	v := ClassifyAttention(d, q, PhaseNeedsAttention)
	if v.Attention != AttentionHuman || v.Reason != ReasonQuestionHumanRequired {
		t.Fatalf("verdict = %+v, want a human_decision for the open question", v)
	}
	if v.Action != "Which database should the migration target?" {
		t.Fatalf("action = %q, want the question text itself", v.Action)
	}
}
