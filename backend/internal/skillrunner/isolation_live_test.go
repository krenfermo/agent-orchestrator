package skillrunner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// The tests in this file drive a REAL container runtime. They are the evidence
// behind every control this package attests: ADR 0004's rule is that a control
// may be claimed only when a test demonstrates it, and these are those tests.
//
// They skip when no runtime is available, because a machine without one cannot
// prove anything about containment — but the fail-closed behavior is covered
// unconditionally in probe_test.go, so the property that matters most on such a
// machine ("refuse, never fall back to the host") is still tested there.
//
// Everything here is local and synthetic. Nothing reaches a real host; the one
// outbound attempt targets a literal address specifically to observe that it
// FAILS.

// liveRunner returns a Runner over the host's real runtime, or skips.
func liveRunner(t *testing.T) *Runner {
	t.Helper()
	if testing.Short() {
		t.Skip("live container tests are skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := New(ctx)
	if !r.Available() {
		t.Skipf("no container runtime on this host: %s", r.Unavailable())
	}
	return r
}

// pinnedImage resolves a small local image to a digest. Pulling is avoided: a
// test that needs the network to prove the network is off would be silly.
func pinnedImage(t *testing.T, r *Runner) string {
	t.Helper()
	out, err := exec.Command(r.runtime.Binary, "image", "inspect", "alpine:3.19",
		"--format", "{{.Id}}").Output()
	if err != nil {
		t.Skipf("alpine:3.19 is not present locally, and these tests do not pull: %v", err)
	}
	id := strings.TrimSpace(string(out))
	if !strings.HasPrefix(id, "sha256:") {
		t.Skipf("unexpected image id %q", id)
	}
	return "alpine@" + id
}

// workspace stages a synthetic input tree plus a "secret" the run must not be
// able to reach, in a sibling directory that is deliberately NOT mounted.
func workspace(t *testing.T) (inputDir, secretPath string) {
	t.Helper()
	// The container runtime may be a VM that only shares certain host paths,
	// so stage under the repository, which is by definition shared here.
	base, err := os.MkdirTemp(repoScratchRoot(t), "skillrunner-")
	if err != nil {
		t.Fatalf("stage workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	inputDir = filepath.Join(base, "input")
	secretDir := filepath.Join(base, "not-mounted")
	for _, d := range []string{inputDir, secretDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(inputDir, "app.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatalf("write input: %v", err)
	}
	secretPath = filepath.Join(secretDir, "creds.txt")
	if err := os.WriteFile(secretPath, []byte("SYNTHETIC-SECRET-NEVER-REAL\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return inputDir, secretPath
}

// repoScratchRoot is a directory inside the repository checkout. A container
// runtime backed by a VM shares only configured host paths, and a mount from
// an unshared path arrives EMPTY and silent — the failure this whole package
// is built to catch — so the tests stage where the runtime can definitely see.
func repoScratchRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("create testdata: %v", err)
	}
	return root
}

func runEvidence(t *testing.T, r *Runner, inputDir string, limits Limits) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image:    pinnedImage(t, r),
		Argv:     EvidenceArgv(),
		InputDir: inputDir,
		Limits:   limits,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("evidence run exited %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

// Requirements 1, 2, 3, 4 and 6 in one run, because they are properties of one
// boundary and asserting them separately would start five containers to learn
// the same thing.
func TestLive_BoundaryHoldsAndReportsItsOwnEvidence(t *testing.T) {
	r := liveRunner(t)
	inputDir, _ := workspace(t)
	limits := Limits{
		Wall: 90 * time.Second, MemoryBytes: 256 << 20, CPUs: 1,
		MaxPIDs: 64, MaxOutputBytes: 64 << 10,
	}
	res := runEvidence(t, r, inputDir, limits)
	ev := res.Evidence

	// 1. Filesystem: the inputs arrived, and the root filesystem is immutable.
	if ev.InputFilesVisible < 1 {
		t.Fatalf("the input mount arrived empty; a run over no inputs would report a clean audit of nothing\n%s", res.Stdout)
	}
	if !ev.ReadOnlyRootFS {
		t.Fatal("the container root filesystem was writable")
	}
	// 2. Credentials: nothing credential-shaped came in from outside.
	if ev.InheritedDaemonEnv != 0 {
		t.Fatalf("%d credential-shaped variables leaked into the container", ev.InheritedDaemonEnv)
	}
	// 3. Network denied by default.
	if ev.NetworkReachable {
		t.Fatal("outbound network was reachable from a --network none run")
	}
	// 4. The kernel's own numbers match what AO asked for.
	if ev.EffectiveUID == 0 {
		t.Fatal("the run was root inside the container")
	}
	if ev.MemoryMaxBytes != limits.MemoryBytes {
		t.Fatalf("memory.max = %d, want %d", ev.MemoryMaxBytes, limits.MemoryBytes)
	}
	if ev.PIDsMax != limits.MaxPIDs {
		t.Fatalf("pids.max = %d, want %d", ev.PIDsMax, limits.MaxPIDs)
	}
	// 6. The evidence is AO's, and it demonstrates exactly the runtime's claim.
	if ev.RunnerID == "" || ev.Runtime == "" || !strings.HasPrefix(ev.ImageDigest, "alpine@sha256:") {
		t.Fatalf("evidence is not attributable: %#v", ev)
	}
	if err := ev.Verify(r.runtime.Controls()); err != nil {
		t.Fatalf("the run did not demonstrate what the runtime claims: %v", err)
	}
	// And it must not have demonstrated what nothing here implements.
	for _, forbidden := range []skillcatalog.Control{
		skillcatalog.ControlEgressAllowlist,
		skillcatalog.ControlWritableWorkspace,
		skillcatalog.ControlScopedSecretDelivery,
		skillcatalog.ControlArbitraryProcessExecution,
	} {
		for _, got := range ev.Controls {
			if got == forbidden {
				t.Fatalf("the run claimed %s", forbidden)
			}
		}
	}
}

// Requirement 1, stated as the attack rather than as a property: a run must not
// be able to read a file next to its inputs that AO did not mount.
func TestLive_CannotReadAFileOutsideTheInputMount(t *testing.T) {
	r := liveRunner(t)
	inputDir, secretPath := workspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image: pinnedImage(t, r),
		// Try the secret by its real host path, and try to walk out of /work.
		Argv: []string{"sh", "-c",
			"cat " + secretPath + " 2>&1; cat /work/../not-mounted/creds.txt 2>&1; ls /Users 2>&1; true"},
		InputDir: inputDir,
		Limits:   DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	combined := res.Stdout + res.Stderr
	if strings.Contains(combined, "SYNTHETIC-SECRET-NEVER-REAL") {
		t.Fatalf("the run read a file outside its inputs:\n%s", combined)
	}
	if !strings.Contains(combined, "No such file") && !strings.Contains(combined, "can't open") {
		t.Fatalf("expected the reads to fail; got:\n%s", combined)
	}
}

// Requirement 2, stated as the attack: a variable in the DAEMON's environment
// must not appear in the container. AO clears the child environment entirely,
// so this holds whether or not a name looks like a credential.
func TestLive_DoesNotInheritTheDaemonEnvironment(t *testing.T) {
	r := liveRunner(t)
	inputDir, _ := workspace(t)
	t.Setenv("AO_TEST_DAEMON_MARKER", "SYNTHETIC-DAEMON-VALUE")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image:    pinnedImage(t, r),
		Argv:     []string{"sh", "-c", "env"},
		InputDir: inputDir,
		Limits:   DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(res.Stdout, "AO_TEST_DAEMON_MARKER") ||
		strings.Contains(res.Stdout, "SYNTHETIC-DAEMON-VALUE") {
		t.Fatalf("the daemon's environment leaked into the container:\n%s", res.Stdout)
	}
}

// Requirement 4, the half a cgroup cannot enforce: an unbounded stdout is a
// memory limit on the DAEMON, not on the container, so AO caps it itself.
func TestLive_OutputIsTruncatedAtTheCap(t *testing.T) {
	r := liveRunner(t)
	inputDir, _ := workspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := r.Run(ctx, Request{
		Image:    pinnedImage(t, r),
		Argv:     []string{"sh", "-c", "i=0; while [ $i -lt 2000 ]; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; i=$((i+1)); done"},
		InputDir: inputDir,
		Limits: Limits{
			Wall: 45 * time.Second, MemoryBytes: 128 << 20, CPUs: 1,
			MaxPIDs: 32, MaxOutputBytes: 512,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated || len(res.Stdout) != 512 {
		t.Fatalf("output was not capped: truncated=%v len=%d", res.Truncated, len(res.Stdout))
	}
}

// Requirement 5: the wall clock kills the run, and nothing survives it. The
// container spawns children specifically so "no orphan" means something.
func TestLive_TimeoutKillsTheRunAndLeavesNoOrphan(t *testing.T) {
	r := liveRunner(t)
	inputDir, _ := workspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	res, err := r.Run(ctx, Request{
		Image:    pinnedImage(t, r),
		Argv:     []string{"sh", "-c", "sleep 600 & sleep 600 & sleep 600 & wait"},
		InputDir: inputDir,
		Limits: Limits{
			Wall: 5 * time.Second, MemoryBytes: 128 << 20, CPUs: 1,
			MaxPIDs: 32, MaxOutputBytes: 4096,
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TimedOut {
		t.Fatal("a run that outlived its wall clock did not report a timeout")
	}
	if elapsed := time.Since(started); elapsed > 60*time.Second {
		t.Fatalf("the timeout did not bound the run: %s", elapsed)
	}
	// Nothing AO started may survive. The label is what makes this checkable
	// without touching anybody else's containers.
	out, err := exec.Command(r.runtime.Binary, "ps", "-a", "--filter", "label="+RunLabel+"=1",
		"--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if leftover := strings.TrimSpace(string(out)); leftover != "" {
		t.Fatalf("containers survived teardown: %s", leftover)
	}
}

// Requirement 7 against the real API surface: a digest-less image is refused
// before anything starts, so "we ran what we tested" stays true.
func TestLive_RefusesATaggedImageWithoutStartingAnything(t *testing.T) {
	r := liveRunner(t)
	inputDir, _ := workspace(t)

	_, err := r.Run(context.Background(), Request{
		Image: "alpine:3.19", Argv: []string{"true"}, InputDir: inputDir, Limits: DefaultLimits(),
	})
	if err == nil || !strings.Contains(err.Error(), "must be a sha256 digest") {
		t.Fatalf("err = %v, want a digest refusal", err)
	}
	out, listErr := exec.Command(r.runtime.Binary, "ps", "-a", "--filter", "label="+RunLabel+"=1",
		"--format", "{{.Names}}").Output()
	if listErr != nil {
		t.Fatalf("list containers: %v", listErr)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a refused request started a container: %s", out)
	}
}

// A runtime that IS available still attests only its five controls, so nothing
// a person could do makes a blocked capability grantable.
func TestLive_AvailableRuntimeUnblocksNothing(t *testing.T) {
	r := liveRunner(t)
	att := r.Attestation()

	if !att.Isolated() {
		t.Fatalf("a live runtime should attest isolation: %#v", att)
	}
	if att.EgressControlled() {
		t.Fatal("deny-all was mistaken for an egress allowlist")
	}
	for _, blocked := range []skillcatalog.Capability{
		skillcatalog.CapRepoWrite, skillcatalog.CapProcessExec, skillcatalog.CapSecretsRead,
		skillcatalog.CapNetEgress, skillcatalog.CapNetActiveScan,
	} {
		spec, ok := blocked.Spec()
		if !ok {
			t.Fatalf("%s has no spec", blocked)
		}
		missing, satisfied := firstMissingControlFor(spec.RequiresControls, att)
		if satisfied {
			t.Fatalf("%s became grantable on a runtime that implements none of its controls", blocked)
		}
		if missing == "" {
			t.Fatalf("%s was refused without naming a control", blocked)
		}
	}
}

func firstMissingControlFor(needs []skillcatalog.Control, att skillcatalog.RunnerAttestation) (skillcatalog.Control, bool) {
	for _, need := range needs {
		if !att.Provides(need) {
			return need, false
		}
	}
	return "", true
}

// listSkillRunContainers reports AO's own skill-run containers, by label, so a
// leak check never touches anybody else's.
func listSkillRunContainers(t *testing.T, r *Runner) string {
	t.Helper()
	out, err := exec.Command(r.runtime.Binary, "ps", "-a",
		"--filter", "label="+RunLabel+"=1", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// reportText renders a report as JSON so a test can assert that a value never
// appears anywhere in it, rather than checking field by field and missing one.
func reportText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	return string(b)
}
