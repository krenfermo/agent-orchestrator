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
}

// Register mounts the project-memory routes.
func (c *ProjectMemoryController) Register(r chi.Router) {
	r.Get("/projects/{id}/memory", c.status)
	r.Get("/projects/{id}/memory/items", c.inspect)
	r.Post("/projects/{id}/memory/rebuild", c.rebuild)
	r.Post("/projects/{id}/memory/invalidate", c.invalidate)
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
	if !item.InvalidatedAt.IsZero() {
		v := item.InvalidatedAt.Format(rfc3339Milli)
		out.InvalidatedAt = &v
	}
	return out
}
