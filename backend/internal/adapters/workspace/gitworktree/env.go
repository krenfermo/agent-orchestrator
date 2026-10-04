package gitworktree

import "os"

// gitEnv returns the environment every git invocation in this package runs
// with: the caller's own environment plus a pinned C message locale.
//
// git translates its diagnostics. Several decisions here are made by matching
// those diagnostics -- "is a missing but already registered worktree" selects
// the stale-registration recovery in addNewBranchWorktree, "cannot remove a
// locked working tree" distinguishes a locked worktree from a dirty one, and
// "not a git repository" distinguishes an unregistered path from a real git
// failure. On a machine whose locale is not English git says the same things
// in another language ("es un árbol de trabajo faltante pero ya registrado"),
// every one of those matches fails, and the recovery paths silently stop
// existing: the worktree add fails outright, a locked worktree is reported as
// dirty, and so on. The behavior depended on the operator's LANG.
//
// Pinning LC_ALL is the fix git itself documents for scripted callers. LANGUAGE
// is cleared as well because gettext consults it FIRST and it would otherwise
// re-translate the messages LC_ALL just pinned.
//
// This governs git's messages only. Repository content, refs, and porcelain
// output are byte-exact either way, so no parsed data changes -- only the
// language of the prose around it.
func gitEnv(extra ...string) []string {
	env := os.Environ()
	env = append(env, "LC_ALL=C", "LANGUAGE=")
	return append(env, extra...)
}
