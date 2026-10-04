package httpd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// skill_private_registry_e2e_test.go -- the phase-11 routes through the real
// router, against a real HTTPS registry.
//
// The existing marketplace e2e covers the local-directory half. This one covers
// what only exists once there is a socket: the connection test, the revocation
// sync, and the permission gate on both. It runs the production HTTP stack over
// the production client; the only thing a test supplies is the fixture's own
// certificate authority, through a seam no configuration reaches.

type privateWorld struct {
	*marketplaceWorld
	registry *registrytest.Server
}

func newPrivateWorld(t *testing.T) *privateWorld {
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

	registry := registrytest.New(t, "company-private")
	skillsSvc := skills.New(st, dataDir)
	// No credential resolver: this world's registry is unauthenticated, which
	// is a legitimate shape for a read-only mirror inside a network that
	// already authenticates.
	factory := skillregistry.DefaultProviderFactory{Options: registry.Options()}
	marketplace := skills.NewMarketplace(st, skillsSvc, factory, "0.12.0", dataDir).
		WithConnectivity(st, skillregistry.SecretResolverFunc(
			func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
				return skillsecrets.SecretValue{}, nil
			}))

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

	return &privateWorld{
		marketplaceWorld: &marketplaceWorld{
			t: t, store: st, srv: srv, client: &http.Client{},
			owner: owner, member: member, viewer: viewer,
		},
		registry: registry,
	}
}

// configure saves the fixture as a private HTTPS registry, with the loopback
// exception an on-host registry genuinely needs.
func (w *privateWorld) configure(cookie *http.Cookie) {
	w.t.Helper()
	w.expect(http.MethodPut, "/api/v1/skills/registries/company-private", cookie, `{
		"displayName": "Company Private",
		"type": "https",
		"location": "`+w.registry.BaseURL()+`",
		"enabled": true,
		"trustPolicy": "digest",
		"permittedPrivateCidrs": ["127.0.0.0/8"]
	}`, http.StatusOK)
}

// TestPrivateRegistryRoutes walks configure → test → search → install →
// revoke → sync through HTTP, and asserts the two things a client acts on:
// that a search moved no bytes, and that a revoked release stops installing.
func TestPrivateRegistryRoutes(t *testing.T) {
	w := newPrivateWorld(t)
	w.registry.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	owner := w.login("owner")
	w.configure(owner)

	// The connection test answers 200 with a verdict. A failed test is a
	// successful answer to the question that was asked.
	body := w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/test", owner, "", http.StatusOK)
	var probe struct {
		State, Detail, Origin, ReportedRegistryID, Assurance string
	}
	decodeInto(t, body, &probe)
	if probe.State != "CONNECTED" {
		t.Fatalf("connection test said %s: %s", probe.State, probe.Detail)
	}
	if probe.Assurance == "" {
		t.Fatal("the response does not state what a connection test does")
	}
	if w.registry.FetchedArtifact() {
		t.Fatalf("the connection test downloaded something: %v", w.registry.Requests())
	}

	// Search, then confirm nothing was fetched.
	body = w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", owner, "", http.StatusOK)
	if !strings.Contains(body, `"skillId":"security-audit"`) {
		t.Fatalf("search did not find the release: %s", body)
	}
	if !strings.Contains(body, `"trust":"unverified"`) {
		t.Fatalf("a search result claims more than unverified: %s", body)
	}
	if w.registry.FetchedArtifact() {
		t.Fatalf("searching downloaded an artifact: %v", w.registry.Requests())
	}

	// Install, and confirm it is verified, recorded, and enabled nowhere.
	body = w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "company-private", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusCreated)
	if !strings.Contains(body, `"trust":"verified"`) {
		t.Fatalf("the install did not report verified: %s", body)
	}
	if strings.Contains(body, `"trust":"trusted"`) {
		t.Fatalf("something claimed trusted: %s", body)
	}
	if !w.registry.FetchedArtifact() {
		t.Fatal("the install fetched nothing")
	}
	projectSkills := w.expect(http.MethodGet, "/api/v1/projects/medusa/skills", owner, "", http.StatusOK)
	if strings.Contains(projectSkills, `"enabled":true`) {
		t.Fatalf("installing enabled it on a project: %s", projectSkills)
	}

	// The registry withdraws it. A sync marks the install and blocks new ones.
	w.registry.Revoke("security-audit", "1.0.0", "a dependency shipped a backdoor")
	body = w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/revocations/sync", owner, "", http.StatusOK)
	if !strings.Contains(body, `"affectedInstalls":["security-audit@1.0.0"]`) {
		t.Fatalf("the sync did not mark the installed release: %s", body)
	}
	if !strings.Contains(body, `"installed":true`) {
		t.Fatalf("the revocation is not reported as installed here: %s", body)
	}
	if !strings.Contains(body, "does NOT uninstall") {
		t.Fatalf("the response does not state the non-promise: %s", body)
	}

	// It is still installed. AO marked it and removed nothing.
	installs, err := w.store.ListSkillInstalls(context.Background())
	if err != nil {
		t.Fatalf("ListSkillInstalls: %v", err)
	}
	if len(installs) != 1 {
		t.Fatalf("the revocation changed the installed set: %d", len(installs))
	}

	// And a new install of that exact release is refused.
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "company-private", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusForbidden)
}

// TestPrivateRegistryRoutesRequireSettingsManage holds the gate. Both routes
// make AO open a connection and present a stored credential, which is not a
// read of AO's own state.
func TestPrivateRegistryRoutesRequireSettingsManage(t *testing.T) {
	w := newPrivateWorld(t)
	w.configure(w.login("owner"))

	for _, who := range []string{"member", "viewer"} {
		cookie := w.login(who)
		w.expect(http.MethodPost,
			"/api/v1/skills/registries/company-private/test", cookie, "", http.StatusForbidden)
		w.expect(http.MethodPost,
			"/api/v1/skills/registries/company-private/revocations/sync", cookie, "", http.StatusForbidden)
	}
	w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/test", nil, "", http.StatusUnauthorized)
	w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/revocations/sync", nil, "", http.StatusUnauthorized)
}

// TestPrivateRegistryStillHasNoExecutionSurface repeats phase 10's assertion
// against the routes phase 11 added. Reaching a registry over the network must
// not have quietly grown a way to run something.
func TestPrivateRegistryStillHasNoExecutionSurface(t *testing.T) {
	w := newPrivateWorld(t)
	owner := w.login("owner")
	w.configure(owner)

	for _, path := range []string{
		"/api/v1/skills/registries/company-private/run",
		"/api/v1/skills/registries/company-private/test/run",
		"/api/v1/skills/registries/company-private/revocations/run",
	} {
		status, _ := w.do(http.MethodPost, path, owner, "")
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s answered %d; the registry surface must run nothing", path, status)
		}
	}
}

// TestUnreachableRegistryIsNamedNotSilent holds the difference between "this
// registry has nothing" and "AO could not read it". Only one of them means
// somebody should go and look.
func TestUnreachableRegistryIsNamedNotSilent(t *testing.T) {
	w := newPrivateWorld(t)
	w.registry.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	owner := w.login("owner")
	w.configure(owner)
	w.registry.Stop()

	body := w.expect(http.MethodGet, "/api/v1/skills/marketplace", owner, "", http.StatusOK)
	if !strings.Contains(body, `"registryId":"company-private"`) {
		t.Fatalf("an unreadable registry vanished from the result: %s", body)
	}
	if !strings.Contains(body, `"notes"`) {
		t.Fatalf("the response carries no notes: %s", body)
	}

	// The connection test says WHY, and it is not CONNECTED.
	probeBody := w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/test", owner, "", http.StatusOK)
	var probe struct{ State string }
	decodeInto(t, probeBody, &probe)
	if probe.State == "CONNECTED" {
		t.Fatalf("a stopped registry tested as CONNECTED: %s", probeBody)
	}

	// A sync that could not ask says so and changes nothing.
	syncBody := w.expect(http.MethodPost,
		"/api/v1/skills/registries/company-private/revocations/sync", owner, "", http.StatusOK)
	if !strings.Contains(syncBody, `"unreachable"`) {
		t.Fatalf("an unreachable sync reported success: %s", syncBody)
	}
}

func decodeInto(t *testing.T, body string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), into); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
}

// TestMarketplaceSearchCarriesMetadataFreshness is Check 19 over HTTP: the
// state the provider knows has to survive every layer between it and a client.
//
// It asserts the wire, not the service, because the whole defect was that the
// service knew and the wire did not: a search run with the registry down came
// back byte-identical to one run with the registry up, and no client could
// have told the difference no matter how carefully it was written.
func TestMarketplaceSearchCarriesMetadataFreshness(t *testing.T) {
	w := newPrivateWorld(t)
	w.registry.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})
	owner := w.login("owner")
	w.configure(owner)

	search := func() marketplaceSearchBody {
		w.t.Helper()
		raw := w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", owner, "", http.StatusOK)
		var out marketplaceSearchBody
		decodeInto(t, raw, &out)
		return out
	}

	live := search()
	if live.Offline {
		t.Fatalf("a search against a running registry reported offline: %+v", live.Sources)
	}
	if len(live.Releases) != 1 || live.Releases[0].MetadataFreshness != "live" {
		t.Fatalf("a live search reported %+v", live.Releases)
	}
	if len(live.Sources) != 1 || live.Sources[0].Freshness != "live" || live.Sources[0].Offline {
		t.Fatalf("a live search reported sources %+v", live.Sources)
	}
	if live.FreshnessNotice != "" {
		t.Fatalf("a live search carried an offline notice: %s", live.FreshnessNotice)
	}

	// The registry goes away. The result is still served -- from cache -- and
	// every part of it says so.
	w.registry.Pause()
	offline := search()
	if len(offline.Releases) != 1 {
		t.Fatalf("the cached search returned %d releases", len(offline.Releases))
	}
	if !offline.Offline {
		t.Fatal("a search with the registry down did not set offline on the response")
	}
	rel := offline.Releases[0]
	if rel.MetadataFreshness != "offline" || !rel.MetadataOffline {
		t.Fatalf("a cached row reported freshness=%q offline=%v", rel.MetadataFreshness, rel.MetadataOffline)
	}
	if rel.MetadataAsOf.IsZero() {
		t.Fatal("a cached row carried no as-of time")
	}
	if len(offline.Sources) != 1 || offline.Sources[0].Freshness != "offline" || !offline.Sources[0].Offline {
		t.Fatalf("the offline search reported sources %+v", offline.Sources)
	}
	if offline.Sources[0].Explanation == "" {
		t.Fatal("the offline source carried no explanation from the daemon")
	}
	if offline.FreshnessNotice == "" {
		t.Fatal("an offline search carried no notice for a client to show")
	}
	// The trust state did not move because the metadata went stale.
	if rel.Trust != live.Releases[0].Trust {
		t.Fatalf("going offline moved trust from %s to %s", live.Releases[0].Trust, rel.Trust)
	}
	// And nothing was downloaded to produce any of this.
	if w.registry.FetchedArtifact() {
		t.Fatalf("the offline search fetched an artifact: %v", w.registry.Requests())
	}

	// An install while the registry is unreachable is still refused: the
	// listing may come from cache, the install may not. The refusal names the
	// unreachable registry rather than pretending the release is gone.
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "company-private", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusForbidden)

	// The registry comes back, and the state goes back with it. Offline is a
	// state, not a latch.
	w.registry.Resume()
	back := search()
	if back.Offline {
		t.Fatalf("the search still reported offline after recovery: %+v", back.Sources)
	}
	if len(back.Releases) != 1 || back.Releases[0].MetadataFreshness != "live" {
		t.Fatalf("after recovery the search reported %+v", back.Releases)
	}
	if back.Sources[0].Freshness != "live" || back.Sources[0].Offline {
		t.Fatalf("after recovery the sources reported %+v", back.Sources)
	}
}

// marketplaceSearchBody is the wire shape this test asserts on. It is written
// out here rather than imported from controllers so that a field quietly
// renamed on the DTO fails this test instead of silently agreeing with it.
type marketplaceSearchBody struct {
	Releases []struct {
		SkillID           string    `json:"skillId"`
		Trust             string    `json:"trust"`
		MetadataFreshness string    `json:"metadataFreshness"`
		MetadataOffline   bool      `json:"metadataOffline"`
		MetadataAsOf      time.Time `json:"metadataAsOf"`
	} `json:"releases"`
	Sources []struct {
		RegistryID  string    `json:"registryId"`
		Freshness   string    `json:"freshness"`
		Offline     bool      `json:"offline"`
		FetchedAt   time.Time `json:"fetchedAt"`
		Explanation string    `json:"explanation"`
	} `json:"sources"`
	Offline         bool   `json:"offline"`
	FreshnessNotice string `json:"freshnessNotice"`
}
