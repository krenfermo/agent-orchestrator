package practical3d

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type sandboxRig struct {
	p       SandboxParams
	profile string
	secret  map[string]string // name -> private path the agent must not reach
	ctlSock string
}

func newSandboxRig(t *testing.T) *sandboxRig {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	root := t.TempDir()
	realHome := filepath.Join(root, "home")
	aoHome := filepath.Join(realHome, ".ao")
	run := filepath.Join(aoHome, "scratch", "frente3", "3d-practical", "run")
	pos := filepath.Join(run, "positions", "01-aaaa")
	other := filepath.Join(run, "positions", "02-bbbb")
	for _, d := range []string{filepath.Join(pos, "work"), filepath.Join(pos, "ao-data", "worktrees"), filepath.Join(pos, "runtime-home"), filepath.Join(pos, "tmp"), filepath.Join(other, "work"), filepath.Join(aoHome, "data"), filepath.Join(aoHome, "scratch", "frente3", "3d", "hidden", "A"), filepath.Join(root, "aosrc", "docs"), filepath.Join(root, "tools"), filepath.Join(realHome, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := map[string]string{
		"ledger":         filepath.Join(run, "ledger.jsonl"),
		"registry":       filepath.Join(aoHome, "scratch", "frente3", "registry.jsonl"),
		"envelope":       filepath.Join(run, "envelope.json"),
		"hidden Q4":      filepath.Join(aoHome, "scratch", "frente3", "3d", "hidden", "A", "lockout_oracle_test.go"),
		"other position": filepath.Join(other, "work", "result.txt"),
		"own ao.db":      filepath.Join(pos, "ao-data", "ao.db"),
		"production db":  filepath.Join(aoHome, "data", "ao.db"),
		"AO source":      filepath.Join(root, "aosrc", "docs", "3d-practical.md"),
		"real claude":    filepath.Join(realHome, ".claude", "history.jsonl"),
	}
	for _, p := range secret {
		if err := os.WriteFile(p, []byte("SECRET\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pos, "ao-data", "running.json"), []byte(`{"port":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctlDir, err := os.MkdirTemp("/tmp", "p3dctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ctlDir) })
	profile, err := WriteSandboxProfile(ctlDir)
	if err != nil {
		t.Fatal(err)
	}
	r := &sandboxRig{profile: profile, secret: secret, ctlSock: filepath.Join(ctlDir, "ctl.sock")}
	r.p = SandboxParams{AOHome: aoHome, RealHome: realHome, AOSrc: filepath.Join(root, "aosrc"), PrivateCtl: ctlDir, ToolsRO: filepath.Join(root, "tools"), OracleDir: filepath.Join(root, "oracle"),
		PosWork: filepath.Join(pos, "work"), PosWorktrees: filepath.Join(pos, "ao-data", "worktrees"), PosHome: filepath.Join(pos, "runtime-home"), PosTmp: filepath.Join(pos, "tmp"), PosRunFile: filepath.Join(pos, "ao-data", "running.json"), PosPrompts: filepath.Join(pos, "ao-data", "prompts"), PosHookBin: filepath.Join(pos, "ao-data", "hook-bin"),
		ProxyPort: 1, DaemonPort: 2}
	return r
}

func (r *sandboxRig) sh(t *testing.T, script string) (string, error) {
	t.Helper()
	argv, err := SandboxCommand(r.profile, r.p, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = r.p.PosWork
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + r.p.PosHome, "TMPDIR=" + r.p.PosTmp}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSandboxDeniesExperimentPrivatePaths(t *testing.T) {
	r := newSandboxRig(t)
	for name, path := range r.secret {
		if out, err := r.sh(t, "cat "+shellQuote(path)); err == nil || strings.Contains(out, "SECRET") {
			t.Errorf("read %s allowed: %q", name, out)
		}
		if _, err := r.sh(t, "echo x >> "+shellQuote(path)); err == nil {
			t.Errorf("write %s allowed", name)
		}
		if b, _ := os.ReadFile(path); string(b) != "SECRET\n" {
			t.Errorf("%s modified", name)
		}
	}
	// Traversal from the working copy and listing of the run directory.
	if out, err := r.sh(t, "cat ../../../ledger.jsonl"); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("traversal read allowed: %q", out)
	}
	if out, err := r.sh(t, "ls ../.."); err == nil && strings.Contains(out, "02-bbbb") {
		t.Errorf("listing other positions allowed: %q", out)
	}
	// Symlink escape: the agent may create a link, never read through it.
	if out, err := r.sh(t, "ln -s "+shellQuote(r.secret["ledger"])+" link && cat link"); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("symlink escape allowed: %q", out)
	}
	// Codex's ChatGPT login is the one readable file of ~/.codex.
	if err := os.MkdirAll(filepath.Join(r.p.RealHome, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, allowed := range map[string]bool{"auth.json": true, "history.jsonl": false} {
		p := filepath.Join(r.p.RealHome, ".codex", name)
		if err := os.WriteFile(p, []byte("CODEX\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := r.sh(t, "cat "+shellQuote(p))
		if got := err == nil && strings.Contains(out, "CODEX"); got != allowed {
			t.Errorf("~/.codex/%s readable=%v, want %v", name, got, allowed)
		}
	}
	// The Q4 oracle is out of reach wherever the operator keeps it (here:
	// outside ~/.ao, which the broad AO_HOME deny does not cover).
	hiddenTest := filepath.Join(r.p.OracleDir, "hidden", "A", "lock_test.go")
	if err := os.MkdirAll(filepath.Dir(hiddenTest), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hiddenTest, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := r.sh(t, "find "+shellQuote(r.p.OracleDir)+" -type f -exec cat {} \\;"); strings.Contains(out, "SECRET") {
		t.Errorf("hidden Q4 tests readable: %v %q", err, out)
	}
	// HOME escape: the operator's provider state.
	if out, err := r.sh(t, "cat "+shellQuote(filepath.Join(r.p.RealHome, ".claude", "history.jsonl"))); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("real HOME provider state readable: %q", out)
	}
}

func TestSandboxAllowsNormalWorkInThePosition(t *testing.T) {
	r := newSandboxRig(t)
	script := "echo hi > f.txt && cat f.txt && mkdir -p " + shellQuote(filepath.Join(r.p.PosWorktrees, "w")) + " && echo w > " + shellQuote(filepath.Join(r.p.PosWorktrees, "w", "x")) +
		" && echo h > \"$HOME/h\" && echo t > \"$TMPDIR/t\" && cat " + shellQuote(r.p.PosRunFile) + " && git init -q . && git status --short"
	out, err := r.sh(t, script)
	if err != nil || !strings.Contains(out, "hi") || !strings.Contains(out, `"port"`) {
		t.Fatalf("normal work denied: %v %q", err, out)
	}
}

func TestSandboxNetworkOnlyReachesProxyAndDaemon(t *testing.T) {
	r := newSandboxRig(t)
	allowed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = allowed.Close() }()
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	go func() {
		_ = http.Serve(allowed, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("PROXY-OK")) }))
	}()
	go func() {
		_ = http.Serve(other, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("LEAK")) }))
	}()
	r.p.ProxyPort = allowed.Addr().(*net.TCPAddr).Port
	ctl, err := net.Listen("unix", r.ctlSock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ctl.Close() }()
	go func() {
		_ = http.Serve(ctl, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("CTL-LEAK")) }))
	}()
	if out, err := r.sh(t, fmt.Sprintf("curl -s --max-time 5 http://127.0.0.1:%d/", r.p.ProxyPort)); err != nil || !strings.Contains(out, "PROXY-OK") {
		t.Fatalf("proxy unreachable: %v %q", err, out)
	}
	for name, script := range map[string]string{
		"other loopback port": fmt.Sprintf("curl -s --max-time 5 http://127.0.0.1:%d/", other.Addr().(*net.TCPAddr).Port),
		"internet":            "curl -s --max-time 5 https://api.anthropic.com/v1/messages",
		"control socket":      "curl -s --max-time 5 --unix-socket " + shellQuote(r.ctlSock) + " http://ctl/token",
	} {
		if out, err := r.sh(t, script); err == nil || strings.Contains(out, "LEAK") || strings.Contains(out, "OK") {
			t.Errorf("%s reachable: %q", name, out)
		}
	}
	// A nested, fully permissive profile cannot lift the outer confinement.
	if out, err := r.sh(t, "/usr/bin/sandbox-exec -p '(version 1)(allow default)' /bin/cat "+shellQuote(r.secret["ledger"])); err == nil || strings.Contains(out, "SECRET") {
		t.Errorf("nested sandbox escaped: %q", out)
	}
	for _, bin := range []string{"/usr/bin/osascript", "/bin/launchctl"} {
		out, err := r.sh(t, bin+" help; echo rc=$?")
		if err != nil || !strings.Contains(out, "rc=126") {
			t.Errorf("exec %s not denied: %v %q", bin, err, out)
		}
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Codex review R4 (P1): the reviewer (Codex) cannot read the worker's Claude
// transcripts in the shared position HOME, nor the worker the reviewer's.
func TestSandboxSeparatesHarnessState(t *testing.T) {
	r := newSandboxRig(t)
	claudeDir := filepath.Join(r.p.PosHome, ".claude", "projects")
	codexDir := filepath.Join(r.p.PosHome, "codex-home", "sessions")
	for _, d := range []string{claudeDir, codexDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "t.jsonl"), []byte("MEMORY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for harness, denied := range map[string]string{"claude": codexDir, "codex": claudeDir} {
		r.p.HarnessDeny = filepath.Join(r.p.PosHome, "codex-home")
		own := claudeDir
		if harness == "codex" {
			r.p.HarnessDeny, own = filepath.Join(r.p.PosHome, ".claude"), codexDir
		}
		if out, err := r.sh(t, "cat "+shellQuote(filepath.Join(denied, "t.jsonl"))); err == nil || strings.Contains(out, "MEMORY") {
			t.Errorf("%s read the other harness's transcript: %q", harness, out)
		}
		if out, err := r.sh(t, "cat "+shellQuote(filepath.Join(own, "t.jsonl"))); err != nil || !strings.Contains(out, "MEMORY") {
			t.Errorf("%s cannot read its own state: %v %q", harness, err, out)
		}
	}
}

// Codex review R6 (P1 claim): an agent cannot mint a provider token for
// another AO subject. The launch shim (which reads AO_USAGE_SUBJECT) lives in
// the private control directory and cannot be executed from inside the
// sandbox; token issuance needs the control socket, which is unreachable
// (TestSandboxNetworkOnlyReachesProxyAndDaemon, "control socket").
func TestSandboxAgentCannotRunTheLaunchShim(t *testing.T) {
	r := newSandboxRig(t)
	bin := filepath.Join(r.p.PrivateCtl, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(bin, "claude")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\necho SHIM_RAN\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := r.sh(t, "AO_USAGE_SUBJECT=session:worker-1 "+shellQuote(shim)+" -p hi")
	if err == nil || strings.Contains(out, "SHIM_RAN") {
		t.Fatalf("agent ran the launch shim: %v %q", err, out)
	}
}
