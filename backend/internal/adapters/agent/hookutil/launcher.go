package hookutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The hook files AO installs into a workspace are executed by the agent CLI
// through a bare, non-interactive shell (`/bin/sh -c "<command>"`). That shell
// sources no ~/.zshrc, ~/.bashrc, profile, or alias definition, so it can only
// resolve a command name through whatever PATH it happens to inherit. A hook
// written as a bare `ao hooks <agent> <event>` therefore dies with exit 127
// ("/bin/sh: ao: command not found") on every machine where AO is not
// installed on the global PATH — the normal case, because AO ships as a
// desktop app whose daemon binary lives inside its own install tree.
//
// EnsureLauncher removes that ambiguity: hook commands invoke an absolute path
// AO itself owns and writes. The path points at a tiny shim under the AO data
// directory rather than at the daemon executable directly, so the command
// string persisted into a workspace settings file stays stable across daemon
// upgrades, app-bundle relocation, and rebuilds. Every launch rewrites the
// shim to exec whatever daemon binary is current, so the stable path always
// reaches the running AO.
//
// The shim's *target* needs the same care. Under `go run`/`go test` the
// running executable lives in the Go build cache (a temp directory deleted
// when the process exits), so a shim recording that path degrades into
// "No such file or directory" the moment AO restarts or exits — a hook that
// fires afterwards fails with no way back. EnsureLauncher therefore never
// persists an ephemeral path: it copies such a binary into the AO data
// directory and targets the copy. A stable, installed executable is still
// targeted directly.
const (
	// LauncherDirName is the AO-owned directory, relative to the data dir,
	// holding the hook launcher.
	LauncherDirName = "hook-bin"
	// LauncherBaseName is the AO CLI's executable name. Hook commands are
	// recognized as AO-owned by this name (see hooksjson), so it must match
	// the name the shim is written under.
	LauncherBaseName = "ao"
	// StableBinaryBaseName is the name of the AO-owned copy of the daemon
	// binary the shim execs when the running executable lives at an ephemeral
	// path (`go run`, `go test`, any Go build-cache exe directory). It is
	// deliberately NOT LauncherBaseName: the shim itself occupies that name in
	// the same directory.
	StableBinaryBaseName = "ao-bin"
)

// LauncherName is the shim's file name: bare on Unix, a .cmd batch wrapper on
// Windows so cmd.exe will execute it.
func LauncherName() string {
	if runtime.GOOS == "windows" {
		return LauncherBaseName + ".cmd"
	}
	return LauncherBaseName
}

// StableBinaryName is the file name of the AO-owned copy of the daemon binary.
func StableBinaryName() string {
	if runtime.GOOS == "windows" {
		return StableBinaryBaseName + ".exe"
	}
	return StableBinaryBaseName
}

// LauncherDir is the directory EnsureLauncher writes the shim into.
func LauncherDir(dataDir string) string {
	return filepath.Join(dataDir, LauncherDirName)
}

// StableBinaryPath is where EnsureLauncher parks its own copy of an ephemeral
// daemon binary.
func StableBinaryPath(dataDir string) string {
	return filepath.Join(LauncherDir(dataDir), StableBinaryName())
}

// IsEphemeralExecutable reports whether path names a binary produced into the
// Go build cache — the layout `go run`/`go test` use, e.g.
// /var/folders/.../T/go-build2455361342/b001/exe/ao. Such a binary is deleted
// when the process that ran it exits, so persisting its path into a hook
// command produces the "No such file or directory" failure this guards
// against: the hook fires long after the path stopped existing.
func IsEphemeralExecutable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i, part := range parts {
		part = strings.ToLower(part)
		if strings.HasPrefix(part, "go-build") {
			return true
		}
		// The per-action work directory layout inside the build cache:
		// <root>/b001/exe/<name>.
		if part == "exe" && i > 0 && isBuildActionDir(parts[i-1]) {
			return true
		}
	}
	return false
}

// isBuildActionDir reports whether name is a Go build action directory ("b001").
func isBuildActionDir(name string) bool {
	if len(name) < 2 || (name[0] != 'b' && name[0] != 'B') {
		return false
	}
	for _, r := range name[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// EnsureLauncher writes (or refreshes) AO's hook launcher under dataDir and
// returns its absolute path, suitable for embedding in a hook command.
//
// It is deterministic: the returned path depends only on dataDir and the
// running executable, never on the caller's PATH, shell, or shell startup
// files. It is idempotent and cheap enough to call on every session launch.
//
// When dataDir is empty (callers that have no AO state root) or the shim
// cannot be written, it falls back to the absolute path of the running
// executable. That is still an absolute, PATH-independent command — it just
// loses the stable-across-rebuilds property the shim provides.
func EnsureLauncher(dataDir string) (string, error) {
	return EnsureLauncherFor(dataDir, os.Executable)
}

// EnsureLauncherFor is EnsureLauncher with an injectable executable resolver so
// tests can pin the target binary.
func EnsureLauncherFor(dataDir string, executable func() (string, error)) (string, error) {
	if executable == nil {
		executable = os.Executable
	}
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("resolve AO hook executable: %w", err)
	}
	if strings.TrimSpace(exe) == "" {
		return "", fmt.Errorf("resolve AO hook executable: empty path")
	}
	if !filepath.IsAbs(exe) {
		exe, err = filepath.Abs(exe)
		if err != nil {
			return "", fmt.Errorf("make AO hook executable absolute: %w", err)
		}
	}
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return exe, nil
	}
	if !filepath.IsAbs(dataDir) {
		abs, absErr := filepath.Abs(dataDir)
		if absErr != nil {
			return exe, nil //nolint:nilerr // an unusable data dir degrades to the direct executable path
		}
		dataDir = abs
	}
	dir := LauncherDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return exe, nil //nolint:nilerr // see EnsureLauncher's fallback contract
	}
	target := stableTarget(dataDir, exe)
	path := filepath.Join(dir, LauncherName())
	if err := AtomicWriteFile(path, []byte(launcherScript(target)), 0o700); err != nil { // #nosec G302 -- the launcher must be executable by the AO user
		return exe, nil //nolint:nilerr // see EnsureLauncher's fallback contract
	}
	return path, nil
}

// stableTarget returns the binary the shim should exec.
//
// For an installed or packaged AO the running executable already lives at a
// stable path, and the shim execs it directly — that keeps hooks reaching the
// exact binary the user launched, and leaves app upgrades in charge of the
// file. For a binary at an ephemeral Go build-cache path (`go run`, `go
// test`), the running executable is deleted the moment AO exits, so AO instead
// copies it into its own data directory and points the shim at that copy. The
// hook then keeps working after the temp binary is gone, which is exactly the
// stale-launcher failure this repairs — and because the shim and the copy are
// refreshed on every launch, an already-stale launcher heals itself on the
// next start.
func stableTarget(dataDir, exe string) string {
	if !IsEphemeralExecutable(exe) {
		// A previous dev run may have parked a copy; it is stale now, and the
		// shim no longer references it.
		_ = os.Remove(StableBinaryPath(dataDir))
		return exe
	}
	stable := StableBinaryPath(dataDir)
	if err := copyExecutable(exe, stable); err != nil {
		// Best effort: a copy left by an earlier launch is still a better
		// target than a path that will not exist when the hook fires.
		if IsExecutableFile(stable) {
			return stable
		}
		return exe
	}
	return stable
}

// copyExecutable copies src to dst as an executable file, skipping the copy
// when dst already holds the same build (same size and modification time).
// The write goes through AtomicWriteFile so a hook firing mid-copy sees either
// the old binary or the new one, never a truncated file.
func copyExecutable(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat AO hook executable: %w", err)
	}
	if dstInfo, ok := statRegularFile(dst); ok &&
		dstInfo.Size() == srcInfo.Size() && dstInfo.ModTime().Equal(srcInfo.ModTime()) {
		return nil
	}
	data, err := os.ReadFile(src) //nolint:gosec // src is the running AO executable
	if err != nil {
		return fmt.Errorf("read AO hook executable: %w", err)
	}
	if err := AtomicWriteFile(dst, data, 0o700); err != nil { // #nosec G302 -- the copy must be executable by the AO user
		return fmt.Errorf("write AO hook executable copy: %w", err)
	}
	// Carry the source mtime so the next launch can skip an identical copy.
	if err := os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime()); err != nil {
		return nil //nolint:nilerr // the copy is usable; only the skip-fast path is lost
	}
	return nil
}

// launcherScript is a shim that execs the resolved AO binary with the hook's
// arguments, forwarding the exit status unchanged so the agent still sees a
// hook failure as a failure.
func launcherScript(executable string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\n\"" + executable + "\" %*\r\n"
	}
	return "#!/bin/sh\nexec " + ShellQuote(executable) + " \"$@\"\n"
}

// ShellQuote renders value as a single shell word, so a path containing
// spaces, quotes, or other metacharacters survives being spliced into a hook
// command string that the agent runs through a shell.
func ShellQuote(value string) string {
	if runtime.GOOS == "windows" {
		return `"` + value + `"`
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// SplitCommandWord splits the first shell word off command, honoring single
// and double quotes the way ShellQuote emits them, and returns the unquoted
// word plus the remainder with leading spaces trimmed. It is deliberately
// minimal: it exists to recover the executable a hook command invokes, not to
// implement shell parsing.
func SplitCommandWord(command string) (word, rest string) {
	command = strings.TrimLeft(command, " \t")
	var b strings.Builder
	i := 0
	for i < len(command) {
		c := command[i]
		switch c {
		case ' ', '\t':
			return b.String(), strings.TrimLeft(command[i:], " \t")
		case '\'', '"':
			end := strings.IndexByte(command[i+1:], c)
			if end < 0 {
				b.WriteString(command[i+1:])
				return b.String(), ""
			}
			b.WriteString(command[i+1 : i+1+end])
			i += end + 2
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), ""
}

// IsAOExecutable reports whether word (a command's first shell word, already
// unquoted) names AO's CLI, whether it is the bare `ao` a legacy hook file
// carries or an absolute path to the daemon binary or launcher shim.
func IsAOExecutable(word string) bool {
	word = strings.TrimSpace(word)
	if word == "" {
		return false
	}
	base := filepath.Base(word)
	if runtime.GOOS == "windows" {
		base = strings.ToLower(base)
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".cmd"), ".exe")
	}
	return base == LauncherBaseName
}
