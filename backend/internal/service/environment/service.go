package environment

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

// AgentProber is the narrow surface this service needs from the agent
// inventory service: a fresh, bounded local probe for one harness id.
// *agentsvc.Service already satisfies this.
type AgentProber interface {
	Probe(ctx context.Context, agentID string) (agentsvc.ProbeResult, error)
}

// Harness ids this checkpoint's Settings surface reports on.
const (
	HarnessCodex  = "codex"
	HarnessClaude = "claude-code"
)

// CapabilityState is a coarse, UI-facing readiness bucket derived from real
// probe results — never invented.
type CapabilityState string

// The capability states. Ready and unavailable are observations; anything else
// records why the observation could not be made, so a caller never reads an
// unprobed capability as a working one.
const (
	CapabilityReady        CapabilityState = "ready"
	CapabilityUnavailable  CapabilityState = "unavailable"
	CapabilityAuthRequired CapabilityState = "auth_required"
	CapabilityUnknown      CapabilityState = "unknown"
)

// Overall readiness values.
const (
	ReadinessReady         = "ready"
	ReadinessSetupRequired = "setup_required"
)

// AgentCapability is the Settings-facing snapshot of one development agent
// (Codex or Claude Code).
type AgentCapability struct {
	ID         string                `json:"id"`
	Installed  bool                  `json:"installed"`
	BinaryPath string                `json:"binaryPath,omitempty"`
	Version    string                `json:"version,omitempty"`
	AuthState  ports.AgentAuthStatus `json:"authState" enum:"authorized,unauthorized,unknown"`
	// ConfiguredRoles lists the project roles (currently only "orchestrator" is
	// derivable cheaply from the project summary list) at least one registered
	// project has assigned to this harness. Empty does not mean "never used" —
	// it means no cheap signal was available.
	ConfiguredRoles []string `json:"configuredRoles,omitempty"`
	// Model is left empty: AO has no real signal for which model a CLI will
	// use without an expensive/authenticated call, and this surface only ever
	// reports facts backed by a probe.
	Model string `json:"model,omitempty"`
	// Source is always "unknown": AO cannot determine subscription vs.
	// API-key billing from a local probe.
	Source        string    `json:"source"`
	LastCheckedAt time.Time `json:"lastCheckedAt"`
}

// ProjectsSummary is the Settings-facing project registration count.
type ProjectsSummary struct {
	Count int `json:"count"`
}

// Readiness summarizes whether AO can currently start an autonomous workflow,
// using only today's requirements: a registered project plus at least one
// installed-and-authorized development agent. GitHub and future ReviewPolicy
// requirements are reported individually but do not gate Overall yet.
type Readiness struct {
	Codex    CapabilityState `json:"codex"`
	Claude   CapabilityState `json:"claude"`
	GitHub   CapabilityState `json:"github"`
	Projects CapabilityState `json:"projects"`
	// Headless is "ready" whenever this response was produced at all — the
	// daemon answering the request is itself the headless-readiness signal.
	Headless CapabilityState `json:"headless"`
	Overall  string          `json:"overall" enum:"ready,setup_required"`
}

// Status is the full Settings → Environment payload.
type Status struct {
	Codex     AgentCapability `json:"codex"`
	Claude    AgentCapability `json:"claude"`
	GitHub    GitHubStatus    `json:"github"`
	Projects  ProjectsSummary `json:"projects"`
	Readiness Readiness       `json:"readiness"`
}

// Deps carries the service's collaborators. Agents and Projects are
// required; Clock and GH are test seams.
type Deps struct {
	Agents   AgentProber
	Projects projectsvc.Manager
	Clock    func() time.Time
	gh       ghDeps
}

// Service aggregates real local probes into the Setup UX surface.
type Service struct {
	agents   AgentProber
	projects projectsvc.Manager
	clock    func() time.Time
	gh       ghDeps
}

// New returns a Service over the given agent prober and project manager.
func New(agents AgentProber, projects projectsvc.Manager) *Service {
	return NewWithDeps(Deps{Agents: agents, Projects: projects})
}

// NewWithDeps returns a Service with optional test seams.
func NewWithDeps(d Deps) *Service {
	clock := d.Clock
	if clock == nil {
		clock = time.Now
	}
	gh := d.gh
	if gh.LookPath == nil {
		gh = defaultGHDeps()
	}
	return &Service{agents: d.Agents, projects: d.Projects, clock: clock, gh: gh}
}

// Status runs every probe and returns the aggregated Setup UX snapshot.
func (s *Service) Status(ctx context.Context) (Status, error) {
	codex := s.agentCapability(ctx, HarnessCodex)
	claude := s.agentCapability(ctx, HarnessClaude)
	gh := probeGitHub(ctx, s.gh, s.clock)

	var projects []projectsvc.Summary
	if s.projects != nil {
		var err error
		projects, err = s.projects.List(ctx)
		if err != nil {
			return Status{}, err
		}
	}
	codex.ConfiguredRoles = configuredRoles(projects, domain.AgentHarness(codex.ID))
	claude.ConfiguredRoles = configuredRoles(projects, domain.AgentHarness(claude.ID))

	readiness := computeReadiness(codex, claude, gh, len(projects))
	return Status{
		Codex:     codex,
		Claude:    claude,
		GitHub:    gh,
		Projects:  ProjectsSummary{Count: len(projects)},
		Readiness: readiness,
	}, nil
}

// TestGitHub runs a fresh GitHub CLI probe, bypassing any cache, for the
// Settings "Test connection" action.
func (s *Service) TestGitHub(ctx context.Context) (GitHubStatus, error) {
	return probeGitHub(ctx, s.gh, s.clock), nil
}

func (s *Service) agentCapability(ctx context.Context, id string) AgentCapability {
	capability := AgentCapability{ID: id, Source: "unknown", AuthState: ports.AgentAuthStatusUnknown, LastCheckedAt: s.clock()}
	if s.agents == nil {
		return capability
	}
	res, err := s.agents.Probe(ctx, id)
	if err != nil {
		return capability
	}
	capability.Installed = res.Installed
	capability.BinaryPath = res.Agent.BinaryPath
	capability.Version = res.Agent.Version
	if res.Agent.AuthStatus != "" {
		capability.AuthState = res.Agent.AuthStatus
	}
	return capability
}

func configuredRoles(projects []projectsvc.Summary, harness domain.AgentHarness) []string {
	for _, p := range projects {
		if p.OrchestratorAgent == harness {
			return []string{"orchestrator"}
		}
	}
	return nil
}

func capabilityState(installed bool, auth ports.AgentAuthStatus) CapabilityState {
	if !installed {
		return CapabilityUnavailable
	}
	switch auth {
	case ports.AgentAuthStatusAuthorized:
		return CapabilityReady
	case ports.AgentAuthStatusUnauthorized:
		return CapabilityAuthRequired
	default:
		return CapabilityUnknown
	}
}

func githubCapabilityState(gh GitHubStatus) CapabilityState {
	if !gh.Installed {
		return CapabilityUnavailable
	}
	switch gh.AuthState {
	case GitHubAuthStateAuthenticated:
		return CapabilityReady
	case GitHubAuthStateUnauthenticated:
		return CapabilityAuthRequired
	default:
		return CapabilityUnknown
	}
}

func computeReadiness(codex, claude AgentCapability, gh GitHubStatus, projectCount int) Readiness {
	codexState := capabilityState(codex.Installed, codex.AuthState)
	claudeState := capabilityState(claude.Installed, claude.AuthState)
	ghState := githubCapabilityState(gh)

	projectsState := CapabilityUnavailable
	if projectCount > 0 {
		projectsState = CapabilityReady
	}

	overall := ReadinessSetupRequired
	if projectsState == CapabilityReady && (codexState == CapabilityReady || claudeState == CapabilityReady) {
		overall = ReadinessReady
	}

	return Readiness{
		Codex:    codexState,
		Claude:   claudeState,
		GitHub:   ghState,
		Projects: projectsState,
		Headless: CapabilityReady,
		Overall:  overall,
	}
}
