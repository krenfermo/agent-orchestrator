package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// project_intelligence.go -- P4-G's read surface.
//
// Every route here is project-scoped and goes through Guard.AllowProject,
// which since P4-C resolves through the caller's organizations. That is the
// whole of the tenancy story for this feature: a project in another
// organization never enters the caller's resolved subject, so a guessed
// project id on any of these routes answers 404 exactly as it does on
// /projects/{id} itself. There is no separate tenancy check here, because a
// second one is a second thing that can be wrong.
//
// Read routes require memory.read; the two that cost real work -- sync and
// rebuild -- require project.manage, matching the existing graph sync.

// ProjectIntelligenceService is the controller-facing contract.
type ProjectIntelligenceService interface {
	Intelligence(ctx context.Context, projectID domain.ProjectID) (ProjectIntelligenceOverview, error)
	IntelligenceArchitecture(ctx context.Context, projectID domain.ProjectID, repoPath string) (ProjectIntelligenceArchitecture, error)
	IntelligenceSubgraph(ctx context.Context, req ProjectIntelligenceSubgraphQuery) (ProjectIntelligenceSubgraph, error)
	IntelligenceSearch(ctx context.Context, req ProjectIntelligenceSearchQuery) (ProjectIntelligenceSearchResult, error)
	IntelligenceContext(ctx context.Context, req ProjectIntelligenceContextQuery) (ProjectIntelligenceContextPreview, error)
}

// ProjectIntelligenceOverview is the body of GET /projects/{id}/intelligence.
type ProjectIntelligenceOverview struct {
	ProjectID string                          `json:"projectId"`
	Repos     []ProjectIntelligenceRepoStatus `json:"repos"`
}

// ProjectIntelligenceRepoStatus is one repository's state.
type ProjectIntelligenceRepoStatus struct {
	RepoID   string `json:"repoId"`
	RepoPath string `json:"repoPath"`
	Backend  string `json:"backend,omitempty"`
	// State is the derived lifecycle: pending, indexing, ready, stale, failed.
	State string `json:"state" enum:"pending,indexing,ready,stale,failed"`
	// Drift explains a stale state in words.
	Drift         string `json:"drift,omitempty"`
	IndexedCommit string `json:"indexedCommit,omitempty"`
	HeadCommit    string `json:"headCommit,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Generation    int64  `json:"generation"`
	Files         int64  `json:"files"`
	Symbols       int64  `json:"symbols"`
	Edges         int64  `json:"edges"`
	LastSyncKind  string `json:"lastSyncKind,omitempty"`
	FilesParsed   int64  `json:"filesParsed"`
	FilesReused   int64  `json:"filesReused"`
	FilesRemoved  int64  `json:"filesRemoved"`
	LastMillis    int64  `json:"lastMillis"`
	LastError     string `json:"lastError,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
	MemoryItems   int64  `json:"memoryItems"`
	MemoryState   string `json:"memoryState,omitempty"`
}

// ProjectIntelligenceArchitecture is the body of the architecture route.
type ProjectIntelligenceArchitecture struct {
	// Architecture is the structured summary the build derived. Its shape is
	// the code graph's own, passed through rather than re-modelled here.
	Architecture map[string]any `json:"architecture"`
	// Rendered is the same facts as text, which is what a dispatch is given.
	Rendered string `json:"rendered,omitempty"`
}

// ProjectIntelligenceSubgraphQuery is one bounded traversal request.
type ProjectIntelligenceSubgraphQuery struct {
	ProjectID domain.ProjectID
	RepoPath  string
	Symbol    string
	Path      string
	Depth     int
	NodeKinds []string
	EdgeKinds []string
	MaxNodes  int
	MaxEdges  int
}

// ProjectIntelligenceSubgraphNode is one node of a traversal.
type ProjectIntelligenceSubgraphNode struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Path      string `json:"path,omitempty"`
	Language  string `json:"language,omitempty"`
	Line      int64  `json:"line,omitempty"`
	Signature string `json:"signature,omitempty"`
	Summary   string `json:"summary,omitempty"`
	Exported  bool   `json:"exported"`
	Depth     int    `json:"depth"`
}

// ProjectIntelligenceSubgraphEdge is one relation of a traversal.
type ProjectIntelligenceSubgraphEdge struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	Path string `json:"path,omitempty"`
	Line int64  `json:"line,omitempty"`
}

// ProjectIntelligenceSubgraph is the body of the graph route.
type ProjectIntelligenceSubgraph struct {
	Seeds []string                          `json:"seeds"`
	Nodes []ProjectIntelligenceSubgraphNode `json:"nodes"`
	Edges []ProjectIntelligenceSubgraphEdge `json:"edges"`
	// Truncated reports that a ceiling bound the walk and the neighbourhood
	// shown is incomplete.
	Truncated     bool   `json:"truncated"`
	Generation    int64  `json:"generation"`
	IndexedCommit string `json:"indexedCommit,omitempty"`
}

// ProjectIntelligenceSearchQuery is one search.
type ProjectIntelligenceSearchQuery struct {
	ProjectID domain.ProjectID
	RepoPath  string
	Query     string
	Limit     int
}

// ProjectIntelligenceSearchHit is one answer with its provenance.
type ProjectIntelligenceSearchHit struct {
	Kind         string `json:"kind" enum:"memory,symbol"`
	Title        string `json:"title"`
	Detail       string `json:"detail,omitempty"`
	Path         string `json:"path,omitempty"`
	Line         int64  `json:"line,omitempty"`
	SymbolKind   string `json:"symbolKind,omitempty"`
	MemoryType   string `json:"memoryType,omitempty"`
	State        string `json:"state,omitempty"`
	SourceCommit string `json:"sourceCommit,omitempty"`
	Score        int    `json:"score"`
}

// ProjectIntelligenceSearchResult is the body of the search route.
type ProjectIntelligenceSearchResult struct {
	Query         string                         `json:"query"`
	Hits          []ProjectIntelligenceSearchHit `json:"hits"`
	MemoryHits    int                            `json:"memoryHits"`
	SymbolHits    int                            `json:"symbolHits"`
	Truncated     bool                           `json:"truncated"`
	Generation    int64                          `json:"generation"`
	IndexedCommit string                         `json:"indexedCommit,omitempty"`
}

// ProjectIntelligenceContextQuery asks what one role would be given.
type ProjectIntelligenceContextQuery struct {
	ProjectID    domain.ProjectID
	RepoPath     string
	Role         string
	ChangedPaths []string
	Keywords     []string
}

// ProjectIntelligenceContextItem is one durable fact in a previewed pack.
type ProjectIntelligenceContextItem struct {
	Summary      string   `json:"summary"`
	Content      string   `json:"content,omitempty"`
	BodyIncluded bool     `json:"bodyIncluded"`
	Type         string   `json:"type,omitempty"`
	State        string   `json:"state,omitempty"`
	SourcePaths  []string `json:"sourcePaths,omitempty"`
	SourceCommit string   `json:"sourceCommit,omitempty"`
	Score        float64  `json:"score"`
	Reason       string   `json:"reason,omitempty"`
}

// ProjectIntelligenceContextSection is one group of facts in a previewed pack.
type ProjectIntelligenceContextSection struct {
	Title string                           `json:"title"`
	Type  string                           `json:"type,omitempty"`
	Items []ProjectIntelligenceContextItem `json:"items"`
}

// ProjectIntelligenceContextGraph is the graph half of a previewed pack.
type ProjectIntelligenceContextGraph struct {
	Backend           string   `json:"backend,omitempty"`
	Generation        int64    `json:"generation"`
	IndexedCommit     string   `json:"indexedCommit,omitempty"`
	Architecture      string   `json:"architecture,omitempty"`
	Symbols           []string `json:"symbols,omitempty"`
	Files             []string `json:"files,omitempty"`
	Endpoints         []string `json:"endpoints,omitempty"`
	Tables            []string `json:"tables,omitempty"`
	Tests             []string `json:"tests,omitempty"`
	ConsideredSymbols int      `json:"consideredSymbols"`
	ConsideredEdges   int      `json:"consideredEdges"`
	SelectedSymbols   int      `json:"selectedSymbols"`
	SelectedEdges     int      `json:"selectedEdges"`
	Bytes             int      `json:"bytes"`
	EstimatedTokens   int      `json:"estimatedTokens"`
}

// ProjectIntelligenceContextPreview is the body of the context route.
//
// The measurement vocabulary is "selected" and "avoided", never "saved": AO
// cannot observe what the coding harness reads inside the worktree, so it
// cannot know what its context prevented anybody from reading. Every number
// here is something AO actually did.
type ProjectIntelligenceContextPreview struct {
	Role             string                              `json:"role" enum:"planner,worker,reviewer,repair"`
	ProjectID        string                              `json:"projectId"`
	RepoID           string                              `json:"repoId,omitempty"`
	Sections         []ProjectIntelligenceContextSection `json:"sections"`
	Graph            ProjectIntelligenceContextGraph     `json:"graph"`
	CandidateItems   int                                 `json:"candidateItems"`
	CandidateBytes   int                                 `json:"candidateBytes"`
	SelectedItems    int                                 `json:"selectedItems"`
	SelectedBytes    int                                 `json:"selectedBytes"`
	EstimatedTokens  int                                 `json:"estimatedTokens"`
	DroppedItems     int                                 `json:"droppedItems"`
	DroppedToSummary int                                 `json:"droppedToSummary"`
	StaleExcluded    int                                 `json:"staleExcluded"`
	SourcesReused    []string                            `json:"sourcesReused,omitempty"`
	FallbackReason   string                              `json:"fallbackReason,omitempty"`
	IndexedCommit    string                              `json:"indexedCommit,omitempty"`
	Generation       int64                               `json:"generation"`
	Digest           string                              `json:"digest,omitempty"`
	Empty            bool                                `json:"empty"`
}

// ProjectIntelligenceController owns the /projects/{id}/intelligence routes.
type ProjectIntelligenceController struct {
	Svc ProjectIntelligenceService
	// Sync reuses the existing graph service for the two write actions, so a
	// manual sync from this UI exercises exactly the production path rather
	// than a second one that could diverge from it.
	Sync  ProjectMemoryGraphService
	Guard Guard
}

// Register mounts the Project Intelligence routes.
func (c *ProjectIntelligenceController) Register(r chi.Router) {
	r.Get("/projects/{id}/intelligence", c.scoped(domain.PermMemoryRead, c.overview))
	r.Get("/projects/{id}/intelligence/architecture", c.scoped(domain.PermMemoryRead, c.architecture))
	r.Get("/projects/{id}/intelligence/graph", c.scoped(domain.PermMemoryRead, c.subgraph))
	r.Get("/projects/{id}/intelligence/search", c.scoped(domain.PermMemoryRead, c.search))
	r.Get("/projects/{id}/intelligence/context", c.scoped(domain.PermMemoryRead, c.context))
	r.Post("/projects/{id}/intelligence/sync", c.scoped(domain.PermProjectManage, c.sync))
	r.Post("/projects/{id}/intelligence/rebuild", c.scoped(domain.PermProjectManage, c.rebuild))
}

func (c *ProjectIntelligenceController) scoped(perm domain.Permission, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Guard.AllowProject(w, r, perm, projectID(r), "PROJECT_NOT_FOUND", "project not found") {
			return
		}
		h(w, r)
	}
}

func (c *ProjectIntelligenceController) overview(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/intelligence")
		return
	}
	out, err := c.Svc.Intelligence(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ProjectIntelligenceController) architecture(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/intelligence/architecture")
		return
	}
	out, err := c.Svc.IntelligenceArchitecture(r.Context(), projectID(r),
		strings.TrimSpace(r.URL.Query().Get("repoPath")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ProjectIntelligenceController) subgraph(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/intelligence/graph")
		return
	}
	q := r.URL.Query()
	out, err := c.Svc.IntelligenceSubgraph(r.Context(), ProjectIntelligenceSubgraphQuery{
		ProjectID: projectID(r),
		RepoPath:  strings.TrimSpace(q.Get("repoPath")),
		Symbol:    strings.TrimSpace(q.Get("symbol")),
		Path:      strings.TrimSpace(q.Get("path")),
		Depth:     intParam(q.Get("depth")),
		NodeKinds: splitTerms(q.Get("nodeKinds")),
		EdgeKinds: splitTerms(q.Get("edgeKinds")),
		MaxNodes:  intParam(q.Get("maxNodes")),
		MaxEdges:  intParam(q.Get("maxEdges")),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ProjectIntelligenceController) search(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/intelligence/search")
		return
	}
	q := r.URL.Query()
	out, err := c.Svc.IntelligenceSearch(r.Context(), ProjectIntelligenceSearchQuery{
		ProjectID: projectID(r),
		RepoPath:  strings.TrimSpace(q.Get("repoPath")),
		Query:     strings.TrimSpace(q.Get("q")),
		Limit:     intParam(q.Get("limit")),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *ProjectIntelligenceController) context(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/intelligence/context")
		return
	}
	q := r.URL.Query()
	out, err := c.Svc.IntelligenceContext(r.Context(), ProjectIntelligenceContextQuery{
		ProjectID:    projectID(r),
		RepoPath:     strings.TrimSpace(q.Get("repoPath")),
		Role:         strings.TrimSpace(q.Get("role")),
		ChangedPaths: splitTerms(q.Get("changedPaths")),
		Keywords:     splitTerms(q.Get("keywords")),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// sync brings the graph up to date, choosing incremental or full the way a
// dispatch would.
func (c *ProjectIntelligenceController) sync(w http.ResponseWriter, r *http.Request) {
	c.runSync(w, r, false, "/api/v1/projects/{id}/intelligence/sync")
}

// rebuild forces a full pass. The confirmation this needs is the frontend's
// job: an API that asked for one would be an API a script could not use.
func (c *ProjectIntelligenceController) rebuild(w http.ResponseWriter, r *http.Request) {
	c.runSync(w, r, true, "/api/v1/projects/{id}/intelligence/rebuild")
}

func (c *ProjectIntelligenceController) runSync(w http.ResponseWriter, r *http.Request, full bool, route string) {
	if c.Sync == nil {
		apispec.NotImplemented(w, r, "POST", route)
		return
	}
	out, err := c.Sync.GraphSync(r.Context(), projectID(r),
		strings.TrimSpace(r.URL.Query().Get("repoPath")), full)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectMemoryGraphSyncResponse(out))
}

func intParam(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return n
}

// --- OpenAPI query-parameter shapes -------------------------------------
//
// These exist only so the generated spec documents the query strings above.
// They are never decoded from; the handlers read r.URL.Query() directly, the
// way every other query-parameter route in this package does.

// ProjectIntelligenceRepoQuery is the query string of the routes that take
// only a repository.
type ProjectIntelligenceRepoQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root. Defaults to the project's own root, which is the single-repo case."`
}

// GetProjectIntelligenceGraphQuery is the query string of the bounded
// traversal route.
type GetProjectIntelligenceGraphQuery struct {
	RepoPath  string `query:"repoPath,omitempty" description:"Repository root. Defaults to the project's own root."`
	Symbol    string `query:"symbol,omitempty" description:"Declaration to start the walk from, by name or by full symbol id. One of symbol or path is required: there is no whole-graph export."`
	Path      string `query:"path,omitempty" description:"Repo-relative file to start the walk from. Every symbol declared in it seeds the traversal."`
	Depth     int    `query:"depth,omitempty" description:"Hops to walk outward. Capped at 2 — three hops from a densely-connected symbol is already tens of thousands of nodes."`
	NodeKinds string `query:"nodeKinds,omitempty" description:"Comma-separated symbol kinds to keep (func, type, method, ...). Empty keeps every kind."`
	EdgeKinds string `query:"edgeKinds,omitempty" description:"Comma-separated relation kinds to keep (calls, imports, ...). Empty keeps every kind."`
	MaxNodes  int    `query:"maxNodes,omitempty" description:"Node ceiling for this walk. Clamped to the server's hard maximum; a caller cannot raise it."`
	MaxEdges  int    `query:"maxEdges,omitempty" description:"Edge ceiling for this walk. Clamped to the server's hard maximum."`
}

// GetProjectIntelligenceSearchQuery is the query string of the search route.
type GetProjectIntelligenceSearchQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root. Defaults to the project's own root."`
	Q        string `query:"q" description:"The question, in any language. Terms shorter than three characters are ignored because they match everything."`
	Limit    int    `query:"limit,omitempty" description:"Maximum hits. Clamped to the server's maximum."`
}

// GetProjectIntelligenceContextQuery is the query string of the context-pack
// preview route.
type GetProjectIntelligenceContextQuery struct {
	RepoPath     string `query:"repoPath,omitempty" description:"Repository root. Defaults to the project's own root; a planner preview may span every repository."`
	Role         string `query:"role" description:"Which agent's pack to assemble: planner, worker, reviewer or repair." enum:"planner,worker,reviewer,repair"`
	ChangedPaths string `query:"changedPaths,omitempty" description:"Comma-separated repo-relative paths the work touches. The strongest relevance signal there is."`
	Keywords     string `query:"keywords,omitempty" description:"Comma-separated free-text terms from the objective. The weakest signal, used to break ties."`
}
