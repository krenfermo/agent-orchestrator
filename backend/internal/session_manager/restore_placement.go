package sessionmanager

import (
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// restore_placement.go — a restored session goes back where it was, not where
// the project would put it today.
//
// A spawn carries its run's FROZEN execution placement into the workspace
// router (ports.WorkspaceConfig.Placement), so the router honours what was
// decided for THAT obligation rather than the project's current execution
// mode. Restore did not. It built its WorkspaceConfig without a placement at
// all, and the empty value means "route by project configuration".
//
// That is the freeze losing to config drift on the one path where drift is most
// likely, because the gap between a spawn and a restore is a daemon restart and
// often a person changing settings in between:
//
//	a run is frozen into isolated_worktree and writes code in its worktree
//	  -> the project is switched to direct_branch
//	  -> the daemon restarts
//	  -> restore asks the PROJECT, gets the direct-branch adapter, and
//	     re-attaches the session to the operator's own checkout on the
//	     operator's own branch -- while the worktree holding the work sits
//	     untouched and unreferenced
//
// The placement does not have to be guessed to close this, and it must not be:
// it is already recorded, in the two paths the session itself was created with.
// An AO worktree has a workspace path that is NOT the repository; a
// direct-branch session's workspace IS the repository. Both adapters write both
// values at create time, so the comparison is a durable fact about what was
// actually built rather than a re-derivation of policy.

// PlacementFromSessionFacts reports the execution placement a session was
// actually created in, derived from the paths recorded when it was.
//
// Two durable facts can answer it, and both are compared against the same
// thing: the workspace AO recorded for this session.
//
//   - WorkspaceRepoPath, the repository the workspace was cut from. Written by
//     both adapters at create time. When the workspace IS that repository, the
//     session is direct-branch; a worktree can never be, because it lives under
//     the managed root by construction.
//   - projectPath, the registered repository, as the fallback for a session
//     written before that metadata was reliably stored. It answers the same
//     question against the same workspace, so it is not a weaker guess -- just
//     a fact read from the project row instead of the session row.
//
// It returns the empty placement -- the router's "decide by project" value --
// only when neither comparison can be made. That is an honest "AO cannot tell
// from what it stored", and it leaves such a session with exactly the behaviour
// it has always had rather than inventing a placement for it.
func PlacementFromSessionFacts(meta domain.SessionMetadata, projectPath string) domain.ExecutionPlacementType {
	workspace := strings.TrimSpace(meta.WorkspacePath)
	if workspace == "" {
		return ""
	}
	repo := strings.TrimSpace(meta.WorkspaceRepoPath)
	if repo == "" {
		repo = strings.TrimSpace(projectPath)
	}
	if repo == "" {
		return ""
	}
	if samePath(workspace, repo) {
		// The session's workspace is the registered repository itself. Only a
		// direct-branch create produces that.
		return domain.PlacementDirectBranch
	}
	return domain.PlacementIsolatedWorktree
}

// samePath compares two recorded paths as paths rather than as bytes: a
// trailing separator or a `.` segment names the same directory, and reading it
// as a different one would send a direct-branch session through the worktree
// adapter.
//
// Symlinks are resolved when both sides still exist, because macOS hands out
// /var paths that git reports back as /private/var -- so a session created
// through one spelling and restored through the other would otherwise look like
// two different directories. A path that no longer exists falls back to the
// lexical comparison, which is the honest answer for a checkout that is gone.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, aerr := filepath.EvalSymlinks(a)
	rb, berr := filepath.EvalSymlinks(b)
	if aerr != nil || berr != nil {
		return false
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}
