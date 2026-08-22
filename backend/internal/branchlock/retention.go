package branchlock

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// This file is Checkpoint 8P-E.13A's answer to one question the original
// Checkpoint 8P-E.11 lock never had to answer: "this branch is held by a
// workflow that has stopped and will not restart itself — now what?"
//
// Before this file the answer was "hold the branch forever". Reconcile only
// released a lock whose run was missing or terminal, and needs_attention is
// neither, so a run parked on a decision only a human could make kept the
// repository+branch to itself indefinitely. Every later workflow targeting that
// branch queued behind a workflow nobody was going to resume. A real one did:
// wf-3220567f held feat/engineering-control-center for ~19h after its review
// step stopped for human attention, blocking every subsequent run in the same
// repository.
//
// The invariant this file states and enforces:
//
//	A held branch lock is legitimate only while its owner can still write that
//	branch — because it is live, because AO will resume it by itself, or
//	because it left uncommitted work in the repository that only it knows
//	about. A lock that meets none of those is stale and is released.
//
// Two things it deliberately does NOT do:
//
//   - It never decides from elapsed time. A lock is not stale because it is
//     old, and a run is not dead because its session went idle. Every input
//     below is a durable state fact.
//   - It never releases a lock protecting real uncommitted work. That case is
//     retained on purpose and reported as such (Retention.Reason), so the UI
//     can name the owner and offer cancelling it rather than silently queueing
//     behind it forever.

// Retention is what reconciliation decided about one held lock.
type Retention struct {
	Decision RetentionDecision
	// Reason is the human-readable justification. For a release it becomes the
	// lock row's release_reason; for a retained lock it is what the Board and
	// the run detail page show as "why is this branch still held".
	Reason string
}

// RetentionDecision is the closed set of outcomes for one held lock.
type RetentionDecision string

const (
	// RetentionRelease means the lock is stale: nothing will resume its owner
	// and it is protecting nothing.
	RetentionRelease RetentionDecision = "release"
	// RetentionAdopt means the lock is legitimate but belongs to a previous
	// daemon instance, so this instance takes ownership of it.
	RetentionAdopt RetentionDecision = "adopt"
	// RetentionKeep means the lock is legitimate and already owned by this
	// instance: nothing to do.
	RetentionKeep RetentionDecision = "keep"
)

// OwnerDisposition is what the workflow layer knows about a lock owner that is
// parked in needs_attention. The branchlock package deliberately cannot derive
// it: "can AO still finish this by itself" is the attention vocabulary's
// question, and it lives in internal/workflow.
type OwnerDisposition struct {
	// SelfRemediable means AO has a durable plan to resume this run (a
	// scheduled retry, a queued branch, a fix cycle). The lock stays.
	SelfRemediable bool
	// ProtectsWork means the run has already put changes into the working tree
	// that only it knows about. Releasing the branch would let a second
	// workflow start writing on top of them, so the lock stays and the
	// situation is reported instead.
	ProtectsWork bool
	// Reason names the owner's stop in the canonical attention vocabulary, for
	// the retained-lock explanation.
	Reason string
}

// OwnerClassifier resolves the disposition of a stopped lock owner. Optional:
// with no classifier wired, a needs_attention owner keeps its lock, which is
// exactly the pre-8P-E.13A behavior and is the safe direction to fail in.
type OwnerClassifier interface {
	ClassifyLockOwner(ctx context.Context, run domain.WorkflowRun) (OwnerDisposition, error)
}

// decideRetention is the whole policy, as a pure function of durable facts.
//
// found/run describe the owning workflow run; disp is consulted only for the
// one state where the run row alone cannot decide (needs_attention), and only
// when a classifier is wired.
func decideRetention(lock domain.BranchLock, run domain.WorkflowRun, found bool, disp OwnerDisposition, classified bool, ownerToken string) Retention {
	switch {
	case !found:
		return Retention{Decision: RetentionRelease, Reason: "stale: workflow run no longer exists"}
	case run.State.Terminal():
		return Retention{Decision: RetentionRelease, Reason: "stale: workflow run is " + string(run.State)}
	case run.State == domain.WorkflowRunNeedsAttention:
		switch {
		case !classified:
			// Nothing told us what this stop is. Holding is the conservative
			// direction: a wrongly released lock can corrupt a run's working
			// tree, a wrongly held one only delays another run.
			return keepOrAdopt(lock, ownerToken, "retained: owner stopped for attention and AO could not classify the stop")
		case disp.SelfRemediable:
			return keepOrAdopt(lock, ownerToken, "retained: owner is stopped on something AO resumes by itself ("+dispReason(disp)+")")
		case disp.ProtectsWork:
			return keepOrAdopt(lock, ownerToken, "retained: owner left uncommitted work in this repository and needs a human decision ("+dispReason(disp)+")")
		default:
			return Retention{
				Decision: RetentionRelease,
				Reason:   "stale: owner stopped for a human decision (" + dispReason(disp) + ") and has no uncommitted work in this repository",
			}
		}
	default:
		// pending / running / waiting: a live run, including one queued behind
		// another branch or waiting on provider capacity. It is going to write
		// this branch.
		return keepOrAdopt(lock, ownerToken, "owner is live ("+string(run.State)+")")
	}
}

func dispReason(d OwnerDisposition) string {
	if d.Reason == "" {
		return "unclassified_stop"
	}
	return d.Reason
}

func keepOrAdopt(lock domain.BranchLock, ownerToken, reason string) Retention {
	if lock.OwnerToken != ownerToken {
		return Retention{Decision: RetentionAdopt, Reason: reason}
	}
	return Retention{Decision: RetentionKeep, Reason: reason}
}

// decideSessionRetention is the retention policy for a lock an ordinary task
// session owns (Checkpoint 8P-E.14).
//
// It answers the same question decideRetention does -- "can this owner still
// write this branch?" -- against the only durable fact a session has:
// IsTerminated. There is deliberately no equivalent of the needs_attention
// branch here, because a task has no run state that could mean "stopped but
// resumable": a session is either terminated, in which case nothing will ever
// write through it again, or it is not, in which case it may still be working
// and its branch must be protected.
//
// Note what this does NOT do: it does not release a lock because the session
// looks idle. An idle session is the normal state of a task between agent
// turns, and releasing on idleness would hand the branch to a second writer
// while the first is still mid-task.
func decideSessionRetention(lock domain.BranchLock, session domain.SessionRecord, found bool, ownerToken string) Retention {
	switch {
	case !found:
		return Retention{Decision: RetentionRelease, Reason: "stale: task session no longer exists"}
	case session.IsTerminated:
		return Retention{Decision: RetentionRelease, Reason: "stale: task session is terminated"}
	default:
		return keepOrAdopt(lock, ownerToken, "owner is a live task session")
	}
}

// classifyLock resolves one held lock's owner and applies the retention policy
// for that owner kind.
func (m *Manager) classifyLock(ctx context.Context, lock domain.BranchLock) (Retention, error) {
	if lock.SessionOwned() {
		session, ok, err := m.store.GetSession(ctx, domain.SessionID(lock.SessionID))
		if err != nil {
			return Retention{}, fmt.Errorf("branch lock: load owner session %s: %w", lock.SessionID, err)
		}
		return decideSessionRetention(lock, session, ok, m.ownerToken), nil
	}
	run, ok, err := m.store.GetWorkflowRun(ctx, lock.WorkflowRunID)
	if err != nil {
		return Retention{}, fmt.Errorf("branch lock: load owner run %s: %w", lock.WorkflowRunID, err)
	}
	var (
		disp       OwnerDisposition
		classified bool
	)
	if ok && run.State == domain.WorkflowRunNeedsAttention && m.classifier != nil {
		disp, err = m.classifier.ClassifyLockOwner(ctx, run)
		if err != nil {
			// A classifier failure must not turn into a release: fall back to
			// the unclassified branch, which keeps the lock.
			if m.log != nil {
				m.log.Warn("branchlock: owner classification failed", "run", lock.WorkflowRunID, "err", err)
			}
		} else {
			classified = true
		}
	}
	return decideRetention(lock, run, ok, disp, classified, m.ownerToken), nil
}

// apply performs the decided action on one lock.
func (m *Manager) apply(ctx context.Context, lock domain.BranchLock, r Retention, now time.Time) error {
	switch r.Decision {
	case RetentionRelease:
		if _, err := m.store.ReleaseBranchLock(ctx, lock.ID, r.Reason, now); err != nil {
			return err
		}
	case RetentionAdopt:
		if _, err := m.store.AdoptBranchLock(ctx, lock.ID, m.ownerToken, now); err != nil {
			return err
		}
	}
	return nil
}

// Inspect reports what reconciliation would decide about every currently held
// lock, without changing anything. It is the read side the API/UI needs to say
// "this branch is held by workflow X, and here is why it is still held".
func (m *Manager) Inspect(ctx context.Context) ([]LockStatus, error) {
	locks, err := m.store.ListHeldBranchLocks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LockStatus, 0, len(locks))
	for _, lock := range locks {
		r, cerr := m.classifyLock(ctx, lock)
		if cerr != nil {
			return nil, cerr
		}
		out = append(out, LockStatus{Lock: lock, Retention: r})
	}
	return out, nil
}

// LockStatus pairs a held lock with the retention decision that currently
// applies to it.
type LockStatus struct {
	Lock      domain.BranchLock
	Retention Retention
}

// RecoverStale releases every lock held by one run that is stale under
// decideRetention, and reports how many it freed.
//
// This is the online half of stale-lock recovery. Boot reconciliation alone was
// not enough: the deadlock this checkpoint fixes appeared and persisted inside
// a single daemon lifetime, and "restart the daemon" is not a recovery path a
// user should have to find. Every waiting run calls it on the exact owner that
// blocked it, so the first workflow that actually needs the branch is the one
// that reclaims it — no sweep, no timer, no background scan.
func (m *Manager) RecoverStale(ctx context.Context, runID string) (int64, error) {
	if runID == "" {
		return 0, nil
	}
	locks, err := m.store.ListHeldBranchLocksByRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	now := m.clock()
	var released int64
	for _, lock := range locks {
		r, cerr := m.classifyLock(ctx, lock)
		if cerr != nil {
			return released, cerr
		}
		if r.Decision != RetentionRelease {
			continue
		}
		if err := m.apply(ctx, lock, r, now); err != nil {
			return released, err
		}
		released++
		if m.log != nil {
			m.log.Info("branchlock: released stale lock", "lock", lock.ID, "run", runID, "branch", lock.Branch, "reason", r.Reason)
		}
	}
	return released, nil
}
