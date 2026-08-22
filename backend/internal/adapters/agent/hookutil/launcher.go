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
// upgrades, app-bundle relocation, and `go run` rebuilds (whose executable
// lives in a temp directory that is deleted when the process exits). Every
// launch rewrites the shim to exec whatever daemon binary is current, so the
// stable path always reaches the running AO.
const (
	// LauncherDirName is the AO-owned directory, relative to the data dir,
	// holding the hook launcher.
	LauncherDirName = "hook-bin"
	// LauncherBaseName is the AO CLI's executable name. Hook commands are
	// recognized as AO-owned by this name (see hooksjson), so it must match
	// the name the shim is written under.
	LauncherBaseName = "ao"
)

// LauncherName is the shim's file name: bare on Unix, a .cmd batch wrapper on
// Windows so cmd.exe will execute it.
func LauncherName() string {
	if runtime.GOOS == "windows" {
		return LauncherBaseName + ".cmd"
	}
	return LauncherBaseName
}

// LauncherDir is the directory EnsureLauncher writes the shim into.
func LauncherDir(dataDir string) string {
	return filepath.Join(dataDir, LauncherDirName)
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
	path := filepath.Join(dir, LauncherName())
	if err := AtomicWriteFile(path, []byte(launcherScript(exe)), 0o700); err != nil { // #nosec G302 -- the launcher must be executable by the AO user
		return exe, nil //nolint:nilerr // see EnsureLauncher's fallback contract
	}
	return path, nil
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
