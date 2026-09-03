package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// usage_attribution_store.go — the P3-E reads and the one write that make the
// usage ledger answer "which role spent this".
//
// The write is deliberately the only one: OpenUsageAttributionWindow. There is
// no update and no close. A window is a fact about an instant AO chose (it
// handed this role this session, now), and the next window's opening is what
// ends the previous one, so there is no second write that a crash could lose
// and no state a restart could disagree with.

// OpenUsageAttributionWindow records that, from window.OpenedAt onward, tokens
// spent in window.SessionID belong to window.Role.
//
// It is idempotent on DedupeKey. That is what makes it safe to call from a
// dispatch path that is replayed by failover, by resume after a restart, and
// by a wake: the second call is a no-op, so a role's tokens are never split
// across two windows for one obligation.
func (s *Store) OpenUsageAttributionWindow(ctx context.Context, window domain.UsageAttributionWindow) error {
	if window.DedupeKey == "" || !window.Subject().Valid() || window.Role == "" {
		return fmt.Errorf("usage attribution window needs a dedupe key, a valid subject and a role")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.InsertUsageAttributionWindow(ctx, gen.InsertUsageAttributionWindowParams{
		DedupeKey:           window.DedupeKey,
		SubjectKind:         string(window.Subject().Kind),
		SessionID:           window.SessionID,
		ProjectID:           window.ProjectID,
		WorkflowRunID:       window.WorkflowRunID,
		ParentWorkflowRunID: window.ParentWorkflowRunID,
		TaskID:              window.TaskID,
		WorkflowStepID:      window.WorkflowStepID,
		AttemptID:           window.AttemptID,
		AttemptOrdinal:      window.AttemptOrdinal,
		Cycle:               window.Cycle,
		Role:                string(window.Role),
		Harness:             window.Harness,
		Provider:            window.Provider,
		Model:               window.Model,
		OpenedAt:            timeOrNow(window.OpenedAt),
		CreatedAt:           timeOrNow(window.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("open usage attribution window %q: %w", window.DedupeKey, err)
	}
	return nil
}

// ListUsageAttributionWindowsForRun returns every role window a run opened,
// oldest first, each carrying whether its session can be metered at all.
func (s *Store) ListUsageAttributionWindowsForRun(ctx context.Context, runID string) ([]domain.UsageAttributionWindow, error) {
	rows, err := s.qr.ListUsageAttributionWindowsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list usage attribution windows for run %s: %w", runID, err)
	}
	out := make([]domain.UsageAttributionWindow, 0, len(rows))
	for _, row := range rows {
		w := usageWindowFromGen(gen.UsageAttributionWindow{
			ID: row.ID, DedupeKey: row.DedupeKey, SubjectKind: row.SubjectKind, SessionID: row.SessionID,
			ProjectID: row.ProjectID, WorkflowRunID: row.WorkflowRunID,
			ParentWorkflowRunID: row.ParentWorkflowRunID, TaskID: row.TaskID,
			WorkflowStepID: row.WorkflowStepID, AttemptID: row.AttemptID,
			AttemptOrdinal: row.AttemptOrdinal, Cycle: row.Cycle, Role: row.Role,
			Harness: row.Harness, Provider: row.Provider, Model: row.Model,
			OpenedAt: row.OpenedAt, CreatedAt: row.CreatedAt,
		})
		w.HasUsageBinding = row.HasUsageBinding != 0
		out = append(out, w)
	}
	return out, nil
}

// ListUsageAttributionWindowsForSession returns one session's windows in the
// order they were opened.
func (s *Store) ListUsageAttributionWindowsForSession(ctx context.Context, sessionID string) ([]domain.UsageAttributionWindow, error) {
	rows, err := s.qr.ListUsageAttributionWindowsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list usage attribution windows for session %s: %w", sessionID, err)
	}
	out := make([]domain.UsageAttributionWindow, 0, len(rows))
	for _, row := range rows {
		out = append(out, usageWindowFromGen(row))
	}
	return out, nil
}

// UsageLedgerLine is one grouped fold of the ledger: the grain a cost can be
// computed at (one model) together with the attribution that grain belongs to.
type UsageLedgerLine struct {
	WorkflowRunID     string
	Role              domain.WorkflowRole
	Cycle             int64
	TaskID            string
	AttemptID         string
	AttemptOrdinal    int64
	Harness           string
	Provider          string
	ModelID           string
	Tokens            domain.UsageTokenTotals
	ApproximateEvents int64
}

// AggregateWorkflowRunUsage folds one run's ledger into per-model lines.
func (s *Store) AggregateWorkflowRunUsage(ctx context.Context, runID string) ([]UsageLedgerLine, error) {
	rows, err := s.qr.AggregateWorkflowRunUsage(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage for run %s: %w", runID, err)
	}
	out := make([]UsageLedgerLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageLedgerLine{
			WorkflowRunID: runID,
			Role:          domain.WorkflowRole(r.Role), Cycle: r.Cycle, TaskID: r.TaskID,
			AttemptID: r.AttemptID, AttemptOrdinal: r.AttemptOrdinal,
			Harness: r.Harness, Provider: r.Provider, ModelID: r.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens: r.InputTokens, UncachedInputTokens: r.UncachedInputTokens,
				CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens,
				OutputTokens: r.OutputTokens, ReasoningTokens: r.ReasoningTokens,
				ReasoningKnown: r.ReasoningEventCount > 0, EventCount: r.EventCount,
			},
			ApproximateEvents: r.ApproximateCount,
		})
	}
	return out, nil
}

// AggregateProjectUsage folds a project's ledger for one period into per-model
// lines, keyed by run and role.
func (s *Store) AggregateProjectUsage(ctx context.Context, projectID string, from, to time.Time) ([]UsageLedgerLine, error) {
	rows, err := s.qr.AggregateProjectUsage(ctx, gen.AggregateProjectUsageParams{
		ProjectID: projectID, FromAt: from, ToAt: to,
	})
	if err != nil {
		return nil, fmt.Errorf("aggregate usage for project %s: %w", projectID, err)
	}
	out := make([]UsageLedgerLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageLedgerLine{
			WorkflowRunID: r.WorkflowRunID, Role: domain.WorkflowRole(r.Role),
			Harness: r.Harness, Provider: r.Provider, ModelID: r.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens: r.InputTokens, UncachedInputTokens: r.UncachedInputTokens,
				CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens,
				OutputTokens: r.OutputTokens, ReasoningTokens: r.ReasoningTokens,
				ReasoningKnown: r.ReasoningEventCount > 0, EventCount: r.EventCount,
			},
			ApproximateEvents: r.ApproximateCount,
		})
	}
	return out, nil
}

// AggregateCompactRunUsageForProject folds every run in a project in ONE
// query, so a board of fifty cards is one round trip rather than fifty.
func (s *Store) AggregateCompactRunUsageForProject(ctx context.Context, projectID string) ([]UsageLedgerLine, error) {
	rows, err := s.qr.AggregateCompactRunUsageForProject(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("aggregate compact usage for project %s: %w", projectID, err)
	}
	out := make([]UsageLedgerLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageLedgerLine{
			WorkflowRunID: r.WorkflowRunID, Provider: r.Provider, ModelID: r.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens: r.InputTokens, UncachedInputTokens: r.UncachedInputTokens,
				CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens,
				OutputTokens: r.OutputTokens, EventCount: r.EventCount,
			},
			ApproximateEvents: r.ApproximateCount,
		})
	}
	return out, nil
}

// AggregateRunFamilyUsage folds a run together with every child it launched.
// It is one query rather than a fan-out so an autonomous parent's budget check
// can never read a stale child total.
func (s *Store) AggregateRunFamilyUsage(ctx context.Context, runID string) ([]UsageLedgerLine, error) {
	rows, err := s.qr.AggregateRunFamilyUsage(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("aggregate family usage for run %s: %w", runID, err)
	}
	out := make([]UsageLedgerLine, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageLedgerLine{
			WorkflowRunID: r.WorkflowRunID, Provider: r.Provider, ModelID: r.ModelID,
			Tokens: domain.UsageTokenTotals{
				InputTokens: r.InputTokens, UncachedInputTokens: r.UncachedInputTokens,
				CacheReadTokens: r.CacheReadTokens, CacheWriteTokens: r.CacheWriteTokens,
				OutputTokens: r.OutputTokens, EventCount: r.EventCount,
			},
		})
	}
	return out, nil
}

// CountProjectUsageWorkflows counts distinct runs that spent anything in the
// period, so an average-per-workflow divides by a real denominator.
func (s *Store) CountProjectUsageWorkflows(ctx context.Context, projectID string, from, to time.Time) (int64, error) {
	n, err := s.qr.CountProjectUsageWorkflows(ctx, gen.CountProjectUsageWorkflowsParams{
		ProjectID: projectID, FromAt: from, ToAt: to,
	})
	if err != nil {
		return 0, fmt.Errorf("count usage workflows for project %s: %w", projectID, err)
	}
	return n, nil
}

func usageWindowFromGen(row gen.UsageAttributionWindow) domain.UsageAttributionWindow {
	return domain.UsageAttributionWindow{
		ID: row.ID, DedupeKey: row.DedupeKey,
		SubjectKind: domain.UsageSubjectKind(row.SubjectKind), SessionID: row.SessionID,
		ProjectID: row.ProjectID, WorkflowRunID: row.WorkflowRunID,
		ParentWorkflowRunID: row.ParentWorkflowRunID, TaskID: row.TaskID,
		WorkflowStepID: row.WorkflowStepID, AttemptID: row.AttemptID,
		AttemptOrdinal: row.AttemptOrdinal, Cycle: row.Cycle,
		Role:    domain.WorkflowRole(row.Role),
		Harness: row.Harness, Provider: row.Provider, Model: row.Model,
		OpenedAt: row.OpenedAt, CreatedAt: row.CreatedAt,
	}
}

// SumRunFamilyUsage folds a run together with every child it launched into
// per-model lines. It returns domain types rather than store types so the
// workflow coordinator's budget gate can read it without importing storage.
func (s *Store) SumRunFamilyUsage(ctx context.Context, runID string) ([]domain.ModelUsageLine, error) {
	lines, err := s.AggregateRunFamilyUsage(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ModelUsageLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, domain.ModelUsageLine{
			Provider: line.Provider, Harness: line.Harness, ModelID: line.ModelID,
			Tokens: line.Tokens, Source: domain.TokenSourceProvider,
		})
	}
	return out, nil
}

// RecordDirectUsageEvent stores a usage fact reported in a provider RESPONSE
// rather than read from a transcript — today, a planner invocation.
//
// It writes no cursor and touches no source, because there is no artifact: the
// planner runs under --no-session-persistence and states its spend exactly once,
// in the envelope it returns. Exactly-once therefore rests entirely where it
// always did, on UNIQUE (binding_id, source_event_key): a replayed recording of
// the same invocation inserts nothing.
func (s *Store) RecordDirectUsageEvent(ctx context.Context, bindingID int64, ev domain.ModelUsageEvent, recordedAt time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.qw.InsertDirectUsageEvent(ctx, gen.InsertDirectUsageEventParams{
		BindingID:           bindingID,
		ModelID:             ev.ModelID,
		InputTokens:         ev.Tokens.InputTokens,
		UncachedInputTokens: ev.Tokens.UncachedInputTokens,
		CacheReadTokens:     ev.Tokens.CacheReadTokens,
		CacheWriteTokens:    ev.Tokens.CacheWriteTokens,
		OutputTokens:        ev.Tokens.OutputTokens,
		ReasoningTokens:     ptrInt64ToNull(ev.Tokens.ReasoningTokens),
		SourceEventKey:      ev.SourceEventKey,
		ObservedAt:          ptrTimeToNullTime(ev.ObservedAt),
		RecordedAt:          sql.NullTime{Time: timeOrNow(recordedAt), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("record direct usage event for binding %d: %w", bindingID, err)
	}
	return nil
}
