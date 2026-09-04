package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// project_memory.go — P2-A's operational read/repair surface.
//
// It is deliberately small and deliberately unglamorous. What an operator has
// to be able to answer is: which generation is this memory at, which commit was
// it derived from, how many facts does it hold and how many of them can AO
// still vouch for, when did it last index, and is a pass running right now.
// Everything here serves one of those questions, plus the two repairs that
// follow from a bad answer — rebuild it, or invalidate part of it.
//
// There is no visual UX yet, on purpose: P2-A's bar is inspectability, and a
// dashboard built before the numbers are trusted is a dashboard that hides
// them.

// ProjectMemoryService is the controller-facing contract for project memory.
type ProjectMemoryService interface {
	// Status reports every repository of a project.
	Status(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryStatus, error)
	// Inspect reads stored facts, including the ones AO can no longer vouch
	// for — seeing those is the point of an inspect.
	Inspect(ctx context.Context, req ProjectMemoryInspectQuery) (ProjectMemoryInspection, error)
	// Rebuild re-derives one repository's memory.
	Rebuild(ctx context.Context, projectID domain.ProjectID, repoPath string, purge bool) (ProjectMemoryRebuildOutcome, error)
	// Invalidate marks everything derived from the named paths as no longer
	// authoritative. With no paths it runs drift detection and applies what it
	// finds, which is the "I do not know what moved" repair.
	Invalidate(ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string) (ProjectMemoryInvalidateOutcome, error)
	// Report answers the P2-B question an operator actually has: is this
	// project's memory warm, what is it costing per role, and what did the
	// last freshness check have to do.
	Report(ctx context.Context, projectID domain.ProjectID, repoPath string) (ProjectMemoryReport, error)
	// Knowledge reads shared task knowledge — what tasks learned, which
	// decisions still govern, which risks are still open (P2-C §17).
	Knowledge(ctx context.Context, req ProjectMemoryKnowledgeQuery) (ProjectMemoryKnowledgeResult, error)
	// Manifests reads what one execution was actually told (P2-C §16).
	Manifests(ctx context.Context, req ProjectMemoryManifestQuery) (ProjectMemoryManifestResult, error)
	// Validate runs the P2-D authority pass: which facts can AO still prove it
	// is entitled to serve. It is a second verb beside Invalidate rather than a
	// mode of it, because the two find different problems with different
	// repairs — a drifted fact needs re-deriving, an unprovable one needs its
	// promotion found or never promoting at all.
	Validate(ctx context.Context, req ProjectMemoryValidateQuery) (ProjectMemoryValidation, error)
	// Provenance answers, for one fact: why is this valid, what task produced
	// it, which commit supports it, how did it become canonical, and what
	// withdrew it (P2-D §27).
	Provenance(ctx context.Context, projectID domain.ProjectID, itemID string) (ProjectMemoryProvenance, bool, error)
	// Prune retires canonical memories that were minted from a worktree rather
	// than from a repository (P2-E). Dry run unless apply is set.
	Prune(ctx context.Context, projectID domain.ProjectID, apply bool) (ProjectMemoryPruneResult, error)
}

// ProjectMemoryPruneResult is what one prune found and did.
type ProjectMemoryPruneResult struct {
	Applied          bool
	CanonicalRepoIDs []string
	Candidates       []ProjectMemoryPruneCandidate
	PurgedItems      int
	PurgedRelations  int
}

// ProjectMemoryPruneCandidate is one repository memory the prune considered.
type ProjectMemoryPruneCandidate struct {
	RepoID     string
	RepoPath   string
	Items      int
	Relations  int
	ParentRepo string
	Prunable   bool
	Reason     string
	Purged     bool
}

// ProjectMemoryValidateQuery asks for one authority pass.
type ProjectMemoryValidateQuery struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// Apply writes the demotions. It defaults to false so the diagnostic is
	// safe to run: an operator investigating an integrity question should be
	// able to look before anything changes.
	Apply bool
	Limit int
}

// ProjectMemoryValidation is what an authority pass found.
type ProjectMemoryValidation struct {
	RepoID           string
	RepoIdentity     string
	Applied          bool
	Checked          int
	Provable         int
	IdentityWithheld int64
	LegacyClassified int64
	EdgesRetired     int64
	Truncated        bool
	Findings         []ProjectMemoryValidationFinding
}

// ProjectMemoryValidationFinding is one fact whose licence no longer holds.
type ProjectMemoryValidationFinding struct {
	ItemID      string
	Type        string
	Scope       string
	Key         string
	Summary     string
	From        string
	To          string
	ReasonClass string
	Detail      string
	Applied     bool
}

// ProjectMemoryProvenance is the full evidence chain for one fact.
type ProjectMemoryProvenance struct {
	Item                 domain.ProjectMemoryItem
	Servable             bool
	AuthorityReasonClass string
	Relations            []domain.ProjectMemoryRelation
}

// ProjectMemoryKnowledgeQuery narrows a shared-knowledge read.
type ProjectMemoryKnowledgeQuery struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// Type narrows to decisions, risks or task results. Empty means all three.
	Type domain.ProjectMemoryType
	// Status narrows by lifecycle. Empty means active only, which is what
	// retrieval would serve.
	Status domain.KnowledgeStatus
	// TaskRef narrows to what one task produced. When set, EVERY status is
	// returned: "what did we learn from this task" includes the decision a
	// later task has since replaced.
	TaskRef string
	Limit   int
}

// ProjectMemoryKnowledgeEntry is one shared fact with its lifecycle rendered.
type ProjectMemoryKnowledgeEntry struct {
	Item          domain.ProjectMemoryItem
	Status        string
	Kind          string
	Share         string
	Subject       string
	SourceTask    string
	SupersededBy  string
	Supersedes    string
	ResolvedBy    string
	ConflictsWith string
}

// ProjectMemoryKnowledgeResult is what a knowledge read found.
type ProjectMemoryKnowledgeResult struct {
	RepoID  string
	Entries []ProjectMemoryKnowledgeEntry
}

// ProjectMemoryManifestQuery asks what one execution was told.
type ProjectMemoryManifestQuery struct {
	ProjectID     domain.ProjectID
	TaskRef       string
	WorkflowRunID string
	// Expand resolves each manifest's item ids back into the facts they name,
	// and reports the ones that no longer exist.
	Expand bool
}

// ProjectMemoryManifestEntry is one execution's frozen context.
type ProjectMemoryManifestEntry struct {
	Manifest domain.MemoryContextManifest
	Items    []ProjectMemoryKnowledgeEntry
	// Missing names manifest items that no longer exist. A quietly shorter
	// list would hide the single most interesting thing a manifest can
	// reveal: that an execution was told something AO has since discarded.
	Missing []string
}

// ProjectMemoryManifestResult is what a manifest read found.
type ProjectMemoryManifestResult struct {
	Entries []ProjectMemoryManifestEntry
}

// ProjectMemoryReport is the P2-B operational view: the policy in force, how
// warm the project is, and what a pack currently costs each role.
//
// The per-role figures are computed by assembling each role's pack exactly as
// a dispatch would, so what an operator reads is what an agent would receive
// rather than an estimate of it.
type ProjectMemoryReport struct {
	Mode          string
	CacheEnabled  bool
	SyncTimeout   string
	RepoID        string
	RepoPath      string
	Warm          bool
	Generation    int64
	IndexedCommit string
	SyncKind      string
	SyncReason    string
	SyncFilesRead int
	SyncMillis    int64
	Roles         []ProjectMemoryRoleReport
	CacheHits     int64
	CacheMisses   int64
}

// ProjectMemoryRoleReport is one role's current context cost.
type ProjectMemoryRoleReport struct {
	Role                string
	BudgetBytes         int
	BudgetItems         int
	BudgetDocuments     int
	PackItems           int
	PackBytes           int
	EstimatedPackTokens int
	Candidates          int
	RejectedByBudget    int
	ReducedToSummary    int
	StaleExcluded       int
	FallbackReason      string
}

// ProjectMemoryInspectQuery narrows an inspect read.
type ProjectMemoryInspectQuery struct {
	ProjectID  domain.ProjectID
	RepoPath   string
	State      domain.ProjectMemoryState
	Type       domain.ProjectMemoryType
	PathPrefix string
	Limit      int
}

// ProjectMemoryInspection is what an inspect read found.
type ProjectMemoryInspection struct {
	RepoID    string
	Items     []domain.ProjectMemoryItem
	Total     int
	Truncated bool
}

// ProjectMemoryRebuildOutcome is what a rebuild did.
type ProjectMemoryRebuildOutcome struct {
	RepoID           string
	Generation       int64
	Skipped          bool
	SkipReason       string
	FilesIndexed     int
	FilesSkipped     int
	ItemsWritten     int
	ItemsReconfirmed int
	ItemsRetired     int64
	IndexedCommit    string
	Truncated        bool
	TruncatedReason  string
}

// ProjectMemoryInvalidateOutcome is what an invalidate did.
type ProjectMemoryInvalidateOutcome struct {
	RepoID           string
	ItemsInvalidated int64
	DriftChecked     int
	DriftFound       int
}

// ProjectMemoryController owns the /projects/{id}/memory routes.
type ProjectMemoryController struct {
	Svc ProjectMemoryService
	// Guard is P4-B's authorization gate. Every route here is addressed by a
	// project id, so reading memory needs memory.read on that project and
	// rebuilding/pruning it needs project.manage -- a repair that discards
	// derived knowledge is a change to the project, not a read of it.
	Guard Guard
}

// Register mounts the project-memory routes.
func (c *ProjectMemoryController) Register(r chi.Router) {
	r.Get("/projects/{id}/memory", c.scoped(domain.PermMemoryRead, c.status))
	r.Get("/projects/{id}/memory/items", c.scoped(domain.PermMemoryRead, c.inspect))
	r.Post("/projects/{id}/memory/rebuild", c.scoped(domain.PermProjectManage, c.rebuild))
	r.Post("/projects/{id}/memory/invalidate", c.scoped(domain.PermProjectManage, c.invalidate))
	r.Get("/projects/{id}/memory/report", c.scoped(domain.PermMemoryRead, c.report))
	r.Get("/projects/{id}/memory/knowledge", c.scoped(domain.PermMemoryRead, c.knowledge))
	r.Get("/projects/{id}/memory/manifests", c.scoped(domain.PermMemoryRead, c.manifests))
	r.Post("/projects/{id}/memory/validate", c.scoped(domain.PermMemoryRead, c.validate))
	r.Get("/projects/{id}/memory/provenance/{itemId}", c.scoped(domain.PermMemoryRead, c.provenance))
	r.Post("/projects/{id}/memory/prune", c.scoped(domain.PermProjectManage, c.prune))
}

// scoped wraps a handler addressed by {id} with the project permission it
// needs.
func (c *ProjectMemoryController) scoped(perm domain.Permission, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Guard.AllowProject(w, r, perm, projectID(r), "PROJECT_NOT_FOUND", "project not found") {
			return
		}
		h(w, r)
	}
}

// prune retires the canonical memories a worktree should never have had.
//
// POST and dry-run-by-default, matching validate: a repair operation an
// operator should be able to look at before it changes anything.
func (c *ProjectMemoryController) prune(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/memory/prune")
		return
	}
	var req PruneProjectMemoryRequest
	if err := decodeJSON(r, &req); err != nil {
		envelope.WriteError(w, r, apierr.Invalid("invalid_body", "request body is not valid JSON", nil))
		return
	}
	res, err := c.Svc.Prune(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), req.Apply)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	cands := make([]ProjectMemoryPruneCandidateResponse, 0, len(res.Candidates))
	for _, cand := range res.Candidates {
		cands = append(cands, ProjectMemoryPruneCandidateResponse(cand))
	}
	envelope.WriteJSON(w, http.StatusOK, PruneProjectMemoryResponse{
		Applied:          res.Applied,
		CanonicalRepoIDs: res.CanonicalRepoIDs,
		PurgedItems:      res.PurgedItems,
		PurgedRelations:  res.PurgedRelations,
		Candidates:       cands,
	})
}

// validate runs the P2-D authority pass.
//
// It is a POST even in its default dry-run form, because the pass is real work
// over a repository rather than a read of stored state, and because the
// apply/dry-run choice belongs in a body rather than in a query string that a
// browser could follow by accident.
func (c *ProjectMemoryController) validate(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/memory/validate")
		return
	}
	var req ValidateProjectMemoryRequest
	if err := decodeJSON(r, &req); err != nil {
		envelope.WriteError(w, r, apierr.Invalid("invalid_body", "request body is not valid JSON", nil))
		return
	}
	limit := 0
	if req.Limit != nil {
		if *req.Limit < 0 {
			envelope.WriteError(w, r, apierr.Invalid("invalid_limit",
				"limit must be a non-negative integer", nil))
			return
		}
		limit = int(*req.Limit)
	}
	res, err := c.Svc.Validate(r.Context(), ProjectMemoryValidateQuery{
		ProjectID: domain.ProjectID(chi.URLParam(r, "id")),
		RepoPath:  strings.TrimSpace(req.RepoPath),
		Apply:     req.Apply,
		Limit:     limit,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	findings := make([]ProjectMemoryValidationFindingResponse, 0, len(res.Findings))
	for _, f := range res.Findings {
		findings = append(findings, ProjectMemoryValidationFindingResponse(f))
	}
	envelope.WriteJSON(w, http.StatusOK, ValidateProjectMemoryResponse{
		RepoID:           res.RepoID,
		RepoIdentity:     res.RepoIdentity,
		Applied:          res.Applied,
		Checked:          res.Checked,
		Provable:         res.Provable,
		IdentityWithheld: res.IdentityWithheld,
		LegacyClassified: res.LegacyClassified,
		EdgesRetired:     res.EdgesRetired,
		Truncated:        res.Truncated,
		Findings:         findings,
	})
}

// provenance answers "why is this fact valid, and what produced it".
func (c *ProjectMemoryController) provenance(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/provenance/{itemId}")
		return
	}
	res, found, err := c.Svc.Provenance(r.Context(),
		domain.ProjectID(chi.URLParam(r, "id")), chi.URLParam(r, "itemId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if !found {
		envelope.WriteError(w, r, apierr.NotFound("memory_item_not_found",
			"no project memory item with that id"))
		return
	}
	rels := make([]ProjectMemoryRelationResponse, 0, len(res.Relations))
	for _, rel := range res.Relations {
		rels = append(rels, projectMemoryRelationResponse(rel))
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectMemoryProvenanceResponse{
		Item:                 projectMemoryItemResponse(res.Item),
		Servable:             res.Servable,
		AuthorityReasonClass: res.AuthorityReasonClass,
		Relations:            rels,
	})
}

// knowledge answers "what have tasks taught this project, and what of it still
// holds".
func (c *ProjectMemoryController) knowledge(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/knowledge")
		return
	}
	q := r.URL.Query()
	limit, ok := parseMemoryLimit(w, r, q.Get("limit"))
	if !ok {
		return
	}
	res, err := c.Svc.Knowledge(r.Context(), ProjectMemoryKnowledgeQuery{
		ProjectID: domain.ProjectID(chi.URLParam(r, "id")),
		RepoPath:  strings.TrimSpace(q.Get("repoPath")),
		Type:      domain.ProjectMemoryType(strings.TrimSpace(q.Get("type"))),
		Status:    domain.KnowledgeStatus(strings.TrimSpace(q.Get("status"))),
		TaskRef:   strings.TrimSpace(q.Get("task")),
		Limit:     limit,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	entries := make([]ProjectMemoryKnowledgeResponse, 0, len(res.Entries))
	for _, e := range res.Entries {
		entries = append(entries, projectMemoryKnowledgeResponse(e))
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectMemoryKnowledgeResponse{
		RepoID: res.RepoID, Entries: entries, Total: len(entries),
	})
}

// manifests answers "what did this execution actually know".
func (c *ProjectMemoryController) manifests(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/manifests")
		return
	}
	q := r.URL.Query()
	task := strings.TrimSpace(q.Get("task"))
	run := strings.TrimSpace(q.Get("run"))
	if task == "" && run == "" {
		envelope.WriteError(w, r, apierr.Invalid("missing_execution",
			"a manifest read must name a task or a workflow run", nil))
		return
	}
	res, err := c.Svc.Manifests(r.Context(), ProjectMemoryManifestQuery{
		ProjectID:     domain.ProjectID(chi.URLParam(r, "id")),
		TaskRef:       task,
		WorkflowRunID: run,
		Expand:        strings.TrimSpace(q.Get("expand")) == "true",
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	entries := make([]ProjectMemoryManifestResponse, 0, len(res.Entries))
	for _, e := range res.Entries {
		items := make([]ProjectMemoryKnowledgeResponse, 0, len(e.Items))
		for _, it := range e.Items {
			items = append(items, projectMemoryKnowledgeResponse(it))
		}
		entries = append(entries, ProjectMemoryManifestResponse{
			ID: e.Manifest.ID, TaskRef: e.Manifest.TaskRef,
			WorkflowRunID: e.Manifest.WorkflowRunID, Role: e.Manifest.Role,
			PackDigest: e.Manifest.PackDigest, PolicyVersion: e.Manifest.PolicyVersion,
			Generation: e.Manifest.Generation, IndexedCommit: e.Manifest.IndexedCommit,
			ItemIDs: e.Manifest.ItemIDs, ItemCount: len(e.Manifest.ItemIDs),
			SelectedBytes: e.Manifest.SelectedBytes, EstimatedTokens: e.Manifest.EstimatedTokens,
			CreatedAt: e.Manifest.CreatedAt, Items: items, Missing: e.Missing,
		})
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectMemoryManifestsResponse{
		Entries: entries, Total: len(entries),
	})
}

// parseMemoryLimit reads a non-negative limit, writing the error envelope and
// reporting false when it cannot.
func parseMemoryLimit(w http.ResponseWriter, r *http.Request, raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		envelope.WriteError(w, r, apierr.Invalid("invalid_limit",
			"limit must be a non-negative integer", nil))
		return 0, false
	}
	return n, true
}

func (c *ProjectMemoryController) report(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/report")
		return
	}
	out, err := c.Svc.Report(r.Context(), domain.ProjectID(chi.URLParam(r, "id")),
		strings.TrimSpace(r.URL.Query().Get("repoPath")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	roles := make([]ProjectMemoryRoleReportResponse, 0, len(out.Roles))
	for _, role := range out.Roles {
		roles = append(roles, ProjectMemoryRoleReportResponse(role))
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectMemoryReportResponse{
		Mode: out.Mode, CacheEnabled: out.CacheEnabled, SyncTimeout: out.SyncTimeout,
		RepoID: out.RepoID, RepoPath: out.RepoPath, Warm: out.Warm,
		Generation: out.Generation, IndexedCommit: out.IndexedCommit,
		SyncKind: out.SyncKind, SyncReason: out.SyncReason,
		SyncFilesRead: out.SyncFilesRead, SyncMillis: out.SyncMillis,
		Roles: roles, CacheHits: out.CacheHits, CacheMisses: out.CacheMisses,
	})
}

func (c *ProjectMemoryController) status(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory")
		return
	}
	statuses, err := c.Svc.Status(r.Context(), domain.ProjectID(chi.URLParam(r, "id")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := make([]ProjectMemoryStatusResponse, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, projectMemoryStatusResponse(s))
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectMemoryResponse{Repositories: out})
}

func (c *ProjectMemoryController) inspect(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/items")
		return
	}
	q := r.URL.Query()
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			envelope.WriteError(w, r, apierr.Invalid("invalid_limit",
				"limit must be a non-negative integer", nil))
			return
		}
		limit = n
	}
	res, err := c.Svc.Inspect(r.Context(), ProjectMemoryInspectQuery{
		ProjectID:  domain.ProjectID(chi.URLParam(r, "id")),
		RepoPath:   strings.TrimSpace(q.Get("repoPath")),
		State:      domain.ProjectMemoryState(strings.TrimSpace(q.Get("state"))),
		Type:       domain.ProjectMemoryType(strings.TrimSpace(q.Get("type"))),
		PathPrefix: strings.TrimSpace(q.Get("path")),
		Limit:      limit,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	items := make([]ProjectMemoryItemResponse, 0, len(res.Items))
	for _, item := range res.Items {
		items = append(items, projectMemoryItemResponse(item))
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectMemoryItemsResponse{
		RepoID: res.RepoID, Items: items, Total: res.Total, Truncated: res.Truncated,
	})
}

func (c *ProjectMemoryController) rebuild(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/memory/rebuild")
		return
	}
	var req RebuildProjectMemoryRequest
	if err := decodeJSON(r, &req); err != nil {
		envelope.WriteError(w, r, apierr.Invalid("invalid_body", "request body is not valid JSON", nil))
		return
	}
	out, err := c.Svc.Rebuild(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), strings.TrimSpace(req.RepoPath), req.Purge)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	// The wire shape and the service outcome carry the same fields in the same
	// order, so this is a conversion rather than a copy. Keeping them aligned
	// is deliberate: a field added to one and forgotten in the other becomes a
	// compile error here instead of a silently missing JSON key.
	envelope.WriteJSON(w, http.StatusOK, RebuildProjectMemoryResponse(out))
}

func (c *ProjectMemoryController) invalidate(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/memory/invalidate")
		return
	}
	var req InvalidateProjectMemoryRequest
	if err := decodeJSON(r, &req); err != nil {
		envelope.WriteError(w, r, apierr.Invalid("invalid_body", "request body is not valid JSON", nil))
		return
	}
	out, err := c.Svc.Invalidate(r.Context(), domain.ProjectID(chi.URLParam(r, "id")),
		strings.TrimSpace(req.RepoPath), req.Paths, strings.TrimSpace(req.Reason))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, InvalidateProjectMemoryResponse(out))
}

// ProjectMemoryStatusResponse is one repository's memory on the wire.
//
// Every count is reported separately rather than rolled into a health score:
// "412 facts, 9 stale, 2 invalidated, indexed at abc123 four minutes ago" is
// diagnosable, and a green tick is not.
type ProjectMemoryStatusResponse struct {
	RepoID   string `json:"repoId"`
	RepoPath string `json:"repoPath"`
	// Phase is where the current or last pass got to. "idle" means none is
	// running; "failed" means the memory is not vouched for.
	Phase         string `json:"phase" enum:"idle,scanning,summarizing,linking,finalizing,failed"`
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit"`
	Branch        string `json:"branch,omitempty"`
	// ResumeCursor is the path a crashed pass had reached, and is what a
	// restart resumes from.
	ResumeCursor string `json:"resumeCursor,omitempty"`
	LastError    string `json:"lastError,omitempty"`
	// Healthy reports whether this memory may be relied on as a whole: a pass
	// has completed, none is failed, and at least one fact is valid.
	Healthy bool `json:"healthy"`

	Items       int `json:"items"`
	Valid       int `json:"valid"`
	Stale       int `json:"stale"`
	Invalidated int `json:"invalidated"`
	Rebuilding  int `json:"rebuilding"`
	// TaskLocal counts facts belonging to individual tasks' unintegrated work.
	// They are reported apart from the rest because they are deliberately not
	// part of the project's canonical memory.
	TaskLocal int `json:"taskLocal"`
	Relations int `json:"relations"`

	FilesIndexed int `json:"filesIndexed"`
	FilesSkipped int `json:"filesSkipped"`

	LastIndexedAt *string `json:"lastIndexedAt"`
	LastUpdatedAt *string `json:"lastUpdatedAt"`

	// ByType is the per-type census, so an operator can see that conventions
	// were captured and modules were not.
	ByType map[string]int `json:"byType,omitempty"`
}

// ListProjectMemoryResponse is the body of GET /api/v1/projects/{id}/memory.
type ListProjectMemoryResponse struct {
	Repositories []ProjectMemoryStatusResponse `json:"repositories"`
}

// ProjectMemoryItemResponse is one stored fact on the wire. The body is
// deliberately omitted: an inspect answers "what does AO remember and can it
// still vouch for it", and shipping every body would make the answer unreadable.
type ProjectMemoryItemResponse struct {
	ID            string   `json:"id"`
	RepoID        string   `json:"repoId"`
	Type          string   `json:"type"`
	Scope         string   `json:"scope" enum:"project,repository,module,file,symbol,task"`
	Key           string   `json:"key,omitempty"`
	Origin        string   `json:"origin" enum:"canonical,task_local"`
	OriginRef     string   `json:"originRef,omitempty"`
	Summary       string   `json:"summary"`
	State         string   `json:"state" enum:"valid,stale,invalidated,rebuilding"`
	StateReason   string   `json:"stateReason,omitempty"`
	Confidence    float64  `json:"confidence"`
	Generation    int64    `json:"generation"`
	SourceCommit  string   `json:"sourceCommit,omitempty"`
	SourcePaths   []string `json:"sourcePaths,omitempty"`
	ContentBytes  int      `json:"contentBytes"`
	UpdatedAt     string   `json:"updatedAt"`
	InvalidatedAt *string  `json:"invalidatedAt"`

	// P2-D: the authority axis, reported beside the drift state rather than
	// folded into it. Servable is the conjunction the read side actually
	// applies, given explicitly so a surface never has to re-derive it.
	Authority          string `json:"authority" enum:"authoritative,unprovable,legacy_unprovable"`
	AuthorityReason    string `json:"authorityReason,omitempty"`
	Servable           bool   `json:"servable"`
	ProvenanceKind     string `json:"provenanceKind,omitempty" enum:"repo_derivation,task_outcome,workflow_knowledge,legacy"`
	RepoIdentity       string `json:"repoIdentity,omitempty"`
	PromotionAuthority string `json:"promotionAuthority,omitempty"`
	VerifiedCommit     string `json:"verifiedCommit,omitempty"`
	IntegratedCommit   string `json:"integratedCommit,omitempty"`
}

// ProjectMemoryRelationResponse is one graph edge on the wire.
type ProjectMemoryRelationResponse struct {
	ID              string `json:"id"`
	FromKind        string `json:"fromKind"`
	FromKey         string `json:"fromKey"`
	Kind            string `json:"kind"`
	ToKind          string `json:"toKind"`
	ToKey           string `json:"toKey"`
	State           string `json:"state" enum:"valid,stale,invalidated,rebuilding"`
	StateReason     string `json:"stateReason,omitempty"`
	Authority       string `json:"authority" enum:"authoritative,unprovable,legacy_unprovable"`
	AuthorityReason string `json:"authorityReason,omitempty"`
	SourceCommit    string `json:"sourceCommit,omitempty"`
}

// ProjectMemoryItemIDParam is the {itemId} path parameter of the provenance
// route. Item ids are derived hashes rather than sequential keys, so a caller
// gets them from an inspect or a validate rather than by guessing.
type ProjectMemoryItemIDParam struct {
	ID     string `path:"id" description:"Project identifier (registry key)."`
	ItemID string `path:"itemId" description:"Derived project-memory item id, as returned by the items or validate routes."`
}

// ValidateProjectMemoryRequest is the body of
// POST /api/v1/projects/{id}/memory/validate.
type ValidateProjectMemoryRequest struct {
	RepoPath string `json:"repoPath,omitempty" description:"Repository root to validate. Defaults to the project's own root."`
	Apply    bool   `json:"apply,omitempty" description:"Write the demotions. Defaults to false, so the diagnostic can be run before anything changes."`
	Limit    *int64 `json:"limit,omitempty" minimum:"1" maximum:"20000" description:"Maximum facts to check. Defaults to 2000."`
}

// ValidateProjectMemoryResponse is the body of the same route.
type ValidateProjectMemoryResponse struct {
	RepoID string `json:"repoId"`
	// RepoIdentity is what the checkout identifies as right now. Empty means
	// AO could not identify it, which is itself the answer to several
	// questions and is why it is reported even on a clean pass.
	RepoIdentity     string                                   `json:"repoIdentity,omitempty"`
	Applied          bool                                     `json:"applied"`
	Checked          int                                      `json:"checked"`
	Provable         int                                      `json:"provable"`
	IdentityWithheld int64                                    `json:"identityWithheld"`
	LegacyClassified int64                                    `json:"legacyClassified"`
	EdgesRetired     int64                                    `json:"edgesRetired"`
	Truncated        bool                                     `json:"truncated"`
	Findings         []ProjectMemoryValidationFindingResponse `json:"findings"`
}

// ProjectMemoryValidationFindingResponse is one withheld fact on the wire.
type ProjectMemoryValidationFindingResponse struct {
	ItemID      string `json:"itemId"`
	Type        string `json:"type"`
	Scope       string `json:"scope"`
	Key         string `json:"key,omitempty"`
	Summary     string `json:"summary"`
	From        string `json:"from"`
	To          string `json:"to"`
	ReasonClass string `json:"reasonClass"`
	Detail      string `json:"detail"`
	Applied     bool   `json:"applied"`
}

// PruneProjectMemoryRequest is the body of
// POST /api/v1/projects/{id}/memory/prune.
type PruneProjectMemoryRequest struct {
	Apply bool `json:"apply,omitempty" description:"Purge the worktree-minted memories. Defaults to false, so the repair can be inspected before it changes anything."`
}

// PruneProjectMemoryResponse is the body of the same route.
type PruneProjectMemoryResponse struct {
	Applied bool `json:"applied"`
	// CanonicalRepoIDs are the repository memories the prune preserved, named
	// so an operator sees what survives and not only what goes.
	CanonicalRepoIDs []string                              `json:"canonicalRepoIds"`
	PurgedItems      int                                   `json:"purgedItems"`
	PurgedRelations  int                                   `json:"purgedRelations"`
	Candidates       []ProjectMemoryPruneCandidateResponse `json:"candidates"`
}

// ProjectMemoryPruneCandidateResponse is one repository memory considered.
type ProjectMemoryPruneCandidateResponse struct {
	RepoID     string `json:"repoId"`
	RepoPath   string `json:"repoPath"`
	Items      int    `json:"items"`
	Relations  int    `json:"relations"`
	ParentRepo string `json:"parentRepo,omitempty"`
	Prunable   bool   `json:"prunable"`
	Reason     string `json:"reason"`
	Purged     bool   `json:"purged"`
}

// ProjectMemoryProvenanceResponse is the body of
// GET /api/v1/projects/{id}/memory/provenance/{itemId}.
type ProjectMemoryProvenanceResponse struct {
	Item                 ProjectMemoryItemResponse       `json:"item"`
	Servable             bool                            `json:"servable"`
	AuthorityReasonClass string                          `json:"authorityReasonClass,omitempty"`
	Relations            []ProjectMemoryRelationResponse `json:"relations"`
}

// ListProjectMemoryItemsQuery is the query-string contract of
// GET /api/v1/projects/{id}/memory/items.
type ListProjectMemoryItemsQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root to inspect. Defaults to the project's own root, which is the single-repo case."`
	State    string `query:"state,omitempty" enum:"valid,stale,invalidated,rebuilding" description:"Narrow to one state. Omit to see every state, including the facts AO can no longer vouch for."`
	Type     string `query:"type,omitempty" description:"Narrow to one item type (module, convention, architecture, ...)."`
	Path     string `query:"path,omitempty" description:"Narrow to facts about a subtree, by repo-relative path prefix."`
	Limit    *int64 `query:"limit,omitempty" minimum:"1" maximum:"1000" description:"Maximum items to return. Defaults to 200."`
}

// ListProjectMemoryKnowledgeQuery is the query-string contract of
// GET /api/v1/projects/{id}/memory/knowledge.
type ListProjectMemoryKnowledgeQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root to read. Defaults to every repository of the project."`
	Type     string `query:"type,omitempty" enum:"task_result,decision,known_risk" description:"Narrow to one kind of shared knowledge. Omit for all three."`
	Status   string `query:"status,omitempty" enum:"active,superseded,resolved,obsolete,conflicting" description:"Narrow to one lifecycle status. Defaults to active, which is what retrieval would actually serve."`
	Task     string `query:"task,omitempty" description:"Narrow to what one task produced. When set, every status is returned: a decision this task made that a later one replaced is still something this task produced."`
	Limit    *int64 `query:"limit,omitempty" minimum:"1" maximum:"1000" description:"Maximum entries to return. Defaults to 200."`
}

// ListProjectMemoryManifestsQuery is the query-string contract of
// GET /api/v1/projects/{id}/memory/manifests.
type ListProjectMemoryManifestsQuery struct {
	Task   string `query:"task,omitempty" description:"The task whose frozen context to read. One of task or run is required."`
	Run    string `query:"run,omitempty" description:"The workflow run whose executions to read. One of task or run is required."`
	Expand string `query:"expand,omitempty" enum:"true,false" description:"Resolve each manifest's item ids back into the facts they name, and report the ones that no longer exist."`
}

// GetProjectMemoryReportQuery is the query-string contract of
// GET /api/v1/projects/{id}/memory/report.
type GetProjectMemoryReportQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root to report on. Defaults to the project's own root, which is the single-repo case."`
}

// ListProjectMemoryItemsResponse is the body of
// GET /api/v1/projects/{id}/memory/items.
type ListProjectMemoryItemsResponse struct {
	RepoID    string                      `json:"repoId"`
	Items     []ProjectMemoryItemResponse `json:"items"`
	Total     int                         `json:"total"`
	Truncated bool                        `json:"truncated"`
}

// RebuildProjectMemoryRequest asks for a rebuild.
type RebuildProjectMemoryRequest struct {
	// RepoPath names the repository. It is required: a rebuild is expensive,
	// and "rebuild everything" is not a thing an operator should be able to
	// ask for by omission.
	RepoPath string `json:"repoPath"`
	// Purge deletes the existing facts before re-deriving, rather than
	// re-deriving over them. It is the escape hatch for memory that is wrong
	// in a way a re-derivation cannot fix.
	Purge bool `json:"purge,omitempty"`
}

// RebuildProjectMemoryResponse reports what a rebuild did.
type RebuildProjectMemoryResponse struct {
	RepoID           string `json:"repoId"`
	Generation       int64  `json:"generation"`
	Skipped          bool   `json:"skipped"`
	SkipReason       string `json:"skipReason,omitempty"`
	FilesIndexed     int    `json:"filesIndexed"`
	FilesSkipped     int    `json:"filesSkipped"`
	ItemsWritten     int    `json:"itemsWritten"`
	ItemsReconfirmed int    `json:"itemsReconfirmed"`
	ItemsRetired     int64  `json:"itemsRetired"`
	IndexedCommit    string `json:"indexedCommit,omitempty"`
	Truncated        bool   `json:"truncated"`
	TruncatedReason  string `json:"truncatedReason,omitempty"`
}

// InvalidateProjectMemoryRequest asks for an invalidation.
type InvalidateProjectMemoryRequest struct {
	RepoPath string `json:"repoPath"`
	// Paths names what to invalidate. With no paths the daemon runs drift
	// detection and applies what it finds — the "something moved and I do not
	// know what" repair.
	Paths  []string `json:"paths,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

// InvalidateProjectMemoryResponse reports what an invalidation did.
type InvalidateProjectMemoryResponse struct {
	RepoID           string `json:"repoId"`
	ItemsInvalidated int64  `json:"itemsInvalidated"`
	DriftChecked     int    `json:"driftChecked"`
	DriftFound       int    `json:"driftFound"`
}

func projectMemoryStatusResponse(s domain.ProjectMemoryStatus) ProjectMemoryStatusResponse {
	out := ProjectMemoryStatusResponse{
		RepoID: s.RepoID, RepoPath: s.RepoPath,
		Phase: string(s.Index.Phase), Generation: s.Index.Generation,
		IndexedCommit: s.Index.IndexedCommit, Branch: s.Index.Branch,
		ResumeCursor: s.Index.Cursor, LastError: s.Index.LastError,
		Healthy: s.Healthy(),
		Items:   s.Counts.Total, Valid: s.Counts.Valid, Stale: s.Counts.Stale,
		Invalidated: s.Counts.Invalidated, Rebuilding: s.Counts.Rebuilding,
		TaskLocal: s.Counts.TaskLocal, Relations: s.Counts.Relations,
		FilesIndexed: s.Index.FilesIndexed, FilesSkipped: s.Index.FilesSkipped,
	}
	if !s.LastIndexedAt.IsZero() {
		v := s.LastIndexedAt.Format(rfc3339Milli)
		out.LastIndexedAt = &v
	}
	if !s.LastUpdatedAt.IsZero() {
		v := s.LastUpdatedAt.Format(rfc3339Milli)
		out.LastUpdatedAt = &v
	}
	if len(s.ByType) > 0 {
		out.ByType = make(map[string]int, len(s.ByType))
		for t, n := range s.ByType {
			out.ByType[string(t)] = n
		}
	}
	return out
}

func projectMemoryItemResponse(item domain.ProjectMemoryItem) ProjectMemoryItemResponse {
	out := ProjectMemoryItemResponse{
		ID: item.ID, RepoID: item.Key.RepoID,
		Type: string(item.Key.Type), Scope: string(item.Key.Scope), Key: item.Key.Key,
		Origin: string(item.Origin), OriginRef: item.OriginRef,
		Summary: item.Summary, State: string(item.State), StateReason: item.StateReason,
		Confidence: item.Confidence, Generation: item.Generation,
		SourceCommit: item.SourceCommit, SourcePaths: item.SourcePaths,
		ContentBytes: len(item.Content),
		UpdatedAt:    item.UpdatedAt.Format(rfc3339Milli),
	}
	out.Authority = string(item.Authority)
	out.AuthorityReason = item.AuthorityReason
	out.Servable = item.Servable()
	out.ProvenanceKind = string(item.ProvenanceKind)
	out.RepoIdentity = string(item.RepoIdentity)
	out.PromotionAuthority = item.PromotionAuthority
	out.VerifiedCommit = item.VerifiedCommit
	out.IntegratedCommit = item.IntegratedCommit
	if !item.InvalidatedAt.IsZero() {
		v := item.InvalidatedAt.Format(rfc3339Milli)
		out.InvalidatedAt = &v
	}
	return out
}

func projectMemoryRelationResponse(rel domain.ProjectMemoryRelation) ProjectMemoryRelationResponse {
	return ProjectMemoryRelationResponse{
		ID:              rel.ID,
		FromKind:        string(rel.From.Kind),
		FromKey:         rel.From.Key,
		Kind:            string(rel.Kind),
		ToKind:          string(rel.To.Kind),
		ToKey:           rel.To.Key,
		State:           string(rel.State),
		StateReason:     rel.StateReason,
		Authority:       string(rel.Authority),
		AuthorityReason: rel.AuthorityReason,
		SourceCommit:    rel.SourceCommit,
	}
}

// ProjectMemoryReportResponse is the body of
// GET /api/v1/projects/{id}/memory/report.
//
// It answers the P2-B operational question directly: is this project warm, and
// what is memory costing each role right now. "Warm" means the freshness check
// found memory already at the repository's current commit and therefore did no
// work — which is the whole point of the optimisation, so it is reported as a
// fact rather than inferred from timings.
type ProjectMemoryReportResponse struct {
	Mode         string `json:"mode" enum:"off,assisted,preferred"`
	CacheEnabled bool   `json:"cacheEnabled"`
	SyncTimeout  string `json:"syncTimeout"`

	RepoID   string `json:"repoId"`
	RepoPath string `json:"repoPath"`
	// Warm reports that the freshness check was a no-op: memory was already at
	// the repository's current commit.
	Warm          bool   `json:"warm"`
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit,omitempty"`
	// SyncKind is what the check had to do: none, incremental, full,
	// coalesced or skipped.
	SyncKind      string `json:"syncKind" enum:"none,incremental,full,coalesced,skipped"`
	SyncReason    string `json:"syncReason,omitempty"`
	SyncFilesRead int    `json:"syncFilesRead"`
	SyncMillis    int64  `json:"syncMillis"`

	Roles []ProjectMemoryRoleReportResponse `json:"roles"`

	CacheHits   int64 `json:"cacheHits"`
	CacheMisses int64 `json:"cacheMisses"`
}

// ProjectMemoryRoleReportResponse is one role's current context cost, measured
// by assembling that role's pack exactly as a dispatch would.
type ProjectMemoryRoleReportResponse struct {
	Role                string `json:"role" enum:"planner,worker,reviewer,repair"`
	BudgetBytes         int    `json:"budgetBytes"`
	BudgetItems         int    `json:"budgetItems"`
	BudgetDocuments     int    `json:"budgetDocuments"`
	PackItems           int    `json:"packItems"`
	PackBytes           int    `json:"packBytes"`
	EstimatedPackTokens int    `json:"estimatedPackTokens"`
	Candidates          int    `json:"candidates"`
	RejectedByBudget    int    `json:"rejectedByBudget"`
	ReducedToSummary    int    `json:"reducedToSummary"`
	StaleExcluded       int    `json:"staleExcluded"`
	FallbackReason      string `json:"fallbackReason,omitempty"`
}

// ListProjectMemoryKnowledgeResponse is the body of
// GET /api/v1/projects/{id}/memory/knowledge.
//
// It is the P2-C inspection surface: what tasks have taught this project, and
// what of it still holds. Every entry carries its lifecycle links, so the four
// questions the brief names — what did we learn, which decisions are active,
// which risks are open, what was superseded — are all answerable from one
// response rather than from four correlated ones.
type ListProjectMemoryKnowledgeResponse struct {
	RepoID  string                           `json:"repoId,omitempty"`
	Entries []ProjectMemoryKnowledgeResponse `json:"entries"`
	Total   int                              `json:"total"`
}

// ProjectMemoryKnowledgeResponse is one shared fact and its lifecycle.
type ProjectMemoryKnowledgeResponse struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Scope and Key say what the fact is about.
	Scope   string `json:"scope"`
	Key     string `json:"key,omitempty"`
	Summary string `json:"summary"`
	Content string `json:"content,omitempty"`
	// Status is the lifecycle status retrieval would apply: active,
	// superseded, resolved, obsolete or conflicting.
	Status string `json:"status" enum:"active,superseded,resolved,obsolete,conflicting"`
	// Kind distinguishes a risk from a follow-up; empty for other types.
	Kind string `json:"kind,omitempty" enum:"risk,follow_up"`
	// Share is how far the fact may travel.
	Share string `json:"share" enum:"task,workflow,canonical"`
	// Subject is the stable identity of what the fact is about — the field
	// supersession turns on.
	Subject string `json:"subject,omitempty"`
	// SourceTask is the task that produced it.
	SourceTask string `json:"sourceTask,omitempty"`
	// The lifecycle links. Empty when they do not apply.
	SupersededBy  string `json:"supersededBy,omitempty"`
	Supersedes    string `json:"supersedes,omitempty"`
	ResolvedBy    string `json:"resolvedBy,omitempty"`
	ConflictsWith string `json:"conflictsWith,omitempty"`

	State        string   `json:"state" enum:"valid,stale,invalidated,rebuilding"`
	StateReason  string   `json:"stateReason,omitempty"`
	Confidence   float64  `json:"confidence"`
	SourceCommit string   `json:"sourceCommit,omitempty"`
	SourcePaths  []string `json:"sourcePaths,omitempty"`
	UpdatedAt    string   `json:"updatedAt"`
}

// ListProjectMemoryManifestsResponse is the body of
// GET /api/v1/projects/{id}/memory/manifests.
type ListProjectMemoryManifestsResponse struct {
	Entries []ProjectMemoryManifestResponse `json:"entries"`
	Total   int                             `json:"total"`
}

// ProjectMemoryManifestResponse is one execution's frozen context.
//
// It names the facts by id rather than carrying their text, which is what
// keeps a manifest small and what keeps it CORRECT: the items may have been
// superseded since, and a manifest that had copied them would report a premise
// nobody ever held.
type ProjectMemoryManifestResponse struct {
	ID              string    `json:"id"`
	TaskRef         string    `json:"taskRef,omitempty"`
	WorkflowRunID   string    `json:"workflowRunId,omitempty"`
	Role            string    `json:"role" enum:"planner,worker,reviewer,repair"`
	PackDigest      string    `json:"packDigest,omitempty"`
	PolicyVersion   int       `json:"policyVersion"`
	Generation      int64     `json:"generation"`
	IndexedCommit   string    `json:"indexedCommit,omitempty"`
	ItemIDs         []string  `json:"itemIds"`
	ItemCount       int       `json:"itemCount"`
	SelectedBytes   int       `json:"selectedBytes"`
	EstimatedTokens int       `json:"estimatedTokens"`
	CreatedAt       time.Time `json:"createdAt"`
	// Items is the expanded form, present only when the caller asked for it.
	Items []ProjectMemoryKnowledgeResponse `json:"items,omitempty"`
	// Missing names manifest items that no longer exist.
	Missing []string `json:"missing,omitempty"`
}

func projectMemoryKnowledgeResponse(e ProjectMemoryKnowledgeEntry) ProjectMemoryKnowledgeResponse {
	item := e.Item
	return ProjectMemoryKnowledgeResponse{
		ID: item.ID, Type: string(item.Key.Type),
		Scope: string(item.Key.Scope), Key: item.Key.Key,
		Summary: item.Summary, Content: item.Content,
		Status: e.Status, Kind: e.Kind, Share: e.Share, Subject: e.Subject,
		SourceTask:   e.SourceTask,
		SupersededBy: e.SupersededBy, Supersedes: e.Supersedes,
		ResolvedBy: e.ResolvedBy, ConflictsWith: e.ConflictsWith,
		State: string(item.State), StateReason: item.StateReason,
		Confidence:   item.Confidence,
		SourceCommit: item.SourceCommit, SourcePaths: item.SourcePaths,
		UpdatedAt: item.UpdatedAt.Format(rfc3339Milli),
	}
}
