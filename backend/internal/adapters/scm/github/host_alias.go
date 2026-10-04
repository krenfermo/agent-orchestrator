package github

// host_alias.go -- SSH host aliases, and why AO has to understand them.
//
// A GitHub remote does not have to say "github.com". A developer who juggles
// two GitHub accounts on one machine gives each an SSH alias:
//
//	Host github-nuevo
//	  HostName github.com
//	  IdentityFile ~/.ssh/id_ed25519_other
//
// and their origin then reads git@github-nuevo:owner/repo.git. Git resolves
// that alias through ssh(1) and pushes to GitHub exactly as it always did;
// AO's origin parser saw a host it did not recognize and classified the whole
// repository as "unsupported SCM origin", which turned every GitHub feature
// off for that project silently.
//
// The rule this file implements is narrow on purpose: an SSH remote whose
// literal host is not a GitHub host is resolved through the user's OWN ssh
// configuration, and it counts as GitHub only when that configuration says the
// alias points at a GitHub hostname. An arbitrary SSH host is never guessed
// into GitHub, and https:// remotes are not alias-resolved at all -- a URL host
// is the host.
//
// Evidence, in order:
//
//  1. The ssh config files themselves (~/.ssh/config, /etc/ssh/ssh_config,
//     plus whatever they Include). Pure file reads: no subprocess, fully
//     deterministic, and the shape every real alias uses.
//  2. `ssh -G <alias>`, which is ssh's own answer and therefore handles Match
//     blocks, canonicalization and anything else a hand parser would miss. It
//     is a fallback, bounded by a timeout, and its answers are cached.
//
// Both negative and positive answers are cached: the common case is a host
// that is not an alias at all, and re-reading the config for every poll of
// every repository would be the wrong kind of thorough.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// HostAliasResolver maps an SSH host alias to the real hostname ssh would
// connect to. It returns ok=false when the name is not an alias, which is the
// answer for almost every host.
type HostAliasResolver interface {
	ResolveHost(alias string) (string, bool)
}

const (
	// hostAliasTTL is how long one alias answer is trusted. An ssh config
	// edited while the daemon runs is picked up within this window; the window
	// exists so a poll loop does not stat and parse the config every cycle.
	hostAliasTTL = 5 * time.Minute
	// sshProbeTimeout bounds the `ssh -G` fallback. ssh -G does no network
	// work, so anything slower than this is a misconfiguration, not latency.
	sshProbeTimeout = 2 * time.Second
	// maxIncludeDepth bounds Include recursion. A config that includes itself
	// is a user error we decline to hang on.
	maxIncludeDepth = 8
	// maxAliasEntries bounds the resolver's memo. Alias names come from git
	// remotes, so the real key space is tiny; the cap is here so a pathological
	// project set cannot grow the map without bound.
	maxAliasEntries = 256
)

// SSHHostAliases resolves aliases from the user's ssh configuration.
//
// The zero value is usable and reads the standard config paths. Tests inject
// ConfigPaths and Probe so nothing touches the developer's real ~/.ssh.
type SSHHostAliases struct {
	// ConfigPaths are the ssh config files to read, most specific first. Nil
	// means the standard pair (~/.ssh/config, /etc/ssh/ssh_config).
	ConfigPaths []string
	// Probe is the `ssh -G` fallback. Nil means the real ssh binary; a test
	// that wants no subprocess sets it to a func returning ("", false).
	Probe func(ctx context.Context, alias string) (string, bool)
	// Clock is the cache clock. Nil means time.Now.
	Clock func() time.Time

	mu     sync.Mutex
	memo   map[string]aliasEntry
	config map[string]string
	loaded time.Time
}

type aliasEntry struct {
	host    string
	ok      bool
	expires time.Time
}

// NewSSHHostAliases returns a resolver over the standard ssh config paths.
func NewSSHHostAliases() *SSHHostAliases { return &SSHHostAliases{} }

// ResolveHost returns the hostname ssh would connect to for alias, and whether
// the alias resolved to something different from itself. A host that is not
// configured, or that maps to itself, returns ok=false: there is nothing to
// resolve and the caller should treat the literal host as final.
func (r *SSHHostAliases) ResolveHost(alias string) (string, bool) {
	name := strings.ToLower(strings.TrimSpace(alias))
	if name == "" {
		return "", false
	}
	now := r.now()

	r.mu.Lock()
	if entry, ok := r.memo[name]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.host, entry.ok
	}
	r.mu.Unlock()

	host, ok := r.lookup(name, now)
	if ok && strings.EqualFold(host, name) {
		// An alias whose HostName is its own name resolves nothing.
		host, ok = "", false
	}

	r.mu.Lock()
	if r.memo == nil {
		r.memo = map[string]aliasEntry{}
	}
	if len(r.memo) >= maxAliasEntries {
		r.memo = map[string]aliasEntry{}
	}
	r.memo[name] = aliasEntry{host: host, ok: ok, expires: now.Add(hostAliasTTL)}
	r.mu.Unlock()
	return host, ok
}

func (r *SSHHostAliases) lookup(name string, now time.Time) (string, bool) {
	if host, ok := r.fromConfig(name, now); ok {
		return host, true
	}
	probe := r.Probe
	if probe == nil {
		probe = sshProbeHostName
	}
	ctx, cancel := context.WithTimeout(context.Background(), sshProbeTimeout)
	defer cancel()
	return probe(ctx, name)
}

func (r *SSHHostAliases) fromConfig(name string, now time.Time) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.config == nil || now.Sub(r.loaded) >= hostAliasTTL {
		r.config = loadSSHHostNames(r.paths())
		r.loaded = now
	}
	host, ok := r.config[name]
	return host, ok && host != ""
}

func (r *SSHHostAliases) paths() []string {
	if len(r.ConfigPaths) > 0 {
		return r.ConfigPaths
	}
	var paths []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".ssh", "config"))
	}
	return append(paths, "/etc/ssh/ssh_config")
}

func (r *SSHHostAliases) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// loadSSHHostNames reads every config path and returns the literal
// alias -> HostName mapping it can prove.
//
// Only literal Host patterns are recorded. A pattern containing a wildcard
// says nothing about which concrete alias it applies to without implementing
// ssh's matching rules, and a wrong answer here classifies somebody's private
// SSH host as GitHub -- so wildcards are skipped and the `ssh -G` fallback
// handles them. Match blocks end the current Host block for the same reason:
// their condition is evaluated per connection, not per config read.
func loadSSHHostNames(paths []string) map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, path := range paths {
		readSSHConfig(path, out, seen, 0)
	}
	return out
}

func readSSHConfig(path string, out map[string]string, seen map[string]bool, depth int) {
	if depth > maxIncludeDepth || strings.TrimSpace(path) == "" {
		return
	}
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return
	}
	if seen[abs] {
		return
	}
	seen[abs] = true

	f, err := os.Open(abs) //nolint:gosec // an ssh config path the user already trusts ssh with
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var hosts []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		keyword, value, ok := splitSSHConfigLine(scanner.Text())
		if !ok {
			continue
		}
		switch keyword {
		case "host":
			hosts = literalHostPatterns(value)
		case "match":
			// A Match block is not a Host block; anything after it belongs to
			// a condition we do not evaluate.
			hosts = nil
		case "hostname":
			if value == "" {
				continue
			}
			for _, h := range hosts {
				if _, exists := out[h]; !exists {
					out[h] = strings.ToLower(value)
				}
			}
		case "include":
			for _, pattern := range strings.Fields(value) {
				expandInclude(abs, pattern, out, seen, depth)
			}
		}
	}
}

// expandInclude resolves one Include pattern relative to the including file's
// directory, as ssh does for relative paths, and reads every match.
func expandInclude(from, pattern string, out map[string]string, seen map[string]bool, depth int) {
	expanded := expandHome(pattern)
	if !filepath.IsAbs(expanded) {
		expanded = filepath.Join(filepath.Dir(from), expanded)
	}
	matches, err := filepath.Glob(expanded)
	if err != nil || len(matches) == 0 {
		readSSHConfig(expanded, out, seen, depth+1)
		return
	}
	for _, match := range matches {
		readSSHConfig(match, out, seen, depth+1)
	}
}

// splitSSHConfigLine returns the lowercased keyword and its value. ssh accepts
// both "Key value" and "Key=value"; comments and blank lines yield ok=false.
func splitSSHConfigLine(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	var keyword, value string
	if eq := strings.IndexByte(trimmed, '='); eq >= 0 && !strings.ContainsAny(trimmed[:eq], " \t") {
		keyword, value = trimmed[:eq], trimmed[eq+1:]
	} else {
		fields := strings.SplitN(trimmed, " ", 2)
		if len(fields) == 1 {
			fields = strings.SplitN(trimmed, "\t", 2)
		}
		keyword = fields[0]
		if len(fields) == 2 {
			value = fields[1]
		}
	}
	return strings.ToLower(strings.TrimSpace(keyword)), strings.Trim(strings.TrimSpace(value), `"`), true
}

// literalHostPatterns keeps only the patterns that name one concrete host.
func literalHostPatterns(value string) []string {
	var out []string
	for _, pattern := range strings.Fields(value) {
		if strings.ContainsAny(pattern, "*?!") {
			continue
		}
		out = append(out, strings.ToLower(pattern))
	}
	return out
}

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// sshProbeHostName asks ssh itself. `ssh -G host` prints the fully resolved
// configuration without connecting to anything; we read only the hostname line.
func sshProbeHostName(ctx context.Context, alias string) (string, bool) {
	out, err := aoprocess.CommandContext(ctx, "ssh", "-G", alias).Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		keyword, value, ok := splitSSHConfigLine(line)
		if ok && keyword == "hostname" && value != "" {
			return strings.ToLower(value), true
		}
	}
	return "", false
}
