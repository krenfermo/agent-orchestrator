// Package environment aggregates local, read-only capability probes (Codex,
// Claude Code, GitHub CLI, registered projects) into the Setup UX Settings
// surface. Every field it reports comes from a real local probe — installed,
// authenticated, available, or unknown — never a guessed or invented status.
package environment

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// GitHubAuthState mirrors ports.AgentAuthStatus's three-value shape for the
// GitHub CLI, kept as its own type so this package does not need to import
// the agent ports for an unrelated capability.
type GitHubAuthState string

const (
	GitHubAuthStateAuthenticated   GitHubAuthState = "authenticated"
	GitHubAuthStateUnauthenticated GitHubAuthState = "unauthenticated"
	GitHubAuthStateUnknown         GitHubAuthState = "unknown"
)

// GitHubStatus is the Settings-facing snapshot of the `gh` CLI. It never
// carries a token: Authenticated/Login/Host come from parsing `gh auth
// status`'s human-readable summary, not from reading or printing the token
// itself.
type GitHubStatus struct {
	Installed     bool            `json:"installed"`
	BinaryPath    string          `json:"binaryPath,omitempty"`
	Version       string          `json:"version,omitempty"`
	AuthState     GitHubAuthState `json:"authState" enum:"authenticated,unauthenticated,unknown"`
	Login         string          `json:"login,omitempty"`
	Host          string          `json:"host,omitempty"`
	LastCheckedAt time.Time       `json:"lastCheckedAt"`
	// ErrorCode is a safe, non-secret classification of a probe failure (e.g.
	// "GH_NOT_INSTALLED", "GH_AUTH_STATUS_FAILED"). Never derived from raw
	// command output, which could echo unexpected content.
	ErrorCode string `json:"errorCode,omitempty"`
}

// ghDeps is the small injectable shell-out surface this probe needs,
// mirroring the shape `ao doctor` already uses (see cli/root.go's Deps) so
// tests never touch the real `gh` binary.
type ghDeps struct {
	LookPath       func(name string) (string, error)
	CombinedOutput func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func defaultGHDeps() ghDeps {
	return ghDeps{
		LookPath: exec.LookPath,
		CombinedOutput: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return aoprocess.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

const ghProbeTimeout = 2 * time.Second

var ghLoggedInRE = regexp.MustCompile(`(?i)Logged in to (\S+)\s+(?:as|account)\s+([^\s(]+)`)

// probeGitHub runs the cheapest safe local probe for `gh`: LookPath, then
// `gh --version`, then `gh auth status` (never `--show-token`, never a call to
// the GitHub REST API). Every branch returns rather than panics; a probe
// failure degrades AuthState to "unknown", it never surfaces as an error to
// the caller — this mirrors the agent capability probe's advisory contract.
func probeGitHub(ctx context.Context, deps ghDeps, now func() time.Time) GitHubStatus {
	status := GitHubStatus{AuthState: GitHubAuthStateUnknown, LastCheckedAt: now()}

	path, err := deps.LookPath("gh")
	if err != nil || strings.TrimSpace(path) == "" {
		status.ErrorCode = "GH_NOT_INSTALLED"
		return status
	}
	status.Installed = true
	status.BinaryPath = path

	if out, err := runGH(ctx, deps, "--version"); err == nil {
		status.Version = firstLine(out)
	}

	out, err := runGH(ctx, deps, "auth", "status")
	if err != nil {
		// A non-zero exit from `gh auth status` is the normal "not logged in"
		// signal, not a probe failure — gh writes "You are not logged into any
		// GitHub hosts" to stderr and exits 1 in that case.
		status.AuthState = GitHubAuthStateUnauthenticated
		return status
	}
	if m := ghLoggedInRE.FindStringSubmatch(string(out)); len(m) == 3 {
		status.AuthState = GitHubAuthStateAuthenticated
		status.Host = m[1]
		status.Login = m[2]
		return status
	}
	// Exit 0 but the summary didn't match the expected shape (a future gh
	// version changed its wording): report unknown rather than guessing.
	status.ErrorCode = "GH_AUTH_STATUS_UNPARSED"
	return status
}

func runGH(ctx context.Context, deps ghDeps, args ...string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, ghProbeTimeout)
	defer cancel()
	return deps.CombinedOutput(reqCtx, "gh", args...)
}

func firstLine(out []byte) string {
	clean := strings.TrimSpace(string(out))
	if clean == "" {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(clean, "\n", 2)[0])
}
