package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// session_compaction_optin_test.go -- the per-run opt-in, end to end.
//
// Everything in session_compaction_test.go proves what COMPACT does once the
// flag is true. It got there by writing a policy snapshot by hand, because
// until now there was no other way: default off, no API field, no inheritance.
// These tests use the production path instead -- the two Apply methods the
// create route calls -- and finish with the contract fixture for the
// controlled experiment the whole change exists to make possible.

func policySnapshotOf(t *testing.T, store *fakeStore, runID string) domain.WorkflowPolicy {
	t.Helper()
	run, ok, err := store.GetWorkflowRun(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v ok=%t", runID, err, ok)
	}
	var p domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &p); err != nil {
		t.Fatalf("decode policy snapshot %q: %v", run.PolicySnapshot, err)
	}
	return p
}

// newPendingRun creates a run and leaves it pending, which is the only state
// either freeze is accepted in.
func newPendingRun(t *testing.T) (*workflowcore.Coordinator, *fakeStore, string) {
	t.Helper()
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC)}
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: &fakeSpawner{}, Clock: clk.Now,
	})
	created, err := c.CreateRun(context.Background(), "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return c, store, created.Run.ID
}

// PHASE E, cases 1-3. A freshly created run is off and unrecorded; an explicit
// choice in either direction is written durably, with its provenance.
func TestApplySessionCompactionPolicyFreezesAnExplicitChoice(t *testing.T) {
	ctx := context.Background()

	t.Run("creation alone opts nobody in", func(t *testing.T) {
		_, store, runID := newPendingRun(t)
		p := policySnapshotOf(t, store, runID)
		if p.SessionCompactionEnabled {
			t.Fatal("a newly created run has compaction enabled")
		}
		if _, recorded := p.SessionCompactionRequested(); recorded {
			t.Fatalf("a newly created run claims a recorded choice: %+v", p.CompactionProvenance)
		}
	})

	for _, enabled := range []bool{true, false} {
		t.Run("explicit choice is durable", func(t *testing.T) {
			c, store, runID := newPendingRun(t)
			if err := c.ApplySessionCompactionPolicy(ctx, runID, enabled, "operator-1"); err != nil {
				t.Fatalf("ApplySessionCompactionPolicy(%t): %v", enabled, err)
			}
			p := policySnapshotOf(t, store, runID)
			if p.SessionCompactionEnabled != enabled {
				t.Fatalf("SessionCompactionEnabled = %t, want %t", p.SessionCompactionEnabled, enabled)
			}
			got, recorded := p.SessionCompactionRequested()
			if !recorded || got != enabled {
				t.Fatalf("SessionCompactionRequested() = (%t, %t), want (%t, true)", got, recorded, enabled)
			}
			if p.CompactionProvenance.Source != domain.SessionCompactionExplicit {
				t.Fatalf("source = %q, want explicit", p.CompactionProvenance.Source)
			}
			if p.CompactionProvenance.RequestedBy != "operator-1" {
				t.Fatalf("requestedBy = %q, want operator-1", p.CompactionProvenance.RequestedBy)
			}
			if p.CompactionProvenance.At.IsZero() {
				t.Fatal("the freeze recorded no time")
			}
			// Nothing else in the snapshot moved.
			def := domain.DefaultWorkflowPolicy()
			if p.MaxFixCycles != def.MaxFixCycles || p.MaxWorkProviderAttempts != def.MaxWorkProviderAttempts {
				t.Fatalf("the compaction freeze disturbed another budget: %+v", p)
			}
		})
	}
}

// PHASE E, case 10. Neither knob can be moved once the run has left pending, so
// a conversation that has already grown under one contract is never judged
// under another.
func TestContextEconomyFreezesAreRefusedAfterThePendingWindow(t *testing.T) {
	ctx := context.Background()
	c, store, runID := newPendingRun(t)
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now().UTC()); err != nil {
		t.Fatalf("UpdateWorkflowRunState: %v", err)
	}
	if err := c.ApplySessionCompactionPolicy(ctx, runID, true, "operator-1"); !errors.Is(err, workflowcore.ErrInvalid) {
		t.Fatalf("ApplySessionCompactionPolicy on a running run = %v, want ErrInvalid", err)
	}
	if err := c.ApplyContextPerCallWarnTokens(ctx, runID, 70_000); !errors.Is(err, workflowcore.ErrInvalid) {
		t.Fatalf("ApplyContextPerCallWarnTokens on a running run = %v, want ErrInvalid", err)
	}
	if policySnapshotOf(t, store, runID).SessionCompactionEnabled {
		t.Fatal("a refused freeze still enabled compaction")
	}
}

// PHASE E, cases 7-8. An in-range threshold is frozen; an out-of-range one is
// REFUSED rather than clamped, so a run never executes against a threshold
// other than the one its creator named.
func TestApplyContextPerCallWarnTokens(t *testing.T) {
	ctx := context.Background()

	t.Run("a valid override is persisted and effective", func(t *testing.T) {
		c, store, runID := newPendingRun(t)
		if err := c.ApplyContextPerCallWarnTokens(ctx, runID, 70_000); err != nil {
			t.Fatalf("ApplyContextPerCallWarnTokens: %v", err)
		}
		p := policySnapshotOf(t, store, runID)
		budget := p.EffectiveUsageBudgetPolicy()
		if budget.WorkflowContextPerCallWarnTokens != 70_000 {
			t.Fatalf("stored threshold = %d, want 70000", budget.WorkflowContextPerCallWarnTokens)
		}
		if budget.Version == "" {
			t.Fatal("the freeze left the usage policy unversioned, so inheritance would not carry it")
		}
		if got := domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask).
			WithOverrides(budget).ContextPerCallTokens; got != 70_000 {
			t.Fatalf("effective threshold = %d, want 70000", got)
		}
		// PHASE E, case 9: no hard ceiling appeared.
		if budget.Configured() {
			t.Fatalf("an advisory override created a budget ceiling: %+v", budget)
		}
	})

	for _, bad := range []int64{0, 1, -1, domain.MinContextPerCallWarnTokens - 1, domain.MaxContextPerCallWarnTokens + 1} {
		t.Run("refused", func(t *testing.T) {
			c, store, runID := newPendingRun(t)
			err := c.ApplyContextPerCallWarnTokens(ctx, runID, bad)
			if !errors.Is(err, workflowcore.ErrInvalid) {
				t.Fatalf("ApplyContextPerCallWarnTokens(%d) = %v, want ErrInvalid", bad, err)
			}
			if !strings.Contains(err.Error(), "contextPerCallWarnTokens") {
				t.Fatalf("error %q does not name the field", err)
			}
			if p := policySnapshotOf(t, store, runID); p.Usage.WorkflowContextPerCallWarnTokens != 0 {
				t.Fatalf("a refused value was persisted anyway: %d", p.Usage.WorkflowContextPerCallWarnTokens)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PHASE F: the controlled experiment, as a contract fixture.
//
// RUN A and RUN B differ in exactly ONE field. Everything else -- objective,
// strategy, the context-pressure threshold, every other policy knob -- is
// identical, which is the only thing that makes the two comparable.
//
// Both runs are driven to the same fix cycle against the same conversation
// size: 90,000 tokens. That number is chosen to be BELOW the Task profile's own
// 150,000 default and ABOVE the 70,000 override, so the test also proves the
// override is load-bearing -- without it neither arm would reach COMPACT at
// all, and the experiment would be two identical control runs.
//
// No model is called: fakeCompactingSender records the request and returns.
// ---------------------------------------------------------------------------

const (
	experimentContextTokens   int64 = 90_000
	experimentThresholdTokens int64 = 70_000
)

func configureExperimentArm(t *testing.T, compaction bool) func(*workflowcore.Coordinator, *fakeStore, *fakeClock, string) {
	t.Helper()
	return func(c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, runID string) {
		ctx := context.Background()
		// The strategy stamp is what creation's own freeze writes; the fake
		// store's CreateRun does not, so it is set here for both arms alike.
		if err := c.ApplyContextPerCallWarnTokens(ctx, runID, experimentThresholdTokens); err != nil {
			t.Fatalf("ApplyContextPerCallWarnTokens: %v", err)
		}
		if err := c.ApplySessionCompactionPolicy(ctx, runID, compaction, "experiment"); err != nil {
			t.Fatalf("ApplySessionCompactionPolicy: %v", err)
		}
		stampTaskStrategy(t, store, runID, clk.Now())
	}
}

func stampTaskStrategy(t *testing.T, store *fakeStore, runID string, now time.Time) {
	t.Helper()
	p := policySnapshotOf(t, store, runID)
	p.Strategy = domain.ExecutionStrategySelection{Effective: domain.ExecutionStrategyTask}
	snapshot, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := store.UpdateWorkflowRunPolicySnapshot(context.Background(), runID, string(snapshot), now); err != nil {
		t.Fatalf("UpdateWorkflowRunPolicySnapshot: %v", err)
	}
}

func TestControlledCompactionExperimentRunAAndRunB(t *testing.T) {
	senderA := &fakeCompactingSender{supported: true}
	runA := newCompactionFixtureWithPolicy(t, experimentContextTokens, senderA, configureExperimentArm(t, false))

	senderB := &fakeCompactingSender{supported: true}
	runB := newCompactionFixtureWithPolicy(t, experimentContextTokens, senderB, configureExperimentArm(t, true))

	policyA := policySnapshotOf(t, runA.store, runA.runID)
	policyB := policySnapshotOf(t, runB.store, runB.runID)

	// 1. The two arms differ in exactly one field.
	if policyA.SessionCompactionEnabled || !policyB.SessionCompactionEnabled {
		t.Fatalf("arms are not opposed: A=%t B=%t",
			policyA.SessionCompactionEnabled, policyB.SessionCompactionEnabled)
	}
	normalisedA, normalisedB := policyA, policyB
	normalisedA.SessionCompactionEnabled, normalisedB.SessionCompactionEnabled = false, false
	normalisedA.CompactionProvenance, normalisedB.CompactionProvenance = domain.SessionCompactionProvenance{}, domain.SessionCompactionProvenance{}
	gotA, err := json.Marshal(normalisedA)
	if err != nil {
		t.Fatalf("marshal A: %v", err)
	}
	gotB, err := json.Marshal(normalisedB)
	if err != nil {
		t.Fatalf("marshal B: %v", err)
	}
	if string(gotA) != string(gotB) {
		t.Fatalf("the two arms differ in more than compaction:\nA=%s\nB=%s", gotA, gotB)
	}

	// 2. Both were explicitly recorded -- the control arm is a decision, not a gap.
	if enabled, recorded := policyA.SessionCompactionRequested(); enabled || !recorded {
		t.Fatalf("RUN A provenance = (%t, %t), want an explicit false", enabled, recorded)
	}
	if enabled, recorded := policyB.SessionCompactionRequested(); !enabled || !recorded {
		t.Fatalf("RUN B provenance = (%t, %t), want an explicit true", enabled, recorded)
	}

	// 3. Both share the same threshold, and it is the override rather than the
	//    profile default -- otherwise 90,000 tokens would be under the line and
	//    neither arm would reach COMPACT.
	for name, p := range map[string]domain.WorkflowPolicy{"A": policyA, "B": policyB} {
		got := domain.UsageBudgetProfileFor(p.Strategy.Effective).
			WithOverrides(p.EffectiveUsageBudgetPolicy()).ContextPerCallTokens
		if got != experimentThresholdTokens {
			t.Fatalf("RUN %s threshold = %d, want %d", name, got, experimentThresholdTokens)
		}
	}
	if experimentContextTokens >= domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask).ContextPerCallTokens {
		t.Fatalf("the experiment's %d-token conversation is not below the %d default; the override proves nothing",
			experimentContextTokens, domain.UsageBudgetProfileFor(domain.ExecutionStrategyTask).ContextPerCallTokens)
	}

	// 4. BOTH arms reach the same lifecycle decision -- COMPACT -- because the
	//    decision is about context pressure, not about the opt-in. What the
	//    opt-in changes is whether the ACT happens.
	decisionA, okA := fixLifecycleDecision(t, runA.store, runA.runID)
	decisionB, okB := fixLifecycleDecision(t, runB.store, runB.runID)
	if !okA || !okB {
		t.Fatalf("no fix_worker lifecycle decision recorded (A=%t B=%t)", okA, okB)
	}
	if decisionA.Action != domain.LifecycleCompact || decisionB.Action != domain.LifecycleCompact {
		t.Fatalf("actions A=%q B=%q, want both compact at %d tokens against a %d threshold",
			decisionA.Action, decisionB.Action, experimentContextTokens, experimentThresholdTokens)
	}

	// 5. And only RUN B actually compacts.
	if senderA.compactCalls != 0 {
		t.Fatalf("RUN A made %d compaction requests; the control arm must make none", senderA.compactCalls)
	}
	if decisionA.CompactionRequested {
		t.Fatal("RUN A recorded a compaction it never made")
	}
	if senderB.compactCalls != 1 {
		t.Fatalf("RUN B made %d compaction requests, want 1", senderB.compactCalls)
	}
	if !decisionB.CompactionRequested {
		t.Fatal("RUN B did not record the compaction it made")
	}
}
