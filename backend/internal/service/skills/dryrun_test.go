package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// enabledFixture installs security-audit and enables it on medusa with the
// given grant.
func enabledFixture(t *testing.T, caps []skillcatalog.Capability) (fixture, domain.ProjectID) {
	t.Helper()
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: caps, Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	return f, medusa
}

func decisionFor(dr skills.DryRun, want skillcatalog.Capability) (skills.CapabilityDecision, bool) {
	for _, d := range dr.Decisions {
		if d.Capability == want {
			return d, true
		}
	}
	return skills.CapabilityDecision{}, false
}

// The read-only mode is the one AO can honestly answer "yes" to today.
func TestDryRun_ReadOnlyModeIsExecutable(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs:           map[string]string{"mode": "static-code"},
		ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunExecutable {
		t.Fatalf("verdict = %q, reasons = %v", dr.Verdict, dr.Reasons)
	}
	if dr.SkillID != "security-audit" || dr.Version == "" || dr.ModeName == "" {
		t.Fatalf("dry run = %#v", dr)
	}
	if len(dr.Decisions) != 2 {
		t.Fatalf("decisions = %#v", dr.Decisions)
	}
	for _, d := range dr.Decisions {
		if !d.Satisfied || d.Description == "" || d.RequiredPermission == "" {
			t.Fatalf("decision = %#v", d)
		}
	}
	if dr.Runner.NeedsIsolation || dr.Runner.NeedsEgressControl {
		t.Fatalf("a read-only mode should need no containment: %#v", dr.Runner)
	}
	if len(dr.MissingPermissions) != 0 || len(dr.Reasons) != 0 {
		t.Fatalf("executable run reported blockers: %#v %#v", dr.MissingPermissions, dr.Reasons)
	}
}

// The same skill, a different mode: granted by the project, and still blocked,
// because no runner attests the containment net.egress needs.
func TestDryRun_BlocksOnTheMissingRunnerEvenWithAFullGrant(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapDepsRead,
		skillcatalog.CapReportWrite, skillcatalog.CapNetEgress,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "dependencies",
		Inputs:           map[string]string{"mode": "dependencies"},
		ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q", dr.Verdict)
	}
	egress, ok := decisionFor(dr, skillcatalog.CapNetEgress)
	if !ok || egress.Satisfied {
		t.Fatalf("net.egress decision = %#v ok=%v", egress, ok)
	}
	if egress.DenialReason != skillcatalog.DenyNeedsIsolation {
		t.Fatalf("reason = %q, want needs_isolated_runner", egress.DenialReason)
	}
	if !dr.Runner.NeedsIsolation || !dr.Runner.NeedsEgressControl {
		t.Fatalf("runner requirements = %#v", dr.Runner)
	}
	if dr.Runner.Available || dr.Runner.Isolated || dr.Runner.EgressControlled {
		t.Fatalf("AO reported a runner it does not have: %#v", dr.Runner)
	}
	if dr.Runner.RunnerID != "none" {
		t.Fatalf("runner id = %q", dr.Runner.RunnerID)
	}
	// The capabilities that are fine report as satisfied, so a user can see
	// that net.egress is the one blocker rather than being told to grant
	// four things. The RUN is still refused -- that is what Verdict says.
	repo, _ := decisionFor(dr, skillcatalog.CapRepoRead)
	if !repo.Satisfied || repo.DenialReason != "" {
		t.Fatalf("repo.read should pass its own checks: %#v", repo)
	}
}

// A project that never granted a capability gets a different, correct reason
// from a project that granted it but has no runner.
func TestDryRun_DistinguishesAMissingGrantFromAMissingRunner(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "dependencies",
		Inputs:           map[string]string{"mode": "dependencies"},
		ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	egress, _ := decisionFor(dr, skillcatalog.CapNetEgress)
	if egress.DenialReason != skillcatalog.DenyNotGranted {
		t.Fatalf("reason = %q, want not_granted", egress.DenialReason)
	}
	if !strings.Contains(strings.Join(dr.Reasons, " "), "did not grant") {
		t.Fatalf("reasons = %v", dr.Reasons)
	}
}

// A caller who lacks the permission a capability is gated on is told which
// permission, by name.
func TestDryRun_ReportsMissingPermissionsByName(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"},
		// No permissions at all: the unauthenticated case.
		ActorPermissions: nil,
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q", dr.Verdict)
	}
	if len(dr.MissingPermissions) != 1 || dr.MissingPermissions[0] != domain.PermProjectRead {
		t.Fatalf("missing permissions = %#v", dr.MissingPermissions)
	}
}

// A per-target capability with no named target is "requires approval" only
// once a runner exists; today it is blocked, and the reason says which of the
// two is missing.
func TestDryRun_ActivePentestReportsEveryOutstandingRequirement(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
		skillcatalog.CapNetEgress, skillcatalog.CapNetActiveScan,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "active-pentest",
		Inputs:           map[string]string{"mode": "active-pentest"},
		ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q", dr.Verdict)
	}
	if dr.RequiredApproval != skillcatalog.ApprovalPerTarget {
		t.Fatalf("required approval = %q", dr.RequiredApproval)
	}
	scan, ok := decisionFor(dr, skillcatalog.CapNetActiveScan)
	if !ok || scan.Risk != skillcatalog.RiskCritical {
		t.Fatalf("active-scan decision = %#v ok=%v", scan, ok)
	}
	if scan.RequiredPermission != domain.PermSettingsManage {
		t.Fatalf("active-scan permission = %q", scan.RequiredPermission)
	}
}

// A dry run validates the declared inputs too, so a bad parameter is caught
// here rather than deferred to a run that cannot happen yet anyway.
func TestDryRun_ValidatesInputs(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs:           map[string]string{"mode": "nuclear"},
		ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q", dr.Verdict)
	}
	if !strings.Contains(strings.Join(dr.Reasons, " "), "must be one of") {
		t.Fatalf("reasons = %v", dr.Reasons)
	}
}

func TestDryRun_RefusesADisabledOrUnknownTarget(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	ctx := context.Background()

	if _, err := f.svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "nope",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("an unknown mode was planned")
	} else if code := apiCode(t, err); code != "SKILL_MODE_UNKNOWN" {
		t.Fatalf("code = %q", code)
	}

	if err := f.svc.Disable(ctx, medusa, "security-audit", admin); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := f.svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: adminPerms(),
	}); err == nil {
		t.Fatal("a disabled skill was planned")
	} else if code := apiCode(t, err); code != "SKILL_DISABLED" {
		t.Fatalf("code = %q", code)
	}
}

// A dry run must leave nothing behind: no process, no file, no row. It is the
// one execution-shaped surface that ships in this phase, so its lack of side
// effects is worth asserting rather than assuming.
func TestDryRun_HasNoSideEffects(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
	})
	ctx := context.Background()

	snapshot := func() (string, int, int) {
		t.Helper()
		var files []string
		root := skillcatalog.Dir(f.dataDir)
		if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() {
				files = append(files, p)
			}
			return nil
		}); err != nil {
			t.Fatalf("walk catalog: %v", err)
		}
		audit, err := f.svc.AuditForSkill(ctx, "security-audit")
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		acts, err := f.svc.ListForProject(ctx, medusa)
		if err != nil {
			t.Fatalf("activations: %v", err)
		}
		return strings.Join(files, "\n"), len(audit), len(acts)
	}

	beforeFiles, beforeAudit, beforeActs := snapshot()
	for _, mode := range []string{"static-code", "dependencies", "active-pentest", "secret-scan"} {
		if _, err := f.svc.DryRun(ctx, skills.DryRunRequest{
			ProjectID: medusa, SkillID: "security-audit", ModeID: mode,
			Inputs: map[string]string{"mode": mode}, ActorPermissions: adminPerms(),
		}); err != nil {
			t.Fatalf("DryRun %s: %v", mode, err)
		}
	}
	afterFiles, afterAudit, afterActs := snapshot()

	if beforeFiles != afterFiles {
		t.Fatalf("a dry run changed the catalog files")
	}
	if beforeAudit != afterAudit {
		t.Fatalf("a dry run wrote %d audit rows", afterAudit-beforeAudit)
	}
	if beforeActs != afterActs {
		t.Fatalf("a dry run changed the activations")
	}
}

// The dry-run surface takes no attestation from its caller. This is the
// tripwire for a future change that adds one: a self-declared Isolated:true is
// exactly the claim this design refuses to accept as proof.
func TestDryRun_TakesNoRunnerAttestationFromTheCaller(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite, skillcatalog.CapNetEgress,
	})
	dr, err := f.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "dependencies",
		Inputs: map[string]string{"mode": "dependencies"}, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Runner.Isolated || dr.Runner.EgressControlled || dr.Runner.Available {
		t.Fatalf("the dry run reported containment AO cannot provide: %#v", dr.Runner)
	}
}
