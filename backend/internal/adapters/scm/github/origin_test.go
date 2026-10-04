package github

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// fakeAliases is a resolver with no filesystem and no subprocess behind it.
type fakeAliases map[string]string

func (f fakeAliases) ResolveHost(alias string) (string, bool) {
	host, ok := f[alias]
	return host, ok
}

func newTestProvider(t *testing.T, aliases HostAliasResolver) *Provider {
	t.Helper()
	p, err := NewProvider(ProviderOptions{Client: NewClient(ClientOptions{}), HostAliases: aliases})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestParseRepositoryOriginForms(t *testing.T) {
	// The alias map is deliberately the real MEDUSA case: the origin AO logged
	// as "unsupported SCM origin" against an ssh config that maps the alias to
	// github.com.
	p := newTestProvider(t, fakeAliases{
		"github-nuevo": "github.com",
		"internal-git": "git.corp.invalid",
	})

	for _, tc := range []struct {
		name   string
		remote string
		want   ports.SCMRepo
		ok     bool
	}{
		{
			name:   "https",
			remote: "https://github.com/krenfermo/medusa.git",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "krenfermo", Name: "medusa", Repo: "krenfermo/medusa"},
			ok:     true,
		},
		{
			name:   "ssh github.com",
			remote: "git@github.com:DarkaMX/MEDUSASASBACK.git",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "DarkaMX", Name: "MEDUSASASBACK", Repo: "DarkaMX/MEDUSASASBACK"},
			ok:     true,
		},
		{
			name:   "ssh host alias",
			remote: "git@github-nuevo:DarkaMX/MEDUSASASBACK.git",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "DarkaMX", Name: "MEDUSASASBACK", Repo: "DarkaMX/MEDUSASASBACK"},
			ok:     true,
		},
		{
			name:   "ssh scheme host alias",
			remote: "ssh://git@github-nuevo/DarkaMX/MEDUSASASBACK.git",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "DarkaMX", Name: "MEDUSASASBACK", Repo: "DarkaMX/MEDUSASASBACK"},
			ok:     true,
		},
		{
			name:   "ssh scheme github.com",
			remote: "ssh://git@github.com/aoagents/agent-orchestrator.git",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "aoagents", Name: "agent-orchestrator", Repo: "aoagents/agent-orchestrator"},
			ok:     true,
		},
		{
			name:   "bare owner/repo",
			remote: "aoagents/agent-orchestrator",
			want:   ports.SCMRepo{Provider: "github", Host: "github.com", Owner: "aoagents", Name: "agent-orchestrator", Repo: "aoagents/agent-orchestrator"},
			ok:     true,
		},
		// An alias that resolves to something that is not GitHub stays not
		// GitHub. This is the whole point of resolving rather than guessing.
		{name: "alias to a non-github host", remote: "git@internal-git:team/service.git"},
		{name: "unknown ssh host", remote: "git@bitbucket.org:team/service.git"},
		{name: "unconfigured alias", remote: "git@some-box:team/service.git"},
		// https is never alias-resolved: a URL host is the host.
		{name: "https non-github", remote: "https://gitlab.com/group/project.git"},
		{name: "https alias-looking host", remote: "https://github-nuevo/DarkaMX/MEDUSASASBACK.git"},
		{name: "empty", remote: ""},
		{name: "garbage", remote: "not a remote at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := p.ParseRepository(tc.remote)
			if ok != tc.ok {
				t.Fatalf("ParseRepository(%q) ok = %v; want %v (got %+v)", tc.remote, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseRepository(%q) = %+v; want %+v", tc.remote, got, tc.want)
			}
		})
	}
}

// A provider that cannot resolve aliases still classifies canonical origins,
// and still refuses everything else. Alias support is additive.
func TestParseRepositoryWithoutAliasResolver(t *testing.T) {
	p := newTestProvider(t, NoHostAliases{})
	if _, ok := p.ParseRepository("git@github.com:o/r.git"); !ok {
		t.Fatal("canonical ssh origin rejected without an alias resolver")
	}
	if _, ok := p.ParseRepository("git@github-nuevo:o/r.git"); ok {
		t.Fatal("alias origin accepted with alias resolution switched off")
	}
}

// The default provider installs the real ssh-config resolver rather than
// leaving alias support off, which is the bug this replaced.
func TestNewProviderInstallsAliasResolver(t *testing.T) {
	p := newTestProvider(t, nil)
	if p.aliases == nil {
		t.Fatal("provider built without a host alias resolver")
	}
}

func TestSplitSSHRemote(t *testing.T) {
	for _, tc := range []struct {
		remote            string
		host, owner, name string
		ok                bool
	}{
		{remote: "git@github-nuevo:DarkaMX/MEDUSASASBACK.git", host: "github-nuevo", owner: "DarkaMX", name: "MEDUSASASBACK", ok: true},
		{remote: "ssh://git@Alias-Box:2222/team/svc.git", host: "alias-box", owner: "team", name: "svc", ok: true},
		{remote: "https://github.com/o/r", ok: false},
		{remote: "git@github.com", ok: false},
		{remote: "o/r", ok: false},
	} {
		host, owner, name, ok := splitSSHRemote(tc.remote)
		if ok != tc.ok || (ok && (host != tc.host || owner != tc.owner || name != tc.name)) {
			t.Fatalf("splitSSHRemote(%q) = %q,%q,%q,%v; want %q,%q,%q,%v",
				tc.remote, host, owner, name, ok, tc.host, tc.owner, tc.name, tc.ok)
		}
	}
}

// The resolver contract the provider relies on: ResolveHost must never be
// consulted for a host that already is GitHub, so the common path costs
// nothing.
func TestParseRepositorySkipsAliasLookupForGitHubHosts(t *testing.T) {
	var asked []string
	p := newTestProvider(t, aliasSpy{fn: func(alias string) (string, bool) {
		asked = append(asked, alias)
		return "", false
	}})
	if _, ok := p.ParseRepository("git@github.com:o/r.git"); !ok {
		t.Fatal("canonical origin rejected")
	}
	if len(asked) != 0 {
		t.Fatalf("alias resolver consulted for a github.com origin: %v", asked)
	}
}

type aliasSpy struct{ fn func(string) (string, bool) }

func (a aliasSpy) ResolveHost(alias string) (string, bool) { return a.fn(alias) }
