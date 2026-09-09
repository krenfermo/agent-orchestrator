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

// With no runner wired, even the read-only mode is refused: reading a
// checkout has no AO-enforced boundary outside the container, so it is not
// exempt from confinement.
func TestDryRun_ReadOnlyModeNeedsConfinementToo(t *testing.T) {
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
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q, reasons = %v", dr.Verdict, dr.Reasons)
	}
	if dr.SkillID != "security-audit" || dr.Version == "" || dr.ModeName == "" {
		t.Fatalf("dry run = %#v", dr)
	}
	if len(dr.Decisions) != 2 {
		t.Fatalf("decisions = %#v", dr.Decisions)
	}
	// It needs containment, and only containment -- no control beyond the five
	// a container provides.
	if !dr.Runner.NeedsIsolation {
		t.Fatalf("a read mode must need confinement: %#v", dr.Runner)
	}
	if dr.Runner.NeedsEgressControl {
		t.Fatalf("a read mode must not need an egress allowlist: %#v", dr.Runner)
	}
	repo, ok := decisionFor(dr, skillcatalog.CapRepoRead)
	if !ok || repo.MissingControl != skillcatalog.ControlFilesystemIsolation {
		t.Fatalf("repo.read = %#v", repo)
	}
	// report.write genuinely needs nothing: it is AO storing a document on
	// AO's side of the boundary.
	report, ok := decisionFor(dr, skillcatalog.CapReportWrite)
	if !ok || !report.Satisfied {
		t.Fatalf("report.write = %#v", report)
	}
}

// The same skill, a different mode: granted by the project, carried by a
// confining runner, and STILL blocked — because the control it is missing is
// the egress allowlist, which confinement does not provide.
func TestDryRun_BlocksOnTheMissingEgressAllowlistEvenWithAFullGrant(t *testing.T) {
	f, medusa := enabledFixture(t, []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapDepsRead,
		skillcatalog.CapReportWrite, skillcatalog.CapNetEgress,
	})
	svc := skills.New(f.store, f.dataDir, skills.WithRunner(fixedRunner{
		skillcatalog.RunnerAttestation{RunnerID: "container/docker", Controls: []skillcatalog.Control{
			skillcatalog.ControlFilesystemIsolation, skillcatalog.ControlProcessIsolation,
			skillcatalog.ControlNoCredentialInheritance, skillcatalog.ControlResourceLimits,
			skillcatalog.ControlEgressDenyAll,
		}},
	}, ""))
	dr, err := svc.DryRun(context.Background(), skills.DryRunRequest{
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
	if egress.DenialReason != skillcatalog.DenyMissingControl {
		t.Fatalf("reason = %q, want missing_control", egress.DenialReason)
	}
	if egress.MissingControl != skillcatalog.ControlEgressAllowlist {
		t.Fatalf("missing control = %q, want egress_allowlist", egress.MissingControl)
	}
	if len(egress.RequiresControls) == 0 {
		t.Fatalf("the decision must carry the full requirement: %#v", egress)
	}
	if !dr.Runner.NeedsIsolation || !dr.Runner.NeedsEgressControl {
		t.Fatalf("runner requirements = %#v", dr.Runner)
	}
	// The missing-control list is exactly the one thing that has to be built.
	if len(dr.Runner.MissingControls) != 1 ||
		dr.Runner.MissingControls[0] != skillcatalog.ControlEgressAllowlist {
		t.Fatalf("missing controls = %v", dr.Runner.MissingControls)
	}
	// Confinement is reported honestly and is NOT mistaken for an allowlist.
	if !dr.Runner.Isolated || dr.Runner.EgressControlled {
		t.Fatalf("confinement without an allowlist was misreported: %#v", dr.Runner)
	}
	if dr.Runner.RunnerID != "container/docker" {
		t.Fatalf("runner id = %q", dr.Runner.RunnerID)
	}
	// The capabilities that are fine report as satisfied, so a user can see
	// that net.egress is the one blocker rather than being told to grant
	// four things. The RUN is still refused -- that is what Verdict says.
	repo, _ := decisionFor(dr, skillcatalog.CapRepoRead)
	if !repo.Satisfied || repo.DenialReason != "" {
		t.Fatalf("repo.read should pass its own checks under confinement: %#v", repo)
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
	if len(dr.Runner.Controls) != 0 {
		t.Fatalf("the default service attested %v", dr.Runner.Controls)
	}
}

// Phase 3: a service wired to a REAL confining runner reports that
// environment truthfully -- and still refuses every blocked capability,
// because confinement is not the control any of them is missing.
func TestDryRun_ReportsAConfiningRunnerAndStillBlocks(t *testing.T) {
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	confined := skillcatalog.RunnerAttestation{
		RunnerID: "container/docker",
		Controls: []skillcatalog.Control{
			skillcatalog.ControlFilesystemIsolation,
			skillcatalog.ControlProcessIsolation,
			skillcatalog.ControlNoCredentialInheritance,
			skillcatalog.ControlResourceLimits,
			skillcatalog.ControlEgressDenyAll,
		},
	}
	svc := skills.New(f.store, f.dataDir, skills.WithRunner(fixedRunner{confined}, ""))
	ctx := context.Background()
	if _, err := svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{
			skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
			skillcatalog.CapDepsRead, skillcatalog.CapNetEgress,
		},
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// The read-only mode is executable, and now says so against a real runner.
	static, err := svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if static.Verdict != skills.DryRunExecutable {
		t.Fatalf("static-code = %q, reasons %v", static.Verdict, static.Reasons)
	}
	if static.Runner.RunnerID != "container/docker" || !static.Runner.Available {
		t.Fatalf("the dry run did not report the real runner: %#v", static.Runner)
	}

	// The dependency mode is still blocked, and the reason has moved from "no
	// runner at all" to the one control this runner does not implement.
	deps, err := svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "dependencies",
		Inputs: map[string]string{"mode": "dependencies"}, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if deps.Verdict != skills.DryRunBlocked {
		t.Fatalf("dependencies = %q", deps.Verdict)
	}
	egress, ok := decisionFor(deps, skillcatalog.CapNetEgress)
	if !ok || egress.MissingControl != skillcatalog.ControlEgressAllowlist {
		t.Fatalf("net.egress denial = %#v", egress)
	}
	if deps.Runner.EgressControlled {
		t.Fatalf("deny-all was reported as an egress allowlist: %#v", deps.Runner)
	}
}

// fixedRunner is a runner that attests a fixed set and executes nothing. It
// stands in for internal/skillrunner so this package's tests need no container.
type fixedRunner struct {
	att skillcatalog.RunnerAttestation
}

func (f fixedRunner) Attestation() skillcatalog.RunnerAttestation { return f.att }

func (fixedRunner) Execute(context.Context, skillcatalog.Plan) (skillcatalog.Result, error) {
	return skillcatalog.Result{}, skillcatalog.ErrNoRunner
}

// Phase 4: with the real confining runner, the read modes become executable
// and everything else stays blocked — per project, so one project's grant
// cannot make another's run possible.
func TestDryRun_ProjectIsolationHoldsUnderAConfiningRunner(t *testing.T) {
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	poseidon := f.seedProject(t, "poseidon")
	confined := fixedRunner{skillcatalog.RunnerAttestation{
		RunnerID: "container/docker",
		Controls: []skillcatalog.Control{
			skillcatalog.ControlFilesystemIsolation,
			skillcatalog.ControlProcessIsolation,
			skillcatalog.ControlNoCredentialInheritance,
			skillcatalog.ControlResourceLimits,
			skillcatalog.ControlEgressDenyAll,
		},
	}}
	svc := skills.New(f.store, f.dataDir, skills.WithRunner(confined, ""))
	ctx := context.Background()

	// Only medusa is granted the read capabilities.
	if _, err := svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{
			skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
		},
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable medusa: %v", err)
	}
	// Poseidon enables the same skill but grants only the report capability.
	if _, err := svc.Enable(ctx, skills.EnableRequest{
		ProjectID: poseidon, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable poseidon: %v", err)
	}

	medusaRun, err := svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: adminPerms(),
	})
	if err != nil || medusaRun.Verdict != skills.DryRunExecutable {
		t.Fatalf("medusa static-code = %q %v", medusaRun.Verdict, err)
	}

	// Same skill, same version, same runner, different project: refused,
	// because the grant is per project and poseidon's omits repo.read.
	poseidonRun, err := svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: poseidon, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("poseidon DryRun: %v", err)
	}
	if poseidonRun.Verdict != skills.DryRunBlocked {
		t.Fatalf("poseidon static-code = %q, reasons %v", poseidonRun.Verdict, poseidonRun.Reasons)
	}
	repo, ok := decisionFor(poseidonRun, skillcatalog.CapRepoRead)
	if !ok || repo.DenialReason != skillcatalog.DenyNotGranted {
		t.Fatalf("poseidon repo.read = %#v", repo)
	}
}

// A caller with too few permissions is refused even on a project that granted
// the capability and with a runner that carries it. Capability authorization,
// user authorization and runtime enforcement are three separate gates and none
// substitutes for another.
func TestDryRun_InsufficientUserAuthorizationStillRefuses(t *testing.T) {
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	confined := fixedRunner{skillcatalog.RunnerAttestation{
		RunnerID: "container/docker", Controls: []skillcatalog.Control{
			skillcatalog.ControlFilesystemIsolation,
			skillcatalog.ControlProcessIsolation,
			skillcatalog.ControlNoCredentialInheritance,
			skillcatalog.ControlResourceLimits,
			skillcatalog.ControlEgressDenyAll,
		},
	}}
	svc := skills.New(f.store, f.dataDir, skills.WithRunner(confined, ""))
	ctx := context.Background()
	if _, err := svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{
			skillcatalog.CapRepoRead, skillcatalog.CapReportWrite,
		},
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// The project granted it and the runner carries it; the CALLER holds
	// nothing, so the run is refused on the third gate.
	dr, err := svc.DryRun(ctx, skills.DryRunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Inputs: map[string]string{"mode": "static-code"}, ActorPermissions: nil,
	})
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dr.Verdict != skills.DryRunBlocked {
		t.Fatalf("verdict = %q", dr.Verdict)
	}
	if len(dr.MissingPermissions) == 0 {
		t.Fatalf("the refusal did not name a missing permission: %#v", dr)
	}
	// And the reason is the permission, not the runner: the runner is fine.
	if len(dr.Runner.MissingControls) != 0 {
		t.Fatalf("the runner was blamed for a permission problem: %#v", dr.Runner)
	}
}
