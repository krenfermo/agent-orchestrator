package practical3d

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SandboxProfileVersion versions the Seatbelt profile below.
const SandboxProfileVersion = "ao.3d-practical.agent-sandbox.v1"

// agentSandboxProfile confines every AO-launched agent process of a position
// (worker, reviewer, fix and every child: hooks, shells, tools). Ported from
// the validated lab3d agent.sb, tightened for Practical: ALL network is
// denied except the position's provider proxy and AO daemon on loopback, so
// a provider call can only exist through the observed proxy. SBPL: the LAST
// matching rule wins.
const agentSandboxProfile = `(version 1)
(allow default)

; Nothing under ~/.ao except this position's working surfaces: production
; data, the experiment ledger/registry/envelope/artifacts, hidden oracles and
; every other position stay out of reach. stat() of ancestors stays allowed
; so path resolution works; listing and content do not.
(deny file-read* file-write* (subpath (param "AO_HOME")))
(allow file-read-metadata (subpath (param "AO_HOME")))
(allow file-read* file-write* (subpath (param "POS_WORK")))
(allow file-read* file-write* (subpath (param "POS_WORKTREES")))
(allow file-read* file-write* (subpath (param "POS_HOME")))
(allow file-read* file-write* (subpath (param "POS_TMP")))
(allow file-read* (literal (param "POS_RUN_FILE")))
(allow file-read* (subpath (param "POS_PROMPTS")))
(allow file-read* (subpath (param "POS_HOOKBIN")))
(allow file-read* (subpath (param "TOOLS_RO")))

; AO source trees (they document the experiment) and the supervisor's
; private control directory.
(deny file-read* file-write* (subpath (param "AO_SRC")))
(deny file-read* file-write* (subpath (param "PRIVATE_CTL")))
; The Q4 oracle (script and hidden tests), wherever the operator keeps it.
(deny file-read* file-write* (subpath (param "ORACLE_DIR")))

; The operator's provider state (transcripts, history, caches of other work).
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/.claude")))
(deny file-read* file-write* (prefix (string-append (param "REAL_HOME") "/.claude.json")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/.cache/claude")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/.codex")))
; Codex authenticates with the operator's ChatGPT login (read-only); with all
; network but the proxy denied, every use of it is an observed attempt.
(allow file-read* (literal (string-append (param "REAL_HOME") "/.codex/auth.json")))
(allow file-read-metadata (literal (string-append (param "REAL_HOME") "/.codex")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/Library/Caches/claude-cli-nodejs")))
(deny file-read* file-write* (subpath (string-append (param "REAL_HOME") "/Library/Application Support/Claude")))

; Network: nothing leaves the host and no local service is reachable except
; the observed provider proxy and this position's AO daemon. Unix sockets
; are denied except those an agent creates in its own TMPDIR.
(deny network-outbound)
(allow network-outbound (remote unix-socket (subpath (param "POS_TMP"))))
(allow network-outbound (remote ip (string-append "localhost:" (param "PROXY_PORT"))))
(allow network-outbound (remote ip (string-append "localhost:" (param "DAEMON_PORT"))))
(deny network-inbound (local ip "*:*"))

; No escape by spawning outside the sandbox. (Re-sandboxing is allowed: a
; nested Seatbelt profile can only restrict further; Codex applies its own.)
; (Claude Code
; reads its OAuth credential through /usr/bin/security, so the keychain CLI
; stays allowed; with every network destination but the proxy denied, any
; use of that credential is still an observed provider attempt.)
(deny process-exec (literal "/bin/launchctl") (literal "/usr/bin/open") (literal "/usr/bin/osascript")
                   (literal "/usr/bin/tmux") (literal "/opt/homebrew/bin/tmux")
                   (subpath "/opt/homebrew/Cellar/tmux") (subpath "/opt/homebrew/opt/tmux"))
(deny mach-lookup (global-name "com.apple.coreservices.launchservicesd"))
`

// SandboxParams are the concrete paths/ports of one position.
type SandboxParams struct {
	AOHome, RealHome, AOSrc, PrivateCtl, ToolsRO, OracleDir                    string
	PosWork, PosWorktrees, PosHome, PosTmp, PosRunFile, PosPrompts, PosHookBin string
	ProxyPort, DaemonPort                                                      int
}

func (p SandboxParams) args() ([]string, error) {
	vals := map[string]string{
		"AO_HOME": p.AOHome, "REAL_HOME": p.RealHome, "AO_SRC": p.AOSrc, "PRIVATE_CTL": p.PrivateCtl, "TOOLS_RO": p.ToolsRO, "ORACLE_DIR": p.OracleDir,
		"POS_WORK": p.PosWork, "POS_WORKTREES": p.PosWorktrees, "POS_HOME": p.PosHome, "POS_TMP": p.PosTmp, "POS_RUN_FILE": p.PosRunFile, "POS_PROMPTS": p.PosPrompts, "POS_HOOKBIN": p.PosHookBin,
		"PROXY_PORT": fmt.Sprint(p.ProxyPort), "DAEMON_PORT": fmt.Sprint(p.DaemonPort),
	}
	keys := make([]string, 0, len(vals))
	for k, v := range vals {
		if strings.TrimSpace(v) == "" || v == "0" {
			return nil, fmt.Errorf("sandbox parameter %s is empty", k)
		}
		if k != "PROXY_PORT" && k != "DAEMON_PORT" {
			// Seatbelt matches resolved paths: /tmp is /private/tmp, etc.
			if !filepath.IsAbs(v) {
				return nil, fmt.Errorf("sandbox parameter %s is not absolute", k)
			}
			vals[k] = resolveOrSelf(v)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []string{}
	for _, k := range keys {
		out = append(out, "-D", k+"="+vals[k])
	}
	return out, nil
}

// SandboxCommand returns the argv that runs argv under the position profile.
func SandboxCommand(profilePath string, p SandboxParams, argv []string) ([]string, error) {
	params, err := p.args()
	if err != nil {
		return nil, err
	}
	out := append([]string{"/usr/bin/sandbox-exec", "-f", profilePath}, params...)
	return append(out, argv...), nil
}

// WriteSandboxProfile writes the profile read-only into dir.
func WriteSandboxProfile(dir string) (string, error) {
	path := filepath.Join(dir, "agent.sb")
	return path, writeExclusive(path, []byte(agentSandboxProfile))
}

// ShimConfig is the trusted launch configuration the daemon's `claude` shim
// reads (before confinement) from the supervisor's private directory.
type ShimConfig struct {
	RealClaude    string            `json:"real_claude"`
	Profile       string            `json:"profile"`
	Sandbox       SandboxParams     `json:"sandbox"`
	ControlSocket string            `json:"control_socket"`
	ExtraEnv      map[string]string `json:"extra_env"`
	ClaudeArgs    []string          `json:"claude_args"`
	LaunchLog     string            `json:"launch_log"`
	// Capture, when set, makes every model-calling launch record its argv
	// there and exit without contacting any provider (calibration).
	Capture string `json:"capture,omitempty"`
	// RealCodex and CodexArgs configure the Codex launch; "{BASE}" in an
	// argument is replaced by the position proxy's tokenized Responses URL.
	RealCodex string   `json:"real_codex,omitempty"`
	CodexArgs []string `json:"codex_args,omitempty"`
}

// shimKeepEnv are the only inherited variables an agent receives (plus AO_*
// control-plane variables other than the arm switches, which are dropped).
var shimKeepEnv = []string{"USER", "LOGNAME", "PATH", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM"}

// ShimSubject derives the AO usage subject of a launch from the variables AO's
// control plane sets on the pane; a launch without one is not attributable.
func ShimSubject(env func(string) string) (string, error) {
	if s := strings.TrimSpace(env("AO_USAGE_SUBJECT")); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(env("AO_SESSION_ID")); s != "" {
		return "session:" + s, nil
	}
	return "", errors.New("launch carries no AO usage subject")
}

// nonSessionClaudeArgs are CLI subcommands/flags that never call a model.
var nonSessionClaudeArgs = map[string]bool{"--version": true, "-v": true, "-h": true, "--help": true, "auth": true, "doctor": true, "config": true, "mcp": true, "update": true, "install": true}

// nonSessionCodexArgs are Codex subcommands/flags that never call a model.
var nonSessionCodexArgs = map[string]bool{"--version": true, "-V": true, "-h": true, "--help": true, "login": true, "logout": true, "completion": true, "features": true}

// BuildShimLaunch computes the confined argv and the complete environment of
// one agent launch. Model-calling launches get a proxy token bound to their
// AO subject; others get an unusable provider endpoint.
func BuildShimLaunch(cfg ShimConfig, args, environ []string, token func(subject string) (string, error)) ([]string, []string, error) {
	return buildShimLaunch("claude", cfg, args, environ, token)
}

func buildShimLaunch(harness string, cfg ShimConfig, args, environ []string, token func(subject string) (string, error)) ([]string, []string, error) {
	get := envLookup(environ)
	env := []string{}
	for _, k := range shimKeepEnv {
		if v, ok := get(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "AO_") || strings.HasPrefix(k, "AO_MEMORY") || strings.HasPrefix(k, "AO_CONTEXT_ROUTER") || strings.HasPrefix(k, "AO_PROJECT_MEMORY") || k == "AO_3DP_SHIM_CONFIG" {
			continue
		}
		env = append(env, k+"="+v)
	}
	base := "http://127.0.0.1:1/unreachable"
	session := len(args) == 0 || !nonSessionClaudeArgs[args[0]]
	if harness == "codex" {
		session = len(args) == 0 || !nonSessionCodexArgs[args[0]]
	}
	if session {
		subject, err := ShimSubject(func(k string) string { v, _ := get(k); return v })
		if err != nil {
			return nil, nil, err
		}
		tok, err := token(subject)
		if err != nil {
			return nil, nil, fmt.Errorf("proxy token: %w", err)
		}
		base = fmt.Sprintf("http://127.0.0.1:%d/t/%s", cfg.Sandbox.ProxyPort, tok)
	}
	forced := map[string]string{
		"HOME": cfg.Sandbox.PosHome, "TMPDIR": cfg.Sandbox.PosTmp + "/", "ANTHROPIC_BASE_URL": base,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1", "DISABLE_TELEMETRY": "1",
		"DISABLE_ERROR_REPORTING": "1", "ENABLE_CLAUDEAI_MCP_SERVERS": "0", "CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL": "1",
		"GOTOOLCHAIN": "local",
	}
	for k, v := range cfg.ExtraEnv {
		forced[k] = v
	}
	keys := make([]string, 0, len(forced))
	for k := range forced {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := forced[k]; !ok {
			out = append(out, kv)
		}
	}
	for _, k := range keys {
		out = append(out, k+"="+forced[k])
	}
	binary, extra := cfg.RealClaude, cfg.ClaudeArgs
	if harness == "codex" {
		binary, extra = cfg.RealCodex, nil
		forced["CODEX_HOME"] = cfg.Sandbox.PosHome + "/codex-home"
		out = append(out, "CODEX_HOME="+forced["CODEX_HOME"])
		for _, a := range cfg.CodexArgs {
			extra = append(extra, strings.ReplaceAll(a, "{BASE}", strings.TrimSuffix(base, "/")+"/backend-api/codex"))
		}
	}
	finalArgs := args
	if session || harness == "codex" {
		finalArgs = append(append([]string{}, extra...), args...)
	}
	if binary == "" {
		return nil, nil, fmt.Errorf("no %s executable configured", harness)
	}
	argv, err := SandboxCommand(cfg.Profile, cfg.Sandbox, append([]string{binary}, finalArgs...))
	if err != nil {
		return nil, nil, err
	}
	return argv, out, nil
}

func envLookup(environ []string) func(string) (string, bool) {
	m := map[string]string{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			m[k] = v
		}
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// RequestProxyToken asks the supervisor's control socket for a token.
func RequestProxyToken(socket, subject string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	body, _ := json.Marshal(map[string]string{"subject": subject})
	resp, err := client.Post("http://ctl/token", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("control socket: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Token == "" {
		return "", errors.New("control socket returned no token")
	}
	return out.Token, nil
}

// ReadShimConfig loads the shim configuration.
func ReadShimConfig(path string) (ShimConfig, error) {
	var c ShimConfig
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	err = strictUnmarshal(raw, &c)
	return c, err
}

// ShimEnvConfig names the daemon-environment variable holding the shim
// configuration path; the shim never passes it to an agent.
const ShimEnvConfig = "AO_3DP_SHIM_CONFIG"

// ShimCapture is one captured launch (calibration only).
type ShimCapture struct {
	Subject string   `json:"subject"`
	Args    []string `json:"args"`
}

// RunShim is the entry point when the harness binary is invoked as `claude`
// or `codex` from a position daemon's PATH. Codex is not part of
// 3D-PRACTICAL and always fails. A Claude launch is confined and pointed at
// the position proxy, or captured in calibration mode. It returns an exit
// status only on failure; on success the process is replaced.
func RunShim(name string, args []string, stderr io.Writer, execFn func(string, []string, []string) error) int {
	if name != "claude" && name != "codex" {
		_, _ = fmt.Fprintf(stderr, "3d-practical: %s is not an allowed agent harness\n", name)
		return 1
	}
	cfg, err := ReadShimConfig(os.Getenv(ShimEnvConfig))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "3d-practical shim: %v\n", err)
		return 1
	}
	session := len(args) == 0 || !nonSessionClaudeArgs[args[0]]
	if name == "codex" {
		session = len(args) == 0 || !nonSessionCodexArgs[args[0]]
	}
	subject, _ := ShimSubject(os.Getenv)
	appendLaunch(cfg.LaunchLog, subject, args)
	if cfg.Capture != "" && session {
		if err := os.MkdirAll(cfg.Capture, 0o700); err == nil {
			raw, _ := json.Marshal(ShimCapture{Subject: subject, Args: args})
			_ = writeExclusive(filepath.Join(cfg.Capture, fmt.Sprintf("%d-%d.json", time.Now().UnixNano(), os.Getpid())), raw)
		}
		return 0
	}
	argv, env, err := buildShimLaunch(name, cfg, args, os.Environ(), func(subject string) (string, error) {
		return RequestProxyToken(cfg.ControlSocket, subject)
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "3d-practical shim: %v\n", err)
		return 1
	}
	// AO probes the CLI from its own working directory (its data dir, which
	// the sandbox denies); a confined process must start somewhere it may read.
	if cwd, err := os.Getwd(); err != nil || !cwdAllowed(cfg.Sandbox, cwd) {
		if err := os.Chdir(cfg.Sandbox.PosTmp); err != nil {
			_, _ = fmt.Fprintf(stderr, "3d-practical shim: chdir: %v\n", err)
			return 1
		}
	}
	if err := execFn(argv[0], argv, env); err != nil {
		_, _ = fmt.Fprintf(stderr, "3d-practical shim: exec: %v\n", err)
		return 1
	}
	return 0
}

// cwdAllowed reports whether a working directory lies in a position area the
// sandbox lets an agent read.
func cwdAllowed(p SandboxParams, cwd string) bool {
	c := resolveOrSelf(cwd)
	for _, base := range []string{p.PosWork, p.PosWorktrees, p.PosHome, p.PosTmp} {
		if within(c, resolveOrSelf(base)) {
			return true
		}
	}
	return false
}

func appendLaunch(path, subject string, args []string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	raw, _ := json.Marshal(map[string]any{"subject": subject, "first_arg": first, "argc": len(args), "at": time.Now().UTC()})
	_, _ = f.Write(append(raw, '\n'))
}
