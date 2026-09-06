package github

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// noProbe is the "do not shell out" fallback for tests that only exercise the
// config parser.
func noProbe(context.Context, string) (string, bool) { return "", false }

func TestSSHHostAliasesResolvesRealWorldAlias(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "config", `
Host 192.168.10.163
  HostName 192.168.10.163
  User udheka

Host github-nuevo
  HostName github.com
  User git
  IdentityFile ~/.ssh/id_ed25519_github_nuevo
  IdentitiesOnly yes
`)
	r := &SSHHostAliases{ConfigPaths: []string{cfg}, Probe: noProbe}

	host, ok := r.ResolveHost("github-nuevo")
	if !ok || host != "github.com" {
		t.Fatalf("ResolveHost(github-nuevo) = %q,%v; want github.com,true", host, ok)
	}
	// An alias whose HostName is itself resolves nothing: there is no
	// indirection to follow and reporting one would be a lie.
	if host, ok := r.ResolveHost("192.168.10.163"); ok {
		t.Fatalf("self-referential host resolved to %q", host)
	}
	if _, ok := r.ResolveHost("never-configured"); ok {
		t.Fatal("unconfigured host resolved")
	}
}

func TestSSHHostAliasesFollowsInclude(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "extra/work.conf", "Host gh-work\n  HostName github.com\n")
	cfg := writeFile(t, dir, "config", "Include extra/*.conf\n\nHost other\n  HostName example.invalid\n")
	r := &SSHHostAliases{ConfigPaths: []string{cfg}, Probe: noProbe}

	if host, ok := r.ResolveHost("gh-work"); !ok || host != "github.com" {
		t.Fatalf("included alias = %q,%v; want github.com,true", host, ok)
	}
	if host, ok := r.ResolveHost("other"); !ok || host != "example.invalid" {
		t.Fatalf("non-github alias = %q,%v; want example.invalid,true", host, ok)
	}
}

func TestSSHHostAliasesSkipsWildcardAndMatchBlocks(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "config", `
Host *
  HostName github.com

Match host bastion
  HostName github.com
`)
	r := &SSHHostAliases{ConfigPaths: []string{cfg}, Probe: noProbe}

	// A wildcard Host says nothing about which concrete alias it covers, and a
	// Match condition is evaluated per connection. Guessing either way would
	// classify an arbitrary SSH host as GitHub.
	if host, ok := r.ResolveHost("anything"); ok {
		t.Fatalf("wildcard pattern resolved %q", host)
	}
	if host, ok := r.ResolveHost("bastion"); ok {
		t.Fatalf("Match block resolved %q", host)
	}
}

func TestSSHHostAliasesFallsBackToSSHProbe(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "config", "Host unrelated\n  HostName example.invalid\n")
	calls := 0
	r := &SSHHostAliases{
		ConfigPaths: []string{cfg},
		Probe: func(_ context.Context, alias string) (string, bool) {
			calls++
			if alias == "gh-match" {
				return "github.com", true
			}
			return "", false
		},
	}

	if host, ok := r.ResolveHost("gh-match"); !ok || host != "github.com" {
		t.Fatalf("probe fallback = %q,%v; want github.com,true", host, ok)
	}
	// The answer is memoized, probe included: a poll loop must not fork ssh
	// once per repository per cycle.
	if _, _ = r.ResolveHost("gh-match"); calls != 1 {
		t.Fatalf("probe called %d times; want 1", calls)
	}
}

func TestSSHHostAliasesCachesNegativeAnswers(t *testing.T) {
	dir := t.TempDir()
	cfg := writeFile(t, dir, "config", "Host known\n  HostName github.com\n")
	calls := 0
	now := time.Now()
	r := &SSHHostAliases{
		ConfigPaths: []string{cfg},
		Clock:       func() time.Time { return now },
		Probe: func(context.Context, string) (string, bool) {
			calls++
			return "", false
		},
	}
	for i := 0; i < 5; i++ {
		if _, ok := r.ResolveHost("github.com"); ok {
			t.Fatal("plain hostname reported as an alias")
		}
	}
	if calls != 1 {
		t.Fatalf("probe called %d times for a repeated miss; want 1", calls)
	}

	// Past the TTL the config is re-read, so an alias added while the daemon
	// runs is picked up without a restart.
	writeFile(t, dir, "config", "Host github.com\n  HostName github.com\nHost added\n  HostName github.com\n")
	now = now.Add(hostAliasTTL + time.Second)
	if host, ok := r.ResolveHost("added"); !ok || host != "github.com" {
		t.Fatalf("alias added after first load = %q,%v; want github.com,true", host, ok)
	}
}

func TestSplitSSHConfigLineAcceptsEqualsForm(t *testing.T) {
	for _, tc := range []struct{ in, key, val string }{
		{"HostName=github.com", "hostname", "github.com"},
		{"  HostName   github.com  ", "hostname", "github.com"},
		{"\tHostName\tgithub.com", "hostname", "github.com"},
		{`HostName "github.com"`, "hostname", "github.com"},
	} {
		key, val, ok := splitSSHConfigLine(tc.in)
		if !ok || key != tc.key || val != tc.val {
			t.Fatalf("splitSSHConfigLine(%q) = %q,%q,%v; want %q,%q,true", tc.in, key, val, ok, tc.key, tc.val)
		}
	}
	if _, _, ok := splitSSHConfigLine("  # comment"); ok {
		t.Fatal("comment parsed as a directive")
	}
	if _, _, ok := splitSSHConfigLine("   "); ok {
		t.Fatal("blank line parsed as a directive")
	}
}
