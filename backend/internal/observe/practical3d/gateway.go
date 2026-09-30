package practical3d

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sync"
	"time"
)

// DaemonGateway is the only route from an agent to its position's AO daemon.
// The daemon's API also serves Project Memory (items, knowledge, graph,
// manifests) and session views that carry the worker's prompt, i.e. the
// ASSISTED attachment: an agent reaching those would receive Project Memory
// outside the frozen treatment (in OFF, or in a role whose cell is absent).
// The gateway forwards only what AO's own agent-side CLI needs (discovery
// probes, hooks, review submission) and refuses and records everything else.
type DaemonGateway struct {
	DaemonPort int

	mu      sync.Mutex
	refused []string
	srv     *http.Server
}

// gatewayAllow is the closed set of agent-to-daemon requests: the AO CLI's
// daemon discovery (healthz/readyz), the provider hooks (usage subject and
// session/review activity) and `ao review submit` / `ao review list`.
var gatewayAllow = []struct {
	method string
	path   *regexp.Regexp
}{
	{http.MethodGet, regexp.MustCompile(`^/healthz$`)},
	{http.MethodGet, regexp.MustCompile(`^/readyz$`)},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/usage/subject-hook$`)},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/sessions/[A-Za-z0-9._-]+/activity$`)},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/reviews/[A-Za-z0-9._-]+/activity$`)},
	{http.MethodPost, regexp.MustCompile(`^/api/v1/sessions/[A-Za-z0-9._-]+/reviews/submit$`)},
	{http.MethodGet, regexp.MustCompile(`^/api/v1/sessions/[A-Za-z0-9._-]+/reviews$`)},
}

func gatewayAllowed(method, path string) bool {
	for _, a := range gatewayAllow {
		if a.method == method && a.path.MatchString(path) {
			return true
		}
	}
	return false
}

// Start listens on a loopback port and returns it.
func (g *DaemonGateway) Start() (int, error) {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", g.DaemonPort)}
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = &http.Transport{Proxy: nil}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	g.srv = &http.Server{ReadHeaderTimeout: 30 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" && r.Method != http.MethodGet || !gatewayAllowed(r.Method, r.URL.Path) {
			g.mu.Lock()
			g.refused = append(g.refused, r.Method+" "+r.URL.Path)
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"forbidden","message":"ao 3d-practical: this daemon endpoint is not available to agents"}`))
			return
		}
		rp.ServeHTTP(w, r)
	})}
	go func() { _ = g.srv.Serve(ln) }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("gateway listener has no TCP address")
	}
	return addr.Port, nil
}

// Close stops the gateway.
func (g *DaemonGateway) Close() {
	if g.srv != nil {
		_ = g.srv.Close()
	}
}

// Refused returns the refused requests, in order.
func (g *DaemonGateway) Refused() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string{}, g.refused...)
}

// WriteGatewayRunFile writes the run file agents discover the daemon by
// (AO_RUN_FILE): the daemon's own run file with the gateway's port, so the
// CLI's identity probes (pid, instance, data dir) still verify through it.
func WriteGatewayRunFile(daemonRunFile, out string, gatewayPort int) error {
	raw, err := os.ReadFile(daemonRunFile)
	if err != nil {
		return err
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		return fmt.Errorf("daemon run file: %w", err)
	}
	if _, ok := info["port"]; !ok {
		return fmt.Errorf("daemon run file has no port")
	}
	info["port"] = gatewayPort
	b, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o600)
}
