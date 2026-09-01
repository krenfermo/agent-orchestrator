package projectmemory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// indexer.go — the bounded, restart-safe indexing pass.
//
// Three properties are what this file is for, and each one is a rule the P2-A
// brief states directly:
//
//   - Bounded. Every dimension of the walk has an explicit cap (IndexLimits),
//     the caps are enforced here rather than trusted to the caller, and a pass
//     that hits one records that it did instead of silently covering less.
//     Nothing indexes "every byte of the repository".
//   - Restart-safe. The generation and the resume cursor are durable. A crash
//     mid-pass leaves a claimed generation and a cursor; a restart resumes
//     from that path rather than starting the repository over, and the
//     already-written facts of that generation stay written.
//   - Fenced. Every write carries the pass's generation, and the store refuses
//     any write from a generation behind the stored row's. A pass that stalls
//     and wakes up after a newer one finished cannot undo it.
//
// One honest limitation, stated here because the brief forbids claiming
// savings AO cannot demonstrate: a full pass must READ every admitted file,
// because a content digest cannot be computed without reading. What a full
// pass saves on an unchanged repository is the *derivation and the writes* —
// the store reconfirms an unchanged fact instead of rewriting it, and
// FilesSkipped counts exactly that. The saving on the read itself belongs to
// UpdateChanged (incremental.go), which visits only the paths a diff names.

// IndexLimits are the explicit bounds of one pass. They exist as a struct so
// an operator can see, in one place, everything that constrains what AO will
// look at.
type IndexLimits struct {
	// MaxFiles caps how many paths a pass admits. A repository with more is
	// indexed up to the cap, in walk order, and the pass reports the
	// truncation rather than pretending to completeness.
	MaxFiles int
	// MaxFileBytes caps one file. A larger file is skipped entirely: it is
	// far more likely to be generated, vendored or a data blob than a fact
	// worth remembering.
	MaxFileBytes int64
	// MaxTotalBytes caps how much a pass reads in total.
	MaxTotalBytes int64
	// MaxModules caps how many module facts one repository produces.
	MaxModules int
	// IgnoredDirs are directory names skipped wherever they appear. They are
	// matched by base name, not by path, because `node_modules` is
	// `node_modules` at every depth.
	IgnoredDirs []string
	// IgnoredExts are file extensions never admitted.
	IgnoredExts []string
	// CheckpointEvery is how many admitted files pass between durable
	// progress writes. Smaller means a cheaper restart and more writes.
	CheckpointEvery int
}

// DefaultIndexLimits are the bounds AO indexes with unless told otherwise.
//
// They are chosen to cover a real repository's *shape* rather than its
// contents: this repository, for instance, has roughly two thousand source
// files, and the point of the cap is that a checkout with a vendored SDK or a
// generated client does not turn into a hundred thousand.
func DefaultIndexLimits() IndexLimits {
	return IndexLimits{
		MaxFiles:      6000,
		MaxFileBytes:  512 * 1024,
		MaxTotalBytes: 256 * 1024 * 1024,
		// 800 is chosen from measurement rather than taste: this repository has
		// 454 directories that contain files, and a cap that a real project of
		// AO's own size sits above would leave every such project permanently
		// truncated — reporting honestly that its memory is incomplete, but
		// incomplete all the same.
		MaxModules: 800,
		IgnoredDirs: []string{
			".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build",
			"out", "target", "coverage", ".next", ".nuxt", ".turbo", ".cache",
			"__pycache__", ".venv", "venv", ".tox", ".mypy_cache", ".pytest_cache",
			".gradle", ".idea", ".vscode", "Pods", "DerivedData", ".terraform",
			"bin", "obj", ".ao",
		},
		IgnoredExts: []string{
			".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".icns", ".svg",
			".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar",
			".mp3", ".mp4", ".mov", ".avi", ".wav", ".ogg", ".webm",
			".woff", ".woff2", ".ttf", ".otf", ".eot",
			".so", ".dylib", ".dll", ".exe", ".a", ".o", ".class", ".jar",
			".wasm", ".bin", ".dat", ".db", ".sqlite", ".sqlite3", ".lock",
			".pyc", ".pyo", ".map",
		},
		CheckpointEvery: 200,
	}
}

// Normalized fills in any bound the caller left at zero, so a partially
// specified IndexLimits can never mean "unbounded".
func (l IndexLimits) Normalized() IndexLimits {
	d := DefaultIndexLimits()
	if l.MaxFiles <= 0 {
		l.MaxFiles = d.MaxFiles
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = d.MaxFileBytes
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = d.MaxTotalBytes
	}
	if l.MaxModules <= 0 {
		l.MaxModules = d.MaxModules
	}
	if len(l.IgnoredDirs) == 0 {
		l.IgnoredDirs = d.IgnoredDirs
	}
	if len(l.IgnoredExts) == 0 {
		l.IgnoredExts = d.IgnoredExts
	}
	if l.CheckpointEvery <= 0 {
		l.CheckpointEvery = d.CheckpointEvery
	}
	return l
}

// isZero reports that the caller set no bound at all, and therefore wants the
// indexer's configured defaults rather than a partially specified set.
func (l IndexLimits) isZero() bool {
	return l.MaxFiles == 0 && l.MaxFileBytes == 0 && l.MaxTotalBytes == 0 &&
		l.MaxModules == 0 && l.CheckpointEvery == 0 &&
		len(l.IgnoredDirs) == 0 && len(l.IgnoredExts) == 0
}

func (l IndexLimits) ignoresDir(name string) bool {
	for _, d := range l.IgnoredDirs {
		if d == name {
			return true
		}
	}
	return false
}

func (l IndexLimits) ignoresExt(ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range l.IgnoredExts {
		if e == ext {
			return true
		}
	}
	return false
}

// IndexRequest asks for one pass over one repository.
type IndexRequest struct {
	// ProjectID is the registered project the repository belongs to.
	ProjectID domain.ProjectID
	// RepoPath is the absolute canonical repository root. It must be the
	// repository, not a task worktree: a worktree contributes task-local
	// facts through TaskMemory, never a parallel canonical memory of its own.
	RepoPath string
	// Commit and Branch are the provenance this pass stamps on everything it
	// writes. An empty Commit is allowed — a non-git directory is still worth
	// remembering — and is recorded as such rather than faked.
	Commit string
	Branch string
	// Limits bounds the pass. A zero value means DefaultIndexLimits.
	Limits IndexLimits
	// Force re-derives every admitted file even when its digest is unchanged.
	// It is the `memory rebuild` path; an ordinary pass leaves it false.
	Force bool
}

// IndexOutcome reports what a pass did. Every count is an audit trail entry,
// not a statistic: together they answer "what did AO look at, what did it
// write, and what did it decline to do".
type IndexOutcome struct {
	RepoID     string
	Generation int64
	// Resumed reports that this pass took over one a crash had left in
	// flight, rather than starting a new generation.
	Resumed bool
	// Skipped reports that the pass did not run because another one holds the
	// repository. It is a normal outcome, not a failure.
	Skipped    bool
	SkipReason string
	// Truncated reports that a bound stopped the walk short, and which one.
	Truncated       bool
	TruncatedReason string

	FilesSeen     int
	FilesAdmitted int
	FilesIndexed  int
	FilesSkipped  int
	FilesRemoved  int
	BytesRead     int64

	ItemsWritten      int
	ItemsReconfirmed  int
	ItemsRefused      int
	RelationsWritten  int
	ItemsInvalidated  int64
	ItemsRetired      int64
	IndexedCommit     string
	Duration          time.Duration
	ModulesDiscovered int
}

// Indexer runs bounded passes over a repository and keeps the durable memory
// in step with it.
type Indexer struct {
	repo   Repository
	graph  MemoryGraph
	now    func() time.Time
	limits IndexLimits
}

// IndexerOption configures an Indexer.
type IndexerOption func(*Indexer)

// WithIndexClock replaces the clock, for tests that need deterministic
// timestamps.
func WithIndexClock(now func() time.Time) IndexerOption {
	return func(i *Indexer) {
		if now != nil {
			i.now = now
		}
	}
}

// WithIndexLimits sets the default bounds for passes that do not carry their
// own.
func WithIndexLimits(l IndexLimits) IndexerOption {
	return func(i *Indexer) { i.limits = l.Normalized() }
}

// NewIndexer builds an indexer over a durable repository and a graph backend.
// A nil graph falls back to the local one, because edges are canonical state
// and must always have somewhere to go.
func NewIndexer(repo Repository, graph MemoryGraph, opts ...IndexerOption) *Indexer {
	if graph == nil {
		graph = NewLocalGraph(repo)
	}
	idx := &Indexer{
		repo:   repo,
		graph:  graph,
		now:    func() time.Time { return time.Now().UTC() },
		limits: DefaultIndexLimits(),
	}
	for _, o := range opts {
		o(idx)
	}
	return idx
}

// Index runs one bounded pass.
//
// The sequence is: claim (or resume) a generation, walk, finalize the
// repository-wide facts, retire what the walk did not re-confirm, promote the
// commit. Every step is durable, and a failure at any step ends the pass in
// IndexPhaseFailed with the reason recorded — which leaves indexed_commit
// where it was, so the next pass still sees the changes this one never
// reached.
func (idx *Indexer) Index(ctx context.Context, req IndexRequest) (IndexOutcome, error) {
	started := idx.now()
	limits := req.Limits
	if limits.isZero() {
		limits = idx.limits
	}
	limits = limits.Normalized()

	repoPath, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		return IndexOutcome{}, err
	}
	repoID := domain.ProjectMemoryRepoID(repoPath)
	out := IndexOutcome{RepoID: repoID}

	if err := idx.repo.EnsureProjectMemoryRepo(ctx, req.ProjectID, repoID, repoPath, started); err != nil {
		return out, err
	}

	state, resumed, ok, err := idx.claim(ctx, req, repoID, started)
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = true
		out.SkipReason = "another indexing pass holds this repository"
		return out, nil
	}
	out.Generation = state.Generation
	out.Resumed = resumed

	pass := &indexPass{
		idx: idx, req: req, limits: limits,
		repoPath: repoPath, repoID: repoID, state: state,
		resumeAfter: "",
		out:         &out,
	}
	if resumed {
		pass.resumeAfter = state.Cursor
	}

	if err := pass.run(ctx); err != nil {
		reason := err.Error()
		if _, ferr := idx.repo.FailProjectMemoryIndexPass(ctx, req.ProjectID, repoID, state.Generation, reason, idx.now()); ferr != nil {
			return out, errors.Join(err, ferr)
		}
		return out, err
	}

	out.Duration = idx.now().Sub(started)
	out.IndexedCommit = req.Commit
	return out, nil
}

// claim takes the pass. It prefers resuming a pass left in flight — that is
// what makes a crash cost the tail of a walk rather than the whole of it — and
// falls back to claiming a fresh generation from a terminal phase.
func (idx *Indexer) claim(
	ctx context.Context, req IndexRequest, repoID string, now time.Time,
) (state domain.ProjectMemoryIndexState, resumed, ok bool, err error) {
	current, found, err := idx.repo.GetProjectMemoryIndexState(ctx, req.ProjectID, repoID)
	if err != nil {
		return state, false, false, err
	}
	if found && current.Running() {
		state, ok, err = idx.repo.ResumeProjectMemoryIndexPass(
			ctx, req.ProjectID, repoID, current.Generation,
			domain.IndexPhaseScanning, req.Commit, req.Branch, now)
		return state, true, ok, err
	}
	state, ok, err = idx.repo.ClaimProjectMemoryIndexPass(ctx, req.ProjectID, repoID, req.Commit, req.Branch, now)
	return state, false, ok, err
}

// indexPass is one running pass. It exists as a struct so the walk, the
// finalize and the retire steps can share the accumulated state without a
// twelve-parameter function.
type indexPass struct {
	idx      *Indexer
	req      IndexRequest
	limits   IndexLimits
	repoPath string
	repoID   string
	state    domain.ProjectMemoryIndexState
	// resumeAfter is the last path the crashed pass had finished. Paths at or
	// before it are still read (module membership and imports have to stay
	// complete) but are not re-derived into items.
	resumeAfter string
	out         *IndexOutcome

	base itemBase
	// modules is the per-directory census, accumulated during the walk.
	modules map[string]*moduleFacts
	// imports maps a module to the raw, unresolved import targets its files
	// named. They are resolved once the manifest's own module path is known.
	imports      map[string]map[string]struct{}
	goModulePath string
	// digest accumulates every admitted path's content hash, in walk order,
	// so the repository-wide facts (overview, modules) carry a provenance
	// digest of their own.
	digest []string
	// walkTruncated records that a bound stopped the WALK short, which is a
	// different thing from the module cap. Only an incomplete walk makes "this
	// fact was not re-confirmed" unsafe to act on: after a complete walk that
	// simply wrote fewer module items, a module left at an older generation
	// really is one AO no longer has memory of, and retiring it is correct.
	walkTruncated bool
}

func (p *indexPass) run(ctx context.Context) error {
	p.base = itemBase{
		ProjectID:  p.req.ProjectID,
		RepoID:     p.repoID,
		Commit:     p.req.Commit,
		Generation: p.state.Generation,
		Origin:     domain.OriginCanonical,
		// P2-D: every fact this pass derives is stamped with the repository it
		// was read from and with the proof that applies to it. RepoIdentityOf
		// runs once per pass, not once per fact -- it shells out to git, and a
		// pass over a large repository would otherwise pay for it thousands of
		// times for an answer that cannot change mid-walk.
		RepoIdentity:   RepoIdentityOf(ctx, p.req.RepoPath),
		ProvenanceKind: domain.ProvenanceRepoDerivation,
	}
	p.modules = map[string]*moduleFacts{}
	p.imports = map[string]map[string]struct{}{}

	if err := p.walk(ctx); err != nil {
		return err
	}
	if err := p.finalize(ctx); err != nil {
		return err
	}
	return p.retire(ctx)
}

// walk visits the repository in lexical order — which is what makes the resume
// cursor meaningful — applying every bound as it goes.
func (p *indexPass) walk(ctx context.Context) error {
	p.state.Phase = domain.IndexPhaseScanning
	sinceCheckpoint := 0

	err := filepath.WalkDir(p.repoPath, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			// A path that vanished mid-walk is not a failure: a repository is
			// allowed to change while AO reads it, and the digest ledger will
			// report the path as gone at the end of the pass.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		rel, relErr := repoRelative(p.repoPath, abs)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if rel != "." && p.limits.ignoresDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		p.out.FilesSeen++

		if p.limits.ignoresExt(path.Ext(rel)) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return nil
			}
			return statErr
		}
		if info.Size() > p.limits.MaxFileBytes {
			return nil
		}
		if p.out.FilesAdmitted >= p.limits.MaxFiles {
			p.truncateWalk(fmt.Sprintf("stopped at the %d-file bound", p.limits.MaxFiles))
			return errWalkBudgetReached
		}
		if p.out.BytesRead+info.Size() > p.limits.MaxTotalBytes {
			p.truncateWalk(fmt.Sprintf("stopped at the %s total-read bound", humanBytes(p.limits.MaxTotalBytes)))
			return errWalkBudgetReached
		}

		content, readErr := os.ReadFile(abs) //nolint:gosec // abs comes from WalkDir under the repository root
		if readErr != nil {
			if errors.Is(readErr, fs.ErrNotExist) {
				return nil
			}
			return readErr
		}
		if isBinary(content) {
			return nil
		}
		p.out.FilesAdmitted++
		p.out.BytesRead += int64(len(content))

		if err := p.admit(ctx, rel, content); err != nil {
			return err
		}

		sinceCheckpoint++
		if sinceCheckpoint >= p.limits.CheckpointEvery {
			sinceCheckpoint = 0
			if err := p.checkpoint(ctx, rel); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errWalkBudgetReached) {
		return err
	}
	return nil
}

// truncateWalk records that a bound cut the walk short. It is the only thing
// that suppresses the retire sweep: an incomplete walk cannot tell "this fact
// is gone" from "I never got to it".
func (p *indexPass) truncateWalk(reason string) {
	p.walkTruncated = true
	p.out.Truncated = true
	if p.out.TruncatedReason == "" {
		p.out.TruncatedReason = reason
	}
}

// errWalkBudgetReached stops the walk at a bound without turning the bound
// into a failure. A truncated pass is a successful pass that says it was
// truncated.
var errWalkBudgetReached = errors.New("projectmemory: index bound reached")

// admit records one file: its ledger entry, its module membership, its
// imports, and — unless the resume cursor says a previous pass already did —
// the facts derived from it.
func (p *indexPass) admit(ctx context.Context, rel string, content []byte) error {
	now := p.idx.now()
	digest := hashBytes(content)
	p.digest = append(p.digest, rel+":"+digest)

	role := classifyPath(rel)
	mod := moduleOf(rel)
	facts, ok := p.modules[mod]
	if !ok {
		facts = &moduleFacts{Path: mod, Languages: map[string]int{}}
		p.modules[mod] = facts
	}
	facts.Files++
	facts.Bytes += int64(len(content))
	if ext := strings.ToLower(path.Ext(rel)); ext != "" {
		facts.Languages[ext]++
	}
	if role.derivesItem() && len(facts.Notable) < 12 {
		facts.Notable = append(facts.Notable, rel)
	}

	derived := deriveFile(p.base, rel, role, content)
	if len(derived.Imports) > 0 {
		set, ok := p.imports[mod]
		if !ok {
			set = map[string]struct{}{}
			p.imports[mod] = set
		}
		for _, imp := range derived.Imports {
			set[imp] = struct{}{}
		}
	}
	if role == roleManifest && strings.EqualFold(path.Base(rel), "go.mod") && p.goModulePath == "" {
		p.goModulePath = parseGoModulePath(content)
	}

	// Did a previous incarnation of this same generation already derive this
	// path? If so its facts are already durable at this generation, and
	// re-writing them buys nothing.
	alreadyDerived := p.resumeAfter != "" && rel <= p.resumeAfter
	if alreadyDerived {
		p.out.FilesSkipped++
		return nil
	}

	prior, found, err := p.idx.repo.GetProjectMemoryFile(ctx, p.req.ProjectID, p.repoID, rel)
	if err != nil {
		return err
	}
	unchanged := found && prior.Digest == digest && !p.req.Force

	if err := p.idx.repo.UpsertProjectMemoryFile(ctx, p.req.ProjectID, p.repoID, rel,
		digest, int64(len(content)), p.state.Generation, p.req.Commit, now); err != nil {
		return err
	}
	if unchanged {
		p.out.FilesSkipped++
	} else {
		p.out.FilesIndexed++
	}

	// Facts are written whether or not the digest moved. An unchanged fact
	// costs a reconfirmation (provenance and generation refreshed, updated_at
	// left alone), which is what carries it forward past the retire sweep
	// without pretending it changed.
	if err := p.writeItems(ctx, now, derived.Items...); err != nil {
		return err
	}
	return p.writeRelations(ctx, now, derived.Relations...)
}

func (p *indexPass) writeItems(ctx context.Context, now time.Time, items ...domain.ProjectMemoryItem) error {
	tally, err := putItems(ctx, p.idx.repo, now, items...)
	p.out.ItemsWritten += tally.Written
	p.out.ItemsReconfirmed += tally.Reconfirmed
	p.out.ItemsRefused += tally.Refused
	return err
}

// writeTally counts what a batch of item writes did, so the two pass types can
// share one write path without sharing their outcome structs.
type writeTally struct {
	Written     int
	Reconfirmed int
	Refused     int
}

// putItems writes a batch of facts under the store's CAS fence.
//
// A refusal is counted, not returned: a stale generation means a newer pass
// already wrote this fact, which is exactly what the fence is for. Any other
// error stops the batch, because a pass that cannot write is a pass whose
// remaining output would be incomplete in a way nothing downstream could see.
func putItems(ctx context.Context, repo Repository, now time.Time, items ...domain.ProjectMemoryItem) (writeTally, error) {
	var tally writeTally
	for _, item := range items {
		outcome, err := repo.PutProjectMemoryItem(ctx, item, now)
		switch {
		case errors.Is(err, store.ErrProjectMemoryStaleGeneration):
			tally.Refused++
		case err != nil:
			return tally, fmt.Errorf("write %s: %w", item.Key, err)
		case outcome == store.MemoryWriteReconfirmed:
			tally.Reconfirmed++
		default:
			tally.Written++
		}
	}
	return tally, nil
}

func (p *indexPass) writeRelations(ctx context.Context, now time.Time, rels ...domain.ProjectMemoryRelation) error {
	if len(rels) == 0 {
		return nil
	}
	if err := p.idx.graph.Upsert(ctx, now, rels...); err != nil {
		return err
	}
	p.out.RelationsWritten += len(rels)
	return nil
}

// checkpoint writes the pass's progress durably. A false result means a newer
// pass has claimed the repository, and this one must stop rather than keep
// writing facts nobody will read.
func (p *indexPass) checkpoint(ctx context.Context, cursor string) error {
	p.state.Cursor = cursor
	p.state.FilesSeen = p.out.FilesSeen
	p.state.FilesIndexed = p.out.FilesIndexed
	p.state.FilesSkipped = p.out.FilesSkipped
	p.state.ItemsWritten = p.out.ItemsWritten
	p.state.RelationsWritten = p.out.RelationsWritten
	ok, err := p.idx.repo.AdvanceProjectMemoryIndexPass(ctx, p.state, p.idx.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrPassSuperseded
	}
	return nil
}

// ErrPassSuperseded ends a pass whose generation is no longer current. It is a
// clean stop, not a corruption: everything the pass wrote was fenced, so the
// newer pass's memory is intact.
var ErrPassSuperseded = errors.New("projectmemory: indexing pass superseded by a newer generation")

// finalize writes the facts that are about the repository as a whole rather
// than about any one file: the module census, the module dependency edges, and
// the overview.
//
// They are derived last because they are aggregates. They are also rewritten
// on every pass, which is what lets the retire sweep recognise a module that
// no longer exists — it is the one left behind at an older generation.
func (p *indexPass) finalize(ctx context.Context) error {
	now := p.idx.now()
	p.state.Phase = domain.IndexPhaseSummarizing
	if ok, err := p.idx.repo.AdvanceProjectMemoryIndexPass(ctx, p.state, now); err != nil {
		return err
	} else if !ok {
		return ErrPassSuperseded
	}

	ordered := make([]moduleFacts, 0, len(p.modules))
	for _, m := range p.modules {
		ordered = append(ordered, *m)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Files != ordered[j].Files {
			return ordered[i].Files > ordered[j].Files
		}
		return ordered[i].Path < ordered[j].Path
	})
	if len(ordered) > p.limits.MaxModules {
		ordered = ordered[:p.limits.MaxModules]
		p.out.Truncated = true
		if p.out.TruncatedReason == "" {
			p.out.TruncatedReason = fmt.Sprintf("kept the %d largest modules", p.limits.MaxModules)
		}
	}
	p.out.ModulesDiscovered = len(ordered)

	treeDigest := hashStrings(p.digest)
	kept := map[string]struct{}{}
	for _, m := range ordered {
		kept[m.Path] = struct{}{}
		if err := p.writeItems(ctx, now, moduleItem(p.base, m, treeDigest)); err != nil {
			return err
		}
		if err := p.writeRelations(ctx, now, p.base.relation(
			domain.ProjectMemoryNode{Kind: domain.NodeRepository, Key: p.repoID},
			domain.RelationContains,
			domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: m.Path},
			nil, confidenceStructural,
		)); err != nil {
			return err
		}
	}

	if err := p.writeModuleDependencies(ctx, now, kept); err != nil {
		return err
	}

	if err := p.writeItems(ctx, now, overviewItem(
		p.base, p.repoPath, ordered, p.out.FilesAdmitted, treeDigest,
	)); err != nil {
		return err
	}

	p.state.Phase = domain.IndexPhaseLinking
	p.state.ItemsWritten = p.out.ItemsWritten
	p.state.RelationsWritten = p.out.RelationsWritten
	if ok, err := p.idx.repo.AdvanceProjectMemoryIndexPass(ctx, p.state, now); err != nil {
		return err
	} else if !ok {
		return ErrPassSuperseded
	}
	return nil
}

// writeModuleDependencies resolves the raw import targets collected during the
// walk into module-to-module edges.
//
// An import that does not resolve to a module inside this repository is
// dropped, not guessed at. That is the difference between a dependency graph
// and a plausible-looking one: an edge AO cannot demonstrate is an edge a
// Reviewer would later be misled by.
func (p *indexPass) writeModuleDependencies(ctx context.Context, now time.Time, kept map[string]struct{}) error {
	fromModules := make([]string, 0, len(p.imports))
	for m := range p.imports {
		fromModules = append(fromModules, m)
	}
	sort.Strings(fromModules)

	for _, from := range fromModules {
		if _, ok := kept[from]; !ok {
			continue
		}
		targets := make([]string, 0, len(p.imports[from]))
		for t := range p.imports[from] {
			targets = append(targets, t)
		}
		sort.Strings(targets)

		seen := map[string]struct{}{}
		for _, target := range targets {
			to, ok := resolveImport(from, target, p.goModulePath)
			if !ok || to == from {
				continue
			}
			if _, known := kept[to]; !known {
				// The target resolves to a directory this pass did not admit
				// as a module (below the module cap, or ignored). Asserting an
				// edge to a module with no fact behind it would produce a
				// graph a traversal cannot follow.
				continue
			}
			if _, dup := seen[to]; dup {
				continue
			}
			seen[to] = struct{}{}
			if err := p.writeRelations(ctx, now, p.base.relation(
				domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: from},
				domain.RelationDependsOn,
				domain.ProjectMemoryNode{Kind: domain.NodeModule, Key: to},
				nil, confidenceStructural,
			)); err != nil {
				return err
			}
		}
	}
	return nil
}

// retire closes the pass: it invalidates what disappeared, retires what the
// walk did not re-confirm, and promotes the pending commit.
//
// The order matters. Deletions are detected from the digest ledger — a path
// the walk did not re-observe is gone — and are invalidated by path, so
// everything derived from that path is retired precisely. Only then is the
// generation sweep run, and only when the walk was not truncated: after a
// truncated walk, "not re-confirmed" would include every fact the walk simply
// never reached, and invalidating those would be a lie.
func (p *indexPass) retire(ctx context.Context) error {
	now := p.idx.now()
	p.state.Phase = domain.IndexPhaseFinalizing
	if ok, err := p.idx.repo.AdvanceProjectMemoryIndexPass(ctx, p.state, now); err != nil {
		return err
	} else if !ok {
		return ErrPassSuperseded
	}

	gone, err := p.idx.repo.ListProjectMemoryFilesBelowGeneration(ctx, p.req.ProjectID, p.repoID, p.state.Generation)
	if err != nil {
		return err
	}
	for _, f := range gone {
		items, _, err := p.idx.repo.InvalidateProjectMemoryByPath(ctx, p.req.ProjectID, p.repoID, f.Path,
			domain.MemoryStateInvalidated,
			fmt.Sprintf("source path %s is no longer present at %s", f.Path, orNone(p.req.Commit)), now)
		if err != nil {
			return err
		}
		p.out.ItemsInvalidated += items
		p.out.FilesRemoved++
	}
	if len(gone) > 0 && !p.walkTruncated {
		if _, err := p.idx.repo.PruneProjectMemoryFilesBelowGeneration(
			ctx, p.req.ProjectID, p.repoID, p.state.Generation); err != nil {
			return err
		}
	}

	if !p.walkTruncated {
		items, _, err := p.idx.repo.RetireProjectMemoryBelowGeneration(ctx, p.req.ProjectID, p.repoID,
			p.state.Generation,
			fmt.Sprintf("not re-derived by the full pass at generation %d", p.state.Generation), now)
		if err != nil {
			return err
		}
		p.out.ItemsRetired += items
	}

	p.state.FilesSeen = p.out.FilesSeen
	p.state.FilesIndexed = p.out.FilesIndexed
	p.state.FilesSkipped = p.out.FilesSkipped
	p.state.ItemsWritten = p.out.ItemsWritten
	p.state.RelationsWritten = p.out.RelationsWritten
	ok, err := p.idx.repo.CompleteProjectMemoryIndexPass(ctx, p.state, now)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPassSuperseded
	}
	return nil
}

// --- helpers ----------------------------------------------------------------

// canonicalRepoPath resolves the repository root to the absolute, symlink-free
// path the repository identity is derived from.
//
// Resolving symlinks matters: two paths that reach the same checkout must
// produce the same RepoID, or the same repository would be indexed twice into
// two disjoint memories.
func canonicalRepoPath(repoPath string) (string, error) {
	trimmed := strings.TrimSpace(repoPath)
	if trimmed == "" {
		return "", errors.New("projectmemory: repository path is required")
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("projectmemory: resolve repository path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// A path that cannot be resolved (it moved, or a parent is a broken
		// link) is reported rather than silently indexed under a different
		// identity than the one it will have next time.
		return "", fmt.Errorf("projectmemory: resolve repository path %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("projectmemory: stat repository path %s: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("projectmemory: repository path %s is not a directory", resolved)
	}
	return resolved, nil
}

// repoRelative renders an absolute path as the slash-separated, repo-relative
// form every memory key and source path uses. Normalising the separator here
// is what lets a memory written on Windows be read on macOS.
func repoRelative(root, abs string) (string, error) {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", fmt.Errorf("projectmemory: relativise %s against %s: %w", abs, root, err)
	}
	return filepath.ToSlash(rel), nil
}

// isBinary reports whether a file's head contains a NUL byte, which no text
// format this indexer understands does. It is checked in addition to the
// extension list, because a `.dat` renamed to `.txt` is still not text.
func isBinary(content []byte) bool {
	const sniff = 8000
	head := content
	if len(head) > sniff {
		head = head[:sniff]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hashStrings(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
