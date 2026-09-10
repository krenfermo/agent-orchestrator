// Package skillegress is the network boundary for skill runs: an allowlist
// bound to one scope and one attempt, and a forward proxy that is the only
// route out of the container.
//
// # The boundary is the topology, not the proxy
//
// The skill container joins ONE Docker network, created --internal. Measured on
// this design: from inside it, an IP literal on the internet is "Network
// unreachable", the cloud-metadata address is "Network unreachable", and
// Docker's embedded resolver answers SERVFAIL for any external name. There is
// no route to bypass, so HTTP_PROXY is a convenience for well-behaved clients
// rather than the control — unsetting it inside the container restores nothing.
//
// The proxy sits on that internal network AND on a second one with egress. It
// is the only thing the skill can reach, and it enforces the allowlist.
//
// # What this package is not
//
// It is not a firewall, and it does not inspect payloads. It decides which
// destinations a connection may be made to, and it refuses everything else.
// net.active_scan is a separate capability with a separate control precisely
// because "may open a connection to this host" and "may probe this host for
// weaknesses" are different permissions.
package skillegress

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ErrInvalid marks a destination or grant that could not be enforced.
var ErrInvalid = errors.New("skillegress: invalid")

// Scheme is the protocol a destination may be reached over.
type Scheme string

const (
	// SchemeHTTPS is a CONNECT tunnel. The proxy sees the host and port and
	// nothing else, which is the point of TLS.
	SchemeHTTPS Scheme = "https"
	// SchemeHTTP is a plain forwarded request. The proxy sees every request
	// line, so a redirect to another host is checked like any other request.
	SchemeHTTP Scheme = "http"
)

func (s Scheme) valid() bool { return s == SchemeHTTPS || s == SchemeHTTP }

// DefaultPort is the port a scheme implies when none is given.
func (s Scheme) DefaultPort() int {
	if s == SchemeHTTP {
		return 80
	}
	return 443
}

// Destination is one place a run may connect to: exactly one scheme, one host
// and one port.
//
// There is deliberately no wildcard. A grant for "*.example.com" is a grant
// nobody can reason about — it covers hosts that do not exist yet, and the
// person approving it cannot enumerate what they approved.
type Destination struct {
	Scheme Scheme `json:"scheme"`
	// Host is a DNS name, lowercased. An IP literal is refused: an allowlist
	// written against an address rather than a name cannot be reasoned about
	// when the address moves, and it is the shape an exfiltration destination
	// takes.
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ParseDestination parses "scheme://host[:port]" into a checked Destination.
func ParseDestination(raw string) (Destination, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Destination{}, fmt.Errorf("%w: an empty destination allows nothing and says nothing", ErrInvalid)
	}
	if strings.Contains(trimmed, "*") {
		return Destination{}, fmt.Errorf("%w: %q contains a wildcard; a grant that covers hosts "+
			"that do not exist yet is one nobody can reason about", ErrInvalid, raw)
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" {
		return Destination{}, fmt.Errorf("%w: %q is not scheme://host[:port]", ErrInvalid, raw)
	}
	if u.Path != "" && u.Path != "/" {
		return Destination{}, fmt.Errorf("%w: %q carries a path; a destination is a host and a "+
			"port, and a path-scoped grant is not one this boundary can enforce", ErrInvalid, raw)
	}
	if u.User != nil {
		return Destination{}, fmt.Errorf("%w: %q carries credentials", ErrInvalid, raw)
	}
	scheme := Scheme(strings.ToLower(u.Scheme))
	if !scheme.valid() {
		return Destination{}, fmt.Errorf("%w: scheme %q is not http or https", ErrInvalid, u.Scheme)
	}

	host := strings.ToLower(u.Hostname())
	port := scheme.DefaultPort()
	if raw := u.Port(); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil {
			return Destination{}, fmt.Errorf("%w: port %q is not a number", ErrInvalid, raw)
		}
	}
	dest := Destination{Scheme: scheme, Host: host, Port: port}
	if err := dest.Validate(); err != nil {
		return Destination{}, err
	}
	return dest, nil
}

// Validate rejects a destination that could not be enforced or should not be
// reachable.
func (d Destination) Validate() error {
	if !d.Scheme.valid() {
		return fmt.Errorf("%w: scheme %q is not http or https", ErrInvalid, d.Scheme)
	}
	if d.Port < 1 || d.Port > 65535 {
		return fmt.Errorf("%w: port %d is out of range", ErrInvalid, d.Port)
	}
	if err := validateHostName(d.Host); err != nil {
		return err
	}
	return nil
}

// String renders the destination in the form it is granted in.
func (d Destination) String() string {
	return fmt.Sprintf("%s://%s:%d", d.Scheme, d.Host, d.Port)
}

// Authority is the host:port form a proxy request carries.
func (d Destination) Authority() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

// validateHostName rejects anything that is not a plain DNS name.
//
// An IP literal is refused on purpose. An allowlist entry is meant to be read
// by the person approving it, and "may reach 34.117.x.y" is not a statement
// anybody can evaluate. Refusing literals also removes the shape a
// exfiltration destination usually takes.
func validateHostName(host string) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("%w: a destination needs a host", ErrInvalid)
	}
	if len(host) > 253 {
		return fmt.Errorf("%w: host %q is longer than 253 characters", ErrInvalid, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return fmt.Errorf("%w: %q is an IP literal; grant a DNS name so the entry says what it "+
			"reaches", ErrInvalid, host)
	}
	if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
		return fmt.Errorf("%w: host %q is malformed", ErrInvalid, host)
	}
	if !strings.Contains(host, ".") {
		// A bare label resolves differently depending on the container's
		// search domains, so what it reaches is not knowable from the grant.
		return fmt.Errorf("%w: host %q must be fully qualified", ErrInvalid, host)
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("%w: host %q has a malformed label", ErrInvalid, host)
		}
		for i, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case r == '-' && i > 0 && i < len(label)-1:
			default:
				return fmt.Errorf("%w: host %q contains %q, which is not a DNS character",
					ErrInvalid, host, string(r))
			}
		}
	}
	return nil
}

// blockedNets are addresses no grant may ever reach, whatever a name resolves
// to. They are checked at CONNECT time against the resolved address, which is
// what makes DNS rebinding ineffective: an allowlisted name that resolves to
// one of these is refused at the moment of connection, not at grant time.
//
// The metadata address is first because it is the one that matters: on a cloud
// host it hands out the instance's own credentials to anything that asks.
var blockedNets = func() []*net.IPNet {
	cidrs := []string{
		"169.254.0.0/16", // link-local, incl. 169.254.169.254 cloud metadata
		"fe80::/10",      // link-local v6
		"127.0.0.0/8",    // loopback
		"::1/128",        // loopback v6
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"100.64.0.0/10",  // carrier-grade NAT / Tailscale
		"fc00::/7",       // unique local v6
		"0.0.0.0/8",      // this network
		"::/128",         // unspecified
		"224.0.0.0/4",    // multicast
		"ff00::/8",       // multicast v6
		"240.0.0.0/4",    // reserved
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			panic("skillegress: bad blocked CIDR " + c)
		}
		out = append(out, network)
	}
	return out
}()

// BlockedAddress reports whether an address is one no grant may reach, and why.
//
// It is deliberately a denylist ON TOP of the allowlist rather than instead of
// it: a name has to be granted AND resolve outside these ranges. Either check
// alone would be insufficient — the allowlist cannot know what a name resolves
// to, and the ranges cannot know what was approved.
func BlockedAddress(ip net.IP) (string, bool) { return blockedIn(blockedNets, ip) }

// blockedIn is BlockedAddress against a given list. The list is a parameter so
// this package's own tests can stand a synthetic upstream on loopback, which
// the production list blocks; no caller outside the package can supply one.
func blockedIn(nets []*net.IPNet, ip net.IP) (string, bool) {
	if ip == nil {
		return "unparseable address", true
	}
	if ip.IsUnspecified() {
		return "unspecified address", true
	}
	for _, network := range nets {
		if network.Contains(ip) {
			return "address is in the blocked range " + network.String(), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// Reuse by other AO components.
//
// The daemon itself reaches out to a configured private skill registry
// (internal/skillregistry), and that is a second place where "which addresses
// may AO connect to" gets decided. It is deliberately NOT a second policy: the
// two functions below are the ONLY way that decision is made outside this
// package, and they are thin exports of the same blocked-range list and the
// same never-re-openable link-local rule the skill proxy enforces.
//
// Two incompatible network policies in one product is how the metadata address
// ends up reachable through the half nobody audited.

// ParsePermittedCIDRs validates private-range exceptions and returns them
// parsed.
//
// It refuses any entry that overlaps a range no exception may re-open --
// link-local, and therefore the cloud metadata address. A caller that ignores
// the error and uses a nil list gets the full denylist, which is the safe
// direction.
func ParsePermittedCIDRs(raw []string) ([]*net.IPNet, error) { return validatePermittedCIDRs(raw) }

// AddressBlocked reports whether AO may connect to ip, and why not.
//
// permitted are ranges an operator explicitly re-opened for one destination --
// an on-premises registry that genuinely lives at 10.x. They widen which
// ADDRESSES are acceptable and never which hosts may be reached: the caller's
// own allowlist is a separate, earlier check, and this one runs against the
// address a name actually resolved to, which is what makes DNS rebinding
// ineffective.
func AddressBlocked(ip net.IP, permitted []*net.IPNet) (string, bool) {
	reason, blocked := blockedIn(blockedNets, ip)
	if !blocked {
		return "", false
	}
	for _, network := range permitted {
		if network.Contains(ip) {
			return "", false
		}
	}
	return reason, true
}
