package contextrouter

import (
	"os"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The flag defaults to disabled. This is the guarantee the whole feature rests
// on: with nothing set, dispatch keeps its pre-existing full-context behavior.
func TestFlagDefaultsToDisabled(t *testing.T) {
	if _, set := os.LookupEnv(FlagEnv); set {
		t.Setenv(FlagEnv, "")
		os.Unsetenv(FlagEnv)
	}
	if Enabled() {
		t.Fatalf("%s is unset and routing reported itself enabled", FlagEnv)
	}
}

func TestFlagParsing(t *testing.T) {
	enabled := []string{"1", "true", "TRUE", "yes", "on", " enabled "}
	disabled := []string{"", " ", "0", "false", "no", "off", "maybe", "2"}
	for _, value := range enabled {
		t.Setenv(FlagEnv, value)
		if !Enabled() {
			t.Fatalf("%s=%q did not enable routing", FlagEnv, value)
		}
	}
	for _, value := range disabled {
		t.Setenv(FlagEnv, value)
		if Enabled() {
			t.Fatalf("%s=%q enabled routing; an unrecognised value must read as off", FlagEnv, value)
		}
	}
}

func TestRoleFromWorkflowRole(t *testing.T) {
	cases := map[domain.WorkflowRole]Role{
		domain.WorkflowRolePlanner:   RolePlanner,
		domain.WorkflowRoleWorker:    RoleWorker,
		domain.WorkflowRoleReviewer:  RoleReviewer,
		domain.WorkflowRoleFixWorker: RoleFix,
		domain.WorkflowRoleVerify:    RoleVerify,
	}
	for in, want := range cases {
		got, ok := RoleFromWorkflowRole(in)
		if !ok || got != want {
			t.Fatalf("RoleFromWorkflowRole(%q) = %q,%v; want %q,true", in, got, ok, want)
		}
		if !got.Valid() {
			t.Fatalf("mapped role %q is not budgeted", got)
		}
	}
	if _, ok := RoleFromWorkflowRole(domain.WorkflowRoleDecisionResolver); ok {
		t.Fatal("decision resolution routes no checkout context and must not map to a budgeted role")
	}
	if _, ok := RoleFromWorkflowRole("nonsense"); ok {
		t.Fatal("an unknown workflow role mapped to a budgeted role")
	}
}
