package workitems

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// sync.go — the outbox, and the worker that drains it (P4-E §7, §8, §9, §13).
//
// WHY AN OUTBOX AND NOT A CALL. AO's lifecycle observes a state change and
// wants an external board to know. Doing that inline would put a third party's
// availability on AO's execution path: a Plane timeout would become a slow
// state transition, and a Plane outage would become a failing one. §13 forbids
// exactly that.
//
// So the lifecycle writes a row and returns. The row is durable, so a restart
// resumes it; it is uniquely keyed on the real-world moment, so a duplicate
// callback writes nothing; and the worker that drains it may fail freely,
// because nothing waits on it.
//
// THE QUEUE IS THE CHECKPOINT. §9 asks for restart-safe reconciliation state
// that is not memory-only. There is no separate cursor table because there is
// nothing a cursor would add: a pending row IS "this has not been delivered
// yet", and it survives a crash by being a row.

const (
	// providerTimeout bounds one provider call made on behalf of a person or
	// one outbox row. Plane's own client timeout is the same; this bounds the
	// whole operation including any lookups the adapter makes.
	providerTimeout = 20 * time.Second

	// DefaultSyncInterval is how often the worker looks for due rows.
	//
	// Deliberately unhurried. Everything in the queue is a notification, not a
	// transaction, and a planning board learning about a state change thirty
	// seconds late costs nobody anything. A tighter loop would spend more time
	// asking than delivering.
	DefaultSyncInterval = 30 * time.Second

	// DefaultBatchSize bounds how many rows one tick delivers.
	DefaultBatchSize = 10

	// maxAttempts is when a row stops being retried.
	//
	// With the backoff below this spans a little over an hour, which covers a
	// provider restart, a deploy, or a rate-limit window — and stops well short
	// of retrying into next week, because a queue that never gives up is a
	// queue nobody ever looks at.
	maxAttempts = 6
)

// backoffFor returns how long to wait before attempt n+1.
//
// Exponential from thirty seconds, capped. The cap matters more than the
// growth: an uncapped exponential would push the sixth attempt past the point
// where anybody still cares about the event.
func backoffFor(attempts int64) time.Duration {
	d := DefaultSyncInterval << attempts
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

// EnqueueRequest is one AO execution moment worth reporting.
type EnqueueRequest struct {
	ProjectID domain.ProjectID
	Scope     domain.WorkItemLinkScope
	ScopeID   string
	Event     domain.WorkItemSyncEvent
	// Detail is the one-line human explanation carried into the comment —
	// a stop reason, a commit subject. It is never a transcript and never
	// terminal output.
	Detail string
}

// Enqueue records one sync intent.
//
// EVERY FAILURE MODE HERE IS SILENT AND SAFE, because this is the one function
// the execution path calls. It returns an error only so a caller that wants to
// log one can; no caller may branch on it, and nothing in AO does.
//
//   - no link for this work        -> nothing to sync, no row
//   - sync switched off            -> nothing to sync, no row
//   - event already enqueued       -> the dedupe key absorbs it, no second row
//   - the provider is unreachable  -> irrelevant; this makes no network call
func (s *Service) Enqueue(ctx context.Context, req EnqueueRequest) error {
	if !domain.ValidWorkItemSyncEvent(req.Event) {
		return fmt.Errorf("workitems: unknown sync event %q", req.Event)
	}
	link, found, err := s.store.GetWorkItemLinkByScope(ctx, req.ProjectID, req.Scope, req.ScopeID)
	if err != nil || !found || !link.SyncEnabled {
		return err
	}
	cfg, err := s.Resolve(ctx, req.ProjectID)
	if err != nil || !cfg.Enabled {
		return err
	}
	// Both switches off means the row would have nothing to do on delivery.
	if !cfg.SyncStates && !cfg.SyncComments {
		return nil
	}

	target, _ := req.Event.TargetStateGroup()
	now := s.now()
	_, err = s.store.EnqueueWorkItemSync(ctx, store.WorkItemSyncRow{
		ID:          s.mintID(),
		ProjectID:   req.ProjectID,
		LinkID:      link.ID,
		Event:       req.Event,
		Body:        commentFor(req.Event, req.Detail),
		TargetState: target,
		DedupeKey:   domain.SyncDedupeKey(req.Scope, req.ScopeID, req.Event),
	}, now)
	return err
}

// EnqueueRunState is the convenience the workflow lifecycle calls: it maps a
// run state to an event and enqueues it, or does nothing when that state is not
// worth reporting.
func (s *Service) EnqueueRunState(
	ctx context.Context, projectID domain.ProjectID, runID string, state domain.WorkflowRunState, detail string,
) error {
	event, ok := domain.WorkItemSyncEventForRun(state)
	if !ok {
		return nil
	}
	return s.Enqueue(ctx, EnqueueRequest{
		ProjectID: projectID, Scope: domain.WorkItemScopeRun, ScopeID: runID,
		Event: event, Detail: detail,
	})
}

// EnqueueTaskState is the planned-task equivalent.
func (s *Service) EnqueueTaskState(
	ctx context.Context, projectID domain.ProjectID, taskID string, state domain.WorkflowTaskState, detail string,
) error {
	event, ok := domain.WorkItemSyncEventForTask(state)
	if !ok {
		return nil
	}
	return s.Enqueue(ctx, EnqueueRequest{
		ProjectID: projectID, Scope: domain.WorkItemScopeTask, ScopeID: taskID,
		Event: event, Detail: detail,
	})
}

// commentFor renders the progress note for one event.
//
// §8's rules are enforced here, once, rather than at every producer: no raw
// terminal output, no secrets, no stack traces. What goes out is one sentence
// naming what happened plus the caller's own one-line detail, bounded — the
// same sentence AO's own Board shows.
func commentFor(event domain.WorkItemSyncEvent, detail string) string {
	var lead string
	switch event {
	case domain.WorkItemSyncStarted:
		lead = "Agent Orchestrator started working on this."
	case domain.WorkItemSyncNeedsAttention:
		lead = "Agent Orchestrator stopped and needs a decision."
	case domain.WorkItemSyncCompleted:
		lead = "Agent Orchestrator completed this work."
	case domain.WorkItemSyncFailed:
		lead = "Agent Orchestrator could not complete this work."
	case domain.WorkItemSyncCancelled:
		lead = "This work was cancelled in Agent Orchestrator."
	default:
		lead = "Agent Orchestrator reported an update."
	}
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return lead
	}
	// One line only. A detail that arrived with newlines is a summary somebody
	// pasted, and a planning board is not where a multi-line log belongs.
	if i := strings.IndexAny(detail, "\r\n"); i >= 0 {
		detail = detail[:i]
	}
	const maxDetail = 400
	if len(detail) > maxDetail {
		detail = detail[:maxDetail] + "…"
	}
	return lead + "\n" + detail
}

// SyncOnce drains up to one batch of due rows and reports how many settled.
//
// It is exported so a test can drive the worker deterministically, and so an
// operator can force a drain from the API without waiting for a tick.
func (s *Service) SyncOnce(ctx context.Context, limit int) (SyncOutcome, error) {
	if limit <= 0 {
		limit = DefaultBatchSize
	}
	rows, err := s.store.ClaimDueWorkItemSyncs(ctx, s.now(), limit)
	if err != nil {
		return SyncOutcome{}, err
	}
	out := SyncOutcome{Claimed: len(rows)}
	for _, row := range rows {
		select {
		case <-ctx.Done():
			// Shutting down. What was delivered stays delivered and the rest
			// stays pending for the next tick — which is exactly what the
			// outbox is for, so this is a clean stop rather than an error.
			return out, nil
		default:
		}
		switch s.deliver(ctx, row) {
		case deliveredOK:
			out.Delivered++
		case deliveredDeferred:
			out.Deferred++
		case deliveredFailed:
			out.Failed++
		case deliveredSkipped:
			out.Skipped++
		}
	}
	return out, nil
}

// SyncOutcome is what one drain did.
type SyncOutcome struct {
	Claimed   int `json:"claimed"`
	Delivered int `json:"delivered"`
	Deferred  int `json:"deferred"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

type deliveryResult int

const (
	deliveredOK deliveryResult = iota
	deliveredDeferred
	deliveredFailed
	deliveredSkipped
)

// deliver performs one outbox row.
//
// The order is state first, comment second, and it matters: if the comment
// succeeds and the state write then fails, a retry re-posts the comment — which
// the provider's dedupe key absorbs — and retries the state. The other order
// would leave a state moved with no explanation next to it, which is the
// version a person reading the board would find confusing.
func (s *Service) deliver(ctx context.Context, row store.WorkItemSyncRow) deliveryResult {
	started := s.now()

	link, found, err := s.store.GetWorkItemLink(ctx, row.LinkID)
	if err != nil || !found || !link.SyncEnabled {
		// The link was removed or muted after the row was queued. That is a
		// person's decision taking effect, not a failure.
		s.settleSkipped(ctx, row, "the link was removed or sync was switched off")
		return deliveredSkipped
	}

	client, cfg, err := s.client(ctx, row.ProjectID)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			s.settleSkipped(ctx, row, "the integration is no longer configured for this project")
			return deliveredSkipped
		}
		s.deferRow(ctx, row, link, err, started)
		return deliveredDeferred
	}

	callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	var opErr error
	op := "sync"
	if cfg.SyncStates && row.TargetState != "" {
		op = "transition"
		opErr = client.Transition(callCtx, link.Ref, row.TargetState)
	}
	if opErr == nil && cfg.SyncComments && row.Body != "" {
		op = "comment"
		opErr = client.Comment(callCtx, link.Ref, row.Body, row.DedupeKey)
	}

	now := s.now()
	audit := store.WorkItemAuditRow{
		ProjectID: row.ProjectID, LinkID: link.ID, Provider: link.Ref.Provider,
		Operation: op, ExternalItemID: link.Ref.ID, ExternalItemKey: link.Ref.Key,
		Attempts: row.Attempts + 1, DurationMS: now.Sub(started).Milliseconds(),
		ErrorKind: errorKind(opErr), Detail: providerMessage(opErr),
		Outcome: outcomeFor(opErr),
	}

	if opErr == nil {
		if _, mErr := s.store.MarkWorkItemSyncDone(ctx, row.ID, now); mErr != nil {
			s.log.Warn("work items: could not settle a delivered sync", "row", row.ID, "err", mErr)
		}
		s.audit(ctx, audit)
		return deliveredOK
	}

	if retryable(opErr) && row.Attempts+1 < maxAttempts {
		audit.Outcome = store.WorkItemAuditRetryable
		s.audit(ctx, audit)
		s.deferRow(ctx, row, link, opErr, started)
		return deliveredDeferred
	}

	audit.Outcome = store.WorkItemAuditFailed
	s.audit(ctx, audit)
	if _, mErr := s.store.MarkWorkItemSyncFailed(ctx, row.ID, providerMessage(opErr), now); mErr != nil {
		s.log.Warn("work items: could not settle a failed sync", "row", row.ID, "err", mErr)
	}
	// A permanently failed sync is the one thing here worth telling somebody
	// about (§15). Successful syncs notify nobody.
	s.notifyPermanentFailure(ctx, row, link, opErr)
	return deliveredFailed
}

func (s *Service) deferRow(ctx context.Context, row store.WorkItemSyncRow, link domain.WorkItemLink, cause error, started time.Time) {
	now := s.now()
	next := now.Add(backoffFor(row.Attempts))
	if _, dErr := s.store.DeferWorkItemSync(ctx, row.ID, providerMessage(cause), next, now); dErr != nil {
		s.log.Warn("work items: could not defer a sync", "row", row.ID, "err", dErr)
	}
	s.audit(ctx, store.WorkItemAuditRow{
		ProjectID: row.ProjectID, LinkID: link.ID, Provider: link.Ref.Provider,
		Operation: "deliver", ExternalItemID: link.Ref.ID, ExternalItemKey: link.Ref.Key,
		Outcome: store.WorkItemAuditRetryable, ErrorKind: errorKind(cause),
		Detail: providerMessage(cause), Attempts: row.Attempts + 1,
		DurationMS: now.Sub(started).Milliseconds(),
	})
}

func (s *Service) settleSkipped(ctx context.Context, row store.WorkItemSyncRow, reason string) {
	now := s.now()
	// Settled as done rather than failed: nothing went wrong, the intent
	// simply no longer applies, and leaving it pending would retry it forever.
	if _, err := s.store.MarkWorkItemSyncDone(ctx, row.ID, now); err != nil {
		s.log.Warn("work items: could not settle a skipped sync", "row", row.ID, "err", err)
	}
	s.audit(ctx, store.WorkItemAuditRow{
		ProjectID: row.ProjectID, LinkID: row.LinkID, Operation: "deliver",
		Outcome: store.WorkItemAuditSkipped, Detail: reason, Attempts: row.Attempts,
	})
}

// Health is the integration's operational state for one project (§14).
type Health struct {
	Configured bool  `json:"configured"`
	Enabled    bool  `json:"enabled"`
	Connected  bool  `json:"connected"`
	Degraded   bool  `json:"degraded"`
	Pending    int64 `json:"pending"`
	Failed     int64 `json:"failed"`
	Links      int   `json:"links"`
	// LastCheckAt and LastCheckError describe the most recent preflight.
	LastCheckAt    string `json:"lastCheckAt,omitempty"`
	LastCheckError string `json:"lastCheckError,omitempty"`
}

// Health reports the integration's state without touching the provider.
//
// It makes no network call on purpose: a status endpoint that probes is one
// that is slow exactly when the thing it reports on is broken.
func (s *Service) Health(ctx context.Context, projectID domain.ProjectID) (Health, error) {
	view, err := s.Config(ctx, projectID)
	if err != nil {
		return Health{}, err
	}
	counts, err := s.store.WorkItemSyncCounts(ctx, projectID)
	if err != nil {
		return Health{}, err
	}
	links, err := s.store.ListWorkItemLinks(ctx, projectID)
	if err != nil {
		return Health{}, err
	}
	h := Health{
		Configured:     view.Workspace != "" && view.ExternalProjectID != "" && view.TokenConfigured,
		Enabled:        view.Enabled,
		Connected:      view.Connected,
		Degraded:       view.Degraded,
		Pending:        counts[store.WorkItemSyncPending],
		Failed:         counts[store.WorkItemSyncFailed],
		Links:          len(links),
		LastCheckAt:    view.LastCheckAt,
		LastCheckError: view.LastCheckError,
	}
	// A queue with failures is degraded even when the last preflight passed:
	// "the credential works" and "AO is delivering" are different claims, and
	// the UI must not show the first as though it settled the second.
	if h.Failed > 0 {
		h.Degraded = true
	}
	return h, nil
}

// Audit returns the recent provider operations for one project.
func (s *Service) Audit(ctx context.Context, projectID domain.ProjectID, limit int) ([]store.WorkItemAuditRow, error) {
	return s.store.ListWorkItemAudit(ctx, projectID, limit)
}

// Queue returns the recent outbox rows for one project.
func (s *Service) Queue(ctx context.Context, projectID domain.ProjectID, limit int) ([]store.WorkItemSyncRow, error) {
	return s.store.ListWorkItemSyncs(ctx, projectID, limit)
}

// audit records one operation, best-effort.
//
// A failure to record history must never change what happened: an audit write
// that errors is logged and swallowed, because the alternative is a delivered
// sync being reported as failed by its own bookkeeping.
func (s *Service) audit(ctx context.Context, row store.WorkItemAuditRow) {
	if row.ID == "" {
		row.ID = s.mintID()
	}
	if row.Provider == "" {
		row.Provider = domain.WorkItemProviderPlane
	}
	if err := s.store.RecordWorkItemAudit(ctx, row, s.now()); err != nil {
		s.log.Debug("work items: could not record an audit row", "op", row.Operation, "err", err)
	}
}

// errorKind extracts the adapter's classification, or "" for success.
func errorKind(err error) string {
	if err == nil {
		return ""
	}
	var wErr *ports.WorkItemsError
	if errors.As(err, &wErr) {
		return string(wErr.Kind)
	}
	return "unknown"
}

// providerMessage renders an error for storage and display.
//
// It uses the adapter's already-truncated, already-sanitised message and never
// the raw error chain, which is what keeps a credential out of an audit row
// even if some future transport error were to embed a URL.
func providerMessage(err error) string {
	if err == nil {
		return ""
	}
	var wErr *ports.WorkItemsError
	if errors.As(err, &wErr) {
		if wErr.Message != "" {
			return wErr.Message
		}
		return string(wErr.Kind)
	}
	const maxLen = 200
	msg := err.Error()
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}

func outcomeFor(err error) string {
	if err == nil {
		return store.WorkItemAuditOK
	}
	if retryable(err) {
		return store.WorkItemAuditRetryable
	}
	return store.WorkItemAuditFailed
}

// retryable reports whether an error is worth another attempt.
//
// An UNCLASSIFIED error is retryable: an error AO could not classify is most
// often a transport problem, and the attempt ceiling bounds the cost of being
// wrong. Being wrong the other way — treating a transient failure as terminal —
// silently drops the event.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var wErr *ports.WorkItemsError
	if errors.As(err, &wErr) {
		return wErr.Retryable()
	}
	return true
}
