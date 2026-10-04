package controllers_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	providerprofilesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeWorkflowOwnership is a minimal WorkflowOwnershipStore fake scoped to
// a single fixed owner, enough to exercise workflowRunDetailView's
// profile-resolution path without a real store.
type fakeWorkflowOwnership struct{ owner domain.UserID }

func (f fakeWorkflowOwnership) GetWorkflowRunOwner(context.Context, string) (*domain.UserID, error) {
	return &f.owner, nil
}
func (f fakeWorkflowOwnership) SetWorkflowRunOwner(context.Context, string, domain.UserID) (bool, error) {
	return true, nil
}

// fakeProviderProfilesManager is a minimal providerprofilesvc.Manager fake
// returning a fixed profile list for List, unused elsewhere.
type fakeProviderProfilesManager struct {
	profiles []domain.ProviderProfile
}

func (f *fakeProviderProfilesManager) List(context.Context, domain.UserID) ([]domain.ProviderProfile, error) {
	return f.profiles, nil
}
func (f *fakeProviderProfilesManager) Create(context.Context, domain.UserID, providerprofilesvc.CreateInput) (domain.ProviderProfile, error) {
	return domain.ProviderProfile{}, nil
}
func (f *fakeProviderProfilesManager) Get(context.Context, domain.UserID, domain.ProviderProfileID) (domain.ProviderProfile, error) {
	return domain.ProviderProfile{}, nil
}
func (f *fakeProviderProfilesManager) Update(context.Context, domain.UserID, domain.ProviderProfileID, providerprofilesvc.UpdateInput) (domain.ProviderProfile, error) {
	return domain.ProviderProfile{}, nil
}
func (f *fakeProviderProfilesManager) Connect(context.Context, domain.UserID, domain.ProviderProfileID) (domain.ProviderProfile, error) {
	return domain.ProviderProfile{}, nil
}
func (f *fakeProviderProfilesManager) Disconnect(context.Context, domain.UserID, domain.ProviderProfileID) (domain.ProviderProfile, error) {
	return domain.ProviderProfile{}, nil
}
func (f *fakeProviderProfilesManager) Test(context.Context, domain.UserID, domain.ProviderProfileID) (providerprofilesvc.TestResult, error) {
	return providerprofilesvc.TestResult{}, nil
}
func (f *fakeProviderProfilesManager) Registry(context.Context) []domain.ProviderAdapterDescriptor {
	return nil
}

// TestWorkflowGetRun_SurfacesPersistedRoutingDecision is Checkpoint
// 8P-C.1's routing-observability proof: the run-detail response includes
// the routing decision already persisted at dispatch time (never
// recomputed for display), with safe profile display metadata resolved
// and NO secret/runtime-path fields anywhere in the response body.
func TestWorkflowGetRun_SurfacesPersistedRoutingDecision(t *testing.T) {
	owner := domain.UserID("user-a")
	claudeProfile := domain.ProviderProfile{
		ID: "prof-claude", UserID: owner, Provider: "anthropic", Harness: domain.HarnessClaudeCode,
		DisplayName: "My Claude", DefaultModel: "sonnet", Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated,
		SecretCiphertext: []byte("must-never-appear-in-response"),
	}
	codexProfile := domain.ProviderProfile{
		ID: "prof-codex", UserID: owner, Provider: "openai", Harness: domain.HarnessCodex,
		DisplayName: "My Codex", Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated,
	}
	decision := domain.RoutingDecision{
		Role: domain.WorkflowRoleWorker, Complexity: "normal",
		PreferredHarness: domain.HarnessClaudeCode, SelectedHarness: domain.HarnessCodex,
		PreferredProfileID: claudeProfile.ID, SelectedProfileID: codexProfile.ID,
		ReasonCodes:   []domain.RoutingReason{domain.RoutingReasonPreferredUnavailable, domain.RoutingReasonFallbackSelected},
		PolicyVersion: domain.UserExecutionPolicyVersion,
		CapacityStateByProfile: map[domain.ProviderProfileID]domain.CapacityState{
			claudeProfile.ID: domain.CapacityCooldown, codexProfile.ID: domain.CapacityAvailable,
		},
	}
	svc := &fakeWorkflowService{detail: workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning},
		Steps: []workflowcore.StepDetail{
			{Step: domain.WorkflowStep{ID: "wfs-1", Kind: domain.WorkflowStepWork, State: domain.WorkflowStepCompleted}, Routing: &decision},
		},
	}}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpd.NewRouterWithControl(config.Config{TrustedLocalMode: true}, log, nil, httpd.APIDeps{
		Workflows:         svc,
		WorkflowOwnership: fakeWorkflowOwnership{owner: owner},
		ProviderProfiles:  &fakeProviderProfilesManager{profiles: []domain.ProviderProfile{claudeProfile, codexProfile}},
	}, httpd.ControlDeps{})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	bodyBytes, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	body := string(bodyBytes)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, body)
	}

	// --- E2E D/E: the decision surfaced is the persisted one (Codex
	// selected as fallback from a Claude preference), not a recomputation. ---
	for _, want := range []string{
		`"role":"worker"`,
		`"preferredHarness":"claude-code"`,
		`"selectedHarness":"codex"`,
		`"fallbackUsed":true`,
		`"preferred_unavailable"`,
		`"fallback_selected"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q\nbody=%s", want, body)
		}
	}

	// --- §15: safe display metadata present ---
	for _, want := range []string{`"My Claude"`, `"My Codex"`, `"anthropic"`, `"openai"`, `"sonnet"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing safe display field %q\nbody=%s", want, body)
		}
	}

	// --- §14/§19: never a secret, credential, or runtime-home path ---
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"secretciphertext", "must-never-appear", "runtime-home", "/.ao/", "passwordhash"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("response leaked forbidden content %q\nbody=%s", forbidden, body)
		}
	}
}
