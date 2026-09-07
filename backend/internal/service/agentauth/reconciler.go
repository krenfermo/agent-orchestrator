package agentauth

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/agentcred"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// reconciler.go -- the end of an agent's authority, as a reconciliation rather
// than as an event.
//
// WHY THIS IS NOT AN EVENT HANDLER. Revoking on a "review finished" callback is
// the obvious design and it is the one that produced the bug. A credential's
// life then depends on somebody remembering to end it at every one of the
// places a review can end -- a verdict, a cancellation, a supersession, a
// failed launch, a stall, a daemon killed in between -- and the success path,
// the only one that always happens, was the one nobody wired. What is left
// behind is invisible: a live grant over work that concluded, with nothing in
// the system that knows it should not exist.
//
// So the obligation is DERIVED instead of remembered:
//
//	a credential may live exactly as long as its review run is running.
//
// That predicate is durable, it is one SQL statement, and it is re-evaluated
// from scratch on every pass. Nothing about it can be lost by a crash and there
// is nothing to replay after one; an installation upgraded onto this build with
// credentials already stranded discharges them on its first pass, without
// anybody migrating anything.
//
// EAGER AND SWEPT ARE THE SAME DECISION. CloseReviewRun runs the instant a
// verdict lands, so a finished reviewer stops being able to speak within its
// own request rather than within a tick. ReconcileOnce runs on boot and on a
// timer and covers everything else -- cancellation, replacement, an abandoned
// launch, a revocation whose write failed. Neither holds an opinion of its own:
// both ask the same guarded statement, so they cannot disagree, and either one
// alone would still converge.
//
// WHAT IT DELIBERATELY DOES NOT DO. It never anticipates a closure. A reviewer
// whose run is still running keeps its identity however long the review takes,
// because taking it away early recreates precisely the failure the credential
// exists to prevent: a real review that cannot be recorded (wf-98ab416c, and
// the late-verdict preservation in service/review that exists because of it).
// And it never fails anything: a workflow that completed did complete, whatever
// happened to the cleanup afterwards.

// DefaultReconcileInterval is how often the sweep asks whether any credential
// has outlived its review run.
//
// It is short relative to the 72-hour credential TTL and long relative to the
// eager path, which is what actually ends a credential in the ordinary case.
// This is the backstop for the paths that have no request to hang off -- a
// cancellation, a supersession, a failed write -- so it is measured against how
// long a stranded grant may acceptably remain, not against how fast anything
// needs to feel.
const DefaultReconcileInterval = 60 * time.Second

// ReconcilerConfig configures the revocation sweep.
type ReconcilerConfig struct {
	// DataDir is where credential FILES live. Empty disables file removal and
	// leaves row revocation untouched -- the row is what authorises, so a
	// reconciler with no data dir is degraded, never unsafe.
	DataDir  string
	Interval time.Duration
	Logger   *slog.Logger
}

// Reconciler ends agent credentials whose authority is over: the durable row
// that authorises, and the file the token was handed over in.
//
// Both, and in that order. The row is what a request is checked against, so it
// goes first and its failure is the only one worth retrying; the file is a
// convenience for the pane that already read it, and once the row is revoked
// what is left on disk is a dead token.
type Reconciler struct {
	svc      *Service
	dataDir  string
	interval time.Duration
	log      *slog.Logger
}

// NewReconciler builds the revocation sweep over the credential service.
func NewReconciler(svc *Service, cfg ReconcilerConfig) *Reconciler {
	r := &Reconciler{svc: svc, dataDir: cfg.DataDir, interval: cfg.Interval, log: cfg.Logger}
	if r.interval <= 0 {
		r.interval = DefaultReconcileInterval
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

// Start runs an immediate pass followed by interval passes until ctx is
// cancelled, returning a channel that closes when the goroutine exits.
//
// The immediate pass IS the restart recovery: whatever a previous incarnation
// failed to revoke, or died before revoking, is derivable from the same rows
// and is taken back here.
//
// A nil Reconciler (an installation with no credential service wired) starts
// nothing and returns a closed channel, so the call site needs no guard.
func (r *Reconciler) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if r == nil || r.svc == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		r.pass(ctx)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.pass(ctx)
			}
		}
	}()
	return done
}

// ReconcileOnce runs a single sweep synchronously and reports what it took
// back. It exists so boot recovery and tests can drive a pass without waiting
// out a ticker.
func (r *Reconciler) ReconcileOnce(ctx context.Context) ([]domain.RevocableAgentCredential, error) {
	if r == nil || r.svc == nil {
		return nil, nil
	}
	revoked, err := r.svc.ReconcileClosedReviewRuns(ctx)
	if err != nil {
		return nil, err
	}
	r.removeFiles(revoked)
	return revoked, nil
}

// CloseReviewRun ends the authority of one review run's credentials, the moment
// that run stops running.
//
// This is the eager half, called from the durable transition itself. It is
// guarded in SQL rather than by its caller: a call made while the review is
// still running revokes nothing and reports nothing, which is what makes it
// safe to place at the transition instead of after it.
//
// It returns an error only so a caller that wants to log one can. No caller may
// fail on it: a review that concluded concluded, and a cleanup that did not
// land is picked up by the next sweep.
func (r *Reconciler) CloseReviewRun(ctx context.Context, reviewRunID string) error {
	if r == nil || r.svc == nil {
		return nil
	}
	revoked, err := r.svc.RevokeForClosedReviewRun(ctx, reviewRunID)
	if err != nil {
		return err
	}
	r.removeFiles(revoked)
	return nil
}

// pass is one sweep, with its failure absorbed. A sweep that cannot read or
// write must not take the daemon down and must not be retried in a tight loop:
// the obligation is durable, so the next tick is a complete retry.
func (r *Reconciler) pass(ctx context.Context) {
	revoked, err := r.ReconcileOnce(ctx)
	if err != nil {
		// The error text is the store's own and names rows, never tokens.
		r.log.Warn("agent credential revocation pass failed; it will be retried", "error", err)
		return
	}
	for _, cred := range revoked {
		// Identifiers only. A credential id and a review run id are exactly
		// what an operator needs to correlate this against the run; the token
		// and its hash are never read by this path at all.
		r.log.Info("revoked an agent credential whose review run had ended",
			"credential", cred.CredentialID, "reviewRun", cred.ReviewRunID)
	}
}

// removeFiles deletes the credential files of credentials that have just been
// revoked. Best effort in every direction: the grant is already gone, so a file
// that will not delete is litter rather than authority, and the next pass will
// not see the row again to try once more.
func (r *Reconciler) removeFiles(revoked []domain.RevocableAgentCredential) {
	if r.dataDir == "" {
		return
	}
	for _, cred := range revoked {
		if cred.RuntimeHandle == "" {
			continue
		}
		if err := agentcred.Remove(agentcred.Path(r.dataDir, cred.RuntimeHandle)); err != nil {
			r.log.Debug("could not remove a revoked agent credential file",
				"credential", cred.CredentialID, "error", err)
		}
	}
}
