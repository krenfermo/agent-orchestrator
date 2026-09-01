// Package projectmemory is the read/repair service behind AO's project-memory
// API and CLI.
//
// It is a thin resolver over internal/projectmemory: its whole job is to turn
// a project id (which is what an HTTP caller has) into the repository paths
// project memory is keyed by (which is what the memory subsystem needs), and
// to keep the two vocabularies from leaking into each other.
//
// One policy lives here rather than in the memory package, because it is about
// AO's project registry rather than about memory: **a request that names no
// repository operates on the project's own root.** That is the single-repo
// case, which is most of them, and requiring an operator to paste a path for
// it would make `ao memory status` useless.
package projectmemory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	pm "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

// ProjectReader is the slice of the project registry this service needs: where
// a project lives, and which repositories it spans.
type ProjectReader interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	ListWorkspaceRepos(ctx context.Context, projectID string) ([]domain.WorkspaceRepoRecord, error)
}

// Service backs the project-memory HTTP surface.
type Service struct {
	memory   *pm.Service
	projects ProjectReader
	// provisioner is the P2-B dispatch path. It is optional: with memory
	// switched off it is nil, and the report says so rather than pretending to
	// measure a subsystem nobody enabled.
	provisioner *pm.Provisioner
}

// New builds the service over the memory subsystem and the project registry.
func New(memory *pm.Service, projects ProjectReader) *Service {
	return &Service{memory: memory, projects: projects}
}

// WithProvisioner attaches the dispatch-path provisioner, so `memory report`
// measures the same assembly a dispatch would rather than a reconstruction of
// it. A nil provisioner leaves the report saying memory is off.
func (s *Service) WithProvisioner(p *pm.Provisioner) *Service {
	s.provisioner = p
	return s
}

// Compile-time proof that the service satisfies the controller contract.
var _ controllers.ProjectMemoryService = (*Service)(nil)

// Status reports every repository of a project that has memory.
//
// It reports what the memory subsystem has actually registered rather than
// what the project registry contains, and the difference is the useful part:
// a repository that appears in the registry and not here is one AO has never
// indexed, which is exactly what an operator asking "why is there no memory"
// needs to see.
func (s *Service) Status(ctx context.Context, projectID domain.ProjectID) ([]domain.ProjectMemoryStatus, error) {
	return s.memory.StatusAll(ctx, projectID)
}

// Inspect reads stored facts for an operator.
func (s *Service) Inspect(ctx context.Context, req controllers.ProjectMemoryInspectQuery) (controllers.ProjectMemoryInspection, error) {
	repoPath, err := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
	if err != nil {
		return controllers.ProjectMemoryInspection{}, err
	}
	res, err := s.memory.Inspect(ctx, pm.InspectRequest{
		ProjectID: req.ProjectID, RepoPath: repoPath,
		State: req.State, Type: req.Type, PathPrefix: req.PathPrefix, Limit: req.Limit,
	})
	if err != nil {
		return controllers.ProjectMemoryInspection{}, err
	}
	return controllers.ProjectMemoryInspection{
		RepoID: res.RepoID, Items: res.Items, Total: res.Total, Truncated: res.Truncated,
	}, nil
}

// Rebuild re-derives one repository's memory at its current checkout state.
func (s *Service) Rebuild(ctx context.Context, projectID domain.ProjectID, repoPath string, purge bool) (controllers.ProjectMemoryRebuildOutcome, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryRebuildOutcome{}, err
	}
	commit, branch := pm.HeadOf(ctx, resolved)
	out, err := s.memory.Rebuild(ctx, projectID, resolved, commit, branch, purge)
	if err != nil {
		return controllers.ProjectMemoryRebuildOutcome{}, err
	}
	return controllers.ProjectMemoryRebuildOutcome{
		RepoID: out.RepoID, Generation: out.Generation,
		Skipped: out.Skipped, SkipReason: out.SkipReason,
		FilesIndexed: out.FilesIndexed, FilesSkipped: out.FilesSkipped,
		ItemsWritten: out.ItemsWritten, ItemsReconfirmed: out.ItemsReconfirmed,
		ItemsRetired: out.ItemsRetired, IndexedCommit: out.IndexedCommit,
		Truncated: out.Truncated, TruncatedReason: out.TruncatedReason,
	}, nil
}

// Invalidate retires memory an operator knows, or suspects, is wrong.
//
// With explicit paths it retires exactly what those paths proved. With no
// paths it runs drift detection and applies what that finds — the honest
// answer to "something moved and I cannot tell you what", and a far better one
// than invalidating everything.
func (s *Service) Invalidate(
	ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (controllers.ProjectMemoryInvalidateOutcome, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryInvalidateOutcome{}, err
	}
	if len(domain.NormalizeMemorySourcePaths(paths)) > 0 {
		n, err := s.memory.Invalidate(ctx, projectID, resolved, paths, reason)
		if err != nil {
			return controllers.ProjectMemoryInvalidateOutcome{}, err
		}
		return controllers.ProjectMemoryInvalidateOutcome{
			RepoID: domain.ProjectMemoryRepoID(resolved), ItemsInvalidated: n,
		}, nil
	}

	commit, _ := pm.HeadOf(ctx, resolved)
	report, err := s.memory.Verify(ctx, projectID, resolved, commit, true)
	if err != nil {
		return controllers.ProjectMemoryInvalidateOutcome{}, err
	}
	applied := int64(0)
	for _, f := range report.Findings {
		if f.Applied {
			applied++
		}
	}
	return controllers.ProjectMemoryInvalidateOutcome{
		RepoID: report.RepoID, ItemsInvalidated: applied,
		DriftChecked: report.Checked, DriftFound: len(report.Findings),
	}, nil
}

// Validate runs the P2-D authority pass over one repository.
//
// It resolves the repository through the same guard every other write goes
// through, so a caller cannot point a validation pass at an unrelated checkout
// under a project's id -- which would report that project's memory as
// unprovable on the strength of a repository it is not about.
func (s *Service) Validate(
	ctx context.Context, req controllers.ProjectMemoryValidateQuery,
) (controllers.ProjectMemoryValidation, error) {
	resolved, err := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
	if err != nil {
		return controllers.ProjectMemoryValidation{}, err
	}
	report, err := s.memory.Validate(ctx, pm.ValidateRequest{
		ProjectID: req.ProjectID,
		RepoPath:  resolved,
		Apply:     req.Apply,
		MaxChecks: req.Limit,
	})
	if err != nil {
		return controllers.ProjectMemoryValidation{}, err
	}
	out := controllers.ProjectMemoryValidation{
		RepoID:           report.RepoID,
		RepoIdentity:     report.Observed.String(),
		Applied:          req.Apply,
		Checked:          report.Checked,
		Provable:         report.Provable,
		IdentityWithheld: report.IdentityWithheld,
		LegacyClassified: report.LegacyClassified,
		EdgesRetired:     report.EdgesRetired,
		Truncated:        report.Truncated,
	}
	for _, f := range report.Findings {
		out.Findings = append(out.Findings, controllers.ProjectMemoryValidationFinding{
			ItemID:      f.ItemID,
			Type:        string(f.Key.Type),
			Scope:       string(f.Key.Scope),
			Key:         f.Key.Key,
			From:        string(f.From),
			To:          string(f.To),
			ReasonClass: f.ReasonClass,
			Detail:      f.Detail,
			Applied:     f.Applied,
		})
	}
	return out, nil
}

// Provenance answers the whole P2-D section 27 question list for one fact:
// why is it valid, what task produced it, which commit supports it, was it
// repaired, where was it born, how did it become canonical, what invalidated
// it, and what replaced it.
//
// The project id is checked rather than merely accepted. Item ids are derived
// hashes and are not guessable, but a diagnostic that returned any project's
// fact to any project's caller would be a cross-project read, and this
// subsystem's whole argument is that one project's knowledge must not reach
// another.
func (s *Service) Provenance(
	ctx context.Context, projectID domain.ProjectID, itemID string,
) (controllers.ProjectMemoryProvenance, bool, error) {
	prov, found, err := s.memory.Provenance(ctx, strings.TrimSpace(itemID))
	if err != nil || !found {
		return controllers.ProjectMemoryProvenance{}, found, err
	}
	if prov.Item.Key.ProjectID != projectID {
		return controllers.ProjectMemoryProvenance{}, false, nil
	}
	return controllers.ProjectMemoryProvenance{
		Item:                 prov.Item,
		Servable:             prov.Servable,
		AuthorityReasonClass: prov.AuthorityReasonClass,
		Relations:            prov.Relations,
	}, true, nil
}

// resolveRepo turns an optional repository path into a concrete one.
//
// An explicit path is honoured as given. An empty path resolves to the
// project's own root — the single-repo case. A path that is not one of the
// project's repositories is refused rather than indexed: project memory is
// scoped by project, and indexing an unrelated checkout under a project's id
// would put facts about one codebase into another's memory.
func (s *Service) resolveRepo(ctx context.Context, projectID domain.ProjectID, repoPath string) (string, error) {
	project, found, err := s.projects.GetProject(ctx, string(projectID))
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("project %s is not registered", projectID)
	}
	requested := strings.TrimSpace(repoPath)
	if requested == "" {
		return project.Path, nil
	}

	allowed := []string{project.Path}
	repos, err := s.projects.ListWorkspaceRepos(ctx, string(projectID))
	if err == nil {
		for _, r := range repos {
			// A workspace child is registered by its path relative to the
			// project root, so the absolute form has to be rebuilt here.
			allowed = append(allowed, filepath.Join(project.Path, r.RelativePath))
		}
	}
	for _, candidate := range allowed {
		if pm.SameRepoPath(candidate, requested) {
			return requested, nil
		}
	}
	return "", fmt.Errorf("%s is not a repository of project %s", requested, projectID)
}

// Report answers the P2-B operational question: is this project's memory warm,
// and what is it costing each role right now.
//
// It performs a real freshness check and assembles a real pack per role,
// through the same provisioner a dispatch uses. That is deliberate: an operator
// surface that estimated the cost would drift from what agents actually
// receive, and the whole point of this report is that the two are the same
// number.
//
// The check it runs is the ordinary lifecycle one, so on a warm project this
// costs a row read and no file I/O — running the report does not itself make
// the project warm, and cannot be mistaken for having done so.
func (s *Service) Report(ctx context.Context, projectID domain.ProjectID, repoPath string) (controllers.ProjectMemoryReport, error) {
	resolved, err := s.resolveRepo(ctx, projectID, repoPath)
	if err != nil {
		return controllers.ProjectMemoryReport{}, err
	}
	if s.provisioner == nil {
		// Memory is switched off. Saying so plainly is more useful than an
		// empty report an operator would read as "warm with nothing in it".
		return controllers.ProjectMemoryReport{
			Mode: string(pm.ModeOff), RepoPath: resolved,
			SyncKind: string(pm.SyncSkipped), SyncReason: "project memory is switched off",
		}, nil
	}

	cfg := s.provisioner.Config()
	report := controllers.ProjectMemoryReport{
		Mode: string(cfg.Mode), CacheEnabled: cfg.CacheEnabled,
		SyncTimeout: cfg.SyncTimeout.String(), RepoPath: resolved,
	}

	for _, role := range []pm.PackRole{pm.RolePlanner, pm.RoleWorker, pm.RoleReviewer, pm.RoleRepair} {
		out := s.provisioner.Provision(ctx, pm.ProvisionRequest{
			ProjectID: projectID, RepoPath: resolved, Role: role,
			// Only the first role performs the freshness check; the rest reuse
			// it, exactly as four dispatch boundaries would.
			SkipSync: role != pm.RolePlanner,
		})
		if role == pm.RolePlanner {
			report.RepoID = out.Freshness.RepoID
			report.Warm = out.Freshness.Kind == pm.SyncNone
			report.Generation = out.Metrics.Generation
			report.IndexedCommit = out.Metrics.IndexedCommit
			report.SyncKind = out.Metrics.SyncKind
			report.SyncReason = out.Freshness.Reason
			report.SyncFilesRead = out.Metrics.SyncFilesRead
			report.SyncMillis = out.Metrics.SyncMillis
		}
		budget := cfg.Budgets.For(role)
		report.Roles = append(report.Roles, controllers.ProjectMemoryRoleReport{
			Role:                string(role),
			BudgetBytes:         budget.MaxBytes,
			BudgetItems:         budget.MaxItems,
			BudgetDocuments:     budget.MaxDocuments,
			PackItems:           out.Metrics.PackItems,
			PackBytes:           out.Metrics.PackBytes,
			EstimatedPackTokens: out.Metrics.EstimatedPackTokens,
			Candidates:          out.Metrics.PackCandidates,
			RejectedByBudget:    out.Metrics.PackRejectedByBudget,
			ReducedToSummary:    out.Metrics.PackReducedToSummary,
			StaleExcluded:       out.Metrics.PackStaleExcluded,
			FallbackReason:      out.Metrics.FallbackReason,
		})
	}
	stats := s.provisioner.CacheStats()
	report.CacheHits, report.CacheMisses = stats.Hits, stats.Misses
	return report, nil
}

// Knowledge reads shared task knowledge for an operator (P2-C §17).
//
// It applies the same lifecycle rules retrieval applies, through the same
// service call, so an operator told a decision is active is looking at exactly
// the judgement a Worker's pack made. An inspection that computed "active"
// its own way would be worse than no inspection at all: it would look like
// corroboration while being an independent guess.
//
// Asking for one task's knowledge widens the status filter to everything,
// because "what did we learn from this task" includes the decision a later
// task has since replaced — that is a fact ABOUT the task, and hiding it would
// make the answer wrong rather than tidy.
func (s *Service) Knowledge(
	ctx context.Context, req controllers.ProjectMemoryKnowledgeQuery,
) (controllers.ProjectMemoryKnowledgeResult, error) {
	var repoPath string
	if strings.TrimSpace(req.RepoPath) != "" {
		resolved, err := s.resolveRepo(ctx, req.ProjectID, req.RepoPath)
		if err != nil {
			return controllers.ProjectMemoryKnowledgeResult{}, err
		}
		repoPath = resolved
	}

	filter := pm.KnowledgeFilter{
		ProjectID: req.ProjectID, RepoPath: repoPath,
		TaskRef: strings.TrimSpace(req.TaskRef), Limit: req.Limit,
	}
	if req.Type != "" {
		filter.Types = []domain.ProjectMemoryType{req.Type}
	}
	switch {
	case req.Status != "":
		filter.Statuses = []domain.KnowledgeStatus{req.Status}
	case filter.TaskRef != "":
		filter.Statuses = []domain.KnowledgeStatus{
			domain.KnowledgeActive, domain.KnowledgeSuperseded, domain.KnowledgeResolved,
			domain.KnowledgeObsolete, domain.KnowledgeConflicting,
		}
	}

	entries, err := s.memory.Knowledge(ctx, filter)
	if err != nil {
		return controllers.ProjectMemoryKnowledgeResult{}, err
	}
	out := controllers.ProjectMemoryKnowledgeResult{
		Entries: make([]controllers.ProjectMemoryKnowledgeEntry, 0, len(entries)),
	}
	if repoPath != "" {
		out.RepoID = domain.ProjectMemoryRepoID(repoPath)
	}
	for _, e := range entries {
		out.Entries = append(out.Entries, knowledgeEntryResponse(e))
	}
	return out, nil
}

// Manifests reads what one execution was actually told (P2-C §16).
func (s *Service) Manifests(
	ctx context.Context, req controllers.ProjectMemoryManifestQuery,
) (controllers.ProjectMemoryManifestResult, error) {
	manifests, err := s.memory.ContextManifests(ctx, req.ProjectID, req.TaskRef, req.WorkflowRunID)
	if err != nil {
		return controllers.ProjectMemoryManifestResult{}, err
	}
	out := controllers.ProjectMemoryManifestResult{
		Entries: make([]controllers.ProjectMemoryManifestEntry, 0, len(manifests)),
	}
	for _, m := range manifests {
		entry := controllers.ProjectMemoryManifestEntry{Manifest: m}
		if req.Expand {
			items, missing, err := s.memory.ManifestItems(ctx, m)
			if err != nil {
				return controllers.ProjectMemoryManifestResult{}, err
			}
			entry.Missing = missing
			for _, it := range items {
				entry.Items = append(entry.Items, knowledgeEntryResponse(it))
			}
		}
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

func knowledgeEntryResponse(e pm.KnowledgeEntry) controllers.ProjectMemoryKnowledgeEntry {
	return controllers.ProjectMemoryKnowledgeEntry{
		Item: e.Item, Status: string(e.Status), Kind: string(e.Kind),
		Share: string(e.Share), Subject: e.Subject, SourceTask: e.SourceTask,
		SupersededBy: e.SupersededBy, Supersedes: e.Supersedes,
		ResolvedBy: e.ResolvedBy, ConflictsWith: e.ConflictsWith,
	}
}
