package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// usage_attribution_store_test.go — P3-E's exactly-once and role-attribution
// matrix, exercised against a real SQLite database rather than a fake.
//
// The invariant every test here defends is the same one: a token is counted
// ONCE, under ONE role, no matter how many times AO reads the artifact that
// reported it or how many times the daemon restarts.

const (
	attrRunID     = "wf-attr"
	attrProjectID = "usage"
)

func TestUsageAttribution_RoleWindowsSplitOneSessionByTime(t *testing.T) {
	// The central case, and the bug this whole mechanism exists to fix: AO
	// delivers a repair into the WORKER's own session, so one session, one
	// binding and one native root carry both the worker's base execution and
	// every repair cycle. Splitting them is a question of WHEN, not of which
	// session — and the answer must be exact, not a share.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		opened: base, harness: "claude-code", provider: "anthropic",
	})
	openWindow(t, s, window{
		key: "w-fix-1", session: string(sess.ID), role: domain.WorkflowRoleFixWorker,
		cycle: 1, opened: base.Add(30 * time.Minute), harness: "claude-code", provider: "anthropic",
	})

	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		// Two events inside the worker's window.
		attrEvent("e1", 1000, 200, base.Add(5*time.Minute)),
		attrEvent("e2", 2000, 300, base.Add(10*time.Minute)),
		// One after the repair window opened.
		attrEvent("e3", 400, 100, base.Add(40*time.Minute)),
	})

	byRole := roleTotals(ctx, t, s)
	if got := byRole[domain.WorkflowRoleWorker]; got.InputTokens != 3000 || got.OutputTokens != 500 {
		t.Fatalf("worker tokens = %+v, want input 3000 / output 500", got)
	}
	if got := byRole[domain.WorkflowRoleFixWorker]; got.InputTokens != 400 || got.OutputTokens != 100 {
		t.Fatalf("repair tokens = %+v, want input 400 / output 100", got)
	}
	// And the sum is the ledger, not a multiple of it: the pre-P3-E read model
	// asked per session once per step and reported 3400+3400 here.
	if total := byRole[domain.WorkflowRoleWorker].Add(byRole[domain.WorkflowRoleFixWorker]); total.Total() != 4000 {
		t.Fatalf("run total = %d, want 4000 (a shared session must not be counted twice)", total.Total())
	}
}

func TestUsageAttribution_DuplicateIngestAndRestartDoNotDoubleCount(t *testing.T) {
	// Two failures in one: a provider callback replayed within a session, and a
	// daemon restart that re-reads the same artifact from a stale cursor. Both
	// are the same defence — the event key is derived from the artifact, so the
	// second insert is a no-op rather than a second token.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700100000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w1", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})

	events := []domain.ModelUsageEvent{
		attrEvent("dup-1", 500, 50, base.Add(time.Minute)),
		attrEvent("dup-2", 700, 70, base.Add(2*time.Minute)),
	}
	applyEvents(t, s, source, base.Add(time.Hour), events)
	before := runTotal(ctx, t, s)

	// The duplicate callback: the same chunk, offered again.
	applyEventsAtOffset(t, s, source, base.Add(2*time.Hour), events, offsetOf(t, s, source))
	// The restart: the cursor is re-read from where it was and the same
	// records are parsed a third time.
	applyEventsAtOffset(t, s, source, base.Add(3*time.Hour), events, offsetOf(t, s, source))

	after := runTotal(ctx, t, s)
	if before.Total() != 1320 {
		t.Fatalf("first ingest total = %d, want 1320", before.Total())
	}
	if after.Total() != before.Total() {
		t.Fatalf("total after duplicate callback + restart = %d, want %d unchanged", after.Total(), before.Total())
	}
	if after.EventCount != 2 {
		t.Fatalf("event count = %d, want 2", after.EventCount)
	}
}

func TestUsageAttribution_WindowOpenIsIdempotentAcrossReplay(t *testing.T) {
	// A dispatch is replayed by failover, by resume after a restart, and by a
	// wake. Each replay re-derives the same durable obligation, so it must
	// re-open the SAME window — not a second one that splits the role's tokens.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700200000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	for i := 0; i < 3; i++ {
		openWindow(t, s, window{
			key: "w-same", session: string(sess.ID), role: domain.WorkflowRoleWorker,
			// A later replay carries a later clock; the key is what decides.
			opened: base.Add(time.Duration(i) * time.Hour),
		})
	}
	windows, err := s.ListUsageAttributionWindowsForRun(ctx, attrRunID)
	mustNoError(t, err)
	if len(windows) != 1 {
		t.Fatalf("windows = %d, want 1 — a replayed dispatch must not open a second window", len(windows))
	}
	if !windows[0].OpenedAt.Equal(base) {
		t.Fatalf("openedAt = %s, want the FIRST opening %s — a replay must not move the boundary", windows[0].OpenedAt, base)
	}

	applyEvents(t, s, source, base.Add(4*time.Hour), []domain.ModelUsageEvent{
		attrEvent("k1", 100, 10, base.Add(30*time.Minute)),
	})
	if total := runTotal(ctx, t, s); total.Total() != 110 {
		t.Fatalf("total = %d, want 110", total.Total())
	}
}

func TestUsageAttribution_FailedProviderAttemptKeepsItsTokens(t *testing.T) {
	// A failover: the preferred provider burned tokens and then failed, and the
	// fallback did the work. Both attempts' spend is real money and both stay
	// on the ledger — P3-E §18. They are kept apart by attempt ordinal.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700300000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-hop1", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		attemptID: "att-1", ordinal: 1, opened: base, provider: "openai", harness: "codex",
	})
	openWindow(t, s, window{
		key: "w-hop2", session: string(sess.ID), role: domain.WorkflowRoleWorker,
		attemptID: "att-2", ordinal: 2, opened: base.Add(20 * time.Minute),
		provider: "anthropic", harness: "claude-code",
	})
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("f1", 900, 90, base.Add(5*time.Minute)),
		attrEvent("f2", 1500, 150, base.Add(30*time.Minute)),
	})

	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	byAttempt := map[string]int64{}
	for _, l := range lines {
		byAttempt[l.AttemptID] += l.Tokens.Total()
	}
	if byAttempt["att-1"] != 990 {
		t.Fatalf("failed attempt tokens = %d, want 990 — a failed attempt still spent them", byAttempt["att-1"])
	}
	if byAttempt["att-2"] != 1650 {
		t.Fatalf("successor attempt tokens = %d, want 1650", byAttempt["att-2"])
	}
}

func TestUsageAttribution_EventWithNoProviderTimestampIsApproximateNotDropped(t *testing.T) {
	// A legacy row, or an artifact whose records carry no timestamp. Its tokens
	// are real and must be in the total; only the ROLE split is uncertain, and
	// the ledger says so rather than guessing or discarding.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700400000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w-a", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})
	openWindow(t, s, window{
		key: "w-b", session: string(sess.ID), role: domain.WorkflowRoleFixWorker,
		cycle: 1, opened: base.Add(time.Hour),
	})

	untimed := attrEvent("no-ts", 800, 80, time.Time{})
	untimed.ObservedAt = nil
	applyEvents(t, s, source, base.Add(2*time.Hour), []domain.ModelUsageEvent{untimed})

	total := runTotal(ctx, t, s)
	if total.Total() != 880 {
		t.Fatalf("total = %d, want 880 — an untimed event is still spend", total.Total())
	}
	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	if lines[0].Role != domain.WorkflowRoleWorker {
		t.Fatalf("fallback role = %s, want the session's FIRST window (worker)", lines[0].Role)
	}
	if lines[0].ApproximateEvents != 1 {
		t.Fatalf("approximateEvents = %d, want 1 — an inferred attribution must be labeled one", lines[0].ApproximateEvents)
	}
}

func TestUsageAttribution_UnreportedRoleIsUnobservableNotZero(t *testing.T) {
	// A reviewer pane CAN be metered now — it binds under its own subject. What
	// this asserts is the remaining honest gap: a pane that has not yet reported
	// a provider conversation has no binding, and the read model must call that
	// unknown rather than zero. "This review cost nothing" and "AO has not been
	// told what this review cost" are different claims.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700500000, 0).UTC()
	sess, _ := seedAttributionSession(t, s, base)

	openWindow(t, s, window{key: "w-worker", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})
	openWindow(t, s, window{
		key: "w-review-silent", subjectKind: domain.UsageSubjectRuntimePane, session: "rr-silent",
		role: domain.WorkflowRoleReviewer, opened: base.Add(time.Hour), harness: "codex", provider: "openai",
	})
	openWindow(t, s, window{
		key: "w-review-reported", subjectKind: domain.UsageSubjectRuntimePane, session: "rr-reported",
		role: domain.WorkflowRoleReviewer, opened: base.Add(2 * time.Hour), harness: "codex", provider: "openai",
	})
	// Only the second reviewer's pane reported a conversation.
	seedPaneSource(t, s, base, domain.RuntimePaneSubject("rr-reported"), domain.HarnessCodex, "reported-thread")

	windows, err := s.ListUsageAttributionWindowsForRun(ctx, attrRunID)
	mustNoError(t, err)
	byKey := map[string]domain.UsageAttributionWindow{}
	for _, w := range windows {
		byKey[w.DedupeKey] = w
	}
	if !byKey["w-worker"].HasUsageBinding {
		t.Fatal("the worker's session has a binding and must report one")
	}
	if byKey["w-review-silent"].HasUsageBinding {
		t.Fatal("a pane that reported nothing has no binding; claiming one would let its absent tokens read as zero")
	}
	if !byKey["w-review-reported"].HasUsageBinding {
		t.Fatal("a reviewer pane that DID report must be metered — that is the whole point of the subject model")
	}
}

func TestUsageAttribution_ParentFamilyFoldsChildren(t *testing.T) {
	// P3-E §16: a parent's budget must be measured against the whole family, or
	// ten children at 100k each run happily under a 200k parent.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700600000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)

	openWindow(t, s, window{
		key: "w-parent", session: string(sess.ID), role: domain.WorkflowRolePlanner, opened: base,
	})
	childSess, childSource := seedNamedAttributionSession(t, s, base, "child")
	openWindow(t, s, window{
		key: "w-child", session: string(childSess.ID), role: domain.WorkflowRoleWorker,
		run: "wf-child", parent: attrRunID, opened: base.Add(time.Minute),
	})

	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("p1", 100, 10, base.Add(10*time.Second)),
	})
	applyEvents(t, s, childSource, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("c1", 5000, 500, base.Add(5*time.Minute)),
	})

	family, err := s.AggregateRunFamilyUsage(ctx, attrRunID)
	mustNoError(t, err)
	var total int64
	for _, l := range family {
		total += l.Tokens.Total()
	}
	if total != 5610 {
		t.Fatalf("family total = %d, want 5610 (110 parent + 5500 child)", total)
	}

	// And the parent's OWN ledger stays its own: folding the child into both
	// would double the family.
	own := runTotal(ctx, t, s)
	if own.Total() != 110 {
		t.Fatalf("parent's own total = %d, want 110", own.Total())
	}
}

func TestUsageAttribution_CacheDimensionsSurvive(t *testing.T) {
	// Cache reads and writes are billed at different rates from fresh input, so
	// they have to reach the ledger as their own dimensions rather than being
	// folded away. AO captures what the provider reported and infers nothing.
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700700000, 0).UTC()
	sess, source := seedAttributionSession(t, s, base)
	openWindow(t, s, window{key: "w", session: string(sess.ID), role: domain.WorkflowRoleWorker, opened: base})

	ev := domain.ModelUsageEvent{
		ModelID:        "claude-opus-5",
		SourceEventKey: "cache-1",
		Tokens: domain.UsageTokenMetrics{
			InputTokens: 10_000, UncachedInputTokens: 1_000,
			CacheReadTokens: 7_000, CacheWriteTokens: 2_000, OutputTokens: 500,
		},
		ObservedAt: timePtrUTC(base.Add(time.Minute)),
	}
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{ev})

	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	got := lines[0].Tokens
	if got.CacheReadTokens != 7000 || got.CacheWriteTokens != 2000 || got.UncachedInputTokens != 1000 {
		t.Fatalf("cache dimensions = %+v, want read 7000 / write 2000 / uncached 1000", got)
	}
}

func TestUsageAttribution_LegacyRunWithNoWindowsReportsNothing(t *testing.T) {
	// A run created before P3-E has no windows, so it has no attributable
	// usage. The correct answer is an empty ledger the read model renders as
	// "no usage data recorded" — never a zero that reads as "this was free".
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1700800000, 0).UTC()
	_, source := seedAttributionSession(t, s, base)
	applyEvents(t, s, source, base.Add(time.Hour), []domain.ModelUsageEvent{
		attrEvent("legacy-1", 1234, 56, base.Add(time.Minute)),
	})

	lines, err := s.AggregateWorkflowRunUsage(ctx, "wf-never-existed")
	mustNoError(t, err)
	if len(lines) != 0 {
		t.Fatalf("legacy run lines = %d, want 0", len(lines))
	}
	windows, err := s.ListUsageAttributionWindowsForRun(ctx, "wf-never-existed")
	mustNoError(t, err)
	if len(windows) != 0 {
		t.Fatalf("legacy run windows = %d, want 0", len(windows))
	}
}

// --- helpers --------------------------------------------------------------

type window struct {
	key         string
	subjectKind domain.UsageSubjectKind
	session     string
	run         string
	parent      string
	role        domain.WorkflowRole
	attemptID   string
	ordinal     int64
	cycle       int64
	harness     string
	provider    string
	opened      time.Time
}

func openWindow(t *testing.T, s *sqlite.Store, w window) {
	t.Helper()
	runID := w.run
	if runID == "" {
		runID = attrRunID
	}
	kind := w.subjectKind
	if kind == "" {
		kind = domain.UsageSubjectSession
	}
	err := s.OpenUsageAttributionWindow(context.Background(), domain.UsageAttributionWindow{
		DedupeKey: w.key, SubjectKind: kind, SessionID: w.session, ProjectID: attrProjectID,
		WorkflowRunID: runID, ParentWorkflowRunID: w.parent,
		AttemptID: w.attemptID, AttemptOrdinal: w.ordinal, Cycle: w.cycle,
		Role: w.role, Harness: w.harness, Provider: w.provider,
		OpenedAt: w.opened, CreatedAt: w.opened,
	})
	mustNoError(t, err)
}

func seedAttributionSession(t *testing.T, s *sqlite.Store, now time.Time) (domain.SessionRecord, domain.UsageSourceRecord) {
	t.Helper()
	return seedNamedAttributionSession(t, s, now, "root")
}

func seedNamedAttributionSession(t *testing.T, s *sqlite.Store, now time.Time, name string) (domain.SessionRecord, domain.UsageSourceRecord) {
	t.Helper()
	seedProject(t, s, attrProjectID)
	rec := sampleRecord(attrProjectID)
	rec.Harness = domain.HarnessClaudeCode
	sess, err := s.CreateSession(context.Background(), rec)
	mustNoError(t, err, "create attribution session")
	binding := mustUpsertUsageBinding(t, s, sess, now, domain.UsageBindingRecord{
		NativeRootID: name + "-thread", InitialModelID: "claude-opus-5",
		State: domain.UsageBindingActive,
	})
	source := mustInsertUsageSource(t, s, now, domain.UsageSourceRecord{
		BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain,
		NativeSessionID: name + "-thread",
		ArtifactPath:    "/tmp/claude/" + name + ".jsonl",
		FileIdentity:    "dev:ino:" + name, State: domain.UsageSourcePending,
	})
	return sess, source
}

func attrEvent(key string, input, output int64, observed time.Time) domain.ModelUsageEvent {
	ev := domain.ModelUsageEvent{
		ModelID:        "claude-opus-5",
		SourceEventKey: key,
		Tokens: domain.UsageTokenMetrics{
			InputTokens: input, UncachedInputTokens: input, OutputTokens: output,
		},
	}
	if !observed.IsZero() {
		ev.ObservedAt = timePtrUTC(observed)
	}
	return ev
}

func timePtrUTC(t time.Time) *time.Time {
	utc := t.UTC()
	return &utc
}

func applyEvents(t *testing.T, s *sqlite.Store, source domain.UsageSourceRecord, at time.Time, events []domain.ModelUsageEvent) {
	t.Helper()
	applyEventsAtOffset(t, s, source, at, events, 0)
}

func applyEventsAtOffset(t *testing.T, s *sqlite.Store, source domain.UsageSourceRecord, at time.Time, events []domain.ModelUsageEvent, offset int64) {
	t.Helper()
	current := currentUsageSource(t, s, source)
	err := s.ApplyUsageChunk(context.Background(), source.ID, offset, current.UpdatedAt, domain.SourceCursorState{
		ByteOffset: offset + int64(len(events)),
		State:      domain.UsageSourceActive,
		UpdatedAt:  at,
	}, events)
	mustNoError(t, err, "apply usage chunk")
}

func currentUsageSource(t *testing.T, s *sqlite.Store, source domain.UsageSourceRecord) domain.UsageSourceRecord {
	t.Helper()
	sources, err := s.ListUsageSourcesForBinding(context.Background(), source.BindingID)
	mustNoError(t, err)
	for _, candidate := range sources {
		if candidate.ID == source.ID {
			return candidate
		}
	}
	t.Fatalf("usage source %d disappeared", source.ID)
	return domain.UsageSourceRecord{}
}

func offsetOf(t *testing.T, s *sqlite.Store, source domain.UsageSourceRecord) int64 {
	t.Helper()
	return currentUsageSource(t, s, source).ByteOffset
}

func roleTotals(ctx context.Context, t *testing.T, s *sqlite.Store) map[domain.WorkflowRole]domain.UsageTokenTotals {
	t.Helper()
	lines, err := s.AggregateWorkflowRunUsage(ctx, attrRunID)
	mustNoError(t, err)
	out := map[domain.WorkflowRole]domain.UsageTokenTotals{}
	for _, l := range lines {
		out[l.Role] = out[l.Role].Add(l.Tokens)
	}
	return out
}

func runTotal(ctx context.Context, t *testing.T, s *sqlite.Store) domain.UsageTokenTotals {
	t.Helper()
	var total domain.UsageTokenTotals
	for _, tokens := range roleTotals(ctx, t, s) {
		total = total.Add(tokens)
	}
	return total
}
