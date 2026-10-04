package skillegress

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillscope"
)

// Scope is re-exported so egress and secrets share one definition of "which
// run may do this".
type Scope = skillscope.Scope

// Grant authorizes one scope to reach a fixed set of destinations, until it
// expires or is revoked. It mirrors the secret grant deliberately: the same
// questions have the same answers, and a reader who understands one
// understands the other.
type Grant struct {
	ID           string        `json:"id"`
	Scope        Scope         `json:"scope"`
	Destinations []Destination `json:"destinations"`
	GrantedBy    string        `json:"grantedBy"`
	GrantedAt    time.Time     `json:"grantedAt"`
	// ExpiresAt is required. A network grant that never expires is one nobody
	// remembers to remove.
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

// Active reports whether the grant may be used at the given moment.
func (g Grant) Active(now time.Time) bool {
	if g.RevokedAt != nil && !g.RevokedAt.After(now) {
		return false
	}
	return g.ExpiresAt.After(now)
}

// Validate rejects a grant that could not be enforced.
func (g Grant) Validate() error {
	if err := g.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(g.Destinations) == 0 {
		return fmt.Errorf("%w: a grant with no destinations allows nothing and should not exist", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, d := range g.Destinations {
		if err := d.Validate(); err != nil {
			return err
		}
		if seen[d.String()] {
			return fmt.Errorf("%w: %s is granted twice", ErrInvalid, d)
		}
		seen[d.String()] = true
	}
	if strings.TrimSpace(g.GrantedBy) == "" {
		return fmt.Errorf("%w: a grant records who made it", ErrInvalid)
	}
	if g.ExpiresAt.IsZero() || !g.ExpiresAt.After(g.GrantedAt) {
		return fmt.Errorf("%w: a grant must expire after it is made", ErrInvalid)
	}
	return nil
}

// Lease is one attempt's right to use a grant, for a bounded window.
//
// Unlike a secret lease it is not single-use: a run makes many connections. It
// is bounded by time and by the attempt it belongs to, and the proxy stops
// serving the moment it expires — so a run that outlives its lease loses the
// network rather than keeping it.
type Lease struct {
	ID           string        `json:"id"`
	Scope        Scope         `json:"scope"`
	RunID        string        `json:"runId"`
	AttemptID    string        `json:"attemptId"`
	Destinations []Destination `json:"destinations"`
	IssuedAt     time.Time     `json:"issuedAt"`
	ExpiresAt    time.Time     `json:"expiresAt"`
}

// Validate rejects a lease that could not be enforced.
func (l Lease) Validate() error {
	if err := l.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if strings.TrimSpace(l.RunID) == "" || strings.TrimSpace(l.AttemptID) == "" {
		return fmt.Errorf("%w: a lease binds to one run AND one attempt", ErrInvalid)
	}
	if len(l.Destinations) == 0 {
		return fmt.Errorf("%w: a lease with no destinations allows nothing", ErrInvalid)
	}
	for _, d := range l.Destinations {
		if err := d.Validate(); err != nil {
			return err
		}
	}
	if !l.ExpiresAt.After(l.IssuedAt) {
		return fmt.Errorf("%w: a lease expires after it is issued", ErrInvalid)
	}
	return nil
}

// Policy is what the proxy enforces for one run. It is the serialized form of
// a Lease, written to a file the proxy reads at startup — the proxy has no
// other source of authority and no way to be told to allow more.
type Policy struct {
	LeaseID      string        `json:"leaseId"`
	Scope        Scope         `json:"scope"`
	RunID        string        `json:"runId"`
	AttemptID    string        `json:"attemptId"`
	Destinations []Destination `json:"destinations"`
	ExpiresAt    time.Time     `json:"expiresAt"`
	// PermittedPrivateCIDRs are private ranges this run may reach, on top of
	// the allowlist. It exists because the blanket refusal of private
	// addresses is right by default and wrong for an installation whose
	// artifact registry genuinely lives at 10.x -- a control nobody can use is
	// a control that gets turned off.
	//
	// It is narrow on purpose: each entry is written down, each is validated
	// against LinkLocalNever below, and each appears in the proxy's startup
	// line and its decision log. Empty is the default and the common case.
	PermittedPrivateCIDRs []string `json:"permittedPrivateCidrs,omitempty"`
}

// linkLocalNever are ranges no exception may ever re-open. The cloud metadata
// address is the reason this list exists: on a cloud host it hands out the
// instance's own credentials to anything that asks, and no legitimate skill
// destination lives there.
var linkLocalNever = []string{"169.254.0.0/16", "fe80::/10"}

// validatePermittedCIDRs rejects an exception that would re-open something no
// grant may reach.
func validatePermittedCIDRs(raw []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(raw))
	for _, entry := range raw {
		_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a CIDR", ErrInvalid, entry)
		}
		for _, forbidden := range linkLocalNever {
			_, never, parseErr := net.ParseCIDR(forbidden)
			if parseErr != nil {
				continue
			}
			if never.Contains(network.IP) || network.Contains(never.IP) {
				return nil, fmt.Errorf("%w: %q overlaps %s, which no exception may re-open "+
					"(it is where cloud metadata hands out the host's own credentials)",
					ErrInvalid, entry, forbidden)
			}
		}
		out = append(out, network)
	}
	return out, nil
}

// PolicyFor renders a lease as the policy its proxy will enforce.
func PolicyFor(l Lease) (Policy, error) {
	if err := l.Validate(); err != nil {
		return Policy{}, err
	}
	dests := append([]Destination(nil), l.Destinations...)
	sort.Slice(dests, func(i, j int) bool { return dests[i].String() < dests[j].String() })
	return Policy{
		LeaseID: l.ID, Scope: l.Scope, RunID: l.RunID, AttemptID: l.AttemptID,
		Destinations: dests, ExpiresAt: l.ExpiresAt,
	}, nil
}

// Encode renders the policy for the proxy's config file.
func (p Policy) Encode() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }

// DecodePolicy reads a policy and re-validates every destination. A policy
// file that was tampered with produces a refusal, not a wider allowlist.
func DecodePolicy(body []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%w: parse policy: %w", ErrInvalid, err)
	}
	if len(p.Destinations) == 0 {
		return Policy{}, fmt.Errorf("%w: a policy with no destinations allows nothing", ErrInvalid)
	}
	for _, d := range p.Destinations {
		if err := d.Validate(); err != nil {
			return Policy{}, err
		}
	}
	if _, err := validatePermittedCIDRs(p.PermittedPrivateCIDRs); err != nil {
		return Policy{}, err
	}
	if p.ExpiresAt.IsZero() {
		return Policy{}, fmt.Errorf("%w: a policy must expire", ErrInvalid)
	}
	return p, nil
}

// Allows reports whether one authority (host:port) is granted, for one scheme.
//
// The match is exact on all three. A destination granted as https://x:443 does
// not authorize http://x:80, because "may talk TLS to this host" and "may send
// cleartext to it" are different permissions.
func (p Policy) Allows(scheme Scheme, host string, port int) bool {
	want := Destination{Scheme: scheme, Host: strings.ToLower(host), Port: port}
	for _, d := range p.Destinations {
		if d == want {
			return true
		}
	}
	return false
}

// Expired reports whether the policy's window has closed. The proxy checks it
// per request, so a run that outlives its lease loses the network mid-run
// rather than keeping it to the end.
func (p Policy) Expired(now time.Time) bool { return !p.ExpiresAt.After(now) }

// Summary renders the allowlist for an audit line, including any private-range
// exception -- an exception nobody can see is one nobody reviews.
func (p Policy) Summary() string {
	out := make([]string, 0, len(p.Destinations))
	for _, d := range p.Destinations {
		out = append(out, d.String())
	}
	summary := strings.Join(out, ", ")
	if len(p.PermittedPrivateCIDRs) > 0 {
		summary += " (private ranges permitted: " + strings.Join(p.PermittedPrivateCIDRs, ", ") + ")"
	}
	return summary
}
