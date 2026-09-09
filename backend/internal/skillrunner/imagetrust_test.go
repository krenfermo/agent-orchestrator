package skillrunner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
)

// Every way AO can be asked to run an image it is not authorized to run. None
// of them may end in a container starting, and none may fall back to whatever
// happens to be on the host.

const (
	approvedDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	otherDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// trustRunner is a runner whose runtime answers image inspects from a map, so
// a test can make the host hold a digest, hold a different one, or hold none.
func trustRunner(present map[string]string) *Runner {
	answers := []scriptedAnswer{}
	for ref, id := range present {
		answers = append(answers, scriptedAnswer{match: "image inspect " + ref, out: id + "\n"})
	}
	answers = append(answers, scriptedAnswer{
		match: "image inspect", err: errors.New("Error: No such image"),
	})
	return &Runner{
		runtime: Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2",
			OSType: "linux", Architecture: "aarch64"},
		runner: &scriptedCLI{answers: answers},
	}
}

// The happy path, so the refusals below are refusals and not a broken fixture.
func TestResolveApprovedImage_RunsExactlyWhatWasApproved(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})

	image, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if err != nil {
		t.Fatalf("ResolveApprovedImage: %v", err)
	}
	if image.Digest != approvedDigest {
		t.Fatalf("resolved %q, approved %q", image.Digest, approvedDigest)
	}
	// What AO passes the runtime is the bare digest. A name would reintroduce
	// the mutable pointer the approval exists to remove.
	if image.Ref() != approvedDigest {
		t.Fatalf("Ref() = %q, want the bare digest", image.Ref())
	}
	if image.Approval.ApprovedBy != "ada" {
		t.Fatalf("the approval did not travel: %+v", image.Approval)
	}
}

// Requirement: a substituted digest is refused. This is the attack the whole
// trust root exists for -- the host holds something under the approved identity
// that is not what was approved.
func TestResolveApprovedImage_RefusesASubstitutedDigest(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	// The runtime answers the inspect, but with different bytes.
	r := trustRunner(map[string]string{approvedDigest: otherDigest})

	_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if !errors.Is(err, ErrImageNotApproved) {
		t.Fatalf("err = %v, want ErrImageNotApproved", err)
	}
	if !strings.Contains(err.Error(), otherDigest) || !strings.Contains(err.Error(), approvedDigest) {
		t.Fatalf("the refusal should name both digests: %v", err)
	}
}

// Requirement: an absent image is a refusal, never a pull. AO has no code path
// that fetches, and this is the one where a fallback would be tempting.
func TestResolveApprovedImage_RefusesAnAbsentImageAndNeverPulls(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := trustRunner(nil) // the host holds nothing

	_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if !errors.Is(err, ErrImageNotApproved) {
		t.Fatalf("err = %v, want ErrImageNotApproved", err)
	}
	if !strings.Contains(err.Error(), "does not pull") {
		t.Fatalf("the refusal should say AO does not pull: %v", err)
	}
	cli, ok := r.runner.(*scriptedCLI)
	if !ok {
		t.Fatal("unexpected runner")
	}
	for _, cmd := range cli.seen {
		for _, forbidden := range []string{"pull", "build", "import", "load"} {
			if strings.Contains(cmd, " "+forbidden) {
				t.Fatalf("AO ran %q while resolving an absent image", cmd)
			}
		}
	}
}

// Requirement: an approval belonging to a different tenant, project, version or
// mode does not authorize this run. Each field is checked separately, because a
// check that only compared the project would let a neighbouring mode through.
func TestResolveApprovedImage_RefusesEveryNeighbouringScope(t *testing.T) {
	approved := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(approved, approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})

	cases := map[string]skillimage.Scope{
		"another tenant":  {TenantID: "tenant-2", ProjectID: approved.ProjectID, SkillID: approved.SkillID, Version: approved.Version, ModeID: approved.ModeID},
		"another project": {TenantID: approved.TenantID, ProjectID: "proj-2", SkillID: approved.SkillID, Version: approved.Version, ModeID: approved.ModeID},
		"another skill":   {TenantID: approved.TenantID, ProjectID: approved.ProjectID, SkillID: "dep-audit", Version: approved.Version, ModeID: approved.ModeID},
		"another version": {TenantID: approved.TenantID, ProjectID: approved.ProjectID, SkillID: approved.SkillID, Version: "1.3.0", ModeID: approved.ModeID},
		"another mode":    {TenantID: approved.TenantID, ProjectID: approved.ProjectID, SkillID: approved.SkillID, Version: approved.Version, ModeID: "pentest"},
	}
	for name, scope := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
			if err == nil {
				t.Fatalf("%s was authorized by an approval it does not match", name)
			}
		})
	}
}

// A partially-specified scope is refused before anything else happens. A scope
// missing a field authorizes somebody, somewhere, running something.
func TestResolveApprovedImage_RefusesAnIncompleteOrWildcardScope(t *testing.T) {
	auth := newFakeAuthority()
	auth.put(approvalFor(testScope(), approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})

	full := testScope()
	for name, scope := range map[string]skillimage.Scope{
		"no mode":          {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version},
		"no version":       {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, ModeID: full.ModeID},
		"no tenant":        {ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version, ModeID: full.ModeID},
		"wildcard mode":    {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version, ModeID: "*"},
		"wildcard version": {TenantID: full.TenantID, ProjectID: full.ProjectID, SkillID: full.SkillID, Version: "1.*", ModeID: full.ModeID},
		"wildcard tenant":  {TenantID: domain.TenantID("*"), ProjectID: full.ProjectID, SkillID: full.SkillID, Version: full.Version, ModeID: full.ModeID},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// An expired or revoked approval authorizes nothing, and the two are
// distinguishable from "never approved" so an operator knows which happened.
func TestResolveApprovedImage_RefusesExpiredAndRevoked(t *testing.T) {
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})
	now := time.Now().UTC()

	t.Run("expired", func(t *testing.T) {
		scope := testScope()
		auth := newFakeAuthority()
		a := approvalFor(scope, approvedDigest)
		past := now.Add(-time.Minute)
		a.ExpiresAt = &past
		auth.put(a)
		_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
		if err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("err = %v, want an expiry refusal", err)
		}
	})
	t.Run("revoked", func(t *testing.T) {
		scope := testScope()
		auth := newFakeAuthority()
		auth.put(approvalFor(scope, approvedDigest))
		auth.revoke(scope, string(ToolStaticScan), now.Add(-time.Second))
		_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
		if err == nil || !strings.Contains(err.Error(), "revoked") {
			t.Fatalf("err = %v, want a revocation refusal", err)
		}
	})
	t.Run("never approved", func(t *testing.T) {
		_, err := r.ResolveApprovedImage(context.Background(), newFakeAuthority(), testScope(), ToolStaticScan)
		if !errors.Is(err, skillimage.ErrNotApproved) {
			t.Fatalf("err = %v, want ErrNotApproved", err)
		}
	})
}

// An installation with no trust root authorizes nothing. A nil authority must
// never read as "allow everything", which is the shape this mistake takes.
func TestResolveApprovedImage_ANilAuthorityAuthorizesNothing(t *testing.T) {
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})
	_, err := r.ResolveApprovedImage(context.Background(), nil, testScope(), ToolStaticScan)
	if !errors.Is(err, ErrImageNotApproved) {
		t.Fatalf("err = %v, want ErrImageNotApproved", err)
	}
	if !strings.Contains(err.Error(), "no image trust root") {
		t.Fatalf("the refusal should name the missing trust root: %v", err)
	}
	// And the re-check refuses too: a run cannot slip past by being resolved
	// with an authority and launched without one.
	if err := r.RecheckBeforeLaunch(context.Background(), nil, ApprovedImage{}); err == nil {
		t.Fatal("the pre-launch re-check accepted a nil authority")
	}
}

// An image approved to back the static scan is not thereby approved to back a
// different tool with a different command and a different blast radius.
func TestResolveApprovedImage_RefusesAnApprovalForAnotherTool(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	a := approvalFor(scope, approvedDigest)
	a.Tool = "ao.dependency-audit/v1"
	auth.put(a)
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})

	// Looked up under the static scan, the dependency-audit approval is simply
	// not there: the authority keys on the tool.
	if _, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan); err == nil {
		t.Fatal("an approval for another tool authorized the static scan")
	}
	// And a tool AO ships no contract for is refused before the authority is
	// even consulted.
	if _, err := r.ResolveApprovedImage(context.Background(), auth, scope, Tool("nmap")); !errors.Is(err, ErrToolNotApproved) {
		t.Fatalf("err = %v, want ErrToolNotApproved", err)
	}
}

// Requirement: revocation BETWEEN resolution and launch stops the launch. This
// is the window the pre-launch re-check exists for -- staging a checkout takes
// real time, and an approval withdrawn during it must not be honoured.
func TestRecheckBeforeLaunch_CatchesARevocationInTheWindow(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})

	image, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if err != nil {
		t.Fatalf("ResolveApprovedImage: %v", err)
	}
	before := auth.lookups()

	// The window: an administrator revokes while AO is staging.
	auth.revoke(scope, string(ToolStaticScan), time.Now().UTC().Add(-time.Second))

	if err := r.RecheckBeforeLaunch(context.Background(), auth, image); err == nil {
		t.Fatal("a revoked approval survived to launch")
	}
	// The re-check must actually ask again. A cached answer is the bug this
	// test exists to catch, and it would pass every other assertion.
	if auth.lookups() <= before {
		t.Fatal("the pre-launch re-check did not consult the authority")
	}
}

// The same window, for a REPLACED approval: an administrator who approves a
// different digest between resolution and launch has changed their mind, and
// AO must not launch the digest they moved away from.
func TestRecheckBeforeLaunch_CatchesAReplacedApproval(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest, otherDigest: otherDigest})

	image, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if err != nil {
		t.Fatalf("ResolveApprovedImage: %v", err)
	}
	replaced := approvalFor(scope, otherDigest)
	replaced.ID = "img-2"
	auth.put(replaced)

	err = r.RecheckBeforeLaunch(context.Background(), auth, image)
	if err == nil || !strings.Contains(err.Error(), "changed to") {
		t.Fatalf("err = %v, want a refusal naming the change", err)
	}
	// A deleted approval is caught too.
	auth.remove(scope, string(ToolStaticScan))
	if err := r.RecheckBeforeLaunch(context.Background(), auth, image); err == nil {
		t.Fatal("a deleted approval survived to launch")
	}
}

// The stated policy for a run already in flight: AO does not kill it, and it
// says so in the report rather than staying quiet.
func TestRevokedSince_ReportsWithoutPretendingToStopTheRun(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := trustRunner(map[string]string{approvedDigest: approvedDigest})
	image, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if err != nil {
		t.Fatalf("ResolveApprovedImage: %v", err)
	}
	now := time.Now().UTC()

	if r.RevokedSince(context.Background(), auth, image, now) {
		t.Fatal("a live approval was reported as revoked")
	}
	auth.revoke(scope, string(ToolStaticScan), now.Add(-time.Second))
	if !r.RevokedSince(context.Background(), auth, image, now) {
		t.Fatal("a revocation during the run was not reported")
	}

	// The policy is written down, not implied. A caller building a runbook
	// needs the exact non-promise as much as the promise.
	for _, must := range []string{
		"stops new executions immediately",
		"does NOT stop a container that is already running",
		"does NOT recall secrets already delivered",
	} {
		if !strings.Contains(RevocationPolicy, must) {
			t.Fatalf("RevocationPolicy no longer states %q", must)
		}
	}
}

// An unusable runtime resolves nothing. It is checked before the authority so a
// host with no Docker does not produce an authorization decision it cannot use.
func TestResolveApprovedImage_AnUnusableRuntimeResolvesNothing(t *testing.T) {
	scope := testScope()
	auth := newFakeAuthority()
	auth.put(approvalFor(scope, approvedDigest))
	r := &Runner{probeErr: errors.New("Cannot connect to the Docker daemon"), runner: &scriptedCLI{}}

	_, err := r.ResolveApprovedImage(context.Background(), auth, scope, ToolStaticScan)
	if !errors.Is(err, ErrRuntimeUnavailable) {
		t.Fatalf("err = %v, want ErrRuntimeUnavailable", err)
	}
	if auth.lookups() != 0 {
		t.Fatal("AO asked for an authorization it could not have used")
	}
}
