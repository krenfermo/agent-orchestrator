// Package repoaccess is the one place AO decides which repository files its
// derived-knowledge indexers may look at, and how they may read them.
//
// Frente 3 / 3B. Before this package the code graph and project memory each
// walked the filesystem with their own skip lists, their own secret rules and
// their own read path, and the three disagreed in ways that mattered:
//
//   - the code graph did not skip `.claude/`, so a linked git worktree an agent
//     had checked out inside a project (MEDUSA: `.claude/worktrees/…`) was
//     indexed as that project's own code — 24.7% of its served symbols;
//   - project memory's incremental path used os.Stat + os.ReadFile, which
//     follow symlinks, so a committed `CLAUDE.md -> ~/.aws/credentials` named
//     in a diff would have been read and stored;
//   - project memory had no secret rule before reading at all: `.env` and key
//     files were read and hashed, and only an accident of its role tables kept
//     their content out of items.
//
// The contract, in three parts:
//
//  1. ELIGIBILITY (eligible.go). In a git repository the candidate set is the
//     files git tracks — `git ls-files`, nothing the filesystem merely
//     happens to contain. Untracked scratch, ignored build output, nested
//     checkouts and linked worktrees are therefore out by construction, not
//     by a name list. Outside git, a filesystem walk applies the same
//     exclusions and refuses any directory that is itself a checkout.
//  2. SECRETS (secret.go). A path that holds secret material by convention is
//     refused BEFORE it is opened. Nothing reads a file to decide afterwards
//     that it was secret.
//  3. CONFINED READS (read.go). A read goes through os.Root, refuses a symlink
//     at ANY component of the path (file, directory, chain, dangling), refuses
//     anything that is not a regular file or is over the size cap, and never
//     leaves the canonical root.
//
// Every refusal is a normal outcome with a stated reason, never a crash and
// never a silent partial read: an indexer skips the path and counts it.
package repoaccess

import "errors"

// Refusal reasons. Callers match them with errors.Is; each is a "skip this
// path" outcome, not a failure of the pass.
var (
	// ErrEscapesRoot: the path is absolute, contains "..", or otherwise names
	// something outside the root.
	ErrEscapesRoot = errors.New("repoaccess: path escapes the repository root")
	// ErrSymlink: a component of the path is a symbolic link. AO does not
	// follow repository symlinks, inside or outside the root.
	ErrSymlink = errors.New("repoaccess: path traverses a symbolic link")
	// ErrSecretPath: the path holds secret material by convention and is never
	// opened.
	ErrSecretPath = errors.New("repoaccess: path is a secret by convention")
	// ErrExcluded: the path lies under a directory AO never indexes (VCS
	// internals, dependency trees, build output, agent scratch space).
	ErrExcluded = errors.New("repoaccess: path is under an excluded directory")
	// ErrNotRegular: the path is a directory, device, socket or similar.
	ErrNotRegular = errors.New("repoaccess: path is not a regular file")
	// ErrTooLarge: the file exceeds the caller's size cap.
	ErrTooLarge = errors.New("repoaccess: file exceeds the size cap")
	// ErrNotExist: the path does not exist (a normal outcome for a stale diff).
	ErrNotExist = errors.New("repoaccess: path does not exist")
)

// IsRefusal reports whether err is one of the policy refusals above -- i.e. a
// "skip this path" outcome rather than an I/O failure the pass must surface.
func IsRefusal(err error) bool {
	for _, r := range []error{ErrEscapesRoot, ErrSymlink, ErrSecretPath, ErrExcluded, ErrNotRegular, ErrTooLarge, ErrNotExist} {
		if errors.Is(err, r) {
			return true
		}
	}
	return false
}
