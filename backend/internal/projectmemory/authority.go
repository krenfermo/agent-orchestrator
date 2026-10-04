package projectmemory

import (
	stdctx "context"
	"sort"
	"strings"
)

// authority.go — carrying one execution's sharing entitlement to the boundary
// that assembles its context (P2-C §14, §15).
//
// The two halves of a shared-knowledge decision live in different packages by
// design. WHICH tasks a dispatch may read the unintegrated knowledge of is a
// workflow fact — it comes from workflow_task_dependencies and from the run
// this task belongs to, and nothing in the memory package could derive it
// without re-implementing the plan. WHAT to do with that entitlement is a
// memory fact. Neither package should learn the other's model to connect them.
//
// So the entitlement travels on the context, exactly as the routing and memory
// metrics already travel back the other way. The workflow coordinator stamps
// it on the context before it dispatches; the wfmemory wrappers read it when
// they provision. A dispatch that carries no authority provisions with none,
// and the pack falls back to canonical knowledge alone.
//
// That fallback direction is the whole safety argument. A missing entitlement
// makes a task learn LESS than it could; there is no value of this type, and
// no way of failing to set it, that makes a task learn more than it may. A
// sibling that never declared a dependency names nobody, so it is entitled to
// nobody's unintegrated work however much its file set overlaps.

// TaskAuthority is what one dispatch is entitled to read.
type TaskAuthority struct {
	// TaskRef is the task being dispatched. Its own task-local knowledge is
	// always readable by it.
	TaskRef string
	// WorkflowRunID is the run it belongs to. Workflow-local knowledge is
	// scoped by it, so a later, unrelated run that happens to name the same
	// upstream task reads nothing.
	WorkflowRunID string
	// UpstreamTaskRefs are the tasks this one explicitly depends on. They come
	// from the durable plan, never from "these two tasks touched the same
	// files" — an inferred dependency is exactly the invented relationship
	// P2-C §5 forbids, and here it would also be a safety hole.
	UpstreamTaskRefs []string
}

// Empty reports whether the authority names nothing at all.
func (a TaskAuthority) Empty() bool {
	return strings.TrimSpace(a.TaskRef) == "" &&
		strings.TrimSpace(a.WorkflowRunID) == "" &&
		len(a.UpstreamTaskRefs) == 0
}

// Normalized trims the refs and puts them in a stable order, so two dispatches
// with the same entitlement produce the same cache key.
func (a TaskAuthority) Normalized() TaskAuthority {
	a.TaskRef = strings.TrimSpace(a.TaskRef)
	a.WorkflowRunID = strings.TrimSpace(a.WorkflowRunID)
	seen := make(map[string]struct{}, len(a.UpstreamTaskRefs))
	refs := make([]string, 0, len(a.UpstreamTaskRefs))
	for _, ref := range a.UpstreamTaskRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" || ref == a.TaskRef {
			continue
		}
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	a.UpstreamTaskRefs = refs
	return a
}

type authorityContextKey struct{}

// WithTaskAuthority returns a context carrying one dispatch's entitlement.
func WithTaskAuthority(ctx stdctx.Context, a TaskAuthority) stdctx.Context {
	return stdctx.WithValue(ctx, authorityContextKey{}, a.Normalized())
}

// TaskAuthorityFrom reads the entitlement a coordinator stamped, if any. The
// zero value is a complete answer: it entitles the dispatch to canonical
// knowledge and nothing else.
func TaskAuthorityFrom(ctx stdctx.Context) TaskAuthority {
	a, _ := ctx.Value(authorityContextKey{}).(TaskAuthority)
	return a
}

// --- role head (P2-D §17) ---------------------------------------------------
//
// A reviewer judges a specific commit, and the pack it was given was assembled
// at a specific memory generation. When those two disagree -- the pack was
// built for SHA A and the reviewer ended up judging SHA B -- the reviewer's
// verdict is about work it was not briefed on, and until P2-D nothing recorded
// enough to tell.
//
// The head travels on the context for the same reason the sharing entitlement
// does: the dispatch boundary knows it, the memory boundary needs it, and
// neither should have to learn the other's model. It widens no launch port,
// which matters because ReviewerLaunchRequest is a contract several launchers
// implement.
//
// It is recorded and used for nothing else. Memory selection is deliberately
// NOT narrowed by it: a reviewer given a different pack from the worker whose
// work it is reviewing is the P2-C §7 problem, and narrowing here would
// recreate it. What this buys is a manifest that can be compared.

type roleHeadContextKey struct{}

// WithRoleHead returns a context carrying the commit a dispatch is reasoning
// about.
func WithRoleHead(ctx stdctx.Context, sha string) stdctx.Context {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		// An empty head is the zero value, and stamping it would only add a
		// context frame that says nothing.
		return ctx
	}
	return stdctx.WithValue(ctx, roleHeadContextKey{}, sha)
}

// RoleHeadFrom reads the commit a dispatch is reasoning about, if a boundary
// stamped one. The empty string is a complete answer: a planner is not
// reasoning about one commit.
func RoleHeadFrom(ctx stdctx.Context) string {
	sha, _ := ctx.Value(roleHeadContextKey{}).(string)
	return sha
}

// --- task changed paths (P2-E A4) -------------------------------------------
//
// The files a task has already rewritten in its own workspace, travelling the
// same way the sharing entitlement and the role head do: the workflow boundary
// knows them, the memory boundary needs them, and neither should have to learn
// the other's model.
//
// They are an EXCLUSION and never a relevance hint. See PackRequest's
// TaskChangedPaths for the rule and for why it is "every source path" rather
// than "any".

type taskChangedPathsContextKey struct{}

// WithTaskChangedPaths returns a context carrying the paths this execution's
// task has rewritten but not integrated.
func WithTaskChangedPaths(ctx stdctx.Context, paths []string) stdctx.Context {
	if len(paths) == 0 {
		return ctx
	}
	return stdctx.WithValue(ctx, taskChangedPathsContextKey{}, append([]string(nil), paths...))
}

// TaskChangedPathsFrom reads those paths, if a boundary stamped any. Nil is a
// complete answer and excludes nothing, which is the pre-P2-E behaviour.
func TaskChangedPathsFrom(ctx stdctx.Context) []string {
	paths, _ := ctx.Value(taskChangedPathsContextKey{}).([]string)
	return paths
}
