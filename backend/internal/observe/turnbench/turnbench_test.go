package turnbench

import (
	"os"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// turnbench_test.go -- the before/after, against the run that started this.
//
// testdata/wf-1c2cb9bd.json is the recorded shape of the worker session of run
// wf-1c2cb9bd (2026-09-09, project medusa): 193 provider calls for a
// one-field defect. It is numbers and a closed vocabulary -- context tokens,
// output tokens, a turn class, a segment label -- and no prompt, command, path
// or message body is reproduced in it.

func loadWorkedExample(t *testing.T) Series {
	t.Helper()
	f, err := os.Open("testdata/wf-1c2cb9bd.json")
	if err != nil {
		t.Fatalf("open worked example: %v", err)
	}
	defer f.Close()
	s, err := LoadSeries(f)
	if err != nil {
		t.Fatalf("load worked example: %v", err)
	}
	return s
}

// The fixture must keep reproducing the figures AO's own ledger recorded for
// this run. If it stops, the benchmark below is comparing against something
// other than what happened.
func TestWorkedExampleMatchesTheRecordedLedger(t *testing.T) {
	m := Measure(loadWorkedExample(t))
	if m.Calls != 193 {
		t.Errorf("calls = %d, want 193", m.Calls)
	}
	if m.CumulativeInput != 36_082_816 {
		t.Errorf("cumulative input = %d, want 36,082,816", m.CumulativeInput)
	}
	if m.OutputTokens != 139_003 {
		t.Errorf("output = %d, want 139,003", m.OutputTokens)
	}
	if m.FirstContext != 54_404 || m.LastContext != 323_738 || m.PeakContext != 323_738 {
		t.Errorf("context first/last/peak = %d/%d/%d, want 54,404/323,738/323,738",
			m.FirstContext, m.LastContext, m.PeakContext)
	}
	if !m.ElapsedKnown || m.Elapsed.Minutes() < 46 || m.Elapsed.Minutes() > 48 {
		t.Errorf("elapsed = %v (known=%v), want about 47 minutes", m.Elapsed, m.ElapsedKnown)
	}
}

// THE FINDING THAT REDIRECTED THIS WHOLE CHECKPOINT. The expensive run was not
// wasteful in turn KIND. 191 of its 193 calls were work; there was no polling
// at all. So no amount of eliminating coordination turns could have paid for
// it, and the levers are elsewhere.
func TestWorkedExampleWasAlmostEntirelyWork(t *testing.T) {
	m := Measure(loadWorkedExample(t))
	if m.Turns.Unclassified != 0 {
		t.Fatalf("unclassified = %d, want every call classified", m.Turns.Unclassified)
	}
	if got := m.Turns.Count(domain.TurnCommand); got != 183 {
		t.Errorf("command turns = %d, want 183", got)
	}
	if got := m.Turns.Count(domain.TurnEdit); got != 8 {
		t.Errorf("edit turns = %d, want 8", got)
	}
	if got := m.Turns.Count(domain.TurnWait); got != 0 {
		t.Errorf("wait turns = %d, want 0 -- this run did not poll", got)
	}
	if got := m.Turns.Count(domain.TurnSubagent); got != 0 {
		t.Errorf("subagent turns = %d, want 0", got)
	}
	share, ok := m.Turns.CoordinationShare()
	if !ok || share != 1 {
		t.Errorf("coordination share = %d%% (known=%v), want 1%%", share, ok)
	}
}

// Where the money went: 75 of the 193 calls were repair cycles, each running
// against a conversation the base exploration had already grown to 197k-324k.
func TestRepairCyclesRanAgainstTheBaseExploration(t *testing.T) {
	order, by := Segments(loadWorkedExample(t))
	want := []string{"work", "fix/1", "fix/2", "fix/3"}
	if len(order) != len(want) {
		t.Fatalf("segments = %v, want %v", order, want)
	}
	var repairCalls, repairInput int64
	for _, name := range order[1:] {
		m := Measure(by[name])
		repairCalls += m.Calls
		repairInput += m.CumulativeInput
		if m.FirstContext < 190_000 {
			t.Errorf("segment %s started at %d, want it to have inherited the base exploration", name, m.FirstContext)
		}
	}
	if repairCalls != 75 {
		t.Errorf("repair calls = %d, want 75", repairCalls)
	}
	whole := Measure(loadWorkedExample(t))
	share := 100 * repairInput / whole.CumulativeInput
	if share < 50 {
		t.Errorf("repair share of billable input = %d%%, want the majority", share)
	}
}

// THE BENCHMARK. Same 193 calls, same work, same order, same outputs -- with
// the conversation replaced at each repair boundary instead of inherited.
//
// postCompactContext is the one figure a replay cannot observe, because no
// call in the recorded series was ever made against a compacted conversation.
// It is set to this run's OWN first-call context: the harness's standing
// overhead plus the task, which is what a session carries before it has
// explored anything, plus 3,000 for the summary and the fact pack AO sends
// after the directive. Choosing the run's own measured floor rather than an
// optimistic number is what keeps this a projection and not a wish; the
// sensitivity test below says what happens when it is wrong.
const postCompactContext = 54_404 + 3_000

func TestCompactingAtRepairBoundariesCutsBillableInput(t *testing.T) {
	before := loadWorkedExample(t)
	after := Apply(before, Policy{CompactAtSegmentBoundary: true, PostCompactContextTokens: postCompactContext})
	cmp := Compare(before, after)

	// The levers that must NOT have moved, or the comparison is not comparing
	// the same work.
	if cmp.After.Calls != cmp.Before.Calls {
		t.Fatalf("calls moved: %d -> %d; the replay must hold the call count", cmp.Before.Calls, cmp.After.Calls)
	}
	if cmp.After.OutputTokens != cmp.Before.OutputTokens {
		t.Fatalf("output moved: %d -> %d", cmp.Before.OutputTokens, cmp.After.OutputTokens)
	}
	if cmp.After.Turns.Classified != cmp.Before.Turns.Classified {
		t.Fatal("the turn mix must be untouched by a context policy")
	}

	reduction := ReductionPercent(cmp.Before.CumulativeInput, cmp.After.CumulativeInput)
	t.Logf("BEFORE  calls=%d peak=%d cumulative_input=%d output=%d elapsed=%v",
		cmp.Before.Calls, cmp.Before.PeakContext, cmp.Before.CumulativeInput, cmp.Before.OutputTokens, cmp.Before.Elapsed)
	t.Logf("AFTER   calls=%d peak=%d cumulative_input=%d output=%d elapsed=%v",
		cmp.After.Calls, cmp.After.PeakContext, cmp.After.CumulativeInput, cmp.After.OutputTokens, cmp.After.Elapsed)
	t.Logf("REDUCTION cumulative_input=%.1f%% peak=%.1f%%",
		reduction, ReductionPercent(cmp.Before.PeakContext, cmp.After.PeakContext))

	// The success criterion this checkpoint was given: at least 30% off one of
	// the two levers. Pinned so a later change that quietly loses the saving
	// fails here rather than in a bill.
	if reduction < 30 {
		t.Fatalf("cumulative input reduction = %.1f%%, want at least 30%%", reduction)
	}
	if cmp.After.PeakContext >= cmp.Before.PeakContext {
		t.Fatalf("peak context = %d, want it below the %d the un-compacted run reached",
			cmp.After.PeakContext, cmp.Before.PeakContext)
	}
}

// How much the answer depends on the one number a replay has to assume. A
// saving that only survives an optimistic post-compaction size would not be a
// finding, it would be a choice of constant.
func TestTheSavingSurvivesAPessimisticPostCompactionSize(t *testing.T) {
	before := loadWorkedExample(t)
	for _, level := range []int64{57_404, 80_000, 100_000, 120_000} {
		after := Apply(before, Policy{CompactAtSegmentBoundary: true, PostCompactContextTokens: level})
		cmp := Compare(before, after)
		reduction := ReductionPercent(cmp.Before.CumulativeInput, cmp.After.CumulativeInput)
		t.Logf("post-compaction context %7d -> cumulative input %10d (%.1f%% less)",
			level, cmp.After.CumulativeInput, reduction)
		if reduction < 25 {
			t.Errorf("at a post-compaction context of %d the reduction is %.1f%%, below the 25%% floor",
				level, reduction)
		}
	}
}

// A policy that changes nothing must measure as changing nothing. The obvious
// property, and the one that would catch a replay quietly rewriting a series
// it was only meant to read.
func TestAnInertPolicyIsAnIdentity(t *testing.T) {
	before := loadWorkedExample(t)
	after := Apply(before, Policy{})
	cmp := Compare(before, after)
	if !reflect.DeepEqual(cmp.Before, cmp.After) {
		t.Fatalf("an inert policy changed the metrics:\n before=%+v\n  after=%+v", cmp.Before, cmp.After)
	}
	if ReductionPercent(cmp.Before.CumulativeInput, cmp.After.CumulativeInput) != 0 {
		t.Fatal("an inert policy must reduce nothing")
	}
	if ReductionPercent(0, 0) != 0 {
		t.Fatal("a percentage of nothing must be zero, not a division")
	}
}
