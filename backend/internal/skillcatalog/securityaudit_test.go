package skillcatalog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// securityAuditDir is the shipped example package. These tests load the real
// files rather than a fixture: a package that only validates in a test fixture
// is a package nobody can install.
const securityAuditDir = "packages/security-audit"

func TestSecurityAuditPackage_LoadsAndVerifies(t *testing.T) {
	pkg, err := LoadPackage(securityAuditDir)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	if pkg.Manifest.ID != "security-audit" {
		t.Fatalf("id = %q", pkg.Manifest.ID)
	}
	if pkg.Manifest.RiskLevel != RiskCritical {
		t.Fatalf("risk = %q; a package with an active-scan mode is critical", pkg.Manifest.RiskLevel)
	}
	wantModes := []string{
		"static-code", "dependencies", "secret-scan",
		"authz-review", "api-infra-review", "active-pentest",
	}
	for _, id := range wantModes {
		if _, ok := pkg.Manifest.Mode(id); !ok {
			t.Fatalf("mode %q is missing", id)
		}
	}
	// The output schema must be a real JSON document, not a placeholder.
	body, err := os.ReadFile(filepath.Join(securityAuditDir, filepath.FromSlash(pkg.Manifest.Outputs.SchemaRef)))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatalf("output schema is not valid JSON: %v", err)
	}
	for _, key := range []string{"$schema", "properties", "required"} {
		if _, ok := schema[key]; !ok {
			t.Fatalf("output schema is missing %q", key)
		}
	}
}

// The declared digest must match the files as committed. This is the test that
// fails when someone edits a mode guide and forgets to refresh the digest --
// which is exactly what an integrity field is for.
func TestSecurityAuditPackage_DigestMatchesTheCommittedFiles(t *testing.T) {
	digest, err := ComputePackageDigest(securityAuditDir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(securityAuditDir, ManifestFileName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(body), digest) {
		t.Fatalf("skill.yaml integrity.digest is stale; recompute it as %s", digest)
	}
}

// Every mode's guide must actually say the mode is bounded. A pentest guide
// that omits its preconditions is the failure mode this package exists to
// avoid.
func TestSecurityAuditPackage_ActivePentestGuideStatesItsPreconditions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(securityAuditDir, "modes", "active-pentest.md"))
	if err != nil {
		t.Fatalf("read guide: %v", err)
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	for _, required := range []string{
		"A named target",
		"Written authorization",
		"non-production target",
		"isolated runner with egress control",
		"No denial of service",
		"Do not exfiltrate data",
		"Stop immediately and report",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("active-pentest guide is missing %q", required)
		}
	}
}

// The reporting rule that keeps an audit report shareable.
func TestSecurityAuditPackage_ForbidsSecretsInReports(t *testing.T) {
	for _, rel := range []string{"SKILL.md", "modes/secret-scan.md"} {
		body, err := os.ReadFile(filepath.Join(securityAuditDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		text := strings.Join(strings.Fields(string(body)), " ")
		if !strings.Contains(text, "Never put a secret in the report") &&
			!strings.Contains(text, "Never write a secret into the report") {
			t.Fatalf("%s does not forbid secrets in reports", rel)
		}
	}
}

// The end-to-end shape the user asked for: install once, enable per project,
// and get a real answer per mode. The read-only mode plans today; the modes
// that need containment are refused with a reason, not silently run.
func TestSecurityAudit_InstallEnablePerProjectThenPlanPerMode(t *testing.T) {
	r := newRegistry(t)
	entry, err := r.Install(securityAuditDir, admin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if entry.Version != "0.1.0" {
		t.Fatalf("installed version = %q", entry.Version)
	}

	// One install, two projects, different grants.
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "security-audit", Version: entry.Version,
		GrantCapabilities:  []Capability{CapRepoRead, CapReportWrite, CapDepsRead, CapNetEgress},
		ApprovedBy:         admin,
		SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable medusa: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "poseidon", SkillID: "security-audit", Version: entry.Version,
		GrantCapabilities:  []Capability{CapRepoRead, CapReportWrite},
		ApprovedBy:         admin,
		SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable poseidon: %v", err)
	}
	// A project nobody enabled it on resolves to nothing.
	if _, err := r.Resolve("crm", "security-audit"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("crm resolved a skill nobody enabled: %v", err)
	}

	perms := []domain.Permission{domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage}

	// Read-only modes plan successfully on today's runner.
	for _, mode := range []string{"static-code", "secret-scan", "authz-review"} {
		plan, err := PlanRun(r, RunRequest{
			ProjectID: "medusa", SkillID: "security-audit", ModeID: mode,
			Inputs:             map[string]string{"mode": mode},
			SubjectPermissions: perms,
		}, NoRunner())
		if err != nil {
			t.Fatalf("PlanRun %s: %v", mode, err)
		}
		if !plan.Decision.Allowed() {
			t.Fatalf("%s was not allowed: %#v", mode, plan.Decision)
		}
	}

	// The dependency mode holds net.egress, so it is refused until a runner
	// can confine outbound traffic -- even though medusa granted it.
	_, err = PlanRun(r, RunRequest{
		ProjectID: "medusa", SkillID: "security-audit", ModeID: "dependencies",
		Inputs:             map[string]string{"mode": "dependencies"},
		SubjectPermissions: perms,
	}, NoRunner())
	if !errors.Is(err, ErrCapabilityDenied) || !strings.Contains(err.Error(), "isolated execution environment") {
		t.Fatalf("dependencies = %v, want a runner-isolation denial", err)
	}

	// Poseidon never granted net.egress, so the same mode is refused there for
	// a different, correctly-reported reason.
	_, err = PlanRun(r, RunRequest{
		ProjectID: "poseidon", SkillID: "security-audit", ModeID: "dependencies",
		Inputs:             map[string]string{"mode": "dependencies"},
		SubjectPermissions: perms,
	}, NoRunner())
	if !errors.Is(err, ErrCapabilityDenied) || !strings.Contains(err.Error(), "did not grant") {
		t.Fatalf("poseidon dependencies = %v, want a missing-grant denial", err)
	}

	// The active pentest is refused on every axis it should be.
	_, err = PlanRun(r, RunRequest{
		ProjectID: "medusa", SkillID: "security-audit", ModeID: "active-pentest",
		Inputs:             map[string]string{"mode": "active-pentest", "target": "staging.example.com:443"},
		SubjectPermissions: perms,
		AuthorizedTargets:  []string{"staging.example.com:443"},
	}, NoRunner())
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("active-pentest = %v, want ErrCapabilityDenied", err)
	}
	if !strings.Contains(err.Error(), string(CapNetActiveScan)) {
		t.Fatalf("denial does not name the active-scan capability: %v", err)
	}
}

// Even with a runner that fully attests containment, the active pentest still
// needs the project to have granted it and a human to have named the target.
// The runner removes one obstacle, not the approval.
func TestSecurityAudit_ActivePentestStillNeedsAGrantAndATarget(t *testing.T) {
	r := newRegistry(t)
	entry, err := r.Install(securityAuditDir, admin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "security-audit", Version: entry.Version,
		GrantCapabilities: []Capability{
			CapRepoRead, CapReportWrite, CapDepsRead, CapNetEgress, CapNetActiveScan,
		},
		ApprovedBy:         admin,
		SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	perms := []domain.Permission{domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage}
	req := RunRequest{
		ProjectID: "medusa", SkillID: "security-audit", ModeID: "active-pentest",
		Inputs:             map[string]string{"mode": "active-pentest"},
		SubjectPermissions: perms,
	}

	if _, err := PlanRun(r, req, isolatedRunner()); !errors.Is(err, ErrCapabilityDenied) ||
		!strings.Contains(err.Error(), string(DenyTargetNotAuthorized)) {
		t.Fatalf("unnamed target = %v, want a target-authorization denial", err)
	}

	req.AuthorizedTargets = []string{"staging.example.com:443"}
	plan, err := PlanRun(r, req, isolatedRunner())
	if err != nil {
		t.Fatalf("PlanRun with a named target: %v", err)
	}
	if plan.Decision.RequiredApproval != ApprovalPerTarget {
		t.Fatalf("required approval = %q, want per_target", plan.Decision.RequiredApproval)
	}
	if plan.Decision.EffectiveRisk != RiskCritical {
		t.Fatalf("effective risk = %q, want critical", plan.Decision.EffectiveRisk)
	}
}
