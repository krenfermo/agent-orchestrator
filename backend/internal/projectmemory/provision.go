package projectmemory

import (
	"context"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	baseline "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
)

// provision.go — the one call a dispatch boundary makes (P2-B).
//
// Everything P2-B adds converges here: the lifecycle sync trigger, the pack,
// the role budget, dedupe against legacy context, the cache, the token
// estimate, and the observability record. A boundary calls Provision and gets
// back what to send; it does not decide any of the policy itself.
//
// That concentration is the design, not an accident. Planner, Worker, Reviewer
// and Repair each reach memory at a different moment with different evidence,
// and if each assembled its own context there would be four opinions about
// when a sync is worth it, four budget interpretations, and four places for the
// fail-closed rule to be forgotten. There is one.
//
// The invariant every path here preserves: **Provision never fails.** It
// returns a Provisioned whose Pack may be empty and whose FallbackReason says
// why, and the caller sends its legacy context exactly as it would have before
// P2-B. There is no failure of project memory that should stop a dispatch —
// not a missing index, not a sync timeout, not an unreadable database, not a
// corrupted item.

// ProvisionRequest is one boundary's ask.
type ProvisionRequest struct {
	ProjectID domain.ProjectID
	// RepoPath is the checkout this dispatch is about. A Planner may leave it
	// empty to span every repository of the project.
	RepoPath string
	// Role decides the section order, the budget and the dedupe allowance.
	Role PackRole
	// ChangedPaths, Modules and Keywords are the relevance evidence, strongest
	// first. A boundary supplies what it actually knows and nothing more —
	// inventing a changed path would misrank the pack.
	ChangedPaths []string
	Modules      []string
	Keywords     []string
	// TaskRef admits that task's own unintegrated memory, and only that
	// task's.
	TaskRef string
	// WorkflowRunID and UpstreamTaskRefs are the shared-knowledge authority
	// (P2-C §14, §15). The run scopes workflow-local facts, and the upstream
	// refs are the tasks this one explicitly depends on and may therefore read
	// the verified, not-yet-integrated knowledge of.
	//
	// A boundary that does not know them supplies neither, and the pack falls
	// back to canonical knowledge alone — which is the safe direction: the
	// failure mode of an unsupplied dependency list is a task that learns less
	// than it could, never one that reads a sibling's unmerged work.
	WorkflowRunID    string
	UpstreamTaskRefs []string
	// Legacy is the context the dispatch was going to send anyway. It is used
	// for dedupe and for the honest before/after byte counts; it is never
	// modified in place.
	Legacy []LegacyDocument
	// TaskBytes is the size of the task/objective text, which memory never
	// replaces. It is carried only so the metrics can report the whole
	// AO-assembled context rather than the part memory touched.
	TaskBytes int
	// SkipSync asks for the pack without a freshness check. It is how a
	// boundary that has already synced this exact state — a Worker launched
	// moments after the Planner that planned it — avoids asking again.
	SkipSync bool
	// HeadSHA is the commit this role is reasoning about, when the boundary
	// knows it (P2-D section 17). For a reviewer it is the reviewed SHA.
	//
	// It is recorded on the context manifest and used for nothing else: memory
	// selection is not narrowed by it, because a pack assembled differently
	// for a reviewer than for the worker whose work is being reviewed would
	// make the two harder to compare rather than easier. What it buys is that
	// "the pack was built for SHA A and the reviewer judged SHA B" becomes a
	// diagnosable fact instead of an unexplained disagreement.
	//
	// Empty is honest and common: a planner is not reasoning about one commit.
	HeadSHA string
}

// Provisioned is what a boundary should send.
type Provisioned struct {
	// Mode is the rollout stage that produced this.
	Mode MemoryMode
	// Pack is the memory to attach. Empty means attach nothing.
	Pack ContextPack
	// Legacy is the documents that survived dedupe, in the caller's order.
	// In ModeAssisted it is always the full input.
	Legacy []LegacyDocument
	// Dedupe is what was decided about each legacy document.
	Dedupe DedupeResult
	// Freshness is what the sync check did.
	Freshness Freshness
	// Metrics is the observability record for this dispatch.
	Metrics baseline.MemoryMetrics
}

// Attached reports whether memory contributed anything.
func (p Provisioned) Attached() bool { return !p.Pack.Empty() }

// Render returns the memory text to attach, or the empty string when there is
// nothing to attach.
func (p Provisioned) Render() string {
	if p.Pack.Empty() {
		return ""
	}
	return p.Pack.Render()
}

// Provisioner assembles memory for dispatch boundaries.
type Provisioner struct {
	svc    *Service
	syncer *Syncer
	cache  *PackCache
	cfg    Config
	now    func() time.Time
}

// NewProvisioner builds the boundary-facing provisioner.
func NewProvisioner(svc *Service, cfg Config) *Provisioner {
	return &Provisioner{
		svc:    svc,
		syncer: NewSyncer(svc, cfg),
		cache:  NewPackCache(cfg.CacheEnabled),
		cfg:    cfg,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Config returns the policy in force, for operator surfaces.
func (p *Provisioner) Config() Config { return p.cfg }

// Syncer exposes the lifecycle syncer, so a workflow boundary that needs to
// invalidate on a mutation can reach it without a second construction.
func (p *Provisioner) Syncer() *Syncer { return p.syncer }

// CacheStats reports the pack cache's counters.
func (p *Provisioner) CacheStats() CacheStats { return p.cache.Stats() }

// Provision performs the freshness check and assembles the role's memory.
func (p *Provisioner) Provision(ctx context.Context, req ProvisionRequest) Provisioned {
	role := req.Role
	if !role.Valid() {
		role = RoleWorker
	}
	budget := p.cfg.Budgets.For(role)
	out := Provisioned{
		Mode:   p.cfg.Mode,
		Legacy: req.Legacy,
		Metrics: baseline.MemoryMetrics{
			Mode: string(p.cfg.Mode), Role: string(role),
			LegacyBytes: legacyBytes(req.Legacy), TaskBytes: req.TaskBytes,
		},
	}
	out.Metrics.ContextBytes = out.Metrics.LegacyBytes + out.Metrics.TaskBytes

	if !p.cfg.Mode.Enabled() {
		out.Metrics.FallbackReason = "project memory is switched off"
		out.Metrics.FallbackBytes = out.Metrics.LegacyBytes
		return out
	}

	// 1. Freshness. A boundary that already knows the state is current skips
	//    it; everyone else coalesces onto at most one real sync.
	if !req.SkipSync && req.RepoPath != "" {
		out.Freshness = p.syncer.EnsureFresh(ctx, req.ProjectID, req.RepoPath)
	} else if req.RepoPath != "" {
		out.Freshness = Freshness{Kind: SyncSkipped, Reason: "the caller had already synced this state"}
	}
	out.Metrics.SyncKind = string(out.Freshness.Kind)
	out.Metrics.SyncPerformed = out.Freshness.Kind == SyncIncremental || out.Freshness.Kind == SyncFull
	out.Metrics.SyncFilesRead = out.Freshness.FilesRead
	out.Metrics.SyncMillis = out.Freshness.Duration.Milliseconds()
	out.Metrics.Generation = out.Freshness.Generation
	out.Metrics.IndexedCommit = out.Freshness.IndexedCommit

	// 2. The pack. In a mode that may replace, the documents this dispatch is
	//    carrying are handed to selection so the facts that can pay for
	//    themselves rank first.
	var coverablePaths []string
	if p.cfg.Mode.MayReplace() && budget.MaxDocuments > 0 {
		coverablePaths = make([]string, 0, len(req.Legacy))
		for _, doc := range req.Legacy {
			if doc.SHA256 != "" {
				coverablePaths = append(coverablePaths, doc.Path)
			}
		}
	}

	//    It is cached only against an authority strong enough to prove a hit is
	//    the same answer — and the coverable set is part of that authority,
	//    because it changes what selection returns.
	key := CacheKey{
		ProjectID: req.ProjectID, RepoID: out.Freshness.RepoID,
		IndexedCommit: out.Freshness.IndexedCommit, Generation: out.Freshness.Generation,
		// P2-D: the epoch that moves on every out-of-band demotion. Without it
		// a pack cached before an invalidation or an authority pass stays
		// reachable and keeps serving a fact AO has just withheld -- see
		// CacheKey's doc comment.
		ChangeMark: p.changeMark(ctx, req.ProjectID, out.Freshness.RepoID),
		Role:       role, PolicyVersion: PackPolicyVersion, Budget: budget,
		// The sharing authority is part of the cache key. Two dispatches that
		// differ only in which upstream tasks they may read are entitled to
		// different packs, and a cache that ignored that would serve one
		// task's authorized knowledge to a sibling that has none.
		Scope: ScopeDigest(req.ChangedPaths, req.Modules,
			append(append([]string(nil), req.Keywords...), coverablePaths...),
			req.TaskRef+"|"+req.WorkflowRunID+"|"+strings.Join(req.UpstreamTaskRefs, ",")),
	}
	out.Metrics.CacheKey = key.String()

	pack, hit := p.cache.Get(key)
	if hit {
		out.Metrics.CacheHit = true
	} else {
		pack = p.svc.Context(ctx, PackRequest{
			ProjectID: req.ProjectID, RepoPath: req.RepoPath, Role: role,
			ChangedPaths: req.ChangedPaths, Modules: req.Modules,
			Keywords: req.Keywords, TaskRef: req.TaskRef,
			WorkflowRunID: req.WorkflowRunID, UpstreamTaskRefs: req.UpstreamTaskRefs,
			CoverablePaths: coverablePaths,
			Budget:         budget.packBudget(),
		})
		p.cache.Put(key, pack)
	}
	out.Pack = pack

	out.Metrics.PackItems = pack.Stats.SelectedItems
	out.Metrics.PackBytes = pack.Stats.SelectedBytes
	out.Metrics.PackCandidates = pack.Stats.CandidateItems
	out.Metrics.PackRejectedByBudget = pack.Stats.DroppedItems
	out.Metrics.PackReducedToSummary = pack.Stats.DroppedToSummary
	out.Metrics.PackStaleExcluded = pack.Stats.StaleExcluded
	out.Metrics.EstimatedPackTokens = pack.Stats.SelectedTokens
	out.Metrics.PackDigest = pack.Digest
	out.Metrics.SharedCandidates = pack.Stats.SharedCandidates
	out.Metrics.SharedSelected = pack.Stats.SharedSelected
	out.Metrics.SharedIrrelevantExcluded = pack.Stats.SharedIrrelevantExcluded
	out.Metrics.SharedUnauthorizedExcluded = pack.Stats.SharedUnauthorizedExcluded
	out.Metrics.SupersededExcluded = pack.Stats.SupersededExcluded
	out.Metrics.ConflictingExcluded = pack.Stats.ConflictingExcluded
	out.Metrics.DecisionsSelected = pack.Stats.DecisionsSelected
	out.Metrics.RisksSelected = pack.Stats.RisksSelected
	out.Metrics.TaskLocalItems = pack.Stats.TaskLocalSelected
	out.Metrics.WorkflowLocalItems = pack.Stats.WorkflowLocalSelected
	out.Metrics.CanonicalItems = pack.Stats.CanonicalSelected
	out.Metrics.KnowledgeBytes = pack.Stats.KnowledgeBytes
	if pack.Stats.IndexedCommit != "" {
		out.Metrics.IndexedCommit = pack.Stats.IndexedCommit
		out.Metrics.Generation = pack.Stats.Generation
	}

	// 3. Dedupe. In assisted mode this only reports what preferred mode would
	//    save; in preferred mode it drops what it can prove is redundant.
	out.Dedupe = NewDeduper(p.cfg.Mode, budget).Apply(req.Legacy, pack)
	out.Legacy = out.Dedupe.Kept
	out.Metrics.DedupeSavedBytes = out.Dedupe.SavedBytes

	// 4. The honest totals. ContextBytes is what AO will actually send:
	//    surviving legacy plus the pack plus the task text.
	survivingLegacy := legacyBytes(out.Legacy)
	out.Metrics.ContextBytes = survivingLegacy + out.Metrics.PackBytes + out.Metrics.TaskBytes
	out.Metrics.EstimatedInputTokens = EstimateTokens(out.Metrics.ContextBytes)
	if pack.Empty() {
		out.Metrics.FallbackBytes = survivingLegacy
	}

	// 5. Why memory contributed less than it might have. A reason from the
	//    pack itself wins, because it is the more specific of the two.
	switch {
	case pack.Stats.FallbackReason != "":
		out.Metrics.FallbackReason = pack.Stats.FallbackReason
	case out.Freshness.Kind == SyncSkipped && out.Freshness.Reason != "":
		out.Metrics.FallbackReason = out.Freshness.Reason
	}

	// 6. The manifest. What this execution was told, by identity, so a
	//    Reviewer can be shown what the Worker knew and a restart can be
	//    checked against what the previous attempt knew.
	p.freeze(ctx, req, role, pack)
	return out
}

// freeze records what one execution received (P2-C §16).
//
// It writes only for a dispatch that names an execution — a task ref or a run
// — because a manifest with neither identifies nothing and would be a row
// nobody can ever look up. It is best-effort in the strongest sense: a
// manifest that cannot be written is a lost observation, and Provision's
// contract is that nothing about memory may affect the dispatch.
func (p *Provisioner) freeze(ctx context.Context, req ProvisionRequest, role PackRole, pack ContextPack) {
	if p.svc == nil || pack.Empty() {
		return
	}
	if strings.TrimSpace(req.TaskRef) == "" && strings.TrimSpace(req.WorkflowRunID) == "" {
		return
	}
	ids := make([]string, 0, pack.Stats.SelectedItems)
	// P2-D section 18: the VERSION of each fact as well as its identity.
	//
	// The ids alone answer "which facts was this execution told" and cannot
	// answer "which version of them", and the reviewer incident P2-D section
	// 17 describes is a version question. The content hash is the right
	// version marker here rather than the generation: task knowledge is
	// written at generation 0 and never re-indexed, so a generation would be
	// the same number for two genuinely different revisions of a decision.
	versions := make([]string, 0, pack.Stats.SelectedItems)
	for _, section := range pack.Sections {
		for _, sel := range section.Items {
			ids = append(ids, sel.Item.ID)
			versions = append(versions, sel.Item.ContentHash)
		}
	}
	if len(ids) > domain.MaxManifestItems {
		ids = ids[:domain.MaxManifestItems]
		versions = versions[:domain.MaxManifestItems]
	}
	p.svc.RecordContextManifest(ctx, domain.MemoryContextManifest{
		ProjectID:     req.ProjectID,
		RepoID:        pack.RepoID,
		WorkflowRunID: req.WorkflowRunID,
		TaskRef:       req.TaskRef,
		Role:          string(role),
		PackDigest:    pack.Digest,
		PolicyVersion: PackPolicyVersion,
		Generation:    pack.Stats.Generation,
		IndexedCommit: pack.Stats.IndexedCommit,
		ItemIDs:       ids,
		ItemVersions:  versions,
		// The head this role was reasoning about, when the caller knew it. For
		// a reviewer it is the reviewed SHA, which is what makes "the pack was
		// built for SHA A and the reviewer judged SHA B" a diagnosable fact
		// rather than an unexplained disagreement.
		RoleHeadSHA:     req.HeadSHA,
		SelectedBytes:   pack.Stats.SelectedBytes,
		EstimatedTokens: pack.Stats.SelectedTokens,
	})
}

// changeMark reads the epoch the cache key is fenced by.
//
// A read failure yields zero, which is the SAFE direction only because zero is
// a value the epoch also legitimately takes for an empty repository: two
// dispatches that both fail to read it share a cache entry they would have
// shared anyway, and any successful read afterwards produces a different key.
// What it must never do is fail the provision -- memory is an optimisation, and
// an unreadable epoch is a reason to cache less confidently, not to stop.
func (p *Provisioner) changeMark(ctx context.Context, projectID domain.ProjectID, repoID string) int64 {
	if p.svc == nil || repoID == "" {
		return 0
	}
	mark, err := p.svc.ChangeMark(ctx, projectID, repoID)
	if err != nil {
		return 0
	}
	return mark.UnixNano()
}

// WithMetrics attaches this dispatch's memory record to a context, so the
// baseline recorder can pick it up without the two wrappers knowing about each
// other.
func (p Provisioned) WithMetrics(ctx context.Context) context.Context {
	return baseline.WithMemory(ctx, p.Metrics)
}

func legacyBytes(docs []LegacyDocument) int {
	total := 0
	for _, d := range docs {
		total += d.Bytes()
	}
	return total
}

// EstimateTokens converts bytes to an estimated token count.
//
// It is an ESTIMATE and is named as one everywhere it surfaces. AO does not
// have the provider's tokenizer, and a number presented as exact would be
// wrong in a way nobody could audit — so the four-bytes-per-token convention
// the context router already uses is applied here too, which at least makes
// the two surfaces comparable.
//
// A provider-specific tokenizer can be added later as an adapter; nothing in
// the core takes a dependency on one, which is what keeps pack assembly
// provider-neutral.
func EstimateTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + packBytesPerToken - 1) / packBytesPerToken
}
