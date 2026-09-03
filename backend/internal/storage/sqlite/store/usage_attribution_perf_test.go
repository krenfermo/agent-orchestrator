package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// usage_attribution_perf_test.go — P3-E §32.
//
// The ledger is append-only and never pruned (usage is audit/provenance), so it
// grows without bound while the queries over it sit on a run detail page and a
// board. This test exists to make that growth visible before a user finds it:
// it loads a real database with 10,000 events across many sessions and asserts
// the three read paths stay interactive.
//
// The thresholds are deliberately loose. This is a regression tripwire for an
// accidental full scan or an N+1, not a benchmark: a query that quietly loses
// its index here goes from milliseconds to tens of seconds, which no threshold
// in this range could miss.
func TestUsageAttributionQueriesStayFastAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	if raceEnabled {
		// The assertions below are wall-clock budgets, and race instrumentation
		// costs roughly an order of magnitude. Under -race they would measure
		// the instrumentation rather than the query plan, so the tripwire would
		// fire on every run and stop meaning anything. The plan itself is
		// exercised by the ordinary run.
		t.Skip("wall-clock budgets are meaningless under race instrumentation")
	}
	for _, tc := range []struct {
		name          string
		sessions      int
		eventsPerSess int
		budget        time.Duration
	}{
		{"1k events", 10, 100, time.Second},
		{"10k events", 50, 200, 2 * time.Second},
		{"100k events", 100, 1000, 10 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runUsageScale(t, tc.sessions, tc.eventsPerSess, tc.budget)
		})
	}
}

func runUsageScale(t *testing.T, sessions, eventsPerSess int, budget time.Duration) {
	t.Helper()
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Unix(1710000000, 0).UTC()
	seedProject(t, s, attrProjectID)

	const (
		windowsPerSess = 4
		// One reviewer-pane event and one planner event per session, so every
		// scale tier carries all three subject kinds.
		paneEventsPerSess = 1
	)
	for i := 0; i < sessions; i++ {
		runID := fmt.Sprintf("wf-perf-%d", i%10)
		rec := sampleRecord(attrProjectID)
		rec.Harness = domain.HarnessClaudeCode
		sess, err := s.CreateSession(ctx, rec)
		mustNoError(t, err)
		binding := mustUpsertUsageBinding(t, s, sess, base, domain.UsageBindingRecord{
			NativeRootID: fmt.Sprintf("root-%d", i), InitialModelID: "claude-opus-5",
			State: domain.UsageBindingActive,
		})
		source := mustInsertUsageSource(t, s, base, domain.UsageSourceRecord{
			BindingID: binding.ID, Kind: domain.UsageSourceClaudeMain,
			NativeSessionID: fmt.Sprintf("root-%d", i),
			ArtifactPath:    fmt.Sprintf("/tmp/claude/perf-%d.jsonl", i),
			FileIdentity:    fmt.Sprintf("dev:ino:%d", i), State: domain.UsageSourcePending,
		})
		roles := []domain.WorkflowRole{
			domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker,
			domain.WorkflowRoleWorker, domain.WorkflowRoleFixWorker,
		}
		for w := 0; w < windowsPerSess; w++ {
			openWindow(t, s, window{
				key:     fmt.Sprintf("w-%d-%d", i, w),
				session: string(sess.ID), run: runID, role: roles[w], cycle: int64(w / 2),
				opened: base.Add(time.Duration(w) * time.Hour),
			})
		}
		events := make([]domain.ModelUsageEvent, 0, eventsPerSess)
		for e := 0; e < eventsPerSess; e++ {
			events = append(events, attrEvent(
				fmt.Sprintf("perf-%d-%d", i, e), 100, 20,
				base.Add(time.Duration(e)*time.Minute),
			))
		}
		applyEvents(t, s, source, base.Add(240*time.Hour), events)

		// P3-E §19: the same run also carries PANE and PLANNER subjects, so the
		// scale check exercises the mixed-subject scoping rather than a
		// session-only shape. Generalizing attribution must not reintroduce the
		// quadratic plan, and a query that only ever saw one subject kind would
		// not prove it.
		paneSubject := domain.RuntimePaneSubject(fmt.Sprintf("rr-%d", i))
		openWindow(t, s, window{
			key: fmt.Sprintf("w-pane-%d", i), subjectKind: domain.UsageSubjectRuntimePane,
			session: paneSubject.ID, run: runID, role: domain.WorkflowRoleReviewer,
			opened: base.Add(5 * time.Hour), harness: "codex",
		})
		paneSource := seedPaneSource(t, s, base, paneSubject, domain.HarnessCodex, fmt.Sprintf("pane-thread-%d", i))
		paneEvents := make([]domain.ModelUsageEvent, 0, paneEventsPerSess)
		for e := 0; e < paneEventsPerSess; e++ {
			paneEvents = append(paneEvents, codexEvent(
				fmt.Sprintf("pane-%d-%d", i, e), 100, 20,
				base.Add(5*time.Hour+time.Duration(e)*time.Minute),
			))
		}
		applyEvents(t, s, paneSource, base.Add(240*time.Hour), paneEvents)

		plannerSubject := domain.PlannerInvocationSubject(fmt.Sprintf("%s#%d", runID, i))
		openWindow(t, s, window{
			key: fmt.Sprintf("w-plan-%d", i), subjectKind: domain.UsageSubjectPlannerInvocation,
			session: plannerSubject.ID, run: runID, role: domain.WorkflowRolePlanner,
			opened: base.Add(-time.Hour), harness: "claude-code",
		})
		planBinding, perr := s.UpsertUsageBinding(ctx, domain.UsageBindingRecord{
			Subject: plannerSubject, Harness: domain.HarnessClaudeCode,
			NativeRootID: plannerSubject.ID, State: domain.UsageBindingComplete, UpdatedAt: base,
		})
		mustNoError(t, perr)
		mustNoError(t, s.RecordDirectUsageEvent(ctx, planBinding.ID, domain.ModelUsageEvent{
			ModelID: "claude-sonnet-5", SourceEventKey: fmt.Sprintf("plan-%d", i),
			Tokens:     domain.UsageTokenMetrics{InputTokens: 100, UncachedInputTokens: 100, OutputTokens: 20},
			ObservedAt: timePtrUTC(base.Add(-30 * time.Minute)),
		}, base))
	}

	measure := func(name string, fn func() error) {
		t.Helper()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if elapsed := time.Since(start); elapsed > budget {
			t.Errorf("%s took %s, budget %s — a read this slow means an index was lost or the CTE was flattened back into a quadratic plan",
				name, elapsed, budget)
		} else {
			t.Logf("%s: %s", name, elapsed)
		}
	}

	measure("run detail (AggregateWorkflowRunUsage)", func() error {
		lines, err := s.AggregateWorkflowRunUsage(ctx, "wf-perf-3")
		if err == nil && len(lines) == 0 {
			return fmt.Errorf("expected rows for a seeded run")
		}
		return err
	})
	measure("board (AggregateCompactRunUsageForProject)", func() error {
		_, err := s.AggregateCompactRunUsageForProject(ctx, attrProjectID)
		return err
	})
	measure("project 30d (AggregateProjectUsage)", func() error {
		_, err := s.AggregateProjectUsage(ctx, attrProjectID, base.Add(-24*time.Hour), base.Add(720*time.Hour))
		return err
	})
	measure("parent family (AggregateRunFamilyUsage)", func() error {
		_, err := s.AggregateRunFamilyUsage(ctx, "wf-perf-1")
		return err
	})

	// The point of the whole exercise: the totals must still be right at scale.
	lines, err := s.AggregateCompactRunUsageForProject(ctx, attrProjectID)
	mustNoError(t, err)
	var total int64
	for _, l := range lines {
		total += l.Tokens.Total()
	}
	// Sessions + panes + planner invocations, each counted exactly once.
	if want := int64(sessions * (eventsPerSess + paneEventsPerSess + 1) * 120); total != want {
		t.Fatalf("project total = %d, want %d", total, want)
	}
}
