package skills_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
)

// The trust root, over a real database. Every test here is about the ONE door:
// who may open it, what it will not accept through it, and what a run gets back
// when nothing was approved.

const staticScanTool = "ao.static-scan/v1"

func digestOf(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

func imageFixture(t *testing.T) (fixture, *skills.ImageAuthority, skillimage.Scope) {
	t.Helper()
	f := newFixture(t)
	project := f.seedProject(t, "medusa")
	auth := skills.NewImageAuthority(f.store, f.store).WithImageInspector(acceptAll())
	scope := skillimage.Scope{
		TenantID: domain.DefaultTenantID, ProjectID: project,
		SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
	}
	return f, auth, scope
}

func approveRequest(scope skillimage.Scope, digest string) skills.ApproveRequest {
	return skills.ApproveRequest{
		Scope: scope, Tool: staticScanTool, Reference: "alpine", Digest: digest,
		Note:    "pulled by hand and diffed against the upstream manifest",
		Confirm: true, Actor: admin, ActorPermissions: adminPerms(),
	}
}

// The happy path, and the audit row it must leave. An approval nobody can find
// later is an approval nobody can be asked about.
func TestApprove_RecordsTheDecisionAndAuditsIt(t *testing.T) {
	f, auth, scope := imageFixture(t)
	ctx := context.Background()

	approval, err := auth.Approve(ctx, approveRequest(scope, digestOf('a')))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approval.ApprovedBy != admin || approval.Digest != digestOf('a') {
		t.Fatalf("approval = %+v", approval)
	}
	if approval.ExpiresAt != nil {
		t.Fatal("no expiry was asked for and one was invented")
	}

	got, err := auth.ApprovedImage(ctx, scope, staticScanTool)
	if err != nil {
		t.Fatalf("ApprovedImage: %v", err)
	}
	if got.ID != approval.ID {
		t.Fatalf("read back %q, wrote %q", got.ID, approval.ID)
	}

	entries, err := f.store.ListSkillAuditForSkill(ctx, "security-audit")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var found bool
	for _, e := range entries {
		if string(e.Action) == "image_approved" && e.Digest == digestOf('a') && e.Actor == admin {
			found = true
		}
	}
	if !found {
		t.Fatalf("no image_approved entry in %d audit rows", len(entries))
	}
}

// Requirement: insufficient permission cannot approve. This is the whole trust
// root -- if a project maintainer could approve their own image, "administrative
// approval" would be a label rather than a control.
func TestApprove_RefusesWithoutSettingsManage(t *testing.T) {
	_, auth, scope := imageFixture(t)
	req := approveRequest(scope, digestOf('a'))
	req.ActorPermissions = []domain.Permission{
		domain.PermProjectRead, domain.PermProjectManage, domain.PermWorkflowRun,
	}

	_, err := auth.Approve(context.Background(), req)
	if err == nil {
		t.Fatal("a caller without settings.manage approved an image")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_APPROVAL_REFUSED" {
		t.Fatalf("code = %q", code)
	}
	// Revocation is gated the same way: a permission that cannot approve must
	// not be able to un-approve either, in both directions.
	if err := auth.Revoke(context.Background(), "whatever", "mallory", req.ActorPermissions); err == nil {
		t.Fatal("a caller without settings.manage revoked an approval")
	}
}

// Approving an image is not a side effect of filling in a form. The absence of
// an explicit confirmation is a refusal, never a default.
func TestApprove_RequiresAnExplicitConfirmation(t *testing.T) {
	_, auth, scope := imageFixture(t)
	req := approveRequest(scope, digestOf('a'))
	req.Confirm = false

	_, err := auth.Approve(context.Background(), req)
	if err == nil {
		t.Fatal("an unconfirmed approval was recorded")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_APPROVAL_UNCONFIRMED" {
		t.Fatalf("code = %q", code)
	}
}

// Everything the model refuses must be refused here too, with the API envelope
// rather than a raw error.
func TestApprove_RefusesAnythingItCouldNotEnforce(t *testing.T) {
	_, auth, scope := imageFixture(t)
	wild := scope
	wild.Version = "1.*"
	partial := scope
	partial.ModeID = ""

	cases := map[string]func(*skills.ApproveRequest){
		"a tag instead of a digest": func(r *skills.ApproveRequest) { r.Digest = "alpine:3.19" },
		"latest":                    func(r *skills.ApproveRequest) { r.Digest = "latest" },
		"a short digest":            func(r *skills.ApproveRequest) { r.Digest = "sha256:abc" },
		"a tagged reference":        func(r *skills.ApproveRequest) { r.Reference = "alpine:3.19" },
		"no tool":                   func(r *skills.ApproveRequest) { r.Tool = "" },
		"no stated reason":          func(r *skills.ApproveRequest) { r.Note = "" },
		"a wildcard scope":          func(r *skills.ApproveRequest) { r.Scope = wild },
		"an incomplete scope":       func(r *skills.ApproveRequest) { r.Scope = partial },
		"an anonymous approver":     func(r *skills.ApproveRequest) { r.Actor = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := approveRequest(scope, digestOf('a'))
			mutate(&req)
			if _, err := auth.Approve(context.Background(), req); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// Re-approving REPLACES. "Which image may this scope run" must have one answer,
// and a second digest accumulating beside the first would make it two.
func TestApprove_ReplacesRatherThanAccumulates(t *testing.T) {
	_, auth, scope := imageFixture(t)
	ctx := context.Background()

	first, err := auth.Approve(ctx, approveRequest(scope, digestOf('a')))
	if err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	second, err := auth.Approve(ctx, approveRequest(scope, digestOf('b')))
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	got, err := auth.ApprovedImage(ctx, scope, staticScanTool)
	if err != nil {
		t.Fatalf("ApprovedImage: %v", err)
	}
	if got.Digest != digestOf('b') {
		t.Fatalf("resolved %q after re-approval to %q", got.Digest, digestOf('b'))
	}
	all, err := auth.ListApprovals(ctx)
	if err != nil {
		t.Fatalf("ListApprovals: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("%d approvals for one scope; re-approval accumulated", len(all))
	}
	// The row is replaced in place, so the old id no longer resolves.
	if first.ID == second.ID {
		t.Log("the store reused the id, which is fine: the digest is the identity")
	}
}

// A revoked approval stops authorizing, immediately, and the refusal says which
// of "revoked" and "never approved" happened.
func TestRevoke_StopsAuthorizingAndSaysSo(t *testing.T) {
	_, auth, scope := imageFixture(t)
	ctx := context.Background()

	approval, err := auth.Approve(ctx, approveRequest(scope, digestOf('a')))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := auth.Revoke(ctx, approval.ID, admin, adminPerms()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, err = auth.ApprovedImage(ctx, scope, staticScanTool)
	if err == nil {
		t.Fatal("a revoked approval still authorized")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_APPROVAL_INACTIVE" {
		t.Fatalf("code = %q, want the inactive code rather than not-found", code)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the refusal should say it was revoked: %v", err)
	}

	// Revoking twice is a conflict, not a silent success: an operator needs to
	// know whether their action did anything.
	if err := auth.Revoke(ctx, approval.ID, admin, adminPerms()); err == nil {
		t.Fatal("a second revoke reported success")
	}
	// Revoking something that was never approved is a not-found.
	if err := auth.Revoke(ctx, "skimg-nope", admin, adminPerms()); err == nil {
		t.Fatal("revoking an unknown approval reported success")
	}
	// The listing keeps it: it is the history of what this installation once
	// allowed, and hiding it would answer "what did we approve" with "what is
	// approved now".
	all, err := auth.ListApprovals(ctx)
	if err != nil || len(all) != 1 || all[0].RevokedAt == nil {
		t.Fatalf("the revoked approval is not in the history: %+v (%v)", all, err)
	}
}

// An expired approval is refused the same way, and the two reasons are
// distinguishable.
func TestApprovedImage_RefusesAnExpiredApproval(t *testing.T) {
	_, auth, scope := imageFixture(t)
	ctx := context.Background()
	req := approveRequest(scope, digestOf('a'))
	req.ExpiresIn = 30 * time.Millisecond
	if _, err := auth.Approve(ctx, req); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	_, err := auth.ApprovedImage(ctx, scope, staticScanTool)
	if err == nil {
		t.Fatal("an expired approval still authorized")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Fatalf("the refusal should say it expired: %v", err)
	}
}

// Requirement: another tenant, project, version or mode is not covered, read
// through the real store rather than through the in-memory model.
func TestApprovedImage_IsExactInEveryDimension(t *testing.T) {
	f, auth, scope := imageFixture(t)
	ctx := context.Background()
	other := f.seedProject(t, "other-project")
	if _, err := auth.Approve(ctx, approveRequest(scope, digestOf('a'))); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	neighbours := map[string]skillimage.Scope{
		"another tenant":  {TenantID: "tenant-2", ProjectID: scope.ProjectID, SkillID: scope.SkillID, Version: scope.Version, ModeID: scope.ModeID},
		"another project": {TenantID: scope.TenantID, ProjectID: other, SkillID: scope.SkillID, Version: scope.Version, ModeID: scope.ModeID},
		"another skill":   {TenantID: scope.TenantID, ProjectID: scope.ProjectID, SkillID: "dep-audit", Version: scope.Version, ModeID: scope.ModeID},
		"another version": {TenantID: scope.TenantID, ProjectID: scope.ProjectID, SkillID: scope.SkillID, Version: "1.3.0", ModeID: scope.ModeID},
		"another mode":    {TenantID: scope.TenantID, ProjectID: scope.ProjectID, SkillID: scope.SkillID, Version: scope.Version, ModeID: "pentest"},
	}
	for name, neighbour := range neighbours {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.ApprovedImage(ctx, neighbour, staticScanTool); err == nil {
				t.Fatalf("%s was authorized", name)
			}
		})
	}
	// And another TOOL, on the exact same scope.
	if _, err := auth.ApprovedImage(ctx, scope, "ao.dependency-audit/v1"); err == nil {
		t.Fatal("an approval for the static scan authorized another tool")
	}
}

// An installation with no trust root authorizes nothing. A nil authority must
// never behave as "allow everything", which is the shape this mistake takes.
func TestImageAuthority_ANilTrustRootAuthorizesNothing(t *testing.T) {
	var auth *skills.ImageAuthority
	if auth.Available() {
		t.Fatal("a nil trust root reported itself available")
	}
	_, err := auth.ApprovedImage(context.Background(), skillimage.Scope{
		TenantID: "t", ProjectID: "p", SkillID: "s", Version: "1", ModeID: "m",
	}, staticScanTool)
	if err == nil {
		t.Fatal("a nil trust root authorized an image")
	}
	if code := apiCode(t, err); code != "SKILL_IMAGE_TRUST_ROOT_UNAVAILABLE" {
		t.Fatalf("code = %q", code)
	}
}

// A project-scoped listing stays inside its project. A view that leaked another
// tenant's approvals would leak which images they run.
func TestListApprovalsForProject_StaysInsideTheProject(t *testing.T) {
	f, auth, scope := imageFixture(t)
	ctx := context.Background()
	other := f.seedProject(t, "other-project")
	if _, err := auth.Approve(ctx, approveRequest(scope, digestOf('a'))); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	otherScope := scope
	otherScope.ProjectID = other
	if _, err := auth.Approve(ctx, approveRequest(otherScope, digestOf('b'))); err != nil {
		t.Fatalf("Approve other: %v", err)
	}

	mine, err := auth.ListApprovalsForProject(ctx, scope.ProjectID)
	if err != nil {
		t.Fatalf("ListApprovalsForProject: %v", err)
	}
	if len(mine) != 1 || mine[0].Scope.ProjectID != scope.ProjectID {
		t.Fatalf("the project listing returned %+v", mine)
	}
	if all, err := auth.ListApprovals(ctx); err != nil || len(all) != 2 {
		t.Fatalf("the installation listing returned %d approvals (%v)", len(all), err)
	}
}

// The trust root must not record a decision about bytes it cannot see. A digest
// in an approval request is CLIENT-SUPPLIED — it is whatever the form sent —
// and until the host is asked, it is a claim about an artifact rather than the
// artifact. These are the four ways that claim fails, and each must refuse
// BEFORE anything is stored.
func TestApprove_RefusesADigestThisHostCannotShowIt(t *testing.T) {
	ctx := context.Background()

	t.Run("the host does not have it", func(t *testing.T) {
		f := newFixture(t)
		project := f.seedProject(t, "medusa")
		auth := skills.NewImageAuthority(f.store, f.store).
			WithImageInspector(&stubInspector{present: map[string]string{digestOf('a'): digestOf('a')}})
		scope := skillimage.Scope{
			TenantID: domain.DefaultTenantID, ProjectID: project,
			SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
		}

		if _, err := auth.Approve(ctx, approveRequest(scope, digestOf('b'))); err == nil {
			t.Fatal("approved a digest that is not on this host")
		}
		// Nothing may be stored: a refused approval that left a row would be a
		// trust root that records what it rejected.
		if _, ok, _ := f.store.GetSkillImageApprovalForScope(ctx, scope, staticScanTool); ok {
			t.Fatal("a refused approval was stored anyway")
		}
	})

	t.Run("the host holds something else under that name", func(t *testing.T) {
		f := newFixture(t)
		project := f.seedProject(t, "medusa")
		// Asked for 'a', the runtime answers 'c'. Approving would record a
		// decision about bytes nobody reviewed.
		auth := skills.NewImageAuthority(f.store, f.store).
			WithImageInspector(&stubInspector{present: map[string]string{digestOf('a'): digestOf('c')}})
		scope := skillimage.Scope{
			TenantID: domain.DefaultTenantID, ProjectID: project,
			SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
		}

		_, err := auth.Approve(ctx, approveRequest(scope, digestOf('a')))
		if err == nil {
			t.Fatal("approved a digest the host resolves to different bytes")
		}
		if !strings.Contains(err.Error(), digestOf('c')) {
			t.Fatalf("the refusal must name what the host actually holds; got: %v", err)
		}
	})

	t.Run("the runtime is unusable", func(t *testing.T) {
		f := newFixture(t)
		project := f.seedProject(t, "medusa")
		auth := skills.NewImageAuthority(f.store, f.store).
			WithImageInspector(&stubInspector{err: errors.New("no container runtime on this host")})
		scope := skillimage.Scope{
			TenantID: domain.DefaultTenantID, ProjectID: project,
			SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
		}

		if _, err := auth.Approve(ctx, approveRequest(scope, digestOf('a'))); err == nil {
			t.Fatal("approved while the runtime could not answer")
		}
	})

	// No inspector at all is a REFUSAL, not a skip. "Record it now, find out at
	// run time" is exactly the trust-what-you-were-told this check exists to
	// prevent.
	t.Run("there is no inspector wired", func(t *testing.T) {
		f := newFixture(t)
		project := f.seedProject(t, "medusa")
		auth := skills.NewImageAuthority(f.store, f.store)
		scope := skillimage.Scope{
			TenantID: domain.DefaultTenantID, ProjectID: project,
			SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
		}

		if _, err := auth.Approve(ctx, approveRequest(scope, digestOf('a'))); err == nil {
			t.Fatal("an installation with no runtime approved an image anyway")
		}
	})
}

// Verifying presence must never be a way to make AO fetch or start anything.
// The inspector is only ever asked to look.
func TestApprove_VerificationOnlyReads(t *testing.T) {
	f := newFixture(t)
	project := f.seedProject(t, "medusa")
	spy := &countingInspector{inner: acceptAll()}
	auth := skills.NewImageAuthority(f.store, f.store).WithImageInspector(spy)
	scope := skillimage.Scope{
		TenantID: domain.DefaultTenantID, ProjectID: project,
		SkillID: "security-audit", Version: "1.2.0", ModeID: "static-code",
	}

	if _, err := auth.Approve(context.Background(), approveRequest(scope, digestOf('a'))); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("the host was asked %d times; approval looks exactly once", spy.calls)
	}
}
