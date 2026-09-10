package skillregistry_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// probe_test.go -- the six states, each reached by the thing that actually
// causes it.
//
// The most important case in this file is TestProbeIsNotConnectedJustBecause
// SomethingAnswered: a green badge that appears whenever a socket opens is
// worse than no badge, because people act on it.

// probeOf opens the registry the way the service does -- through the factory --
// and asks it to identify itself. A test that built its own client would be
// testing a different client than the one every other operation uses.
func probeOf(t *testing.T, reg skillregistry.Registry, srv *registrytest.Server,
	resolver skillregistry.SecretResolver,
) skillregistry.ProbeResult {
	t.Helper()
	return probeWith(t, reg, skillregistry.DefaultProviderFactory{
		Secrets: resolver, Options: srv.Options(),
	})
}

func probeWith(
	t *testing.T, reg skillregistry.Registry, factory skillregistry.DefaultProviderFactory,
) skillregistry.ProbeResult {
	t.Helper()
	provider, err := factory.Open(context.Background(), reg)
	if err != nil {
		return skillregistry.ProbeRefused(reg, err, nil)
	}
	prober, ok := provider.(skillregistry.ConnectionProbe)
	if !ok {
		t.Fatalf("%T cannot be probed", provider)
	}
	return prober.Probe(context.Background(), nil)
}

func TestProbeConnected(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Publish(t, registrytest.Spec{SkillID: "security-audit", Version: "1.0.0"})

	got := probeOf(t, srv.Registry("corp"), srv, nil)
	if got.State != skillregistry.ProbeConnected {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
	if got.ProtocolVersion != skillregistry.ProtocolVersion {
		t.Fatalf("protocol %q", got.ProtocolVersion)
	}
	// A connection test downloads nothing. The whole value of the button is
	// that it is safe to press.
	if srv.FetchedArtifact() {
		t.Fatalf("the connection test fetched an artifact: %v", srv.Requests())
	}
	for _, path := range srv.Requests() {
		if path != "/v1/registry" {
			t.Fatalf("the connection test called %s; it reads one small metadata endpoint", path)
		}
	}
}

// TestProbeIsNotConnectedJustBecauseSomethingAnswered is the point of the
// handshake. A 200 from something that is not this registry is
// INVALID_RESPONSE, not CONNECTED.
func TestProbeIsNotConnectedJustBecauseSomethingAnswered(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.SetRegistryID("some-other-registry")

	got := probeOf(t, srv.Registry("corp"), srv, nil)
	if got.State != skillregistry.ProbeInvalidResponse {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
	if got.ReportedRegistryID != "some-other-registry" {
		t.Fatalf("the result does not say what answered: %+v", got)
	}
}

func TestProbeAuthFailed(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.RequireBearer("right")
	reg := srv.Registry("corp")
	reg.AuthType = skillregistry.AuthBearer
	reg.CredentialSecretName = "CORP_TOKEN"

	got := probeOf(t, reg, srv, skillregistry.SecretResolverFunc(
		func(context.Context, skillregistry.Registry) (skillsecrets.SecretValue, error) {
			return skillsecrets.NewSecretValue("wrong"), nil
		}))
	if got.State != skillregistry.ProbeAuthFailed {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
	if strings.Contains(got.Detail, "wrong") {
		t.Fatalf("the probe detail quotes the credential: %s", got.Detail)
	}
	if !strings.Contains(got.Detail, "CORP_TOKEN") {
		t.Fatalf("the probe detail does not name the secretRef: %s", got.Detail)
	}
}

func TestProbeTLSFailed(t *testing.T) {
	srv := registrytest.New(t, "corp")
	opts := srv.Options()
	opts.RootCAs = srv.UntrustedCA.Pool
	got := probeWith(t, srv.Registry("corp"), skillregistry.DefaultProviderFactory{Options: opts})
	if got.State != skillregistry.ProbeTLSFailed {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
}

func TestProbeUnreachable(t *testing.T) {
	srv := registrytest.New(t, "corp")
	reg := srv.Registry("corp")
	srv.Stop()
	got := probeOf(t, reg, srv, nil)
	if got.State != skillregistry.ProbeUnreachable {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
}

func TestProbeServerErrorIsUnreachableNotConnected(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Fail(registrytest.Failures{ServerError: true})
	got := probeOf(t, srv.Registry("corp"), srv, nil)
	if got.State != skillregistry.ProbeUnreachable {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
}

func TestProbeInvalidResponseOnUnknownProtocol(t *testing.T) {
	srv := registrytest.New(t, "corp")
	srv.Fail(registrytest.Failures{WrongAPIVersion: true})
	got := probeOf(t, srv.Registry("corp"), srv, nil)
	if got.State != skillregistry.ProbeInvalidResponse {
		t.Fatalf("state %s: %s", got.State, got.Detail)
	}
}

func TestProbePolicyBlocked(t *testing.T) {
	t.Run("the origin resolves into a blocked range", func(t *testing.T) {
		srv := registrytest.New(t, "corp")
		opts := srv.Options()
		opts.Resolver = registrytest.FixedResolver{IPs: []net.IP{net.ParseIP("169.254.169.254")}}
		got := probeWith(t, srv.Registry("corp"), skillregistry.DefaultProviderFactory{Options: opts})
		if got.State != skillregistry.ProbePolicyBlocked {
			t.Fatalf("state %s: %s", got.State, got.Detail)
		}
	})
	t.Run("the credential cannot be resolved", func(t *testing.T) {
		srv := registrytest.New(t, "corp")
		reg := srv.Registry("corp")
		reg.AuthType = skillregistry.AuthBearer
		reg.CredentialSecretName = "MISSING_TOKEN"
		got := probeOf(t, reg, srv, nil)
		if got.State != skillregistry.ProbePolicyBlocked {
			t.Fatalf("state %s: %s", got.State, got.Detail)
		}
		if !strings.Contains(got.Detail, "MISSING_TOKEN") {
			t.Fatalf("the detail does not name the secretRef: %s", got.Detail)
		}
	})
}

// TestProbeStatesAreTheSix keeps the vocabulary closed. A seventh state that
// existed only in a switch somewhere would be a state no screen renders.
func TestProbeStatesAreTheSix(t *testing.T) {
	want := []skillregistry.ProbeState{
		skillregistry.ProbeConnected, skillregistry.ProbeAuthFailed, skillregistry.ProbeTLSFailed,
		skillregistry.ProbeUnreachable, skillregistry.ProbeInvalidResponse, skillregistry.ProbePolicyBlocked,
	}
	for _, s := range want {
		if !s.Valid() {
			t.Fatalf("%s is not valid", s)
		}
	}
	if skillregistry.ProbeState("HEALTHY").Valid() {
		t.Fatal("an unknown state validated")
	}
	if !skillregistry.ProbeConnected.OK() || skillregistry.ProbeUnreachable.OK() {
		t.Fatal("OK does not mean connected")
	}
}
