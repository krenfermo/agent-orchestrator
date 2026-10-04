package skillrunner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// The boundary check, against the REAL container runtime on this host.
//
// Nothing here contacts an external service. The two addresses the probe tries
// are reserved — RFC 5737 TEST-NET-1 and the link-local metadata address — and
// on an --internal network there is no default route, so the kernel refuses
// them before a packet is built. Observing that refusal IS the measurement.

// The topology half, measured. This runs on every machine with a runtime,
// including a plain `go test ./...` that packages no proxy: the network
// boundary is a property of the host, and AO must be able to measure it
// separately from whether it happens to be carrying a binary.
func TestVerifyEgressBoundary_MeasuresTheRealTopology(t *testing.T) {
	r := liveRunner(t)
	pinnedImage(t, r) // skips when alpine:3.19 is absent; these tests do not pull.

	// Measured directly, so this holds on a development build too: the
	// topology is a property of the HOST, and AO must be able to observe it
	// separately from whether this binary happens to carry a proxy.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b := r.measureNetworkBoundary(ctx, EgressBoundary{})

	if !b.InternalNetwork {
		t.Fatalf("this runtime could not create an internal network: %s", b.Explain())
	}
	if !b.OutboundBlocked {
		t.Fatalf("a container on AO's internal network reached out; measured %q, explain: %s",
			b.Observed, b.Explain())
	}
	// The evidence names both addresses, so a later reader can see what was
	// actually tried rather than trusting a boolean.
	for _, addr := range unroutableProbes {
		if !strings.Contains(b.Observed, addr) {
			t.Fatalf("the evidence does not mention %s: %q", addr, b.Observed)
		}
	}

	// AO leaves no network behind. This is the teardown half of ADR 0005's
	// negative test 7, checked against the real runtime.
	out, err := exec.Command(r.runtime.Binary, "network", "ls",
		"--filter", "name=ao-egress-check-", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("network ls: %v", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the boundary check left networks behind: %q", out)
	}
}

// The whole gate, on this host, in whichever of its two real configurations
// this build is. Both branches are assertions, not skips: a development build
// MUST refuse, and a release build MUST attest.
func TestVerifyEgressBoundary_AttestationFollowsWhatWasPackaged(t *testing.T) {
	r := liveRunner(t)
	pinnedImage(t, r)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	b := r.VerifyEgressBoundary(ctx)
	att := r.WithEgressBoundary(b).Attestation()

	// The runner keeps what it measured, so a caller can explain the refusal
	// rather than only observe it. This is the whole diagnostic surface: there
	// is no HTTP route or CLI verb for egress in this phase, deliberately.
	if r.EgressBoundaryStatus() != b {
		t.Fatalf("the runner reports %+v, measured %+v", r.EgressBoundaryStatus(), b)
	}
	described := b.Describe()
	if b.OK() && !strings.Contains(described, b.ProxyDigest) {
		t.Fatalf("Describe() = %q, missing the digest that enforced it", described)
	}
	if !b.OK() && !strings.Contains(described, "unavailable") {
		t.Fatalf("Describe() = %q, should say the allowlist is unavailable", described)
	}

	if !proxybin.Embedded().Packaged() {
		// A plain `go build`/`go test`: the topology holds and there is no
		// proxy, so the allowlist stays unattested and net.egress stays refused
		// with the control named. This is the state of every developer machine.
		if b.OK() || att.Provides(skillcatalog.ControlEgressAllowlist) {
			t.Fatal("a build with no packaged proxy attested an egress allowlist")
		}
		if !strings.Contains(b.Explain(), "ao_embed_egress_proxy") {
			t.Fatalf("the refusal should name the build tag: %s", b.Explain())
		}
		netEgress, _ := skillcatalog.CapNetEgress.Spec()
		missing, ok := firstMissingControlFor(netEgress.RequiresControls, att)
		if ok || missing != skillcatalog.ControlEgressAllowlist {
			t.Fatalf("net.egress refused for %q, want egress_allowlist", missing)
		}
		return
	}

	// A release build on a host whose topology holds: the artifact matched the
	// runtime's architecture, its digest verified, and the boundary was
	// observed. Only now is the control attested.
	if !b.OK() {
		t.Fatalf("a packaged build did not establish the boundary: %s", b.Explain())
	}
	if !att.Provides(skillcatalog.ControlEgressAllowlist) {
		t.Fatal("a measured boundary did not attest the allowlist")
	}
	want, err := proxybin.ArchFromRuntime(r.runtime.Architecture)
	if err != nil {
		t.Fatalf("ArchFromRuntime(%q): %v", r.runtime.Architecture, err)
	}
	if b.ProxyArch != want {
		t.Fatalf("selected linux/%s for a runtime that runs %q", b.ProxyArch, r.runtime.Architecture)
	}
	// The digest recorded in the evidence is the one in the committed
	// provenance, so "which proxy would have enforced this" is answerable.
	var recorded string
	for _, a := range proxybin.Embedded().Provenance().Artifacts {
		if a.GOARCH == want {
			recorded = a.SHA256
		}
	}
	if b.ProxyDigest != recorded {
		t.Fatalf("evidence records %s, provenance records %s", b.ProxyDigest, recorded)
	}
	// net.active_scan is still refused: a proxy cannot express "may probe".
	activeScan, _ := skillcatalog.CapNetActiveScan.Spec()
	if missing, ok := firstMissingControlFor(activeScan.RequiresControls, att); ok ||
		missing != skillcatalog.ControlArbitraryProcessExecution {
		t.Fatalf("net.active_scan is missing %q, want arbitrary_process_execution", missing)
	}
}

// The runtime must tell AO what it runs. Everything about selecting a binary
// depends on it, and on macOS it is the VM's architecture rather than the
// daemon's — an arm64 `ao` can be driving an amd64 VM.
func TestProbe_ReportsTheRuntimeArchitecture(t *testing.T) {
	r := liveRunner(t)
	if strings.TrimSpace(r.runtime.Architecture) == "" {
		t.Fatal("the probe learned nothing about the runtime's architecture")
	}
	if _, err := proxybin.ArchFromRuntime(r.runtime.Architecture); err != nil {
		t.Fatalf("this host reports %q, which AO cannot map: %v", r.runtime.Architecture, err)
	}
	if !strings.Contains(r.runtime.Describe(), r.runtime.Architecture) {
		t.Fatalf("Describe() = %q, missing the architecture", r.runtime.Describe())
	}
}
