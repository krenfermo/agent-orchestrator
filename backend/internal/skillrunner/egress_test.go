package skillrunner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// These cover the gate phase 8 puts in front of the egress attestation: the
// proxy has to be the right one AND the boundary has to have been observed to
// hold. Every case below is a way one of those is false, and every one of them
// must end with the control NOT attested.

// scriptedCLI answers per subcommand, so a test can make exactly one step of
// the boundary check fail while the rest behaves.
type scriptedCLI struct {
	// answers maps a substring of the joined argv to a response.
	answers []scriptedAnswer
	// seen records every argv, so a test can assert what AO actually ran.
	seen []string
}

type scriptedAnswer struct {
	match string
	out   string
	err   error
}

func (s *scriptedCLI) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	s.seen = append(s.seen, joined)
	for _, a := range s.answers {
		if strings.Contains(joined, a.match) {
			return []byte(a.out), a.err
		}
	}
	return nil, errors.New("scriptedCLI: nothing matched " + joined)
}

func (s *scriptedCLI) ran(substr string) bool {
	for _, cmd := range s.seen {
		if strings.Contains(cmd, substr) {
			return true
		}
	}
	return false
}

const fakeImageID = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// workingCLI answers every step of the check the way a healthy host would.
func workingCLI(observed string) *scriptedCLI {
	return &scriptedCLI{answers: []scriptedAnswer{
		{match: "image inspect", out: fakeImageID},
		{match: "network create", out: "netid"},
		{match: "network inspect", out: "true\n"},
		{match: "network rm", out: "netid"},
		{match: "run --rm", out: observed},
	}}
}

func runnerOn(arch string, cli commandRunner) *Runner {
	return &Runner{
		runtime: Runtime{Binary: "docker", ServerVersion: "29.2.1", CgroupVersion: "2",
			OSType: "linux", Architecture: arch},
		runner: cli,
	}
}

// packagedStore is a store that behaves like a release build for one
// architecture, without this package needing the 6 MiB artifact.
func packagedStore(t *testing.T, goarch string) proxybin.Store {
	t.Helper()
	return proxybin.StoreForTest(goarch, []byte("the packaged proxy for "+goarch))
}

// The whole gate, positive: a verified proxy for the runtime's architecture and
// an outbound connection observed to fail. Only then is the control attested.
func TestVerifyEgressBoundary_AttestsOnlyAfterMeasuringBothHalves(t *testing.T) {
	cli := workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n")
	r := runnerOn("aarch64", cli)

	b := r.verifyEgressBoundary(context.Background(), packagedStore(t, "arm64"))
	if !b.OK() {
		t.Fatalf("boundary not OK: %s (%+v)", b.Explain(), b)
	}
	if b.ProxyArch != "arm64" || len(b.ProxyDigest) != 64 {
		t.Fatalf("boundary records %+v", b)
	}
	// The network AO created must be internal, and AO must have said so rather
	// than assumed it.
	if !cli.ran("network create --internal") || !cli.ran("network inspect") {
		t.Fatalf("AO did not create and inspect an internal network: %v", cli.seen)
	}
	// The probe container runs with the same posture a skill container does.
	if !cli.ran("--user "+nobodyUser) || !cli.ran("--read-only") || !cli.ran("--cap-drop ALL") {
		t.Fatalf("the probe container ran with a weaker posture: %v", cli.seen)
	}
	// And the network is removed on the way out, whatever happened.
	if !cli.ran("network rm") {
		t.Fatalf("AO left its probe network behind: %v", cli.seen)
	}

	att := r.WithEgressBoundary(b).Attestation()
	if !att.Provides(skillcatalog.ControlEgressAllowlist) {
		t.Fatal("a measured boundary did not attest the allowlist")
	}
	// Still not a scan: opening a connection and probing what is on the other
	// end are different permissions.
	activeScan, _ := skillcatalog.CapNetActiveScan.Spec()
	if _, ok := firstMissingControlFor(activeScan.RequiresControls, att); ok {
		t.Fatal("net.active_scan became grantable when the allowlist arrived")
	}
}

// Requirement 4, negatively. Each row is one way the check comes back short,
// and none of them may attest.
func TestVerifyEgressBoundary_FailsClosedOnEveryMissingPiece(t *testing.T) {
	good := []byte("the packaged proxy for arm64")
	cases := []struct {
		name    string
		arch    string
		store   func(t *testing.T) proxybin.Store
		cli     func() *scriptedCLI
		wantSub string
	}{
		{
			name:  "no proxy in this build",
			arch:  "aarch64",
			store: func(*testing.T) proxybin.Store { return proxybin.UnpackagedStoreForTest("arm64", good) },
			cli:   func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n") },
			// The refusal must name the build tag: nobody guesses it.
			wantSub: "ao_embed_egress_proxy",
		},
		{
			name:    "the proxy is for the wrong architecture",
			arch:    "x86_64",
			store:   func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli:     func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n") },
			wantSub: "runs linux/amd64",
		},
		{
			name:    "the runtime does not say what it runs",
			arch:    "",
			store:   func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli:     func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n") },
			wantSub: "reported no architecture",
		},
		{
			name:    "the runtime runs an architecture AO does not package",
			arch:    "s390x",
			store:   func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli:     func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n") },
			wantSub: "does not package a proxy for",
		},
		{
			name:  "the artifact does not hash to its provenance",
			arch:  "aarch64",
			store: func(*testing.T) proxybin.Store { return proxybin.CorruptStoreForTest("arm64", good) },
			cli:   func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n") },
			// The digest check is what makes the embed a supply-chain boundary.
			wantSub: "does not match its provenance",
		},
		{
			name:  "the runtime cannot create an internal network",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli: func() *scriptedCLI {
				return &scriptedCLI{answers: []scriptedAnswer{
					{match: "image inspect", out: fakeImageID},
					{match: "network create", err: errors.New("plugin not found")},
					{match: "network rm", out: ""},
				}}
			},
			wantSub: "could not create an internal network",
		},
		{
			name:  "the network exists but is not internal",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli: func() *scriptedCLI {
				cli := workingCLI("blocked 192.0.2.1\nblocked 169.254.169.254\n")
				cli.answers[2] = scriptedAnswer{match: "network inspect", out: "false\n"}
				return cli
			},
			wantSub: "does not report it as internal",
		},
		{
			name:  "the boundary leaks: the probe reached out",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli: func() *scriptedCLI {
				return workingCLI("REACHED 192.0.2.1\nblocked 169.254.169.254\n")
			},
			wantSub: "still reached out",
		},
		{
			name:  "the probe reported nothing, so nothing was measured",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli:   func() *scriptedCLI { return workingCLI("   \n") },
			// Silence is not a pass. An empty answer means the measurement did
			// not happen, which is indistinguishable from it having failed.
			wantSub: "reported nothing",
		},
		{
			name:  "the probe only covered one of the two addresses",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli:   func() *scriptedCLI { return workingCLI("blocked 192.0.2.1\n") },
			// The metadata address is the one a skill container most needs not
			// to reach; a probe that did not test it proves nothing about it.
			wantSub: "did not report on 169.254.169.254",
		},
		{
			name:  "the probe container could not run at all",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli: func() *scriptedCLI {
				cli := workingCLI("")
				cli.answers[4] = scriptedAnswer{match: "run --rm", err: errors.New("no such image")}
				return cli
			},
			wantSub: "probe container did not run",
		},
		{
			name:  "the base image AO probes with is not on the host",
			arch:  "aarch64",
			store: func(t *testing.T) proxybin.Store { return packagedStore(t, "arm64") },
			cli: func() *scriptedCLI {
				return &scriptedCLI{answers: []scriptedAnswer{
					{match: "image inspect", err: errors.New("No such image")},
				}}
			},
			wantSub: "cannot measure the egress boundary",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := tc.cli()
			r := runnerOn(tc.arch, cli)
			b := r.verifyEgressBoundary(context.Background(), tc.store(t))
			if b.OK() {
				t.Fatalf("the boundary passed with %s: %+v", tc.name, b)
			}
			if !strings.Contains(b.Explain(), tc.wantSub) {
				t.Fatalf("Explain() = %q, want it to mention %q", b.Explain(), tc.wantSub)
			}
			// The attestation is the thing that matters: a short measurement
			// must leave net.egress refused with the control named.
			att := r.WithEgressBoundary(b).Attestation()
			if att.Provides(skillcatalog.ControlEgressAllowlist) {
				t.Fatalf("%s still attested the allowlist", tc.name)
			}
			netEgress, _ := skillcatalog.CapNetEgress.Spec()
			missing, ok := firstMissingControlFor(netEgress.RequiresControls, att)
			if ok {
				t.Fatal("net.egress was grantable without a boundary")
			}
			if missing != skillcatalog.ControlEgressAllowlist {
				t.Fatalf("net.egress is missing %q, want egress_allowlist", missing)
			}
			// Deny-all is still true, and is still a weaker claim.
			if !att.Provides(skillcatalog.ControlEgressDenyAll) {
				t.Fatal("a plain container stopped attesting deny-all")
			}
			// Whatever failed afterwards, AO does not leave its probe network
			// behind. A create that itself failed made nothing to remove, and
			// removing a name AO does not own is not better than leaking one.
			createdOne := cli.ran("network create") &&
				!strings.Contains(b.Explain(), "could not create an internal network")
			if createdOne && !cli.ran("network rm") {
				t.Fatalf("AO created a network and did not remove it: %v", cli.seen)
			}
		})
	}
}

// A runtime that cannot run anything cannot measure anything, and must say so
// rather than report a boundary it never looked at.
func TestVerifyEgressBoundary_AnUnusableRuntimeMeasuresNothing(t *testing.T) {
	r := &Runner{probeErr: errors.New("Cannot connect to the Docker daemon"), runner: &scriptedCLI{}}
	b := r.verifyEgressBoundary(context.Background(), proxybin.StoreForTest("arm64", []byte("proxy")))
	if b.OK() {
		t.Fatal("an unusable runtime reported a working boundary")
	}
	if !errors.Is(b.Err, ErrRuntimeUnavailable) {
		t.Fatalf("Err = %v, want ErrRuntimeUnavailable", b.Err)
	}
	if len(r.WithEgressBoundary(b).Attestation().Controls) != 0 {
		t.Fatal("an unusable runtime attested something")
	}
}

// The zero boundary is the state of a daemon that never asked. It must attest
// nothing: "we did not check" and "we checked and it held" cannot be the same
// value.
func TestEgressBoundary_TheZeroValueAttestsNothing(t *testing.T) {
	var b EgressBoundary
	if b.OK() {
		t.Fatal("the zero boundary reported itself OK")
	}
	if b.Explain() == "" {
		t.Fatal("the zero boundary explains nothing")
	}
	r := runnerOn("aarch64", &scriptedCLI{})
	if r.Attestation().Provides(skillcatalog.ControlEgressAllowlist) {
		t.Fatal("a runner that never measured attested the allowlist")
	}
	// Every partially-true boundary is also false. There is no partial credit:
	// a proxy with no topology and a topology with no proxy each enforce
	// nothing.
	for _, partial := range []EgressBoundary{
		{ProxyPackaged: true},
		{ProxyPackaged: true, InternalNetwork: true},
		{InternalNetwork: true, OutboundBlocked: true},
		{ProxyPackaged: true, OutboundBlocked: true},
		{ProxyPackaged: true, InternalNetwork: true, OutboundBlocked: true, Err: errors.New("late failure")},
	} {
		if partial.OK() {
			t.Fatalf("%+v passed with a piece missing", partial)
		}
	}
}
