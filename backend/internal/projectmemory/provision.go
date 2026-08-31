package projectmemory

import (
	"context"
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
		Role: role, PolicyVersion: PackPolicyVersion, Budget: budget,
		Scope: ScopeDigest(req.ChangedPaths, req.Modules,
			append(append([]string(nil), req.Keywords...), coverablePaths...), req.TaskRef),
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
	return out
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
