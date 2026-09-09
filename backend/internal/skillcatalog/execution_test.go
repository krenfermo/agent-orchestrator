package skillcatalog

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func enabledRegistry(t *testing.T, caps []Capability) *Registry {
	t.Helper()
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "example-audit", Version: "1.2.3",
		GrantCapabilities: caps, ApprovedBy: admin, SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return r
}

func TestPlanRun_BuildsAnAuthorizedPlanWithoutRunningAnything(t *testing.T) {
	r := enabledRegistry(t, []Capability{CapRepoRead, CapReportWrite})
	plan, err := PlanRun(r, RunRequest{
		ProjectID:          "medusa",
		SkillID:            "example-audit",
		ModeID:             "quick",
		Inputs:             map[string]string{"mode": "deep"},
		RequestedBy:        admin,
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
	}, NoRunner())
	if err != nil {
		t.Fatalf("PlanRun: %v", err)
	}
	if plan.Mode.ID != "quick" || !plan.Decision.Allowed() {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.Inputs["mode"] != "deep" {
		t.Fatalf("inputs = %#v", plan.Inputs)
	}
}

func TestPlanRun_FailsClosedWhenTheGrantIsMissing(t *testing.T) {
	r := enabledRegistry(t, []Capability{CapRepoRead})
	_, err := PlanRun(r, RunRequest{
		ProjectID: "medusa", SkillID: "example-audit", ModeID: "quick",
		Inputs:             map[string]string{"mode": "quick"},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
	}, NoRunner())
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("err = %v, want ErrCapabilityDenied", err)
	}
}

func TestPlanRun_ValidatesInputsAgainstTheManifest(t *testing.T) {
	r := enabledRegistry(t, []Capability{CapRepoRead, CapReportWrite})
	perms := []domain.Permission{domain.PermProjectRead}
	cases := []struct {
		name    string
		inputs  map[string]string
		wantSub string
	}{
		{"missing required", map[string]string{}, "is required"},
		{"unknown input", map[string]string{"mode": "quick", "sneaky": "1"}, "is not declared"},
		{"enum out of range", map[string]string{"mode": "nuclear"}, "must be one of"},
		{"blank required", map[string]string{"mode": "   "}, "is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PlanRun(r, RunRequest{
				ProjectID: "medusa", SkillID: "example-audit", ModeID: "quick",
				Inputs: tc.inputs, SubjectPermissions: perms,
			}, NoRunner())
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

func TestPlanRun_RefusesADisabledSkill(t *testing.T) {
	r := enabledRegistry(t, []Capability{CapRepoRead, CapReportWrite})
	if err := r.Disable("medusa", "example-audit"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	_, err := PlanRun(r, RunRequest{
		ProjectID: "medusa", SkillID: "example-audit", ModeID: "quick",
		Inputs:             map[string]string{"mode": "quick"},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
	}, NoRunner())
	if !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("err = %v, want ErrNotEnabled", err)
	}
}

// The only runner this phase ships refuses everything. The alternative -- a
// runner that executes in the daemon's own process while the catalog reports
// it as contained -- would make every check above cosmetic.
func TestUnavailableRunner_RefusesEveryPlan(t *testing.T) {
	r := enabledRegistry(t, []Capability{CapRepoRead, CapReportWrite})
	plan, err := PlanRun(r, RunRequest{
		ProjectID: "medusa", SkillID: "example-audit", ModeID: "quick",
		Inputs:             map[string]string{"mode": "quick"},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
	}, NoRunner())
	if err != nil {
		t.Fatalf("PlanRun: %v", err)
	}
	if _, err := (UnavailableRunner{}).Execute(context.Background(), plan); !errors.Is(err, ErrNoRunner) {
		t.Fatalf("Execute = %v, want ErrNoRunner", err)
	}
	if att := (UnavailableRunner{}).Attestation(); att.Isolated() || att.EgressControlled() || len(att.Controls) != 0 {
		t.Fatalf("the shipped runner attests containment: %#v", att)
	}
}
