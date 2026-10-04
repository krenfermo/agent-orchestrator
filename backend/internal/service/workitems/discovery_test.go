package workitems_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/workitems"
)

// halfConfigured is the state a real project got stuck in: a base URL, a
// workspace and a stored token, no project mapping, and the connection off.
// Saving that much is deliberately allowed -- an incomplete configuration is a
// draft -- so it is a state the service must keep answering usefully from.
func halfConfigured(t *testing.T, f *fixture, id domain.ProjectID) {
	t.Helper()
	base, ws, token := "https://plane.example.test", "acme-dev", "plane-token"
	if _, err := f.svc.PutConfig(f.ctx, id, workitems.ConfigUpdate{
		BaseURL: &base, Workspace: &ws, APIToken: &token,
	}); err != nil {
		t.Fatalf("save a half-written configuration: %v", err)
	}
}

// The deadlock this closes. Listing the provider's projects is HOW somebody
// finds a project id, so gating it on already having one made the picker
// useless exactly when it was needed, and left a project that had never been
// mapped with no way to become mapped.
func TestProviderProjectsAreListableBeforeTheMappingExists(t *testing.T) {
	f := newFixture(t)
	halfConfigured(t, f, projectA)

	projects, err := f.svc.ListProviderProjects(f.ctx, projectA)
	if err != nil {
		t.Fatalf("ListProviderProjects on a workspace with a token: %v", err)
	}
	if len(projects) == 0 {
		t.Fatal("the picker got no projects to choose from, which is the deadlock")
	}
	if projects[0].ID == "" {
		t.Fatalf("projects = %+v, want ids somebody can map to", projects)
	}
}

// A preflight asks whether this token can see this workspace. That has to be
// answerable BEFORE the integration is switched on: a test you can only run
// after committing to the connection is not a test.
func TestConnectionIsTestableBeforeItIsSwitchedOn(t *testing.T) {
	f := newFixture(t)
	halfConfigured(t, f, projectA)

	identity, err := f.svc.TestConnection(f.ctx, projectA)
	if err != nil {
		t.Fatalf("TestConnection on a workspace with a token: %v", err)
	}
	if identity.Workspace == "" {
		t.Fatalf("identity = %+v, want the provider's answer", identity)
	}

	// And the result is recorded, so the settings page can say "Connected"
	// before anybody promises AO will write to the board.
	view, err := f.svc.Config(f.ctx, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Connected {
		t.Fatal("a successful preflight was not recorded on the configuration")
	}
	if view.Enabled {
		t.Fatal("testing a connection must not switch the integration on by itself")
	}
}

// The weaker gate is exactly two fields wide. Neither a missing workspace nor a
// missing token is discoverable-from, and both still answer with the sentinel
// rather than a provider call.
func TestDiscoveryStillRefusesWithoutAWorkspaceOrAToken(t *testing.T) {
	for _, tt := range []struct {
		name      string
		workspace string
		token     string
	}{
		{"nothing at all", "", ""},
		{"a workspace but no token", "acme-dev", ""},
		{"a token but no workspace", "", "plane-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{
				Workspace: &tt.workspace, APIToken: &tt.token,
			}); err != nil {
				t.Fatalf("save: %v", err)
			}

			if _, err := f.svc.ListProviderProjects(f.ctx, projectA); !errors.Is(err, workitems.ErrNotConfigured) {
				t.Fatalf("ListProviderProjects err = %v, want ErrNotConfigured", err)
			}
			// A sentinel the provider would return if it were ever reached.
			// Getting the refusal instead is the proof no call was made.
			f.provider.preflightErr = errPreflightReached

			_, err := f.svc.TestConnection(f.ctx, projectA)
			if err == nil {
				t.Fatal("TestConnection succeeded with nothing to reach the provider with")
			}
			if errors.Is(err, errPreflightReached) {
				t.Fatal("an unreachable configuration still called the provider")
			}
			// The controller maps this to PLANE_NOT_CONFIGURED; the message now
			// names what is actually missing rather than saying "no provider".
			if !strings.Contains(err.Error(), "workspace") || !strings.Contains(err.Error(), "token") {
				t.Fatalf("refusal = %q, want it to name the workspace and the token", err)
			}
		})
	}
}

// The weaker gate is for DISCOVERY only. Anything that can touch somebody's
// board still needs the full configuration, switched on.
func TestEverythingThatWritesStillNeedsTheFullConfiguration(t *testing.T) {
	f := newFixture(t)
	halfConfigured(t, f, projectA)

	if _, err := f.svc.Link(f.ctx, workitems.LinkRequest{
		ProjectID: projectA, Scope: domain.WorkItemScopeProject,
		Reference: "ACME-7", SyncEnabled: true, Actor: "ana@example.test",
	}); !isNotConfigured(err) {
		t.Fatalf("Link err = %v, want the not-configured refusal", err)
	}
	if len(f.provider.creates) != 0 {
		t.Fatal("a half-written configuration reached the provider's create path")
	}
	transitions, comments, _ := f.provider.snapshot()
	if len(transitions) != 0 || len(comments) != 0 {
		t.Fatalf("a half-written configuration moved %d states and posted %d comments", len(transitions), len(comments))
	}
}

// Enabling is still gated on completeness: the project id remains required, and
// the refusal names it. Discovery got easier; the promise did not.
func TestEnablingStillRequiresTheProjectMapping(t *testing.T) {
	f := newFixture(t)
	halfConfigured(t, f, projectA)

	on := true
	_, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{Enabled: &on})
	if err == nil {
		t.Fatal("a configuration with no project mapping was switched on")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("refusal = %q, want it to name the missing project", err)
	}

	// With the mapping, it goes on -- which is the whole path the UI could not
	// walk before: workspace, project id and token, then enable, then use.
	projectRef := "40a788c8-505b-4144-a05b-a00d04455eea"
	if _, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{
		ExternalProjectID: &projectRef,
	}); err != nil {
		t.Fatalf("store the project mapping: %v", err)
	}
	view, err := f.svc.PutConfig(f.ctx, projectA, workitems.ConfigUpdate{Enabled: &on})
	if err != nil {
		t.Fatalf("enable a complete configuration: %v", err)
	}
	if !view.Enabled || view.ExternalProjectID != projectRef {
		t.Fatalf("view = %+v, want it enabled and mapped to the stated project", view)
	}
}

var errPreflightReached = errors.New("the provider's preflight was reached")

func isNotConfigured(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, workitems.ErrNotConfigured) {
		return true
	}
	var wErr *ports.WorkItemsError
	if errors.As(err, &wErr) && wErr.Kind == ports.WorkItemsErrNotConfigured {
		return true
	}
	return strings.Contains(err.Error(), "no work-management provider")
}
