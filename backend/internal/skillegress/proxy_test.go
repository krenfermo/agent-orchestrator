package skillegress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillscope"
)

// Everything here is local and synthetic. The "upstreams" are httptest servers
// on loopback; no real host is ever contacted.

func testScope() Scope {
	return skillscope.Scope{
		TenantID: "tenant-a", ProjectID: "medusa",
		SkillID: "security-audit", Version: "0.1.0", ModeID: "dependencies",
	}
}

// fixedResolver answers lookups from a table, so a test can simulate a name
// that points wherever it likes — including at a blocked range, which is what
// DNS rebinding looks like from the proxy's side.
type fixedResolver map[string][]net.IP

func (f fixedResolver) LookupIP(_ context.Context, host string) ([]net.IP, error) {
	ips, ok := f[strings.ToLower(host)]
	if !ok || len(ips) == 0 {
		return nil, errors.New("no such host")
	}
	return ips, nil
}

// upstream starts a local server and returns its host and port.
func upstream(t *testing.T, body string) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://denied.example.test/landing", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}
	return srv, u.Hostname(), port
}

// proxyFor starts the proxy in front of a policy, with a resolver that maps
// granted names onto the local upstream.
func proxyFor(t *testing.T, policy Policy, resolver Resolver) (*httptest.Server, *MemoryRecorder) {
	t.Helper()
	rec := &MemoryRecorder{}
	p := NewProxy(policy, rec)
	if resolver != nil {
		p.WithResolver(resolver)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, rec
}

// reachableProxyFor is proxyFor with loopback removed from the blocked ranges,
// so a test can stand a synthetic upstream on 127.0.0.1. Every OTHER blocked
// range is kept, so the rebinding cases still exercise the real list — and
// TestProxy_DefaultsBlockLoopbackAndMetadata asserts a real proxy blocks
// loopback too.
func reachableProxyFor(t *testing.T, policy Policy, resolver Resolver) (*httptest.Server, *MemoryRecorder) {
	t.Helper()
	rec := &MemoryRecorder{}
	p := NewProxy(policy, rec).WithResolver(resolver)
	p.blocked = withoutLoopback(blockedNets)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, rec
}

func withoutLoopback(nets []*net.IPNet) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(nets))
	for _, n := range nets {
		if n.String() == "127.0.0.0/8" || n.String() == "::1/128" {
			continue
		}
		out = append(out, n)
	}
	return out
}

// clientThrough builds a client that reaches everything through the proxy.
func clientThrough(t *testing.T, proxy *httptest.Server) *http.Client {
	t.Helper()
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   10 * time.Second,
		// Redirects are NOT followed automatically: each test decides, so a
		// redirect out of scope is observed rather than silently retried.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func policyWith(t *testing.T, ttl time.Duration, raw ...string) Policy {
	t.Helper()
	dests := make([]Destination, 0, len(raw))
	for _, r := range raw {
		d, err := ParseDestination(r)
		if err != nil {
			t.Fatalf("ParseDestination(%q): %v", r, err)
		}
		dests = append(dests, d)
	}
	p, err := PolicyFor(Lease{
		ID: "lease-1", Scope: testScope(), RunID: "run-1", AttemptID: "attempt-1",
		Destinations: dests, IssuedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(ttl),
	})
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	return p
}

// The allowed destination works, and the decision is recorded as evidence.
func TestProxy_AllowsAGrantedDestination(t *testing.T) {
	_, upHost, upPort := upstream(t, "ALLOWED-BODY")
	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+strconv.Itoa(upPort))
	proxy, rec := reachableProxyFor(t, policy, fixedResolver{
		"allowed.example.test": {net.ParseIP(upHost)},
	})

	resp, err := clientThrough(t, proxy).Get("http://allowed.example.test:" + strconv.Itoa(upPort) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ALLOWED-BODY" {
		t.Fatalf("body = %q", body)
	}

	decisions := rec.Decisions()
	if len(decisions) != 1 || !decisions[0].Allowed {
		t.Fatalf("decisions = %+v", decisions)
	}
	if decisions[0].Host != "allowed.example.test" || decisions[0].Resolved == "" {
		t.Fatalf("the decision does not record what was reached: %+v", decisions[0])
	}
}

// NEGATIVE: a host nobody granted.
func TestProxy_RefusesAnUngrantedHost(t *testing.T) {
	_, upHost, upPort := upstream(t, "SHOULD NOT BE REACHED")
	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+strconv.Itoa(upPort))
	proxy, rec := reachableProxyFor(t, policy, fixedResolver{
		"allowed.example.test": {net.ParseIP(upHost)},
		"denied.example.test":  {net.ParseIP(upHost)},
	})

	resp, err := clientThrough(t, proxy).Get("http://denied.example.test:" + strconv.Itoa(upPort) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "SHOULD NOT BE REACHED") {
		t.Fatal("the refused upstream was reached anyway")
	}
	for _, d := range rec.Decisions() {
		if d.Allowed {
			t.Fatalf("a refused host produced an allow: %+v", d)
		}
	}
}

// NEGATIVE: the right host on a port nobody granted. "May reach this host" is
// not "may reach every service on it".
func TestProxy_RefusesAnUngrantedPort(t *testing.T) {
	_, upHost, upPort := upstream(t, "BODY")
	policy := policyWith(t, time.Hour, "http://allowed.example.test:9999")
	proxy, _ := reachableProxyFor(t, policy, fixedResolver{"allowed.example.test": {net.ParseIP(upHost)}})

	resp, err := clientThrough(t, proxy).Get("http://allowed.example.test:" + strconv.Itoa(upPort) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// NEGATIVE: the right host and port on a scheme nobody granted. "May talk TLS
// to this host" is not "may send it cleartext".
func TestProxy_RefusesAnUngrantedScheme(t *testing.T) {
	_, upHost, upPort := upstream(t, "BODY")
	port := strconv.Itoa(upPort)
	policy := policyWith(t, time.Hour, "https://allowed.example.test:"+port)
	proxy, _ := reachableProxyFor(t, policy, fixedResolver{"allowed.example.test": {net.ParseIP(upHost)}})

	resp, err := clientThrough(t, proxy).Get("http://allowed.example.test:" + port + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// NEGATIVE: DNS rebinding. The name is granted; it resolves to the cloud
// metadata address. The refusal happens at connection time, against the
// RESOLVED address, which is the only place it can be caught.
func TestProxy_RefusesARebindingAnswer(t *testing.T) {
	cases := map[string]string{
		"cloud metadata": "169.254.169.254",
		"loopback":       "127.0.0.1",
		"RFC1918":        "10.1.2.3",
		"link-local v6":  "fe80::1",
		"unique local":   "fd00::1",
		"CGNAT":          "100.64.1.1",
	}
	for name, addr := range cases {
		t.Run(name, func(t *testing.T) {
			policy := policyWith(t, time.Hour, "http://allowed.example.test:80")
			proxy, rec := proxyFor(t, policy, fixedResolver{
				"allowed.example.test": {net.ParseIP(addr)},
			})
			resp, err := clientThrough(t, proxy).Get("http://allowed.example.test/")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", resp.StatusCode)
			}
			decisions := rec.Decisions()
			if len(decisions) != 1 || decisions[0].Allowed {
				t.Fatalf("decisions = %+v", decisions)
			}
			if decisions[0].Resolved != addr {
				t.Fatalf("the decision does not record the address it refused: %+v", decisions[0])
			}
			if !strings.Contains(decisions[0].Reason, "blocked range") {
				t.Fatalf("reason = %q", decisions[0].Reason)
			}
		})
	}
}

// NEGATIVE: a name that resolves to BOTH a usable address and a blocked one is
// refused. Allowing it would leave which one gets used to chance.
func TestProxy_RefusesAMixedResolution(t *testing.T) {
	_, upHost, upPort := upstream(t, "BODY")
	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+strconv.Itoa(upPort))
	proxy, _ := reachableProxyFor(t, policy, fixedResolver{
		"allowed.example.test": {net.ParseIP(upHost), net.ParseIP("169.254.169.254")},
	})
	resp, err := clientThrough(t, proxy).Get("http://allowed.example.test:" + strconv.Itoa(upPort) + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// NEGATIVE: a redirect out of scope. The proxy does not follow it; the client's
// next request comes back through and is refused.
func TestProxy_ARedirectOutOfScopeIsRefusedOnTheNextHop(t *testing.T) {
	_, upHost, upPort := upstream(t, "BODY")
	port := strconv.Itoa(upPort)
	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+port)
	proxy, _ := reachableProxyFor(t, policy, fixedResolver{
		"allowed.example.test": {net.ParseIP(upHost)},
		"denied.example.test":  {net.ParseIP(upHost)},
	})
	client := clientThrough(t, proxy)

	resp, err := client.Get("http://allowed.example.test:" + port + "/redirect")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "denied.example.test") {
		t.Fatalf("location = %q", location)
	}
	// Following it by hand is what a client would do, and it is refused.
	next, err := client.Get(location)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer func() { _ = next.Body.Close() }()
	if next.StatusCode != http.StatusForbidden {
		t.Fatalf("the redirect target was reachable: %d", next.StatusCode)
	}
}

// NEGATIVE: an expired lease. The window is checked per request, so a run that
// outlives its lease loses the network mid-run rather than keeping it.
func TestProxy_RefusesAfterTheLeaseExpires(t *testing.T) {
	_, upHost, upPort := upstream(t, "BODY")
	port := strconv.Itoa(upPort)
	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+port)
	rec := &MemoryRecorder{}
	p := NewProxy(policy, rec).WithResolver(fixedResolver{
		"allowed.example.test": {net.ParseIP(upHost)},
	})
	// Move the clock past the lease.
	p.now = func() time.Time { return time.Now().UTC().Add(2 * time.Hour) }
	srv := httptest.NewServer(p)
	defer srv.Close()

	proxyURL, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://allowed.example.test:" + port + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if d := rec.Decisions(); len(d) != 1 || !strings.Contains(d[0].Reason, "expired") {
		t.Fatalf("decisions = %+v", d)
	}
}

// NEGATIVE: a CONNECT tunnel to an ungranted host. The proxy sees only the
// authority, and that is what it authorizes.
func TestProxy_RefusesAnUngrantedConnect(t *testing.T) {
	policy := policyWith(t, time.Hour, "https://allowed.example.test:443")
	proxy, rec := proxyFor(t, policy, fixedResolver{
		"allowed.example.test": {net.ParseIP("93.184.216.34")},
		"denied.example.test":  {net.ParseIP("93.184.216.34")},
	})

	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(proxy.URL, "http://"), 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("CONNECT denied.example.test:443 HTTP/1.1\r\n" +
		"Host: denied.example.test:443\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Fatalf("CONNECT response = %q", buf[:n])
	}
	for _, d := range rec.Decisions() {
		if d.Allowed {
			t.Fatalf("an ungranted CONNECT was allowed: %+v", d)
		}
	}
}

// A name that does not resolve is refused rather than dialled.
func TestProxy_RefusesAnUnresolvableHost(t *testing.T) {
	policy := policyWith(t, time.Hour, "http://allowed.example.test:80")
	proxy, rec := proxyFor(t, policy, fixedResolver{})

	resp, err := clientThrough(t, proxy).Get("http://allowed.example.test/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if d := rec.Decisions(); len(d) != 1 || !strings.Contains(d[0].Reason, "does not resolve") {
		t.Fatalf("decisions = %+v", d)
	}
}

// Proxy-Authorization must never reach an upstream.
func TestProxy_DoesNotForwardProxyCredentials(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	policy := policyWith(t, time.Hour, "http://allowed.example.test:"+strconv.Itoa(port))
	proxy, _ := reachableProxyFor(t, policy, fixedResolver{"allowed.example.test": {net.ParseIP(u.Hostname())}})

	req, _ := http.NewRequest(http.MethodGet,
		"http://allowed.example.test:"+strconv.Itoa(port)+"/", nil)
	req.Header.Set("Proxy-Authorization", "Basic SYNTHETIC")
	req.Header.Set("Proxy-Connection", "keep-alive")
	resp, err := clientThrough(t, proxy).Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if seen.Get("Proxy-Authorization") != "" {
		t.Fatal("Proxy-Authorization reached the upstream")
	}
	if seen.Get("Proxy-Connection") != "" {
		t.Fatal("a hop-by-hop header reached the upstream")
	}
}

// The test seam narrows; the production default must not. This asserts a proxy
// built the way the runner builds one blocks loopback and the metadata address,
// so no test convenience leaked into what ships.
func TestProxy_DefaultsBlockLoopbackAndMetadata(t *testing.T) {
	p := NewProxy(policyWith(t, time.Hour, "http://allowed.example.test:80"), nil)

	for _, addr := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "192.168.1.1"} {
		if _, blocked := blockedIn(p.blocked, net.ParseIP(addr)); !blocked {
			t.Fatalf("a default proxy does not block %s", addr)
		}
	}
	// And the exported helper, which is what any other caller would use.
	if _, blocked := BlockedAddress(net.ParseIP("169.254.169.254")); !blocked {
		t.Fatal("BlockedAddress does not block the cloud metadata address")
	}
	if _, blocked := BlockedAddress(net.ParseIP("93.184.216.34")); blocked {
		t.Fatal("BlockedAddress blocks an ordinary public address")
	}
	if _, blocked := BlockedAddress(nil); !blocked {
		t.Fatal("BlockedAddress accepts a nil address")
	}
}
