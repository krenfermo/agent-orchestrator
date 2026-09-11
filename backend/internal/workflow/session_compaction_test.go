package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// session_compaction_test.go -- Checkpoint P7.
//
// Two behaviours are pinned here and they pull in opposite directions, which
// is the point. AO must now ask a session to shrink its own conversation when
// that conversation is measurably large; and the fix cycle it was about to
// deliver must arrive intact whatever happens to that request. Every test
// below is one or the other.

// fakeContextFacts is the per-session context reading. The fixture runs one
// session, so it answers the same reading for any id -- an unobservable zero
// value unless the test installed one, which is the state every session is in
// before the usage pipeline has placed one of its calls in time.
type fakeContextFacts struct {
	reading domain.SessionContextReading
}

func (f *fakeContextFacts) GetSessionContextReading(_ context.Context, _ string) domain.SessionContextReading {
	return f.reading
}

// fakeCompactingSender is a message sender that also carries the compaction
// capability, the way *session_manager.Manager does in production.
type fakeCompactingSender struct {
	fakeMessageSender
	compactCalls int
	compactFocus string
	supported    bool
	compactErr   error
}

func (f *fakeCompactingSender) CompactConversation(_ context.Context, _ domain.SessionID, focus string) (bool, error) {
	f.compactCalls++
	f.compactFocus = focus
	if f.compactErr != nil {
		return false, f.compactErr
	}
	return f.supported, nil
}

type compactionFixture struct {
	c      *workflowcore.Coordinator
	store  *fakeStore
	clk    *fakeClock
	sender *fakeCompactingSender
	facts  *fakeContextFacts
	runID  string
}

// newCompactionFixture drives a run to changes_requested with a compaction-
// capable sender and a context reading installed for the worker's session.
func newCompactionFixture(t *testing.T, contextTokens int64, compactionEnabled bool, sender *fakeCompactingSender) compactionFixture {
	t.Helper()
	return newCompactionFixtureWithPolicy(t, contextTokens, sender,
		func(_ *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, runID string) {
			setCompactionPolicy(t, store, runID, compactionEnabled, clk.Now())
		})
}

// newCompactionFixtureWithPolicy is newCompactionFixture with the policy freeze
// left to the caller, so a test can exercise the REAL per-run opt-in
// (ApplySessionCompactionPolicy / ApplyContextPerCallWarnTokens) rather than
// writing a snapshot by hand. `configure` runs while the run is still pending,
// which is the same window the create route freezes policy in.
func newCompactionFixtureWithPolicy(
	t *testing.T,
	contextTokens int64,
	sender *fakeCompactingSender,
	configure func(c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, runID string),
) compactionFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	contextFacts := &fakeContextFacts{}
	if contextTokens > 0 {
		contextFacts.reading = domain.SessionContextReading{
			Observable: true, ProviderCalls: 118,
			LastContextTokens: contextTokens, PeakContextTokens: contextTokens,
		}
	}

	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts,
		SessionContextFacts: contextFacts,
		WorkspaceFacts:      workspaceFacts, ReviewRuns: reviewRuns,
		ReviewerLauncher: launcher, MessageSender: sender, Clock: clk.Now,
		NewID: func() string { idSeq++; return fmt.Sprintf("cid%d", idSeq) },
	})

	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	configure(c, store, clk, created.Run.ID)

	driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	return compactionFixture{c: c, store: store, clk: clk, sender: sender, facts: contextFacts, runID: created.Run.ID}
}

// setCompactionPolicy rewrites the run's frozen policy snapshot, the same
// durable fact production writes at create time.
func setCompactionPolicy(t *testing.T, store *fakeStore, runID string, enabled bool, now time.Time) {
	t.Helper()
	policy := domain.DefaultWorkflowPolicy()
	policy.SessionCompactionEnabled = enabled
	policy.Strategy = domain.ExecutionStrategySelection{Effective: domain.ExecutionStrategyTask}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := store.UpdateWorkflowRunPolicySnapshot(context.Background(), runID, string(snapshot), now); err != nil {
		t.Fatalf("UpdateWorkflowRunPolicySnapshot: %v", err)
	}
}

func fixLifecycleDecision(t *testing.T, store *fakeStore, runID string) (domain.SessionLifecycleDecision, bool) {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase != "session_lifecycle_decision" {
			continue
		}
		if d, _, ok := workflowcore.DecodeSessionLifecycleDecisionForTest(cp.RetryState); ok &&
			d.Role == domain.WorkflowRoleFixWorker {
			return d, true
		}
	}
	return domain.SessionLifecycleDecision{}, false
}

// A conversation measurably larger than the strategy profile expects a single
// call to be is real context pressure, and the 8M policy has always said that
// escalates REUSE to COMPACT. Before P7 nothing could set the field.
func TestContextPressureEscalatesFixLifecycleToCompact(t *testing.T) {
	sender := &fakeCompactingSender{supported: true}
	fx := newCompactionFixture(t, 196_853, true, sender)

	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok {
		t.Fatal("no fix_worker lifecycle decision recorded")
	}
	if decision.Action != domain.LifecycleCompact {
		t.Fatalf("action = %q, want compact at a context of 196,853 against a Task line of 150,000", decision.Action)
	}
	if !hasLifecycleReason(decision, domain.LifecycleReasonContextPressureActual) {
		t.Fatalf("reasons = %v, want context_pressure_actual", decision.Reasons)
	}
	if sender.compactCalls != 1 {
		t.Fatalf("compaction requests = %d, want 1", sender.compactCalls)
	}
	if !decision.CompactionRequested {
		t.Fatal("decision must record that the conversation was actually asked to compact")
	}
	if !strings.Contains(sender.compactFocus, "still unresolved") {
		t.Fatalf("focus = %q, want it to name what must survive", sender.compactFocus)
	}
}

// A conversation AO has not observed is not a small conversation. The policy
// must reuse and say WHY it could not tell.
func TestUnobservedContextDoesNotCompact(t *testing.T) {
	sender := &fakeCompactingSender{supported: true}
	fx := newCompactionFixture(t, 0, true, sender)

	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok {
		t.Fatal("no fix_worker lifecycle decision recorded")
	}
	if decision.Action != domain.LifecycleReuse {
		t.Fatalf("action = %q, want reuse on an unobserved session", decision.Action)
	}
	if !hasLifecycleReason(decision, domain.LifecycleReasonUnknownUsage) {
		t.Fatalf("reasons = %v, want unknown_usage recorded rather than silence", decision.Reasons)
	}
	if sender.compactCalls != 0 {
		t.Fatalf("compaction requests = %d, want none", sender.compactCalls)
	}
}

// The knob is off by default and off means off, however large the
// conversation is.
func TestCompactionIsNotRequestedWhenThePolicyIsOff(t *testing.T) {
	sender := &fakeCompactingSender{supported: true}
	fx := newCompactionFixture(t, 300_000, false, sender)

	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok {
		t.Fatal("no fix_worker lifecycle decision recorded")
	}
	if decision.Action != domain.LifecycleCompact {
		t.Fatalf("action = %q, want the DECISION to still be compact", decision.Action)
	}
	if sender.compactCalls != 0 {
		t.Fatalf("compaction requests = %d, want none while the policy is off", sender.compactCalls)
	}
	if decision.CompactionRequested {
		t.Fatal("audit must not claim a compaction that was never asked for")
	}
}

// THE SAFETY PROPERTY. A compaction AO could not perform must never become a
// fix cycle AO did not deliver -- for a harness with no compaction vocabulary
// and for a transport that refused alike.
func TestFixCycleIsDeliveredWhateverHappensToTheCompaction(t *testing.T) {
	cases := []struct {
		name   string
		sender *fakeCompactingSender
	}{
		{"harness has no compaction vocabulary", &fakeCompactingSender{supported: false}},
		{"transport refused the directive", &fakeCompactingSender{supported: true, compactErr: ports.ErrPromptUndelivered}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCompactionFixture(t, 300_000, true, tc.sender)
			if tc.sender.calls != 1 {
				t.Fatalf("fix prompt deliveries = %d, want exactly 1", tc.sender.calls)
			}
			if !strings.Contains(tc.sender.lastMsg, "SessionContextPack") {
				t.Fatalf("the fix prompt must still carry its fact pack; got %.120q", tc.sender.lastMsg)
			}
			decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
			if !ok {
				t.Fatal("no fix_worker lifecycle decision recorded")
			}
			if decision.CompactionRequested {
				t.Fatal("audit must not claim a compaction that did not happen")
			}
		})
	}
}

// A compaction record is written before the request is attempted, so a
// coordinator re-entering the same cycle after a restart finds it and does not
// send a second directive into a conversation that has already been replaced.
func TestCompactionIsRequestedAtMostOncePerCycle(t *testing.T) {
	sender := &fakeCompactingSender{supported: true}
	fx := newCompactionFixture(t, 250_000, true, sender)
	if sender.compactCalls != 1 {
		t.Fatalf("compaction requests = %d, want 1", sender.compactCalls)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := fx.c.GetRun(ctx, fx.runID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	if sender.compactCalls != 1 {
		t.Fatalf("compaction requests after repeated polling = %d, want still 1", sender.compactCalls)
	}
	if sender.calls != 1 {
		t.Fatalf("fix prompt deliveries after repeated polling = %d, want still 1", sender.calls)
	}
}

func hasLifecycleReason(d domain.SessionLifecycleDecision, want domain.SessionLifecycleReason) bool {
	for _, r := range d.Reasons {
		if r == want {
			return true
		}
	}
	return false
}

// A cycle marker must not be a prefix of another cycle's. Before the trailing
// comma, cycle 1's marker matched cycle 10's record -- so a run whose policy
// allowed ten repair cycles would have found cycle 10's record while looking
// for cycle 1's and skipped a compaction it had never performed.
func TestCompactionRecordMarkerIsNotAPrefixOfAnother(t *testing.T) {
	one := workflowcore.CompactionRecordMarkerForTest("wfs-1", 1)
	ten := workflowcore.CompactionRecordMarkerForTest("wfs-1", 10)
	if strings.Contains(ten, one) {
		t.Fatalf("cycle 1's marker %q is contained in cycle 10's %q", one, ten)
	}
	payloadTen := "{" + ten + `"session":"s","policyVersion":"v1"}`
	if strings.Contains(payloadTen, one) {
		t.Fatalf("cycle 1's marker %q matches a cycle 10 payload %q", one, payloadTen)
	}
}
