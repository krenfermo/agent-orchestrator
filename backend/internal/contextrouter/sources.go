package contextrouter

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/codegraph"
	memory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// DiffSource reports the change set a payload should be routed around. It is
// an interface rather than a git call so a caller that already knows what
// changed — a workflow step that just applied a patch, a test — can say so
// without the router shelling out.
type DiffSource interface {
	// Changes returns the file-level change set for a project. An empty diff
	// is a valid answer, not an error: a task that has not touched anything
	// yet is routed on its documents, graph, and memory alone.
	Changes(ctx context.Context, project Project) (codegraph.Diff, error)
}

// GraphQuerier is the read-only slice of codegraph.CodeGraphProvider the
// router uses. It is narrowed on purpose: routing must never index, update, or
// otherwise mutate a project's graph as a side effect of assembling a payload,
// and a type that cannot express those verbs cannot accidentally perform them.
// Every CodeGraphProvider satisfies it.
type GraphQuerier interface {
	Query(ctx context.Context, req codegraph.QueryRequest) (codegraph.QueryResult, error)
}

// MemorySource reads a project's durable memory. It is satisfied as-is by
// *projectmemory.Store, so the router adds no storage code of its own.
type MemorySource interface {
	// List returns every stored item for a project, including stale ones. The
	// router filters and ranks; it does not ask the store to.
	List(project string) ([]memory.MemoryItem, error)
}

// GitDiffSource answers Changes by asking git what the working tree has that
// the base ref does not. It is read-only: one `git diff --name-status`, parsed
// by codegraph's own porcelain parser rather than a second copy of it.
type GitDiffSource struct {
	// Binary is the git executable. Empty means "git" from PATH.
	Binary string
}

// NewGitDiffSource returns a diff source backed by the git binary.
func NewGitDiffSource() *GitDiffSource { return &GitDiffSource{} }

// Changes runs `git diff --name-status --find-renames <base>` in the project
// root. A root that is not an absolute path is refused rather than resolved
// against the daemon's working directory, which is the same rule the code
// graph and the memory store apply to their own roots.
func (g *GitDiffSource) Changes(ctx context.Context, project Project) (codegraph.Diff, error) {
	root := strings.TrimSpace(project.Root)
	if root == "" {
		return codegraph.Diff{}, fmt.Errorf("contextrouter: diff needs a project root")
	}
	if !filepath.IsAbs(root) {
		return codegraph.Diff{}, fmt.Errorf("contextrouter: project root %q must be absolute", root)
	}
	base := strings.TrimSpace(project.BaseRef)
	if base == "" {
		base = "HEAD"
	}
	binary := strings.TrimSpace(g.Binary)
	if binary == "" {
		binary = "git"
	}
	cmd := exec.CommandContext(ctx, binary, "-C", filepath.Clean(root), "diff", "--name-status", "--find-renames", base, "--") //nolint:gosec // fixed read-only argv; base is a caller-supplied ref passed as one argument, never a shell string
	out, err := cmd.Output()
	if err != nil {
		return codegraph.Diff{}, fmt.Errorf("contextrouter: git diff in %s: %w", root, err)
	}
	return codegraph.ParseGitNameStatus(string(out))
}
