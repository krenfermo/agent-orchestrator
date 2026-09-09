package skillegress

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These drive REAL containers on REAL Docker networks. Everything they reach is
// a synthetic container started by the test; no external host is contacted, and
// the one probe aimed at a public address exists specifically to observe that
// it FAILS.
//
// The topology under test:
//
//	ao-egress-int-<id>  --internal   skill + proxy
//	ao-egress-ext-<id>  bridge       proxy + synthetic upstreams
//
// The skill joins only the internal network. Measured there: an internet IP is
// "Network unreachable", the cloud-metadata address is "Network unreachable",
// and the embedded resolver answers SERVFAIL for external names. So the proxy
// is not a politeness the workload could decline.

const egressLabel = "ao.egresstest"

func dockerAvailable(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("live egress tests are skipped under -short")
	}
	out, err := exec.Command("docker", "info", "--format", "{{.OSType}}/{{.CgroupVersion}}").Output()
	if err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(out)), "linux/2") {
		t.Skipf("runtime is %q, not linux/cgroup v2", strings.TrimSpace(string(out)))
	}
	if err := exec.Command("docker", "image", "inspect", "alpine:3.19").Run(); err != nil {
		t.Skip("alpine:3.19 is not present locally, and these tests do not pull")
	}
}

// world is one isolated egress topology, torn down with the test.
type world struct {
	t        *testing.T
	id       string
	intNet   string
	extNet   string
	dir      string
	proxyBin string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	dockerAvailable(t)
	// The id becomes both a container name and a DNS label, and the validator
	// correctly refuses an underscore in a hostname -- which Go test names are
	// full of. Reduce it to what a DNS label may hold.
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, t.Name())
	if len(id) > 32 {
		id = id[len(id)-32:]
	}
	w := &world{
		t: t, id: id,
		intNet: "ao-eg-int-" + id,
		extNet: "ao-eg-ext-" + id,
	}
	// Stage under the repository: the container runtime here is a VM that
	// shares only certain host paths, and this one is known to be shared.
	base, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolve testdata: %v", err)
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("create testdata: %v", err)
	}
	dir, err := os.MkdirTemp(base, "egress-")
	if err != nil {
		t.Fatalf("stage dir: %v", err)
	}
	w.dir = dir

	w.run("network", "create", "--internal", "--label", egressLabel+"=1", w.intNet)
	w.run("network", "create", "--label", egressLabel+"=1", w.extNet)
	t.Cleanup(w.teardown)

	w.proxyBin = filepath.Join(dir, "ao-egress-proxy")
	build := exec.Command("go", "build", "-o", w.proxyBin, "./cmd/ao-egress-proxy")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+containerArch(t))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile the proxy: %v\n%s", err, out)
	}
	return w
}

func containerArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "info", "--format", "{{.Architecture}}").Output()
	if err != nil {
		return "arm64"
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64", "amd64":
		return "amd64"
	default:
		return "arm64"
	}
}

func (w *world) run(args ...string) string {
	w.t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		w.t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (w *world) teardown() {
	// Only AO's own labelled resources, never anybody else's.
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+egressLabel+"=1").Output()
	for _, id := range strings.Fields(string(out)) {
		_ = exec.Command("docker", "rm", "-f", id).Run()
	}
	for _, n := range []string{w.intNet, w.extNet} {
		_ = exec.Command("docker", "network", "rm", n).Run()
	}
	_ = os.RemoveAll(w.dir)
}

// upstreamContainer starts a synthetic HTTP responder on the external network
// and returns the address it can be reached at from the proxy.
func (w *world) upstreamContainer(name, body string) string {
	w.t.Helper()
	w.run("run", "-d", "--name", name, "--label", egressLabel+"=1",
		"--network", w.extNet, "alpine:3.19", "sh", "-c",
		"while true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: "+
			itoa(len(body))+"\\r\\nConnection: close\\r\\n\\r\\n"+body+"' | nc -l -p 8080; done")
	// The proxy resolves names itself, so the test gives it a hosts entry at
	// CREATION time -- the proxy's root filesystem is immutable once running,
	// which is the posture a real deployment wants and this test keeps.
	return w.run("inspect", "-f",
		"{{(index .NetworkSettings.Networks \""+w.extNet+"\").IPAddress}}", name)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// startProxy writes the policy and starts the sidecar, dual-homed.
func (w *world) startProxy(policy Policy, hosts ...string) string {
	w.t.Helper()
	body, err := policy.Encode()
	if err != nil {
		w.t.Fatalf("Encode: %v", err)
	}
	if err := os.WriteFile(filepath.Join(w.dir, "policy.json"), body, 0o600); err != nil {
		w.t.Fatalf("write policy: %v", err)
	}
	name := "ao-eg-proxy-" + w.id
	args := []string{"run", "-d", "--name", name, "--label", egressLabel + "=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only"}
	for _, h := range hosts {
		args = append(args, "--add-host", h)
	}
	args = append(args,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		// The proxy keeps the same posture as a skill container -- non-root,
		// immutable root filesystem, no capabilities -- and gets one bounded
		// tmpfs for its decision log, which is the only thing it writes.
		"--tmpfs", "/tmp:rw,size=8m,mode=0700,uid=65534,gid=65534,noexec,nosuid,nodev",
		"-v", w.dir+":/policy:ro", "--entrypoint", "/policy/ao-egress-proxy",
		"alpine:3.19",
		"-policy", "/policy/policy.json", "-addr", ":3128", "-decisions", "/tmp/decisions.jsonl")
	w.run(args...)
	// The second leg: this is what makes the proxy the ONLY route out.
	w.run("network", "connect", w.extNet, name)
	// Give it a moment to bind.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(out), "enforcing") {
			return name
		}
		if strings.Contains(string(out), "ao-egress-proxy:") && strings.Contains(string(out), "invalid") {
			w.t.Fatalf("the proxy refused to start: %s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	w.t.Fatalf("the proxy did not start: %s", logs)
	return ""
}

// skill runs a workload on the internal network only, with the proxy env set.
func (w *world) skill(script string, proxyName string) string {
	w.t.Helper()
	args := []string{
		"run", "--rm", "--label", egressLabel + "=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,size=8m",
	}
	if proxyName != "" {
		args = append(args,
			"-e", "HTTP_PROXY=http://"+proxyName+":3128",
			"-e", "HTTPS_PROXY=http://"+proxyName+":3128",
			"-e", "http_proxy=http://"+proxyName+":3128")
	}
	args = append(args, "alpine:3.19", "sh", "-c", script)
	out, _ := exec.Command("docker", args...).CombinedOutput()
	return string(out)
}

func (w *world) proxyDecisions(proxyName string) []Decision {
	w.t.Helper()
	out, err := exec.Command("docker", "exec", proxyName, "cat", "/tmp/decisions.jsonl").Output()
	if err != nil {
		return nil
	}
	var decisions []Decision
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d Decision
		if err := json.Unmarshal([]byte(line), &d); err == nil {
			decisions = append(decisions, d)
		}
	}
	return decisions
}

func (w *world) policy(ttl time.Duration, raw ...string) Policy {
	w.t.Helper()
	dests := make([]Destination, 0, len(raw))
	for _, r := range raw {
		d, err := ParseDestination(r)
		if err != nil {
			w.t.Fatalf("ParseDestination(%q): %v", r, err)
		}
		dests = append(dests, d)
	}
	p, err := PolicyFor(Lease{
		ID: "lease-" + w.id, Scope: testScope(), RunID: "run-1", AttemptID: "attempt-1",
		Destinations: dests, IssuedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(ttl),
	})
	if err != nil {
		w.t.Fatalf("PolicyFor: %v", err)
	}
	// The synthetic upstreams live on a Docker bridge, whose addresses are
	// RFC1918 and therefore blocked by default -- correctly. The exception is
	// written down here, exactly as an installation with an internal registry
	// would write one, and it can never re-open link-local: that is asserted
	// in TestPolicy_AnExceptionCannotReopenMetadata.
	p.PermittedPrivateCIDRs = []string{"172.16.0.0/12", "10.0.0.0/8", "192.168.0.0/16"}
	return p
}

// The load-bearing test: with NO proxy configured at all, the skill container
// has no route anywhere. This is what makes the proxy a boundary rather than a
// convention — there is nothing to bypass.
func TestLiveEgress_NoDirectRouteExistsToBypass(t *testing.T) {
	w := newWorld(t)
	w.upstreamContainer("ao-eg-up-"+w.id, "SHOULD-NOT-BE-REACHED")

	out := w.skill(`
probe() { if wget -T 3 -q -O- "$2" >/tmp/body 2>/tmp/err; then echo "$1 REACHED: $(head -c 40 /tmp/body)"; else echo "$1 UNREACHABLE"; fi; }
probe IP        http://1.1.1.1/
probe METADATA  http://169.254.169.254/
probe UPSTREAM  http://ao-eg-up-`+w.id+`:8080/
echo "--- external DNS ---"; nslookup example.com 2>&1 | tail -2
`, "")
	for _, want := range []string{"IP UNREACHABLE", "METADATA UNREACHABLE", "UPSTREAM UNREACHABLE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SHOULD-NOT-BE-REACHED") {
		t.Fatalf("the skill reached an upstream directly:\n%s", out)
	}
	if !strings.Contains(out, "SERVFAIL") && !strings.Contains(out, "can't find") {
		t.Fatalf("external DNS resolved from inside the isolated network:\n%s", out)
	}
}

// Through the proxy, the granted destination works and the ungranted one does
// not — and unsetting the proxy variables restores nothing, because the
// variables were never the control.
func TestLiveEgress_AllowsOnlyTheGrantedDestination(t *testing.T) {
	w := newWorld(t)
	allowed := "ao-eg-ok-" + w.id
	denied := "ao-eg-no-" + w.id
	allowedIP := w.upstreamContainer(allowed, "ALLOWED-BODY")
	deniedIP := w.upstreamContainer(denied, "DENIED-BODY")
	// Both names resolve for the proxy. Only one is granted — so what refuses
	// the second is the allowlist, not a failure to resolve it.
	proxy := w.startProxy(w.policy(time.Hour, "http://"+allowed+".example.test:8080"),
		allowed+".example.test:"+allowedIP, denied+".example.test:"+deniedIP)

	out := w.skill(`
if wget -T 5 -q -O- http://`+allowed+`.example.test:8080/ >/tmp/a 2>/tmp/ae; then cat /tmp/a; echo; else echo "GRANTED REFUSED"; fi
if wget -T 5 -q -O- http://`+denied+`.example.test:8080/ >/tmp/d 2>/tmp/de; then cat /tmp/d; echo; else echo "DENIED REFUSED"; fi
if env -u HTTP_PROXY -u http_proxy -u HTTPS_PROXY wget -T 3 -q -O- http://`+allowed+`.example.test:8080/ >/tmp/n 2>/tmp/ne; then cat /tmp/n; echo; else echo "NO ROUTE WITHOUT PROXY"; fi
`, proxy)

	if !strings.Contains(out, "ALLOWED-BODY") {
		t.Fatalf("the granted destination was not reachable:\n%s", out)
	}
	if strings.Contains(out, "DENIED-BODY") {
		t.Fatalf("an ungranted destination was reached:\n%s", out)
	}
	// The variables are a convenience; the topology is the control.
	if !strings.Contains(out, "NO ROUTE WITHOUT PROXY") {
		t.Fatalf("unsetting the proxy variables left a usable route:\n%s", out)
	}

	decisions := w.proxyDecisions(proxy)
	if len(decisions) == 0 {
		t.Fatal("the proxy recorded no decisions")
	}
	var allowedSeen, refusedSeen bool
	for _, d := range decisions {
		if d.Allowed && strings.HasPrefix(d.Host, allowed) {
			allowedSeen = true
		}
		if !d.Allowed && strings.HasPrefix(d.Host, denied) {
			refusedSeen = true
		}
	}
	if !allowedSeen || !refusedSeen {
		t.Fatalf("the evidence does not show both outcomes: %+v", decisions)
	}
}

// A proxy that is not running leaves the skill with no network at all, rather
// than with a fallback.
func TestLiveEgress_ProxyDownMeansNoNetwork(t *testing.T) {
	w := newWorld(t)
	allowed := "ao-eg-ok-" + w.id
	allowedIP := w.upstreamContainer(allowed, "ALLOWED-BODY")
	proxy := w.startProxy(w.policy(time.Hour, "http://"+allowed+".example.test:8080"),
		allowed+".example.test:"+allowedIP)
	w.run("stop", "-t", "1", proxy)

	out := w.skill(`if wget -T 4 -q -O- http://`+allowed+`.example.test:8080/ >/tmp/b 2>/dev/null; then cat /tmp/b; else echo "NO EGRESS"; fi`, proxy)
	if strings.Contains(out, "ALLOWED-BODY") {
		t.Fatalf("traffic flowed with the proxy down:\n%s", out)
	}
	if !strings.Contains(out, "NO EGRESS") {
		t.Fatalf("expected a refusal:\n%s", out)
	}
}

// A policy the proxy cannot accept stops it from starting. There is no
// permissive fallback to fall into.
func TestLiveEgress_TamperedPolicyRefusesToStart(t *testing.T) {
	w := newWorld(t)
	if err := os.WriteFile(filepath.Join(w.dir, "policy.json"),
		[]byte(`{"leaseId":"x","destinations":[{"scheme":"http","host":"*.example.test","port":80}],`+
			`"expiresAt":"2030-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	name := "ao-eg-badproxy-" + w.id
	w.run("run", "-d", "--name", name, "--label", egressLabel+"=1",
		"--network", w.intNet, "--user", "65534:65534", "--read-only",
		"-v", w.dir+":/policy:ro", "--entrypoint", "/policy/ao-egress-proxy",
		"alpine:3.19", "-policy", "/policy/policy.json")

	deadline := time.Now().Add(15 * time.Second)
	var logs string
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		logs = string(out)
		// The refusal may name the wildcard directly or the character that
		// makes the host malformed; both are the validator refusing to start.
		if strings.Contains(logs, "wildcard") || strings.Contains(logs, "not a DNS character") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the proxy did not refuse a wildcard policy: %s", logs)
}

// A grant for another project cannot be used: the policy carries the scope it
// was minted for, and a lease for another scope is a different file the proxy
// never sees.
func TestLiveEgress_AnotherProjectsGrantIsADifferentPolicy(t *testing.T) {
	w := newWorld(t)
	other := testScope()
	other.ProjectID = "poseidon"

	mine := w.policy(time.Hour, "http://a.example.test:8080")
	if mine.Scope.Matches(other) {
		t.Fatal("a policy minted for medusa matched poseidon")
	}
	// And the lease that produced it is bound to the run and attempt.
	if mine.RunID != "run-1" || mine.AttemptID != "attempt-1" {
		t.Fatalf("policy = %+v", mine)
	}
}
