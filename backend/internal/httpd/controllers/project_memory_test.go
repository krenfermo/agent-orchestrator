package controllers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
)

// project_memory_test.go — the operator-facing contract of P2-A's memory
// surface: that a nil service keeps the routes registered and answers 501, and
// that the status shape reports what an operator actually needs to diagnose
// with, rather than a health tick.

type fakeProjectMemory struct {
	statuses    []domain.ProjectMemoryStatus
	inspection  controllers.ProjectMemoryInspection
	rebuilt     controllers.ProjectMemoryRebuildOutcome
	invalidated controllers.ProjectMemoryInvalidateOutcome
	report      controllers.ProjectMemoryReport
	knowledge   controllers.ProjectMemoryKnowledgeResult
	manifests   controllers.ProjectMemoryManifestResult

	gotKnowledge controllers.ProjectMemoryKnowledgeQuery
	gotManifests controllers.ProjectMemoryManifestQuery
	gotRebuild   struct {
		repoPath string
		purge    bool
	}
	gotInvalidate struct {
		paths  []string
		reason string
	}

	validation  controllers.ProjectMemoryValidation
	gotValidate controllers.ProjectMemoryValidateQuery

	pruned        controllers.ProjectMemoryPruneResult
	gotPruneApply bool

	provenance      controllers.ProjectMemoryProvenance
	provenanceFound bool
	gotProvenanceID string
}

func (f *fakeProjectMemory) Status(context.Context, domain.ProjectID) ([]domain.ProjectMemoryStatus, error) {
	return f.statuses, nil
}

func (f *fakeProjectMemory) Inspect(context.Context, controllers.ProjectMemoryInspectQuery) (controllers.ProjectMemoryInspection, error) {
	return f.inspection, nil
}

func (f *fakeProjectMemory) Knowledge(_ context.Context, q controllers.ProjectMemoryKnowledgeQuery) (controllers.ProjectMemoryKnowledgeResult, error) {
	f.gotKnowledge = q
	return f.knowledge, nil
}

func (f *fakeProjectMemory) Manifests(_ context.Context, q controllers.ProjectMemoryManifestQuery) (controllers.ProjectMemoryManifestResult, error) {
	f.gotManifests = q
	return f.manifests, nil
}

func (f *fakeProjectMemory) Rebuild(_ context.Context, _ domain.ProjectID, repoPath string, purge bool) (controllers.ProjectMemoryRebuildOutcome, error) {
	f.gotRebuild.repoPath, f.gotRebuild.purge = repoPath, purge
	return f.rebuilt, nil
}

func (f *fakeProjectMemory) Invalidate(_ context.Context, _ domain.ProjectID, _ string, paths []string, reason string) (controllers.ProjectMemoryInvalidateOutcome, error) {
	f.gotInvalidate.paths, f.gotInvalidate.reason = paths, reason
	return f.invalidated, nil
}

func (f *fakeProjectMemory) Report(context.Context, domain.ProjectID, string) (controllers.ProjectMemoryReport, error) {
	return f.report, nil
}

func (f *fakeProjectMemory) Validate(_ context.Context, q controllers.ProjectMemoryValidateQuery) (controllers.ProjectMemoryValidation, error) {
	f.gotValidate = q
	return f.validation, nil
}

func (f *fakeProjectMemory) Prune(_ context.Context, _ domain.ProjectID, apply bool) (controllers.ProjectMemoryPruneResult, error) {
	f.gotPruneApply = apply
	return f.pruned, nil
}

func (f *fakeProjectMemory) Provenance(_ context.Context, _ domain.ProjectID, itemID string) (controllers.ProjectMemoryProvenance, bool, error) {
	f.gotProvenanceID = itemID
	return f.provenance, f.provenanceFound, nil
}

func projectMemoryRequest(t *testing.T, svc controllers.ProjectMemoryService, method, path, body string) (int, []byte) {
	t.Helper()
	r := chi.NewRouter()
	(&controllers.ProjectMemoryController{Svc: svc}).Register(r)
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// A nil service keeps every route registered and answers from the spec, so the
// OpenAPI contract and the router never disagree about which paths exist. This
// is what makes project memory a genuinely optional surface: a daemon built
// without it behaves as it did before P2-A.
func TestProjectMemoryRoutesAreNotImplementedWithoutAService(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/projects/p1/memory", ""},
		{http.MethodGet, "/projects/p1/memory/items", ""},
		{http.MethodPost, "/projects/p1/memory/rebuild", `{"repoPath":"/x"}`},
		{http.MethodPost, "/projects/p1/memory/invalidate", `{"repoPath":"/x"}`},
		{http.MethodGet, "/projects/p1/memory/report", ""},
	} {
		status, _ := projectMemoryRequest(t, nil, tc.method, tc.path, tc.body)
		if status != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", tc.method, tc.path, status)
		}
	}
}

func TestProjectMemoryStatusReportsWhatAnOperatorDiagnosesWith(t *testing.T) {
	indexed := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	svc := &fakeProjectMemory{statuses: []domain.ProjectMemoryStatus{{
		RepoID: "repo_abc", RepoPath: "/checkout/app",
		Index: domain.ProjectMemoryIndexState{
			Generation: 7, Phase: domain.IndexPhaseIdle,
			IndexedCommit: "abc123", Branch: "main",
			FilesIndexed: 12, FilesSkipped: 300, CompletedAt: indexed,
		},
		Counts: domain.ProjectMemoryCounts{
			Total: 40, Valid: 37, Stale: 2, Invalidated: 1, TaskLocal: 3, Relations: 55,
		},
		ByType:        map[domain.ProjectMemoryType]int{domain.MemoryTypeModule: 20},
		LastIndexedAt: indexed, LastUpdatedAt: indexed,
	}}}

	status, body := projectMemoryRequest(t, svc, http.MethodGet, "/projects/p1/memory", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out controllers.ListProjectMemoryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Repositories) != 1 {
		t.Fatalf("repositories = %d", len(out.Repositories))
	}
	got := out.Repositories[0]
	switch {
	case got.Generation != 7 || got.IndexedCommit != "abc123":
		t.Errorf("provenance = gen %d at %q", got.Generation, got.IndexedCommit)
	case got.Valid != 37 || got.Stale != 2 || got.Invalidated != 1:
		t.Errorf("the per-state census was not reported: %+v", got)
	case got.TaskLocal != 3:
		t.Error("task-local facts are not reported apart from the canonical ones")
	case !got.Healthy:
		t.Error("a completed pass with valid facts is not reported healthy")
	case got.LastIndexedAt == nil || got.ByType["module"] != 20:
		t.Errorf("lastIndexedAt=%v byType=%v", got.LastIndexedAt, got.ByType)
	}
}

// A failed pass must be visible as such, and must not read as healthy: memory
// AO cannot vouch for is not being served, and an operator has to be able to
// see why.
func TestProjectMemoryStatusSurfacesAFailedPass(t *testing.T) {
	svc := &fakeProjectMemory{statuses: []domain.ProjectMemoryStatus{{
		RepoID: "repo_abc", RepoPath: "/checkout/app",
		Index: domain.ProjectMemoryIndexState{
			Generation: 3, Phase: domain.IndexPhaseFailed, LastError: "disk full",
			Cursor: "internal/workflow/plan.go",
		},
	}}}
	_, body := projectMemoryRequest(t, svc, http.MethodGet, "/projects/p1/memory", "")
	var out controllers.ListProjectMemoryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	got := out.Repositories[0]
	if got.Healthy {
		t.Error("a failed pass is reported healthy")
	}
	if got.LastError != "disk full" || got.Phase != "failed" {
		t.Errorf("failure not surfaced: phase=%q err=%q", got.Phase, got.LastError)
	}
	if got.ResumeCursor != "internal/workflow/plan.go" {
		t.Error("the resume point a restart would continue from is not reported")
	}
}

func TestProjectMemoryRebuildPassesThroughPurge(t *testing.T) {
	svc := &fakeProjectMemory{rebuilt: controllers.ProjectMemoryRebuildOutcome{
		RepoID: "repo_abc", Generation: 8, FilesIndexed: 5, ItemsWritten: 9, IndexedCommit: "def",
	}}
	status, body := projectMemoryRequest(t, svc, http.MethodPost, "/projects/p1/memory/rebuild",
		`{"repoPath":"/checkout/app","purge":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if svc.gotRebuild.repoPath != "/checkout/app" || !svc.gotRebuild.purge {
		t.Fatalf("service received %+v", svc.gotRebuild)
	}
	var out controllers.RebuildProjectMemoryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Generation != 8 || out.ItemsWritten != 9 {
		t.Fatalf("outcome = %+v", out)
	}
}

// An invalidate with no paths is the drift-detection repair, and the response
// distinguishes "checked and found nothing" from "did not check".
func TestProjectMemoryInvalidateReportsDriftCounts(t *testing.T) {
	svc := &fakeProjectMemory{invalidated: controllers.ProjectMemoryInvalidateOutcome{
		RepoID: "repo_abc", ItemsInvalidated: 2, DriftChecked: 37, DriftFound: 2,
	}}
	status, body := projectMemoryRequest(t, svc, http.MethodPost, "/projects/p1/memory/invalidate",
		`{"repoPath":"/checkout/app","reason":"post-rebase"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	if svc.gotInvalidate.reason != "post-rebase" || len(svc.gotInvalidate.paths) != 0 {
		t.Fatalf("service received %+v", svc.gotInvalidate)
	}
	var out controllers.InvalidateProjectMemoryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.DriftChecked != 37 || out.DriftFound != 2 || out.ItemsInvalidated != 2 {
		t.Fatalf("outcome = %+v", out)
	}
}

func TestProjectMemoryInspectRejectsABadLimit(t *testing.T) {
	status, _ := projectMemoryRequest(t, &fakeProjectMemory{}, http.MethodGet,
		"/projects/p1/memory/items?limit=-3", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}

// An inspect reports facts AO can no longer vouch for, with the reason. That is
// the difference between an inspect and a context pack, and it is the whole
// point of the surface.
func TestProjectMemoryInspectCarriesStateAndReason(t *testing.T) {
	item := domain.ProjectMemoryItem{
		Key: domain.ProjectMemoryKey{
			ProjectID: "p1", RepoID: "repo_abc",
			Type: domain.MemoryTypeConvention, Scope: domain.MemoryScopeRepository, Key: "AGENTS.md#hard-rules",
		},
		Summary: "AGENTS.md: Hard rules", Content: "a body that should not be shipped",
		State: domain.MemoryStateStale, StateReason: "source content moved since this fact was derived",
		Confidence: 0.95, Generation: 4, SourcePaths: []string{"AGENTS.md"},
		UpdatedAt: time.Now().UTC(),
	}
	svc := &fakeProjectMemory{inspection: controllers.ProjectMemoryInspection{
		RepoID: "repo_abc", Items: []domain.ProjectMemoryItem{item.Normalized()}, Total: 1,
	}}
	_, body := projectMemoryRequest(t, svc, http.MethodGet, "/projects/p1/memory/items?state=stale", "")
	var out controllers.ListProjectMemoryItemsResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %d", len(out.Items))
	}
	got := out.Items[0]
	if got.State != "stale" || got.StateReason == "" {
		t.Errorf("state=%q reason=%q", got.State, got.StateReason)
	}
	if strings.Contains(string(body), "should not be shipped") {
		t.Error("an inspect shipped the fact bodies; it reports what is remembered, not the whole memory")
	}
	if got.ContentBytes == 0 {
		t.Error("the body size is not reported, so an operator cannot tell an empty fact from a large one")
	}
}

// The report is the P2-B operational answer: is this project warm, and what is
// memory costing each role. A warm project must be reported as such — that is
// the optimisation's own success criterion.
func TestProjectMemoryReportSurfacesWarmthAndPerRoleCost(t *testing.T) {
	svc := &fakeProjectMemory{report: controllers.ProjectMemoryReport{
		Mode: "assisted", CacheEnabled: true, SyncTimeout: "20s",
		RepoID: "repo_abc", RepoPath: "/checkout/app",
		Warm: true, Generation: 7, IndexedCommit: "abc123",
		SyncKind: "none", SyncFilesRead: 0, SyncMillis: 2,
		CacheHits: 3, CacheMisses: 1,
		Roles: []controllers.ProjectMemoryRoleReport{
			{Role: "planner", BudgetBytes: 24576, BudgetItems: 40, PackItems: 40,
				PackBytes: 5981, EstimatedPackTokens: 1496, Candidates: 467, RejectedByBudget: 427},
			{Role: "worker", BudgetBytes: 16384, BudgetItems: 24, PackItems: 14,
				PackBytes: 15900, EstimatedPackTokens: 3975, Candidates: 546, RejectedByBudget: 532},
		},
	}}

	status, body := projectMemoryRequest(t, svc, http.MethodGet, "/projects/p1/memory/report", "")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	var out controllers.ProjectMemoryReportResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	switch {
	case !out.Warm || out.SyncKind != "none":
		t.Errorf("a warm project was not reported as warm: warm=%v kind=%q", out.Warm, out.SyncKind)
	case out.SyncFilesRead != 0:
		t.Errorf("a warm project reported %d files read", out.SyncFilesRead)
	case len(out.Roles) != 2:
		t.Fatalf("roles = %d", len(out.Roles))
	case out.Roles[0].EstimatedPackTokens == 0:
		t.Error("the per-role token estimate is missing")
	case out.Roles[0].RejectedByBudget == 0:
		t.Error("the budget's effect is not reported")
	case out.Mode != "assisted":
		t.Errorf("mode = %q", out.Mode)
	}
}
