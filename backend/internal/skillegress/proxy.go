package skillegress

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// proxy.go — the only route out of a skill container.
//
// # What it decides, and what it cannot
//
// It decides which (scheme, host, port) a connection may be made to, and it
// decides which ADDRESS that name is allowed to resolve to. It does not inspect
// payloads and does not know what the client does with the bytes. That is why
// net.active_scan is a separate capability: "may open a connection to this
// host" and "may probe this host for weaknesses" are different permissions and
// only the first is expressible here.
//
// # Rebinding
//
// The proxy resolves the host ITSELF, checks every returned address against the
// blocked ranges, and then dials the checked address rather than the name. A
// name that passed the allowlist and resolves to 169.254.169.254 is refused at
// the moment of connection; a second lookup that would have returned something
// different never happens, because the dial does not re-resolve.
//
// # Redirects
//
// The proxy does not follow them. A 30x reaches the client, and the client's
// next request comes back through here and is checked like any other. For a
// CONNECT tunnel there is nothing to follow — the proxy sees one authority and
// authorizes that one.

// Decision is one authorization the proxy made, kept as evidence. It records
// destinations and outcomes, never payloads and never credentials.
type Decision struct {
	At       time.Time `json:"at"`
	Method   string    `json:"method"`
	Scheme   string    `json:"scheme"`
	Host     string    `json:"host"`
	Port     int       `json:"port"`
	Resolved string    `json:"resolved,omitempty"`
	Allowed  bool      `json:"allowed"`
	Reason   string    `json:"reason,omitempty"`
}

// Recorder collects decisions. It is an interface so the proxy binary can write
// them to a file and a test can hold them in memory.
type Recorder interface {
	Record(Decision)
}

// MemoryRecorder keeps decisions in memory, safely for concurrent use.
type MemoryRecorder struct {
	mu        sync.Mutex
	decisions []Decision
}

// Record appends one decision.
func (m *MemoryRecorder) Record(d Decision) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decisions = append(m.decisions, d)
}

// Decisions returns a copy of what has been recorded.
func (m *MemoryRecorder) Decisions() []Decision {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Decision(nil), m.decisions...)
}

// Resolver looks up a host. It is an interface so a test can simulate a
// rebinding answer without controlling DNS.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

type netResolver struct{}

func (netResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// Proxy enforces one policy.
type Proxy struct {
	policy   Policy
	resolver Resolver
	recorder Recorder
	now      func() time.Time
	// dialTimeout bounds one upstream connection.
	dialTimeout time.Duration
	// blocked are the address ranges no grant may reach. It is a field rather
	// than a package global so this package's own tests can stand a synthetic
	// upstream on loopback -- which the production list blocks, correctly.
	//
	// It is UNEXPORTED and has no setter: nothing outside this package can
	// narrow it, and TestProxy_DefaultsBlockLoopbackAndMetadata asserts the
	// default list is the one a real proxy gets.
	blocked []*net.IPNet
	// permitted are private ranges this policy explicitly re-opened. They are
	// checked AFTER the allowlist, so an exception widens which addresses a
	// GRANTED name may resolve to and never which names may be reached.
	permitted []*net.IPNet
}

// NewProxy builds a proxy for one policy.
func NewProxy(policy Policy, recorder Recorder) *Proxy {
	if recorder == nil {
		recorder = &MemoryRecorder{}
	}
	// A malformed exception cannot widen anything: validate here and drop the
	// whole list if it does not parse, which leaves the full denylist in place.
	permitted, err := validatePermittedCIDRs(policy.PermittedPrivateCIDRs)
	if err != nil {
		permitted = nil
	}
	return &Proxy{
		policy: policy, resolver: netResolver{}, recorder: recorder,
		now: func() time.Time { return time.Now().UTC() }, dialTimeout: 10 * time.Second,
		blocked: blockedNets, permitted: permitted,
	}
}

// WithResolver replaces the resolver. Used by the rebinding tests, which need
// a lookup that answers differently than DNS would.
func (p *Proxy) WithResolver(r Resolver) *Proxy {
	p.resolver = r
	return p
}

// ServeHTTP handles both a CONNECT tunnel and a plain forwarded request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleForward(w, r)
}

// authorize is the one place a destination is decided. Every path goes through
// it, so there is no request shape that reaches an upstream unchecked.
func (p *Proxy) authorize(ctx context.Context, method string, scheme Scheme, host string, port int) (net.IP, error) {
	record := func(resolved string, allowed bool, reason string) {
		p.recorder.Record(Decision{
			At: p.now(), Method: method, Scheme: string(scheme), Host: host, Port: port,
			Resolved: resolved, Allowed: allowed, Reason: reason,
		})
	}
	// The window is checked per request, so a run that outlives its lease loses
	// the network mid-run rather than keeping it to the end.
	if p.policy.Expired(p.now()) {
		record("", false, "lease expired")
		return nil, fmt.Errorf("egress lease expired")
	}
	if !p.policy.Allows(scheme, host, port) {
		record("", false, "destination not in the allowlist")
		return nil, fmt.Errorf("%s://%s:%d is not in this run's allowlist", scheme, host, port)
	}

	// Resolve here, once, and dial the ADDRESS. A name that passes the
	// allowlist and points at a blocked range is refused now; a second lookup
	// that would answer differently never happens.
	lookupCtx, cancel := context.WithTimeout(ctx, p.dialTimeout)
	defer cancel()
	ips, err := p.resolver.LookupIP(lookupCtx, host)
	if err != nil || len(ips) == 0 {
		record("", false, "host does not resolve")
		return nil, fmt.Errorf("%s does not resolve", host)
	}
	// EVERY answer must be acceptable, not just one. A name that resolves to
	// both a public address and 169.254.169.254 is refused: allowing it would
	// leave which one gets used to chance.
	for _, ip := range ips {
		if reason, blocked := blockedIn(p.blocked, ip); blocked && !p.permits(ip) {
			record(ip.String(), false, reason)
			return nil, fmt.Errorf("%s resolves to %s: %s", host, ip, reason)
		}
	}
	chosen := ips[0]
	record(chosen.String(), true, "")
	return chosen, nil
}

// permits reports whether an otherwise-blocked address falls in a range this
// policy explicitly re-opened. It never applies to link-local: those ranges are
// rejected when the exception is validated, so nothing here can reach the
// metadata address.
func (p *Proxy) permits(ip net.IP) bool {
	for _, network := range p.permitted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitAuthority(r.Host, SchemeHTTPS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ip, err := p.authorize(r.Context(), http.MethodConnect, SchemeHTTPS, host, port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)), p.dialTimeout)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = upstream.Close() }()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tunnelling unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, "tunnelling failed", http.StatusInternalServerError)
		return
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, client) }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, upstream) }()
	wg.Wait()
}

func (p *Proxy) handleForward(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Host == "" {
		http.Error(w, "this is a forward proxy; send an absolute-form request", http.StatusBadRequest)
		return
	}
	scheme := Scheme(strings.ToLower(r.URL.Scheme))
	if !scheme.valid() {
		http.Error(w, "scheme is not http or https", http.StatusBadRequest)
		return
	}
	host, port, err := splitAuthority(r.URL.Host, scheme)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ip, err := p.authorize(r.Context(), r.Method, scheme, host, port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	// Dial the checked address, and never re-resolve. The Host header keeps
	// the name so the upstream serves the right site.
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: p.dialTimeout}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		},
		// A redirect is not followed here; it is returned to the client, whose
		// next request comes back through this proxy and is checked again.
		DisableKeepAlives: true,
	}
	outbound := r.Clone(r.Context())
	outbound.RequestURI = ""
	// Hop-by-hop headers must not be forwarded, and Proxy-Authorization in
	// particular must never reach an upstream.
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection",
		"Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		outbound.Header.Del(h)
	}
	resp, err := transport.RoundTrip(outbound)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func splitAuthority(authority string, scheme Scheme) (string, int, error) {
	host, rawPort, err := net.SplitHostPort(authority)
	if err != nil {
		// No port in the authority: the scheme implies one. This is the normal
		// shape of "http://host/", not a failure.
		return strings.ToLower(authority), scheme.DefaultPort(), nil //nolint:nilerr // a missing port is not an error.
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port %q is not valid", rawPort)
	}
	return strings.ToLower(host), port, nil
}
