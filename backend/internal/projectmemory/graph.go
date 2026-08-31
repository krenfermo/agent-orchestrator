package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// graph.go — the relationship port, and the local backend that is AO's
// default.
//
// The port exists so AO is never hard-wired to one graph tool. Grae, Graphify,
// an LSP-backed indexer or a hosted service can each be plugged in by
// implementing MemoryGraph; nothing above this boundary learns which one is
// behind it, and nothing above it may assume one is present at all.
//
// The rule that keeps it honest: **the local backend is not a placeholder.**
// LocalGraph, over the SQLite relations table, is the implementation AO ships
// and relies on. An external adapter is additive — it may make traversal
// richer, and it may never become the only place an edge lives. That is what
// makes "AO works even when Grae is unavailable" a structural property rather
// than a promise, and it is why UnavailableGraph below degrades to a warning
// and an empty result instead of an error.
//
// Connecting an external graph later:
//
//  1. Implement MemoryGraph over the external client. Map
//     domain.ProjectMemoryRelationKind onto the backend's own edge labels; do
//     not leak the backend's vocabulary upward.
//  2. Keep writing to LocalGraph as well — compose with TeeGraph. The local
//     table stays canonical, so a backend outage costs traversal quality and
//     never costs facts.
//  3. Register it where the service is constructed. Nothing else changes.
//
// As of P2-A no such adapter exists in this repository and none is vendored:
// the audit's search (docs/p2-project-memory-audit.md §4.1) found Graphify
// named only in prose and Grae not at all. Adding a fragile external
// dependency in order to claim an integration would be worse than the clean
// port this file defines.

// ErrGraphUnavailable is what an optional graph backend returns when it cannot
// be reached. Callers must treat it as a degradation, never as a failure of
// the operation they were performing.
var ErrGraphUnavailable = errors.New("projectmemory: graph backend unavailable")

// GraphDirection says which way a traversal runs.
type GraphDirection string

// Graph traversal directions.
const (
	// DirectionOut follows edges away from the node (what it depends on).
	DirectionOut GraphDirection = "out"
	// DirectionIn follows edges into the node (what depends on it).
	DirectionIn GraphDirection = "in"
	// DirectionBoth follows both.
	DirectionBoth GraphDirection = "both"
)

// GraphQuery is one bounded question about the graph.
//
// Every field that could make the answer unbounded has a cap, and the caps are
// enforced by the backend rather than trusted to the caller: a traversal on
// the dispatch path that can fan out over a whole repository is a traversal
// that will, on the one repository where it matters.
type GraphQuery struct {
	ProjectID domain.ProjectID
	RepoID    string
	// Node is where the traversal starts.
	Node domain.ProjectMemoryNode
	// Direction is which way it runs. Empty means DirectionOut.
	Direction GraphDirection
	// Kinds narrows to specific edge kinds. Empty means every kind.
	Kinds []domain.ProjectMemoryRelationKind
	// Depth is how many hops to follow. Zero or negative means one, and
	// anything above MaxGraphDepth is clamped to it.
	Depth int
	// Limit caps the edges returned. Zero or negative means
	// DefaultGraphLimit; anything above MaxGraphLimit is clamped.
	Limit int
	// IncludeStale asks for edges that are no longer authoritative. It is for
	// operator inspection; context assembly never sets it.
	IncludeStale bool
}

// Traversal bounds. They are deliberately small: the graph exists to answer
// "what is next to this", not to be walked.
const (
	// DefaultGraphLimit is the edge cap when a query does not set one.
	DefaultGraphLimit = 64
	// MaxGraphLimit is the hard cap no query can exceed.
	MaxGraphLimit = 512
	// MaxGraphDepth is the hop cap. Three hops already crosses
	// file -> module -> module, which is as far as a bounded context pack can
	// afford to reason.
	MaxGraphDepth = 3
)

// Normalized clamps the query to its bounds and fills in the defaults.
func (q GraphQuery) Normalized() GraphQuery {
	q.Node = q.Node.Normalized()
	if q.Direction == "" {
		q.Direction = DirectionOut
	}
	if q.Depth <= 0 {
		q.Depth = 1
	}
	if q.Depth > MaxGraphDepth {
		q.Depth = MaxGraphDepth
	}
	if q.Limit <= 0 {
		q.Limit = DefaultGraphLimit
	}
	if q.Limit > MaxGraphLimit {
		q.Limit = MaxGraphLimit
	}
	return q
}

func (q GraphQuery) matchesKind(kind domain.ProjectMemoryRelationKind) bool {
	if len(q.Kinds) == 0 {
		return true
	}
	for _, k := range q.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// MemoryGraph is the relationship abstraction. It is intentionally three verbs
// wide, so an external backend can be wrapped without exposing its model.
//
// Implementation contract:
//
//   - Name is stable and appears in operator output.
//   - Upsert is idempotent. Writing the same edge twice leaves one edge.
//   - Neighbors is read-only and must never lazily rebuild anything. It must
//     honour the query's Limit and Depth, and must return edges in a
//     deterministic order — a context pack built twice from an unchanged graph
//     has to be byte-identical.
//   - A backend that cannot be reached returns ErrGraphUnavailable rather than
//     a backend-specific error, so callers can degrade on the cause.
type MemoryGraph interface {
	// Name identifies the implementation ("local", "grae", ...).
	Name() string
	// Upsert writes edges, idempotently.
	Upsert(ctx context.Context, now time.Time, rels ...domain.ProjectMemoryRelation) error
	// Neighbors answers one bounded traversal.
	Neighbors(ctx context.Context, q GraphQuery) ([]domain.ProjectMemoryRelation, error)
}

// LocalGraph is the in-tree MemoryGraph backed by the durable relations table.
// It is the default and the canonical store of edges.
type LocalGraph struct {
	repo Repository
}

// NewLocalGraph returns the default graph backend over a Repository.
func NewLocalGraph(repo Repository) *LocalGraph { return &LocalGraph{repo: repo} }

// Name identifies the local backend.
func (g *LocalGraph) Name() string { return "local" }

// Upsert writes edges through the repository's generation-conditioned CAS. A
// refused write (a stale generation) is not an error for the batch: it means a
// newer pass already asserted this edge, which is the outcome the fence is for.
func (g *LocalGraph) Upsert(ctx context.Context, now time.Time, rels ...domain.ProjectMemoryRelation) error {
	for _, rel := range rels {
		if _, err := g.repo.PutProjectMemoryRelation(ctx, rel, now); err != nil {
			if errors.Is(err, store.ErrProjectMemoryStaleGeneration) {
				continue
			}
			return fmt.Errorf("upsert relation %s: %w", rel.Normalized().ID, err)
		}
	}
	return nil
}

// Neighbors runs a bounded breadth-first traversal.
//
// It is breadth-first rather than depth-first so that a Limit truncates the
// far edge of the fan-out rather than one arbitrary branch: with a budget for
// twenty edges, twenty immediate neighbours are more useful than one chain of
// twenty.
func (g *LocalGraph) Neighbors(ctx context.Context, q GraphQuery) ([]domain.ProjectMemoryRelation, error) {
	q = q.Normalized()
	state := domain.MemoryStateValid
	if q.IncludeStale {
		state = domain.MemoryStateStale
	}

	seenEdge := map[string]struct{}{}
	seenNode := map[string]struct{}{q.Node.String(): {}}
	frontier := []domain.ProjectMemoryNode{q.Node}
	var out []domain.ProjectMemoryRelation

	for depth := 0; depth < q.Depth && len(frontier) > 0 && len(out) < q.Limit; depth++ {
		var next []domain.ProjectMemoryNode
		for _, node := range frontier {
			edges, err := g.edgesFor(ctx, q, node, state)
			if err != nil {
				return nil, err
			}
			for _, e := range edges {
				if !q.matchesKind(e.Kind) {
					continue
				}
				if _, dup := seenEdge[e.ID]; dup {
					continue
				}
				seenEdge[e.ID] = struct{}{}
				out = append(out, e)
				if len(out) >= q.Limit {
					break
				}
				for _, end := range []domain.ProjectMemoryNode{e.From, e.To} {
					if _, dup := seenNode[end.String()]; dup {
						continue
					}
					seenNode[end.String()] = struct{}{}
					next = append(next, end)
				}
			}
			if len(out) >= q.Limit {
				break
			}
		}
		frontier = next
	}

	sortRelations(out)
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (g *LocalGraph) edgesFor(
	ctx context.Context, q GraphQuery, node domain.ProjectMemoryNode, state domain.ProjectMemoryState,
) ([]domain.ProjectMemoryRelation, error) {
	var edges []domain.ProjectMemoryRelation
	if q.Direction == DirectionOut || q.Direction == DirectionBoth {
		out, err := g.repo.ListProjectMemoryRelationsFrom(ctx, q.ProjectID, q.RepoID, node, state)
		if err != nil {
			return nil, fmt.Errorf("traverse out of %s: %w", node, err)
		}
		edges = append(edges, out...)
	}
	if q.Direction == DirectionIn || q.Direction == DirectionBoth {
		in, err := g.repo.ListProjectMemoryRelationsTo(ctx, q.ProjectID, q.RepoID, node, state)
		if err != nil {
			return nil, fmt.Errorf("traverse into %s: %w", node, err)
		}
		edges = append(edges, in...)
	}
	sortRelations(edges)
	return edges, nil
}

// sortRelations imposes the deterministic order every backend must return.
// Determinism here is not cosmetic: a context pack's digest is what proves two
// dispatches were given the same memory, and an unordered traversal would make
// that digest change for no reason.
func sortRelations(rels []domain.ProjectMemoryRelation) {
	sort.SliceStable(rels, func(i, j int) bool {
		a, b := rels[i], rels[j]
		switch {
		case a.From.Kind != b.From.Kind:
			return a.From.Kind < b.From.Kind
		case a.From.Key != b.From.Key:
			return a.From.Key < b.From.Key
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		case a.To.Kind != b.To.Kind:
			return a.To.Kind < b.To.Kind
		case a.To.Key != b.To.Key:
			return a.To.Key < b.To.Key
		default:
			return a.ID < b.ID
		}
	})
}

// TeeGraph writes to a canonical backend and mirrors to an optional one.
//
// It is how an external graph is added without ever becoming load-bearing: the
// canonical write must succeed, the mirror's failure is logged by the caller
// and otherwise ignored, and reads come from whichever backend answered. A
// mirror that is down therefore costs nothing but traversal quality.
type TeeGraph struct {
	// Canonical is the backend AO relies on. It is never optional.
	Canonical MemoryGraph
	// Optional is the external backend. A nil Optional is the ordinary case.
	Optional MemoryGraph
	// OnOptionalError is called when the optional backend fails, so the
	// degradation is visible in logs. A nil hook drops the error.
	OnOptionalError func(error)
}

// Name reports both backends, so operator output says what is actually wired.
func (t *TeeGraph) Name() string {
	if t.Optional == nil {
		return t.Canonical.Name()
	}
	return t.Canonical.Name() + "+" + t.Optional.Name()
}

// Upsert writes canonically first. The optional backend's failure never fails
// the write: the fact is already durable in the canonical one.
func (t *TeeGraph) Upsert(ctx context.Context, now time.Time, rels ...domain.ProjectMemoryRelation) error {
	if err := t.Canonical.Upsert(ctx, now, rels...); err != nil {
		return err
	}
	if t.Optional == nil {
		return nil
	}
	if err := t.Optional.Upsert(ctx, now, rels...); err != nil && t.OnOptionalError != nil {
		t.OnOptionalError(err)
	}
	return nil
}

// Neighbors prefers the optional backend when it can answer — that is the
// whole reason to attach a richer one — and falls back to the canonical
// backend on any failure, including ErrGraphUnavailable.
func (t *TeeGraph) Neighbors(ctx context.Context, q GraphQuery) ([]domain.ProjectMemoryRelation, error) {
	if t.Optional != nil {
		rels, err := t.Optional.Neighbors(ctx, q)
		if err == nil {
			return rels, nil
		}
		if t.OnOptionalError != nil {
			t.OnOptionalError(err)
		}
	}
	return t.Canonical.Neighbors(ctx, q)
}

// UnavailableGraph is a MemoryGraph that is configured but cannot be reached.
//
// It exists so the "optional adapter unavailable" path is a real, testable
// state rather than a nil check: writes are dropped and traversals return
// ErrGraphUnavailable, which TeeGraph absorbs into a fallback. Wiring one in
// front of TeeGraph.Optional is how the failure mode is exercised.
type UnavailableGraph struct {
	// Backend is the name of the backend that is missing, for operator output.
	Backend string
}

// Name reports the unreachable backend's name.
func (u UnavailableGraph) Name() string {
	if u.Backend == "" {
		return "unavailable"
	}
	return u.Backend + "(unavailable)"
}

// Upsert drops the write and reports the outage.
func (u UnavailableGraph) Upsert(context.Context, time.Time, ...domain.ProjectMemoryRelation) error {
	return fmt.Errorf("%w: %s", ErrGraphUnavailable, u.Name())
}

// Neighbors reports the outage rather than an empty answer, so a caller cannot
// mistake "the backend is down" for "this node has no neighbours".
func (u UnavailableGraph) Neighbors(context.Context, GraphQuery) ([]domain.ProjectMemoryRelation, error) {
	return nil, fmt.Errorf("%w: %s", ErrGraphUnavailable, u.Name())
}
