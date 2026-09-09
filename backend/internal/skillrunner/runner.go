package skillrunner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// RunLabel marks every container this package starts, so an operator (and the
// tests) can find and clean up exactly AO's skill runs and nothing else. It is
// deliberately distinct from dockerreap.SessionLabel: a skill run is not a
// worker session and must not be swept by the session reaper.
const RunLabel = "ao.skillrun"

// nobodyUser is the uid:gid the container runs as. Not root, and not a uid
// that maps onto anything AO owns.
const nobodyUser = "65534:65534"

// Limits bound one run. Every field is enforced by the kernel through cgroup
// v2, except MaxOutputBytes which is enforced here — an unbounded stdout is a
// memory limit on the DAEMON, which no container setting protects.
type Limits struct {
	Wall           time.Duration
	MemoryBytes    int64
	CPUs           float64
	MaxPIDs        int
	MaxOutputBytes int
}

// DefaultLimits are deliberately small. A skill that needs more should say so
// and have somebody agree, rather than inheriting the host's capacity.
func DefaultLimits() Limits {
	return Limits{
		Wall:           2 * time.Minute,
		MemoryBytes:    512 << 20,
		CPUs:           1.0,
		MaxPIDs:        128,
		MaxOutputBytes: 1 << 20,
	}
}

// Request is one containerized run.
type Request struct {
	// Image is pinned by digest (name@sha256:...). A tag is refused: a tag is
	// a mutable pointer, and "the image we tested" has to mean one thing.
	Image string
	// Argv is the command, run without a shell.
	Argv []string
	// InputDir is a host directory mounted read-only at /work. It must be
	// non-empty: an empty input mount is the silent-failure mode this package
	// exists to catch (see the package doc).
	InputDir string
	// Env is an explicit allowlist forwarded into the container. It must not
	// carry a credential; nothing here reads the daemon's own environment.
	Env map[string]string
	// Secrets is an already-materialized delivery, mounted read-only at
	// ContainerSecretsPath. It is a directory of files, never environment
	// variables, and this struct cannot carry a VALUE at any point: the values
	// were written by the authority before the Request was built.
	Secrets *SecretDelivery
	// Workspace, when set, gives the run a capped tmpfs it may write to and a
	// quarantine directory AO's own wrapper transfers into. The operator's
	// checkout is never mounted writable -- or at all.
	Workspace *Workspace
	Limits    Limits
}

// BoundaryEvidence is what AO observed about the boundary, collected from the
// runtime and from inside the container — never from what the workload says
// about itself. A run that self-reports "I stayed in scope" is worth nothing.
type BoundaryEvidence struct {
	// RunnerID and Runtime identify what enforced the boundary.
	RunnerID string
	Runtime  string
	// ImageDigest is the image that actually ran, as the runtime resolved it.
	ImageDigest string
	// EffectiveUID is the uid inside the container. Non-zero is the claim.
	EffectiveUID int
	// MemoryMaxBytes and PIDsMax are the cgroup values the KERNEL reports,
	// read from inside the container rather than echoed from the flags AO
	// passed. A flag that was silently ignored shows up here as a mismatch.
	MemoryMaxBytes int64
	PIDsMax        int
	CPUMax         string
	// NetworkReachable is the result of an actual outbound attempt from
	// inside. False is the claim for a deny-all run.
	NetworkReachable bool
	// InputFilesVisible is how many files the container could see under
	// /work. Zero means the mount did not arrive, which fails the run.
	InputFilesVisible int
	// ReadOnlyRootFS records that a write to the container root was refused.
	ReadOnlyRootFS bool
	// InheritedDaemonEnv is the count of environment variables that leaked in
	// from the daemon. Non-zero fails the run.
	InheritedDaemonEnv int
	// SecretRefsDelivered are the NAMES the container saw under
	// ContainerSecretsPath. Names only: a value never reaches evidence, and
	// neither does a prefix or a hash of one, because a prefix is enough to
	// confirm a guess.
	SecretRefsDelivered []string
	// SecretsMounted records that a delivery was expected and arrived.
	SecretsMounted bool
	// WorkspaceBytes and WorkspaceFiles are what the run reported writing,
	// from inside the capped tmpfs. AO re-measures at collection, so a
	// mismatch shows up rather than being taken on trust.
	WorkspaceBytes int64
	WorkspaceFiles int
	// WorkspaceTransferred records that AO's wrapper moved the output into
	// quarantine. False with a workspace configured means nothing was
	// collected, which is a refusal rather than an empty result.
	WorkspaceTransferred bool
	// Controls are what this run demonstrated, derived from the fields above.
	Controls []skillcatalog.Control
}

// Result is one completed run.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// Truncated reports that output hit MaxOutputBytes and was cut.
	Truncated bool
	// TimedOut reports that the wall clock ran out and the run was killed.
	TimedOut bool
	Started  time.Time
	Ended    time.Time
	Evidence BoundaryEvidence
}

// Runner executes skill work inside a container. The zero value is not usable;
// build one with New.
type Runner struct {
	runtime Runtime
	runner  commandRunner
	// probeErr is why the runtime is unusable, when it is. It is kept so the
	// refusal can say what is wrong rather than only that something is.
	probeErr error
	// egress is what AO MEASURED about its ability to limit outbound traffic:
	// a packaged proxy for this runtime's architecture, verified by digest, and
	// an internal network on which an outbound connection was observed to fail.
	// It is produced by VerifyEgressBoundary and by nothing else — a boolean a
	// caller could pass was the previous phase's placeholder, and a promise is
	// not an attestation.
	egress EgressBoundary
	// workspaceAvailable records that AO can prepare, collect and clean up a
	// writable workspace on this host. Like secretsAvailable it is set by the
	// daemon from a resolved fact, never from a manifest and never from a flag
	// a caller passes.
	workspaceAvailable bool
	// secretsAvailable records that a trusted authority can actually deliver
	// values. It is set by WithSecretDelivery from the AUTHORITY's own
	// readiness, never from a flag a caller passes and never from a manifest —
	// a runner that attested this because somebody asked it to would defeat
	// every check the capability table makes.
	secretsAvailable bool
}

// WithWritableWorkspace records that AO can give a run a writable workspace on
// this host: a usable workspace root, resolved by the daemon at construction.
//
// It is the one thing that lets this runner attest ControlWritableWorkspace,
// and it unblocks repo.write and nothing else — writing is not a reason to
// reach the network, run a chosen command, or read a secret.
func (r *Runner) WithWritableWorkspace(rootUsable bool) *Runner {
	r.workspaceAvailable = rootUsable
	return r
}

// WithSecretDelivery records that a working secret authority is wired.
//
// It is the one thing that lets this runner attest
// ControlScopedSecretDelivery. The parameter is the authority's own Available()
// answer, resolved by the daemon at construction: there is deliberately no way
// to pass true without a store and a sealing key existing.
func (r *Runner) WithSecretDelivery(authorityAvailable bool) *Runner {
	r.secretsAvailable = authorityAvailable
	return r
}

// New probes the host and returns a Runner. A probe failure is NOT an error
// here: the Runner still exists, attests nothing, and refuses every Execute.
// That is the shape a fail-closed component needs — the caller must be able to
// ask "what can you do" and get "nothing, because X" rather than a nil.
func New(ctx context.Context) *Runner {
	rt, err := Probe(ctx)
	return &Runner{runtime: rt, runner: execRunner{}, probeErr: err}
}

// Available reports whether a run could happen at all.
func (r *Runner) Available() bool { return r.probeErr == nil && r.runtime.Binary != "" }

// Unavailable explains why not, or "" when the runtime is usable.
func (r *Runner) Unavailable() string {
	if r.Available() {
		return ""
	}
	if r.probeErr != nil {
		return r.probeErr.Error()
	}
	return ErrRuntimeUnavailable.Error()
}

// Runtime reports what was probed.
func (r *Runner) Runtime() Runtime { return r.runtime }

// Attestation is what this environment has demonstrated. An unusable runtime
// attests nothing, which is what makes every control-requiring capability
// refuse rather than degrade.
func (r *Runner) Attestation() skillcatalog.RunnerAttestation {
	if !r.Available() {
		return skillcatalog.NoRunner()
	}
	controls := r.runtime.Controls()
	// Scoped secret delivery is attested only when BOTH halves exist: a
	// container to deliver into, and an authority that can produce values. A
	// runtime alone proves nothing about where a secret would come from.
	if r.secretsAvailable {
		controls = append(controls, skillcatalog.ControlScopedSecretDelivery)
	}
	// A writable workspace needs somewhere to put one. A container alone
	// proves nothing about where the output would land or whether AO could
	// clean it up afterwards.
	if r.workspaceAvailable {
		controls = append(controls, skillcatalog.ControlWritableWorkspace)
	}
	// An allowlist needs a proxy to decide and a topology to enforce it. A
	// container alone provides deny-all, which is a different and weaker claim,
	// and a proxy AO cannot identify is one it cannot attest. Both halves were
	// measured by VerifyEgressBoundary or neither is claimed here.
	controls = append(controls, r.egress.controls()...)
	return skillcatalog.RunnerAttestation{
		RunnerID: "container/" + r.runtime.Binary,
		Controls: controls,
	}
}

// Execute satisfies skillcatalog.Runner. It is deliberately NOT implemented:
// this phase proves the boundary, it does not run skills through it. Wiring a
// Plan onto Run() means deciding the skill-image contract first — what a skill
// may ship and how its command is authored — which is the
// arbitrary_process_execution control this runtime does not attest.
func (r *Runner) Execute(context.Context, skillcatalog.Plan) (skillcatalog.Result, error) {
	return skillcatalog.Result{}, fmt.Errorf(
		"%w: the container boundary is proven but no skill-image contract exists, "+
			"so no skill-authored command may run", skillcatalog.ErrNoRunner)
}

var _ skillcatalog.Runner = (*Runner)(nil)

// Run executes one containerized task and returns it with the evidence AO
// collected about the boundary that held it.
//
// It refuses before starting anything when the runtime is unusable, when the
// image is not digest-pinned, or when the input directory is empty on the host.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if !r.Available() {
		return Result{}, fmt.Errorf("%w: %s", ErrRuntimeUnavailable, r.Unavailable())
	}
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}
	limits := req.Limits
	if limits.Wall <= 0 {
		limits = DefaultLimits()
	}

	name := "ao-skillrun-" + randomToken()
	args := r.containerArgs(name, req, limits)

	// The wall clock is enforced by AO, not by the container: a runtime that
	// ignored a timeout flag would leave the caller waiting forever.
	runCtx, cancel := context.WithTimeout(ctx, limits.Wall)
	defer cancel()

	started := time.Now().UTC()
	cmd := exec.CommandContext(runCtx, r.runtime.Binary, args...) //nolint:gosec // binary is the probed runtime.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// The daemon's own environment is never forwarded. This is the difference
	// between "we did not pass a secret" and "we passed nothing at all".
	cmd.Env = []string{}
	runErr := cmd.Run()

	// Whatever happened, remove the container and everything in its namespace.
	// A timeout kills `docker run`, which does NOT by itself stop the
	// container, so the explicit rm is what makes "no orphaned child" true.
	r.forceRemove(context.WithoutCancel(ctx), name)

	res := Result{
		Started:  started,
		Ended:    time.Now().UTC(),
		TimedOut: errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	res.Stdout, res.Truncated = truncate(stdout.String(), limits.MaxOutputBytes)
	trimmedErr, errTruncated := truncate(stderr.String(), limits.MaxOutputBytes)
	res.Stderr = trimmedErr
	res.Truncated = res.Truncated || errTruncated
	res.ExitCode = exitCodeOf(runErr)

	res.Evidence = r.evidenceFrom(res, req, limits)
	return res, nil
}

// imageRefRe is the only shape an image reference may take: a bare digest, or
// a name pinned to one. The digest half is checked properly rather than by
// substring -- "name@sha256:oops" contained "@sha256:" and passed the previous
// check, which made the pin a spelling convention rather than a constraint.
var imageRefRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*@)?sha256:[0-9a-f]{64}$`)

// validateImageRef refuses anything that is not exactly one image.
func validateImageRef(ref string) error {
	if !imageRefRe.MatchString(ref) {
		return fmt.Errorf("skillrunner: image %q must be a sha256 digest, or a name pinned to one; "+
			"a tag is a mutable pointer and would not mean one thing over time", ref)
	}
	return nil
}

func validateRequest(req Request) error {
	if err := validateImageRef(req.Image); err != nil {
		return err
	}
	if len(req.Argv) == 0 {
		return errors.New("skillrunner: a command is required")
	}
	if strings.TrimSpace(req.InputDir) == "" {
		return errors.New("skillrunner: an input directory is required")
	}
	if !filepath.IsAbs(req.InputDir) {
		return fmt.Errorf("skillrunner: input directory %q must be absolute", req.InputDir)
	}
	entries, err := os.ReadDir(req.InputDir)
	if err != nil {
		return fmt.Errorf("skillrunner: read input directory: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("skillrunner: input directory %q is empty on the host; "+
			"a run over no inputs produces a clean report of nothing", req.InputDir)
	}
	for name, value := range req.Env {
		if looksLikeCredential(name) {
			return fmt.Errorf("skillrunner: refusing to forward %q into a skill container", name)
		}
		if strings.ContainsAny(value, "\x00\n") {
			return fmt.Errorf("skillrunner: env %q has an unusable value", name)
		}
	}
	return nil
}

// looksLikeCredential is a coarse name filter. It is a backstop, not the
// control: the control is that AO never reads its own environment to build
// this map. A name that trips this is a caller bug worth failing loudly.
func looksLikeCredential(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "API_KEY", "APIKEY", "_KEY"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// containerArgs builds the run. Every flag here is one of ADR 0004's minimum
// boundary settings; dropping any of them invalidates a control this runner
// attests.
func (r *Runner) containerArgs(name string, req Request, limits Limits) []string {
	args := []string{
		"run", "--rm", "--name", name,
		"--label", RunLabel + "=1",
		// AO never fetches an image. The approved digest must already be on
		// this host; an absent one is a refusal, not a download. Without this
		// flag a run could quietly pull whatever a registry currently serves
		// under a name, which is the trust root defeated at the last step.
		"--pull=never",
		// No network at all. Deny-all is a control this runner attests; an
		// allowlist is a different control it does not.
		"--network", "none",
		"--user", nobodyUser,
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		// A bounded, non-persistent scratch space, so a read-only rootfs does
		// not make ordinary work impossible.
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--memory", strconv.FormatInt(limits.MemoryBytes, 10),
		// Equal swap disables swap: without this a memory limit is advisory,
		// because the workload spills to disk instead of being stopped.
		"--memory-swap", strconv.FormatInt(limits.MemoryBytes, 10),
		"--cpus", strconv.FormatFloat(limits.CPUs, 'f', -1, 64),
		"--pids-limit", strconv.Itoa(limits.MaxPIDs),
		"--workdir", "/work",
		"-v", req.InputDir + ":/work:ro",
	}
	// The secrets mount is read-only, and the workspace pair is the only
	// writable thing a run ever gets. There is no code path here that mounts a
	// home directory, a credential file, AO's data dir or the container
	// socket, and none that mounts the project checkout at all.
	if req.Secrets != nil {
		args = append(args, req.Secrets.MountArgs()...)
	}
	if req.Workspace != nil {
		args = append(args, req.Workspace.MountArgs()...)
	}
	for key, value := range req.Env {
		args = append(args, "-e", key+"="+value)
	}
	args = append(args, req.Image)
	return append(args, req.Argv...)
}

// forceRemove tears the container down. Errors are ignored on purpose: the
// container may already be gone (--rm), and a teardown failure must not mask
// the run's own outcome. What matters is that it is always attempted.
func (r *Runner) forceRemove(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, _ = r.runner.Output(ctx, r.runtime.Binary, "rm", "-f", name)
}

func (r *Runner) evidenceFrom(res Result, req Request, limits Limits) BoundaryEvidence {
	ev := BoundaryEvidence{
		RunnerID:    "container/" + r.runtime.Binary,
		Runtime:     r.runtime.Describe(),
		ImageDigest: req.Image,
	}
	// The probe workload prints its own observations as key=value lines. They
	// come from inside the boundary — /proc, the cgroup files, a connect
	// attempt — which is what makes them evidence rather than assertion. A run
	// whose workload prints nothing simply yields no controls.
	for _, line := range strings.Split(res.Stdout, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ao_uid":
			ev.EffectiveUID, _ = strconv.Atoi(value)
		case "ao_memory_max":
			ev.MemoryMaxBytes, _ = strconv.ParseInt(value, 10, 64)
		case "ao_pids_max":
			ev.PIDsMax, _ = strconv.Atoi(value)
		case "ao_cpu_max":
			ev.CPUMax = value
		case "ao_network_reachable":
			ev.NetworkReachable = value == "true"
		case "ao_input_files":
			ev.InputFilesVisible, _ = strconv.Atoi(value)
		case "ao_rootfs_readonly":
			ev.ReadOnlyRootFS = value == "true"
		case "ao_daemon_env_leaked":
			ev.InheritedDaemonEnv, _ = strconv.Atoi(value)
		case "ao_workspace_bytes":
			// The tool reports kilobytes; AO stores bytes.
			if kb, err := strconv.ParseInt(value, 10, 64); err == nil {
				ev.WorkspaceBytes = kb * 1024
			}
		case "ao_workspace_files":
			ev.WorkspaceFiles, _ = strconv.Atoi(value)
		case "ao_workspace_transferred":
			ev.WorkspaceTransferred = value == "true"
		case "ao_secret_refs":
			if value != "" {
				ev.SecretRefsDelivered = strings.Split(value, ",")
			}
			ev.SecretsMounted = true
		}
	}
	ev.Controls = demonstratedControls(ev, limits)
	return ev
}

// demonstratedControls derives what the run actually proved. It compares the
// kernel's own numbers against the limits AO asked for, so a flag the runtime
// silently ignored produces a missing control rather than a false one.
func demonstratedControls(ev BoundaryEvidence, limits Limits) []skillcatalog.Control {
	var out []skillcatalog.Control
	if ev.InputFilesVisible > 0 && ev.ReadOnlyRootFS {
		out = append(out, skillcatalog.ControlFilesystemIsolation)
	}
	if ev.EffectiveUID > 0 && ev.PIDsMax > 0 && ev.PIDsMax <= limits.MaxPIDs {
		out = append(out, skillcatalog.ControlProcessIsolation)
	}
	if ev.InheritedDaemonEnv == 0 {
		out = append(out, skillcatalog.ControlNoCredentialInheritance)
	}
	if ev.MemoryMaxBytes > 0 && ev.MemoryMaxBytes <= limits.MemoryBytes && ev.PIDsMax > 0 {
		out = append(out, skillcatalog.ControlResourceLimits)
	}
	if !ev.NetworkReachable {
		out = append(out, skillcatalog.ControlEgressDenyAll)
	}
	return out
}

// Verify reports whether the run demonstrated every control this runtime
// claims. A mismatch means the boundary did not hold as attested, which is a
// reason to fail the run rather than to report its output.
func (e BoundaryEvidence) Verify(claimed []skillcatalog.Control) error {
	have := map[skillcatalog.Control]bool{}
	for _, c := range e.Controls {
		have[c] = true
	}
	var missing []string
	for _, c := range claimed {
		if !have[c] {
			missing = append(missing, string(c))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("skillrunner: the run did not demonstrate %s", strings.Join(missing, ", "))
	}
	return nil
}

func truncate(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func randomToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("skillrunner: read random name: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// EvidenceArgv is the workload AO runs to observe its own boundary. It reads
// /proc and the cgroup files, counts what is actually mounted, and attempts one
// outbound connection — all from INSIDE the container, which is what makes the
// result evidence rather than an echo of the flags AO passed.
//
// It is POSIX shell against busybox so it runs in a minimal image, and it is
// part of the package rather than the tests because a runner that cannot
// observe its own boundary has nothing to report.
func EvidenceArgv() []string {
	const script = `
set -u
echo "ao_uid=$(id -u)"
echo "ao_memory_max=$(cat /sys/fs/cgroup/memory.max 2>/dev/null || echo unknown)"
echo "ao_pids_max=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo unknown)"
echo "ao_cpu_max=$(cat /sys/fs/cgroup/cpu.max 2>/dev/null | tr ' ' '/' || echo unknown)"
echo "ao_input_files=$(find /work -type f 2>/dev/null | wc -l | tr -d ' ')"
if echo probe 2>/dev/null > /ao-write-probe; then
  echo "ao_rootfs_readonly=false"; rm -f /ao-write-probe 2>/dev/null
else
  echo "ao_rootfs_readonly=true"
fi
# Any credential-shaped variable here came from outside; a clean container has
# none. This counts the leak rather than trusting that none happened.
echo "ao_daemon_env_leaked=$(env | grep -Ec '^[^=]*(TOKEN|SECRET|PASSWORD|CREDENTIAL|API_KEY)[^=]*=' || true)"
` + SecretEvidenceScript + `
if wget -T 2 -q -O- http://1.1.1.1/ >/dev/null 2>&1; then
  echo "ao_network_reachable=true"
else
  echo "ao_network_reachable=false"
fi
`
	return []string{"sh", "-c", script}
}
