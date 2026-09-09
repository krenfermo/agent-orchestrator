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
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// skill_marketplace_e2e_test.go -- the registry surface through the real
// router, with real users and a real authorization stack.
//
// Every assertion is a direct API call. Nothing here can pass because a button
// was hidden: the routes are what refuse, and this file is where that is
// checked for the two questions the phase brief names -- searching without
// permission, and installing without it.

type marketplaceWorld struct {
	t      *testing.T
	store  *store.Store
	srv    *httptest.Server
	client *http.Client

	owner  domain.User
	member domain.User
	viewer domain.User

	registryRoot string
}

func newMarketplaceWorld(t *testing.T) *marketplaceWorld {
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

	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: "medusa", Path: "/tmp/medusa", DisplayName: "medusa", RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	skillsSvc := skills.New(st, dataDir)
	marketplace := skills.NewMarketplace(st, skillsSvc,
		skillregistry.DefaultProviderFactory{}, "0.12.0", dataDir)

	deps := APIDeps{
		Auth:             authMgr,
		Projects:         &fakeProjectManager{items: map[domain.ProjectID]projectsvc.Project{}},
		ProjectOwnership: st,
		SessionOwnership: st,
		Authz:            authz.New(st),
		ProjectScope:     st,
		RBAC:             rbac.New(st, nil, rbac.NoopAudit{}, nil),
		Skills:           skillsSvc,
		SkillImages:      skillsSvc,
		SkillMarketplace: marketplace,
		SkillTenancy:     st,
	}
	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &marketplaceWorld{
		t: t, store: st, srv: srv, client: &http.Client{},
		owner: owner, member: member, viewer: viewer,
		registryRoot: buildFixtureRegistry(t),
	}
}

// buildFixtureRegistry writes a local registry holding one release of the
// shipped security-audit package, with digests computed from the bytes it
// actually wrote.
func buildFixtureRegistry(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages", "security-audit", "0.1.0")
	if err := skillcatalog.CopyPackage("../skillcatalog/packages/security-audit", pkgDir); err != nil {
		t.Fatalf("stage package: %v", err)
	}
	pkg, err := skillcatalog.LoadPackage(pkgDir)
	if err != nil {
		t.Fatalf("load staged package: %v", err)
	}
	manifestDigest, err := skillregistry.FileDigest(filepath.Join(pkgDir, skillcatalog.ManifestFileName))
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	caps := make([]string, 0, len(pkg.Manifest.Capabilities))
	for _, c := range pkg.Manifest.Capabilities {
		caps = append(caps, string(c))
	}
	modes := make([]map[string]any, 0, len(pkg.Manifest.Modes))
	for _, mode := range pkg.Manifest.Modes {
		modeCaps := make([]string, 0, len(mode.Capabilities))
		for _, c := range mode.Capabilities {
			modeCaps = append(modeCaps, string(c))
		}
		modes = append(modes, map[string]any{
			"id": mode.ID, "name": mode.Name, "description": mode.Description,
			"riskLevel": string(mode.RiskLevel), "capabilities": modeCaps,
		})
	}
	index := map[string]any{
		"apiVersion": skillregistry.IndexAPIVersion,
		"releases": []map[string]any{{
			"skillId":               pkg.Manifest.ID,
			"name":                  pkg.Manifest.Name,
			"version":               pkg.Manifest.Version,
			"publisher":             pkg.Manifest.Provenance.Publisher,
			"description":           pkg.Manifest.Description,
			"riskLevel":             string(pkg.Manifest.RiskLevel),
			"manifestDigest":        manifestDigest,
			"artifactDigest":        pkg.Digest,
			"requestedCapabilities": caps,
			"executionModes":        modes,
			"compatibility":         map[string]any{"aoMinVersion": pkg.Manifest.Compatibility.AOMinVersion},
			"publishedAt":           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"artifactPath":          "packages/security-audit/0.1.0",
		}},
	}
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, skillregistry.IndexFileName),
		append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	return root
}

func (w *marketplaceWorld) login(name string) *http.Cookie {
	w.t.Helper()
	_, cookie := loginOK(w.t, w.srv.URL, name+"@example.test", "correct-horse-"+name)
	return cookie
}

func (w *marketplaceWorld) do(method, path string, cookie *http.Cookie, body string) (int, string) {
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
	out := make([]byte, 0, 8192)
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

func (w *marketplaceWorld) expect(method, path string, cookie *http.Cookie, body string, want int) string {
	w.t.Helper()
	got, out := w.do(method, path, cookie, body)
	if got != want {
		w.t.Fatalf("%s %s: got %d want %d (%s)", method, path, got, want, out)
	}
	return out
}

func (w *marketplaceWorld) addRegistry(cookie *http.Cookie) {
	w.t.Helper()
	w.expect(http.MethodPut, "/api/v1/skills/registries/ao-fixture", cookie, `{
		"displayName":"AO Fixture","type":"local","location":"`+w.registryRoot+`",
		"enabled":true,"trustPolicy":"digest","priority":10
	}`, http.StatusOK)
}

// Configuring a registry and installing from one are installation
// administration: settings.manage. A member and a viewer are refused, and the
// refusal happens at the route.
func TestMarketplace_WritesAreGatedOnSettingsManage(t *testing.T) {
	w := newMarketplaceWorld(t)
	configure := `{"displayName":"X","type":"local","location":"` + w.registryRoot + `",
		"enabled":true,"trustPolicy":"digest"}`

	for _, who := range []string{"member", "viewer"} {
		cookie := w.login(who)
		w.expect(http.MethodPut, "/api/v1/skills/registries/ao-fixture", cookie, configure, http.StatusForbidden)
		w.expect(http.MethodDelete, "/api/v1/skills/registries/ao-fixture", cookie, "", http.StatusForbidden)
		w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", cookie,
			`{"registryId":"ao-fixture","skillId":"security-audit","version":"0.1.0"}`,
			http.StatusForbidden)
	}
	w.expect(http.MethodPut, "/api/v1/skills/registries/ao-fixture", nil, configure, http.StatusUnauthorized)
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", nil,
		`{"registryId":"ao-fixture","skillId":"security-audit","version":"0.1.0"}`,
		http.StatusUnauthorized)
}

// Searching is a read: settings.read, which a member and a viewer both hold.
// An unauthenticated caller holds nothing.
func TestMarketplace_SearchRequiresSettingsRead(t *testing.T) {
	w := newMarketplaceWorld(t)
	w.addRegistry(w.login("owner"))

	for _, who := range []string{"owner", "member", "viewer"} {
		body := w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", w.login(who), "", http.StatusOK)
		var res struct {
			Releases []struct {
				SkillID   string `json:"skillId"`
				Trust     string `json:"trust"`
				Installed bool   `json:"installed"`
			} `json:"releases"`
			InstallNotice string `json:"installNotice"`
		}
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("decode search: %v (%s)", err, body)
		}
		if len(res.Releases) != 1 || res.Releases[0].SkillID != "security-audit" {
			t.Fatalf("%s searched and got %s", who, body)
		}
		// A search hashed nothing, so it must not claim it did.
		if res.Releases[0].Trust != "unverified" {
			t.Fatalf("a search result reported trust %q", res.Releases[0].Trust)
		}
		// The notice a client must show comes from the daemon, so the promise
		// on screen cannot drift from the behaviour in the service.
		if !strings.Contains(res.InstallNotice, "does not enable it on any project") {
			t.Fatalf("installNotice = %q", res.InstallNotice)
		}
	}
	w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", nil, "", http.StatusUnauthorized)
	w.expect(http.MethodGet, "/api/v1/skills/registries", nil, "", http.StatusUnauthorized)
	w.expect(http.MethodGet, "/api/v1/skills/updates", nil, "", http.StatusUnauthorized)
}

// The whole delivery goal, through the API: search, open, review, install --
// and then nothing is enabled anywhere.
func TestMarketplace_SearchOpenInstallEnablesNothing(t *testing.T) {
	w := newMarketplaceWorld(t)
	owner := w.login("owner")
	w.addRegistry(owner)

	detail := w.expect(http.MethodGet,
		"/api/v1/skills/marketplace/ao-fixture/security-audit", owner, "", http.StatusOK)
	var det struct {
		Release struct {
			Publisher             string   `json:"publisher"`
			RequestedCapabilities []string `json:"requestedCapabilities"`
			Trust                 string   `json:"trust"`
			TrustExplanation      string   `json:"trustExplanation"`
			Compatibility         string   `json:"compatibility"`
		} `json:"release"`
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := json.Unmarshal([]byte(detail), &det); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, detail)
	}
	if det.Release.Publisher == "" || len(det.Release.RequestedCapabilities) == 0 || len(det.Versions) != 1 {
		t.Fatalf("detail = %s", detail)
	}
	if det.Release.Compatibility != "compatible" {
		t.Fatalf("compatibility = %q", det.Release.Compatibility)
	}
	// The explanation is served, not written in the UI, and it says what
	// "unverified" actually means.
	if !strings.Contains(det.Release.TrustExplanation, "has not fetched") {
		t.Fatalf("trustExplanation = %q", det.Release.TrustExplanation)
	}

	out := w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner,
		`{"registryId":"ao-fixture","skillId":"security-audit","version":"0.1.0"}`,
		http.StatusCreated)
	var installed struct {
		Install struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"install"`
		Origin struct {
			RegistryID string `json:"registryId"`
			Trust      string `json:"trust"`
		} `json:"origin"`
		NextStep string `json:"nextStep"`
	}
	if err := json.Unmarshal([]byte(out), &installed); err != nil {
		t.Fatalf("decode install: %v (%s)", err, out)
	}
	if installed.Install.ID != "security-audit" || installed.Origin.RegistryID != "ao-fixture" {
		t.Fatalf("install = %s", out)
	}
	// Verified, because AO hashed the bytes -- and never "trusted", because it
	// verified no signature.
	if installed.Origin.Trust != "verified" {
		t.Fatalf("origin trust = %q", installed.Origin.Trust)
	}
	if !strings.Contains(installed.NextStep, "Choose a project to enable it") {
		t.Fatalf("nextStep = %q", installed.NextStep)
	}

	// It is in the catalog...
	list := w.expect(http.MethodGet, "/api/v1/skills", owner, "", http.StatusOK)
	if !strings.Contains(list, `"id":"security-audit"`) {
		t.Fatalf("catalog = %s", list)
	}
	// ...and enabled on nothing.
	projectSkills := w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", owner, "", http.StatusOK)
	var ps struct {
		Activations []any `json:"activations"`
	}
	if err := json.Unmarshal([]byte(projectSkills), &ps); err != nil {
		t.Fatalf("decode project skills: %v (%s)", err, projectSkills)
	}
	if len(ps.Activations) != 0 {
		t.Fatalf("installing from the marketplace activated something: %s", projectSkills)
	}
}

// The marketplace has no route that runs anything, and the one that would be
// the obvious guess does not exist.
func TestMarketplace_HasNoRunRoute(t *testing.T) {
	w := newMarketplaceWorld(t)
	owner := w.login("owner")
	w.addRegistry(owner)
	for _, path := range []string{
		"/api/v1/skills/marketplace/run",
		"/api/v1/skills/marketplace/ao-fixture/security-audit/run",
		"/api/v1/skills/registries/ao-fixture/run",
	} {
		status, _ := w.do(http.MethodPost, path, owner, `{}`)
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s answered %d; the marketplace must have no execution surface", path, status)
		}
	}
}

// A configuration AO cannot read is refused at save time.
func TestMarketplace_RefusesAnUnreadableRegistry(t *testing.T) {
	w := newMarketplaceWorld(t)
	body := w.expect(http.MethodPut, "/api/v1/skills/registries/broken", w.login("owner"), `{
		"displayName":"Broken","type":"local","location":"`+filepath.Join(t.TempDir(), "nope")+`",
		"enabled":true,"trustPolicy":"digest"
	}`, http.StatusBadRequest)
	if !strings.Contains(body, "SKILL_REGISTRY_UNREADABLE") {
		t.Fatalf("refusal = %s", body)
	}
}
