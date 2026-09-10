package skillregistry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress"
)

// netpolicy.go -- the exact set of addresses one configured registry may be
// reached at, and nothing else.
//
// # Why a registry is an ORIGIN and not a URL
//
// A configured registry authorizes exactly one scheme, one host and one port.
// Every request the provider makes is built from that origin plus a path this
// package chose; a URL that arrived in a registry response is never followed.
// That is the difference between a client and an SSRF gadget: the release
// metadata a registry serves is attacker-controlled the moment the registry is
// compromised, and a client that fetched "the URL the release named" would hand
// a compromised registry the daemon's own network position.
//
// # Why the address check runs at dial time
//
// The origin says which NAME may be reached. It cannot say what that name
// resolves to, and a name that answers 169.254.169.254 on the second lookup is
// the whole of DNS rebinding. So the check runs against the resolved address,
// once, and the connection is made to THAT address rather than to the name --
// there is no second lookup for an attacker to answer differently.
//
// # Why the blocked ranges are not defined here
//
// They are internal/skillegress's, exported for this caller. AO already
// decided which addresses a skill's traffic may reach; the daemon's own
// registry traffic reaching a range the skill proxy refuses would be a second,
// weaker policy in the half nobody audited.

// ErrNetworkPolicy marks a destination this registry may not be reached at.
var ErrNetworkPolicy = errors.New("skillregistry: refused by the registry network policy")

// Origin is the one scheme+host+port a registry authorizes.
type Origin struct {
	Scheme string
	Host   string
	Port   int
}

// String renders the origin the way it is configured.
func (o Origin) String() string { return fmt.Sprintf("%s://%s:%d", o.Scheme, o.Host, o.Port) }

// Authority is the host:port form a request carries.
func (o Origin) Authority() string { return net.JoinHostPort(o.Host, strconv.Itoa(o.Port)) }

// Matches reports whether a URL is the SAME origin. It is exact on all three
// components: a redirect from https://reg:443 to http://reg:80 is a different
// origin, because "may talk TLS to this host" and "may send cleartext to it"
// are different permissions -- and the cleartext one would carry the
// credential in the clear.
func (o Origin) Matches(u *url.URL) bool {
	if u == nil {
		return false
	}
	other, err := OriginOf(u)
	if err != nil {
		return false
	}
	return other == o
}

// OriginOf derives the origin of a parsed URL, filling in the scheme's default
// port.
func OriginOf(u *url.URL) (Origin, error) {
	if u == nil {
		return Origin{}, fmt.Errorf("%w: no URL", ErrNetworkPolicy)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" {
		// http, file, unix, gopher, data: all refused by the same rule, and
		// the message names the only one that is allowed rather than
		// enumerating the ones that are not.
		return Origin{}, fmt.Errorf("%w: scheme %q is not https; a private registry carries a "+
			"credential and serves code, and neither may cross a cleartext or local-file transport",
			ErrNetworkPolicy, u.Scheme)
	}
	if u.User != nil {
		return Origin{}, fmt.Errorf("%w: the URL carries credentials; a registry credential is the "+
			"NAME of a sealed secret, never a value in a URL", ErrNetworkPolicy)
	}
	host := strings.ToLower(u.Hostname())
	port := 443
	if raw := u.Port(); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			return Origin{}, fmt.Errorf("%w: port %q is not a port", ErrNetworkPolicy, raw)
		}
		port = p
	}
	if err := validateRegistryHost(host); err != nil {
		return Origin{}, err
	}
	return Origin{Scheme: scheme, Host: host, Port: port}, nil
}

// ParseBaseURL turns a configured baseURL into an origin and a base path.
//
// The path is kept because a registry may legitimately be mounted under a
// prefix ("https://artifacts.corp/skills"). It is normalized to have no
// trailing slash, and a query or fragment is refused: a base URL that carried
// one would silently change every request this package builds.
func ParseBaseURL(raw string) (Origin, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Origin{}, "", fmt.Errorf("%w: a baseURL is required", ErrNetworkPolicy)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return Origin{}, "", fmt.Errorf("%w: %q is not a URL", ErrNetworkPolicy, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Origin{}, "", fmt.Errorf("%w: a baseURL carries no query or fragment; one that did "+
			"would change every request built from it", ErrNetworkPolicy)
	}
	origin, err := OriginOf(u)
	if err != nil {
		return Origin{}, "", err
	}
	basePath := strings.TrimSuffix(u.EscapedPath(), "/")
	if strings.Contains(basePath, "..") {
		return Origin{}, "", fmt.Errorf("%w: a baseURL path must not traverse upward", ErrNetworkPolicy)
	}
	return origin, basePath, nil
}

// validateRegistryHost refuses anything that is not a plain, fully-qualified
// DNS name.
//
// An IP literal is refused for the reason skillegress refuses one in a grant:
// the configuration is meant to be read by the person approving it, and "the
// registry is 10.4.2.9" is not a statement anybody can evaluate a year later.
// An operator whose registry has no name can give it one in /etc/hosts; that
// is a smaller cost than a configuration nobody can review.
func validateRegistryHost(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%w: a registry needs a host", ErrNetworkPolicy)
	}
	if len(host) > 253 {
		return fmt.Errorf("%w: host %q is longer than 253 characters", ErrNetworkPolicy, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return fmt.Errorf("%w: %q is an IP literal; configure a DNS name so the registry entry says "+
			"what it reaches", ErrNetworkPolicy, host)
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return fmt.Errorf("%w: host %q is malformed", ErrNetworkPolicy, host)
	}
	if !strings.Contains(host, ".") {
		// "localhost" lands here, and that is the intent: a bare label
		// resolves differently depending on the host's search domains, so what
		// it reaches is not knowable from the configuration.
		return fmt.Errorf("%w: host %q must be fully qualified", ErrNetworkPolicy, host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: host %q has a malformed label", ErrNetworkPolicy, host)
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case r == '-' && i > 0 && i < len(label)-1:
			default:
				return fmt.Errorf("%w: host %q contains %q, which is not a DNS character",
					ErrNetworkPolicy, host, string(r))
			}
		}
	}
	return nil
}

// Resolver looks up a host. It is an interface for the same reason
// skillegress.Resolver is: a rebinding answer has to be testable without
// controlling DNS.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

type systemResolver struct{}

func (systemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
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

// addressGuard decides which address one registry's traffic may be dialed at.
type addressGuard struct {
	origin    Origin
	resolver  Resolver
	permitted []*net.IPNet
}

// resolve picks the address to dial, or refuses.
//
// EVERY answer must be acceptable, not just one. A name that resolves to both a
// public address and 169.254.169.254 is refused outright: allowing it would
// leave which one gets used to chance, and "usually the safe one" is not a
// security control.
func (g addressGuard) resolve(ctx context.Context, host string) (net.IP, error) {
	if !strings.EqualFold(host, g.origin.Host) {
		return nil, fmt.Errorf("%w: %s is not this registry's host (%s)",
			ErrNetworkPolicy, host, g.origin.Host)
	}
	ips, err := g.resolver.LookupIP(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("%w: %s does not resolve", ErrRegistryUnreachable, host)
	}
	for _, ip := range ips {
		if reason, blocked := skillegress.AddressBlocked(ip, g.permitted); blocked {
			return nil, fmt.Errorf("%w: %s resolves to %s: %s", ErrNetworkPolicy, host, ip, reason)
		}
	}
	return ips[0], nil
}

// dialContext is the transport's only way out. It refuses any authority that is
// not this registry's origin, resolves once, and dials the ADDRESS -- so a
// second lookup that would answer differently never happens.
func (g addressGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %q is not host:port", ErrNetworkPolicy, addr)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port != g.origin.Port {
		return nil, fmt.Errorf("%w: port %s is not this registry's port (%d)",
			ErrNetworkPolicy, rawPort, g.origin.Port)
	}
	// An IP literal in the authority means something rewrote the request after
	// the origin check. Refuse rather than resolve it: the guard's whole
	// contract is that the name was approved.
	ip, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), rawPort))
}
