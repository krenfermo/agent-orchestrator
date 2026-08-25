package ctxregress

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// fixtureRepo builds the self-contained checkout both runs are measured
// against, and pins AO's data dir into the test's temp tree so the router's
// code graph and memory stores never touch the developer's real ~/.ao.
func fixtureRepo(t *testing.T) Fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	t.Setenv("AO_DATA_DIR", t.TempDir())
	fixture, err := WriteFixtureRepo(t.TempDir())
	if err != nil {
		t.Fatalf("WriteFixtureRepo: %v", err)
	}
	if err := IndexFixtureEvidence(context.Background(), fixture); err != nil {
		t.Fatalf("IndexFixtureEvidence: %v", err)
	}
	return fixture
}

// The gate in its healthy state: the router AO actually ships sends materially
// less and reaches exactly the same task, review, and verify outcomes.
func TestShippedRouterSendsLessAndChangesNoOutcome(t *testing.T) {
	comparison, err := Run(context.Background(), Options{Fixture: fixtureRepo(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comparison.Regressed() {
		t.Fatalf("shipped router regressed the fixture outcome: %v", comparison.Regressions)
	}
	for _, run := range []RunOutcome{comparison.Disabled, comparison.Enabled} {
		if run.TaskStatus != StatusCompleted || run.ReviewStatus != StatusApproved || run.VerifyStatus != StatusPassed {
			t.Fatalf("routerEnabled=%v reached %s, want a clean run", run.RouterEnabled, run.StatusLine())
		}
	}
	pct, ok := comparison.ContextReductionPercent()
	if !ok {
		t.Fatal("no measured reduction percentage; the unrouted run sent nothing measurable")
	}
	if pct <= 0 {
		t.Fatalf("routing sent %d bytes against an unrouted %d (%.1f%%); the fixture must be large enough to route",
			comparison.Enabled.ContextSentBytes, comparison.Disabled.ContextSentBytes, pct)
	}
	// The routed run draws on the stores AO already built; the unrouted one
	// cannot, because nothing in that path consults them.
	if comparison.Enabled.ReusedBytes <= 0 {
		t.Fatal("the routed run reused nothing from the code graph or project memory, so the reused/new split is untested")
	}
	if comparison.Disabled.ReusedBytes != 0 {
		t.Fatalf("the unrouted run reported %d reused bytes; nothing in that path reads AO's stores", comparison.Disabled.ReusedBytes)
	}
}

// The gate doing its job: a budget too small to carry the task's evidence is a
// regression even though it saves more context than the shipped one.
func TestOutcomeGateFailsOnABudgetThatDropsTheEvidence(t *testing.T) {
	fixture := fixtureRepo(t)
	starved, err := contextrouter.New(contextrouter.Options{
		Budgets: contextrouter.BudgetSet{
			contextrouter.RolePlanner:  {CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 80},
			contextrouter.RoleWorker:   {CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 80},
			contextrouter.RoleReviewer: {CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 80},
			contextrouter.RoleFix:      {CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 80},
			contextrouter.RoleVerify:   {CompactTokens: 40, ExpandedTokens: 60, HardCapTokens: 80},
		},
		Diff: contextrouter.NewGitDiffSource(),
	})
	if err != nil {
		t.Fatalf("build starved router: %v", err)
	}
	comparison, err := Run(context.Background(), Options{Fixture: fixture, Router: starved})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !comparison.Regressed() {
		t.Fatalf("a budget that dropped every required fact was not reported as a regression: %s vs %s",
			comparison.Disabled.StatusLine(), comparison.Enabled.StatusLine())
	}
	if len(comparison.Enabled.MissingFacts) == 0 {
		t.Fatal("the routed run reported no missing facts, so the regression was detected for the wrong reason")
	}
	// The saving is real and irrelevant: the gate is the outcome.
	if pct, ok := comparison.ContextReductionPercent(); !ok || pct <= 0 {
		t.Fatalf("expected the starved router to save context (got %.1f%%, ok=%v)", pct, ok)
	}
	if comparison.Enabled.FileReads <= comparison.Disabled.FileReads {
		t.Fatalf("routed run made %d reads against %d unrouted; searching for dropped evidence should cost reads",
			comparison.Enabled.FileReads, comparison.Disabled.FileReads)
	}
	var report bytes.Buffer
	if err := comparison.Report(&report); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !strings.Contains(report.String(), "QUALITY REGRESSION") {
		t.Fatalf("report does not name the regression:\n%s", report.String())
	}
	if !strings.Contains(report.String(), "measured context reduction:") {
		t.Fatalf("report does not carry the measured reduction:\n%s", report.String())
	}
}

// The metrics half: every dispatch that carries a payload also carries a
// routing block, the disabled run says so, and the routed run's block accounts
// for what it sent by origin.
func TestRoutingMetricsRideAlongsideTheBaselineEvidence(t *testing.T) {
	comparison, err := Run(context.Background(), Options{Fixture: fixtureRepo(t)})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, record := range comparison.Disabled.Records {
		if record.Context.ContextSentBytes.Value == nil {
			continue
		}
		if record.Routing == nil {
			t.Fatalf("%s record carries a payload but no routing block", record.Role)
		}
		if record.Routing.Enabled {
			t.Fatalf("%s record reports routing enabled in the router-disabled run", record.Role)
		}
		if strings.TrimSpace(record.Routing.Reason) == "" {
			t.Fatalf("%s record reports routing disabled without saying why", record.Role)
		}
	}

	var routed int
	for _, record := range comparison.Enabled.Records {
		if record.Routing == nil || !record.Routing.Enabled {
			continue
		}
		routed++
		r := record.Routing
		if r.SchemaVersion != projectmemory.RoutingSchemaVersion {
			t.Fatalf("routing block version %q, want %q", r.SchemaVersion, projectmemory.RoutingSchemaVersion)
		}
		potential, selected := r.PotentialBytes.Value, r.SelectedBytes.Value
		reused, fresh := r.ReusedBytes.Value, r.NewBytes.Value
		if potential == nil || selected == nil || reused == nil || fresh == nil {
			t.Fatalf("%s routing block left a size unmeasured: %+v", record.Role, r)
		}
		if *reused+*fresh != *selected {
			t.Fatalf("%s routing block: reused %d + new %d != selected %d", record.Role, *reused, *fresh, *selected)
		}
		if *selected > *potential {
			t.Fatalf("%s routing block selected %d of a potential %d", record.Role, *selected, *potential)
		}
		if r.PotentialBytes.Basis != projectmemory.BasisMeasured || r.SelectedBytes.Basis != projectmemory.BasisMeasured {
			t.Fatalf("%s routing sizes are not labeled measured: %+v", record.Role, r)
		}
		if r.PotentialTokens.Basis != projectmemory.BasisEstimated || r.SelectedTokens.Basis != projectmemory.BasisEstimated {
			t.Fatalf("%s routing token figures are not labeled estimated: %+v", record.Role, r)
		}
	}
	if routed == 0 {
		t.Fatal("no dispatch in the router-enabled run recorded a routing decision")
	}
}

// The additive-schema promise: a record with no routing story serialises
// exactly as it did before this block existed.
func TestRoutingBlockIsAbsentWhenThereIsNothingToSay(t *testing.T) {
	recorder := projectmemory.NewRecorder(nil)
	span := recorder.Begin(projectmemory.Dispatch{Role: "verify", Observable: projectmemory.Capabilities{ToolCalls: true}})
	span.ObserveToolCall("go build")
	record := span.Build(nil)
	if record.Routing != nil {
		t.Fatalf("a dispatch that carries no payload grew a routing block: %+v", record.Routing)
	}
}
