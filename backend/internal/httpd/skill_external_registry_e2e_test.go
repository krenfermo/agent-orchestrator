package httpd

import (
	"context"
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
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/githubtest"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// skill_external_registry_e2e_test.go -- the phase-13 routes through the REAL
// router, over the real wire bodies.
//
// # Why this file exists, specifically
//
// The phase-13 service tests all called Marketplace.SaveRegistry with a fully
// populated skillregistry.Registry. Every one of them passed while the HTTP
// surface could not create an external registry AT ALL: the wire body carried
// owner, repository and allowedOwners, the service input struct did not, and
// the three fields were dropped silently between them. A client could send
// them and nothing would ever read them.
//
// That is not a bug a service-level test can find, because the mapping it
// skips IS the bug. So these tests go through the router, with JSON, exactly
// as the settings screen does -- and the first assertion in the first test is
// the one that would have caught it.

type externalWorld struct {
	*marketplaceWorld
	forge *githubtest.Server
	repo  *githubtest.Repo
}

func newExternalWorld(t *testing.T) *externalWorld {
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

	forge := githubtest.New(t)
	repo := forge.AddRepo("acme", "skills", false)
	// WithOriginSource, exactly as daemon.go wires it. Without it the catalog
	// list answers with no provenance at all, and the Installed screen would
	// render every forge install as if it had none -- which is the assertion
	// below, and the reason this world is wired like production rather than
	// like the older marketplace e2e.
	skillsSvc := skills.New(st, dataDir, skills.WithOriginSource(st))
	factory := skillregistry.DefaultProviderFactory{Options: forge.Options()}
	trust := skills.NewTrustAuthority(st, nil)
	marketplace := skills.NewMarketplace(st, skillsSvc, factory, "0.12.0", dataDir).
		WithConnectivity(st, nil).
		WithTrust(trust).
		WithExternal(st)

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
		SkillTrust:       trust,
		SkillExternal:    skills.NewExternalAuthority(trust, marketplace),
		SkillTenancy:     st,
	}
	srv := httptest.NewServer(NewRouterWithControl(
		config.Config{TrustedLocalMode: false}, discardLogger(), nil, deps, ControlDeps{}))
	t.Cleanup(srv.Close)

	return &externalWorld{
		marketplaceWorld: &marketplaceWorld{
			t: t, store: st, srv: srv, client: &http.Client{},
			owner: owner, member: member, viewer: viewer,
		},
		forge: forge,
		repo:  repo,
	}
}

// configure saves the fixture forge as a github registry, over the wire.
func (w *externalWorld) configure(cookie *http.Cookie, policy string, extra string) string {
	w.t.Helper()
	return w.expect(http.MethodPut, "/api/v1/skills/registries/acme-skills", cookie, `{
		"displayName": "Acme skills",
		"type": "github",
		"location": "`+w.forge.BaseURL()+`",
		"enabled": true,
		"trustPolicy": "`+policy+`",
		"owner": "acme",
		"repository": "skills",
		"permittedPrivateCidrs": ["127.0.0.0/8"]`+extra+`
	}`, http.StatusOK)
}

// TestExternalRegistryRoutes walks configure -> search -> install -> move the
// tag -> refuse, through HTTP.
func TestExternalRegistryRoutes(t *testing.T) {
	w := newExternalWorld(t)
	spec := githubtest.Spec{SkillID: "security-audit", Version: "1.0.0", Publisher: "acme"}
	published := w.repo.Publish(t, spec)
	owner := w.login("owner")

	// THE assertion that was missing. The wire carries the external scope, so
	// the stored row must carry it back -- a field a client can send and
	// nothing ever reads is a whole registry type nobody can configure.
	body := w.configure(owner, "external_integrity", "")
	if !strings.Contains(body, `"owner":"acme"`) {
		t.Fatalf("the saved registry dropped its owner: %s", body)
	}
	if !strings.Contains(body, `"repository":"skills"`) {
		t.Fatalf("the saved registry dropped its repository: %s", body)
	}
	if !strings.Contains(body, `"external":true`) {
		t.Fatalf("the saved registry is not reported as external: %s", body)
	}

	// A search pins the commit and moves no package bytes.
	body = w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", owner, "", http.StatusOK)
	if !strings.Contains(body, `"sourceCommit":"`+published.Commit+`"`) {
		t.Fatalf("the search did not pin the commit: %s", body)
	}
	if !strings.Contains(body, `"sourceProvider":"github"`) {
		t.Fatalf("the search did not name the provider: %s", body)
	}
	if !strings.Contains(body, `"trust":"unverified"`) {
		t.Fatalf("a search result claims more than unverified: %s", body)
	}
	// The sentence a client must show before installing from a forge.
	if !strings.Contains(body, "does not mean AO trusts the publisher") {
		t.Fatalf("the response carries no hosting notice: %s", body)
	}
	if w.forge.FetchedArchive() {
		t.Fatalf("searching downloaded an archive: %v", w.forge.Requests())
	}

	// Install: verified, never trusted, and the provenance keeps the commit.
	body = w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "acme-skills", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusCreated)
	if !strings.Contains(body, `"trust":"verified"`) {
		t.Fatalf("the install did not report verified: %s", body)
	}
	if strings.Contains(body, `"trust":"trusted"`) {
		t.Fatalf("an unsigned forge install claimed trusted: %s", body)
	}
	if !strings.Contains(body, `"sourceCommit":"`+published.Commit+`"`) {
		t.Fatalf("the provenance did not record the commit: %s", body)
	}
	if !strings.Contains(body, "a claim in the package, not a fact AO checked") {
		t.Fatalf("the provenance does not separate the publisher claim: %s", body)
	}

	// The publisher force-pushes the tag.
	second := w.repo.Republish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
		Extra: "a second build of the same version",
	}, githubtest.CommitSHA("moved-e2e"))

	body = w.expect(http.MethodGet, "/api/v1/skills/marketplace?q=security", owner, "", http.StatusOK)
	if !strings.Contains(body, `"tagMoved":true`) {
		t.Fatalf("the marketplace did not mark the moved tag: %s", body)
	}
	if !strings.Contains(body, published.Commit) || !strings.Contains(body, second.Commit) {
		t.Fatalf("the moved-tag explanation does not carry both commits: %s", body)
	}

	// The installed row still says where its bytes came from.
	body = w.expect(http.MethodGet, "/api/v1/skills", owner, "", http.StatusOK)
	if !strings.Contains(body, `"sourceCommit":"`+published.Commit+`"`) {
		t.Fatalf("the installed provenance moved with the tag: %s", body)
	}

	// And a silent reinstall is refused.
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "acme-skills", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusConflict)

	// The moved tag is on the external surface too.
	body = w.expect(http.MethodGet, "/api/v1/skills/external/tags", owner, "", http.StatusOK)
	if !strings.Contains(body, `"previousCommit":"`+published.Commit+`"`) {
		t.Fatalf("the tag ledger route does not report the move: %s", body)
	}
}

// TestExternalAllowlistOverTheWire holds the FASE M validation on the surface
// an administrator actually uses: a pattern is refused, and a plain list is
// stored and read back.
func TestExternalAllowlistOverTheWire(t *testing.T) {
	w := newExternalWorld(t)
	owner := w.login("owner")

	// A wildcard is how an organization allowlist becomes an everything
	// allowlist, so it never reaches the store.
	w.expect(http.MethodPut, "/api/v1/skills/registries/acme-skills", owner, `{
		"displayName": "Acme skills",
		"type": "github",
		"location": "`+w.forge.BaseURL()+`",
		"enabled": true,
		"trustPolicy": "external_org_allowlist",
		"owner": "acme",
		"allowedOwners": ["*"],
		"permittedPrivateCidrs": ["127.0.0.0/8"]
	}`, http.StatusBadRequest)

	body := w.configure(owner, "external_org_allowlist", `,
		"allowedOwners": ["acme", "globex"]`)
	if !strings.Contains(body, `"allowedOwners":["acme","globex"]`) {
		t.Fatalf("the allowlist did not survive the round trip: %s", body)
	}

	// And the two vocabularies do not mix: a private policy on a forge is
	// refused before anything is stored.
	w.expect(http.MethodPut, "/api/v1/skills/registries/mixed", owner, `{
		"displayName": "Mixed",
		"type": "github",
		"location": "`+w.forge.BaseURL()+`",
		"enabled": true,
		"trustPolicy": "digest",
		"owner": "acme",
		"permittedPrivateCidrs": ["127.0.0.0/8"]
	}`, http.StatusBadRequest)
}

// TestExternalRevocationRoutes covers the administrative surface a forge
// cannot provide: withdraw, block, lift.
func TestExternalRevocationRoutes(t *testing.T) {
	w := newExternalWorld(t)
	w.repo.Publish(t, githubtest.Spec{
		SkillID: "security-audit", Version: "1.0.0", Publisher: "acme",
	})
	owner := w.login("owner")
	w.configure(owner, "external_integrity", "")

	body := w.expect(http.MethodPost, "/api/v1/skills/external/revocations", owner, `{
		"subject": "external_repository",
		"subjectId": "acme/skills",
		"reason": "an advisory somebody here read"
	}`, http.StatusOK)
	if !strings.Contains(body, `"subjectId":"acme/skills"`) {
		t.Fatalf("the withdrawal was not recorded: %s", body)
	}

	// It blocks a new install and removes nothing.
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "acme-skills", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusForbidden)

	body = w.expect(http.MethodGet, "/api/v1/skills/external/revocations", owner, "", http.StatusOK)
	if !strings.Contains(body, "does not uninstall anything") {
		t.Fatalf("the response does not state the non-promise: %s", body)
	}

	// Lifting is possible for a place, and the install goes through again.
	w.expect(http.MethodPost, "/api/v1/skills/external/revocations/lift", owner, `{
		"subject": "external_repository", "subjectId": "acme/skills"
	}`, http.StatusOK)
	w.expect(http.MethodPost, "/api/v1/skills/marketplace/install", owner, `{
		"registryId": "acme-skills", "skillId": "security-audit", "version": "1.0.0"
	}`, http.StatusCreated)

	// A signing key is NOT liftable through this surface.
	w.expect(http.MethodPost, "/api/v1/skills/external/revocations/lift", owner, `{
		"subject": "signing_key", "subjectId": "some-key"
	}`, http.StatusForbidden)
}

// TestExternalRoutesRequireSettingsManage holds the gate: withdrawing a source
// changes what this installation will install, which is not a read.
func TestExternalRoutesRequireSettingsManage(t *testing.T) {
	w := newExternalWorld(t)
	viewer := w.login("viewer")

	w.expect(http.MethodPost, "/api/v1/skills/external/revocations", viewer, `{
		"subject": "external_owner", "subjectId": "acme", "reason": "no"
	}`, http.StatusForbidden)
	w.expect(http.MethodPost, "/api/v1/skills/external/revocations/lift", viewer, `{
		"subject": "external_owner", "subjectId": "acme"
	}`, http.StatusForbidden)
}
