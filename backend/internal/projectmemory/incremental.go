package projectmemory

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// incremental.go — updating memory from a change set rather than a walk.
//
// This is where the actual incrementality lives. A full pass (indexer.go) has
// to read every admitted file, because a digest cannot be computed without
// reading; what it saves is derivation and writes. UpdateChanged reads only
// the paths a change set names, which is the difference between "seconds after
// every task" and "a full scan after every task".
//
// The rule it must not break: **memory that cannot be shown to still hold is
// never served as authoritative.** So an incremental pass invalidates first and
// re-derives second. If it dies between the two, what is left behind is stale
// memory that is correctly marked stale — the safe direction. The unsafe
// direction, re-deriving before invalidating, would leave a window where a
// fact about a deleted file still reads as valid.
//
// An incremental pass deliberately does NOT run the generation retire sweep.
// The sweep means "a walk saw everything and did not re-confirm this", and an
// incremental pass saw only what the diff named. Applying it here would
// invalidate every fact the diff happened not to mention.

// ChangeKind is what happened to one path.
type ChangeKind string

// Change kinds, matching git's name-status vocabulary.
const (
	ChangeAdded    ChangeKind = "added"
	ChangeModified ChangeKind = "modified"
	ChangeDeleted  ChangeKind = "deleted"
	ChangeRenamed  ChangeKind = "renamed"
)

// PathChange is one path's change, as a diff reported it.
type PathChange struct {
	// Kind is what happened.
	Kind ChangeKind
	// Path is the path as it stands after the change. For a deletion it is
	// the path that is gone.
	Path string
	// PreviousPath is where a renamed path used to be, and is empty for every
	// other kind. It matters because the memory derived from a file follows
	// the file: a rename must retire the old key and derive the new one, not
	// leave a fact filed under a path that no longer exists.
	PreviousPath string
}

// UpdateRequest asks for one incremental update.
type UpdateRequest struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// ToCommit and Branch are the provenance the refreshed facts carry.
	ToCommit string
	Branch   string
	// Changes is the change set. An empty change set is valid and cheap: the
	// pass confirms there is nothing to do and advances the indexed commit,
	// which is what "the branch moved but touched nothing we index" should
	// cost.
	Changes []PathChange
	// Limits bounds what the pass will read. A zero value means the indexer's
	// configured bounds.
	Limits IndexLimits
}

// UpdateOutcome reports what an incremental pass did.
type UpdateOutcome struct {
	RepoID     string
	Generation int64
	Skipped    bool
	SkipReason string
	// FellBackToFullIndex reports that the repository had never completed a
	// pass, so there was no baseline to update and a full index ran instead.
	FellBackToFullIndex bool
	FallbackReason      string

	ChangesApplied   int
	PathsRead        int
	PathsGone        int
	ItemsWritten     int
	ItemsReconfirmed int
	ItemsInvalidated int64
	ItemsRefused     int
	// RenamesFollowed counts knowledge items whose evidence was carried from a
	// moved file to its new path rather than retired with it (P2-D section 10).
	RenamesFollowed  int
	RelationsWritten int
	ModulesRefreshed int
	IndexedCommit    string
	Duration         time.Duration
}

// UpdateChanged applies one change set to a repository's memory.
func (idx *Indexer) UpdateChanged(ctx context.Context, req UpdateRequest) (UpdateOutcome, error) {
	started := idx.now()
	limits := req.Limits
	if limits.isZero() {
		limits = idx.limits
	}
	limits = limits.Normalized()

	repoPath, err := canonicalRepoPath(req.RepoPath)
	if err != nil {
		return UpdateOutcome{}, err
	}
	repoID := domain.ProjectMemoryRepoID(repoPath)
	out := UpdateOutcome{RepoID: repoID}

	if err := idx.repo.EnsureProjectMemoryRepo(ctx, req.ProjectID, repoID, repoPath, started); err != nil {
		return out, err
	}
	state, found, err := idx.repo.GetProjectMemoryIndexState(ctx, req.ProjectID, repoID)
	if err != nil {
		return out, err
	}
	if !found || state.IndexedCommit == "" {
		// There is no baseline to update. Falling back to a full index is the
		// only honest option: pretending a change set describes the whole
		// repository would produce memory with holes AO could not detect.
		full, err := idx.Index(ctx, IndexRequest{
			ProjectID: req.ProjectID, RepoPath: repoPath,
			Commit: req.ToCommit, Branch: req.Branch, Limits: limits,
		})
		out.FellBackToFullIndex = true
		out.FallbackReason = "this repository has no completed index to update"
		out.RepoID = full.RepoID
		out.Generation = full.Generation
		out.Skipped = full.Skipped
		out.SkipReason = full.SkipReason
		out.ItemsWritten = full.ItemsWritten
		out.ItemsReconfirmed = full.ItemsReconfirmed
		out.RelationsWritten = full.RelationsWritten
		out.IndexedCommit = full.IndexedCommit
		out.Duration = idx.now().Sub(started)
		return out, err
	}

	claimed, ok, err := idx.repo.ClaimProjectMemoryIndexPass(ctx, req.ProjectID, repoID, req.ToCommit, req.Branch, started)
	if err != nil {
		return out, err
	}
	if !ok {
		out.Skipped = true
		out.SkipReason = "another indexing pass holds this repository"
		return out, nil
	}
	out.Generation = claimed.Generation

	up := &updatePass{
		idx: idx, req: req, limits: limits,
		repoPath: repoPath, repoID: repoID, state: claimed, out: &out,
	}
	if err := up.run(ctx); err != nil {
		reason := err.Error()
		if _, ferr := idx.repo.FailProjectMemoryIndexPass(ctx, req.ProjectID, repoID, claimed.Generation, reason, idx.now()); ferr != nil {
			return out, errors.Join(err, ferr)
		}
		return out, err
	}
	out.IndexedCommit = req.ToCommit
	out.Duration = idx.now().Sub(started)
	return out, nil
}

type updatePass struct {
	idx      *Indexer
	req      UpdateRequest
	limits   IndexLimits
	repoPath string
	repoID   string
	state    domain.ProjectMemoryIndexState
	out      *UpdateOutcome

	base itemBase
	// touchedModules are the modules whose census the change set could have
	// moved. Only these are re-derived, which is the whole point.
	touchedModules map[string]struct{}
	imports        map[string]map[string]struct{}
	goModulePath   string
}

func (p *updatePass) run(ctx context.Context) error {
	p.base = itemBase{
		ProjectID:  p.req.ProjectID,
		RepoID:     p.repoID,
		Commit:     p.req.ToCommit,
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
	p.touchedModules = map[string]struct{}{}
	p.imports = map[string]map[string]struct{}{}

	changes := normalizeChanges(p.req.Changes)
	for _, ch := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.apply(ctx, ch); err != nil {
			return err
		}
		p.out.ChangesApplied++
	}

	if err := p.refreshModules(ctx); err != nil {
		return err
	}

	p.state.Phase = domain.IndexPhaseFinalizing
	p.state.ItemsWritten = p.out.ItemsWritten
	p.state.RelationsWritten = p.out.RelationsWritten
	if ok, err := p.idx.repo.AdvanceProjectMemoryIndexPass(ctx, p.state, p.idx.now()); err != nil {
		return err
	} else if !ok {
		return ErrPassSuperseded
	}
	ok, err := p.idx.repo.CompleteProjectMemoryIndexPass(ctx, p.state, p.idx.now())
	if err != nil {
		return err
	}
	if !ok {
		return ErrPassSuperseded
	}
	return nil
}

// apply handles one changed path: retire what it disproves, then derive what
// it now says. The order is the fail-closed rule (see the file comment).
func (p *updatePass) apply(ctx context.Context, ch PathChange) error {
	now := p.idx.now()

	// A rename retires the old location first: whatever was filed under the
	// previous path is a fact about a path that no longer exists.
	//
	// P2-D section 10: with ONE exception, which is the difference between a
	// rename costing a re-derivation and a rename destroying knowledge. A
	// repository derivation (a file summary, a module census) is re-derived
	// from the new path a few lines below, so retiring it loses nothing. A
	// decision or a risk is not: nothing re-derives it, ever, so retiring it
	// because a file moved would silently delete the project's own reasoning
	// on the grounds that git renamed a file.
	//
	// The rename is followed only where git PROVED it -- PreviousPath is set
	// exactly when the diff reported an `R` status, never inferred from a
	// delete plus an add that happen to look alike. An unproven rename takes
	// the retire-and-rederive path below, which is what section 10 asks for.
	var carried []domain.ProjectMemoryItem
	if ch.PreviousPath != "" {
		// Read the knowledge anchored on the old path BEFORE anything retires
		// it, and re-anchor it AFTER the new path's own invalidation below.
		// The order is forced: retirePath withholds by old path and the
		// invalidation a few lines down withholds by new path, so a rewrite
		// done at either point would immediately be undone by the other.
		var err error
		if carried, err = p.knowledgeAnchoredOn(ctx, ch.PreviousPath); err != nil {
			return err
		}
		if err := p.retirePath(ctx, ch.PreviousPath, now,
			fmt.Sprintf("renamed to %s at %s", ch.Path, orNone(p.req.ToCommit))); err != nil {
			return err
		}
		p.touchedModules[moduleOf(ch.PreviousPath)] = struct{}{}
	}

	if ch.Kind == ChangeDeleted {
		if err := p.retirePath(ctx, ch.Path, now,
			fmt.Sprintf("deleted at %s", orNone(p.req.ToCommit))); err != nil {
			return err
		}
		p.touchedModules[moduleOf(ch.Path)] = struct{}{}
		return nil
	}

	// Everything derived from the previous version of this file stops being
	// authoritative before the new version is read. If the pass dies here, the
	// memory is stale-and-marked-stale, which is the safe state.
	items, _, err := p.idx.repo.InvalidateProjectMemoryByPath(ctx, p.req.ProjectID, p.repoID, ch.Path,
		domain.MemoryStateStale,
		fmt.Sprintf("source changed at %s", orNone(p.req.ToCommit)), now)
	if err != nil {
		return err
	}
	p.out.ItemsInvalidated += items

	if err := p.reanchorRenamedKnowledge(ctx, carried, ch.PreviousPath, ch.Path, now); err != nil {
		return err
	}

	content, ok, err := p.readAdmitted(ch.Path)
	if err != nil {
		return err
	}
	if !ok {
		// The diff says it exists; the file system disagrees, or the bounds
		// exclude it. Either way there is nothing to re-derive, and the
		// invalidation above already recorded that the old fact cannot be
		// proven.
		p.out.PathsGone++
		if _, err := p.idx.repo.DeleteProjectMemoryFile(ctx, p.req.ProjectID, p.repoID, ch.Path); err != nil {
			return err
		}
		p.touchedModules[moduleOf(ch.Path)] = struct{}{}
		return nil
	}
	p.out.PathsRead++
	p.touchedModules[moduleOf(ch.Path)] = struct{}{}

	role := classifyPath(ch.Path)
	if role == roleManifest && strings.EqualFold(path.Base(ch.Path), "go.mod") {
		p.goModulePath = parseGoModulePath(content)
	}
	if err := p.idx.repo.UpsertProjectMemoryFile(ctx, p.req.ProjectID, p.repoID, ch.Path,
		hashBytes(content), int64(len(content)), p.state.Generation, p.req.ToCommit, now); err != nil {
		return err
	}

	derived := deriveFile(p.base, ch.Path, role, content)
	if len(derived.Imports) > 0 {
		mod := moduleOf(ch.Path)
		set, ok := p.imports[mod]
		if !ok {
			set = map[string]struct{}{}
			p.imports[mod] = set
		}
		for _, imp := range derived.Imports {
			set[imp] = struct{}{}
		}
	}
	if err := p.writeItems(ctx, now, derived.Items...); err != nil {
		return err
	}
	return p.writeRelations(ctx, now, derived.Relations...)
}

// retirePath invalidates everything derived from a path that is gone and drops
// its ledger entry, so a later full pass does not rediscover the same deletion.
// knowledgeAnchoredOn reads the facts a rename must carry rather than retire
// (P2-D section 10).
//
// It is the three types nothing re-derives -- task results, decisions and known
// risks -- and nothing else. A repository derivation is left to the
// retire-and-rederive path, because re-deriving a summary from the file at its
// new path is strictly better than rewriting a path on a summary of the old
// one. A decision is different: no pass will ever produce it again, so
// retiring it because git moved a file would silently delete the project's own
// reasoning.
//
// Only facts that are currently valid are carried. A fact already withheld is
// not made current again by a file moving.
func (p *updatePass) knowledgeAnchoredOn(ctx context.Context, from string) ([]domain.ProjectMemoryItem, error) {
	items, err := p.idx.repo.ListProjectMemoryItemsByPath(ctx, p.req.ProjectID, p.repoID, from)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ProjectMemoryItem, 0, len(items))
	for _, item := range items {
		if domain.IsSharedKnowledgeType(item.Key.Type) && item.State == domain.MemoryStateValid {
			out = append(out, item)
		}
	}
	return out, nil
}

// reanchorRenamedKnowledge points carried facts at the file's new location.
//
// It rewrites EVIDENCE, never content. The decision still says what it said;
// only where AO will look to check whether it still holds moves. That is the
// whole safety argument: a rename proves a path changed and proves nothing at
// all about whether the reasoning is still correct, so the fact stays exactly
// as provable, and exactly as disprovable, as it was.
//
// It runs after the new path's own invalidation, which is what makes the
// carried fact end up valid rather than immediately withheld again by the
// change that accompanied the move.
//
// Best-effort per item: a rewrite the CAS refuses belongs to a newer
// generation, and leaving that item retired is the conservative outcome.
func (p *updatePass) reanchorRenamedKnowledge(
	ctx context.Context, carried []domain.ProjectMemoryItem, from, to string, now time.Time,
) error {
	for _, item := range carried {
		rewritten := false
		paths := make([]string, 0, len(item.SourcePaths))
		for _, sp := range item.SourcePaths {
			if sp == from {
				sp, rewritten = to, true
			}
			paths = append(paths, sp)
		}
		if !rewritten {
			continue
		}
		next := item
		next.SourcePaths = paths
		next.State = domain.MemoryStateValid
		next.StateReason = ""
		next.InvalidatedAt = time.Time{}
		// The digest is NOT carried over. It was computed over the file at the
		// old path, and git reports a rename at any similarity above its
		// threshold rather than only at 100%. Keeping it would let drift
		// detection compare the new file against a hash of something else and
		// report a change that is really a move. Clearing it makes the item
		// unverifiable-but-present, which is the honest state and is how these
		// types are treated everywhere else.
		next.SourceDigest = ""
		next = next.Normalized()
		if _, err := p.idx.repo.PutProjectMemoryItem(ctx, next, now); err != nil {
			if errors.Is(err, store.ErrProjectMemoryStaleGeneration) {
				continue
			}
			return err
		}
		p.out.RenamesFollowed++
	}
	return nil
}

func (p *updatePass) retirePath(ctx context.Context, rel string, now time.Time, reason string) error {
	items, _, err := p.idx.repo.InvalidateProjectMemoryByPath(ctx, p.req.ProjectID, p.repoID, rel,
		domain.MemoryStateInvalidated, reason, now)
	if err != nil {
		return err
	}
	p.out.ItemsInvalidated += items
	p.out.PathsGone++
	_, err = p.idx.repo.DeleteProjectMemoryFile(ctx, p.req.ProjectID, p.repoID, rel)
	return err
}

// readAdmitted reads one path if the bounds admit it. A path the bounds
// exclude is reported as absent rather than read, so incremental update and
// the full walk agree about what is in memory.
func (p *updatePass) readAdmitted(rel string) ([]byte, bool, error) {
	if p.limits.ignoresExt(path.Ext(rel)) {
		return nil, false, nil
	}
	for _, seg := range strings.Split(rel, "/") {
		if p.limits.ignoresDir(seg) {
			return nil, false, nil
		}
	}
	abs := filepath.Join(p.repoPath, filepath.FromSlash(rel))
	// Refuse a path that escapes the repository root. A diff is data, and a
	// crafted rename target must not turn into a read outside the checkout.
	if !strings.HasPrefix(abs, p.repoPath+string(os.PathSeparator)) {
		return nil, false, nil
	}
	info, err := os.Stat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("stat %s: %w", rel, err)
	case info.IsDir(), !info.Mode().IsRegular():
		return nil, false, nil
	case info.Size() > p.limits.MaxFileBytes:
		return nil, false, nil
	}
	content, err := os.ReadFile(abs) //nolint:gosec // abs is confined to the repository root above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", rel, err)
	}
	if isBinary(content) {
		return nil, false, nil
	}
	return content, true, nil
}

// refreshModules re-derives the census of the modules the change set touched,
// and the repository overview.
//
// The census comes from the durable file ledger rather than from a fresh walk:
// the ledger already knows every path the repository has, so "how many files
// does this module have now" is a read, not a scan. That is the second half of
// what makes an incremental pass cheap.
func (p *updatePass) refreshModules(ctx context.Context) error {
	if len(p.touchedModules) == 0 && len(p.req.Changes) > 0 {
		return nil
	}
	now := p.idx.now()
	ledger, err := p.idx.repo.ListProjectMemoryFiles(ctx, p.req.ProjectID, p.repoID)
	if err != nil {
		return err
	}

	census := map[string]*moduleFacts{}
	digests := make([]string, 0, len(ledger))
	for _, f := range ledger {
		digests = append(digests, f.Path+":"+f.Digest)
		mod := moduleOf(f.Path)
		facts, ok := census[mod]
		if !ok {
			facts = &moduleFacts{Path: mod, Languages: map[string]int{}}
			census[mod] = facts
		}
		facts.Files++
		facts.Bytes += f.Size
		if ext := strings.ToLower(path.Ext(f.Path)); ext != "" {
			facts.Languages[ext]++
		}
		if classifyPath(f.Path).derivesItem() && len(facts.Notable) < 12 {
			facts.Notable = append(facts.Notable, f.Path)
		}
	}
	sort.Strings(digests)
	treeDigest := hashStrings(digests)

	touched := make([]string, 0, len(p.touchedModules))
	for m := range p.touchedModules {
		touched = append(touched, m)
	}
	sort.Strings(touched)

	for _, mod := range touched {
		facts, ok := census[mod]
		if !ok {
			// Every file of this module is gone. The module fact is retired
			// rather than rewritten as an empty one: a module with no files is
			// not a module.
			if err := p.retireModule(ctx, mod, now); err != nil {
				return err
			}
			continue
		}
		if err := p.writeItems(ctx, now, moduleItem(p.base, *facts, treeDigest)); err != nil {
			return err
		}
		p.out.ModulesRefreshed++
	}

	if err := p.writeModuleDependencies(ctx, now, census); err != nil {
		return err
	}

	ordered := make([]moduleFacts, 0, len(census))
	for _, m := range census {
		ordered = append(ordered, *m)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Files != ordered[j].Files {
			return ordered[i].Files > ordered[j].Files
		}
		return ordered[i].Path < ordered[j].Path
	})
	return p.writeItems(ctx, now, overviewItem(p.base, p.repoPath, ordered, len(ledger), treeDigest))
}

// retireModule invalidates the fact for a module whose files have all gone.
func (p *updatePass) retireModule(ctx context.Context, mod string, now time.Time) error {
	key := domain.ProjectMemoryKey{
		ProjectID: p.req.ProjectID, RepoID: p.repoID,
		Type: domain.MemoryTypeModule, Scope: domain.MemoryScopeModule, Key: mod,
	}
	existing, found, err := p.idx.repo.GetProjectMemoryItem(ctx, key.ID())
	if err != nil || !found {
		return err
	}
	if existing.State == domain.MemoryStateInvalidated {
		return nil
	}
	ok, err := p.idx.repo.MarkProjectMemoryItemState(ctx, existing.ID, p.state.Generation,
		domain.MemoryStateInvalidated,
		fmt.Sprintf("module %s has no indexed files at %s", mod, orNone(p.req.ToCommit)), now)
	if err != nil {
		return err
	}
	if ok {
		p.out.ItemsInvalidated++
	}
	return nil
}

func (p *updatePass) writeModuleDependencies(ctx context.Context, now time.Time, census map[string]*moduleFacts) error {
	fromModules := make([]string, 0, len(p.imports))
	for m := range p.imports {
		fromModules = append(fromModules, m)
	}
	sort.Strings(fromModules)

	for _, from := range fromModules {
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
			if _, known := census[to]; !known {
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

func (p *updatePass) writeItems(ctx context.Context, now time.Time, items ...domain.ProjectMemoryItem) error {
	tally, err := putItems(ctx, p.idx.repo, now, items...)
	p.out.ItemsWritten += tally.Written
	p.out.ItemsReconfirmed += tally.Reconfirmed
	p.out.ItemsRefused += tally.Refused
	return err
}

func (p *updatePass) writeRelations(ctx context.Context, now time.Time, rels ...domain.ProjectMemoryRelation) error {
	if len(rels) == 0 {
		return nil
	}
	if err := p.idx.graph.Upsert(ctx, now, rels...); err != nil {
		return err
	}
	p.out.RelationsWritten += len(rels)
	return nil
}

// normalizeChanges de-duplicates a change set and orders it deterministically.
//
// Deletions and renames are applied before additions and modifications, so a
// rename reported as delete-plus-add cannot retire the path the add just
// wrote. Within each group the order is by path, which makes an update
// reproducible.
func normalizeChanges(in []PathChange) []PathChange {
	seen := map[string]struct{}{}
	out := make([]PathChange, 0, len(in))
	for _, ch := range in {
		ch.Path = strings.TrimSpace(filepath.ToSlash(ch.Path))
		ch.PreviousPath = strings.TrimSpace(filepath.ToSlash(ch.PreviousPath))
		if ch.Path == "" {
			continue
		}
		if ch.Kind == "" {
			ch.Kind = ChangeModified
		}
		key := string(ch.Kind) + "\x00" + ch.Path + "\x00" + ch.PreviousPath
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ch)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ai, aj := changeOrder(out[i].Kind), changeOrder(out[j].Kind)
		if ai != aj {
			return ai < aj
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func changeOrder(k ChangeKind) int {
	switch k {
	case ChangeDeleted:
		return 0
	case ChangeRenamed:
		return 1
	default:
		return 2
	}
}

// ChangesSinceCommit asks git what changed between two commits.
//
// It is the production source of a change set, and it is deliberately the same
// shape of call contextrouter's GitDiffSource already makes: name-status with
// rename detection, no patch bodies. AO never needs the diff's contents here —
// it re-reads the files it cares about from the working tree, at the commit it
// is indexing.
//
// A repository that is not a git checkout, or a commit that is not reachable,
// returns an error the caller degrades on: the honest answer is "I cannot
// enumerate the change set", and the correct response is a full pass, not a
// guess about what moved.
func ChangesSinceCommit(ctx context.Context, repoPath, fromCommit, toCommit string) ([]PathChange, error) {
	from := strings.TrimSpace(fromCommit)
	to := strings.TrimSpace(toCommit)
	if from == "" {
		return nil, errors.New("projectmemory: a change set needs a commit to diff from")
	}
	if to == "" {
		to = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-status", "--find-renames", from, to)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("projectmemory: git diff %s..%s in %s: %w", from, to, repoPath, err)
	}
	return parseNameStatus(string(out)), nil
}

// parseNameStatus reads git's --name-status output into a change set.
func parseNameStatus(out string) []PathChange {
	var changes []PathChange
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		switch {
		case strings.HasPrefix(status, "R") && len(fields) >= 3:
			changes = append(changes, PathChange{
				Kind: ChangeRenamed, PreviousPath: fields[1], Path: fields[2],
			})
		case strings.HasPrefix(status, "A"):
			changes = append(changes, PathChange{Kind: ChangeAdded, Path: fields[1]})
		case strings.HasPrefix(status, "D"):
			changes = append(changes, PathChange{Kind: ChangeDeleted, Path: fields[1]})
		case strings.HasPrefix(status, "C") && len(fields) >= 3:
			// A copy leaves the source in place, so only the destination is
			// new information.
			changes = append(changes, PathChange{Kind: ChangeAdded, Path: fields[2]})
		default:
			changes = append(changes, PathChange{Kind: ChangeModified, Path: fields[1]})
		}
	}
	return changes
}
