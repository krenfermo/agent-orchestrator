package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// manifest.go — building the planner's context manifest without re-reading the
// project (P2-B §5).
//
// The plan-reuse assessment does not need the planner's documents. It needs
// their DIGESTS, to compare against the manifest a plan was recorded with. The
// P2-A audit found it obtaining them the expensive way — a full Build, six
// files read with their bodies — and then throwing the bodies away. Because the
// assessment is re-derived on demand and is reachable from an HTTP read path,
// that is the most frequently repeated scan in the system.
//
// Project memory already hashed those files during indexing. MemoryBackedBuilder
// asks the digest ledger first and reads only what the ledger cannot answer.
//
// Two things it deliberately does NOT do:
//
//   - It does not change what a planner is sent. BuildManifest is a
//     digest-only path used by the drift comparison; Build still returns full
//     documents, because a planner genuinely needs their contents.
//   - It does not trust the ledger further than it can prove. The ledger
//     answers only when memory is indexed at exactly the commit being asked
//     about and the file is small enough that the two digests cover the same
//     bytes (see projectmemory.DigestsFor). Everything else falls through to a
//     real read, which is the pre-P2-B behaviour for that path.

// MemoryDigests is the slice of the memory service this builder needs. It is
// declared here, at the consumer, so the planner adapter depends on a question
// rather than on the memory subsystem's whole surface.
type MemoryDigests interface {
	DigestsFor(
		ctx context.Context, projectID domain.ProjectID, repoPath, commit string,
		paths []string, maxComparableBytes int64,
	) projectmemory.DocumentDigests
}

// MemoryBackedBuilder is a ContextBuilder that answers digest-only requests
// from project memory when it can prove the answer.
//
// A nil Memory makes it behave exactly as the plain ContextBuilder, which is
// what a daemon with project memory switched off gets.
type MemoryBackedBuilder struct {
	// Memory answers digest questions. Nil disables the optimisation.
	Memory MemoryDigests
	// Builder does the real reading. The zero value is the plain builder.
	Builder ContextBuilder
}

// Build returns the full planner context, documents and all. It is unchanged:
// a planner needs the contents, and there is nothing to optimise away.
func (b MemoryBackedBuilder) Build(ctx context.Context, project domain.ProjectRecord) (workflowcore.PlannerContext, error) {
	return b.Builder.Build(ctx, project)
}

// ManifestStats reports what one BuildManifest call cost, so the saving is
// measurable rather than asserted.
type ManifestStats struct {
	// DocumentsRequested is the fixed document set the planner context covers.
	DocumentsRequested int
	// DigestsFromMemory is how many were answered without opening a file.
	DigestsFromMemory int
	// DocumentsRead is how many still had to be read from disk.
	DocumentsRead int
	// BytesRead is what those reads cost.
	BytesRead int
	// MemoryUsable reports whether the ledger could be consulted at all, and
	// MemoryReason says why not when it could not.
	MemoryUsable bool
	MemoryReason string
}

// BuildManifest returns the planner context with every digest resolved and NO
// document bodies.
//
// This is what the drift comparison needs: the manifest it compares is
// content-free by construction (GeneratePlan blanks the bodies before storing
// it), so producing bodies only to discard them is pure cost.
//
// The returned context is byte-identical, after marshalling, to what
// Build-then-blank produces for the same tree — that equivalence is what makes
// it safe to substitute, and it is pinned by test.
func (b MemoryBackedBuilder) BuildManifest(
	ctx context.Context, project domain.ProjectRecord,
) (workflowcore.PlannerContext, error) {
	out, _, err := b.BuildManifestWithStats(ctx, project)
	return out, err
}

// BuildManifestWithStats is BuildManifest plus what it cost, for the baseline
// measurement and for `ao memory report`. It is a separate entry point so the
// interface the workflow package consumes stays two-valued and free of
// measurement concerns.
func (b MemoryBackedBuilder) BuildManifestWithStats(
	ctx context.Context, project domain.ProjectRecord,
) (workflowcore.PlannerContext, ManifestStats, error) {
	out := workflowcore.PlannerContext{
		Version:     workflowcore.PlannerContextVersion,
		ProjectID:   project.ID,
		ProjectPath: project.Path,
		Documents:   []workflowcore.PlannerDocument{},
	}
	git := gitProbe(ctx, project.Path)
	out.Branch = git("branch", "--show-current")
	out.HeadSHA = git("rev-parse", "HEAD")
	out.Dirty = git("status", "--porcelain") != ""

	stats := ManifestStats{DocumentsRequested: len(contextDocumentNames)}

	var known projectmemory.DocumentDigests
	if b.Memory != nil {
		known = b.Memory.DigestsFor(ctx, domain.ProjectID(project.ID), project.Path, out.HeadSHA,
			contextDocumentNames, maxContextDocumentBytes)
		stats.MemoryUsable, stats.MemoryReason = known.Usable, known.Reason
	}

	for _, name := range contextDocumentNames {
		if digest, ok := known.DigestOf(name); ok {
			// The ledger proved this file's content at this commit. A document
			// entry with a digest and no body is exactly what the manifest
			// stores, so nothing is lost by not opening it.
			stats.DigestsFromMemory++
			out.Documents = append(out.Documents, workflowcore.PlannerDocument{Path: name, SHA256: digest})
			continue
		}
		path := filepath.Join(project.Path, filepath.FromSlash(name))
		body, err := os.ReadFile(path) //nolint:gosec // name comes from the fixed contextDocumentNames list
		if err != nil {
			// A document that is not there is absent from the manifest, which
			// is what Build does too — its absence is itself drift evidence.
			continue
		}
		if len(body) > maxContextDocumentBytes {
			body = body[:maxContextDocumentBytes]
		}
		stats.DocumentsRead++
		stats.BytesRead += len(body)
		sum := sha256.Sum256(body)
		out.Documents = append(out.Documents, workflowcore.PlannerDocument{
			Path: name, SHA256: hex.EncodeToString(sum[:]),
		})
	}
	return out, stats, nil
}

// Compile-time proof that the memory-backed builder satisfies both the plain
// context contract and the optional digest-only one. If either signature
// drifts this fails the build, rather than the workflow package silently
// falling back to the expensive path.
var (
	_ workflowcore.PlannerContextBuilder  = MemoryBackedBuilder{}
	_ workflowcore.PlannerManifestBuilder = MemoryBackedBuilder{}
)
