package workitems

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// worker.go — the background drain, and the inbound refresh (P4-E §9).
//
// WHY POLLING AND NOT WEBHOOKS. Plane supports outbound webhooks, and §9 says
// to prefer them when they are clean. They are not, for AO specifically:
//
//   - AO's primary listener is bound to 127.0.0.1 and is unauthenticated by a
//     hard rule in AGENTS.md. There is nowhere for a hosted Plane to deliver
//     to without either exposing that listener or adding a second
//     network-facing bind, which the same rule forbids.
//   - The one LAN listener AO may run is opt-in, plaintext, home-network-only
//     and explicitly not for control routes. It is not a webhook endpoint.
//   - A webhook needs a stable reachable URL and a shared secret per
//     installation. An unattended desktop daemon behind NAT has neither.
//
// So the inbound direction is a bounded poll, and the outbound direction — the
// one that matters, because AO's state is the authoritative one — is the
// outbox, which is event-driven and does not poll at all.
//
// The poll is deliberately cheap and deliberately does NOT reconcile anything
// into AO. It refreshes the display cache on links so the UI has something true
// to show, and it stops there. There is no code path from a polled external
// state into an AO state, which is the direction rule from domain/workitem_sync.go
// enforced at the one place it would be tempting to break.

// WorkerConfig configures the background drain.
type WorkerConfig struct {
	// Interval is how often the queue is drained. Zero means
	// DefaultSyncInterval.
	Interval time.Duration
	// Batch bounds one tick's deliveries. Zero means DefaultBatchSize.
	Batch int
	// RefreshInterval is how often linked items' display caches are refreshed.
	// Zero means DefaultRefreshInterval; negative switches the refresh off.
	RefreshInterval time.Duration
	Logger          *slog.Logger
}

// DefaultRefreshInterval is how often the inbound poll runs.
//
// Fifteen minutes because what it maintains is a display cache, not a decision
// input. Nothing in AO acts on it, so refreshing it faster buys a slightly
// fresher badge and costs a request per link per interval against a provider
// with a 60-per-minute budget.
const DefaultRefreshInterval = 15 * time.Minute

// Worker drains the outbox and refreshes link snapshots.
type Worker struct {
	svc      *Service
	interval time.Duration
	batch    int
	refresh  time.Duration
	log      *slog.Logger

	lastRefresh time.Time
}

// NewWorker builds the background worker.
func NewWorker(svc *Service, cfg WorkerConfig) *Worker {
	w := &Worker{
		svc: svc, interval: cfg.Interval, batch: cfg.Batch,
		refresh: cfg.RefreshInterval, log: cfg.Logger,
	}
	if w.interval <= 0 {
		w.interval = DefaultSyncInterval
	}
	if w.batch <= 0 {
		w.batch = DefaultBatchSize
	}
	if w.refresh == 0 {
		w.refresh = DefaultRefreshInterval
	}
	if w.log == nil {
		w.log = slog.Default()
	}
	return w
}

// Start runs the worker until ctx is cancelled, returning a channel that closes
// when it has stopped — the same shape as every other AO poller.
//
// A nil worker, or one with no service, starts nothing and returns a closed
// channel, so a daemon built without the integration does not have to guard the
// call site.
func (w *Worker) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if w == nil || w.svc == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.tick(ctx)
			}
		}
	}()
	return done
}

// Tick runs one pass synchronously, for tests and for a forced drain.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	out, err := w.svc.SyncOnce(ctx, w.batch)
	if err != nil {
		w.log.Debug("work items: could not drain the sync queue", "err", err)
	} else if out.Claimed > 0 {
		w.log.Info("work items: sync queue drained",
			"claimed", out.Claimed, "delivered", out.Delivered,
			"deferred", out.Deferred, "failed", out.Failed, "skipped", out.Skipped)
	}

	if w.refresh > 0 {
		now := w.svc.now()
		if w.lastRefresh.IsZero() || now.Sub(w.lastRefresh) >= w.refresh {
			w.lastRefresh = now
			w.refreshSnapshots(ctx)
		}
	}
}

// refreshSnapshots re-reads linked items so the UI has a current title and
// state to render, and so a state a person changed in Plane is visible in AO.
//
// It reconciles NOTHING into AO. The only write it makes is
// TouchWorkItemLinkSnapshot, which by construction can change a link's cached
// display fields and nothing else — not what the link points at, and certainly
// not any AO state.
func (w *Worker) refreshSnapshots(ctx context.Context) {
	configs, err := w.svc.store.ListEnabledWorkItemConfigs(ctx)
	if err != nil {
		w.log.Debug("work items: could not list configured projects", "err", err)
		return
	}
	for _, cfg := range configs {
		if ctx.Err() != nil {
			return
		}
		w.refreshProject(ctx, cfg.ProjectID)
	}
}

func (w *Worker) refreshProject(ctx context.Context, projectID domain.ProjectID) {
	links, err := w.svc.store.ListWorkItemLinks(ctx, projectID)
	if err != nil || len(links) == 0 {
		return
	}
	client, cfg, err := w.svc.client(ctx, projectID)
	if err != nil {
		return
	}
	for _, link := range links {
		if ctx.Err() != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
		item, gErr := client.Get(callCtx, link.Ref)
		cancel()
		if gErr != nil {
			// One unreachable item is not evidence about the others, and it is
			// certainly not a reason to change anything in AO. It is recorded
			// and the loop continues.
			w.svc.audit(ctx, store.WorkItemAuditRow{
				ProjectID: projectID, LinkID: link.ID, Provider: cfg.Provider,
				Operation: "refresh", ExternalItemID: link.Ref.ID, ExternalItemKey: link.Ref.Key,
				Outcome: outcomeFor(gErr), ErrorKind: errorKind(gErr), Detail: providerMessage(gErr),
			})
			continue
		}
		if item.Title == link.LastSeenTitle && item.StateGroup == link.LastSeenState {
			continue
		}
		if tErr := w.svc.store.TouchWorkItemLinkSnapshot(
			ctx, link.ID, item.Title, item.StateGroup, w.svc.now(),
		); tErr != nil {
			w.log.Debug("work items: could not refresh a link snapshot", "link", link.ID, "err", tErr)
		}
	}
}

// Notifier is the slice of AO's notification manager this service uses.
// Declared narrowly so the integration cannot invent notification types.
type Notifier interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
}

// WithNotifier attaches the notification manager.
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notifier = n
	return s
}

// notifyPermanentFailure tells somebody when a sync will never be delivered.
//
// §15's rule, applied: this is the ONLY thing the integration notifies about.
// Not a successful sync, not a deferred one, not a rate limit — a person does
// not need to be told that a background queue is working, and a notification
// per delivery would make the notification surface useless.
//
// The event that does deserve one is "AO gave up": the external board is now
// permanently out of step with AO for this piece of work, and only a person can
// decide whether that matters.
func (s *Service) notifyPermanentFailure(
	ctx context.Context, row store.WorkItemSyncRow, link domain.WorkItemLink, cause error,
) {
	if s.notifier == nil {
		return
	}
	target := link.Ref.Key
	if target == "" {
		target = link.Ref.ID
	}
	intent := ports.NotificationIntent{
		Type:      domain.NotificationIntegrationFailed,
		ProjectID: row.ProjectID,
		CreatedAt: s.now(),
		// The dedupe key is the outbox row's own, so a failure announced once
		// is not announced again by a later sweep.
		DedupeKey: "workitem-sync-failed:" + row.DedupeKey,
		Provider:  string(link.Ref.Provider),
		Detail: "Could not update " + target + " in " + string(link.Ref.Provider) +
			": " + providerMessage(cause),
	}
	if link.Scope == domain.WorkItemScopeRun {
		intent.WorkflowRunID = link.ScopeID
	}
	if link.Scope == domain.WorkItemScopeTask {
		intent.TaskID = link.ScopeID
	}
	if err := s.notifier.Notify(ctx, intent); err != nil {
		s.log.Debug("work items: could not raise a sync-failure notification", "err", err)
	}
}
