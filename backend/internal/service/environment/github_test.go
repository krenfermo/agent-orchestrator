package environment

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestProbeGitHubNotInstalled(t *testing.T) {
	deps := ghDeps{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		CombinedOutput: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("CombinedOutput should not be called when gh is not installed")
			return nil, nil
		},
	}
	got := probeGitHub(context.Background(), deps, fixedClock(time.Unix(0, 0)))
	if got.Installed {
		t.Error("Installed = true, want false")
	}
	if got.AuthState != GitHubAuthStateUnknown {
		t.Errorf("AuthState = %q, want %q", got.AuthState, GitHubAuthStateUnknown)
	}
	if got.ErrorCode != "GH_NOT_INSTALLED" {
		t.Errorf("ErrorCode = %q, want GH_NOT_INSTALLED", got.ErrorCode)
	}
}

func TestProbeGitHubUnauthenticated(t *testing.T) {
	deps := ghDeps{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		CombinedOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("gh version 2.40.0 (2023-12-13)\n"), nil
			}
			return []byte("You are not logged into any GitHub hosts. Run gh auth login to authenticate.\n"), errors.New("exit status 1")
		},
	}
	got := probeGitHub(context.Background(), deps, fixedClock(time.Unix(0, 0)))
	if !got.Installed {
		t.Error("Installed = false, want true")
	}
	if got.AuthState != GitHubAuthStateUnauthenticated {
		t.Errorf("AuthState = %q, want %q", got.AuthState, GitHubAuthStateUnauthenticated)
	}
	if got.Login != "" || got.Host != "" {
		t.Errorf("Login/Host = %q/%q, want empty", got.Login, got.Host)
	}
}

func TestProbeGitHubAuthenticated(t *testing.T) {
	deps := ghDeps{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		CombinedOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("gh version 2.40.0 (2023-12-13)\n"), nil
			}
			return []byte("github.com\n  ✓ Logged in to github.com account octocat (keyring)\n  - Active account: true\n  - Git operations protocol: https\n  - Token: gho_************************************\n"), nil
		},
	}
	got := probeGitHub(context.Background(), deps, fixedClock(time.Unix(0, 0)))
	if got.AuthState != GitHubAuthStateAuthenticated {
		t.Errorf("AuthState = %q, want %q", got.AuthState, GitHubAuthStateAuthenticated)
	}
	if got.Host != "github.com" {
		t.Errorf("Host = %q, want github.com", got.Host)
	}
	if got.Login != "octocat" {
		t.Errorf("Login = %q, want octocat", got.Login)
	}
	if got.Version != "gh version 2.40.0 (2023-12-13)" {
		t.Errorf("Version = %q", got.Version)
	}
}

func TestProbeGitHubUnparsedAuthOutput(t *testing.T) {
	deps := ghDeps{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		CombinedOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("gh version 99.0.0\n"), nil
			}
			return []byte("some future gh output format we don't recognize\n"), nil
		},
	}
	got := probeGitHub(context.Background(), deps, fixedClock(time.Unix(0, 0)))
	if got.AuthState != GitHubAuthStateUnknown {
		t.Errorf("AuthState = %q, want %q", got.AuthState, GitHubAuthStateUnknown)
	}
	if got.ErrorCode != "GH_AUTH_STATUS_UNPARSED" {
		t.Errorf("ErrorCode = %q, want GH_AUTH_STATUS_UNPARSED", got.ErrorCode)
	}
}

// TestProbeGitHubNeverLeaksToken guards the "no secret leak" requirement: no
// matter what `gh auth status` prints, and even if a caller mistakenly passed
// `--show-token`-shaped output through this path, none of GitHubStatus's
// fields may ever contain a token-shaped string.
func TestProbeGitHubNeverLeaksToken(t *testing.T) {
	const tokenLike = "gho_1234567890ABCDEFghijklmnopqrstuvwxyz01"
	deps := ghDeps{
		LookPath: func(string) (string, error) { return "/usr/bin/gh", nil },
		CombinedOutput: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if args[0] == "--version" {
				return []byte("gh version 2.40.0\n"), nil
			}
			return []byte("github.com\n  ✓ Logged in to github.com account octocat (keyring)\n  - Token: " + tokenLike + "\n"), nil
		},
	}
	got := probeGitHub(context.Background(), deps, fixedClock(time.Unix(0, 0)))
	tokenRE := regexp.MustCompile(`gh[oprsu]_[A-Za-z0-9]{20,}`)
	for name, val := range map[string]string{
		"BinaryPath": got.BinaryPath,
		"Version":    got.Version,
		"Login":      got.Login,
		"Host":       got.Host,
		"ErrorCode":  got.ErrorCode,
	} {
		if tokenRE.MatchString(val) || strings.Contains(val, tokenLike) {
			t.Errorf("field %s leaked a token-shaped value: %q", name, val)
		}
	}
}
