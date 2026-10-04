package hookutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeFakeAO writes a stand-in for the AO binary that records the arguments it
// was invoked with, and returns its path plus the record file.
func writeFakeAO(t *testing.T, dir string) (exe, record string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(dir, "ao-daemon")
	record = filepath.Join(dir, "argv.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + ShellQuote(record) + "\nexit 0\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
	return exe, record
}

// runHookCommand runs command exactly the way an agent CLI runs a hook: a bare
// non-interactive /bin/sh with an environment that contains NO PATH entry for
// AO — and, here, no PATH at all. This is what made the bare `ao hooks ...`
// form die with "/bin/sh: ao: command not found".
func runHookCommand(t *testing.T, command string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = []string{"PATH="}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEnsureLauncherResolvesWithoutAOOnShellPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	exe, record := writeFakeAO(t, filepath.Join(root, "install"))
	dataDir := filepath.Join(root, "data")

	launcher, err := EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatalf("EnsureLauncherFor: %v", err)
	}

	command := ShellQuote(launcher) + " hooks claude-code stop"
	if out, err := runHookCommand(t, command); err != nil {
		t.Fatalf("hook command %q failed with an empty PATH: %v\n%s", command, err, out)
	}
	got, err := os.ReadFile(record) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("hook did not reach the AO binary: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hooks claude-code stop" {
		t.Fatalf("AO binary received %q, want %q", strings.TrimSpace(string(got)), "hooks claude-code stop")
	}
}

// TestBareAOHookCommandFailsWithoutPATH pins the failure the launcher exists to
// remove, so a future change back to a bare `ao` cannot pass silently.
func TestBareAOHookCommandFailsWithoutPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	out, err := runHookCommand(t, "ao hooks claude-code stop")
	if err == nil {
		t.Fatalf("bare `ao` hook unexpectedly resolved with an empty PATH:\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("err = %v (output %q), want a shell exit 127 (command not found)", err, out)
	}
}

func TestEnsureLauncherIsDeterministic(t *testing.T) {
	root := t.TempDir()
	exe, _ := writeFakeAO(t, filepath.Join(root, "install"))
	dataDir := filepath.Join(root, "data")
	want := filepath.Join(dataDir, LauncherDirName, LauncherName())

	// The resolution must not consult the caller's PATH or shell in any way.
	t.Setenv("PATH", "")
	t.Setenv("SHELL", "/nonexistent/shell")

	for i := 0; i < 3; i++ {
		got, err := EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
		if err != nil {
			t.Fatalf("EnsureLauncherFor #%d: %v", i, err)
		}
		if got != want {
			t.Fatalf("launcher #%d = %q, want %q", i, got, want)
		}
	}
	if !IsExecutableFile(want) {
		t.Fatalf("launcher %q is not an executable file", want)
	}
}

// TestEnsureLauncherRefreshesTargetBinary covers the reason the command embeds
// the shim rather than the daemon path: the recorded command stays identical
// across daemon rebuilds while still reaching the current binary.
func TestEnsureLauncherRefreshesTargetBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	oldExe, _ := writeFakeAO(t, filepath.Join(root, "build-1"))
	newExe, newRecord := writeFakeAO(t, filepath.Join(root, "build-2"))

	first, err := EnsureLauncherFor(dataDir, func() (string, error) { return oldExe, nil })
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureLauncherFor(dataDir, func() (string, error) { return newExe, nil })
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("launcher path changed across rebuilds: %q then %q", first, second)
	}
	if out, err := runHookCommand(t, ShellQuote(second)+" hooks claude-code stop"); err != nil {
		t.Fatalf("hook failed after rebuild: %v\n%s", err, out)
	}
	if _, err := os.Stat(newRecord); err != nil {
		t.Fatalf("hook did not reach the rebuilt binary: %v", err)
	}
}

func TestEnsureLauncherHandlesPathsWithSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	exe, record := writeFakeAO(t, filepath.Join(root, "Application Support", "Agent Orchestrator"))
	dataDir := filepath.Join(root, "my ao data")

	launcher, err := EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatalf("EnsureLauncherFor: %v", err)
	}
	if !strings.Contains(launcher, " ") {
		t.Fatalf("launcher %q lost the spaces the fixture depends on", launcher)
	}
	if out, err := runHookCommand(t, ShellQuote(launcher)+" hooks claude-code stop"); err != nil {
		t.Fatalf("hook command failed for a path with spaces: %v\n%s", err, out)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("hook did not reach the AO binary: %v", err)
	}
}

func TestEnsureLauncherFallsBackToExecutableWithoutDataDir(t *testing.T) {
	root := t.TempDir()
	exe, _ := writeFakeAO(t, root)
	got, err := EnsureLauncherFor("", func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatalf("EnsureLauncherFor: %v", err)
	}
	if got != exe {
		t.Fatalf("launcher = %q, want the executable %q", got, exe)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("launcher %q is not absolute", got)
	}
}

func TestEnsureLauncherAbsolutizesRelativeExecutable(t *testing.T) {
	got, err := EnsureLauncherFor("", func() (string, error) { return filepath.Join("bin", "ao"), nil })
	if err != nil {
		t.Fatalf("EnsureLauncherFor: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("launcher = %q, want an absolute path", got)
	}
}

func TestShellQuoteSurvivesRoundTripThroughSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh quoting is Unix-only")
	}
	for _, value := range []string{
		"/opt/ao/bin/ao",
		"/Users/dev user/Applications/Agent Orchestrator.app/Contents/MacOS/ao",
		"/tmp/dev's build/ao",
		`/tmp/quote"and space/ao`,
		"/tmp/dollar$PATH/ao",
	} {
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+ShellQuote(value)).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", value, err)
		}
		if string(out) != value {
			t.Fatalf("ShellQuote(%q) round-tripped as %q", value, string(out))
		}
	}
}

func TestSplitCommandWord(t *testing.T) {
	for _, tc := range []struct{ command, word, rest string }{
		{"ao hooks claude-code stop", "ao", "hooks claude-code stop"},
		{"  ao hooks claude-code stop", "ao", "hooks claude-code stop"},
		{"'/opt/ao/bin/ao' hooks claude-code stop", "/opt/ao/bin/ao", "hooks claude-code stop"},
		{`'/Users/dev user/ao' hooks claude-code stop`, "/Users/dev user/ao", "hooks claude-code stop"},
		{`"/opt/ao bin/ao" hooks droid stop`, "/opt/ao bin/ao", "hooks droid stop"},
		{`'/tmp/dev'"'"'s/ao' hooks qwen stop`, "/tmp/dev's/ao", "hooks qwen stop"},
		{"ao", "ao", ""},
		{"", "", ""},
	} {
		word, rest := SplitCommandWord(tc.command)
		if word != tc.word || rest != tc.rest {
			t.Fatalf("SplitCommandWord(%q) = (%q, %q), want (%q, %q)", tc.command, word, rest, tc.word, tc.rest)
		}
	}
}

func TestIsAOExecutable(t *testing.T) {
	for _, tc := range []struct {
		word string
		want bool
	}{
		{"ao", true},
		{"/opt/ao/bin/ao", true},
		{"/Users/dev user/.ao/data/hook-bin/ao", true},
		{"", false},
		{"echo", false},
		{"/usr/local/bin/aotool", false},
	} {
		if got := IsAOExecutable(tc.word); got != tc.want {
			t.Fatalf("IsAOExecutable(%q) = %v, want %v", tc.word, got, tc.want)
		}
	}
}

// writeFakeAOAt writes a stand-in AO binary at exe that appends its arguments
// to record. Unlike writeFakeAO the record lives wherever the caller wants, so
// a test can delete the binary's whole directory tree and still read it back.
func writeFakeAOAt(t *testing.T, exe, record string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(exe), 0o750); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + ShellQuote(record) + "\nexit 0\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil { // #nosec G306 -- test fixture must be executable
		t.Fatal(err)
	}
}

// goBuildExe returns a path shaped exactly like the one `go run` hands the
// process it starts: <tmp>/go-build<digits>/b001/exe/<name>.
func goBuildExe(root, name string) string {
	return filepath.Join(root, "go-build2455361342", "b001", "exe", name)
}

// TestEnsureLauncherSurvivesEphemeralGoBuildBinaryVanishing reproduces the
// reported failure: AO started through `go run`, the launcher recorded the
// temp go-build executable, the temp tree was reaped, and every hook then died
// with "No such file or directory". The launcher must keep working after the
// go-build tree is gone.
func TestEnsureLauncherSurvivesEphemeralGoBuildBinaryVanishing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	record := filepath.Join(root, "argv.txt")
	buildRoot := filepath.Join(root, "tmp")
	exe := goBuildExe(buildRoot, "ao")
	writeFakeAOAt(t, exe, record)

	launcher, err := EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatalf("EnsureLauncherFor: %v", err)
	}

	// The go-build tree is deleted when the `go run` process exits.
	if err := os.RemoveAll(buildRoot); err != nil {
		t.Fatal(err)
	}

	script, err := os.ReadFile(launcher) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(script), exe) {
		t.Fatalf("launcher persisted the ephemeral go-build path:\n%s", script)
	}
	if out, err := runHookCommand(t, ShellQuote(launcher)+" hooks claude-code stop"); err != nil {
		t.Fatalf("hook failed after the go-build binary vanished: %v\n%s", err, out)
	}
	got, err := os.ReadFile(record) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatalf("hook did not reach an AO binary: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hooks claude-code stop" {
		t.Fatalf("AO binary received %q, want %q", strings.TrimSpace(string(got)), "hooks claude-code stop")
	}
}

// TestEnsureLauncherRepairsStaleEphemeralLauncher covers the heal-on-restart
// path: a launcher left pointing at a dead go-build binary must be rewritten
// by the next launch, whether that launch is another `go run` or an installed
// AO.
func TestEnsureLauncherRepairsStaleEphemeralLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/bin/sh hook execution is Unix-only")
	}
	for _, tc := range []struct{ name, secondDir string }{
		{"restarted through go run", ""},
		{"restarted as an installed binary", "install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			firstBuild := filepath.Join(root, "tmp-1")
			firstExe := goBuildExe(firstBuild, "ao")
			writeFakeAOAt(t, firstExe, filepath.Join(root, "argv-1.txt"))
			if _, err := EnsureLauncherFor(dataDir, func() (string, error) { return firstExe, nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(firstBuild); err != nil {
				t.Fatal(err)
			}

			record := filepath.Join(root, "argv-2.txt")
			secondExe := goBuildExe(filepath.Join(root, "tmp-2"), "ao")
			if tc.secondDir != "" {
				secondExe = filepath.Join(root, tc.secondDir, "ao")
			}
			writeFakeAOAt(t, secondExe, record)

			launcher, err := EnsureLauncherFor(dataDir, func() (string, error) { return secondExe, nil })
			if err != nil {
				t.Fatal(err)
			}
			if out, err := runHookCommand(t, ShellQuote(launcher)+" hooks claude-code stop"); err != nil {
				t.Fatalf("repaired hook failed: %v\n%s", err, out)
			}
			if _, err := os.Stat(record); err != nil {
				t.Fatalf("hook did not reach the current binary: %v", err)
			}
		})
	}
}

// TestEnsureLauncherTargetsStableExecutableDirectly pins the packaged/installed
// behavior: no copy is made, and the shim execs the real binary so app upgrades
// stay in charge of it.
func TestEnsureLauncherTargetsStableExecutableDirectly(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	exe, _ := writeFakeAO(t, filepath.Join(root, "Agent Orchestrator.app", "Contents", "MacOS"))

	launcher, err := EnsureLauncherFor(dataDir, func() (string, error) { return exe, nil })
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(launcher) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), exe) {
		t.Fatalf("launcher does not exec the installed binary %q:\n%s", exe, script)
	}
	if FileExists(StableBinaryPath(dataDir)) {
		t.Fatalf("launcher copied a stable binary it should have referenced in place")
	}
}

// TestEnsureLauncherDropsStaleCopyWhenBinaryBecomesStable covers a dev machine
// that later runs an installed AO: the parked dev copy must not linger.
func TestEnsureLauncherDropsStaleCopyWhenBinaryBecomesStable(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	devExe := goBuildExe(filepath.Join(root, "tmp"), "ao")
	writeFakeAOAt(t, devExe, filepath.Join(root, "argv.txt"))
	if _, err := EnsureLauncherFor(dataDir, func() (string, error) { return devExe, nil }); err != nil {
		t.Fatal(err)
	}
	if !FileExists(StableBinaryPath(dataDir)) {
		t.Fatalf("expected an AO-owned copy of the ephemeral binary at %q", StableBinaryPath(dataDir))
	}

	installed, _ := writeFakeAO(t, filepath.Join(root, "install"))
	if _, err := EnsureLauncherFor(dataDir, func() (string, error) { return installed, nil }); err != nil {
		t.Fatal(err)
	}
	if FileExists(StableBinaryPath(dataDir)) {
		t.Fatalf("stale dev copy survived at %q", StableBinaryPath(dataDir))
	}
}

func TestIsEphemeralExecutable(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/var/folders/9v/x/T/go-build2455361342/b001/exe/ao", true},
		{"/tmp/go-build123/b001/exe/ao", true},
		{"/home/dev/.cache/go-build/b042/exe/ao", true},
		{"/some/where/b001/exe/ao", true},
		{"", false},
		{"/opt/ao/bin/ao", false},
		{"/usr/local/bin/ao", false},
		{"/Applications/Agent Orchestrator.app/Contents/MacOS/ao", false},
		{"/Users/dev/go/bin/ao", false},
		{"/Users/dev/projects/ao/build/exe/ao", false},
	} {
		if got := IsEphemeralExecutable(tc.path); got != tc.want {
			t.Fatalf("IsEphemeralExecutable(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestInspectLauncher_HealthyInstalledLauncher(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	installed, _ := writeFakeAO(t, filepath.Join(root, "install"))
	if _, err := EnsureLauncherFor(dataDir, func() (string, error) { return installed, nil }); err != nil {
		t.Fatal(err)
	}

	insp := InspectLauncher(dataDir)

	if !insp.Present || !insp.Executable {
		t.Fatalf("launcher present=%v executable=%v, want both true", insp.Present, insp.Executable)
	}
	if insp.Target != installed {
		t.Fatalf("target = %q, want the installed binary %q", insp.Target, installed)
	}
	if !insp.TargetPresent || !insp.TargetExecutable || insp.TargetEphemeral {
		t.Fatalf("target present=%v executable=%v ephemeral=%v, want true/true/false",
			insp.TargetPresent, insp.TargetExecutable, insp.TargetEphemeral)
	}
	if !insp.Healthy() {
		t.Fatal("Healthy() = false for a launcher targeting an installed binary")
	}
}

// TestInspectLauncher_StaleGoBuildTarget is the fd9b87fae defect as an
// observation: a shim written while AO ran from the Go build cache still names
// that temp binary after it has been deleted. The file itself is well-formed
// and executable, so only the target tells the truth.
func TestInspectLauncher_StaleGoBuildTarget(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	ephemeral := filepath.Join(root, "go-build2455361342", "b001", "exe", "ao")
	exe, _ := writeFakeAO(t, filepath.Dir(ephemeral))
	if err := os.Rename(exe, ephemeral); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(LauncherDir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(LauncherDir(dataDir), LauncherName())
	if err := AtomicWriteFile(path, []byte(launcherScript(ephemeral)), 0o700); err != nil {
		t.Fatal(err)
	}

	// The build cache is swept when the process that produced it exits.
	if err := os.RemoveAll(filepath.Join(root, "go-build2455361342")); err != nil {
		t.Fatal(err)
	}

	insp := InspectLauncher(dataDir)

	if !insp.Present || !insp.Executable {
		t.Fatalf("shim present=%v executable=%v, want both true: the shim itself is fine", insp.Present, insp.Executable)
	}
	if insp.Target != ephemeral {
		t.Fatalf("target = %q, want %q", insp.Target, ephemeral)
	}
	if !insp.TargetEphemeral {
		t.Fatal("TargetEphemeral = false for a go-build path")
	}
	if insp.TargetPresent {
		t.Fatal("TargetPresent = true for a deleted build-cache binary")
	}
	if insp.Healthy() {
		t.Fatal("Healthy() = true for a launcher pinned to a deleted build-cache binary")
	}
}

func TestInspectLauncher_MissingLauncher(t *testing.T) {
	dataDir := t.TempDir()

	insp := InspectLauncher(dataDir)

	if insp.Present {
		t.Fatal("Present = true with no launcher installed")
	}
	if insp.Path != filepath.Join(LauncherDir(dataDir), LauncherName()) {
		t.Fatalf("Path = %q, want the expected launcher path even when absent", insp.Path)
	}
	if insp.Healthy() {
		t.Fatal("Healthy() = true with no launcher installed")
	}
	if insp.Error != "" {
		t.Fatalf("Error = %q, want empty: an absent launcher is not a read failure", insp.Error)
	}
}

func TestInspectLauncher_EmptyDataDirIsNotAJudgement(t *testing.T) {
	insp := InspectLauncher("  ")

	if insp != (LauncherInspection{}) {
		t.Fatalf("InspectLauncher(\"\") = %+v, want the zero inspection", insp)
	}
}

func TestParseLauncherTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   string
	}{
		{"shell shim", "#!/bin/sh\nexec '/opt/ao/bin/ao' \"$@\"\n", "/opt/ao/bin/ao"},
		{"shell shim with a quote in the path", "#!/bin/sh\nexec '/opt/it'\"'\"'s/ao' \"$@\"\n", "/opt/it's/ao"},
		{"windows shim", "@echo off\r\n\"C:\\ao\\ao.exe\" %*\r\n", `C:\ao\ao.exe`},
		{"empty", "", ""},
		{"comment only", "#!/bin/sh\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLauncherTarget(tc.script); got != tc.want {
				t.Fatalf("parseLauncherTarget() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A packaged install can leave the shim pointing at a symlink whose target has
// since been removed (an app bundle replaced, a dev symlink into a deleted
// build dir). The shim resolves, the link resolves, and the hook still dies.
func TestInspectLauncher_DanglingSymlinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	binary, _ := writeFakeAO(t, filepath.Join(root, "build"))
	link := filepath.Join(root, "bin", "ao")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureLauncherFor(dataDir, func() (string, error) { return link, nil }); err != nil {
		t.Fatal(err)
	}
	if insp := InspectLauncher(dataDir); !insp.Healthy() {
		t.Fatalf("Healthy() = false while the symlink still resolves: %+v", insp)
	}

	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}

	insp := InspectLauncher(dataDir)

	if insp.Target != link {
		t.Fatalf("target = %q, want the symlink %q", insp.Target, link)
	}
	if insp.TargetPresent || insp.TargetExecutable {
		t.Fatalf("target present=%v executable=%v, want both false for a dangling symlink",
			insp.TargetPresent, insp.TargetExecutable)
	}
	if insp.TargetEphemeral {
		t.Fatal("TargetEphemeral = true for a stable symlink path")
	}
	if insp.Healthy() {
		t.Fatal("Healthy() = true for a launcher targeting a dangling symlink")
	}
}
