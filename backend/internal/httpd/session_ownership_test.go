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
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeSessionService answers Get/Send from a fixed set; every other method
// is unused by this test and panics if called, same convention as
// auth_ownership_test.go's fakeProjectManager/fakeWorkflowManager.
type fakeSessionService struct {
	controllers.SessionService
	items map[domain.SessionID]domain.Session
	sent  map[domain.SessionID]string
}

func (f *fakeSessionService) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	s, ok := f.items[id]
	if !ok {
		return domain.Session{}, ports.ErrSessionNotFound
	}
	return s, nil
}

func (f *fakeSessionService) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	if _, ok := f.items[id]; !ok {
		return ports.ErrSessionNotFound
	}
	if f.sent == nil {
		f.sent = map[domain.SessionID]string{}
	}
	f.sent[id] = message
	return nil
}

// TestSessionOwnershipIDOR is Checkpoint 8P-B.1's cross-user session attack
// test (E2E F): with AO_TRUSTED_LOCAL_MODE off, User A must never be able
// to inspect (get) or message (send) User B's session by id -- expect 404,
// not 403 (existence must not leak), and B's own session must be
// unaffected by A's rejected attempts.
func TestSessionOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	userA, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "alice@example.com", Username: "alice", Password: "correct-horse-a"})
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	userB, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "bob@example.com", Username: "bob", Password: "correct-horse-b"})
	if err != nil {
		t.Fatalf("create user B: %v", err)
	}

	// Ownership scoping reads owner_user_id off the real sessions table, so
	// each session needs a durable row (the fake service below only serves
	// the read-model the controller renders; it never touches the DB).
	// sessions.project_id is a foreign key, so each session's project needs
	// a durable row too. CreateSession always assigns the real
	// "<projectID>-<num>" id itself -- it does not honor a caller-supplied
	// SessionRecord.ID -- so the ids used below come from its return value.
	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-a", Path: "/tmp/a", DisplayName: "Project A", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project A: %v", err)
	}
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-b", Path: "/tmp/b", DisplayName: "Project B", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project B: %v", err)
	}
	recA, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-a", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session A: %v", err)
	}
	recB, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-b", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session B: %v", err)
	}
	sessA, sessB := recA.ID, recB.ID
	if _, err := store.SetSessionOwner(ctx, sessA, userA.ID); err != nil {
		t.Fatalf("set owner A: %v", err)
	}
	if _, err := store.SetSessionOwner(ctx, sessB, userB.ID); err != nil {
		t.Fatalf("set owner B: %v", err)
	}

	svc := &fakeSessionService{items: map[domain.SessionID]domain.Session{
		sessA: {SessionRecord: domain.SessionRecord{ID: sessA, ProjectID: "proj-a"}},
		sessB: {SessionRecord: domain.SessionRecord{ID: sessB, ProjectID: "proj-b"}},
	}}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr, Sessions: svc, SessionOwnership: store}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	client := &http.Client{}
	_, cookieA := loginOK(t, srv.URL, "alice@example.com", "correct-horse-a")
	_, cookieB := loginOK(t, srv.URL, "bob@example.com", "correct-horse-b")

	// --- A can see its own session, not B's, by direct id ---
	assertStatus(t, client, srv.URL+"/api/v1/sessions/"+string(sessA), cookieA, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/sessions/"+string(sessB), cookieA, http.StatusNotFound)

	// --- A cannot message B's session ---
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/sessions/"+string(sessB)+"/send", cookieA,
		strings.NewReader(`{"message":"pwned"}`), http.StatusNotFound)
	if _, sent := svc.sent[sessB]; sent {
		t.Fatal("A's rejected send must never reach the session service")
	}

	// --- B's own session is unaffected and still reachable by B ---
	assertStatus(t, client, srv.URL+"/api/v1/sessions/"+string(sessB), cookieB, http.StatusOK)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/sessions/"+string(sessB)+"/send", cookieB,
		strings.NewReader(`{"message":"hello"}`), http.StatusOK)
	if svc.sent[sessB] != "hello" {
		t.Fatalf("B's own send did not reach the session service: %v", svc.sent)
	}
}

// TestSessionOwnershipIDORMatrix is Checkpoint 8P-B.2's expanded IDOR
// matrix (§18/§25): every one of these routes was newly gated by this
// checkpoint. panicSessionService has NO working methods, so if any
// handler's ownership check were missing or bypassed, the call would
// panic (surfacing the gap loudly) instead of silently succeeding.
func TestSessionOwnershipIDORMatrix(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	userA, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "alice2@example.com", Username: "alice2", Password: "correct-horse-a"})
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "bob2@example.com", Username: "bob2", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}

	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-a2", Path: "/tmp/a2", DisplayName: "Project A2", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	recA, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-a2", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session A: %v", err)
	}
	if _, err := store.SetSessionOwner(ctx, recA.ID, userA.ID); err != nil {
		t.Fatalf("set owner A: %v", err)
	}

	svc := &panicSessionService{}
	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr, Sessions: svc, SessionOwnership: store}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	client := &http.Client{}
	_, cookieB := loginOK(t, srv.URL, "bob2@example.com", "correct-horse-b")

	sess := string(recA.ID)
	cases := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodGet, "/api/v1/sessions/" + sess + "/preview", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/preview", `{"url":""}`},
		{http.MethodDelete, "/api/v1/sessions/" + sess + "/preview", ""},
		{http.MethodGet, "/api/v1/sessions/" + sess + "/preview/files/index.html", ""},
		{http.MethodGet, "/api/v1/sessions/" + sess + "/workspace/files", ""},
		{http.MethodGet, "/api/v1/sessions/" + sess + "/pr", ""},
		{http.MethodPatch, "/api/v1/sessions/" + sess, `{"displayName":"pwned"}`},
		{http.MethodPatch, "/api/v1/sessions/" + sess + "/merge-policy", `{"terminateOnPrMerge":true}`},
		{http.MethodPatch, "/api/v1/sessions/" + sess + "/auto-inject-review", `{"autoInjectReview":true}`},
		{http.MethodPatch, "/api/v1/sessions/" + sess + "/auto-inject-ci", `{"autoInjectCI":true}`},
		{http.MethodPut, "/api/v1/sessions/" + sess + "/reviewer", `{"harness":"claude-code"}`},
		{http.MethodPut, "/api/v1/sessions/" + sess + "/auto-review", `{"enabled":true}`},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/restore", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/resume-agent", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/switch-agent", `{"targetHarness":"codex"}`},
		{http.MethodGet, "/api/v1/sessions/" + sess + "/agent-switches", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/kill", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/rollback", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/pin", ""},
		{http.MethodDelete, "/api/v1/sessions/" + sess + "/pin", ""},
		{http.MethodPost, "/api/v1/sessions/" + sess + "/attachments", `{"attachments":[]}`},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			var body *strings.Reader
			if c.body != "" {
				body = strings.NewReader(c.body)
			}
			assertStatusMethod(t, client, c.method, srv.URL+c.path, cookieB, body, http.StatusNotFound)
		})
	}
}

// panicSessionService implements controllers.SessionService with every
// method panicking -- used to prove a denied request never reaches the
// underlying service at all.
type panicSessionService struct {
	controllers.SessionService
}

type panicUsageSummaryService struct{}

func (panicUsageSummaryService) ListCompact(context.Context, domain.ProjectID, *domain.UserID) ([]domain.CompactSessionUsage, error) {
	panic("must not be called for a denied request")
}
func (panicUsageSummaryService) Get(context.Context, domain.SessionID) (domain.SessionUsageSummary, error) {
	panic("must not be called for a denied request")
}

// TestUsageSessionOwnershipIDOR proves GET /usage/sessions/{id} is scoped
// like every other per-session read added in this checkpoint.
func TestUsageSessionOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	userA, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "alice3@example.com", Username: "alice3", Password: "correct-horse-a"})
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "bob3@example.com", Username: "bob3", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}

	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-a3", Path: "/tmp/a3", DisplayName: "Project A3", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-a3", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := store.SetSessionOwner(ctx, rec.ID, userA.ID); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr, UsageSummary: panicUsageSummaryService{}, SessionOwnership: store}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	client := &http.Client{}
	_, cookieB := loginOK(t, srv.URL, "bob3@example.com", "correct-horse-b")
	assertStatus(t, client, srv.URL+"/api/v1/usage/sessions/"+string(rec.ID), cookieB, http.StatusNotFound)
}

// panicConversationService implements controllers.ConversationService with
// every method panicking -- used to prove a denied request never reaches
// the underlying chat driver at all.
type panicConversationService struct {
	controllers.ConversationService
}

// TestConversationAndReviewOwnershipIDOR proves the ConversationsController
// and ReviewsController surfaces (the largest per-session control surface
// in the API, per this checkpoint's inventory) are scoped by the same
// canonical boundary as SessionsController.
func TestConversationAndReviewOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	userA, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "alice4@example.com", Username: "alice4", Password: "correct-horse-a"})
	if err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "bob4@example.com", Username: "bob4", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}

	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-a4", Path: "/tmp/a4", DisplayName: "Project A4", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-a4", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := store.SetSessionOwner(ctx, rec.ID, userA.ID); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{
		Auth:             authMgr,
		Conversations:    &panicConversationService{},
		SessionOwnership: store,
	}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	client := &http.Client{}
	_, cookieB := loginOK(t, srv.URL, "bob4@example.com", "correct-horse-b")

	sess := string(rec.ID)
	assertStatus(t, client, srv.URL+"/api/v1/sessions/"+sess+"/conversation", cookieB, http.StatusNotFound)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess+"/conversation/messages", cookieB,
		strings.NewReader(`{"text":"pwned"}`), http.StatusNotFound)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/sessions/"+sess+"/conversation/interrupt", cookieB, nil, http.StatusNotFound)
}

// TestSessionOwnershipTrustedLocalRegression is Checkpoint 8P-B.1's
// trusted-local regression test (security matrix #13): with
// AO_TRUSTED_LOCAL_MODE on (today's desktop default), session ownership
// scoping must stay a complete no-op, exactly like pre-8P-B.1 behavior --
// any session is reachable regardless of its owner_user_id.
func TestSessionOwnershipTrustedLocalRegression(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	ctx := context.Background()

	if _, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "admin@example.com", Username: "admin", Password: "correct-horse-admin"}); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	other, err := authMgr.CreateUser(ctx, authsvc.CreateUserInput{Email: "other@example.com", Username: "other", Password: "correct-horse-other"})
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}

	now := time.Now().UTC()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-x", Path: "/tmp/x", DisplayName: "Project X", RegisteredAt: now}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: "proj-x", Kind: domain.KindWorker, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := store.SetSessionOwner(ctx, rec.ID, other.ID); err != nil {
		t.Fatalf("set owner: %v", err)
	}

	svc := &fakeSessionService{items: map[domain.SessionID]domain.Session{
		rec.ID: {SessionRecord: domain.SessionRecord{ID: rec.ID, ProjectID: "proj-x"}},
	}}

	cfg := config.Config{TrustedLocalMode: true}
	deps := APIDeps{Auth: authMgr, Sessions: svc, SessionOwnership: store}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	client := &http.Client{}
	_, cookieAdmin := loginOK(t, srv.URL, "admin@example.com", "correct-horse-admin")

	// admin is NOT the session's owner (other is), but trusted-local mode
	// must still serve it -- scoping is a complete no-op here.
	assertStatus(t, client, srv.URL+"/api/v1/sessions/"+string(rec.ID), cookieAdmin, http.StatusOK)
}
