package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/authz"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// skillsWorld is a real installation with a real database, a real router and a
// real skill package on disk. Every assertion below is a direct API call: the
// frontend is not involved, so nothing here can pass because a button was
// hidden.
type skillsWorld struct {
	t      *testing.T
	store  *store.Store
	rbac   *rbac.Service
	srv    *httptest.Server
	client *http.Client

	owner  domain.User
	member domain.User
	viewer domain.User

	packageSrc string
}

// securityAuditSource stages a copy of the shipped package outside the catalog,
// which is where an install has to come from.
func securityAuditSource(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "security-audit")
	if err := skillcatalog.CopyPackage("../skillcatalog/packages/security-audit", dir); err != nil {
		t.Fatalf("stage package: %v", err)
	}
	return dir
}

func newSkillsWorld(t *testing.T) *skillsWorld {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dataDir)
	authMgr := authsvc.New(st, func() time.Time { return time.Now().UTC() })

	mk := func(name string, role domain.UserRole) domain.User {
		u, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{
			DisplayName: name, Email: name + "@example.test", Username: name,
			Password: "correct-horse-" + name, Role: role,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return u
	}
	owner := mk("owner", domain.UserRoleOwner)
	member := mk("member", domain.UserRoleMember)
	viewer := mk("viewer", domain.UserRoleViewer)

	projects := map[domain.ProjectID]projectsvc.Project{}
	for _, id := range []domain.ProjectID{"medusa", "poseidon"} {
		if err := st.UpsertProject(ctx, domain.ProjectRecord{
			ID: string(id), Path: "/tmp/" + string(id), DisplayName: string(id), RegisteredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed project %s: %v", id, err)
		}
		if _, err := st.SetProjectOwner(ctx, id, owner.ID); err != nil {
			t.Fatalf("set project owner: %v", err)
		}
		projects[id] = projectsvc.Project{ID: id, Name: string(id), Path: "/tmp/" + string(id)}
	}

	rbacSvc := rbac.New(st, nil, rbac.NoopAudit{}, nil)
	// One service instance backs both ports: the catalog and the image trust
	// root are two authorities over one store, not two stores.
	skillsSvc := skills.New(st, dataDir,
		skills.WithSkillExecutor(nil, skills.NewImageAuthority(st, st), st, "", ""))
	deps := APIDeps{
		Auth:             authMgr,
		Projects:         &fakeProjectManager{items: projects},
		ProjectOwnership: st,
		SessionOwnership: st,
		Authz:            authz.New(st),
		ProjectScope:     st,
		RBAC:             rbacSvc,
		Skills:           skillsSvc,
		SkillImages:      skillsSvc,
	}
	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &skillsWorld{
		t: t, store: st, rbac: rbacSvc, srv: srv, client: &http.Client{},
		owner: owner, member: member, viewer: viewer,
		packageSrc: securityAuditSource(t),
	}
}

func (w *skillsWorld) login(name string) *http.Cookie {
	w.t.Helper()
	_, cookie := loginOK(w.t, w.srv.URL, name+"@example.test", "correct-horse-"+name)
	return cookie
}

func (w *skillsWorld) do(method, path string, cookie *http.Cookie, body string) (int, string) {
	w.t.Helper()
	req, err := http.NewRequest(method, w.srv.URL+path, strings.NewReader(body))
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(out)
}

func (w *skillsWorld) expect(method, path string, cookie *http.Cookie, body string, want int) string {
	w.t.Helper()
	got, out := w.do(method, path, cookie, body)
	if got != want {
		w.t.Fatalf("%s %s: got %d want %d (%s)", method, path, got, want, out)
	}
	return out
}

func (w *skillsWorld) installAsOwner(cookie *http.Cookie) string {
	w.t.Helper()
	body := w.expect(http.MethodPost, "/api/v1/skills", cookie,
		`{"sourceDir":"`+w.packageSrc+`"}`, http.StatusCreated)
	var view struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		w.t.Fatalf("decode install: %v (%s)", err, body)
	}
	if view.ID != "security-audit" || view.Version == "" {
		w.t.Fatalf("install view = %s", body)
	}
	return view.Version
}

// grantProjectRole gives a user a role on one project.
func (w *skillsWorld) grantProjectRole(user domain.User, project domain.ProjectID, role domain.ProjectRole) {
	w.t.Helper()
	actor := domain.Principal{User: w.owner, AuthMethod: domain.AuthMethodPassword}
	if _, err := w.rbac.GrantProjectAccess(context.Background(), actor, project,
		domain.GrantSubjectUser, string(user.ID), role); err != nil {
		w.t.Fatalf("grant %s on %s: %v", user.Username, project, err)
	}
}

// Installing is installation administration: settings.manage. A member and a
// viewer are refused, and the refusal happens at the route, before any
// filesystem work.
func TestSkills_InstallIsGatedOnSettingsManage(t *testing.T) {
	w := newSkillsWorld(t)
	body := `{"sourceDir":"` + w.packageSrc + `"}`

	w.expect(http.MethodPost, "/api/v1/skills", w.login("member"), body, http.StatusForbidden)
	w.expect(http.MethodPost, "/api/v1/skills", w.login("viewer"), body, http.StatusForbidden)
	// Unauthenticated is refused too.
	w.expect(http.MethodPost, "/api/v1/skills", nil, body, http.StatusUnauthorized)

	ownerCookie := w.login("owner")
	w.installAsOwner(ownerCookie)

	// Nothing was installed by the refused calls.
	list := w.expect(http.MethodGet, "/api/v1/skills", ownerCookie, "", http.StatusOK)
	if strings.Count(list, `"id":"security-audit"`) != 1 {
		t.Fatalf("catalog = %s", list)
	}
}

// The list carries AO's capability vocabulary so a client renders an
// activation dialog from the server's policy rather than a hard-coded table
// that would drift.
func TestSkills_ListCarriesTheCapabilityPolicy(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	w.installAsOwner(ownerCookie)

	body := w.expect(http.MethodGet, "/api/v1/skills", ownerCookie, "", http.StatusOK)
	var out struct {
		Skills []struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
			Modes  []struct {
				ID string `json:"id"`
			} `json:"modes"`
		} `json:"skills"`
		Capabilities []struct {
			Name                  string `json:"name"`
			Risk                  string `json:"risk"`
			RequiresIsolation     bool   `json:"requiresIsolation"`
			RequiresEgressControl bool   `json:"requiresEgressControl"`
			RequiredPermission    string `json:"requiredPermission"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out.Skills) != 1 || out.Skills[0].Digest == "" || len(out.Skills[0].Modes) != 6 {
		t.Fatalf("skills = %+v", out.Skills)
	}
	byName := map[string]bool{}
	for _, c := range out.Capabilities {
		byName[c.Name] = true
		if c.Risk == "" || c.RequiredPermission == "" {
			t.Fatalf("capability %+v is missing its policy", c)
		}
		if c.RequiresEgressControl && !c.RequiresIsolation {
			t.Fatalf("%s claims egress control without isolation", c.Name)
		}
	}
	for _, want := range []string{"repo.read", "net.egress", "net.active_scan", "process.exec"} {
		if !byName[want] {
			t.Fatalf("capability vocabulary is missing %q", want)
		}
	}

	// The package's absolute path on the daemon host is not client business.
	if strings.Contains(body, w.packageSrc) || strings.Contains(body, "packageDir") {
		t.Fatalf("the response leaks the host filesystem layout: %s", body)
	}
}

// Activation is project-scoped: a project administrator activates a skill on
// their OWN project without holding installation authority, and a member of a
// different project cannot see it at all.
func TestSkills_ActivationIsProjectScoped(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)

	// The member administers medusa only.
	w.grantProjectRole(w.member, "medusa", domain.ProjectRoleAdmin)
	memberCookie := w.login("member")

	enable := `{"version":"` + version + `","capabilities":["repo.read","report.write"]}`
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", memberCookie, enable, http.StatusOK)

	// A project they hold nothing on is indistinguishable from one that does
	// not exist.
	w.expect(http.MethodPut, "/api/v1/projects/poseidon/skills/security-audit", memberCookie, enable, http.StatusNotFound)
	w.expect(http.MethodGet, "/api/v1/projects/poseidon/skills", memberCookie, "", http.StatusNotFound)

	// And they still cannot install or uninstall anything.
	w.expect(http.MethodPost, "/api/v1/skills", memberCookie,
		`{"sourceDir":"`+w.packageSrc+`"}`, http.StatusForbidden)
	w.expect(http.MethodDelete, "/api/v1/skills/security-audit/versions/"+version, memberCookie, "", http.StatusForbidden)

	// The activation is visible on medusa and absent on poseidon.
	medusa := w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", memberCookie, "", http.StatusOK)
	if !strings.Contains(medusa, `"enabled":true`) {
		t.Fatalf("medusa = %s", medusa)
	}
	poseidon := w.expect(http.MethodGet, "/api/v1/projects/poseidon/skills", ownerCookie, "", http.StatusOK)
	if strings.Contains(poseidon, `"enabled":true`) {
		t.Fatalf("poseidon inherited medusa's activation: %s", poseidon)
	}
}

// A viewer may read a project's skills and dry-run them -- both are reads --
// but may not change an activation.
func TestSkills_ViewerReadsButCannotActivate(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", ownerCookie,
		`{"version":"`+version+`","capabilities":["repo.read","report.write"]}`, http.StatusOK)

	w.grantProjectRole(w.viewer, "medusa", domain.ProjectRoleViewer)
	viewerCookie := w.login("viewer")

	w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", viewerCookie, "", http.StatusOK)
	w.expect(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/dry-run", viewerCookie,
		`{"modeId":"static-code","inputs":{"mode":"static-code"}}`, http.StatusOK)

	// A project the viewer CAN see, with a permission they lack, is a plain
	// 403. The 404 disguise is only for a project they cannot see at all.
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", viewerCookie,
		`{"version":"`+version+`","capabilities":["repo.read"]}`, http.StatusForbidden)
	w.expect(http.MethodDelete, "/api/v1/projects/medusa/skills/security-audit", viewerCookie, "", http.StatusForbidden)
}

// A grant may never exceed the approver's own permissions. A project
// administrator holds project.manage but not settings.manage, so they cannot
// grant a capability gated on settings.manage.
func TestSkills_GrantCannotExceedTheApprover(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.grantProjectRole(w.member, "medusa", domain.ProjectRoleAdmin)
	memberCookie := w.login("member")

	body := w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", memberCookie,
		`{"version":"`+version+`","capabilities":["repo.read","net.active_scan"]}`, http.StatusForbidden)
	if !strings.Contains(body, "settings.manage") {
		t.Fatalf("the refusal should name the missing permission: %s", body)
	}
	list := w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", memberCookie, "", http.StatusOK)
	if strings.Contains(list, `"enabled":true`) {
		t.Fatalf("a refused grant activated the skill: %s", list)
	}
}

// The dry run is the phase-2 payoff: it answers what a run would need, over
// HTTP, without starting anything.
func TestSkills_DryRunReportsWhatIsMissing(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", ownerCookie,
		`{"version":"`+version+`","capabilities":["repo.read","report.write","deps.read","net.egress"]}`,
		http.StatusOK)

	type dryRun struct {
		Verdict   string `json:"verdict"`
		ModeID    string `json:"modeId"`
		Decisions []struct {
			Capability     string   `json:"capability"`
			Satisfied      bool     `json:"satisfied"`
			DenialReason   string   `json:"denialReason"`
			MissingControl string   `json:"missingControl"`
			Requires       []string `json:"requiresControls"`
		} `json:"decisions"`
		Runner struct {
			RunnerID           string   `json:"runnerId"`
			Available          bool     `json:"available"`
			Isolated           bool     `json:"isolated"`
			Controls           []string `json:"controls"`
			MissingControls    []string `json:"missingControls"`
			NeedsIsolation     bool     `json:"needsIsolation"`
			NeedsEgressControl bool     `json:"needsEgressControl"`
		} `json:"runner"`
		RequiredApproval string   `json:"requiredApproval"`
		Reasons          []string `json:"reasons"`
	}
	decode := func(body string) dryRun {
		t.Helper()
		var out dryRun
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("decode dry run: %v (%s)", err, body)
		}
		return out
	}

	// The daemon under test wires no runner, so even the read-only mode is
	// blocked: reading a checkout has no AO-enforced boundary outside the
	// container, so it is not exempt from confinement.
	static := decode(w.expect(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/dry-run",
		ownerCookie, `{"modeId":"static-code","inputs":{"mode":"static-code"}}`, http.StatusOK))
	if static.Verdict != "blocked" {
		t.Fatalf("static-code verdict = %q, reasons = %v", static.Verdict, static.Reasons)
	}
	if !static.Runner.NeedsIsolation {
		t.Fatalf("a read mode must need confinement: %+v", static.Runner)
	}
	// It needs confinement and nothing beyond it — which is what makes it the
	// first mode that becomes executable once a runner is wired.
	if static.Runner.NeedsEgressControl {
		t.Fatalf("a read mode must not need an egress allowlist: %+v", static.Runner)
	}

	// The dependency mode is granted and still blocked, because AO has no
	// runner that can confine outbound traffic.
	deps := decode(w.expect(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/dry-run",
		ownerCookie, `{"modeId":"dependencies","inputs":{"mode":"dependencies"}}`, http.StatusOK))
	if deps.Verdict != "blocked" {
		t.Fatalf("dependencies verdict = %q", deps.Verdict)
	}
	if !deps.Runner.NeedsIsolation || !deps.Runner.NeedsEgressControl {
		t.Fatalf("runner requirements = %+v", deps.Runner)
	}
	if deps.Runner.Available || deps.Runner.Isolated || deps.Runner.RunnerID != "none" {
		t.Fatalf("AO reported a runner it does not have: %+v", deps.Runner)
	}
	var egress struct {
		reason, missing string
		requires        []string
	}
	for _, d := range deps.Decisions {
		if d.Capability == "net.egress" {
			egress.reason, egress.missing, egress.requires = d.DenialReason, d.MissingControl, d.Requires
		}
	}
	if egress.reason != "missing_control" || egress.missing == "" {
		t.Fatalf("net.egress denial = %+v", egress)
	}
	// The wire carries the whole requirement, not only the first blocker, so a
	// client can show what has to exist.
	if len(egress.requires) == 0 || len(deps.Runner.MissingControls) == 0 {
		t.Fatalf("the response does not say what is missing: %+v %+v", egress, deps.Runner)
	}
	// The default daemon wires no runner, so it attests nothing.
	if len(deps.Runner.Controls) != 0 {
		t.Fatalf("the daemon reported controls it does not have: %v", deps.Runner.Controls)
	}

	// The active pentest stays blocked on every axis, and reports per_target.
	pentest := decode(w.expect(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/dry-run",
		ownerCookie, `{"modeId":"active-pentest","inputs":{"mode":"active-pentest"},`+
			`"authorizedTargets":["staging.example.com:443"]}`, http.StatusOK))
	if pentest.Verdict != "blocked" || pentest.RequiredApproval != "per_target" {
		t.Fatalf("pentest = %+v", pentest)
	}
}

// Uninstall refuses while a project still has the version enabled, and the
// refusal names the project.
func TestSkills_UninstallRefusesWhileEnabled(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", ownerCookie,
		`{"version":"`+version+`","capabilities":["repo.read"]}`, http.StatusOK)

	body := w.expect(http.MethodDelete, "/api/v1/skills/security-audit/versions/"+version,
		ownerCookie, "", http.StatusConflict)
	if !strings.Contains(body, "medusa") {
		t.Fatalf("the refusal should name the project: %s", body)
	}

	w.expect(http.MethodDelete, "/api/v1/projects/medusa/skills/security-audit", ownerCookie, "", http.StatusOK)
	w.expect(http.MethodDelete, "/api/v1/skills/security-audit/versions/"+version, ownerCookie, "", http.StatusOK)
	list := w.expect(http.MethodGet, "/api/v1/skills", ownerCookie, "", http.StatusOK)
	if strings.Contains(list, `"id":"security-audit"`) {
		t.Fatalf("the package survived uninstall: %s", list)
	}
}

// A package edited on disk after install must break resolution rather than be
// served from the stored row.
func TestSkills_AlteredPackageIsReportedUnavailable(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", ownerCookie,
		`{"version":"`+version+`","capabilities":["repo.read","report.write"]}`, http.StatusOK)

	installed, ok, err := w.store.GetSkillInstall(context.Background(), "security-audit", version)
	if err != nil || !ok {
		t.Fatalf("GetSkillInstall: %v ok=%v", err, ok)
	}
	if err := os.WriteFile(filepath.Join(installed.PackageDir, "SKILL.md"), []byte("# anything\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	list := w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", ownerCookie, "", http.StatusOK)
	if !strings.Contains(list, `"available":false`) || !strings.Contains(list, "does not match package contents") {
		t.Fatalf("an altered package was reported as usable: %s", list)
	}
	// And the dry run refuses outright rather than planning against it.
	code, body := w.do(http.MethodPost, "/api/v1/projects/medusa/skills/security-audit/dry-run",
		ownerCookie, `{"modeId":"static-code","inputs":{"mode":"static-code"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("dry run over an altered package: %d %s", code, body)
	}
}

// The audit trail is readable over HTTP, gated with the rest of installation
// administration.
func TestSkills_AuditIsRecordedAndGated(t *testing.T) {
	w := newSkillsWorld(t)
	ownerCookie := w.login("owner")
	version := w.installAsOwner(ownerCookie)
	w.expect(http.MethodPut, "/api/v1/projects/medusa/skills/security-audit", ownerCookie,
		`{"version":"`+version+`","capabilities":["repo.read"]}`, http.StatusOK)
	w.expect(http.MethodDelete, "/api/v1/projects/medusa/skills/security-audit", ownerCookie, "", http.StatusOK)

	// The catalog LIST is readable by a member (settings.read); the trail is
	// not (audit.read), because it names actors and carries host paths.
	w.expect(http.MethodGet, "/api/v1/skills", w.login("member"), "", http.StatusOK)
	w.expect(http.MethodGet, "/api/v1/skills/security-audit/audit", w.login("member"), "", http.StatusForbidden)
	w.expect(http.MethodGet, "/api/v1/skills/security-audit/audit", w.login("viewer"), "", http.StatusForbidden)

	body := w.expect(http.MethodGet, "/api/v1/skills/security-audit/audit", ownerCookie, "", http.StatusOK)
	for _, want := range []string{`"action":"install"`, `"action":"enable"`, `"action":"disable"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("audit is missing %s: %s", want, body)
		}
	}
	if !strings.Contains(body, string(w.owner.ID)) {
		t.Fatalf("audit does not record the actor: %s", body)
	}
}
