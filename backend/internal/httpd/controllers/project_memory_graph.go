package controllers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// project_memory_graph.go — the code graph's operational surface.
//
// Three verbs, and no more, for the same reason the memory controller beside
// it is small: what an operator has to be able to answer is which backend is
// serving this project's structure, at which generation and commit, how big it
// is, what the last sync had to do, and whether the graph can actually answer a
// question. Everything here serves one of those, plus the one repair that
// follows from a bad answer -- sync it.
//
// There is deliberately no traversal or export endpoint. A graph explorer is a
// product decision this phase does not make (section 34 of the brief), and an
// endpoint that returns the whole graph would be one nobody could use without
// building one.

// ProjectMemoryGraphService is the controller-facing contract for the code
// graph. It is separate from ProjectMemoryService because the graph is
// optional: a build with no graph wired implements this and not that, and the
// routes report not-implemented rather than failing.
type ProjectMemoryGraphService interface {
	// GraphStatus reports every repository's code-graph state.
	GraphStatus(ctx context.Context, projectID domain.ProjectID) ([]ProjectMemoryGraphStatus, error)
	// GraphSync brings one repository's graph up to date, choosing between an
	// incremental update and a full build the same way a dispatch would.
	GraphSync(ctx context.Context, projectID domain.ProjectID, repoPath string, full bool) (ProjectMemoryGraphSyncResult, error)
	// GraphQuery answers a bounded question against the served graph. It is
	// the diagnostic that shows an operator what a dispatch would be told.
	GraphQuery(ctx context.Context, req ProjectMemoryGraphQuery) (ProjectMemoryGraphAnswer, error)
}

// ProjectMemoryGraphStatus is one repository's code-graph state.
type ProjectMemoryGraphStatus struct {
	RepoID   string
	RepoPath string
	// Backend is the implementation that produced this graph, by its real
	// name. It is never a vendor's name for something else.
	Backend       string
	Generation    int64
	Phase         string
	IndexedCommit string
	RepoIdentity  string
	Files         int64
	Symbols       int64
	Edges         int64
	LastSyncKind  string
	FilesParsed   int64
	FilesReused   int64
	FilesRemoved  int64
	LastMillis    int64
	LastError     string
	Architecture  string
	UpdatedAt     string
	// Healthy reports that a complete graph is being served.
	Healthy bool
	// Drift is non-empty when the graph cannot be vouched for: its commit no
	// longer matches the checkout, or its identity does not.
	Drift string
}

// ProjectMemoryGraphSyncResult is what one sync did.
type ProjectMemoryGraphSyncResult struct {
	RepoID         string
	RepoPath       string
	Kind           string
	Generation     int64
	IndexedCommit  string
	FilesScanned   int
	FilesParsed    int
	FilesReused    int
	FilesRemoved   int
	SymbolsAdded   int
	SymbolsRemoved int
	EdgesAdded     int
	EdgesRemoved   int
	Files          int
	Symbols        int
	Edges          int
	Millis         int64
	Truncated      bool
	Reason         string
}

// ProjectMemoryGraphQuery is one bounded question.
type ProjectMemoryGraphQuery struct {
	ProjectID domain.ProjectID
	RepoPath  string
	// Symbol names a declaration to start from; Path anchors on a file; Terms
	// are free text from an objective. At least one must be set.
	Symbol string
	Path   string
	Terms  []string
	Limit  int
}

// ProjectMemoryGraphAnswer is what a query found.
type ProjectMemoryGraphAnswer struct {
	RepoID            string
	Generation        int64
	IndexedCommit     string
	Symbols           []ProjectMemoryGraphSymbol
	Callers           []ProjectMemoryGraphEdge
	Callees           []ProjectMemoryGraphEdge
	Tests             []ProjectMemoryGraphSymbol
	Endpoints         []ProjectMemoryGraphSymbol
	Tables            []string
	Files             []string
	ConsideredSymbols int
	ConsideredEdges   int
	Truncated         bool
	Reason            string
}

// ProjectMemoryGraphSymbol is one declaration in an answer.
type ProjectMemoryGraphSymbol struct {
	ID        string
	Name      string
	Kind      string
	Path      string
	Line      int
	Signature string
	Summary   string
	Exported  bool
	Score     float64
	Reason    string
}

// ProjectMemoryGraphEdge is one relation in an answer.
type ProjectMemoryGraphEdge struct {
	Kind string
	From string
	To   string
	Line int
}

// ProjectMemoryGraphController serves the code graph's routes.
type ProjectMemoryGraphController struct {
	Svc ProjectMemoryGraphService
	// Guard is P4-B's authorization gate, the same one the memory controller
	// uses: every route here is addressed by a project id.
	Guard Guard
}

// Register mounts the routes.
//
// Reading the graph needs the memory read permission -- it is memory, in a
// different shape. Syncing it needs project management, because a full build is
// real work against a real checkout.
func (c *ProjectMemoryGraphController) Register(r chi.Router) {
	r.Get("/projects/{id}/memory/graph", c.scoped(domain.PermMemoryRead, c.status))
	r.Post("/projects/{id}/memory/graph/sync", c.scoped(domain.PermProjectManage, c.sync))
	r.Get("/projects/{id}/memory/graph/query", c.scoped(domain.PermMemoryRead, c.query))
}

func (c *ProjectMemoryGraphController) scoped(perm domain.Permission, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Guard.AllowProject(w, r, perm, projectID(r), "PROJECT_NOT_FOUND", "project not found") {
			return
		}
		h(w, r)
	}
}

func (c *ProjectMemoryGraphController) status(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/graph")
		return
	}
	statuses, err := c.Svc.GraphStatus(r.Context(), domain.ProjectID(chi.URLParam(r, "id")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	out := make([]ProjectMemoryGraphStatusResponse, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, ProjectMemoryGraphStatusResponse(s))
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectMemoryGraphResponse{Repositories: out})
}

func (c *ProjectMemoryGraphController) sync(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/memory/graph/sync")
		return
	}
	q := r.URL.Query()
	out, err := c.Svc.GraphSync(r.Context(), domain.ProjectID(chi.URLParam(r, "id")),
		strings.TrimSpace(q.Get("repoPath")), boolParam(q.Get("full")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectMemoryGraphSyncResponse(out))
}

func (c *ProjectMemoryGraphController) query(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/memory/graph/query")
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
	symbol := strings.TrimSpace(q.Get("symbol"))
	path := strings.TrimSpace(q.Get("path"))
	terms := splitTerms(q.Get("terms"))
	if symbol == "" && path == "" && len(terms) == 0 {
		envelope.WriteError(w, r, apierr.Invalid("invalid_query",
			"a graph query needs a symbol, a path, or search terms", nil))
		return
	}
	out, err := c.Svc.GraphQuery(r.Context(), ProjectMemoryGraphQuery{
		ProjectID: domain.ProjectID(chi.URLParam(r, "id")),
		RepoPath:  strings.TrimSpace(q.Get("repoPath")),
		Symbol:    symbol, Path: path, Terms: terms, Limit: limit,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, projectMemoryGraphAnswerResponse(out))
}

// splitTerms accepts either repeated words separated by spaces or by commas,
// because an operator typing an objective will use both.
func splitTerms(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func boolParam(raw string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	return err == nil && value
}

func projectMemoryGraphAnswerResponse(in ProjectMemoryGraphAnswer) ProjectMemoryGraphAnswerResponse {
	out := ProjectMemoryGraphAnswerResponse{
		RepoID: in.RepoID, Generation: in.Generation, IndexedCommit: in.IndexedCommit,
		Tables: in.Tables, Files: in.Files,
		ConsideredSymbols: in.ConsideredSymbols, ConsideredEdges: in.ConsideredEdges,
		Truncated: in.Truncated, Reason: in.Reason,
		Symbols:   make([]ProjectMemoryGraphSymbolResponse, 0, len(in.Symbols)),
		Callers:   make([]ProjectMemoryGraphEdgeResponse, 0, len(in.Callers)),
		Callees:   make([]ProjectMemoryGraphEdgeResponse, 0, len(in.Callees)),
		Tests:     make([]ProjectMemoryGraphSymbolResponse, 0, len(in.Tests)),
		Endpoints: make([]ProjectMemoryGraphSymbolResponse, 0, len(in.Endpoints)),
	}
	if out.Tables == nil {
		out.Tables = []string{}
	}
	if out.Files == nil {
		out.Files = []string{}
	}
	for _, sym := range in.Symbols {
		out.Symbols = append(out.Symbols, ProjectMemoryGraphSymbolResponse(sym))
	}
	for _, sym := range in.Tests {
		out.Tests = append(out.Tests, ProjectMemoryGraphSymbolResponse(sym))
	}
	for _, sym := range in.Endpoints {
		out.Endpoints = append(out.Endpoints, ProjectMemoryGraphSymbolResponse(sym))
	}
	for _, edge := range in.Callers {
		out.Callers = append(out.Callers, ProjectMemoryGraphEdgeResponse(edge))
	}
	for _, edge := range in.Callees {
		out.Callees = append(out.Callees, ProjectMemoryGraphEdgeResponse(edge))
	}
	return out
}

// GetProjectMemoryGraphQuery is the query string of
// GET /api/v1/projects/{id}/memory/graph/query.
type GetProjectMemoryGraphQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root to query. Defaults to the project's own root, which is the single-repo case."`
	Symbol   string `query:"symbol,omitempty" description:"A declaration to start from, by name (\"Records.MayExport\") or by full symbol id. Ranks above every other signal."`
	Path     string `query:"path,omitempty" description:"A repo-relative file to anchor on. Every symbol declared in it becomes a candidate."`
	Terms    string `query:"terms,omitempty" description:"Free text from an objective, space- or comma-separated. The weakest signal, and the one that finds a starting point when nothing else is known."`
	Limit    int    `query:"limit,omitempty" description:"Maximum symbols to return. Zero uses the retrieval default."`
}

// SyncProjectMemoryGraphQuery is the query string of
// POST /api/v1/projects/{id}/memory/graph/sync.
type SyncProjectMemoryGraphQuery struct {
	RepoPath string `query:"repoPath,omitempty" description:"Repository root to sync. Defaults to the project's own root."`
	Full     bool   `query:"full,omitempty" description:"Force a full rebuild instead of applying the diff since the indexed commit. It is the repair for a graph an operator has reason to distrust; an ordinary sync picks incremental whenever it can prove a change set."`
}
