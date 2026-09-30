package practical3d

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Codex review R3 (P1): the daemon API serves Project Memory and session
// views with the worker's prompt; agents reach only what AO's agent-side CLI
// needs, and every refused request is recorded.
func TestDaemonGatewayAllowsOnlyAgentCLIEndpoints(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var hits []string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits = append(hits, r.Method+" "+r.URL.Path)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer daemon.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(daemon.URL, "http://"))
	port, _ := strconv.Atoi(portStr)
	g := &DaemonGateway{DaemonPort: port}
	gp, err := g.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	base := "http://127.0.0.1:" + strconv.Itoa(gp)
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodGet, "/healthz", true},
		{http.MethodGet, "/readyz", true},
		{http.MethodPost, "/api/v1/usage/subject-hook", true},
		{http.MethodPost, "/api/v1/sessions/practical-a-1/activity", true},
		{http.MethodPost, "/api/v1/reviews/r-1/activity", true},
		{http.MethodPost, "/api/v1/sessions/practical-a-1/reviews/submit", true},
		{http.MethodGet, "/api/v1/sessions/practical-a-1/reviews", true},
		{http.MethodGet, "/api/v1/projects/practical-a/memory/items", false},
		{http.MethodGet, "/api/v1/projects/practical-a/memory/knowledge", false},
		{http.MethodGet, "/api/v1/projects/practical-a/memory/graph/query", false},
		{http.MethodPost, "/api/v1/projects/practical-a/memory/rebuild", false},
		{http.MethodGet, "/api/v1/sessions/practical-a-1", false},
		{http.MethodGet, "/api/v1/sessions", false},
		{http.MethodGet, "/api/v1/workflows/wf-1", false},
		{http.MethodGet, "/api/v1/sessions/practical-a-1/../../projects/practical-a/memory/items", false},
	} {
		req, _ := http.NewRequest(tc.method, base+tc.path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.StatusCode != http.StatusForbidden; got != tc.allowed {
			t.Errorf("%s %s: allowed=%v status=%d", tc.method, tc.path, got, resp.StatusCode)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, h := range hits {
		if strings.Contains(h, "/memory") || h == "GET /api/v1/sessions/practical-a-1" {
			t.Errorf("daemon received %s", h)
		}
	}
	if n := len(g.Refused()); n < 7 {
		t.Errorf("refused requests recorded: %d", n)
	}
}

func TestGatewayRunFilePointsAgentsAtTheGateway(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "running.json")
	if err := os.WriteFile(src, []byte(`{"pid":42,"port":5000,"instanceId":"aod-1","dataDir":"/d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "gw.json")
	if err := WriteGatewayRunFile(src, out, 6000); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(out)
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatal(err)
	}
	if info["port"] != float64(6000) || info["pid"] != float64(42) || info["instanceId"] != "aod-1" {
		t.Fatalf("gateway run file %s", raw)
	}
}
