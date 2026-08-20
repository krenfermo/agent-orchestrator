package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestAuthSetupFlow covers Checkpoint 8P-E.8's first-run onboarding
// surface end to end through a real router: setup-status flips from true to
// false after the first registration, a second registration is rejected,
// and the loopback-only admin reset-password endpoint works and revokes
// sessions.
func TestAuthSetupFlow(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	// --- setup required before any user exists ---
	assertSetupRequired(t, srv.URL, true)

	// --- register the first (owner) account ---
	regBody, cookie := registerOK(t, srv.URL, "Owner", "owner@example.com", "supersecret1")
	if !strings.Contains(regBody, `"role":"owner"`) {
		t.Fatalf("expected registered user to have owner role: %s", regBody)
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatal("register must sign the new owner in with an HttpOnly session cookie")
	}

	// --- setup no longer required ---
	assertSetupRequired(t, srv.URL, false)

	// --- a second registration attempt is rejected ---
	status, body := registerAttempt(t, srv.URL, "Someone Else", "someone@example.com", "supersecret2")
	if status != http.StatusConflict {
		t.Fatalf("expected 409 for second registration, got %d: %s", status, body)
	}

	// --- admin reset-password works and revokes the existing session ---
	client := &http.Client{}
	assertStatus(t, client, srv.URL+"/api/v1/auth/me", cookie, http.StatusOK)

	resetBody, _ := json.Marshal(map[string]string{"email": "owner@example.com", "newPassword": "brandnewpass1"})
	resp, err := http.Post(srv.URL+"/api/v1/auth/admin/reset-password", "application/json", strings.NewReader(string(resetBody)))
	if err != nil {
		t.Fatalf("admin reset-password request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin reset-password failed: %d %s", resp.StatusCode, readAll(t, resp))
	}

	// Old session must now be revoked.
	assertStatus(t, client, srv.URL+"/api/v1/auth/me", cookie, http.StatusUnauthorized)

	// New password logs in; old one doesn't.
	if _, err := authMgr.Authenticate(t.Context(), "owner@example.com", "supersecret1", "test"); err == nil {
		t.Fatal("expected old password to be rejected")
	}
	if _, cookie2 := loginOK(t, srv.URL, "owner@example.com", "brandnewpass1"); cookie2 == nil {
		t.Fatal("expected login with new password to succeed")
	}
}

func assertSetupRequired(t *testing.T, baseURL string, want bool) {
	t.Helper()
	resp, err := http.Get(baseURL + "/api/v1/auth/setup-status")
	if err != nil {
		t.Fatalf("setup-status request: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		SetupRequired bool `json:"setupRequired"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode setup-status: %v", err)
	}
	if out.SetupRequired != want {
		t.Fatalf("setup-status = %v, want %v", out.SetupRequired, want)
	}
}

func registerAttempt(t *testing.T, baseURL, displayName, email, password string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"displayName": displayName, "email": email, "password": password})
	resp, err := http.Post(baseURL+"/api/v1/auth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

func registerOK(t *testing.T, baseURL, displayName, email, password string) (string, *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"displayName": displayName, "email": email, "password": password})
	resp, err := http.Post(baseURL+"/api/v1/auth/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register request: %v", err)
	}
	defer resp.Body.Close()
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register failed: %d %s", resp.StatusCode, raw)
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "ao_session" {
			cookie = c
		}
	}
	return raw, cookie
}
