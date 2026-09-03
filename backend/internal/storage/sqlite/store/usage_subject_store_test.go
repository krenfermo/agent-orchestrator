package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// usage_subject_store_test.go — P3-E completion: every provider-backed role can
// be metered, and none of them can steal another's tokens.
//
// The invariant under test throughout: a SUBJECT owns its usage. A pane is not a
// session, a session is not a pane, and two panes for the same step are two
// subjects.

func TestUsageSubject_PaneBindsWithoutASession(t *testing.T) {
	// The whole gap this pass closes. A reviewer pane is not a row in
	// `sessions`; before this it could carry no binding at all, so every token
	// it spent was invisible.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1720000000, 0).UTC()

	binding, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject:      domain.RuntimePaneSubject("review-run-1"),
		Harness:      domain.HarnessCodex,
		NativeRootID: "codex-thread-1",
		State:        domain.UsageBindingActive,
		UpdatedAt:    now,
	})
	mustNoError(t, err)
	if binding.SessionID != "" {
		t.Fatalf("a pane binding must carry no session, got %q", binding.SessionID)
	}
	if binding.Subject != domain.RuntimePaneSubject("review-run-1") {
		t.Fatalf("subject = %+v", binding.Subject)
	}

	again, ok, err := s.GetUsageBindingBySubject(ctx, domain.RuntimePaneSubject("review-run-1"), domain.HarnessCodex, "codex-thread-1")
	mustNoError(t, err)
	if !ok || again.ID != binding.ID {
		t.Fatalf("re-reading the pane's binding gave %+v (ok=%v), want id %d", again, ok, binding.ID)
	}
}

func TestUsageSubject_SameIDDifferentKindsAreDifferentSubjects(t *testing.T) {
	// A pane whose authority id happens to equal a session id must not inherit
	// that session's ledger. The kind is part of the identity.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1720100000, 0).UTC()
	sess := seedUsageSession(t, s, domain.HarnessClaudeCode)

	sessionBinding, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: domain.SessionSubject(sess.ID), Harness: domain.HarnessClaudeCode,
		NativeRootID: "root-a", State: domain.UsageBindingActive, UpdatedAt: now,
	})
	mustNoError(t, err)
	paneBinding, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: domain.RuntimePaneSubject(string(sess.ID)), Harness: domain.HarnessClaudeCode,
		NativeRootID: "root-a", State: domain.UsageBindingActive, UpdatedAt: now,
	})
	mustNoError(t, err)
	if sessionBinding.ID == paneBinding.ID {
		t.Fatal("a pane and a session sharing an id string collapsed into one binding")
	}
	if paneBinding.SessionID != "" {
		t.Fatalf("the pane binding must not have adopted the session, got %q", paneBinding.SessionID)
	}
}

func TestUsageSubject_PaneFinalizationLeavesTheWorkerAlone(t *testing.T) {
	// The hazard this design exists to prevent: a reviewer's own `session-end`
	// finalizing the WORKER's bindings, cutting the worker's ingestion short
	// mid-run.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1720200000, 0).UTC()
	sess := seedUsageSession(t, s, domain.HarnessClaudeCode)

	worker, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: domain.SessionSubject(sess.ID), Harness: domain.HarnessClaudeCode,
		NativeRootID: "worker-root", State: domain.UsageBindingActive, UpdatedAt: now,
	})
	mustNoError(t, err)
	reviewer, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: domain.RuntimePaneSubject("review-run-9"), Harness: domain.HarnessCodex,
		NativeRootID: "reviewer-root", State: domain.UsageBindingActive, UpdatedAt: now,
	})
	mustNoError(t, err)

	n, err := s.FinalizeUsageBindingsForSubject(ctx, domain.RuntimePaneSubject("review-run-9"), now.Add(time.Minute))
	mustNoError(t, err)
	if n != 1 {
		t.Fatalf("finalized %d bindings, want exactly the reviewer's own", n)
	}

	stillWorking, ok, err := s.GetUsageBindingBySubject(ctx, domain.SessionSubject(sess.ID), domain.HarnessClaudeCode, "worker-root")
	mustNoError(t, err)
	if !ok || stillWorking.State != domain.UsageBindingActive {
		t.Fatalf("the worker's binding = %+v, want still active — a reviewer ending must not end the worker's collection", stillWorking)
	}
	finalized, _, err := s.GetUsageBindingBySubject(ctx, domain.RuntimePaneSubject("review-run-9"), domain.HarnessCodex, "reviewer-root")
	mustNoError(t, err)
	if finalized.State != domain.UsageBindingFinalizing {
		t.Fatalf("the reviewer's own binding = %q, want finalizing", finalized.State)
	}
	_ = worker
	_ = reviewer
}

func TestUsageSubject_LegacySessionBindingsStillWork(t *testing.T) {
	// Every binding that existed before subjects is a session subject, and the
	// session-shaped calls must keep returning exactly what they returned.
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Unix(1720300000, 0).UTC()
	sess := seedUsageSession(t, s, domain.HarnessClaudeCode)

	created := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
		NativeRootID: "legacy-root", State: domain.UsageBindingActive,
	})
	if created.Subject.Kind != domain.UsageSubjectSession {
		t.Fatalf("a record naming only a session must bind as a session subject, got %+v", created.Subject)
	}
	if created.SessionID != sess.ID {
		t.Fatalf("session id = %q, want %q", created.SessionID, sess.ID)
	}
	got, ok, err := s.GetUsageBinding(ctx, sess.ID, domain.HarnessClaudeCode, "legacy-root")
	mustNoError(t, err)
	if !ok || got.ID != created.ID {
		t.Fatalf("the session-shaped lookup must still find it, got %+v ok=%v", got, ok)
	}
	listed, err := s.ListUsageBindingsForSession(ctx, sess.ID)
	mustNoError(t, err)
	if len(listed) != 1 {
		t.Fatalf("session listing returned %d, want 1", len(listed))
	}
}

func TestUsageSubject_DirectUsageIsRecordedOnceAndAttributed(t *testing.T) {
	// The planner: no transcript, one response-reported fact, exactly once.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1720400000, 0).UTC()
	seedProject(t, s, attrProjectID)

	subject := domain.PlannerInvocationSubject("wf-plan#1")
	openWindow(t, s, window{
		key: "w-planner", subjectKind: domain.UsageSubjectPlannerInvocation, session: subject.ID,
		role: domain.WorkflowRolePlanner, opened: base, provider: "anthropic", harness: "claude-code",
	})
	binding, err := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
		Subject: subject, Harness: domain.HarnessClaudeCode, NativeRootID: subject.ID,
		InitialModelID: "claude-sonnet-5", State: domain.UsageBindingComplete, UpdatedAt: base,
	})
	mustNoError(t, err)

	ev := domain.ModelUsageEvent{
		ModelID:        "claude-sonnet-5",
		SourceEventKey: "planner-abc",
		Tokens: domain.UsageTokenMetrics{
			InputTokens: 12_000, UncachedInputTokens: 9_000,
			CacheReadTokens: 2_000, CacheWriteTokens: 1_000, OutputTokens: 800,
		},
		ObservedAt: timePtrUTC(base.Add(time.Minute)),
	}
	mustNoError(t, s.RecordDirectUsageEvent(ctx, binding.ID, ev, base))
	// Replayed after a restart: the same invocation re-derives the same key.
	mustNoError(t, s.RecordDirectUsageEvent(ctx, binding.ID, ev, base.Add(time.Hour)))

	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	if lines[0].Role != domain.WorkflowRolePlanner {
		t.Fatalf("role = %s, want planner", lines[0].Role)
	}
	if lines[0].Tokens.Total() != 12_800 || lines[0].Tokens.EventCount != 1 {
		t.Fatalf("tokens = %+v, want 12800 across ONE event — a replay must not charge twice", lines[0].Tokens)
	}
}

func TestUsageSubject_CrossProviderRolesEachKeepTheirOwnTokens(t *testing.T) {
	// The case P3-E's completion bar names: a Claude worker, a Codex resolver
	// and a Codex reviewer on one run. Each must appear separately, and the run
	// total must be the sum of unique events — not the worker's alone, and not
	// anything counted twice.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1720500000, 0).UTC()
	sess, workerSource := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		opened: base, harness: "claude-code", provider: "anthropic",
	})
	applyEvents(t, s, workerSource, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("w1", 5_000, 500, base.Add(5*time.Minute)),
	})

	resolverSource := seedPaneSource(t, s, base, domain.RuntimePaneSubject("wqr-1"), domain.HarnessCodex, "codex-resolver")
	openWindow(t, s, window{
		key: "w-resolver", subjectKind: domain.UsageSubjectRuntimePane, session: "wqr-1",
		role: domain.WorkflowRoleDecisionResolver, opened: base.Add(10 * time.Minute),
		harness: "codex", provider: "openai",
	})
	applyEvents(t, s, resolverSource, base.Add(time.Hour), []domain.ModelUsageEvent{
		codexEvent("r1", 3_000, 300, base.Add(12*time.Minute)),
	})

	reviewerSource := seedPaneSource(t, s, base, domain.RuntimePaneSubject("rr-1"), domain.HarnessCodex, "codex-reviewer")
	openWindow(t, s, window{
		key: "w-reviewer", subjectKind: domain.UsageSubjectRuntimePane, session: "rr-1",
		role: domain.WorkflowRoleReviewer, opened: base.Add(20 * time.Minute),
		harness: "codex", provider: "openai",
	})
	applyEvents(t, s, reviewerSource, base.Add(time.Hour), []domain.ModelUsageEvent{
		codexEvent("v1", 7_000, 700, base.Add(25*time.Minute)),
	})

	byRole := roleTotals(ctx, t, s)
	for role, want := range map[domain.WorkflowRole]int64{
		domain.WorkflowRoleWorker:           5_500,
		domain.WorkflowRoleDecisionResolver: 3_300,
		domain.WorkflowRoleReviewer:         7_700,
	} {
		if got := byRole[role].Total(); got != want {
			t.Fatalf("%s = %d, want %d", role, got, want)
		}
	}
	if total := runTotal(ctx, t, s).Total(); total != 16_500 {
		t.Fatalf("run total = %d, want 16500 (5500 + 3300 + 7700)", total)
	}
}

func TestUsageSubject_ASecondPaneDoesNotInheritTheFirstsTokens(t *testing.T) {
	// A cancelled reviewer and its replacement are two review runs, therefore
	// two subjects. The first one's spend stays the first one's — and it still
	// counts, because a cancelled reviewer burned real tokens.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1720600000, 0).UTC()
	seedProject(t, s, attrProjectID)

	first := seedPaneSource(t, s, base, domain.RuntimePaneSubject("rr-first"), domain.HarnessCodex, "thread-first")
	openWindow(t, s, window{
		key: "w-r1", subjectKind: domain.UsageSubjectRuntimePane, session: "rr-first",
		role: domain.WorkflowRoleReviewer, attemptID: "rr-first", opened: base, harness: "codex",
	})
	applyEvents(t, s, first, base.Add(time.Hour), []domain.ModelUsageEvent{
		codexEvent("f1", 2_000, 100, base.Add(time.Minute)),
	})

	second := seedPaneSource(t, s, base, domain.RuntimePaneSubject("rr-second"), domain.HarnessCodex, "thread-second")
	openWindow(t, s, window{
		key: "w-r2", subjectKind: domain.UsageSubjectRuntimePane, session: "rr-second",
		role: domain.WorkflowRoleReviewer, attemptID: "rr-second",
		opened: base.Add(30 * time.Minute), harness: "codex",
	})
	applyEvents(t, s, second, base.Add(2*time.Hour), []domain.ModelUsageEvent{
		codexEvent("s1", 4_000, 200, base.Add(35*time.Minute)),
	})

	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	byAttempt := map[string]int64{}
	for _, l := range lines {
		byAttempt[l.AttemptID] += l.Tokens.Total()
	}
	if byAttempt["rr-first"] != 2_100 {
		t.Fatalf("the cancelled reviewer = %d, want 2100 — its tokens were spent and still count", byAttempt["rr-first"])
	}
	if byAttempt["rr-second"] != 4_200 {
		t.Fatalf("the replacement reviewer = %d, want 4200", byAttempt["rr-second"])
	}
}

func TestUsageSubject_NoOrphanSubjectIsAttributed(t *testing.T) {
	// A binding whose subject has no window at all contributes to no run. It
	// must not be swept into an unrelated one.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1720700000, 0).UTC()
	seedProject(t, s, attrProjectID)

	orphan := seedPaneSource(t, s, base, domain.RuntimePaneSubject("nobody"), domain.HarnessCodex, "orphan-thread")
	applyEvents(t, s, orphan, base.Add(time.Hour), []domain.ModelUsageEvent{
		codexEvent("o1", 9_999, 999, base.Add(time.Minute)),
	})

	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	if len(lines) != 0 {
		t.Fatalf("an unwindowed subject was attributed to a run: %+v", lines)
	}
}

// seedPaneSource creates a pane-subject binding plus its transcript source.
func seedPaneSource(
	t *testing.T,
	s *sqlite.Store,
	now time.Time,
	subject domain.UsageSubject,
	harness domain.AgentHarness,
	name string,
) domain.UsageSourceRecord {
	t.Helper()
	binding, err := s.UpsertUsageBinding(context.Background(), domain.UsageBindingRecord{
		Subject: subject, Harness: harness, NativeRootID: name,
		State: domain.UsageBindingActive, UpdatedAt: now,
	})
	mustNoError(t, err)
	kind := domain.UsageSourceClaudeMain
	if harness == domain.HarnessCodex {
		kind = domain.UsageSourceCodexRollout
	}
	return mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
		BindingID: binding.ID, Kind: kind, NativeSessionID: name,
		ArtifactPath: "/tmp/pane/" + name + ".jsonl", FileIdentity: "dev:ino:" + name,
		State: domain.UsageSourcePending,
	})
}

func codexEvent(key string, input, output int64, observed time.Time) domain.ModelUsageEvent {
	ev := attrEvent(key, input, output, observed)
	ev.ModelID = "gpt-5-codex"
	return ev
}
