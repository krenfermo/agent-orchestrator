package codegraph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// index.go — the project-scoped, durable code graph.
//
// NativeIndexer (native.go) is keyed by a filesystem path and persists a whole
// graph as one document. That was the right shape for the context router,
// which knows a checkout and nothing else. It is the wrong shape for project
// memory, which needs four things a document cannot give:
//
//   - the SAME identity project memory uses, (project_id, repo_id), so one
//     registered project has exactly one canonical graph no matter how many
//     worktrees execute against it;
//   - generation-conditioned CAS, so two dispatches that start at once resolve
//     to one builder instead of two full rebuilds;
//   - staging, so a rebuild is invisible until it is complete and a crash
//     leaves the previous complete graph serving;
//   - indexed reads, so retrieval on the dispatch path costs a lookup rather
//     than a scan of every symbol in the repository.
//
// Index is that. It shares this package's extractors, scanner and scoring with
// NativeIndexer -- there is one definition of what a symbol is and one of what
// is relevant -- and differs only in where the facts live and who they belong
// to.

// BackendLocal is the name of the in-tree backend. It is recorded on every
// graph so operator output says what actually produced it; an external adapter
// would record its own name here, and nothing would ever report LocalGraph
// under a vendor's name.
const BackendLocal = "local"

// ErrBuildSuperseded means a newer pass claimed the repository while this one
// was working. It is not a failure of the graph: the newer pass's answer is
// the one that should win, and this one's staged rows are collectable.
var ErrBuildSuperseded = errors.New("codegraph: build superseded by a newer pass")

// ScanLimits bound one pass. They exist as a struct so an operator can see in
// one place everything that constrains what AO will look at.
type ScanLimits struct {
	// MaxFiles caps how many paths a pass admits. A repository with more is
	// indexed up to the cap, in walk order, and the pass reports the
	// truncation rather than pretending to completeness.
	MaxFiles int
	// MaxTotalBytes caps how much source a pass reads in total.
	MaxTotalBytes int64
	// MaxAffectedSymbols caps the blast radius an incremental update reports.
	MaxAffectedSymbols int
	// StaleBuildAfter is how long a claimed build may go without finishing
	// before a later pass is allowed to take it over.
	//
	// It exists because "a build is in flight" and "a build died in flight"
	// look identical in the database, and the two need opposite answers: a
	// second dispatch arriving during a LIVE build must stand down, and a
	// restart arriving after a CRASHED one must take over. Time is the only
	// evidence available to tell them apart, so the threshold is generous --
	// standing down costs a dispatch nothing, and taking over a live build
	// costs both of them their work.
	StaleBuildAfter time.Duration
}

// DefaultScanLimits are the bounds a code-graph pass runs with unless told
// otherwise. They are deliberately larger than project memory's own file cap:
// memory admits the files it derives PROSE from, and the graph admits the
// files it derives STRUCTURE from, which is most of a repository's source.
func DefaultScanLimits() ScanLimits {
	return ScanLimits{
		MaxFiles:           20000,
		MaxTotalBytes:      512 << 20,
		MaxAffectedSymbols: 200,
		StaleBuildAfter:    30 * time.Minute,
	}
}

// Normalized fills in any bound left at zero, so a partially specified
// ScanLimits can never mean "unbounded".
func (l ScanLimits) Normalized() ScanLimits {
	d := DefaultScanLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxAffectedSymbols <= 0 {
		l.MaxAffectedSymbols = d.MaxAffectedSymbols
	}
	if l.StaleBuildAfter <= 0 {
		l.StaleBuildAfter = d.StaleBuildAfter
	}
	return l
}

// Index maintains one project's canonical code graph.
type Index struct {
	repo    Repository
	scanner scanner
	limits  ScanLimits
	now     func() time.Time
	log     *slog.Logger
}

// IndexOption customizes an Index.
type IndexOption func(*Index)

// WithScanLimits replaces the pass bounds.
func WithScanLimits(l ScanLimits) IndexOption {
	return func(ix *Index) { ix.limits = l.Normalized() }
}

// WithIndexClock replaces the source of timestamps, so a test can be
// deterministic.
func WithIndexClock(now func() time.Time) IndexOption {
	return func(ix *Index) {
		if now != nil {
			ix.now = now
		}
	}
}

// WithIndexLogger attaches a logger. A nil logger silently drops the
// degradations this package swallows, which is what a test wants and not what
// production wants.
func WithIndexLogger(log *slog.Logger) IndexOption {
	return func(ix *Index) { ix.log = log }
}

// WithIndexExtractors replaces the language extractors.
func WithIndexExtractors(extractors ...Extractor) IndexOption {
	return func(ix *Index) { ix.scanner.extractors = newExtractorSet(extractors) }
}

// NewIndex returns a project-scoped code graph over a durable repository.
func NewIndex(repo Repository, opts ...IndexOption) *Index {
	ix := &Index{
		repo:    repo,
		scanner: newScanner(),
		limits:  DefaultScanLimits(),
		now:     func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(ix)
	}
	return ix
}

// SyncRequest names the repository a pass runs against.
//
// RepoPath must be the CANONICAL repository root. A linked worktree is not a
// repository, and passing one would mint a second graph for a workspace -- the
// exact contamination P2-E closed for project memory. The caller holds the
// project record and is the authority on its root; this package refuses rather
// than guesses (see projectmemory's EnsureFresh for the funnel that enforces
// it).
type SyncRequest struct {
	ProjectID domain.ProjectID
	RepoID    string
	RepoPath  string
	// Commit and Branch are the revision the pass brings the graph to. An
	// empty commit means the caller could not prove one, and the graph then
	// claims none rather than an unverifiable one.
	Commit string
	Branch string
	// RepoIdentity is the repository identity the facts are derived under, in
	// the same form project memory records. A later mismatch withholds the
	// graph rather than serving facts from a different checkout.
	RepoIdentity string
}

// SyncOutcome is what a pass did. Every field is measured, and the pairs that
// matter -- parsed against reused, considered against selected -- are what make
// "incremental is dramatically cheaper" a recorded fact rather than a claim.
type SyncOutcome struct {
	Kind       store.CodeGraphSyncKind
	Generation int64
	// FilesScanned is every path the pass looked at; FilesParsed the ones it
	// actually read and analysed; FilesReused the ones whose content hash
	// still matched, which cost no parse.
	FilesScanned int
	FilesParsed  int
	FilesReused  int
	FilesRemoved int
	// FilesAdded counts paths that had no row before this pass. It is carried
	// so an incremental update can maintain the graph's totals by delta
	// instead of counting every row.
	FilesAdded int
	// Symbol and edge deltas across the whole pass.
	SymbolsAdded   int
	SymbolsRemoved int
	EdgesAdded     int
	EdgesRemoved   int
	// The graph's size after the pass.
	Files   int
	Symbols int
	Edges   int
	// AffectedSymbols names the symbols that reach a symbol this pass changed:
	// the bounded blast radius of the change. See affectedBy.
	AffectedSymbols []string
	Duration        time.Duration
	// Architecture is the rendered structural summary, refreshed only when
	// something actually changed.
	Architecture string
	// Truncated reports that a bound was hit.
	Truncated bool
	// Reason explains a pass that did nothing.
	Reason string
}

// Changed reports whether the pass altered the graph.
func (o SyncOutcome) Changed() bool {
	return o.FilesParsed > 0 || o.FilesRemoved > 0
}

// Status reads one repository's durable state.
func (ix *Index) Status(ctx context.Context, projectID domain.ProjectID, repoID string) (store.CodeGraphState, bool, error) {
	return ix.repo.GetCodeGraphState(ctx, projectID, repoID)
}

// StatusAll reads every registered repository of one project.
func (ix *Index) StatusAll(ctx context.Context, projectID domain.ProjectID) ([]store.CodeGraphState, error) {
	return ix.repo.ListCodeGraphStates(ctx, projectID)
}

// Build runs a full pass, staged.
//
// "Full" means it visits every admitted path. It does NOT mean it re-analyses
// them: a path whose content hash still matches the served generation is
// carried forward by the database without being parsed, which is what makes a
// rebuild of a quiet repository cost a read and a hash per file instead of a
// parse. What a full pass cannot avoid is the read itself -- a content digest
// cannot be computed without reading -- and that saving belongs to Apply.
func (ix *Index) Build(ctx context.Context, req SyncRequest) (SyncOutcome, error) {
	started := ix.now()
	root, err := ix.prepare(ctx, req)
	if err != nil {
		return SyncOutcome{}, err
	}

	state, claimed, err := ix.claim(ctx, req)
	if err != nil {
		return SyncOutcome{}, err
	}
	if !claimed {
		return SyncOutcome{
			Kind: store.CodeGraphSyncNoop, Generation: state.ServedGeneration,
			Duration: ix.now().Sub(started),
			Reason:   "another pass is already building this repository's code graph",
		}, nil
	}

	outcome, err := ix.runBuild(ctx, req, root, state, started)
	if err != nil {
		if _, failErr := ix.repo.FailCodeGraphBuild(ctx, req.ProjectID, req.RepoID, state.Generation, err.Error(), ix.now()); failErr != nil && ix.log != nil {
			ix.log.Warn("code graph: recording a failed build failed", "err", failErr)
		}
		return SyncOutcome{}, err
	}
	return outcome, nil
}

// runBuild is the body of a claimed full pass.
func (ix *Index) runBuild(
	ctx context.Context, req SyncRequest, root string, state store.CodeGraphState, started time.Time,
) (SyncOutcome, error) {
	generation := state.Generation
	served := state.ServedGeneration

	// Two ledger reads, not two per file. The served ledger answers "is this
	// path unchanged"; the staging ledger answers "did a previous attempt at
	// this same generation already do this path", which is what makes a build
	// interrupted by a crash resume instead of restarting.
	servedLedger, err := ix.ledger(ctx, req, served)
	if err != nil {
		return SyncOutcome{}, err
	}
	stagedLedger, err := ix.ledger(ctx, req, generation)
	if err != nil {
		return SyncOutcome{}, err
	}

	candidates, err := ix.scanner.walk(ctx, root)
	if err != nil {
		return SyncOutcome{}, err
	}

	out := SyncOutcome{Kind: store.CodeGraphSyncFull, Generation: generation}
	present := make(map[string]bool, len(candidates))
	var readBytes int64

	for _, rel := range candidates {
		if err := ctx.Err(); err != nil {
			return SyncOutcome{}, err
		}
		if out.FilesScanned >= ix.limits.MaxFiles || readBytes >= ix.limits.MaxTotalBytes {
			out.Truncated = true
			out.Reason = fmt.Sprintf("stopped at the %d-file / %d-byte pass bound", ix.limits.MaxFiles, ix.limits.MaxTotalBytes)
			break
		}
		out.FilesScanned++
		present[rel] = true

		if _, done := stagedLedger[rel]; done {
			// Already carried into this generation by an earlier attempt.
			out.FilesReused++
			continue
		}
		extractor, ok := ix.scanner.indexable(rel)
		if !ok {
			continue
		}
		data, ok, err := ix.scanner.readCandidate(root, rel)
		if err != nil {
			return SyncOutcome{}, err
		}
		if !ok {
			continue
		}
		readBytes += int64(len(data))
		hash := hashBytes(data)

		if previous, exists := servedLedger[rel]; exists &&
			previous.ContentHash == hash && previous.Language == extractor.Language() {
			copied, err := ix.repo.CopyCodeGraphPathForward(ctx, req.ProjectID, req.RepoID, served, generation, rel)
			if err != nil {
				return SyncOutcome{}, err
			}
			if copied {
				out.FilesReused++
				continue
			}
		}
		if err := ix.writeEntry(ctx, req, generation, rel, data, hash, extractor, &out); err != nil {
			return SyncOutcome{}, err
		}
	}

	// Anything the walk did not re-observe simply never reached the staging
	// generation. Deletion is therefore free and exact: there is no sweep, and
	// no way for a stale entry to survive a full pass.
	for rel := range servedLedger {
		if !present[rel] {
			out.FilesRemoved++
		}
	}

	return ix.finishBuild(ctx, req, generation, out, started)
}

// finishBuild counts the staged graph, refreshes the architecture summary and
// publishes it.
func (ix *Index) finishBuild(
	ctx context.Context, req SyncRequest, generation int64, out SyncOutcome, started time.Time,
) (SyncOutcome, error) {
	files, symbols, edges, err := ix.repo.CountCodeGraph(ctx, req.ProjectID, req.RepoID, generation)
	if err != nil {
		return SyncOutcome{}, err
	}
	out.Files, out.Symbols, out.Edges = int(files), int(symbols), int(edges)

	rendered, encoded, err := ix.renderArchitecture(ctx, req, generation)
	if err != nil {
		return SyncOutcome{}, err
	}
	out.Architecture = rendered
	out.Duration = ix.now().Sub(started)

	applied, err := ix.repo.CompleteCodeGraphBuild(ctx, store.CodeGraphCompletion{
		ProjectID: req.ProjectID, RepoID: req.RepoID, Generation: generation,
		IndexedCommit: req.Commit, RepoIdentity: req.RepoIdentity,
		FileCount: files, SymbolCount: symbols, EdgeCount: edges,
		SyncKind:    store.CodeGraphSyncFull,
		FilesParsed: int64(out.FilesParsed), FilesReused: int64(out.FilesReused),
		FilesRemoved: int64(out.FilesRemoved),
		SymbolsAdded: int64(out.SymbolsAdded), SymbolsRemoved: int64(out.SymbolsRemoved),
		EdgesAdded: int64(out.EdgesAdded), EdgesRemoved: int64(out.EdgesRemoved),
		Duration: out.Duration, Architecture: rendered, ArchitectureJSON: encoded,
	}, ix.now())
	if err != nil {
		return SyncOutcome{}, err
	}
	if !applied {
		return SyncOutcome{}, fmt.Errorf("%w: generation %d", ErrBuildSuperseded, generation)
	}
	return out, nil
}

// Apply applies a diff to the served graph, in place.
//
// This is the path the whole phase exists for. A task that changes one file
// costs one file's work: the diff names the paths, only those paths are
// touched, and of those only the ones whose content hash actually moved are
// re-parsed. Nothing else in the repository is read, hashed, or considered.
//
// It writes at the served generation rather than staging a new one, and it is
// safe to do so because each path's rows are replaced inside a single
// transaction: a reader sees the old version of a file or the new one, never
// half of either.
func (ix *Index) Apply(ctx context.Context, req SyncRequest, diff Diff) (SyncOutcome, error) {
	started := ix.now()
	root, err := ix.prepare(ctx, req)
	if err != nil {
		return SyncOutcome{}, err
	}
	state, found, err := ix.repo.GetCodeGraphState(ctx, req.ProjectID, req.RepoID)
	if err != nil {
		return SyncOutcome{}, err
	}
	if !found || !state.Indexed() || state.Phase != store.CodeGraphIdle {
		// Nothing to update against, or a build is already in flight. A full
		// pass is what the caller wants, and the outcome's Kind says that is
		// what they got.
		return ix.Build(ctx, req)
	}

	generation := state.ServedGeneration
	out := SyncOutcome{Kind: store.CodeGraphSyncIncremental, Generation: generation}
	changed := map[string]bool{}
	// structural records whether anything the ARCHITECTURE summary is derived
	// from could have moved. Most edits cannot move it -- a rewritten function
	// body changes no module, no endpoint, no table and no dependency -- and
	// recomputing a whole-repository census for each of those is the single
	// most expensive thing an ordinary task could pay for. See architectural().
	structural := false

	changes := append([]FileChange(nil), diff.Changes...)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return SyncOutcome{}, err
		}
		rel := normalizeRel(change.Path)
		if rel == "" {
			continue
		}
		out.FilesScanned++
		switch change.Status {
		case ChangeDeleted:
			// A file appearing or disappearing changes the census outright:
			// the module list, the language mix, the entry points.
			structural = true
			if err := ix.dropPath(ctx, req, generation, rel, &out, changed); err != nil {
				return SyncOutcome{}, err
			}
		case ChangeRenamed:
			// A move rewrites every symbol ID in the file, because the path is
			// part of a symbol's identity. So a rename is a delete and a
			// create, recorded as both -- which is also the honest description
			// of what happened to anything that referred to the old path.
			structural = true
			if old := normalizeRel(change.OldPath); old != "" && old != rel {
				if err := ix.dropPath(ctx, req, generation, old, &out, changed); err != nil {
					return SyncOutcome{}, err
				}
			}
			if err := ix.syncPath(ctx, req, root, generation, rel, &out, changed); err != nil {
				return SyncOutcome{}, err
			}
		case ChangeAdded, ChangeModified:
			if change.Status == ChangeAdded {
				structural = true
			}
			moved, err := ix.syncPathTracked(ctx, req, root, generation, rel, &out, changed)
			if err != nil {
				return SyncOutcome{}, err
			}
			structural = structural || moved
		default:
			return SyncOutcome{}, fmt.Errorf("codegraph: unsupported change status %q for %q", change.Status, rel)
		}
	}

	if !out.Changed() {
		out.Kind = store.CodeGraphSyncNoop
		out.Reason = "no indexed file changed between the two commits"
	}
	return ix.finishApply(ctx, req, state, out, changed, structural, started)
}

// syncPathTracked is syncPath plus the answer to "could this have moved the
// architecture", which is decided by comparing the file's ARCHITECTURAL
// fingerprint before and after: its role, the surfaces it declares (endpoints,
// tables, queries, configuration keys), and what it imports.
//
// Two small reads of one file, against a census over every file. That is the
// whole trade, and it is why an ordinary task's sync costs a file's work.
func (ix *Index) syncPathTracked(
	ctx context.Context, req SyncRequest, root string, generation int64, rel string,
	out *SyncOutcome, changed map[string]bool,
) (bool, error) {
	before, err := ix.architectural(ctx, req, generation, rel)
	if err != nil {
		return false, err
	}
	if err := ix.syncPath(ctx, req, root, generation, rel, out, changed); err != nil {
		return false, err
	}
	after, err := ix.architectural(ctx, req, generation, rel)
	if err != nil {
		return false, err
	}
	return before != after, nil
}

// architectural fingerprints the part of one file that the architecture
// summary is derived from.
//
// Deliberately NOT the whole file: a changed function body, a renamed local, a
// new unexported helper all leave this identical, because none of them appear
// anywhere in the summary. What does appear is the file's surfaces and its
// dependencies, and those are what is hashed.
func (ix *Index) architectural(ctx context.Context, req SyncRequest, generation int64, rel string) (string, error) {
	symbols, err := ix.repo.ListCodeGraphSymbolsForPath(ctx, req.ProjectID, req.RepoID, generation, rel)
	if err != nil {
		return "", err
	}
	edges, err := ix.repo.ListCodeGraphEdgesForPath(ctx, req.ProjectID, req.RepoID, generation, rel)
	if err != nil {
		return "", err
	}
	var parts []string
	for _, sym := range symbols {
		switch SymbolKind(sym.Kind) {
		case SymbolEndpoint, SymbolTable, SymbolQuery, SymbolConfig:
			parts = append(parts, sym.Kind+":"+sym.Name)
		}
	}
	for _, edge := range edges {
		if edge.Kind == string(EdgeImport) || edge.Kind == string(EdgeTests) {
			parts = append(parts, edge.Kind+":"+edge.ToKey)
		}
	}
	sort.Strings(parts)
	return hashBytes([]byte(strings.Join(parts, "\x00"))), nil
}

// finishApply records an in-place update, refreshing the architecture summary
// only when something actually moved.
func (ix *Index) finishApply(
	ctx context.Context, req SyncRequest, state store.CodeGraphState,
	out SyncOutcome, changed map[string]bool, structural bool, started time.Time,
) (SyncOutcome, error) {
	generation := state.ServedGeneration
	// Counted by DELTA, not by COUNT(*). Every mutation on this path reports
	// exactly what it added and removed, so the totals follow from the totals
	// that were already published plus this pass's own arithmetic -- and an
	// ordinary one-file task stops paying for a scan of every symbol in the
	// repository. Measurement on AO's own checkout is what made this
	// necessary rather than tidy: 32,000 symbols turned a one-file sync into
	// a second and a half, nearly all of it counting rows nothing had touched.
	//
	// It falls back to the authoritative count if the arithmetic ever produces
	// something impossible, because a wrong total that looks plausible is
	// worse than a slow one.
	files := state.FileCount + int64(out.FilesAdded) - int64(out.FilesRemoved)
	symbols := state.SymbolCount + int64(out.SymbolsAdded) - int64(out.SymbolsRemoved)
	edges := state.EdgeCount + int64(out.EdgesAdded) - int64(out.EdgesRemoved)
	if files < 0 || symbols < 0 || edges < 0 {
		var err error
		if files, symbols, edges, err = ix.repo.CountCodeGraph(ctx, req.ProjectID, req.RepoID, generation); err != nil {
			return SyncOutcome{}, err
		}
	}
	out.Files, out.Symbols, out.Edges = int(files), int(symbols), int(edges)
	var err error

	rendered, encoded := state.Architecture, state.ArchitectureJSON
	switch {
	case !out.Changed():
		// A no-op sync must not pay for a census, and a census over an
		// unchanged graph would produce the same bytes anyway.
	case structural:
		// Something the summary is actually derived from moved: a file
		// appeared or vanished, a route or a table was declared, a dependency
		// changed. Recompute it properly.
		if rendered, encoded, err = ix.renderArchitecture(ctx, req, generation); err != nil {
			return SyncOutcome{}, err
		}
	default:
		// The commonest case by far: bodies changed and structure did not. The
		// summary's SHAPE is unchanged; only its totals and its provenance
		// moved, and those are already in hand from the row counts. Refreshing
		// them is one decode and one render, against a census over every file
		// in the repository.
		if rendered, encoded, err = ix.refreshArchitectureCounts(state, req.Commit, files, symbols, edges); err != nil {
			return SyncOutcome{}, err
		}
	}
	if out.Changed() {
		if out.AffectedSymbols, err = ix.affectedBy(ctx, req, generation, changed); err != nil {
			return SyncOutcome{}, err
		}
	}
	out.Architecture = rendered
	out.Duration = ix.now().Sub(started)

	applied, err := ix.repo.RecordCodeGraphIncremental(ctx, store.CodeGraphCompletion{
		ProjectID: req.ProjectID, RepoID: req.RepoID, Generation: generation,
		IndexedCommit: req.Commit, RepoIdentity: req.RepoIdentity,
		FileCount: files, SymbolCount: symbols, EdgeCount: edges,
		SyncKind:    out.Kind,
		FilesParsed: int64(out.FilesParsed), FilesReused: int64(out.FilesReused),
		FilesRemoved: int64(out.FilesRemoved),
		SymbolsAdded: int64(out.SymbolsAdded), SymbolsRemoved: int64(out.SymbolsRemoved),
		EdgesAdded: int64(out.EdgesAdded), EdgesRemoved: int64(out.EdgesRemoved),
		Duration: out.Duration, Architecture: rendered, ArchitectureJSON: encoded,
	}, ix.now())
	if err != nil {
		return SyncOutcome{}, err
	}
	if !applied {
		return SyncOutcome{}, fmt.Errorf("%w: generation %d", ErrBuildSuperseded, generation)
	}
	return out, nil
}

// syncPath brings one path up to date at a generation.
func (ix *Index) syncPath(
	ctx context.Context, req SyncRequest, root string, generation int64, rel string,
	out *SyncOutcome, changed map[string]bool,
) error {
	extractor, ok := ix.scanner.indexable(rel)
	if !ok {
		// The path is not one AO indexes -- or is no longer one, because it
		// was renamed to an extension with no extractor, or is a secret by
		// convention. Either way anything it left behind must go.
		return ix.dropPath(ctx, req, generation, rel, out, changed)
	}
	data, ok, err := ix.scanner.readCandidate(root, rel)
	if err != nil {
		return err
	}
	if !ok {
		return ix.dropPath(ctx, req, generation, rel, out, changed)
	}
	hash := hashBytes(data)
	previous, exists, err := ix.repo.GetCodeGraphFileRecord(ctx, req.ProjectID, req.RepoID, generation, rel)
	if err != nil {
		return err
	}
	if exists && previous.ContentHash == hash && previous.Language == extractor.Language() {
		// The diff named it, but its bytes did not move -- a whitespace-only
		// rebase, a revert, a file touched and restored. Nothing to do.
		out.FilesReused++
		return nil
	}
	return ix.writeEntry(ctx, req, generation, rel, data, hash, extractor, out, changed)
}

// dropPath removes one path and everything derived from it.
func (ix *Index) dropPath(
	ctx context.Context, req SyncRequest, generation int64, rel string, out *SyncOutcome, changed map[string]bool,
) error {
	previous, err := ix.repo.ListCodeGraphSymbolsForPath(ctx, req.ProjectID, req.RepoID, generation, rel)
	if err != nil {
		return err
	}
	delta, err := ix.repo.DeleteCodeGraphPath(ctx, req.ProjectID, req.RepoID, generation, rel)
	if err != nil {
		return err
	}
	if !delta.FileRemoved && delta.SymbolsBefore == 0 && delta.EdgesBefore == 0 {
		return nil
	}
	out.FilesRemoved++
	out.SymbolsRemoved += int(delta.SymbolsBefore)
	out.EdgesRemoved += int(delta.EdgesBefore)
	if changed != nil {
		for _, sym := range previous {
			changed[sym.Name] = true
		}
	}
	return nil
}

// writeEntry extracts one file and replaces its rows.
func (ix *Index) writeEntry(
	ctx context.Context, req SyncRequest, generation int64, rel string, data []byte, hash string,
	extractor Extractor, out *SyncOutcome, changedSets ...map[string]bool,
) error {
	var changed map[string]bool
	if len(changedSets) > 0 {
		changed = changedSets[0]
	}
	if changed != nil {
		previous, err := ix.repo.ListCodeGraphSymbolsForPath(ctx, req.ProjectID, req.RepoID, generation, rel)
		if err != nil {
			return err
		}
		for _, sym := range previous {
			changed[sym.Name] = true
		}
	}

	extraction, err := extractor.Extract(rel, data)
	if err != nil {
		return fmt.Errorf("codegraph: extract %s: %w", rel, err)
	}
	entry := storeEntry(rel, hash, extractor.Language(), ClassifyFile(rel, data), int64(len(data)), extraction)

	delta, err := ix.repo.PutCodeGraphEntry(ctx, req.ProjectID, req.RepoID, generation, entry, ix.now())
	if err != nil {
		return err
	}
	out.FilesParsed++
	if !delta.FileExisted {
		out.FilesAdded++
	}
	out.SymbolsAdded += int(delta.SymbolsAfter)
	out.SymbolsRemoved += int(delta.SymbolsBefore)
	out.EdgesAdded += int(delta.EdgesAfter)
	out.EdgesRemoved += int(delta.EdgesBefore)
	if changed != nil {
		for _, sym := range entry.Symbols {
			changed[sym.Name] = true
		}
	}
	return nil
}

// affectedBy answers section 9 of the brief: what else may need attention
// because these symbols moved.
//
// The answer is bounded by construction, and the reason is a design choice
// made two files away: edges target NAMES rather than resolved declarations.
// Changing a function's signature therefore does not invalidate any stored
// edge -- the callers still call the same name -- so there is nothing to
// recursively rebuild. What a caller may want is to KNOW, and that is one
// indexed lookup per changed name.
//
// So the blast radius is reported, not chased. A change to one symbol never
// costs more than a bounded set of lookups, and never rebuilds the graph.
func (ix *Index) affectedBy(
	ctx context.Context, req SyncRequest, generation int64, changed map[string]bool,
) ([]string, error) {
	names := sortedKeys(changed)
	if len(names) > ix.limits.MaxAffectedSymbols {
		names = names[:ix.limits.MaxAffectedSymbols]
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		if len(out) >= ix.limits.MaxAffectedSymbols {
			break
		}
		edges, err := ix.repo.ListCodeGraphEdgesTo(ctx, req.ProjectID, req.RepoID, generation, name, affectedFanOut)
		if err != nil {
			return nil, err
		}
		for _, edge := range edges {
			if edge.FromKey == "" || seen[edge.FromKey] || changed[edge.FromKey] {
				continue
			}
			seen[edge.FromKey] = true
			out = append(out, edge.FromKey)
			if len(out) >= ix.limits.MaxAffectedSymbols {
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// affectedFanOut bounds how many dependants one changed symbol reports. A
// symbol called from four hundred places is real; naming all four hundred in a
// sync outcome is not useful to anybody.
const affectedFanOut = 32

// prepare validates the request and registers the repository.
func (ix *Index) prepare(ctx context.Context, req SyncRequest) (string, error) {
	if strings.TrimSpace(string(req.ProjectID)) == "" {
		return "", fmt.Errorf("%w: a project is required", ErrProjectRoot)
	}
	if strings.TrimSpace(req.RepoID) == "" {
		return "", fmt.Errorf("%w: a repository id is required", ErrProjectRoot)
	}
	root, err := CanonicalRoot(req.RepoPath)
	if err != nil {
		return "", err
	}
	if err := ix.repo.EnsureCodeGraphRepo(ctx, req.ProjectID, req.RepoID, root, BackendLocal, ix.now()); err != nil {
		return "", err
	}
	return root, nil
}

// claim takes the pass.
//
// Three outcomes, and the middle one is the whole of this function's
// difficulty. A repository in a terminal phase is claimed outright. A
// repository whose build is in flight and RECENT belongs to somebody else, and
// this caller stands down -- two concurrent full rebuilds of one repository are
// pure waste, and the loser has nothing useful to do with a second answer. A
// repository whose build is in flight and STALE was abandoned by a process that
// is not coming back, and is taken over: the reclaim is conditional on the
// generation this caller read, so two restarts racing to recover the same dead
// build cannot both proceed.
func (ix *Index) claim(ctx context.Context, req SyncRequest) (store.CodeGraphState, bool, error) {
	state, found, err := ix.repo.GetCodeGraphState(ctx, req.ProjectID, req.RepoID)
	if err != nil {
		return store.CodeGraphState{}, false, err
	}
	if found && state.Building() {
		if !ix.abandoned(state) {
			return state, false, nil
		}
		return ix.repo.ReclaimCodeGraphBuild(ctx, req.ProjectID, req.RepoID, state.Generation, req.Commit, req.Branch, ix.now())
	}
	return ix.repo.ClaimCodeGraphBuild(ctx, req.ProjectID, req.RepoID, req.Commit, req.Branch, ix.now())
}

// abandoned reports whether an in-flight build has gone quiet for long enough
// to be taken over. A build with no recorded start is treated as abandoned:
// the row cannot say when it began, so it cannot be shown to be alive.
func (ix *Index) abandoned(state store.CodeGraphState) bool {
	if !state.StartedAt.Valid {
		return true
	}
	return ix.now().Sub(state.StartedAt.Time) >= ix.limits.StaleBuildAfter
}

// ledger reads one generation's file records into a map. Generation zero has
// no rows by definition, so it is answered without a query.
func (ix *Index) ledger(ctx context.Context, req SyncRequest, generation int64) (map[string]store.CodeGraphFileRecord, error) {
	if generation <= 0 {
		return map[string]store.CodeGraphFileRecord{}, nil
	}
	records, err := ix.repo.ListCodeGraphFileRecords(ctx, req.ProjectID, req.RepoID, generation)
	if err != nil {
		return nil, err
	}
	out := make(map[string]store.CodeGraphFileRecord, len(records))
	for _, record := range records {
		out[record.Path] = record
	}
	return out, nil
}

// renderArchitecture recomputes the bounded structural summary from a
// generation.
func (ix *Index) renderArchitecture(ctx context.Context, req SyncRequest, generation int64) (rendered, encoded string, err error) {
	graph, err := ix.loadGraph(ctx, req, generation)
	if err != nil {
		return "", "", err
	}
	arch := graph.Architecture()
	arch.ProjectRoot = req.RepoPath
	arch.IndexedCommit = req.Commit
	payload, err := json.Marshal(arch)
	if err != nil {
		return "", "", fmt.Errorf("encode architecture summary: %w", err)
	}
	return arch.Render(), string(payload), nil
}

// refreshArchitectureCounts updates a stored summary's totals and provenance
// without recomputing it.
//
// It is only ever reached when the pass proved nothing structural moved, so
// what it produces is the same summary with correct numbers -- not a stale one
// dressed up. A stored summary that will not decode falls back to the full
// recompute rather than being patched blind.
func (ix *Index) refreshArchitectureCounts(
	state store.CodeGraphState, commit string, files, symbols, edges int64,
) (rendered, encoded string, err error) {
	if state.ArchitectureJSON == "" {
		return state.Architecture, state.ArchitectureJSON, nil
	}
	var arch Architecture
	if decodeErr := json.Unmarshal([]byte(state.ArchitectureJSON), &arch); decodeErr != nil {
		if ix.log != nil {
			ix.log.Warn("code graph: stored architecture summary would not decode; keeping the previous one",
				"repo", state.RepoID, "err", decodeErr)
		}
		return state.Architecture, state.ArchitectureJSON, nil
	}
	arch.Files, arch.Symbols, arch.Edges = int(files), int(symbols), int(edges)
	if commit != "" {
		arch.IndexedCommit = commit
	}
	payload, err := json.Marshal(arch)
	if err != nil {
		return "", "", fmt.Errorf("encode architecture summary: %w", err)
	}
	return arch.Render(), string(payload), nil
}

// loadGraph rebuilds the in-memory graph for a generation. It is used only by
// the architecture summary, which runs once per changed pass.
func (ix *Index) loadGraph(ctx context.Context, req SyncRequest, generation int64) (*Graph, error) {
	files, symbols, edges, err := ix.repo.LoadCodeGraph(ctx, req.ProjectID, req.RepoID, generation)
	if err != nil {
		return nil, err
	}
	graph := NewGraph(req.RepoPath)
	graph.IndexedCommit = req.Commit
	for _, file := range files {
		graph.Put(FileEntry{
			Path: file.Path, Hash: file.ContentHash, Language: file.Language,
			Role: FileRole(file.Role), Size: file.SizeBytes,
		})
	}
	for _, record := range symbols {
		entry, ok := graph.Lookup(record.Path)
		if !ok {
			continue
		}
		entry.Symbols = append(entry.Symbols, symbolFromRecord(record))
		graph.Put(entry)
	}
	for _, record := range edges {
		entry, ok := graph.Lookup(record.Path)
		if !ok {
			continue
		}
		entry.Edges = append(entry.Edges, Edge{
			Kind: EdgeKind(record.Kind), From: record.FromKey, To: record.ToKey, Line: int(record.Line),
		})
		graph.Put(entry)
	}
	return graph, nil
}

// Purge deletes a repository's graph, keeping its registration so a later pass
// can rebuild it.
func (ix *Index) Purge(ctx context.Context, projectID domain.ProjectID, repoID string) error {
	return ix.repo.PurgeCodeGraph(ctx, projectID, repoID)
}

// storeEntry converts one extraction into the rows the store writes.
func storeEntry(rel, hash, language string, role FileRole, size int64, extraction Extraction) store.CodeGraphEntry {
	symbols := dedupeSymbols(extraction.Symbols)
	edges := dedupeEdges(extraction.Edges)

	entry := store.CodeGraphEntry{
		File: store.CodeGraphFileRecord{
			Path: rel, Language: language, Role: string(role),
			ContentHash: hash, SizeBytes: size,
		},
		Symbols: make([]store.CodeGraphSymbolRecord, 0, len(symbols)),
		Edges:   make([]store.CodeGraphEdgeRecord, 0, len(edges)),
	}
	for _, sym := range symbols {
		entry.Symbols = append(entry.Symbols, store.CodeGraphSymbolRecord{
			SymbolID: sym.ID, Path: rel, Name: sym.Name, Kind: string(sym.Kind),
			Language: language, Line: int64(sym.Line), EndLine: int64(sym.EndLine),
			Signature: sym.Signature, Doc: sym.Doc, Summary: sym.Summary,
			SummarySource: string(orStatic(sym.SummarySource)), Exported: sym.Exported,
			BodyHash: sym.Hash,
		})
	}
	for _, edge := range edges {
		entry.Edges = append(entry.Edges, store.CodeGraphEdgeRecord{
			EdgeID: edgeID(rel, edge), Path: rel, Kind: string(edge.Kind),
			FromKey: edge.From, ToKey: edge.To, Line: int64(edge.Line),
		})
	}
	return entry
}

// edgeID is an edge's stable identity within its file.
//
// The owning path is part of it, not decoration: two different files can
// legitimately assert the same relation -- the same compile-time interface
// assertion about a type from a third package, say -- and if they shared an id
// the second would overwrite the first, and deleting either file would take
// both edges with it.
func edgeID(rel string, edge Edge) string {
	return hashBytes([]byte(rel + "\x00" + string(edge.Kind) + "\x00" + edge.From + "\x00" + edge.To))
}

func orStatic(source SummarySource) SummarySource {
	if source == "" {
		return SummaryStatic
	}
	return source
}

func symbolFromRecord(record store.CodeGraphSymbolRecord) Symbol {
	return Symbol{
		ID: record.SymbolID, Name: record.Name, Kind: SymbolKind(record.Kind),
		File: record.Path, Line: int(record.Line), EndLine: int(record.EndLine),
		Signature: record.Signature, Doc: record.Doc, Summary: record.Summary,
		SummarySource: SummarySource(record.SummarySource), Exported: record.Exported,
		Hash: record.BodyHash,
	}
}
