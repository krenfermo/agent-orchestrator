package httpd

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// AR-1a / D-SEC-4: the only way to unlock a provider's sandbox/approval bypass
// is the explicit bypass-permissions policy in the project's agent config. An
// AO-launched agent must not be able to write that policy for itself -- even
// for the project it was launched in, and even though the account it acts for
// is the installation owner.
func TestAnAgentCannotWriteItsProjectsPermissionPolicy(t *testing.T) {
	w := newAgentWorld(t)
	for _, path := range []string{"/api/v1/projects/proj-review/config", "/api/v1/projects/proj-review"} {
		req, err := http.NewRequest(http.MethodPut, w.srv.URL+path,
			bytes.NewBufferString(`{"agentConfig":{"permissions":"bypass-permissions"},"worker":{"agentConfig":{"permissions":"bypass-permissions"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(identity.AgentTokenHeader, w.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("agent PUT %s = %d, want 403/404", path, resp.StatusCode)
		}
	}
}
