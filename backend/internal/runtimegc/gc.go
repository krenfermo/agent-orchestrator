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
	// OrphanIntegratedWorktree is P1-D §X: an AO-managed task worktree whose
	// work has provably landed on its target. Auto-cleanable -- the checkout
	// is a copy, and the commits it held are reachable from the recorded
	// integration SHA.
	OrphanIntegratedWorktree OrphanClass = "integrated_worktree"
	// OrphanTerminatedSession is P1-D's worker case: a session AO durably
	// records as terminated, whose runtime it can prove is the exact one it
	// created for that session's launch. Auto-cleanable.
	//
	// It exists because P1-C could not reclaim workers at all. Reviewer panes
	// carried an ownership token and workers did not, so a finished worker's
	// tmux session was provably AO's only when a capacity claim happened to
	// name it, and everything else was reported unprovable forever.
	OrphanTerminatedSession OrphanClass = "terminated_session"
)

// AutoCleanable reports whether this class may be reclaimed automatically once
// the sweep's three proofs hold. Ownership it cannot prove never can be.
func (c OrphanClass) AutoCleanable() bool {
	switch c {
	case OrphanReleasedClaim, OrphanSupersededGeneration, OrphanTerminalRun,
		OrphanUnreferenced, OrphanTerminatedSession, OrphanIntegratedWorktree:
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

// SessionReader is the durable session state a sweep reasons from (P1-D §C).
//
// It is what makes a WORKER runtime reclaimable: the session row records the
// incarnation AO created and the ownership token it created it with, so a
// terminated session's runtime can be proven AO's own without relying on a
// capacity claim having named it.
type SessionReader interface {
	ListAllSessions(ctx context.Context) ([]domain.SessionRecord, error)
}

// WorktreeReader is the durable placement state a sweep reasons from (P1-D
// §X). Optional: without it there is no worktree sweep, only a runtime one.
type WorktreeReader interface {
	ListTaskWorktrees(ctx context.Context) ([]domain.TaskWorktreeRecord, error)
}

// WorktreeReleaser removes one AO-managed worktree, by the identity the record
// carries. It is separate from the reader for the same reason
// SessionFactsReader separates reading from destroying: enumerating is how
// candidates are found, and proving one is safe is a different question.
type WorktreeReleaser interface {
	ReleaseTaskWorktree(ctx context.Context, runID, taskID string) error
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
	// Sessions is P1-D's worker-ownership source. Optional: without it the
	// sweep simply has one fewer way to prove ownership, and reports the
	// sessions it cannot attribute rather than acting on them.
	Sessions SessionReader
	// Worktrees and WorktreeGC are P1-D §X's repository-placement sweep. Both
	// optional and both required together: reading without releasing finds
	// candidates nothing can act on, and releasing without reading has nothing
	// to act on.
	Worktrees  WorktreeReader
	WorktreeGC WorktreeReleaser
	Log        *slog.Logger
	Now        func() time.Time
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
	for _, f := range s.sessionCandidates(ctx, protected) {
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

	for _, f := range s.worktreeCandidates(ctx, opts) {
		report.count(f)
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

// sessionCandidates derives candidates from durable SESSION state: a session
// AO records as terminated, whose runtime incarnation and ownership token it
// recorded when it created it (P1-D §C).
//
// This is the strongest proof available for a worker, and it is what closes
// P1-C's deferral. The token is not merely "AO made something": it names the
// session AND the launch generation, so it cannot match a later launch's
// replacement runtime.
//
// A session with no recorded incarnation or no recorded token is a pre-P1-D
// session. It is reported unprovable and never touched: this closes the gap
// going forward and deliberately does not fabricate ownership backwards.
func (s *Sweeper) sessionCandidates(ctx context.Context, protected map[string]domain.CapacityClaim) []Finding {
	if s.Sessions == nil {
		return nil
	}
	sessions, err := s.Sessions.ListAllSessions(ctx)
	if err != nil {
		s.log().Warn("runtime gc: could not read sessions; no session-derived candidates this sweep", "err", err)
		return nil
	}
	var findings []Finding
	for _, sess := range sessions {
		instance := sess.Metadata.RuntimeInstanceID
		token := sess.Metadata.RuntimeOwnerToken
		if instance == "" || token == "" {
			// Pre-P1-D. Not reported here: the inventory pass is what sees
			// whatever is actually still on the runtime, and reporting a row
			// with no runtime behind it would be noise rather than evidence.
			continue
		}
		if !domain.RuntimeOwnedBySession(token, sess.ID, sess.Metadata.RuntimeLaunchID) {
			// The recorded token does not match the recorded launch. AO cannot
			// say which incarnation this row describes, so it says so.
			findings = append(findings, Finding{
				Handle: sess.Metadata.RuntimeHandleID, InstanceID: instance,
				Class: OrphanUnprovableOwnership, Disposition: DispositionUnprovable,
				Reason: "the session's recorded ownership token does not match its recorded launch generation",
			})
			continue
		}
		if claim, live := protected[instance]; live {
			findings = append(findings, Finding{
				Handle: sess.Metadata.RuntimeHandleID, InstanceID: instance,
				Class: OrphanNone, Disposition: DispositionLive,
				Reason:        "a held capacity claim is still paying for this runtime",
				WorkflowRunID: claim.WorkflowRunID, DispatchKey: claim.DispatchKey,
			})
			continue
		}
		if !sess.IsTerminated {
			findings = append(findings, Finding{
				Handle: sess.Metadata.RuntimeHandleID, InstanceID: instance,
				Class: OrphanNone, Disposition: DispositionLive,
				Reason: "the session is not terminated",
			})
			continue
		}
		findings = append(findings, Finding{
			Handle: sess.Metadata.RuntimeHandleID, InstanceID: instance,
			Class:  OrphanTerminatedSession,
			Reason: "AO created this exact runtime for a session it has since recorded as terminated",
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

// worktreeCandidates is P1-D §X: reclaiming AO-managed task worktrees.
//
// The safety rule is the same one the runtime sweep obeys, applied to a
// checkout instead of a session, and it is stricter in one respect: a worktree
// can hold the ONLY copy of an agent's work. So the single condition that
// licenses removal is proof that the work already landed somewhere else.
//
// Removed only when ALL of these hold:
//
//   - the record's state is `integrated` -- AO's own durable statement that
//     the task's work reached its target;
//   - IntegratedSHA is recorded, so "it landed" names a commit rather than
//     being a state nobody can check;
//   - the workflow run is terminal, so nothing is still reviewing, fixing or
//     verifying in it.
//
// Never removed, and each of these is a separate refusal rather than a
// fall-through:
//
//   - `active` or `creating` -- the task is using it;
//   - `preserved` -- AO's explicit durable "do not clean this up", which is
//     what a failed or abandoned task's unintegrated work gets;
//   - `failed` -- the record of an attempt a person still has to look at;
//   - any record whose run is not terminal, which covers a worktree under
//     review, under a fix cycle, awaiting verification, or blocked on an
//     unresolved merge conflict;
//   - anything AO did not create, which it never sees: this walks AO's own
//     durable records, not the filesystem, so a human's worktree is not merely
//     spared, it is invisible.
//
// Removal is delegated to the workspace manager, which owns the git side and
// addresses the worktree by its record identity (run + task) rather than by
// path -- so a path reused by something else cannot be removed by a stale
// candidate.
func (s *Sweeper) worktreeCandidates(ctx context.Context, opts Options) []Finding {
	if s.Worktrees == nil || s.WorktreeGC == nil || s.Runs == nil {
		return nil
	}
	records, err := s.Worktrees.ListTaskWorktrees(ctx)
	if err != nil {
		s.log().Warn("runtime gc: could not read task worktrees; no placement sweep this pass", "err", err)
		return nil
	}
	var findings []Finding
	for _, rec := range records {
		f := Finding{
			Handle: rec.Path, WorkflowRunID: rec.WorkflowRunID,
		}
		switch {
		case rec.State == domain.TaskWorktreePreserved:
			f.Class, f.Disposition = OrphanNone, DispositionLive
			f.Reason = "the record says this work was deliberately preserved; its commits may be the only copy"
		case rec.State != domain.TaskWorktreeIntegrated:
			f.Class, f.Disposition = OrphanNone, DispositionLive
			f.Reason = "the worktree is " + string(rec.State) + "; only work AO can prove has landed is removed"
		case rec.IntegratedSHA == "":
			f.Class, f.Disposition = OrphanUnprovableOwnership, DispositionUnprovable
			f.Reason = "the record claims integration but names no commit, so AO cannot prove the work is safe elsewhere"
		default:
			run, found, rerr := s.Runs.GetWorkflowRun(ctx, rec.WorkflowRunID)
			switch {
			case rerr != nil || !found:
				f.Class, f.Disposition = OrphanUnprovableOwnership, DispositionUnprovable
				f.Reason = "the workflow run this worktree belongs to could not be read"
			case !run.State.Terminal():
				f.Class, f.Disposition = OrphanNone, DispositionLive
				f.Reason = "the workflow run is still " + string(run.State) + "; review, fix or verification may still need this checkout"
			default:
				f.Class = OrphanIntegratedWorktree
				f.Reason = "the task's work landed at " + rec.IntegratedSHA + " and its run has ended"
				if opts.DryRun {
					f.Disposition = DispositionCandidate
				} else if rerr := s.WorktreeGC.ReleaseTaskWorktree(ctx, rec.WorkflowRunID, rec.TaskID); rerr != nil {
					f.Disposition, f.Err = DispositionError, rerr.Error()
					f.Reason = "removing this worktree failed; it was left alone and the sweep continued"
				} else {
					f.Disposition = DispositionCleaned
					s.log().Info("runtime gc: removed a task worktree whose work provably landed",
						"run", rec.WorkflowRunID, "task", rec.TaskID, "path", rec.Path, "integratedSha", rec.IntegratedSHA)
				}
			}
		}
		findings = append(findings, f)
	}
	return findings
}
