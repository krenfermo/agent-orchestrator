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

// ContextBuilder assembles the planner's PlannerContext from a project on
// disk. It holds no state: everything it needs comes from the project record
// and the filesystem, so one value is safe to share.
type ContextBuilder struct{}

// Build reads the project's planning documents and returns the context the
// planner prompt is rendered from, stamped with PlannerContextVersion so a
// stored manifest stays interpretable after the format changes.
func (ContextBuilder) Build(ctx context.Context, project domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	out := workflowcore.PlannerContext{Version: workflowcore.PlannerContextVersion, ProjectID: project.ID, ProjectPath: project.Path, Documents: []workflowcore.PlannerDocument{}}
	git := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = project.Path
		b, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	out.Branch = git("branch", "--show-current")
	out.HeadSHA = git("rev-parse", "HEAD")
	out.Dirty = git("status", "--porcelain") != ""
	for _, name := range []string{"AGENTS.md", "README.md", "go.mod", "package.json", "docs/architecture.md", "docs/STATUS.md"} {
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
