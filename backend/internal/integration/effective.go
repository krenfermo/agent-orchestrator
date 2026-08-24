package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Whether a replay left a task's approval still describing the task's work.
//
// A rebase is not a neutral operation on a review. The reviewer approved ONE
// change -- the diff the task contributes -- and a replay onto a moved target
// can leave that diff identical or can change it, and the two cases deserve
// opposite answers:
//
//   - Identical. The reviewer would read exactly the same change on the new
//     target. Re-asking them would be asking the same question twice, so the
//     approval is reused and only the verification is re-run, against content
//     no verification has seen.
//   - Different. The change that would land is not the change that was
//     approved. Reusing the approval would be AO certifying work no reviewer
//     read, which is the same defect verify_workspace_changed exists to
//     prevent -- and it gets the same remedy: go and ask for a fresh review of
//     what is actually there, rather than crediting the old verdict or parking
//     forever.
//
// "The same change" is the lines this task adds and removes, in the files it
// touches: git's patch-identity rule, narrowed by one more step. Hunk line
// numbers and blob ids are dropped because they move whenever a change is
// replayed anywhere. Context is dropped too, and that one is a judgement rather
// than a mechanic: the code around this task's change is, after a replay,
// precisely the dependency's work -- already reviewed on its own way in, and
// not something this task's reviewer is being asked about again. What is left
// is exactly what this task contributes, which is what the approval was for.

// effectiveFingerprints returns the identity of what this task contributes,
// before the replay and after it.
//
// Before is the task's change relative to the base its history actually
// diverged at, which is the change its review was given for. After is the same
// task's change relative to the target it was just replayed onto. Both are read
// from the repository rather than the worktree: a worktree shares the
// repository's object database, so the replayed commit is visible from either,
// and reading from the repository keeps this an entirely read-only step.
func (c *Coordinator) effectiveFingerprints(ctx context.Context, req Request, targetBefore, sourceSHA, replayed string) (before, after string, err error) {
	base, err := c.git.MergeBase(ctx, req.RepoPath, targetBefore, sourceSHA)
	if err != nil {
		return "", "", err
	}
	if base == "" {
		// Unrelated histories never reach here: selectStrategy already turned
		// that into an attention. Guarding anyway means a future caller cannot
		// get a fingerprint computed against a base that does not exist.
		return "", "", fmt.Errorf("integration: no common ancestor of %s and %s to compute an effective change against", targetBefore, sourceSHA)
	}
	before, err = c.git.PatchIdentity(ctx, req.RepoPath, base, sourceSHA)
	if err != nil {
		return "", "", err
	}
	after, err = c.git.PatchIdentity(ctx, req.RepoPath, targetBefore, replayed)
	if err != nil {
		return "", "", err
	}
	return before, after, nil
}

// reviewSurvivesReplay reports whether the approval this task carries still
// describes what the replay produced, and why not when it does not.
//
// A review that was SKIPPED by policy has no approval to invalidate, so a
// changed diff changes nothing about it. That asymmetry is deliberate: this
// mechanism protects an approval, and where there is none there is nothing to
// protect.
func reviewSurvivesReplay(review ReviewState, before, after string) (bool, string) {
	if before == after {
		return true, ""
	}
	if review != ReviewApproved {
		return true, ""
	}
	return false, fmt.Sprintf(
		"the review approved the change %s and replaying it onto the current target produced %s",
		shortID(before), shortID(after))
}

// patchIdentity hashes a diff after removing everything in it that describes
// WHERE the change sits rather than WHAT it changes.
//
// Two lines are dropped. A hunk header carries the line numbers the change
// landed on, which every replay onto a moved target changes and no reviewer
// reads as part of the change. An `index` line carries the blob ids on either
// side, which differ whenever anything else in the file differs -- including
// changes that belong to the dependency that was integrated first, not to this
// task. The caller asks git for the diff with no context (-U0), so what reaches
// here is the added and removed lines and the file headers naming them.
//
// This is git patch-id's rule, computed here rather than by piping a diff
// through another process: the answer then depends on this function alone
// instead of on a second program's output format.
func patchIdentity(diff []byte) string {
	h := sha256.New()
	for _, line := range strings.Split(string(diff), "\n") {
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "index ") {
			continue
		}
		h.Write([]byte(line))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}
