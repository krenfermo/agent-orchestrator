package skillegress

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseDestination_AcceptsWhatCanBeReasonedAbout(t *testing.T) {
	cases := map[string]Destination{
		"https://api.example.test":      {Scheme: SchemeHTTPS, Host: "api.example.test", Port: 443},
		"http://api.example.test":       {Scheme: SchemeHTTP, Host: "api.example.test", Port: 80},
		"https://api.example.test:8443": {Scheme: SchemeHTTPS, Host: "api.example.test", Port: 8443},
		"HTTPS://API.Example.Test":      {Scheme: SchemeHTTPS, Host: "api.example.test", Port: 443},
		"https://a-b.c-d.example.test/": {Scheme: SchemeHTTPS, Host: "a-b.c-d.example.test", Port: 443},
	}
	for raw, want := range cases {
		got, err := ParseDestination(raw)
		if err != nil {
			t.Fatalf("ParseDestination(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseDestination(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

// Every refusal here is a shape that would make an allowlist entry impossible
// to evaluate, or that is the shape an exfiltration destination takes.
func TestParseDestination_Refusals(t *testing.T) {
	cases := map[string]string{
		"":                                 "empty destination",
		"   ":                              "empty destination",
		"https://*.example.test":           "wildcard",
		"https://sub.*.example.test":       "wildcard",
		"ftp://files.example.test":         "not http or https",
		"ssh://host.example.test":          "not http or https",
		"https://93.184.216.34":            "IP literal",
		"https://[2606:2800:220:1::]":      "IP literal",
		"https://localhost":                "fully qualified",
		"https://internal":                 "fully qualified",
		"https://api.example.test/path":    "carries a path",
		"https://user:pw@api.example.test": "carries credentials",
		"https://.example.test":            "malformed",
		"https://example..test":            "malformed",
		"https://exa_mple.test":            "not a DNS character",
		"https://api.example.test:0":       "out of range",
		"https://api.example.test:99999":   "out of range",
		"not-a-url":                        "not scheme://host",
	}
	for raw, wantSub := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseDestination(raw)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("ParseDestination(%q) = %v, want ErrInvalid", raw, err)
			}
			if !strings.Contains(err.Error(), wantSub) {
				t.Fatalf("err = %v, want %q", err, wantSub)
			}
		})
	}
}

func TestGrant_Validate(t *testing.T) {
	now := time.Now().UTC()
	dest, err := ParseDestination("https://api.example.test")
	if err != nil {
		t.Fatalf("ParseDestination: %v", err)
	}
	base := Grant{
		ID: "g1", Scope: testScope(), Destinations: []Destination{dest},
		GrantedBy: "admin", GrantedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("a valid grant was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Grant){
		"no destinations": func(g *Grant) { g.Destinations = nil },
		"duplicate destination": func(g *Grant) {
			g.Destinations = []Destination{dest, dest}
		},
		"no approver":   func(g *Grant) { g.GrantedBy = "" },
		"no expiry":     func(g *Grant) { g.ExpiresAt = time.Time{} },
		"partial scope": func(g *Grant) { g.Scope.ModeID = "" },
		"expires before it is made": func(g *Grant) {
			g.ExpiresAt = g.GrantedAt.Add(-time.Hour)
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := base
			mutate(&g)
			if err := g.Validate(); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestGrant_ActiveHonoursExpiryAndRevocation(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	dest, _ := ParseDestination("https://api.example.test")
	live := Grant{
		ID: "g1", Scope: testScope(), Destinations: []Destination{dest},
		GrantedBy: "admin", GrantedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
	}
	if !live.Active(now) {
		t.Fatal("a live grant read as inactive")
	}
	expired := live
	expired.ExpiresAt = now.Add(-time.Minute)
	if expired.Active(now) {
		t.Fatal("an expired grant read as active")
	}
	revokedAt := now.Add(-time.Minute)
	revoked := live
	revoked.RevokedAt = &revokedAt
	if revoked.Active(now) {
		t.Fatal("a revoked grant read as active")
	}
}

// The policy is what the proxy reads. A tampered file must produce a refusal,
// never a wider allowlist.
func TestDecodePolicy_RefusesATamperedFile(t *testing.T) {
	dest, _ := ParseDestination("https://api.example.test")
	policy, err := PolicyFor(Lease{
		ID: "l1", Scope: testScope(), RunID: "r", AttemptID: "a",
		Destinations: []Destination{dest},
		IssuedAt:     time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("PolicyFor: %v", err)
	}
	body, err := policy.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := DecodePolicy(body); err != nil {
		t.Fatalf("a well-formed policy was rejected: %v", err)
	}

	for name, tampered := range map[string]string{
		"wildcard host":   strings.Replace(string(body), "api.example.test", "*.example.test", 1),
		"IP literal":      strings.Replace(string(body), "api.example.test", "169.254.169.254", 1),
		"unqualified":     strings.Replace(string(body), "api.example.test", "internal", 1),
		"unknown field":   strings.Replace(string(body), `"leaseId"`, `"allowAll": true, "leaseId"`, 1),
		"no destinations": `{"leaseId":"l1","destinations":[],"expiresAt":"2030-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePolicy([]byte(tampered)); err == nil {
				t.Fatal("a tampered policy was accepted")
			}
		})
	}
}

// The match is exact on scheme, host AND port.
func TestPolicy_AllowsIsExact(t *testing.T) {
	policy := policyWith(t, time.Hour, "https://api.example.test:443")

	if !policy.Allows(SchemeHTTPS, "api.example.test", 443) {
		t.Fatal("the granted destination was refused")
	}
	if !policy.Allows(SchemeHTTPS, "API.Example.Test", 443) {
		t.Fatal("case should not matter for a host")
	}
	for name, check := range map[string]bool{
		"other scheme": policy.Allows(SchemeHTTP, "api.example.test", 443),
		"other port":   policy.Allows(SchemeHTTPS, "api.example.test", 8443),
		"other host":   policy.Allows(SchemeHTTPS, "other.example.test", 443),
		"subdomain":    policy.Allows(SchemeHTTPS, "sub.api.example.test", 443),
		"suffix trick": policy.Allows(SchemeHTTPS, "api.example.test.evil.test", 443),
	} {
		if check {
			t.Fatalf("%s was allowed", name)
		}
	}
}

// An exception may re-open a private range an installation genuinely uses. It
// may NEVER re-open link-local: that is where cloud metadata hands out the
// host's own credentials, and no legitimate skill destination lives there.
func TestPolicy_AnExceptionCannotReopenMetadata(t *testing.T) {
	for _, forbidden := range []string{
		"169.254.0.0/16", "169.254.169.254/32", "169.254.0.0/24", "fe80::/10", "0.0.0.0/0",
	} {
		if _, err := validatePermittedCIDRs([]string{forbidden}); err == nil {
			t.Fatalf("%q was accepted as a private-range exception", forbidden)
		}
	}
	for _, allowed := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fd00::/8"} {
		if _, err := validatePermittedCIDRs([]string{allowed}); err != nil {
			t.Fatalf("%q was refused: %v", allowed, err)
		}
	}
	if _, err := validatePermittedCIDRs([]string{"not-a-cidr"}); err == nil {
		t.Fatal("a malformed CIDR was accepted")
	}

	// And an exception widens which ADDRESSES a granted name may resolve to,
	// never which names may be reached.
	policy := policyWith(t, time.Hour, "http://allowed.example.test:80")
	policy.PermittedPrivateCIDRs = []string{"10.0.0.0/8"}
	p := NewProxy(policy, nil)
	if !p.permits(net.ParseIP("10.1.2.3")) {
		t.Fatal("the exception did not take effect")
	}
	if p.permits(net.ParseIP("169.254.169.254")) {
		t.Fatal("the exception re-opened the metadata address")
	}
	if policy.Allows(SchemeHTTP, "other.example.test", 80) {
		t.Fatal("an address exception widened the host allowlist")
	}
	// A malformed exception leaves the full denylist in place rather than
	// dropping it.
	broken := policyWith(t, time.Hour, "http://allowed.example.test:80")
	broken.PermittedPrivateCIDRs = []string{"garbage"}
	if len(NewProxy(broken, nil).permitted) != 0 {
		t.Fatal("a malformed exception was honoured")
	}
	// The exception is visible in the audit line: one nobody can see is one
	// nobody reviews.
	if !strings.Contains(policy.Summary(), "10.0.0.0/8") {
		t.Fatalf("summary hides the exception: %q", policy.Summary())
	}
}
