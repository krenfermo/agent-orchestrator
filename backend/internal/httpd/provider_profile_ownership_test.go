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
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type fixedDataDir string

func (d fixedDataDir) DataDir() string { return string(d) }

// TestProviderProfileOwnershipIDOR is Checkpoint 8P-B's security test:
// with AO_TRUSTED_LOCAL_MODE off, User A must never be able to list, fetch,
// update, connect, disconnect, or test User B's provider profiles by id
// (expect 404, not 403 -- existence must not leak), and no response body
// may ever contain a secret/ciphertext field.
func TestProviderProfileOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	svc := &providerprofile.Service{Store: store, DataDir: fixedDataDir(t.TempDir())}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr, ProviderProfiles: svc}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "alice@example.com", Username: "alice", Password: "correct-horse-a"}); err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "bob@example.com", Username: "bob", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}

	client := &http.Client{}
	_, cookieA := loginOK(t, srv.URL, "alice@example.com", "correct-horse-a")
	_, cookieB := loginOK(t, srv.URL, "bob@example.com", "correct-horse-b")

	profA := createProfile(t, client, srv.URL, cookieA)
	profB := createProfile(t, client, srv.URL, cookieB)

	// --- A can see its own profile, not B's, by direct id ---
	assertStatus(t, client, srv.URL+"/api/v1/provider-profiles/"+profA, cookieA, http.StatusOK)
	assertStatus(t, client, srv.URL+"/api/v1/provider-profiles/"+profB, cookieA, http.StatusNotFound)

	// --- A's list excludes B's profile entirely ---
	listBody := getBody(t, client, srv.URL+"/api/v1/provider-profiles", cookieA)
	if strings.Contains(listBody, profB) {
		t.Fatalf("provider profile list leaked another user's profile: %s", listBody)
	}
	if !strings.Contains(listBody, profA) {
		t.Fatalf("provider profile list is missing the caller's own profile: %s", listBody)
	}

	// --- A cannot update/connect/disconnect/test B's profile ---
	assertStatusMethod(t, client, http.MethodPatch, srv.URL+"/api/v1/provider-profiles/"+profB, cookieA,
		strings.NewReader(`{"displayName":"pwned","enabled":true}`), http.StatusNotFound)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/provider-profiles/"+profB+"/connect", cookieA, nil, http.StatusNotFound)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/provider-profiles/"+profB+"/disconnect", cookieA, nil, http.StatusNotFound)
	assertStatusMethod(t, client, http.MethodPost, srv.URL+"/api/v1/provider-profiles/"+profB+"/test", cookieA, nil, http.StatusNotFound)

	// --- B's own profile is unaffected by A's rejected attempts ---
	assertStatus(t, client, srv.URL+"/api/v1/provider-profiles/"+profB, cookieB, http.StatusOK)

	// --- no response body ever contains secret/ciphertext material ---
	for _, body := range []string{listBody, getBody(t, client, srv.URL+"/api/v1/provider-profiles/"+profA, cookieA)} {
		lower := strings.ToLower(body)
		if strings.Contains(lower, "secret") || strings.Contains(lower, "ciphertext") {
			t.Fatalf("provider profile response leaked secret material: %s", body)
		}
	}
}

func createProfile(t *testing.T, client *http.Client, baseURL string, cookie *http.Cookie) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"provider": "anthropic", "harness": "claude-code", "displayName": "My Claude"})
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/provider-profiles", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create profile: status %d", resp.StatusCode)
	}
	var out struct {
		Profile struct {
			ID string `json:"id"`
		} `json:"profile"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if out.Profile.ID == "" {
		t.Fatal("create profile: empty id")
	}
	return out.Profile.ID
}

func assertStatusMethod(t *testing.T, client *http.Client, method, url string, cookie *http.Cookie, body *strings.Reader, want int) {
	t.Helper()
	var req *http.Request
	var err error
	if body == nil {
		req, err = http.NewRequest(method, url, http.NoBody)
	} else {
		req, err = http.NewRequest(method, url, body)
		req.Header.Set("Content-Type", "application/json")
	}
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status = %d, want %d", method, url, resp.StatusCode, want)
	}
}
