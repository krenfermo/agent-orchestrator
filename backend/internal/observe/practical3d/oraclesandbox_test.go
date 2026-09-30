package practical3d

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Codex review R5 (P0): the oracle runs agent-authored code. Under the
// oracle sandbox that code reaches neither the network, nor ~/.ao, nor the
// operator's oracle directory, nor (during Q1) the staged hidden tests, and
// cannot rewrite the staged oracle Q4 executes.
func TestOracleSandboxConfinesAgentCode(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt")
	}
	realHome, _ := os.UserHomeDir()
	aoSecret := filepath.Join(realHome, ".ao", "scratch", "frente3", "oracle-sandbox-probe-"+randHex(4))
	if err := os.MkdirAll(filepath.Dir(aoSecret), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aoSecret, []byte("LEDGER"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(aoSecret) })
	opDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(opDir, "hidden", "A"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := []byte("#!/bin/bash\necho tamper >> \"$(dirname \"$0\")/oracle.sh\" 2>/dev/null && echo WROTE_ORACLE\necho ORACLE $1 PASS\n")
	_ = os.WriteFile(filepath.Join(opDir, "oracle.sh"), script, 0o600)
	_ = os.WriteFile(filepath.Join(opDir, "hidden", "A", "h_test.go"), []byte("HIDDEN"), 0o600)
	hidden, err := BuildHiddenManifest(filepath.Join(opDir, "hidden"), "A")
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Q4Oracle: Q4Oracle{CommandSHA256: sha256Hex(script), RunnerImageOrBinarySHA256: sha256Hex(script)}}
	o := RealOracle{Script: filepath.Join(opDir, "oracle.sh"), HiddenDir: filepath.Join(opDir, "hidden"), Manifest: m,
		Exec: &AORealExecutor{Cfg: AORealConfig{AOSrc: t.TempDir(), OracleDir: opDir}}}
	stage, err := stageOracle(o, "A", sha256Hex(hidden))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.cleanup()
	probe := strings.Join([]string{
		"cat " + shellQuote(aoSecret) + " && echo READ_AO",
		"cat " + shellQuote(filepath.Join(opDir, "oracle.sh")) + " >/dev/null && echo READ_OPDIR",
		"cat " + shellQuote(filepath.Join(stage.hidden, "A", "h_test.go")) + " && echo READ_STAGED_HIDDEN",
		"/usr/bin/curl -s -m 5 https://api.anthropic.com >/dev/null && echo NET",
		"env | grep -q ANTHROPIC && echo ENV_LEAK",
		"echo DONE",
	}, "; ")
	q1, err := stage.command(context.Background(), o, false, stage.root, "/bin/sh", "-c", probe)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := q1.CombinedOutput()
	if !strings.Contains(string(out), "DONE") {
		t.Fatalf("probe did not run: %s", out)
	}
	for _, leak := range []string{"LEDGER", "READ_AO", "READ_OPDIR", "HIDDEN", "READ_STAGED_HIDDEN", "NET", "ENV_LEAK"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("Q1 agent code: %s\n%s", leak, out)
		}
	}
	q4, err := stage.command(context.Background(), o, true, stage.root, "/bin/bash", stage.script, "A", stage.root)
	if err != nil {
		t.Fatal(err)
	}
	out, _ = q4.CombinedOutput()
	if strings.Contains(string(out), "WROTE_ORACLE") {
		t.Errorf("Q4 could rewrite the staged oracle: %s", out)
	}
	if err := stage.verify(m, "A", sha256Hex(hidden)); err != nil {
		t.Errorf("staged oracle changed: %v", err)
	}
}

// The real fixture's build/test and the real oracle script still work under
// the oracle sandbox (skipped where the frozen fixture is absent).
func TestOracleSandboxRunsTheRealFixture(t *testing.T) {
	realHome, _ := os.UserHomeDir()
	base := filepath.Join(realHome, ".ao", "scratch", "frente3", "3d")
	if _, err := os.Stat(filepath.Join(base, "oracle.sh")); err != nil || runtime.GOOS != "darwin" {
		t.Skip("frozen fixture not present")
	}
	script, _ := os.ReadFile(filepath.Join(base, "oracle.sh"))
	hidden, err := BuildHiddenManifest(filepath.Join(base, "hidden"), "D")
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{Q4Oracle: Q4Oracle{CommandSHA256: sha256Hex(script), RunnerImageOrBinarySHA256: sha256Hex(script)}}
	o := RealOracle{Script: filepath.Join(base, "oracle.sh"), HiddenDir: filepath.Join(base, "hidden"), Manifest: m,
		Exec: &AORealExecutor{Cfg: AORealConfig{AOSrc: t.TempDir(), OracleDir: base}}}
	stage, err := stageOracle(o, "D", sha256Hex(hidden))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.cleanup()
	clone := filepath.Join(stage.root, "clone")
	if out, err := exec.Command("git", "clone", "--quiet", filepath.Join(base, "fixture-src"), clone).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_ = exec.Command("git", "-C", clone, "remote", "remove", "origin").Run()
	q1, err := stage.command(context.Background(), o, false, clone, "go", "test", "./...")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := q1.CombinedOutput(); err != nil {
		t.Fatalf("Q1 under the oracle sandbox: %v\n%s", err, out)
	}
	q4, err := stage.command(context.Background(), o, true, stage.root, "/bin/bash", stage.script, "D", clone)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := q4.CombinedOutput()
	// The pristine fixture has no task-D change: the oracle must run to its
	// own verdict (not a sandbox error).
	if !strings.Contains(string(out), "ORACLE D FAIL task D: expected exactly a one-line change") {
		t.Fatalf("Q4 under the oracle sandbox:\n%s", out)
	}
}

// Codex review R6 (P0 claim): hidden tests must not exist anywhere in the
// fixture's Git history the agent can read.
func TestCheckFixtureHistoryDetectsCommittedHiddenTests(t *testing.T) {
	t.Parallel()
	repo, hidden := t.TempDir(), t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("init", "-q")
	_ = os.MkdirAll(filepath.Join(hidden, "A"), 0o700)
	_ = os.WriteFile(filepath.Join(hidden, "A", "h_test.go"), []byte("package x // hidden\n"), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "main.go"), []byte("package x\n"), 0o600)
	run("add", ".")
	run("commit", "-q", "-m", "clean")
	if err := CheckFixtureHistory(context.Background(), repo, hidden); err != nil {
		t.Fatalf("clean history rejected: %v", err)
	}
	// Committed, then deleted: still in the object database.
	_ = os.WriteFile(filepath.Join(repo, "h_test.go"), []byte("package x // hidden\n"), 0o600)
	run("add", ".")
	run("commit", "-q", "-m", "leak")
	run("rm", "-q", "h_test.go")
	run("commit", "-q", "-m", "delete")
	if err := CheckFixtureHistory(context.Background(), repo, hidden); err == nil {
		t.Fatal("hidden test in history not detected")
	}
}
