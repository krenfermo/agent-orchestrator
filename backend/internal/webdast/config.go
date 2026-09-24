// Package webdast is AO's own, deliberately small, non-destructive web/API
// dynamic-analysis checker. It is the tool behind the active-pentest mode
// (Frente 2 / 2F).
//
// It is NOT a general scanner and it wraps no external tool. It runs a fixed,
// closed set of safe, reproducible, read-only checks against ONE authorized
// target, reachable only through the egress proxy AO stages beside it. It sends
// no destructive traffic: no fuzzing, no injection payloads, no brute force, no
// load. Every request is one a browser could make. What it cannot reach is not
// a policy it enforces — the container's --internal network and the proxy's
// one-destination allowlist enforce that — but it records every attempt and
// redirect the boundary blocked, so the report shows the edge of the test.
//
// The binary reads a JSON config from a mounted file, runs the checks within
// conservative limits, and writes an ao.pentest/v1 report to stdout. It holds
// no AO credentials and takes nothing from the daemon's environment.
package webdast

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Target is the one authorized destination, canonical.
type Target struct {
	Scheme string `json:"scheme"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
}

// BaseURL renders scheme://host:port with no trailing slash.
func (t Target) BaseURL() string {
	return fmt.Sprintf("%s://%s:%d", t.Scheme, t.Host, t.Port)
}

// Origin renders the RFC 6454 origin (scheme://host:port), used for CORS checks.
func (t Target) Origin() string { return t.BaseURL() }

// Limits are the conservative, configurable bounds on a run. The checker
// enforces every one of them in-process; the wall clock is ALSO enforced by
// the container's cgroup, so a wedged check cannot outlive the run.
type Limits struct {
	MaxRequests       int   `json:"maxRequests"`
	RequestsPerSecond int   `json:"requestsPerSecond"`
	Concurrency       int   `json:"concurrency"`
	MaxDurationMs     int   `json:"maxDurationMs"`
	MaxResponseBytes  int64 `json:"maxResponseBytes"`
	MaxRedirects      int   `json:"maxRedirects"`
	MaxEndpoints      int   `json:"maxEndpoints"`
}

// DefaultLimits are deliberately small: the goal is to detect misconfiguration,
// never to load or stress a target. A run that needs more must say so and be
// authorized for it.
func DefaultLimits() Limits {
	return Limits{
		MaxRequests:       500,
		RequestsPerSecond: 5,
		Concurrency:       4,
		MaxDurationMs:     120000,
		MaxResponseBytes:  2 << 20, // 2 MiB
		MaxRedirects:      0,       // the proxy does not follow; nor does the checker
		MaxEndpoints:      100,
	}
}

// Normalize fills any unset field with its conservative default and clamps
// anything a caller set beyond the safe ceiling back down to it. A config can
// only ever be as aggressive as the defaults, never more: this is the last line
// that keeps active-pentest from being turned into a load generator.
func (l Limits) Normalize() Limits {
	d := DefaultLimits()
	out := l
	if out.MaxRequests <= 0 || out.MaxRequests > d.MaxRequests {
		out.MaxRequests = d.MaxRequests
	}
	if out.RequestsPerSecond <= 0 || out.RequestsPerSecond > d.RequestsPerSecond {
		out.RequestsPerSecond = d.RequestsPerSecond
	}
	if out.Concurrency <= 0 || out.Concurrency > d.Concurrency {
		out.Concurrency = d.Concurrency
	}
	if out.MaxDurationMs <= 0 || out.MaxDurationMs > d.MaxDurationMs {
		out.MaxDurationMs = d.MaxDurationMs
	}
	if out.MaxResponseBytes <= 0 || out.MaxResponseBytes > d.MaxResponseBytes {
		out.MaxResponseBytes = d.MaxResponseBytes
	}
	// MaxRedirects is a hard 0 in 2F: neither the proxy nor the checker follows
	// a redirect, so a non-zero value is meaningless and is clamped away.
	out.MaxRedirects = 0
	if out.MaxEndpoints <= 0 || out.MaxEndpoints > d.MaxEndpoints {
		out.MaxEndpoints = d.MaxEndpoints
	}
	return out
}

// Duration is the wall-clock budget as a time.Duration.
func (l Limits) Duration() time.Duration { return time.Duration(l.MaxDurationMs) * time.Millisecond }

// Config is the whole input to one run, read from a mounted JSON file.
type Config struct {
	// SchemaVersion pins the config shape; the checker refuses an unknown one.
	SchemaVersion string `json:"schemaVersion"`
	// ProjectID, SkillID, SkillVersion, RunID, AuthorizationID and provenance
	// are copied into the report so it is self-describing. The checker does not
	// authorize anything — the daemon did that — it only records what it was
	// told it is running under.
	ProjectID       string   `json:"projectId"`
	SkillID         string   `json:"skillId"`
	SkillVersion    string   `json:"skillVersion"`
	RunID           string   `json:"runId"`
	AuthorizationID string   `json:"authorizationId"`
	Target          Target   `json:"target"`
	ScopePaths      []string `json:"scopePaths"`
	Limits          Limits   `json:"limits"`
	// RequestedBy and AuthorizationRef go into the report's provenance.
	RequestedBy      string `json:"requestedBy"`
	AuthorizationRef string `json:"authorizationRef"`
	// Tool identifies this checker for the report.
	ToolName    string `json:"toolName"`
	ToolVersion string `json:"toolVersion"`
}

// ConfigSchemaVersion is the config file's own version.
const ConfigSchemaVersion = "ao.webdast-config/v1"

// Validate refuses a config the checker cannot safely run.
func (c Config) Validate() error {
	if c.SchemaVersion != ConfigSchemaVersion {
		return fmt.Errorf("webdast: unsupported config schemaVersion %q", c.SchemaVersion)
	}
	if c.Target.Scheme != "http" && c.Target.Scheme != "https" {
		return fmt.Errorf("webdast: target scheme must be http or https")
	}
	if strings.TrimSpace(c.Target.Host) == "" {
		return fmt.Errorf("webdast: target host is required")
	}
	if c.Target.Port < 1 || c.Target.Port > 65535 {
		return fmt.Errorf("webdast: target port out of range")
	}
	if strings.TrimSpace(c.RunID) == "" {
		return fmt.Errorf("webdast: runId is required")
	}
	if strings.TrimSpace(c.AuthorizationID) == "" {
		return fmt.Errorf("webdast: authorizationId is required")
	}
	// Every scope path is a URL path, rooted, no scheme/host: the checker never
	// leaves the authorized target, so a scope entry that looked like a URL
	// would be a mistake worth refusing.
	for _, p := range c.ScopePaths {
		if strings.Contains(p, "://") {
			return fmt.Errorf("webdast: scope path %q must be a path, not a URL", p)
		}
	}
	return nil
}

// endpointPaths returns the paths to probe: the configured scope paths, or a
// small fixed set of safe, conventional paths when none is given. Bounded by
// MaxEndpoints.
func (c Config) endpointPaths(limits Limits) []string {
	paths := c.ScopePaths
	if len(paths) == 0 {
		paths = defaultProbePaths
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = normalizePath(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= limits.MaxEndpoints {
			break
		}
	}
	return out
}

// defaultProbePaths is a tiny, conventional set. Every one is a GET a browser
// or a monitoring probe would make; none is an attack.
var defaultProbePaths = []string{"/"}

// sensitivePaths are conventional locations that must never be world-readable.
// The checker requests them and reports only the STATUS and byte length — it
// never stores or echoes their contents, so a real leak is proven by "200, N
// bytes" and confirmed by a human, not exfiltrated into the report.
var sensitivePaths = []string{
	"/.git/config", "/.git/HEAD", "/.env", "/.env.local",
	"/.aws/credentials", "/config.json", "/wp-config.php.bak", "/.DS_Store",
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// resolve builds an absolute URL for a path against the target, refusing any
// path that would change host or scheme.
func (c Config) resolve(p string) (string, error) {
	u, err := url.Parse(c.Target.BaseURL())
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(p)
	if err != nil {
		return "", err
	}
	if ref.Scheme != "" || ref.Host != "" {
		return "", fmt.Errorf("webdast: refusing off-target path %q", p)
	}
	return u.ResolveReference(&url.URL{Path: ref.Path, RawQuery: ref.RawQuery}).String(), nil
}
