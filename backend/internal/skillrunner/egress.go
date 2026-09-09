package skillrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// egress.go — what has to be true before AO says it can limit outbound traffic.
//
// Phase 7 built the proxy and proved the topology. Phase 8 makes the ATTESTATION
// depend on both being real on THIS host, rather than on a boolean the daemon
// passes in. The difference matters: `WithEgressAllowlist(true)` was a promise,
// and a promise is what the capability table must never accept — the whole point
// of an attestation is that somebody measured something.
//
// Two facts, measured, and the control is attested only when both hold:
//
//  1. THE RIGHT PROXY IS PRESENT. Packaged in this build, for the architecture
//     the container runtime actually runs, hashing to the digest provenance.json
//     records. Not "a proxy exists somewhere on the host".
//
//  2. THE BOUNDARY HOLDS. AO creates an --internal network, puts a container on
//     it, and watches an outbound connection FAIL. The topology is the control;
//     the proxy only decides. A host where that connection succeeds has no
//     boundary to enforce an allowlist on, whatever the proxy would have said.
//
// Nothing here contacts an external service. The addresses probed are reserved
// (RFC 5737 TEST-NET-1 and the link-local metadata address) and on an --internal
// network the kernel refuses them before a packet is built.

// EgressBoundary is what AO measured about its ability to enforce an allowlist.
// The zero value attests nothing, which is the state of a daemon that never
// asked.
type EgressBoundary struct {
	// ProxyDigest, ProxyArch and ProxyVersion identify the artifact AO would
	// stage. They are recorded so "which proxy enforced this" is answerable
	// after the fact, not only at the moment of the check.
	ProxyDigest  string
	ProxyArch    string
	ProxyVersion string
	// ProxyPackaged records that the artifact was selected and its digest
	// verified.
	ProxyPackaged bool
	// InternalNetwork records that AO created an --internal network and the
	// runtime confirmed it was internal. A runtime that cannot make one has no
	// topology to hold the boundary.
	InternalNetwork bool
	// OutboundBlocked records that a container on that network could NOT reach
	// out. This is the measurement, and false is a refusal: it means the
	// network AO created is not the one it thinks it created.
	OutboundBlocked bool
	// Observed is what the probe container actually printed, kept as evidence.
	Observed string
	// Err is why the boundary is unusable, when it is.
	Err error
}

// OK reports whether the allowlist may be attested. Every field must hold;
// there is no partial credit, because a proxy with no boundary and a boundary
// with no proxy both enforce nothing.
func (b EgressBoundary) OK() bool {
	return b.Err == nil && b.ProxyPackaged && b.InternalNetwork && b.OutboundBlocked
}

// Explain says why not, in one line an operator can act on.
func (b EgressBoundary) Explain() string {
	switch {
	case b.OK():
		return ""
	case b.Err != nil:
		return b.Err.Error()
	case !b.ProxyPackaged:
		return "no verified egress proxy for this runtime"
	case !b.InternalNetwork:
		return "the container runtime could not create an internal network"
	case !b.OutboundBlocked:
		return "a container on AO's internal network still reached out, so there is no boundary to enforce"
	default:
		return "the egress boundary was not established"
	}
}

// Describe renders the boundary for a log line.
func (b EgressBoundary) Describe() string {
	if !b.OK() {
		return "egress allowlist unavailable: " + b.Explain()
	}
	return fmt.Sprintf("egress allowlist available: proxy v%s linux/%s sha256:%s, outbound blocked on an internal network",
		b.ProxyVersion, b.ProxyArch, b.ProxyDigest)
}

// WithEgressBoundary records a MEASURED boundary. It replaces the boolean the
// previous phase accepted: the only way to produce an EgressBoundary that
// passes OK() is to have run VerifyEgressBoundary against a real runtime.
func (r *Runner) WithEgressBoundary(b EgressBoundary) *Runner {
	r.egress = b
	return r
}

// EgressBoundaryStatus returns what was measured, for a caller that wants to
// explain the refusal rather than only observe it.
func (r *Runner) EgressBoundaryStatus() EgressBoundary { return r.egress }

// egressProbeTimeout bounds the whole check. A wedged runtime must make egress
// unavailable, not hang daemon startup.
const egressProbeTimeout = 45 * time.Second

// unroutableProbes are the addresses the probe container tries to reach.
//
// 192.0.2.1 is RFC 5737 TEST-NET-1, reserved for documentation and routed by
// nobody. 169.254.169.254 is where a cloud provider hands out the host's own
// credentials, and is the single address a skill container most needs not to
// reach. Neither is an external service: on an --internal network there is no
// default route, so the kernel answers "Network unreachable" locally.
var unroutableProbes = []string{"192.0.2.1", "169.254.169.254"}

// VerifyEgressBoundary measures whether this host can enforce an outbound
// allowlist, and returns what it found.
//
// It never returns an error separately from the value: a caller must be able to
// hand the result straight to WithEgressBoundary and get a runner that refuses
// with a reason, rather than having to decide what a nil boundary means.
func (r *Runner) VerifyEgressBoundary(ctx context.Context) EgressBoundary {
	return r.verifyEgressBoundary(ctx, proxybin.Embedded())
}

// verifyEgressBoundary takes the store so this package's negative tests can
// measure against a build that packages nothing, the wrong architecture, or a
// corrupted artifact.
//
// The artifact is checked FIRST and short-circuits, on purpose. It is the cheap
// half, and a build that carries no proxy can never enforce an allowlist however
// good this host's topology is — spending a container run at every daemon start
// to measure something that cannot be used is work nobody asked for. The live
// tests reach measureNetworkBoundary directly, so the topology stays measurable
// on a development build even though production stops before it.
func (r *Runner) verifyEgressBoundary(ctx context.Context, store proxybin.Store) EgressBoundary {
	b := r.measureProxyArtifact(store)
	if b.Err != nil || !b.ProxyPackaged {
		return b
	}
	return r.measureNetworkBoundary(ctx, b)
}

// measureProxyArtifact answers half one: is the right proxy present, for the
// architecture the RUNTIME runs, hashing to what provenance.json records.
func (r *Runner) measureProxyArtifact(store proxybin.Store) EgressBoundary {
	var b EgressBoundary
	if !r.Available() {
		b.Err = fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
		return b
	}
	goarch, err := proxybin.ArchFromRuntime(r.runtime.Architecture)
	if err != nil {
		b.Err = err
		return b
	}
	b.ProxyArch = goarch
	artifact, _, err := store.Select(goarch)
	if err != nil {
		b.Err = err
		return b
	}
	b.ProxyPackaged = true
	b.ProxyDigest = artifact.SHA256
	b.ProxyVersion = store.Provenance().ArtifactVersion
	return b
}

// measureNetworkBoundary answers half two: does an outbound connection from a
// container on AO's internal network actually fail. It runs against a real
// runtime and cleans up after itself on every exit path.
func (r *Runner) measureNetworkBoundary(ctx context.Context, b EgressBoundary) EgressBoundary {
	if !r.Available() {
		b.Err = fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
		return b
	}
	probeCtx, cancel := context.WithTimeout(ctx, egressProbeTimeout)
	defer cancel()
	contract, err := r.resolveProbeContract(probeCtx, ToolStaticScan)
	if err != nil {
		b.Err = fmt.Errorf("cannot measure the egress boundary: %w", err)
		return b
	}

	network := "ao-egress-check-" + randomToken()
	if _, err := r.runner.Output(probeCtx, r.runtime.Binary, "network", "create",
		"--internal", "--label", RunLabel+"=1", network); err != nil {
		b.Err = fmt.Errorf("the container runtime could not create an internal network: %w", err)
		return b
	}
	defer func() {
		// Best effort, and unconditional: a leaked network is AO's litter, and
		// the label makes it AO's to remove.
		removeCtx, removeCancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
		defer removeCancel()
		_, _ = r.runner.Output(removeCtx, r.runtime.Binary, "network", "rm", network)
	}()

	internal, err := r.runner.Output(probeCtx, r.runtime.Binary, "network", "inspect",
		network, "--format", "{{.Internal}}")
	if err != nil || strings.TrimSpace(string(internal)) != "true" {
		b.Err = fmt.Errorf("the runtime created %q but does not report it as internal (%q); "+
			"the topology AO relies on is not there", network, strings.TrimSpace(string(internal)))
		return b
	}
	b.InternalNetwork = true

	// The probe reports what it observed rather than exiting on it, so a
	// FAILURE to connect is a normal exit AO can read. `nc -w 2 -z` returns
	// non-zero on an unreachable address, which is the outcome under test.
	script := "for a in " + strings.Join(unroutableProbes, " ") + "; do " +
		"if nc -w 2 -z $a 80 2>/dev/null; then echo \"REACHED $a\"; else echo \"blocked $a\"; fi; done"
	out, runErr := r.runner.Output(probeCtx, r.runtime.Binary, "run", "--rm",
		"--label", RunLabel+"=1", "--network", network,
		"--user", nobodyUser, "--read-only", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--network-alias", "ao-egress-check",
		contract.PinnedRef(), "sh", "-c", script)
	b.Observed = strings.TrimSpace(string(out))
	if runErr != nil {
		b.Err = fmt.Errorf("the egress probe container did not run: %w", runErr)
		return b
	}
	if b.Observed == "" {
		b.Err = errors.New("the egress probe container reported nothing, so nothing was measured")
		return b
	}
	if strings.Contains(b.Observed, "REACHED") {
		// Do NOT set OutboundBlocked. The host reached an address that has no
		// route on an internal network, which means the network AO created is
		// not the one it thinks it created.
		return b
	}
	for _, addr := range unroutableProbes {
		if !strings.Contains(b.Observed, "blocked "+addr) {
			b.Err = fmt.Errorf("the egress probe did not report on %s; measured: %q", addr, b.Observed)
			return b
		}
	}
	b.OutboundBlocked = true
	return b
}

// egressControls is what a measured boundary adds to an attestation. It is one
// control and deliberately not two: net.active_scan needs
// arbitrary_process_execution as well, because a forward proxy can express "may
// open a connection to this host" and cannot express "may probe this host for
// weaknesses".
func (b EgressBoundary) controls() []skillcatalog.Control {
	if !b.OK() {
		return nil
	}
	return []skillcatalog.Control{skillcatalog.ControlEgressAllowlist}
}
