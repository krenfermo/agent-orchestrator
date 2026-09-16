package supervisor

import (
	"regexp"
	"strings"
	"testing"
)

// The endpoint names are pure functions of the daemon's instance identity, so
// they are tested on every platform (Windows naming included).

func TestEndpointToken_isStableShortAndPerInstance(t *testing.T) {
	a1, err := EndpointToken("aod-11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := EndpointToken("aod-11111111-1111-4111-8111-111111111111")
	b, _ := EndpointToken("aod-22222222-2222-4222-8222-222222222222")
	if a1 != a2 {
		t.Fatalf("token not deterministic: %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("two instances share token %q", a1)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(a1) {
		t.Fatalf("token %q is not 16 lowercase hex chars", a1)
	}
}

func TestEndpointToken_failsClosedWithoutIdentity(t *testing.T) {
	for _, id := range []string{"", "   ", " aod-1", "aod-1\n"} {
		if _, err := EndpointToken(id); err == nil {
			t.Fatalf("EndpointToken(%q) succeeded; an endpoint without an instance identity must not exist", id)
		}
	}
	if _, err := SocketName(""); err == nil {
		t.Fatal("SocketName without identity succeeded")
	}
	if _, err := PipeName(""); err == nil {
		t.Fatal("PipeName without identity succeeded")
	}
}

func TestEndpointNames_installationsAndInstancesNeverShare(t *testing.T) {
	// Two installations always run two daemon instances; every instance has its own id.
	instances := []string{
		"aod-11111111-1111-4111-8111-111111111111", // installation A
		"aod-22222222-2222-4222-8222-222222222222", // installation B, same logical directory
		"aod-33333333-3333-4333-8333-333333333333", // A restarted
	}
	sockets := map[string]bool{}
	pipes := map[string]bool{}
	for _, id := range instances {
		s, err := SocketName(id)
		if err != nil {
			t.Fatal(err)
		}
		p, err := PipeName(id)
		if err != nil {
			t.Fatal(err)
		}
		if sockets[s] || pipes[p] {
			t.Fatalf("endpoint collision for %s: socket %q pipe %q", id, s, p)
		}
		sockets[s], pipes[p] = true, true
	}
}

func TestPipeName_windowsFormat(t *testing.T) {
	p, err := PipeName("aod-11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, `\\.\pipe\ao-supervise-`) {
		t.Fatalf("pipe %q lacks the \\\\.\\pipe\\ao-supervise- prefix", p)
	}
	if !regexp.MustCompile(`^\\\\\.\\pipe\\ao-supervise-[0-9a-f]{16}$`).MatchString(p) {
		t.Fatalf("pipe %q has unexpected characters", p)
	}
	// The legacy shared names must be impossible.
	if p == `\\.\pipe\ao-supervise` || p == `\\.\pipe\ao-supervise-dev` {
		t.Fatalf("pipe %q is a legacy shared name", p)
	}
}

func TestSocketName_format(t *testing.T) {
	s, err := SocketName("aod-11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^supervise-[0-9a-f]{16}\.sock$`).MatchString(s) {
		t.Fatalf("socket name %q has unexpected shape", s)
	}
	if s == "supervise.sock" {
		t.Fatal("socket name is the legacy shared name")
	}
}
