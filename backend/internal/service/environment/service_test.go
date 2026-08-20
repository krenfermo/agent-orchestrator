package environment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

type fakeAgentProber struct {
	results map[string]agentsvc.ProbeResult
	errs    map[string]error
}

func (f *fakeAgentProber) Probe(_ context.Context, agentID string) (agentsvc.ProbeResult, error) {
	if err, ok := f.errs[agentID]; ok {
		return agentsvc.ProbeResult{}, err
	}
	return f.results[agentID], nil
}

type fakeProjects struct {
	summaries []projectsvc.Summary
	err       error
}

func (f *fakeProjects) List(context.Context) ([]projectsvc.Summary, error) { return f.summaries, f.err }
func (f *fakeProjects) Get(context.Context, domain.ProjectID) (projectsvc.GetResult, error) {
	return projectsvc.GetResult{}, errors.New("not implemented")
}
func (f *fakeProjects) Add(context.Context, projectsvc.AddInput) (projectsvc.Project, error) {
	return projectsvc.Project{}, errors.New("not implemented")
}
func (f *fakeProjects) InitializeRepository(context.Context, projectsvc.InitializeRepositoryInput) (projectsvc.InitializeRepositoryResult, error) {
	return projectsvc.InitializeRepositoryResult{}, errors.New("not implemented")
}
func (f *fakeProjects) UpdateSettings(context.Context, domain.ProjectID, projectsvc.UpdateSettingsInput) (projectsvc.Project, error) {
	return projectsvc.Project{}, errors.New("not implemented")
}
func (f *fakeProjects) SetConfig(context.Context, domain.ProjectID, projectsvc.SetConfigInput) (projectsvc.Project, error) {
	return projectsvc.Project{}, errors.New("not implemented")
}
func (f *fakeProjects) Remove(context.Context, domain.ProjectID) (projectsvc.RemoveResult, error) {
	return projectsvc.RemoveResult{}, errors.New("not implemented")
}
func (f *fakeProjects) ListAllowedRootEntries(context.Context, string) (projectsvc.BrowseResult, error) {
	return projectsvc.BrowseResult{}, errors.New("not implemented")
}
func (f *fakeProjects) CloneFromGitHub(context.Context, projectsvc.CloneInput) (projectsvc.Project, error) {
	return projectsvc.Project{}, errors.New("not implemented")
}
func (f *fakeProjects) TestRepoConnection(context.Context, domain.ProjectID, string) (projectsvc.ConnectionTestResult, error) {
	return projectsvc.ConnectionTestResult{}, errors.New("not implemented")
}
func (f *fakeProjects) RefreshWorkspaceRepos(context.Context, domain.ProjectID) (projectsvc.Project, error) {
	return projectsvc.Project{}, errors.New("not implemented")
}

var _ projectsvc.Manager = (*fakeProjects)(nil)

func newTestService(t *testing.T, agents *fakeAgentProber, projects *fakeProjects) *Service {
	t.Helper()
	return NewWithDeps(Deps{
		Agents:   agents,
		Projects: projects,
		Clock:    func() time.Time { return time.Unix(0, 0) },
		gh: ghDeps{
			LookPath: func(string) (string, error) { return "", errors.New("not found") },
			CombinedOutput: func(context.Context, string, ...string) ([]byte, error) {
				return nil, errors.New("not found")
			},
		},
	})
}

func TestStatusCodexInstalledAuthorized(t *testing.T) {
	agents := &fakeAgentProber{results: map[string]agentsvc.ProbeResult{
		HarnessCodex: {
			Installed: true,
			Agent:     agentsvc.Info{ID: HarnessCodex, BinaryPath: "/usr/local/bin/codex", Version: "codex-cli 1.0.0", AuthStatus: ports.AgentAuthStatusAuthorized},
		},
	}}
	projects := &fakeProjects{summaries: []projectsvc.Summary{{ID: "p1", OrchestratorAgent: domain.AgentHarness(HarnessCodex)}}}
	svc := newTestService(t, agents, projects)

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !status.Codex.Installed || status.Codex.AuthState != ports.AgentAuthStatusAuthorized {
		t.Fatalf("Codex = %+v", status.Codex)
	}
	if status.Codex.BinaryPath == "" || status.Codex.Version == "" {
		t.Errorf("Codex missing binary path/version: %+v", status.Codex)
	}
	if len(status.Codex.ConfiguredRoles) == 0 || status.Codex.ConfiguredRoles[0] != "orchestrator" {
		t.Errorf("Codex.ConfiguredRoles = %v, want [orchestrator]", status.Codex.ConfiguredRoles)
	}
	if status.Readiness.Codex != CapabilityReady {
		t.Errorf("Readiness.Codex = %q, want ready", status.Readiness.Codex)
	}
	if status.Readiness.Overall != ReadinessReady {
		t.Errorf("Readiness.Overall = %q, want ready", status.Readiness.Overall)
	}
}

func TestStatusCodexMissing(t *testing.T) {
	agents := &fakeAgentProber{results: map[string]agentsvc.ProbeResult{}}
	projects := &fakeProjects{}
	svc := newTestService(t, agents, projects)

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Codex.Installed {
		t.Errorf("Codex.Installed = true, want false")
	}
	if status.Readiness.Codex != CapabilityUnavailable {
		t.Errorf("Readiness.Codex = %q, want unavailable", status.Readiness.Codex)
	}
	if status.Readiness.Overall != ReadinessSetupRequired {
		t.Errorf("Readiness.Overall = %q, want setup_required", status.Readiness.Overall)
	}
}

func TestStatusClaudeInstalledUnauthenticated(t *testing.T) {
	agents := &fakeAgentProber{results: map[string]agentsvc.ProbeResult{
		HarnessClaude: {
			Installed: true,
			Agent:     agentsvc.Info{ID: HarnessClaude, BinaryPath: "/usr/local/bin/claude", AuthStatus: ports.AgentAuthStatusUnauthorized},
		},
	}}
	svc := newTestService(t, agents, &fakeProjects{})

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Readiness.Claude != CapabilityAuthRequired {
		t.Errorf("Readiness.Claude = %q, want auth_required", status.Readiness.Claude)
	}
}

// TestUnknownAuthIsNotAuthenticated guards the checkpoint's explicit
// requirement: an "unknown" local probe result must never be presented as
// "authenticated"/"ready".
func TestUnknownAuthIsNotAuthenticated(t *testing.T) {
	agents := &fakeAgentProber{results: map[string]agentsvc.ProbeResult{
		HarnessCodex: {
			Installed: true,
			Agent:     agentsvc.Info{ID: HarnessCodex, BinaryPath: "/usr/local/bin/codex", AuthStatus: ports.AgentAuthStatusUnknown},
		},
	}}
	projects := &fakeProjects{summaries: []projectsvc.Summary{{ID: "p1"}}}
	svc := newTestService(t, agents, projects)

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Codex.AuthState == ports.AgentAuthStatusAuthorized {
		t.Fatal("unknown auth status must not be reported as authorized")
	}
	if status.Readiness.Codex == CapabilityReady {
		t.Fatal("unknown auth status must not be reported as ready")
	}
	if status.Readiness.Overall == ReadinessReady {
		t.Fatal("unknown auth status alone must not satisfy overall readiness")
	}
}

func TestStatusProbeErrorDegradesToUnknown(t *testing.T) {
	agents := &fakeAgentProber{errs: map[string]error{HarnessCodex: errors.New("boom")}}
	svc := newTestService(t, agents, &fakeProjects{})

	status, err := svc.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Codex.Installed {
		t.Errorf("Codex.Installed = true, want false on probe error")
	}
	if status.Codex.AuthState != ports.AgentAuthStatusUnknown {
		t.Errorf("Codex.AuthState = %q, want unknown", status.Codex.AuthState)
	}
}
