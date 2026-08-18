package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestExecutionPolicyOwnershipIDOR is Checkpoint 8P-C's security test
// (checkpoint brief §33): with AO_TRUSTED_LOCAL_MODE off, User A must never
// be able to read User B's execution policy, and PUTting a priority list
// that references User B's ProviderProfile id must be rejected without
// leaking whether that id exists (404-shaped, matching provider_profiles.go's
// IDOR precedent).
func TestExecutionPolicyOwnershipIDOR(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	authMgr := authsvc.New(store, func() time.Time { return time.Now().UTC() })
	profileSvc := &providerprofile.Service{Store: store, DataDir: fixedDataDir(t.TempDir())}
	policySvc := &executionpolicy.Service{Store: store}

	cfg := config.Config{TrustedLocalMode: false}
	deps := APIDeps{Auth: authMgr, ProviderProfiles: profileSvc, ExecutionPolicy: policySvc}
	router := NewRouterWithControl(cfg, discardLogger(), nil, deps, ControlDeps{})
	srv := httptest.NewServer(router)
	defer srv.Close()

	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "alice-ep@example.com", Username: "alice-ep", Password: "correct-horse-a"}); err != nil {
		t.Fatalf("create user A: %v", err)
	}
	if _, err := authMgr.CreateUser(t.Context(), authsvc.CreateUserInput{Email: "bob-ep@example.com", Username: "bob-ep", Password: "correct-horse-b"}); err != nil {
		t.Fatalf("create user B: %v", err)
	}

	client := &http.Client{}
	_, cookieA := loginOK(t, srv.URL, "alice-ep@example.com", "correct-horse-a")
	_, cookieB := loginOK(t, srv.URL, "bob-ep@example.com", "correct-horse-b")

	profA := createProfile(t, client, srv.URL, cookieA)
	profB := createProfile(t, client, srv.URL, cookieB)

	// --- A's own GET never mentions B's profile id ---
	bodyA := getBody(t, client, srv.URL+"/api/v1/execution-policy", cookieA)
	if strings.Contains(bodyA, profB) {
		t.Fatalf("execution policy GET leaked another user's profile id: %s", bodyA)
	}

	// --- PUT referencing another user's own profile is rejected, not silently accepted ---
	putBody, _ := json.Marshal(map[string]any{
		"autonomousMode":           false,
		"plannerPriority":          []string{},
		"workerPriority":           []string{profB},
		"reviewerPriority":         []string{},
		"decisionResolverPriority": []string{},
		"fallbackBehavior":         "use_next_available",
		"reviewIndependence":       "require_different_provider",
	})
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/api/v1/execution-policy", strings.NewReader(string(putBody)))
	if err != nil {
		t.Fatalf("build PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookieA)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT execution-policy: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("PUT execution-policy with a foreign profile id unexpectedly succeeded (status %d)", resp.StatusCode)
	}

	// --- A can legitimately reference its OWN profile ---
	putOwnBody, _ := json.Marshal(map[string]any{
		"autonomousMode":           false,
		"plannerPriority":          []string{},
		"workerPriority":           []string{profA},
		"reviewerPriority":         []string{},
		"decisionResolverPriority": []string{},
		"fallbackBehavior":         "use_next_available",
		"reviewIndependence":       "require_different_provider",
	})
	assertStatusMethod(t, client, http.MethodPut, srv.URL+"/api/v1/execution-policy", cookieA, strings.NewReader(string(putOwnBody)), http.StatusOK)

	// --- B's own policy is unaffected by A's activity and never shows A's profile ---
	bodyB := getBody(t, client, srv.URL+"/api/v1/execution-policy", cookieB)
	if strings.Contains(bodyB, profA) {
		t.Fatalf("execution policy GET leaked another user's profile id: %s", bodyB)
	}
}
