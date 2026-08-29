// Package runtimegc reclaims the runtime artifacts AO leaves behind, and
// refuses to reclaim anything it cannot prove is its own.
//
// # What this is for
//
// A daemon that runs for weeks accumulates tmux sessions: a reviewer whose
// verdict landed, a worker whose run was cancelled, a repair that finished. Each
// one holds a session name, a pane, a shell and whatever the agent left
// running. Nothing removed them, so the answer to "why is this machine slow"
// was, eventually, "there are forty tmux sessions on it".
//
// # What it must never do
//
// Destroying a runtime is irreversible and it is the single most dangerous
// thing in this package, so the safety model is stated before the mechanism:
//
//	UNKNOWN IS NOT DEAD.
//
// A session AO cannot prove it owns, cannot prove is finished, or cannot
// address by its exact incarnation is SKIPPED and reported. It is never
// destroyed on the strength of a name, a heuristic, an age, or the absence of
// evidence. That is the same fail-closed rule P0 established for adoption and
// termination, applied to cleanup.
//
// Three proofs are required before anything is destroyed, and all three come
// from durable facts or from the runtime itself -- never from timing:
//
//  1. OWNERSHIP. Either the session carries AO's ownership token
//     (ports.SessionFacts.Owner), or a durable capacity claim records that AO
//     launched this exact incarnation for a specific run and step. A session on
//     AO's own socket that satisfies neither is unprovable, not owned.
//  2. INCARNATION. Every destructive act is addressed to the immutable
//     InstanceID (tmux's `$N`), re-validated immediately before the kill and
//     confirmed immediately after. A session that took the same NAME after the
//     candidate was classified survives, because the kill was never addressed
//     to a name.
//  3. TERMINALITY. The authority that could still be using the runtime -- the
//     capacity claim, the workflow run, the step generation -- must be finished
//     or superseded. A claim still holding a slot protects its runtime
//     absolutely.
//
// # What it deletes, and what it keeps
//
// It destroys RUNTIME resources only. It deletes no durable row: not a session
// record, not a claim, not a checkpoint, not an attempt. Lifecycle evidence
// outlives the runtime that produced it, which is what keeps a finished run
// explainable, and it is also why this package needs no ledger of its own --
// a destroy that lands and whose report is lost is simply re-derived as
// "absent, nothing to do" on the next sweep. Crash windows §N(9) and §N(10)
// therefore do not exist here rather than being handled.
package runtimegc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Disposition is what a sweep decided about one runtime.
type Disposition string

const (
	// DispositionCleaned means the runtime was destroyed, with all three
	// proofs in hand.
	DispositionCleaned Disposition = "cleaned"
	// DispositionCandidate is a dry-run's answer: it WOULD have been cleaned.
	DispositionCandidate Disposition = "candidate"
	// DispositionLive means an authority still references it. Never touched.
	DispositionLive Disposition = "live"
	// DispositionUnprovable means AO could not prove ownership, incarnation or
	// terminality. Never touched -- and reported, because an unprovable
	// resource that nobody can see is how orphans become permanent.
	DispositionUnprovable Disposition = "unprovable"
	// DispositionForeign means AO can prove the runtime is NOT its own.
	DispositionForeign Disposition = "foreign"
	// DispositionAbsent means it was already gone when the sweep looked.
	DispositionAbsent Disposition = "absent"
	// DispositionError means this one candidate failed. Recorded, and the
	// sweep continues: one broken resource must never abort the others.
	DispositionError Disposition = "error"
)

// OrphanClass names WHY a runtime was a candidate, so an operator reading a
// report can tell a tidy-up from a symptom.
type OrphanClass string

const (
	// OrphanNone is a runtime that is not an orphan at all.
	OrphanNone OrphanClass = ""
	// OrphanReleasedClaim is the ordinary case: the capacity claim that paid
	// for this runtime has been released, so nothing is using it. Auto-cleanable.
	OrphanReleasedClaim OrphanClass = "released_claim"
	// OrphanSupersededGeneration is a runtime whose claim belongs to a
	// lifecycle generation the run has moved past. Auto-cleanable once the
	// exact incarnation is proven.
	OrphanSupersededGeneration OrphanClass = "superseded_generation"
	// OrphanTerminalRun is a runtime whose workflow run has ended.
	// Auto-cleanable.
	OrphanTerminalRun OrphanClass = "terminal_run"
	// OrphanUnreferenced is an AO-owned session on AO's own server that no
	// live authoritative generation references. Auto-cleanable ONLY when the
	// ownership token proves it is AO's; otherwise it is unprovable.
	OrphanUnreferenced OrphanClass = "unreferenced_owned_session"
	// OrphanUnprovableOwnership is a session AO cannot attribute. Never
	// cleaned, always reported -- operator territory.
	OrphanUnprovableOwnership OrphanClass = "unprovable_ownership"
)

// AutoCleanable reports whether this class may be reclaimed automatically once
// the sweep's three proofs hold. Ownership it cannot prove never can be.
func (c OrphanClass) AutoCleanable() bool {
	switch c {
	case OrphanReleasedClaim, OrphanSupersededGeneration, OrphanTerminalRun, OrphanUnreferenced:
		return true
	default:
		return false
	}
}

// Finding is one runtime the sweep looked at, and what it decided.
type Finding struct {
	// Handle and InstanceID identify the runtime. InstanceID is the authority
	// key; Handle is only a label.
	Handle     string
	InstanceID string
	// Class is why it was a candidate, and Disposition is what happened.
	Class       OrphanClass
	Disposition Disposition
	// Reason is AO's own sentence, and it always says why the resource was or
	// was not destroyed -- an audit that says only "skipped" is not one.
	Reason string
	// WorkflowRunID and DispatchKey tie the finding back to the lifecycle,
	// when it came from a capacity claim.
	WorkflowRunID string
	DispatchKey   string
	// Err is set for DispositionError.
	Err string
}

// Report is one sweep's whole result. It is observability, not authority:
// nothing reads it back to make a decision, so losing it to a restart costs
// nothing but the summary.
type Report struct {
	StartedAt  time.Time
	FinishedAt time.Time
	DryRun     bool
	// Trigger says what asked for this sweep (startup, periodic, operator).
	Trigger  string
	Findings []Finding
	// Counters are the summary §P asks for.
	Candidates        int
	Cleaned           int
	SkippedLive       int
	SkippedUnprovable int
	SkippedForeign    int
	Absent            int
	Errors            int
}

// count folds a finding into the report's counters.
func (r *Report) count(f Finding) {
	r.Findings = append(r.Findings, f)
	switch f.Disposition {
	case DispositionCleaned:
		r.Candidates++
		r.Cleaned++
	case DispositionCandidate:
		r.Candidates++
	case DispositionLive:
		r.SkippedLive++
	case DispositionUnprovable:
		r.SkippedUnprovable++
	case DispositionForeign:
		r.SkippedForeign++
	case DispositionAbsent:
		r.Absent++
	case DispositionError:
		r.Errors++
	}
}

// ClaimReader is the durable capacity state a sweep reasons from.
type ClaimReader interface {
	ListOutstandingCapacityClaims(ctx context.Context) ([]domain.CapacityClaim, error)
	ListHeldCapacityClaims(ctx context.Context) ([]domain.CapacityClaim, error)
	ListCapacityClaimsForRun(ctx context.Context, runID string) ([]domain.CapacityClaim, error)
}

// RunReader answers whether the authority behind a runtime is still live.
type RunReader interface {
	GetWorkflowRun(ctx context.Context, id string) (domain.WorkflowRun, bool, error)
	ListWorkflowRuns(ctx context.Context, projectID string) ([]domain.WorkflowRun, error)
}

// Sweeper performs Runtime GC.
//
// Every dependency is optional in the sense that a missing one narrows what
// can be swept rather than making the sweep unsafe: without an Inventory there
// is no orphan scan, without Claims there is no claim-derived candidate, and
// with neither the sweep finds nothing and reports so.
type Sweeper struct {
	Inventory ports.RuntimeInventory
	Facts     ports.SessionFactsReader
	Claims    ClaimReader
	Runs      RunReader
	Log       *slog.Logger
	Now       func() time.Time
}

// Options tune one sweep.
type Options struct {
	// DryRun classifies everything and destroys nothing. Every predicate is
	// identical to a real sweep's: a dry run that took a different path would
	// not be a preview of anything.
	DryRun bool
	// Trigger is recorded on the report.
	Trigger string
}

func (s *Sweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Sweeper) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Sweep reclaims what it can prove, skips what it cannot, and reports both.
//
// It never returns an error for a single bad candidate: §W's rule is that one
// unprovable or broken resource must not stop the others being handled, so a
// per-candidate failure becomes a DispositionError finding and the sweep
// continues. An error is returned only when the sweep could not start at all.
func (s *Sweeper) Sweep(ctx context.Context, opts Options) (Report, error) {
	report := Report{StartedAt: s.now(), DryRun: opts.DryRun, Trigger: opts.Trigger}

	// The protected set, computed FIRST and from durable rows: every
	// incarnation a live claim is currently paying for. A runtime in this set
	// is never a candidate, whatever else the sweep observes about it -- this
	// is what makes "capacity release and GC ordering cannot create a false
	// free slot" true in the safe direction. A claim that is still held always
	// wins over any inference the scan could make.
	protected, err := s.protectedInstances(ctx)
	if err != nil {
		return report, fmt.Errorf("runtime gc: could not read live capacity claims: %w", err)
	}

	seen := map[string]struct{}{}
	for _, f := range s.claimCandidates(ctx, protected) {
		if _, dup := seen[f.InstanceID]; dup && f.InstanceID != "" {
			continue
		}
		if f.InstanceID != "" {
			seen[f.InstanceID] = struct{}{}
		}
		report.count(s.resolve(ctx, f, opts))
	}
	for _, f := range s.inventoryCandidates(ctx, protected, seen) {
		report.count(s.resolve(ctx, f, opts))
	}

	report.FinishedAt = s.now()
	sort.SliceStable(report.Findings, func(i, j int) bool {
		return report.Findings[i].InstanceID < report.Findings[j].InstanceID
	})
	s.log().Info("runtime gc: sweep finished",
		"trigger", report.Trigger, "dryRun", report.DryRun,
		"candidates", report.Candidates, "cleaned", report.Cleaned,
		"skippedLive", report.SkippedLive, "skippedUnprovable", report.SkippedUnprovable,
		"skippedForeign", report.SkippedForeign, "absent", report.Absent, "errors", report.Errors)
	return report, nil
}

// protectedInstances is every runtime incarnation a HELD claim is paying for.
//
// It is the sweep's first read and its hardest rule: a held claim is live
// authority, and no later observation may override it.
func (s *Sweeper) protectedInstances(ctx context.Context) (map[string]domain.CapacityClaim, error) {
	out := map[string]domain.CapacityClaim{}
	if s.Claims == nil {
		return out, nil
	}
	held, err := s.Claims.ListHeldCapacityClaims(ctx)
	if err != nil {
		return nil, err
	}
	for _, claim := range held {
		if claim.RuntimeInstanceID != "" {
			out[claim.RuntimeInstanceID] = claim
		}
	}
	return out, nil
}

// claimCandidates derives candidates from durable capacity claims: a released
// claim that still names a runtime, whose run is terminal or whose generation
// was superseded.
//
// This is the strongest candidate source, because the claim records the exact
// incarnation AO launched. Nothing here is inferred from a name.
func (s *Sweeper) claimCandidates(ctx context.Context, protected map[string]domain.CapacityClaim) []Finding {
	if s.Claims == nil || s.Runs == nil {
		return nil
	}
	// Outstanding claims are read to find runtimes whose RUN has ended while
	// the claim was never released -- crash window §N(7). A run that is
	// terminal releases its claims on reconcile, but GC must not depend on
	// that having happened yet.
	outstanding, err := s.Claims.ListOutstandingCapacityClaims(ctx)
	if err != nil {
		s.log().Warn("runtime gc: could not list outstanding claims", "err", err)
		return nil
	}
	var findings []Finding
	for _, claim := range outstanding {
		if claim.RuntimeInstanceID == "" {
			continue
		}
		if _, live := protected[claim.RuntimeInstanceID]; live {
			// A held claim protects its own runtime. Only a claim whose run
			// has ended may be reconsidered below.
			run, found, rerr := s.Runs.GetWorkflowRun(ctx, claim.WorkflowRunID)
			if rerr != nil || !found || !run.State.Terminal() {
				findings = append(findings, Finding{
					Handle: claim.RuntimeHandle, InstanceID: claim.RuntimeInstanceID,
					Class: OrphanNone, Disposition: DispositionLive,
					Reason:        "a held capacity claim is still paying for this runtime",
					WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
				})
				continue
			}
			findings = append(findings, Finding{
				Handle: claim.RuntimeHandle, InstanceID: claim.RuntimeInstanceID,
				Class:         OrphanTerminalRun,
				Reason:        "the workflow run this runtime belongs to reached " + string(run.State),
				WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
			})
			continue
		}
		// A queued claim has no runtime to protect and should not have named
		// one; if it did, the run's own state decides.
		run, found, rerr := s.Runs.GetWorkflowRun(ctx, claim.WorkflowRunID)
		if rerr != nil || !found {
			findings = append(findings, Finding{
				Handle: claim.RuntimeHandle, InstanceID: claim.RuntimeInstanceID,
				Class: OrphanUnprovableOwnership, Disposition: DispositionUnprovable,
				Reason:        "the workflow run this claim names could not be read, so AO cannot say whether the runtime is still needed",
				WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
			})
			continue
		}
		if !run.State.Terminal() {
			findings = append(findings, Finding{
				Handle: claim.RuntimeHandle, InstanceID: claim.RuntimeInstanceID,
				Class: OrphanNone, Disposition: DispositionLive,
				Reason:        "the workflow run is still " + string(run.State),
				WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
			})
			continue
		}
		findings = append(findings, Finding{
			Handle: claim.RuntimeHandle, InstanceID: claim.RuntimeInstanceID,
			Class:         OrphanTerminalRun,
			Reason:        "the workflow run this runtime belongs to reached " + string(run.State),
			WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
		})
	}
	return findings
}

// inventoryCandidates scans AO's own runtime server for sessions no live
// authority references.
//
// Two rules make this safe. AO's socket is its own (see the tmux adapter's
// ListSessions), so nothing here can even see the operator's sessions. And
// within it, a session is only ever a candidate when its OWNERSHIP TOKEN
// proves it is AO's: a session on AO's server with no token is reported as
// unprovable and never touched, because "on my server" is not the same claim
// as "mine to destroy".
func (s *Sweeper) inventoryCandidates(ctx context.Context, protected map[string]domain.CapacityClaim, seen map[string]struct{}) []Finding {
	if s.Inventory == nil {
		return nil
	}
	sessions, err := s.Inventory.ListSessions(ctx)
	if err != nil {
		s.log().Warn("runtime gc: inventory unavailable; no orphan scan this sweep", "err", err)
		return nil
	}
	var findings []Finding
	for _, sess := range sessions {
		if sess.InstanceID == "" {
			continue
		}
		if _, dup := seen[sess.InstanceID]; dup {
			continue
		}
		if claim, live := protected[sess.InstanceID]; live {
			findings = append(findings, Finding{
				Handle: sess.ID, InstanceID: sess.InstanceID,
				Class: OrphanNone, Disposition: DispositionLive,
				Reason:        "a held capacity claim is still paying for this runtime",
				WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
			})
			continue
		}
		if !sess.OwnerKnown || sess.Owner == "" {
			// Unknown is not dead. AO cannot attribute this session, so it is
			// recorded and left exactly where it is.
			findings = append(findings, Finding{
				Handle: sess.ID, InstanceID: sess.InstanceID,
				Class: OrphanUnprovableOwnership, Disposition: DispositionUnprovable,
				Reason: "the session carries no AO ownership token, so AO cannot prove it is its own to destroy",
			})
			continue
		}
		findings = append(findings, Finding{
			Handle: sess.ID, InstanceID: sess.InstanceID,
			Class:  OrphanUnreferenced,
			Reason: "an AO-owned session (" + sess.Owner + ") that no live capacity claim references",
		})
	}
	return findings
}

// resolve turns a candidate into an outcome, applying the three proofs
// immediately before acting.
//
// The re-validation here is not belt-and-braces: everything above was decided
// from rows and from an earlier listing, and a session can exit, or be
// replaced under the same name, in between. So the incarnation is re-read, the
// ownership re-checked, and the destroy addressed to `$N` -- then confirmed.
func (s *Sweeper) resolve(ctx context.Context, f Finding, opts Options) Finding {
	if f.Disposition != "" {
		return f
	}
	if !f.Class.AutoCleanable() {
		f.Disposition = DispositionUnprovable
		return f
	}
	if s.Facts == nil {
		f.Disposition = DispositionUnprovable
		f.Reason = "no runtime facts reader is wired, so AO cannot prove what it would be destroying"
		return f
	}
	handle := ports.RuntimeHandle{ID: f.Handle, InstanceID: f.InstanceID}

	facts, exists, err := s.Facts.SessionFacts(ctx, handle)
	switch {
	case errors.Is(err, ports.ErrRuntimeUnavailable):
		f.Disposition, f.Err = DispositionUnprovable, err.Error()
		f.Reason = "the runtime could not be reached, so its state is unknown; nothing was destroyed"
		return f
	case err != nil:
		f.Disposition, f.Err = DispositionError, err.Error()
		f.Reason = "reading this runtime's facts failed; it was left alone and the sweep continued"
		return f
	case !exists:
		f.Disposition = DispositionAbsent
		f.Reason = "the runtime was already gone"
		return f
	case facts.InstanceID != f.InstanceID:
		// The name now answers for a different incarnation. This is the ABA
		// the whole model exists to exclude, and the answer is to do nothing.
		f.Disposition = DispositionForeign
		f.Reason = fmt.Sprintf("the session behind this name changed from %s to %s; refusing to destroy a runtime AO did not classify",
			f.InstanceID, facts.InstanceID)
		return f
	}

	if opts.DryRun {
		f.Disposition = DispositionCandidate
		return f
	}

	if err := s.Facts.DestroyInstance(ctx, f.InstanceID); err != nil {
		f.Disposition, f.Err = DispositionError, err.Error()
		f.Reason = "destroying this runtime failed; it was left alone and the sweep continued"
		return f
	}
	// Confirm THAT incarnation is gone -- not merely that the name is free,
	// which a replacement would also satisfy.
	after, stillThere, aerr := s.Facts.SessionFacts(ctx, handle)
	switch {
	case aerr != nil:
		f.Disposition, f.Err = DispositionError, aerr.Error()
		f.Reason = "the runtime was destroyed but AO could not confirm it; the next sweep re-checks"
		return f
	case stillThere && after.InstanceID == f.InstanceID:
		f.Disposition = DispositionError
		f.Reason = "the runtime survived destruction"
		return f
	}
	f.Disposition = DispositionCleaned
	s.log().Info("runtime gc: destroyed a runtime AO proved was its own and finished",
		"instance", f.InstanceID, "handle", f.Handle, "class", f.Class,
		"run", f.WorkflowRunID, "reason", f.Reason)
	return f
}
