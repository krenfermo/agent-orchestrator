// Package skillagent executes a skill's AGENT mode on the host (Frente 2 / 2C,
// ADR 0010): a provider CLI reads the skill's own instructions and a staged,
// read-only copy of the project, and returns a structured report.
//
// # What this is not
//
// It is not a container and it attests no container control. The agent is a
// process of the daemon's user on the daemon's host. What bounds it is four
// narrower things, each of which this package demonstrates before it claims it
// and each of which is named as its own control (skillcatalog.hostAgentControls):
//
//   - staged_read_only_copy: the working directory is an AO-staged copy of the
//     in-scope files, with the manifest's deny list and AO's own agent-config
//     exclusions applied, and every file and directory made read-only. The
//     checkout itself is never the working directory.
//   - agent_tool_confinement: the CLI is launched with ONLY its read tools, in
//     restricted mode (file tools confined to the working directory, code
//     execution and web tools removed, bypass refused, user/project settings
//     ignored), safe mode (no CLAUDE.md, hooks, plugins, skills, MCP), no MCP
//     servers, no slash commands, permission prompts auto-denied. The flags
//     are checked against the binary AO actually resolved; a CLI that does not
//     know one of them attests nothing.
//   - scrubbed_environment: the subprocess gets an allowlist -- HOME, a fixed
//     PATH, locale, TMPDIR and the provider's OWN credential variables. None of
//     AO's variables, no forge token, no SSH agent, no cloud credential.
//   - tamper_detection: after the agent exits, the staged copy must hold exactly
//     the files AO wrote with exactly the bytes AO wrote, and every source file
//     they were copied from must still hash to what was staged. Anything else
//     and the output is not trusted.
//
// Only builtin or signature-trusted packages reach this executor: the service
// verifies that before authorization, and skillcatalog.Authorize refuses a host
// agent for any package it was not told is trusted.
//
// # Provider
//
// Claude Code is the implemented provider. Codex is deliberately not: its
// read-only sandbox forbids writes but not reads, so a Codex agent could read
// the daemon user's home, which is the one thing the staged copy exists to
// prevent. See ADR 0010.
package skillagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerauth"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// RunnerID names this executor in attestations and run records.
const RunnerID = "host-agent/claude-code"

// Tool is recorded as a run's tool, the way ao.static-scan/v1 is for the
// container path.
const Tool = "ao.skill-agent/v1"

// Errors. The service classifies them into refused versus failed runs.
var (
	// ErrUnavailable: the executor attests nothing (no CLI, a CLI without a
	// required flag, credentials that need a person, an unsupported OS).
	ErrUnavailable = errors.New("skillagent: host agent executor unavailable")
	// ErrStagingTampered: the staged copy changed while the agent ran.
	ErrStagingTampered = errors.New("skillagent: the staged copy was modified during the run")
	// ErrSourceChanged: a source file changed between staging and the end of
	// the run. AO cannot attribute that change, so it trusts no output.
	ErrSourceChanged = errors.New("skillagent: the project changed during the run")
	// ErrProvider: the provider CLI failed, errored or timed out.
	ErrProvider = errors.New("skillagent: the provider agent failed")
	// ErrNoStructuredOutput: the CLI succeeded and returned no structured
	// output. Free text is never a report.
	ErrNoStructuredOutput = errors.New("skillagent: the agent returned no structured output")
)

// requiredFlags are the CLI options the confinement depends on. Each one is
// load-bearing: without --restricted the file tools read outside the working
// directory (measured, see ADR 0010), without --safe-mode the staged
// CLAUDE.md and hooks load, and so on.
var requiredFlags = []string{
	"--print", "--output-format", "--json-schema", "--tools", "--permission-mode",
	"--permission-prompts", "--restricted", "--safe-mode", "--strict-mcp-config",
	"--no-session-persistence", "--disable-slash-commands", "--append-system-prompt",
}

// Config is the executor's fixed configuration.
type Config struct {
	// Binary is the CLI to run ("claude" by default).
	Binary string
	// Model is passed as --model ("sonnet" by default).
	Model string
	// Timeout bounds one agent run (15 minutes by default).
	Timeout time.Duration
	// MaxBudgetUSD, when positive, is passed as --max-budget-usd. It is the
	// cost hook: AO sets no budget of its own here.
	MaxBudgetUSD float64
	// StagingRoot is where per-run copies are made. It must be absolute and
	// contain ".ao-skill-staging" (skillrunner.Stage refuses anything else).
	StagingRoot string
	// AuthMode pins the credential mechanism (AO_PROVIDER_AUTH_MODE).
	AuthMode providerauth.Mode
	// ResolveFallback finds the CLI in its well-known install locations when
	// PATH does not have it -- the same resolver the planner and every agent
	// adapter use. The daemon wires claudecode.ResolveClaudeBinary.
	ResolveFallback func(ctx context.Context) (string, error)
	// MaxFiles and MaxFileBytes bound staging.
	MaxFiles     int
	MaxFileBytes int64

	// helpOutput and runCommand are test seams. Nil runs the real binary.
	helpOutput func(ctx context.Context, binary string) ([]byte, error)
	runCommand func(ctx context.Context, cmd *exec.Cmd) ([]byte, error)
}

// Executor is AO's host agent executor.
type Executor struct {
	cfg         Config
	binaryPath  string
	unavailable string
	controls    []skillcatalog.Control
}

// New resolves the CLI and verifies it supports every flag the confinement
// needs. It never fails: an executor that cannot run attests nothing and says
// why, which is the shape a fail-closed component needs.
func New(ctx context.Context, cfg Config) *Executor {
	if cfg.Binary == "" {
		cfg.Binary = "claude"
	}
	if cfg.Model == "" {
		cfg.Model = "sonnet"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Minute
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 5000
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = 512 * 1024
	}
	e := &Executor{cfg: cfg}
	if reason := e.probe(ctx); reason != "" {
		e.unavailable = reason
		return e
	}
	e.controls = []skillcatalog.Control{
		skillcatalog.ControlStagedReadOnlyCopy,
		skillcatalog.ControlAgentToolConfinement,
		skillcatalog.ControlScrubbedEnvironment,
		skillcatalog.ControlTamperDetection,
	}
	return e
}

func (e *Executor) probe(ctx context.Context) string {
	if !supportedOS() {
		return "host agent execution is implemented for macOS and Linux only"
	}
	root := e.cfg.StagingRoot
	if !filepath.IsAbs(root) || !strings.Contains(root, ".ao-skill-staging") {
		return fmt.Sprintf("staging root %q is not an absolute AO staging root", root)
	}
	path, err := resolveBinary(ctx, e.cfg.Binary, e.cfg.ResolveFallback)
	if err != nil {
		return err.Error()
	}
	e.binaryPath = path
	help := e.cfg.helpOutput
	if help == nil {
		help = realHelp
	}
	out, err := help(ctx, path)
	if err != nil {
		return fmt.Sprintf("could not read %s --help: %v", path, err)
	}
	text := string(out)
	for _, flag := range requiredFlags {
		if !strings.Contains(text, flag) {
			return fmt.Sprintf("%s does not support %s, which the host agent's confinement depends on; "+
				"upgrade Claude Code", path, flag)
		}
	}
	return ""
}

// Attestation is what this executor demonstrated: the four host-agent
// controls, or nothing.
func (e *Executor) Attestation() skillcatalog.RunnerAttestation {
	return skillcatalog.RunnerAttestation{
		RunnerID: RunnerID,
		Controls: append([]skillcatalog.Control(nil), e.controls...),
	}
}

// Unavailable explains why the executor attests nothing, or is empty.
func (e *Executor) Unavailable() string { return e.unavailable }

// Model is the model runs are launched with.
func (e *Executor) Model() string { return e.cfg.Model }

// StagingRoot is the root per-run copies live under.
func (e *Executor) StagingRoot() string { return e.cfg.StagingRoot }

// authContract resolves the provider credential for the SCRUBBED environment
// the subprocess will actually receive, and refuses one that would need a
// person (a keychain unlock dialog nobody is there to answer).
func (e *Executor) authContract(ctx context.Context, env []string) (providerauth.Contract, error) {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	c := providerauth.Probe(ctx, providerauth.Request{
		Harness: domain.HarnessClaudeCode, Env: m, Required: e.cfg.AuthMode,
	})
	if c.Status == providerauth.StatusRequiresInteraction {
		return c, fmt.Errorf("%w: provider credentials need a person: %s", ErrUnavailable, c.Reason)
	}
	return c, nil
}

func resolveBinary(ctx context.Context, binary string, fallback func(context.Context) (string, error)) (string, error) {
	if strings.ContainsRune(binary, os.PathSeparator) {
		if err := executable(binary); err != nil {
			return "", fmt.Errorf("%s: %w", binary, err)
		}
		return binary, nil
	}
	if p, err := exec.LookPath(binary); err == nil {
		return p, nil
	}
	if fallback != nil {
		if p, err := fallback(ctx); err == nil && p != "" && executable(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%q is not installed where the daemon can find it", binary)
}

func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("not an executable file")
	}
	return nil
}

func realHelp(ctx context.Context, binary string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--help") //nolint:gosec // the resolved provider CLI, fixed argv.
	cmd.Env = scrubbedEnv(binary, nil)
	return cmd.CombinedOutput()
}

// envAllowlist are the only daemon variables an agent inherits. The provider's
// credential variables are here because the agent must authenticate to its own
// provider; nothing of AO's, no forge token, no SSH agent socket and no cloud
// credential is.
var envAllowlist = []string{
	"HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// scrubbedEnv builds the subprocess environment from the allowlist. source is
// the environment to read from (nil: the daemon's own).
func scrubbedEnv(binary string, source []string) []string {
	if source == nil {
		source = os.Environ()
	}
	allowed := map[string]bool{}
	for _, k := range envAllowlist {
		allowed[k] = true
	}
	out := []string{
		// A fixed PATH plus the CLI's own directory: the agent has no shell
		// tool, and the CLI needs only system utilities (the keychain reader
		// lives in /usr/bin on macOS).
		"PATH=" + filepath.Dir(binary) + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
	for _, kv := range source {
		k, _, ok := strings.Cut(kv, "=")
		if ok && allowed[k] {
			out = append(out, kv)
		}
	}
	return out
}
