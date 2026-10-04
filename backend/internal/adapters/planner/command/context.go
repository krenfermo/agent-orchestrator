package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const maxContextDocumentBytes = 48 * 1024

// contextDocumentNames is the planner's document set.
//
// It is a package-level list rather than a literal inside Build because the
// digest-only manifest path (manifest.go) has to cover exactly the same
// documents in exactly the same order: a manifest built from a different set,
// or the same set in a different order, would compare unequal to a stored one
// and report drift that did not happen.
var contextDocumentNames = []string{
	"AGENTS.md", "README.md", "go.mod", "package.json",
	"docs/architecture.md", "docs/STATUS.md",
}

// gitProbe returns a function that runs one git command in the project and
// yields its trimmed output, or the empty string when it cannot.
//
// An unreadable probe returns empty rather than failing the build: a project
// that is not a git checkout still has planning documents, and refusing to
// plan for it because `git rev-parse` failed would be worse than planning
// without a commit stamp.
func gitProbe(ctx context.Context, dir string) func(args ...string) string {
	return func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		b, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
}

// ContextBuilder assembles the planner's PlannerContext from a project on
// disk. It holds no state: everything it needs comes from the project record
// and the filesystem, so one value is safe to share.
type ContextBuilder struct{}

// Build reads the project's planning documents and returns the context the
// planner prompt is rendered from, stamped with PlannerContextVersion so a
// stored manifest stays interpretable after the format changes.
func (ContextBuilder) Build(ctx context.Context, project domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	out := workflowcore.PlannerContext{Version: workflowcore.PlannerContextVersion, ProjectID: project.ID, ProjectPath: project.Path, Documents: []workflowcore.PlannerDocument{}}
	git := gitProbe(ctx, project.Path)
	out.Branch = git("branch", "--show-current")
	out.HeadSHA = git("rev-parse", "HEAD")
	out.Dirty = git("status", "--porcelain") != ""
	for _, name := range contextDocumentNames {
		path := filepath.Join(project.Path, filepath.FromSlash(name))
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if len(b) > maxContextDocumentBytes {
			b = b[:maxContextDocumentBytes]
		}
		sum := sha256.Sum256(b)
		out.Documents = append(out.Documents, workflowcore.PlannerDocument{Path: name, SHA256: hex.EncodeToString(sum[:]), Content: string(b)})
	}
	return out, nil
}
