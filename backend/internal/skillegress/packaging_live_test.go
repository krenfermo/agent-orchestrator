package skillegress

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillegress/proxybin"
)

// packaging_live_test.go — the artifact AO actually carries, in a real
// container, enforcing a real policy against synthetic upstreams.
//
// Phase 7 proved the ENFORCEMENT by cross-compiling the proxy in the test.
// These prove the PACKAGING: that the embedded, digest-pinned binary, extracted
// through proxybin.Stage with the permissions and the mount a run would get,
// starts and enforces exactly the same way — and that the three ways it can be
// wrong (absent, corrupt, wrong architecture) fail before or at the container
// rather than silently.
//
// Nothing external is contacted. Every upstream is a container this test
// started, on a network this test created.

// requirePackaged skips when the build carries no proxy, which is the state of
// a plain `go test ./...`. Run these with:
//
//	scripts/build-egress-proxy.sh
//	go test -tags ao_embed_egress_proxy ./internal/skillegress/...
func requirePackaged(t *testing.T) proxybin.Store {
	t.Helper()
	store := proxybin.Embedded()
	if !store.Packaged() {
		t.Skip("this build carries no egress proxy; run scripts/build-egress-proxy.sh and " +
			"test with -tags ao_embed_egress_proxy")
	}
	return store
}

// stageProxy puts the packaged proxy where a run would find it, through the
// same code path the daemon uses. The root sits under testdata because the
// container runtime here may be a VM that shares only certain host paths.
func (w *world) stageProxy(store proxybin.Store, policy Policy) proxybin.Staged {
	w.t.Helper()
	base, err := filepath.Abs("testdata")
	if err != nil {
		w.t.Fatalf("resolve testdata: %v", err)
	}
	root, err := proxybin.StagingRootFor("", base)
	if err != nil {
		w.t.Fatalf("StagingRootFor: %v", err)
	}
	body, err := policy.Encode()
	if err != nil {
		w.t.Fatalf("Encode: %v", err)
	}
	staged, err := proxybin.Stage(proxybin.StageRequest{
		Store: store, Root: root, RunID: "live-" + w.runToken(),
		RuntimeArch: containerArch(w.t), Policy: body,
	})
	if err != nil {
		w.t.Fatalf("Stage: %v", err)
	}
	w.t.Cleanup(func() { _ = staged.Cleanup() })
	return staged
}

// runToken makes each staged directory unique, the way a real run id is.
// Staging refuses to reuse a directory that already exists — a leftover from a
// crashed attempt is evidence, not scratch space — so a test that reused one
// name would fail on its second run for a reason that has nothing to do with
// what it is testing.
func (w *world) runToken() string {
	w.t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		w.t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// runtimeGOARCH is what the CONTAINER RUNTIME runs, which on macOS is the VM's
// architecture and not the test binary's.
func (w *world) runtimeGOARCH() string {
	w.t.Helper()
	goarch, err := proxybin.ArchFromRuntime(containerArch(w.t))
	if err != nil {
		w.t.Fatalf("ArchFromRuntime: %v", err)
	}
	return goarch
}

// startStagedProxy starts the proxy from a staged mount, with the posture a
// real run gives it: non-root, immutable root filesystem, no capabilities, one
// bounded tmpfs, and the staged directory mounted READ-ONLY.
func (w *world) startStagedProxy(staged proxybin.Staged, hosts ...string) string {
	w.t.Helper()
	name := "ao-eg-pkg-" + w.id
	args := []string{"run", "-d", "--name", name, "--label", egressLabel + "=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only"}
	for _, h := range hosts {
		args = append(args, "--add-host", h)
	}
	args = append(args,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,size=8m,mode=0700,uid=65534,gid=65534,noexec,nosuid,nodev",
		"-v", staged.Dir+":"+proxybin.ContainerDir+":ro",
		"--entrypoint", staged.ContainerBinary(),
		"alpine:3.19",
		"-policy", staged.ContainerPolicy(), "-addr", ":3128", "-decisions", "/tmp/decisions.jsonl")
	w.run(args...)
	w.run("network", "connect", w.extNet, name)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(out), "enforcing") {
			return name
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	w.t.Fatalf("the packaged proxy did not start: %s", logs)
	return ""
}

// The packaged artifact enforces. Same allowlist behaviour phase 7 proved, from
// a binary that was built ahead of time, pinned by digest and extracted through
// staging — with no Go toolchain involved at run time.
func TestLivePackaging_TheEmbeddedProxyEnforcesFromAStagedMount(t *testing.T) {
	store := requirePackaged(t)
	w := newWorld(t)
	allowed := "ao-pkg-ok-" + w.id
	denied := "ao-pkg-no-" + w.id
	allowedIP := w.upstreamContainer(allowed, "ALLOWED-BODY")
	deniedIP := w.upstreamContainer(denied, "DENIED-BODY")

	staged := w.stageProxy(store, w.policy(time.Hour, "http://"+allowed+".example.test:8080"))
	// The digest AO staged is the one the committed provenance records, so
	// "which proxy enforced this" is answerable from the evidence alone.
	var recorded string
	for _, a := range store.Provenance().Artifacts {
		if a.GOARCH == w.runtimeGOARCH() {
			recorded = a.SHA256
		}
	}
	if staged.Artifact.SHA256 != recorded || recorded == "" {
		t.Fatalf("staged %s, provenance records %s", staged.Artifact.SHA256, recorded)
	}

	proxy := w.startStagedProxy(staged,
		allowed+".example.test:"+allowedIP, denied+".example.test:"+deniedIP)

	out := w.skill(`
if wget -T 5 -q -O- http://`+allowed+`.example.test:8080/ >/tmp/a 2>&1; then cat /tmp/a; echo; else echo "GRANTED REFUSED"; fi
if wget -T 5 -q -O- http://`+denied+`.example.test:8080/ >/tmp/d 2>&1; then cat /tmp/d; echo; else echo "DENIED REFUSED"; fi
`, proxy)
	if !strings.Contains(out, "ALLOWED-BODY") {
		t.Fatalf("the granted destination was not reachable through the packaged proxy:\n%s", out)
	}
	if strings.Contains(out, "DENIED-BODY") || !strings.Contains(out, "DENIED REFUSED") {
		t.Fatalf("the ungranted destination was reachable:\n%s", out)
	}

	// The decisions the packaged binary wrote are the same evidence phase 7
	// asserted, which is what makes this the same proxy and not a lookalike.
	decisions := w.proxyDecisions(proxy)
	var sawAllow, sawDeny bool
	for _, d := range decisions {
		if strings.HasPrefix(d.Host, allowed) && d.Allowed {
			sawAllow = true
		}
		if strings.HasPrefix(d.Host, denied) && !d.Allowed {
			sawDeny = true
		}
	}
	if !sawAllow || !sawDeny {
		t.Fatalf("decisions do not record both outcomes: %+v", decisions)
	}
}

// The mount holds exactly the binary and the policy, and neither is writable by
// anybody — checked from INSIDE the container, where uid 65534 owns nothing.
// A read-only mount is the runtime's promise; this is AO's own.
func TestLivePackaging_TheMountIsExactlyTwoUnwritableFiles(t *testing.T) {
	store := requirePackaged(t)
	w := newWorld(t)
	staged := w.stageProxy(store, w.policy(time.Hour, "http://example.test:8080"))

	// Mounted the way the proxy container gets it — read-only, as uid 65534 —
	// but inspected by a workload rather than by the proxy, so the assertions
	// are about the MOUNT and not about what the proxy happens to do with it.
	// A failed output redirect is reported by the shell BEFORE a later 2>/dev/null
	// can apply to it, so each attempt runs in a subshell whose stderr is
	// already discarded. Otherwise the noise lands in the listing.
	script := `
echo "--- listing ---"
ls -1 ` + proxybin.ContainerDir + `
echo "--- writability ---"
if ( echo x > ` + proxybin.ContainerDir + `/ao-egress-proxy ) 2>/dev/null; then echo "BINARY WRITABLE"; else echo "binary not writable"; fi
if ( echo x > ` + proxybin.ContainerDir + `/policy.json ) 2>/dev/null; then echo "POLICY WRITABLE"; else echo "policy not writable"; fi
if ( echo x > ` + proxybin.ContainerDir + `/newfile ) 2>/dev/null; then echo "MOUNT WRITABLE"; else echo "mount not writable"; fi
echo "--- executable ---"
if [ -x ` + proxybin.ContainerDir + `/ao-egress-proxy ]; then echo "binary executable"; else echo "BINARY NOT EXECUTABLE"; fi
`
	raw, _ := exec.Command("docker", "run", "--rm", "--label", egressLabel+"=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,size=8m",
		"-v", staged.Dir+":"+proxybin.ContainerDir+":ro",
		"alpine:3.19", "sh", "-c", script).CombinedOutput()
	out := string(raw)
	for _, want := range []string{
		"ao-egress-proxy", "policy.json",
		"binary not writable", "policy not writable", "mount not writable",
		"binary executable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
	// Nothing else is on the mount. A read-only mount still exposes everything
	// under it, so "the proxy's directory" has to mean one thing.
	listed := between(out, "--- listing ---", "--- writability ---")
	if len(listed) != 2 {
		t.Fatalf("the mount holds %d entries, want exactly 2: %v\n%s", len(listed), listed, out)
	}
}

// between returns the non-empty lines strictly between two markers.
func between(out, start, end string) []string {
	from := strings.Index(out, start)
	to := strings.Index(out, end)
	if from < 0 || to < 0 || to < from {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(out[from+len(start):to], "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// Requirement 2, in a real container: the binary on the mount is the one for
// the architecture the RUNTIME runs, and never the other one.
//
// This is asserted by digest from inside the container rather than by watching
// a foreign binary fail, because on this class of host it does NOT fail: Docker
// Desktop registers binfmt emulation inside its VM, so a linux/amd64 ELF
// executes on a linux/arm64 runtime and the proxy starts and serves normally.
// (Measured: it ran for five minutes until the test was killed.) That is the
// finding, and it is why selection is AO's control rather than the kernel's —
// there is no exec-format error to rely on.
func TestLivePackaging_OnlyTheRuntimesOwnArchitectureIsStaged(t *testing.T) {
	store := requirePackaged(t)
	w := newWorld(t)
	native := w.runtimeGOARCH()
	other := "amd64"
	if native == "amd64" {
		other = "arm64"
	}

	// Staging takes what the RUNTIME reported and maps it itself, so there is
	// no argument by which a caller could ask for the foreign artifact.
	staged := w.stageProxy(store, w.policy(time.Hour, "http://example.test:8080"))
	if staged.Artifact.GOARCH != native {
		t.Fatalf("staged linux/%s for a runtime that runs linux/%s", staged.Artifact.GOARCH, native)
	}

	var nativeDigest, foreignDigest string
	for _, a := range store.Provenance().Artifacts {
		switch a.GOARCH {
		case native:
			nativeDigest = a.SHA256
		case other:
			foreignDigest = a.SHA256
		}
	}
	if nativeDigest == "" || foreignDigest == "" || nativeDigest == foreignDigest {
		t.Fatalf("provenance does not describe two distinct architectures: %+v", store.Provenance().Artifacts)
	}

	// Hashed from INSIDE the container, over the mount as the proxy would see
	// it. What AO recorded and what the container will execute are then the
	// same claim rather than two.
	raw, err := exec.Command("docker", "run", "--rm", "--label", egressLabel+"=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,size=8m",
		"-v", staged.Dir+":"+proxybin.ContainerDir+":ro",
		"alpine:3.19", "sha256sum", proxybin.ContainerDir+"/ao-egress-proxy").CombinedOutput()
	if err != nil {
		t.Fatalf("hash the mounted binary: %v\n%s", err, raw)
	}
	got := strings.Fields(string(raw))
	if len(got) == 0 {
		t.Fatalf("sha256sum printed nothing: %q", raw)
	}
	if got[0] != nativeDigest {
		t.Fatalf("the container sees %s; the linux/%s artifact is %s and the linux/%s one is %s",
			got[0], native, nativeDigest, other, foreignDigest)
	}
}

// A corrupted artifact never reaches a container: staging hashes what it wrote
// and refuses. And if one somehow did, it does not become a proxy — the
// container fails instead of serving something AO cannot identify.
func TestLivePackaging_ACorruptedProxyNeverServes(t *testing.T) {
	store := requirePackaged(t)
	w := newWorld(t)
	policy, err := w.policy(time.Hour, "http://example.test:8080").Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Half one: staging refuses bytes that do not match the provenance, so the
	// corruption is caught before a mount exists.
	base, _ := filepath.Abs("testdata")
	root, err := proxybin.StagingRootFor("", base)
	if err != nil {
		t.Fatalf("StagingRootFor: %v", err)
	}
	_, body, err := store.Select(w.runtimeGOARCH())
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	corrupt := proxybin.CorruptStoreForTest(w.runtimeGOARCH(), body)
	if _, err := proxybin.Stage(proxybin.StageRequest{
		Store: corrupt, Root: root, RunID: "corrupt-" + w.runToken(),
		RuntimeArch: containerArch(t), Policy: policy,
	}); err == nil {
		t.Fatal("Stage accepted bytes that do not hash to their provenance")
	}
	if _, err := os.Stat(filepath.Join(root, "corrupt-"+w.id)); !os.IsNotExist(err) {
		t.Fatal("the refused stage left a mount behind")
	}

	// Half two: what the refusal is protecting against. A truncated binary in a
	// mount is not a proxy — it is a container that dies, and the run must fail
	// rather than proceed with an unenforced allowlist.
	dir := filepath.Join(w.dir, "corrupt")
	if err := os.Mkdir(dir, 0o711); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	truncated := append([]byte(nil), body[:len(body)/2]...)
	if err := os.WriteFile(filepath.Join(dir, "ao-egress-proxy"), truncated, 0o555); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), policy, 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, runErr := exec.Command("docker", "run", "--rm", "--label", egressLabel+"=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-v", dir+":"+proxybin.ContainerDir+":ro",
		"--entrypoint", proxybin.ContainerDir+"/ao-egress-proxy",
		"alpine:3.19", "-policy", proxybin.ContainerDir+"/policy.json").CombinedOutput()
	if runErr == nil && strings.Contains(string(out), "enforcing") {
		t.Fatalf("a truncated binary served as a proxy:\n%s", out)
	}
}
