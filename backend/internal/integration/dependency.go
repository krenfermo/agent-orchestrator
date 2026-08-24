package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Dependency-aware integration.
//
// Parallel execution is allowed to start B before A has landed -- that is the
// whole point of it -- but it is never allowed to LAND B against a target that
// does not already contain A. The two are different guarantees and only the
// second one is this package's:
//
//   - "B may start" is the scheduler's decision, made from the DAG and the
//     conflict map. It is speculative on purpose: B's worktree is cut from a
//     base that will be behind by the time B finishes.
//   - "B may land" is decided here, under the lane, against the target as it
//     actually is. A dependency that has not integrated is not a failure and
//     not a conflict: it is simply not B's turn yet, and B waits.
//
// The check is deliberately made from COMMITS rather than from task states. A
// task row saying "A completed" is a statement about a run; the guarantee that
// has to hold before the ref moves is "the commit A's integration left on this
// ref is still reachable from it", and only the ref can answer that.

// Dependency is one sibling task whose work must already be on the target ref
// before this task's work may land on it.
//
// It is the caller's read of its own records, not an instruction: the caller
// says which task it depends on and where that task's integration left the
// target, and this package proves the second claim against the ref itself.
type Dependency struct {
	// TaskID is the dependency's task id, used only to say which dependency is
	// unmet -- a reason with no name in it cannot be acted on.
	TaskID string
	// IntegratedSHA is the commit that dependency's own integration left the
	// target ref at. Empty means it has not integrated yet, which is a wait
	// rather than a refusal.
	IntegratedSHA string
}

// ErrDependencyPending means a task this one requires has not been integrated
// yet.
//
// It is an error rather than an Attention for exactly the reason ErrLockBusy is:
// nothing is wrong, nobody has to do anything, and recording an attempt would
// put a row in the ledger for an integration that never started. The caller
// leaves the task where it is and tries again once the dependency lands.
var ErrDependencyPending = errors.New("integration: a required dependency has not been integrated yet")

// pendingDependency returns the first dependency that has not integrated at
// all. It runs BEFORE the lane is entered: a task that cannot land yet must not
// occupy the lane while finding that out, and must not have read a target head
// it is not entitled to act on.
func pendingDependency(deps []Dependency) (Dependency, bool) {
	for _, dep := range deps {
		if strings.TrimSpace(dep.IntegratedSHA) == "" {
			return dep, true
		}
	}
	return Dependency{}, false
}

// dependencyGate proves, inside the lane, that every dependency's integrated
// commit is still reachable from the target this integration is about to move.
//
// A dependency whose commit is NOT reachable is the one case here that needs a
// person: the dependency did integrate, and something has since rewritten the
// target so that its work is no longer on it. Landing B on such a target would
// silently produce the exact state this whole mechanism exists to prevent -- B
// integrated against a target that excludes a dependency it required.
func (c *Coordinator) dependencyGate(ctx context.Context, req Request, targetBefore string, targetExists bool) (*Attention, error) {
	if len(req.Dependencies) == 0 {
		return nil, nil
	}
	var missing []string
	for _, dep := range req.Dependencies {
		sha := strings.TrimSpace(dep.IntegratedSHA)
		if sha == "" {
			// Already refused before the lane; reaching here would mean the
			// caller changed the request underneath us.
			return nil, fmt.Errorf("%w: %s", ErrDependencyPending, dep.TaskID)
		}
		if !targetExists {
			missing = append(missing, describeDependency(dep))
			continue
		}
		// A commit the repository no longer has is as absent from the target as
		// one that was never on it, and it is a fact about this task rather than
		// about the coordinator -- so it becomes an attention here instead of an
		// error that would park the whole objective.
		known, exists, err := c.git.ResolveCommitIfExists(ctx, req.RepoPath, sha)
		if err != nil {
			return nil, err
		}
		if !exists {
			missing = append(missing, describeDependency(dep)+" — that commit is not in the repository any more")
			continue
		}
		contained, err := c.git.IsAncestor(ctx, req.RepoPath, known, targetBefore)
		if err != nil {
			return nil, err
		}
		if !contained {
			missing = append(missing, describeDependency(dep))
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	sort.Strings(missing)
	return &Attention{
		Reason:    ReasonDependencyMissingFromTarget,
		TargetSHA: targetBefore,
		Detail: "the target no longer contains work this task requires, so integrating onto it would " +
			"produce a target that excludes an already-integrated dependency: " + strings.Join(missing, ", "),
	}, nil
}

// dependencyTaskIDs is the record's account of what this integration proved.
func dependencyTaskIDs(deps []Dependency) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		out = append(out, dep.TaskID)
	}
	sort.Strings(out)
	return out
}

func describeDependency(dep Dependency) string {
	return fmt.Sprintf("%s (integrated as %s)", dep.TaskID, shortID(dep.IntegratedSHA))
}

func shortID(v string) string {
	if len(v) > 12 {
		return v[:12]
	}
	if v == "" {
		return "(none)"
	}
	return v
}
