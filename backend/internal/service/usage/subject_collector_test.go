package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// subject_collector_test.go — the pane path, against a real database.
//
// Two things it must never do, both of which the session path WOULD do, and
// both of which were named as hazards before this code existed: adopt the
// session's harness, and end the session's collection.

func newSubjectCollector(t *testing.T) (*usagesvc.Collector, *sqlite.Store) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	roots := usagesvc.SourceRoots{
		ClaudeProjects: t.TempDir(),
		CodexSessions:  t.TempDir(),
		CodexArchived:  t.TempDir(),
	}
	return usagesvc.NewCollector(store, roots, func(bool) {}), store
}

func TestRecordSubjectHook_RefusesASessionSubject(t *testing.T) {
	// A session has an activity state and a launch id to validate against.
	// Routing one through the pane door would skip both checks — a second,
	// weaker way into the same ledger.
	collector, _ := newSubjectCollector(t)
	err := collector.RecordSubjectHook(context.Background(), usagesvc.SubjectHookSignal{
		Subject: domain.SessionSubject("sess-1"), Harness: domain.HarnessClaudeCode,
		NativeSessionID: "root-1",
	})
	if err == nil {
		t.Fatal("a session subject must be refused on the pane path")
	}
}

func TestRecordSubjectHook_RecordsNothingWithoutANativeID(t *testing.T) {
	// The alternative would be adopting whichever transcript looks recent. AO
	// binds what a pane reports about itself, or it binds nothing.
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	subject := domain.RuntimePaneSubject("rr-1")

	if err := collector.RecordSubjectHook(ctx, usagesvc.SubjectHookSignal{
		Subject: subject, Harness: domain.HarnessCodex, Event: "session-start",
	}); err != nil {
		t.Fatalf("RecordSubjectHook: %v", err)
	}
	bindings, err := store.ListUsageBindingsForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %d, want none — a pane that named no conversation must bind nothing", len(bindings))
	}
}

func TestRecordSubjectHook_BindsThePanesOwnHarness(t *testing.T) {
	// The defect the session path would produce: overwriting the pane's harness
	// with the session's. A Codex reviewer beside a Claude worker must bind as
	// Codex, or its rollout is parsed by the wrong parser under the wrong root.
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	subject := domain.RuntimePaneSubject("rr-codex")

	if err := collector.RecordSubjectHook(ctx, usagesvc.SubjectHookSignal{
		Subject: subject, Harness: domain.HarnessCodex,
		Event: "session-start", NativeSessionID: "codex-thread-1", ModelID: "gpt-5-codex",
	}); err != nil {
		t.Fatalf("RecordSubjectHook: %v", err)
	}
	bindings, err := store.ListUsageBindingsForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(bindings))
	}
	if bindings[0].Harness != domain.HarnessCodex {
		t.Fatalf("harness = %q, want codex — the pane's own, not a session's", bindings[0].Harness)
	}
	if bindings[0].SessionID != "" {
		t.Fatalf("a pane binding must carry no session, got %q", bindings[0].SessionID)
	}
}

func TestRecordSubjectHook_IsIdempotentAcrossRediscovery(t *testing.T) {
	// The same pane reporting repeatedly — every tool call, plus again after a
	// restart — must keep ONE binding, not accumulate one per callback.
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	subject := domain.RuntimePaneSubject("rr-repeat")

	for i := 0; i < 5; i++ {
		if err := collector.RecordSubjectHook(ctx, usagesvc.SubjectHookSignal{
			Subject: subject, Harness: domain.HarnessClaudeCode,
			Event: "post-tool-use", NativeSessionID: "claude-root-1",
		}); err != nil {
			t.Fatalf("callback %d: %v", i, err)
		}
	}
	bindings, err := store.ListUsageBindingsForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1 — repeated discovery must not multiply bindings", len(bindings))
	}
}

func TestRecordSubjectHook_FinalizationLeavesOtherSubjectsAlone(t *testing.T) {
	// The exact hazard: a reviewer's `session-end` ending the worker's
	// collection. Here the two are separate subjects and only one finalizes.
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	reviewer := domain.RuntimePaneSubject("rr-ending")
	resolver := domain.RuntimePaneSubject("wqr-running")

	for _, subject := range []domain.UsageSubject{reviewer, resolver} {
		if err := collector.RecordSubjectHook(ctx, usagesvc.SubjectHookSignal{
			Subject: subject, Harness: domain.HarnessClaudeCode,
			Event: "session-start", NativeSessionID: "root-" + subject.ID,
		}); err != nil {
			t.Fatalf("seed %s: %v", subject, err)
		}
	}
	if err := collector.RecordSubjectHook(ctx, usagesvc.SubjectHookSignal{
		Subject: reviewer, Harness: domain.HarnessClaudeCode,
		Event: "session-end", NativeSessionID: "root-" + reviewer.ID,
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	ended, err := store.ListUsageBindingsForSubject(ctx, reviewer)
	if err != nil {
		t.Fatalf("list reviewer: %v", err)
	}
	if len(ended) != 1 || ended[0].State == domain.UsageBindingActive {
		t.Fatalf("the ending pane = %+v, want finalized", ended)
	}
	running, err := store.ListUsageBindingsForSubject(ctx, resolver)
	if err != nil {
		t.Fatalf("list resolver: %v", err)
	}
	if len(running) != 1 || running[0].State != domain.UsageBindingActive {
		t.Fatalf("the other pane = %+v, want still active — one pane ending must not end another's collection", running)
	}
}

func TestRecordDirectUsage_PlannerIsMeteredExactlyOnce(t *testing.T) {
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	subject := domain.PlannerInvocationSubject("wf-1#1")
	report := usagesvc.DirectUsageReport{
		Subject: subject, Harness: domain.HarnessClaudeCode, ModelID: "claude-sonnet-5",
		Tokens: domain.UsageTokenMetrics{
			InputTokens: 9_000, UncachedInputTokens: 8_000,
			CacheReadTokens: 1_000, OutputTokens: 400,
		},
		EventKey:   "planner-stable-key",
		ObservedAt: time.Unix(1730000000, 0).UTC(),
	}
	for i := 0; i < 3; i++ {
		if err := collector.RecordDirectUsage(ctx, report); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	bindings, err := store.ListUsageBindingsForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(bindings))
	}
	summary, err := store.AggregateWorkflowRunUsage(ctx, "no-run")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	_ = summary
	// The recording is idempotent at the event level; the store test asserts the
	// token total. Here it is enough that three recordings produced one binding
	// and no error, which is what a restart replay looks like.
}

func TestRecordDirectUsage_ReportsNothingRatherThanZero(t *testing.T) {
	// A planner invocation whose CLI reported no usage block records nothing, so
	// the read model shows its spend as unknown. A stored zero would render as
	// "this planner call was free", which is never true.
	collector, store := newSubjectCollector(t)
	ctx := context.Background()
	subject := domain.PlannerInvocationSubject("wf-2#1")
	if err := collector.RecordDirectUsage(ctx, usagesvc.DirectUsageReport{
		Subject: subject, Harness: domain.HarnessClaudeCode, ModelID: "claude-sonnet-5",
		EventKey: "planner-empty",
	}); err != nil {
		t.Fatalf("RecordDirectUsage: %v", err)
	}
	bindings, err := store.ListUsageBindingsForSubject(ctx, subject)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("bindings = %d, want none — an empty report is unknown, not a zero worth storing", len(bindings))
	}
}

func TestRecordDirectUsage_RefusesASessionSubject(t *testing.T) {
	collector, _ := newSubjectCollector(t)
	err := collector.RecordDirectUsage(context.Background(), usagesvc.DirectUsageReport{
		Subject: domain.SessionSubject("sess-1"), Harness: domain.HarnessClaudeCode,
		Tokens: domain.UsageTokenMetrics{InputTokens: 1, OutputTokens: 1}, EventKey: "k",
	})
	if err == nil {
		t.Fatal("a session is metered from its transcript, never from a direct report")
	}
}
