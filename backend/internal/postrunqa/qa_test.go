package postrunqa_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
)

func TestWithDefaults_ZeroValueGetsPendingPhaseAndTwoRepairCycles(t *testing.T) {
	got := postrunqa.QARun{}.WithDefaults()

	if got.Phase != postrunqa.PhasePending {
		t.Fatalf("zero-value phase = %q, want %q", got.Phase, postrunqa.PhasePending)
	}
	if got.MaxRepairCycles != 2 {
		t.Fatalf("zero-value max repair cycles = %d, want 2", got.MaxRepairCycles)
	}
	if got.MaxRepairCycles != postrunqa.DefaultMaxRepairCycles {
		t.Fatalf("zero-value max repair cycles = %d, want DefaultMaxRepairCycles (%d)",
			got.MaxRepairCycles, postrunqa.DefaultMaxRepairCycles)
	}
	if got.RepairCycleCount != 0 {
		t.Fatalf("zero-value repair cycle count = %d, want 0", got.RepairCycleCount)
	}
	if got.Result != postrunqa.ResultUnset {
		t.Fatalf("zero-value result = %q, want unset", got.Result)
	}
	if got.CompletedAt != nil {
		t.Fatalf("zero-value completed_at = %v, want nil", got.CompletedAt)
	}
}

func TestWithDefaults_LeavesExplicitValuesAlone(t *testing.T) {
	run := postrunqa.QARun{
		Phase:            postrunqa.PhaseAutoFixing,
		MaxRepairCycles:  5,
		RepairCycleCount: 1,
	}

	got := run.WithDefaults()

	if got.Phase != postrunqa.PhaseAutoFixing {
		t.Fatalf("phase = %q, want %q", got.Phase, postrunqa.PhaseAutoFixing)
	}
	if got.MaxRepairCycles != 5 {
		t.Fatalf("max repair cycles = %d, want the explicit 5", got.MaxRepairCycles)
	}
	if got.RepairCycleCount != 1 {
		t.Fatalf("repair cycle count = %d, want 1", got.RepairCycleCount)
	}
}

func TestRepairBudgetExhausted(t *testing.T) {
	tests := []struct {
		name string
		run  postrunqa.QARun
		want bool
	}{
		{"fresh run under the default budget", postrunqa.QARun{}, false},
		{"one cycle spent of the default two", postrunqa.QARun{RepairCycleCount: 1}, false},
		// The default budget applies even when the field was never set, so a
		// run that never wrote MaxRepairCycles still stops after two cycles
		// instead of comparing against a zero budget and stopping immediately.
		{"default budget spent", postrunqa.QARun{RepairCycleCount: 2}, true},
		{"explicit budget not yet spent", postrunqa.QARun{RepairCycleCount: 2, MaxRepairCycles: 3}, false},
		{"explicit budget spent", postrunqa.QARun{RepairCycleCount: 3, MaxRepairCycles: 3}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run.RepairBudgetExhausted(); got != tc.want {
				t.Fatalf("RepairBudgetExhausted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPhaseTerminal(t *testing.T) {
	terminal := []postrunqa.QAPhase{postrunqa.PhaseClean, postrunqa.PhaseNeedsAttention}
	live := []postrunqa.QAPhase{postrunqa.PhasePending, postrunqa.PhaseChecking, postrunqa.PhaseAutoFixing}

	for _, p := range terminal {
		if !p.Valid() || !p.Terminal() {
			t.Fatalf("%q: want a valid terminal phase", p)
		}
	}
	for _, p := range live {
		if !p.Valid() || p.Terminal() {
			t.Fatalf("%q: want a valid non-terminal phase", p)
		}
	}
	if postrunqa.QAPhase("done").Valid() {
		t.Fatal(`QAPhase("done") reported valid`)
	}
}

func TestValidate(t *testing.T) {
	started := time.Date(2026, time.August, 22, 9, 0, 0, 0, time.UTC)
	base := func() postrunqa.QARun {
		return postrunqa.QARun{
			ID:          "qa-1",
			SubjectKind: postrunqa.SubjectTask,
			SubjectID:   "task-1",
			StartedAt:   started,
		}.WithDefaults()
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}

	tests := map[string]func(*postrunqa.QARun){
		"missing id":           func(r *postrunqa.QARun) { r.ID = "" },
		"missing subject id":   func(r *postrunqa.QARun) { r.SubjectID = "" },
		"unknown subject kind": func(r *postrunqa.QARun) { r.SubjectKind = "session" },
		"unknown phase":        func(r *postrunqa.QARun) { r.Phase = "fixing" },
		"unknown result":       func(r *postrunqa.QARun) { r.Result = "passed" },
		"zero started_at":      func(r *postrunqa.QARun) { r.StartedAt = time.Time{} },
		"unknown attribution": func(r *postrunqa.QARun) {
			r.Findings = []postrunqa.Finding{{Attribution: "maybe", Severity: postrunqa.SeverityMinor}}
		},
		"unknown severity": func(r *postrunqa.QARun) {
			r.Findings = []postrunqa.Finding{{Attribution: postrunqa.AttributionNew, Severity: "bad"}}
		},
		"negative repair count": func(r *postrunqa.QARun) { r.RepairCycleCount = -1 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			run := base()
			mutate(&run)
			if err := run.Validate(); err == nil {
				t.Fatalf("%s: Validate() accepted an invalid run", name)
			}
		})
	}
}
