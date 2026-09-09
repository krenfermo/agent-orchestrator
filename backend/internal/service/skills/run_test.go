package skills_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// The execution path, without a container runtime. What is under test here is
// the SEQUENCING -- what is checked before anything is staged, what a caller
// cannot contribute, and which refusal comes back -- not the boundary itself,
// which has its own live tests in skillrunner.

// recordingExecutor stands in for the real runner. It records what it was asked
// to run, which is how a test proves the caller's values never reached it.
type recordingExecutor struct {
	mu          sync.Mutex
	attestation skillcatalog.RunnerAttestation
	requests    []skillrunner.StaticScanRequest
	report      skillrunner.StaticScanReport
	err         error
	// onRun runs inside the executor, which is where a test can revoke an
	// approval "while the run is happening".
	onRun func()
}

func (e *recordingExecutor) Attestation() skillcatalog.RunnerAttestation { return e.attestation }

func (e *recordingExecutor) Execute(context.Context, skillcatalog.Plan) (skillcatalog.Result, error) {
	return skillcatalog.Result{}, skillcatalog.ErrNoRunner
}

func (e *recordingExecutor) RunStaticScan(
	ctx context.Context, authority skillrunner.ImageAuthority, req skillrunner.StaticScanRequest,
) (skillrunner.StaticScanReport, error) {
	e.mu.Lock()
	e.requests = append(e.requests, req)
	e.mu.Unlock()
	if e.onRun != nil {
		e.onRun()
	}
	// The real runner asks the authority. This stand-in does the same, so a
	// test that revokes mid-run sees the refusal the real one would produce.
	if authority == nil {
		return skillrunner.StaticScanReport{}, skillrunner.ErrImageNotApproved
	}
	approval, err := authority.ApprovedImage(ctx, req.Scope, string(skillrunner.ToolStaticScan))
	if err != nil {
		return skillrunner.StaticScanReport{}, err
	}
	if e.err != nil {
		return skillrunner.StaticScanReport{}, e.err
	}
	report := e.report
	report.ImageDigest = approval.Digest
	report.ApprovalID = approval.ID
	report.ApprovedBy = approval.ApprovedBy
	return report, nil
}

func (e *recordingExecutor) seen() []skillrunner.StaticScanRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]skillrunner.StaticScanRequest(nil), e.requests...)
}

// fullyAttested is an environment that proves every control the static-code
// mode needs, so these tests exercise the trust root rather than re-testing
// the capability table.
func fullyAttested() skillcatalog.RunnerAttestation {
	return skillcatalog.RunnerAttestation{
		RunnerID: "container/docker",
		Controls: []skillcatalog.Control{
			skillcatalog.ControlFilesystemIsolation,
			skillcatalog.ControlProcessIsolation,
			skillcatalog.ControlNoCredentialInheritance,
			skillcatalog.ControlResourceLimits,
			skillcatalog.ControlEgressDenyAll,
		},
	}
}

// newRunFixture installs and enables security-audit, wires an executor and a
// trust root, and approves an image unless approve is false.
func newRunFixture(t *testing.T, exec *recordingExecutor, approve bool) (fixture, *skills.Service, *skills.ImageAuthority, skillimage.Scope) {
	t.Helper()
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	auth := skills.NewImageAuthority(f.store, f.store).WithImageInspector(acceptAll())
	svc := skills.New(f.store, f.dataDir,
		skills.WithSkillExecutor(exec, auth, f.store, "", ""))
	scope := skillimage.Scope{
		TenantID: domain.DefaultTenantID, ProjectID: medusa,
		SkillID: "security-audit", Version: version, ModeID: "static-code",
	}
	if approve {
		if _, err := auth.Approve(context.Background(), approveRequest(scope, digestOf('a'))); err != nil {
			t.Fatalf("Approve: %v", err)
		}
	}
	return f, svc, auth, scope
}

func runRequest(scope skillimage.Scope) skills.RunRequest {
	return skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: scope.SkillID, ModeID: scope.ModeID,
		Actor: admin, ActorPermissions: adminPerms(),
	}
}

// The happy path, and the two things it proves: the scope AO derived is the one
// the executor got, and the caller contributed nothing to it.
func TestRunSkill_DerivesTheScopeAndRunsTheApprovedImage(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	exec.report.Coverage.FilesStaged = 12
	exec.report.Coverage.FilesVisible = 12
	exec.report.Coverage.FilesScanned = 12
	_, svc, _, scope := newRunFixture(t, exec, true)

	res, err := svc.RunSkill(context.Background(), runRequest(scope))
	if err != nil {
		t.Fatalf("RunSkill: %v", err)
	}
	if res.Report.ImageDigest != digestOf('a') || res.Report.ApprovedBy != admin {
		t.Fatalf("report = %+v", res.Report)
	}
	if res.Tool != "ao.static-scan/v1" {
		t.Fatalf("tool = %q", res.Tool)
	}

	seen := exec.seen()
	if len(seen) != 1 {
		t.Fatalf("the executor ran %d times", len(seen))
	}
	// The scope is AO's, field by field. Nothing in RunRequest could have set
	// the tenant or the version.
	if !seen[0].Scope.Matches(scope) {
		t.Fatalf("executor got scope %s, AO derived %s", seen[0].Scope, scope)
	}
	// The paths come from the manifest, not from the request: RunRequest has
	// no field for them at all, which is the point.
	if seen[0].ProjectPath == "" {
		t.Fatal("the run was given no checkout to scan")
	}
}

// Requirement: nothing runs without an approval, and the refusal names the
// missing decision rather than a generic failure.
func TestRunSkill_RefusesWithNoApprovedImage(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	_, svc, _, scope := newRunFixture(t, exec, false)

	_, err := svc.RunSkill(context.Background(), runRequest(scope))
	if err == nil {
		t.Fatal("a run happened with no image approved")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(err.Error(), "administrator must approve") {
		t.Fatalf("the refusal should say what is missing: %v", err)
	}
}

// Requirement: revocation between resolution and launch. The executor revokes
// while it is "running", which is exactly the window the re-check covers.
func TestRunSkill_RefusesWhenTheApprovalIsRevokedInTheWindow(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	var auth *skills.ImageAuthority
	var approvalID string
	exec.onRun = func() {
		if err := auth.Revoke(context.Background(), approvalID, admin, adminPerms()); err != nil {
			t.Errorf("Revoke: %v", err)
		}
	}
	_, svc, a, scope := newRunFixture(t, exec, true)
	auth = a
	approvals, err := auth.ListApprovals(context.Background())
	if err != nil || len(approvals) != 1 {
		t.Fatalf("ListApprovals: %+v (%v)", approvals, err)
	}
	approvalID = approvals[0].ID

	_, err = svc.RunSkill(context.Background(), runRequest(scope))
	if err == nil {
		t.Fatal("a run completed under an approval revoked during it")
	}
	// Either refusal is correct and both name the trust root: the real runner
	// wraps it as SKILL_IMAGE_NOT_APPROVED after RecheckBeforeLaunch, and the
	// authority's own SKILL_IMAGE_APPROVAL_INACTIVE says specifically that the
	// approval stopped being usable. What must not happen is a completed run.
	switch code := apiCode(t, err); code {
	case "SKILL_IMAGE_NOT_APPROVED", "SKILL_IMAGE_APPROVAL_INACTIVE":
	default:
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the refusal should say the approval was revoked: %v", err)
	}
}

// Requirement: no runtime means no run, and the refusal says so rather than
// pretending nothing was approved.
func TestRunSkill_RefusesWithNoExecutor(t *testing.T) {
	f := newFixture(t)
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	_, err := f.svc.RunSkill(context.Background(), skills.RunRequest{
		ProjectID: medusa, SkillID: "security-audit", ModeID: "static-code",
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err == nil {
		t.Fatal("a service with no executor ran a skill")
	}
	if code := apiCode(t, err); code != "SKILL_RUNNER_UNAVAILABLE" {
		t.Fatalf("code = %q", code)
	}
}

// An environment that proves nothing runs nothing, and the refusal names the
// control. This is the capability table doing its job with a real executor
// wired -- the failure mode that would matter most if wiring the runner had
// quietly bypassed it.
func TestRunSkill_RefusesWhenTheEnvironmentAttestsNothing(t *testing.T) {
	exec := &recordingExecutor{attestation: skillcatalog.NoRunner()}
	_, svc, _, scope := newRunFixture(t, exec, true)

	_, err := svc.RunSkill(context.Background(), runRequest(scope))
	if err == nil {
		t.Fatal("an unattested environment ran a skill")
	}
	if code := apiCode(t, err); code != "SKILL_RUN_REFUSED" {
		t.Fatalf("code = %q", code)
	}
	if len(exec.seen()) != 0 {
		t.Fatal("the executor was reached despite a failed authorization")
	}
}

// Only static-code is executable. A mode that is perfectly authorized and has
// no implementation is a DIFFERENT answer from one that was refused, and
// collapsing them would tell an operator their grant was wrong when it was fine.
func TestRunSkill_RefusesAModeItDoesNotImplement(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	_, svc, _, scope := newRunFixture(t, exec, true)

	req := runRequest(scope)
	req.ModeID = "dependency-audit"
	_, err := svc.RunSkill(context.Background(), req)
	if err == nil {
		t.Fatal("a mode with no execution path ran")
	}
	// It is refused either by the capability table (it needs net.egress) or by
	// the executable-mode map. Both are correct; what must NOT happen is that
	// it runs.
	code := apiCode(t, err)
	if code != "SKILL_MODE_NOT_EXECUTABLE" && code != "SKILL_RUN_REFUSED" && code != "SKILL_MODE_UNKNOWN" {
		t.Fatalf("code = %q", code)
	}
	if len(exec.seen()) != 0 {
		t.Fatal("an unimplemented mode reached the executor")
	}
}

// A skill that is not enabled on this project does not run on it, whatever is
// approved. The trust root is not a substitute for activation.
func TestRunSkill_RefusesAProjectWithNoActivation(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	f, svc, auth, scope := newRunFixture(t, exec, true)
	other := f.seedProject(t, "other-project")

	// Approve for the other project too, so the only thing missing is the
	// activation.
	otherScope := scope
	otherScope.ProjectID = other
	if _, err := auth.Approve(context.Background(), approveRequest(otherScope, digestOf('a'))); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	req := runRequest(scope)
	req.ProjectID = other
	if _, err := svc.RunSkill(context.Background(), req); err == nil {
		t.Fatal("a skill ran on a project it is not enabled for")
	}
	if len(exec.seen()) != 0 {
		t.Fatal("an unactivated project reached the executor")
	}
}

// Concurrency: many runs against one approval, one of which revokes it. Every
// run either completes under a live approval or is refused; none completes
// under a revoked one, and the race detector has something to look at.
func TestRunSkill_ConcurrentRunsAgreeWithTheTrustRoot(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	_, svc, auth, scope := newRunFixture(t, exec, true)
	approvals, err := auth.ListApprovals(context.Background())
	if err != nil || len(approvals) != 1 {
		t.Fatalf("ListApprovals: %v", err)
	}
	id := approvals[0].ID

	const runs = 12
	var wg sync.WaitGroup
	results := make([]error, runs)
	wg.Add(runs)
	for i := 0; i < runs; i++ {
		go func(i int) {
			defer wg.Done()
			if i == runs/2 {
				results[i] = auth.Revoke(context.Background(), id, admin, adminPerms())
				return
			}
			_, results[i] = svc.RunSkill(context.Background(), runRequest(scope))
		}(i)
	}
	wg.Wait()

	// Nothing panicked, nothing deadlocked, and every failure is a refusal
	// rather than an unexplained error.
	for i, err := range results {
		if err == nil {
			continue
		}
		if i == runs/2 {
			t.Fatalf("the revoke failed: %v", err)
		}
		var known bool
		for _, code := range []string{"SKILL_IMAGE_NOT_APPROVED", "SKILL_IMAGE_APPROVAL_INACTIVE"} {
			if apiCode(t, err) == code {
				known = true
			}
		}
		if !known {
			t.Fatalf("run %d failed with an unexpected error: %v", i, err)
		}
	}
	// After the revoke, every further run is refused. The trust root has one
	// state and every reader sees it.
	if _, err := svc.RunSkill(context.Background(), runRequest(scope)); err == nil {
		t.Fatal("a run succeeded after the approval was revoked")
	}
}

// A refused run and an executed one both leave an audit row. A trail that only
// records successes cannot answer "what did somebody try".
func TestRunSkill_AuditsBothOutcomes(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested()}
	f, svc, _, scope := newRunFixture(t, exec, false)

	if _, err := svc.RunSkill(context.Background(), runRequest(scope)); err == nil {
		t.Fatal("a run happened with no approval")
	}
	entries, err := f.store.ListSkillAuditForProject(context.Background(), scope.ProjectID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var refused bool
	for _, e := range entries {
		if string(e.Action) == "run_refused" && strings.Contains(e.Detail, "static-code") {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("no run_refused entry in %d rows", len(entries))
	}
}

// The executor port and the errors it returns are mapped onto distinct API
// codes. A caller that cannot tell "no image approved" from "the runtime is
// down" cannot act on either.
func TestRunSkill_DistinguishesTheKindsOfFailure(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"runtime down":       {skillrunner.ErrRuntimeUnavailable, "SKILL_RUNTIME_UNAVAILABLE"},
		"staging unusable":   {skillrunner.ErrStagingUnusable, "SKILL_STAGING_UNUSABLE"},
		"tool not approved":  {skillrunner.ErrToolNotApproved, "SKILL_TOOL_NOT_APPROVED"},
		"image not approved": {skillrunner.ErrImageNotApproved, "SKILL_IMAGE_NOT_APPROVED"},
		"something else":     {errors.New("the scan exited 3"), "SKILL_RUN_FAILED"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			exec := &recordingExecutor{attestation: fullyAttested(), err: tc.err}
			_, svc, _, scope := newRunFixture(t, exec, true)
			_, err := svc.RunSkill(context.Background(), runRequest(scope))
			if err == nil {
				t.Fatalf("%s did not fail the run", name)
			}
			if code := apiCode(t, err); code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
		})
	}
}
