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
