package sessionmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnsureAOShim writes (or overwrites) a small executable shim at
// <dataDir>/reviewer-runtime/bin/ao (ao.cmd on Windows) that execs the
// resolved daemon executable with whatever arguments it's called with, and
// returns the shim's directory. It exists so a bare `ao` command — the form
// every reviewer prompt and workspace hook config uses — still resolves
// correctly when the daemon's own executable isn't literally named "ao"
// (HookPATH's pin only works for that exact name; see HookPATH's doc
// comment). Prepending the returned directory to PATH, on top of HookPATH's
// own already-good result, is what makes `ao review submit`/`ao hooks`
// resolvable without requiring a global `ao` install or the caller
// discovering an absolute path on its own (Checkpoint 8I.2).
//
// Idempotent and cheap enough to call on every launch: PATH-pin failures are
// per-process (a differently-named dev build, a renamed binary), not
// persistent state, so there is nothing to cache across calls.
func EnsureAOShim(dataDir string, executable func() (string, error)) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("AO shim data directory is required")
	}
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("resolve AO executable: %w", err)
	}
	if !filepath.IsAbs(exe) {
		exe, err = filepath.Abs(exe)
		if err != nil {
			return "", fmt.Errorf("make AO executable absolute: %w", err)
		}
	}
	shimDir := filepath.Join(dataDir, "reviewer-runtime", "bin")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		return "", fmt.Errorf("create AO shim directory: %w", err)
	}
	shimPath := filepath.Join(shimDir, "ao")
	if runtime.GOOS == "windows" {
		shimPath += ".cmd"
	}
	if err := os.WriteFile(shimPath, []byte(aoShimScript(exe)), 0o600); err != nil {
		return "", fmt.Errorf("write AO shim: %w", err)
	}
	if err := os.Chmod(shimPath, 0o700); err != nil { // #nosec G302 -- the shim must be executable by the AO user.
		return "", fmt.Errorf("mark AO shim executable: %w", err)
	}
	return shimDir, nil
}

func aoShimScript(executable string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\n\"" + executable + "\" %*\r\n"
	}
	return "#!/bin/sh\nexec " + shimShellQuote(executable) + ` "$@"` + "\n"
}

func shimShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// PrependPathDir prepends dir to path, or returns dir alone when path is
// empty/blank — never emits a leading empty PATH entry (which some shells
// treat as "." for search purposes).
func PrependPathDir(dir, path string) string {
	if strings.TrimSpace(path) == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}
