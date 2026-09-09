package skills_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// run_live_test.go -- the whole path against a REAL container: an activation, an
// administrator's approval of the digest actually on this host, and a scan that
// happens inside the boundary.
//
// Nothing external is contacted. The image is one already on the host (these
// tests never pull), the project is synthetic, and the container has no network.

func liveRunner(t *testing.T) *skillrunner.Runner {
	t.Helper()
	if testing.Short() {
		t.Skip("live container tests are skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := skillrunner.New(ctx)
	if !r.Available() {
		t.Skipf("no container runtime on this host: %s", r.Unavailable())
	}
	return r
}

// hostImageDigest is the digest of the base image actually present. An
// administrator approves THAT: approving something else would test nothing,
// because the point is that AO runs the bytes somebody named.
func hostImageDigest(t *testing.T, r *skillrunner.Runner) string {
	t.Helper()
	out, err := exec.Command(r.Runtime().Binary, "image", "inspect", "alpine:3.19",
		"--format", "{{.Id}}").Output()
	if err != nil {
		t.Skipf("alpine:3.19 is not present locally, and these tests do not pull: %v", err)
	}
	digest, parseErr := skillimage.ParseDigest(strings.TrimSpace(string(out)))
	if parseErr != nil {
		t.Skipf("this host reports %q for alpine:3.19: %v", out, parseErr)
	}
	return digest
}

// liveProject writes a synthetic checkout beside the repository, which is the
// one path the container runtime here is known to share.
func liveProject(t *testing.T) string {
	t.Helper()
	base, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create testdata: %v", err)
	}
	dir, err := os.MkdirTemp(base, "liverun-")
	if err != nil {
		t.Fatalf("stage project: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	files := map[string]string{
		"api/handler.go": "package api\n\nfunc H() { db.Query(\"SELECT * FROM t WHERE id = \" + id) }\n",
		"api/config.go":  "package api\n\nconst token = \"ghp_0123456789abcdefghijklmnopqrstuvwxyzA\"\n",
		"README.md":      "# synthetic\n",
	}
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

// liveFixture installs, enables, wires the real runner and approves the host's
// own image for the exact scope.
func liveFixture(t *testing.T, r *skillrunner.Runner, approve bool) (fixture, *skills.Service, *skills.ImageAuthority, skillimage.Scope) {
	t.Helper()
	f := newFixture(t)
	version := mustInstall(t, f)
	project := liveProject(t)
	id := domain.ProjectID("medusa-live")
	if err := f.store.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: string(id), Path: project, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: id, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	auth := skills.NewImageAuthority(f.store, f.store)
	// Staging goes beside the project, inside the shared tree.
	svc := skills.New(f.store, f.dataDir,
		skills.WithSkillExecutor(r, auth, f.store, filepath.Dir(project), r.Unavailable()))
	scope := skillimage.Scope{
		TenantID: domain.DefaultTenantID, ProjectID: id,
		SkillID: "security-audit", Version: version, ModeID: "static-code",
	}
	if approve {
		if _, err := auth.Approve(context.Background(), approveRequest(scope, hostImageDigest(t, r))); err != nil {
			t.Fatalf("Approve: %v", err)
		}
	}
	return f, svc, auth, scope
}

// The static-code path, end to end, through the service: activation, approval,
// container, report.
func TestLiveRunSkill_ScansThroughTheApprovedImage(t *testing.T) {
	r := liveRunner(t)
	f, svc, _, scope := liveFixture(t, r, true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := svc.RunSkill(ctx, skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: scope.SkillID, ModeID: scope.ModeID,
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("RunSkill: %v", err)
	}

	// It ran the approved bytes, and the report says whose decision that was.
	if res.Report.ImageDigest != hostImageDigest(t, r) {
		t.Fatalf("ran %q, approved %q", res.Report.ImageDigest, hostImageDigest(t, r))
	}
	if res.Report.ApprovedBy != admin || res.Report.ApprovalID == "" {
		t.Fatalf("the report does not name the approval: %+v", res.Report)
	}
	if res.Report.ApprovalRevokedDuringRun {
		t.Fatal("the approval was live throughout and the report says otherwise")
	}

	// The boundary held: non-root, read-only rootfs, no network, no inherited
	// environment. These come from inside the container, not from AO's flags.
	ev := res.Report.Evidence
	if ev.EffectiveUID == 0 || !ev.ReadOnlyRootFS || ev.NetworkReachable || ev.InheritedDaemonEnv != 0 {
		t.Fatalf("evidence = %+v", ev)
	}

	// And the audit trail records what ran.
	entries, err := f.store.ListSkillAuditForProject(context.Background(), scope.ProjectID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var executed bool
	for _, e := range entries {
		if string(e.Action) == "run_executed" && e.Digest == res.Report.ImageDigest {
			executed = true
		}
	}
	if !executed {
		t.Fatalf("no run_executed entry in %d rows", len(entries))
	}
}

// The load-bearing honesty property: an incomplete result is never presented as
// a clean audit.
//
// Three ways a report could be short, and what the reader gets in each:
//
//	the mount did not deliver  -> the run is REFUSED, no report at all
//	files were skipped         -> they are listed, with the reason
//	nothing matched            -> the rules that ran are named, and the
//	                              limitations say what "no findings" does not mean
func TestLiveRunSkill_AShortResultIsNeverACleanAudit(t *testing.T) {
	r := liveRunner(t)
	_, svc, _, scope := liveFixture(t, r, true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := svc.RunSkill(ctx, skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: scope.SkillID, ModeID: scope.ModeID,
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("RunSkill: %v", err)
	}
	cov := res.Report.Coverage

	// AO staged something, the container saw all of it, and the scan read some
	// of it. A report whose visible count is short never gets built -- the run
	// is refused before parsing -- so seeing equality here is the property.
	if cov.FilesStaged == 0 {
		t.Fatal("nothing was staged and a report was produced anyway")
	}
	if cov.FilesVisible != cov.FilesStaged {
		t.Fatalf("the container saw %d of %d staged files and a report was produced",
			cov.FilesVisible, cov.FilesStaged)
	}
	if cov.FilesScanned == 0 {
		t.Fatalf("no file was read and a report was produced: %+v", cov)
	}

	// What was NOT read is named. This is what stops a short report from
	// reading as a clean one: README.md is out of the scanner's extensions and
	// has to appear as skipped rather than silently vanish.
	if len(cov.Skipped) == 0 {
		t.Fatalf("nothing was reported as skipped, but %d of %d files were scanned",
			cov.FilesScanned, cov.FilesStaged)
	}
	for _, s := range cov.Skipped {
		if strings.TrimSpace(s.Reason) == "" {
			t.Fatalf("%s was skipped with no reason given", s.Path)
		}
	}

	// The rules that ran are named, so an empty finding list reads as "these
	// checks found nothing" rather than "nothing was checked".
	if len(cov.RulesRun) == 0 {
		t.Fatal("the report does not say which rules ran")
	}
	// And the limitations are in the report itself, next to the findings,
	// rather than in documentation nobody opens.
	joined := strings.Join(cov.Limitations, " ")
	for _, must := range []string{"not evidence", "pattern"} {
		if !strings.Contains(joined, must) {
			t.Fatalf("the limitations do not mention %q: %v", must, cov.Limitations)
		}
	}
	// A truncated report is not a clean one, and the flag is on the report so a
	// consumer can see it rather than infer it.
	if res.Report.Truncated {
		t.Log("output hit the cap; the report says so, which is the point")
	}
}

// Requirement: revocation between resolution and launch, against the REAL
// runner. The approval is withdrawn while AO is staging, and the launch is
// refused rather than started.
func TestLiveRunSkill_RevocationDuringStagingStopsTheLaunch(t *testing.T) {
	r := liveRunner(t)
	_, svc, auth, scope := liveFixture(t, r, true)
	approvals, err := auth.ListApprovals(context.Background())
	if err != nil || len(approvals) != 1 {
		t.Fatalf("ListApprovals: %+v (%v)", approvals, err)
	}

	// Revoke first, then run: the resolution itself now fails, which is the
	// same refusal the pre-launch re-check produces and is deterministic to
	// assert. The in-window case is covered against a fake clock in run_test.go
	// and against the runner directly in skillrunner's imagetrust_test.go.
	if err := auth.Revoke(context.Background(), approvals[0].ID, admin, adminPerms()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err = svc.RunSkill(ctx, skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: scope.SkillID, ModeID: scope.ModeID,
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err == nil {
		t.Fatal("a run happened under a revoked approval")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the refusal should say the approval was revoked: %v", err)
	}
	// Nothing started. A refusal that leaves a container behind is not one.
	out, listErr := exec.Command(r.Runtime().Binary, "ps", "-a",
		"--filter", "label="+skillrunner.RunLabel+"=1", "--format", "{{.Names}}").Output()
	if listErr == nil && strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a refused run left containers behind: %s", out)
	}
}

// With no approval at all, the real runner refuses before staging: no copy of
// somebody's source is made for a run that was never going to happen.
func TestLiveRunSkill_RefusesBeforeStagingWhenNothingIsApproved(t *testing.T) {
	r := liveRunner(t)
	_, svc, _, scope := liveFixture(t, r, false)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := svc.RunSkill(ctx, skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: scope.SkillID, ModeID: scope.ModeID,
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err == nil {
		t.Fatal("a run happened with no image approved")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("code = %q", code)
	}
	// The staging root beside the project holds nothing for this run.
	base, _ := filepath.Abs("testdata")
	matches, _ := filepath.Glob(filepath.Join(base, ".ao-skill-staging", "run-*"))
	if len(matches) != 0 {
		t.Fatalf("an unauthorized run staged %d directories", len(matches))
	}
}
