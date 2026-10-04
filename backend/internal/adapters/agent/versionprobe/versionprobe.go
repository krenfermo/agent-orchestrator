// Package versionprobe runs a cheap, timeout-bounded "<binary> --version"
// shell-out and extracts a clean, single-line version string. It exists so
// that `ao doctor` and any HTTP-facing capability probe (Settings' Development
// Agents section) share one implementation instead of drifting apart.
package versionprobe

import (
	"context"
	"regexp"
	"strings"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// DefaultTimeout bounds a single version probe. Version flags are expected to
// return near-instantly; anything slower is treated as a failed probe rather
// than blocking the caller.
const DefaultTimeout = 2 * time.Second

// KnownVersionArgs maps a harness/agent id to the flag that prints its
// version. Scoped to the harnesses this checkpoint's Settings surface reports
// on (Codex, Claude Code) — not every adapter in the registry.
var KnownVersionArgs = map[string]string{
	"codex":       "--version",
	"claude-code": "--version",
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// CLIVersion runs "<binary> <versionArg>" with DefaultTimeout and returns the
// first non-empty, ANSI-stripped line of output. An error (binary missing,
// non-zero exit, timeout) is returned as-is; callers that treat version
// detection as best-effort should ignore it rather than fail the whole probe.
func CLIVersion(ctx context.Context, binary, versionArg string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	out, err := aoprocess.CommandContext(reqCtx, binary, versionArg).CombinedOutput()
	if err != nil {
		return "", err
	}
	return FirstOutputLine(out), nil
}

// FirstOutputLine strips ANSI escape sequences and returns the first
// non-blank line of the given command output, trimmed of surrounding
// whitespace. Empty input yields an empty string.
func FirstOutputLine(out []byte) string {
	clean := strings.TrimSpace(ansiRE.ReplaceAllString(string(out), ""))
	if clean == "" {
		return ""
	}
	line := strings.SplitN(clean, "\n", 2)[0]
	return strings.TrimSpace(line)
}
