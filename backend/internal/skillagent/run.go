package skillagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
)

// Request is one agent run. Every field comes from something the service
// resolved and verified: the instructions are the bytes of a builtin or
// trusted package, the scope is the manifest's, the schema is the canonical
// findings contract.
type Request struct {
	RunID        string
	ProjectID    string
	ProjectPath  string
	SkillID      string
	SkillVersion string
	ModeID       string
	// ScopePaths are repo-relative paths to stage; empty is the whole checkout.
	ScopePaths []string
	// DenyGlobs are the manifest's scope.files.deny patterns.
	DenyGlobs []string
	// Instructions is SKILL.md; ModeGuide is modes/<mode>.md.
	Instructions string
	ModeGuide    string
	// Schema is the JSON Schema the structured output must satisfy.
	Schema []byte
}

// Result is what a run produced. Output is UNTRUSTED: the service validates
// and redacts it before anything is stored.
type Result struct {
	Output    []byte
	Staging   skillrunner.Staging
	StartedAt time.Time
	EndedAt   time.Time
	// Literals are the credential-shaped values AO harvested from the staged
	// files, for redaction. Sensitive: memory only, never logged or stored.
	Literals []string
	Evidence Evidence
}

// Evidence is what AO observed about the run, safe to store and render.
type Evidence struct {
	BinaryPath string `json:"binaryPath"`
	Model      string `json:"model"`
	DurationMS int64  `json:"durationMs"`
	NumTurns   int    `json:"numTurns,omitempty"`
	// PermissionDenials counts tool calls the CLI refused -- a read outside
	// the working directory, a tool that was not offered. A non-zero count on
	// a run over hostile content is the confinement doing its job, and is
	// reported rather than hidden. DeniedTools names the tools, never inputs.
	PermissionDenials int      `json:"permissionDenials"`
	DeniedTools       []string `json:"deniedTools,omitempty"`
	InputTokens       int64    `json:"inputTokens,omitempty"`
	OutputTokens      int64    `json:"outputTokens,omitempty"`
	CostUSD           float64  `json:"costUsd,omitempty"`
	AuthMode          string   `json:"authMode,omitempty"`
}

// agentConfigNames are files and directories that configure an agent CLI. They
// are never staged: safe mode already refuses to load them, and not handing
// them over at all means that refusal is not the only thing standing between a
// repository and the agent's configuration.
var agentConfigNames = []string{".claude/", ".mcp.json", ".codex/", ".cursor/", ".gemini/"}

// Run stages the scope, runs the agent over it, and verifies nothing it could
// reach was changed. It returns the raw structured output for the service to
// validate. The staged copy is removed before Run returns, whatever happens.
func (e *Executor) Run(ctx context.Context, req Request) (Result, error) {
	if e.unavailable != "" || len(e.controls) == 0 {
		return Result{}, fmt.Errorf("%w: %s", ErrUnavailable, e.unavailable)
	}
	if !skillrunner.ValidRunID(req.RunID) {
		return Result{}, fmt.Errorf("%w: run id %q is not valid", skillrunner.ErrStagingUnusable, req.RunID)
	}
	deny := skillrunner.CompileDenyGlobs(req.DenyGlobs)
	staging, err := skillrunner.Stage(skillrunner.StageRequest{
		SourceDir:      req.ProjectPath,
		ScopePaths:     req.ScopePaths,
		Root:           e.cfg.StagingRoot,
		RunID:          req.RunID,
		MaxFiles:       e.cfg.MaxFiles,
		MaxFileBytes:   e.cfg.MaxFileBytes,
		AddressAnyName: true,
		Exclude: func(rel string) string {
			for _, name := range agentConfigNames {
				if rel == strings.TrimSuffix(name, "/") || (strings.HasSuffix(name, "/") &&
					(strings.HasPrefix(rel, name) || strings.Contains(rel, "/"+name))) {
					return "agent-config"
				}
			}
			if deny.Match(rel) {
				return skillrunner.SkipReasonDenied
			}
			return ""
		},
	})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = removeStaging(staging) }()

	literals, err := skillreport.HarvestLiterals(staging.Dir, e.cfg.MaxFileBytes)
	if err != nil {
		return Result{}, fmt.Errorf("%w: read staged files: %w", skillrunner.ErrStagingUnusable, err)
	}
	if err := makeReadOnly(staging.Dir); err != nil {
		return Result{}, fmt.Errorf("%w: make staging read-only: %w", skillrunner.ErrStagingUnusable, err)
	}

	env := scrubbedEnv(e.binaryPath, nil)
	auth, err := e.authContract(ctx, env)
	if err != nil {
		return Result{}, err
	}

	res := Result{Staging: staging, Literals: literals, StartedAt: time.Now().UTC()}
	res.Evidence = Evidence{BinaryPath: e.binaryPath, Model: e.cfg.Model, AuthMode: string(auth.Mode)}

	args := e.argv(req, staging)
	callCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()
	cmd := exec.Command(e.binaryPath, args...) //nolint:gosec // resolved provider CLI, argv authored here.
	cmd.Dir = staging.Dir
	cmd.Env = env
	cmd.Stdin = nil
	out, runErr := e.execute(callCtx, cmd, e.pidFile(req.RunID))
	res.EndedAt = time.Now().UTC()
	res.Evidence.DurationMS = res.EndedAt.Sub(res.StartedAt).Milliseconds()

	// Tamper verification runs on EVERY exit path that got this far, before
	// anything about the output is considered: a run that changed what it was
	// given has no output worth reading.
	if err := verifyStaging(staging); err != nil {
		return res, err
	}
	if err := verifySource(req.ProjectPath, staging); err != nil {
		return res, err
	}

	if runErr != nil {
		if ctx.Err() != nil {
			return res, fmt.Errorf("%w: interrupted: %w", ErrProvider, ctx.Err())
		}
		if callCtx.Err() != nil {
			return res, fmt.Errorf("%w: timed out after %s", ErrProvider, e.cfg.Timeout)
		}
		return res, fmt.Errorf("%w: %s", ErrProvider, describeFailure(out, runErr))
	}
	env2, err := parseEnvelope(out)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrNoStructuredOutput, err)
	}
	env2.fill(&res.Evidence)
	if env2.IsError {
		return res, fmt.Errorf("%w: the CLI reported %s (%s)", ErrProvider, env2.Subtype, env2.APIErrorStatus)
	}
	if len(bytes.TrimSpace(env2.StructuredOutput)) == 0 || string(bytes.TrimSpace(env2.StructuredOutput)) == "null" {
		return res, ErrNoStructuredOutput
	}
	res.Output = env2.StructuredOutput
	return res, nil
}

// argv is the whole command line. Nothing in it comes from the project; the
// instructions come from a verified package and the rest is AO's.
func (e *Executor) argv(req Request, staging skillrunner.Staging) []string {
	args := []string{
		"--print",
		"--output-format", "json",
		"--json-schema", cliSchema(req.Schema),
		// The ONLY tools that exist in this session.
		"--tools", "Read,Grep,Glob",
		"--allowedTools", "Read,Grep,Glob",
		"--disallowedTools", "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch,Task",
		// Nothing prompts; anything that would is denied. Never bypass.
		"--permission-mode", "dontAsk",
		"--permission-prompts", "none",
		// File tools confined to the working directory, code-running and web
		// tools removed, bypass refused, user/project/local settings ignored.
		"--restricted",
		// No CLAUDE.md, hooks, plugins, skills, MCP servers, custom agents.
		"--safe-mode",
		"--strict-mcp-config",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--model", e.cfg.Model,
		"--append-system-prompt", systemPrompt(req),
	}
	if e.cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(e.cfg.MaxBudgetUSD, 'f', 2, 64))
	}
	return append(args, taskPrompt(req, staging))
}

// cliSchema is the schema as the CLI can load it. Claude Code's validator does
// not know the 2020-12 meta-schema URI and refuses a schema that names it, so
// the top-level $schema and $id -- identifiers, not constraints -- are dropped.
// Every structural keyword is passed unchanged, and AO validates the output
// against the canonical file itself, not against this copy.
func cliSchema(raw []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return string(raw)
	}
	delete(m, "$schema")
	delete(m, "$id")
	b, err := json.Marshal(m)
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// pidFile is where a running agent's process id is kept, so a daemon that
// restarts after dying can find and stop an agent it no longer supervises.
func (e *Executor) pidFile(runID string) string {
	return filepath.Join(e.cfg.StagingRoot, runID+".pid")
}

func (e *Executor) execute(ctx context.Context, cmd *exec.Cmd, pidFile string) ([]byte, error) {
	if e.cfg.runCommand != nil {
		return e.cfg.runCommand(ctx, cmd)
	}
	return runGroup(ctx, cmd, pidFile)
}

// Reap stops an agent a previous daemon left running and removes its staged
// copy. Both are addressed by the run id alone.
func (e *Executor) Reap(runID string) (killed bool, removed string, err error) {
	if !skillrunner.ValidRunID(runID) || e.cfg.StagingRoot == "" {
		return false, "", nil
	}
	dir := filepath.Join(e.cfg.StagingRoot, runID)
	killed = reapPID(e.pidFile(runID), dir)
	if _, statErr := os.Lstat(dir); statErr == nil {
		if err := removeStaging(skillrunner.Staging{Dir: dir, Root: e.cfg.StagingRoot}); err != nil {
			return killed, "", err
		}
		removed = dir
	}
	return killed, removed, nil
}

// envelope is the print-mode JSON the CLI emits.
type envelope struct {
	StructuredOutput  json.RawMessage `json:"structured_output"`
	IsError           bool            `json:"is_error"`
	Subtype           string          `json:"subtype"`
	APIErrorStatus    string          `json:"api_error_status"`
	NumTurns          int             `json:"num_turns"`
	TotalCostUSD      float64         `json:"total_cost_usd"`
	PermissionDenials []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
	Usage *struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

func (v envelope) fill(ev *Evidence) {
	ev.NumTurns = v.NumTurns
	ev.CostUSD = v.TotalCostUSD
	ev.PermissionDenials = len(v.PermissionDenials)
	seen := map[string]bool{}
	for _, d := range v.PermissionDenials {
		if !seen[d.ToolName] {
			seen[d.ToolName] = true
			ev.DeniedTools = append(ev.DeniedTools, d.ToolName)
		}
	}
	if v.Usage != nil {
		ev.InputTokens = v.Usage.InputTokens + v.Usage.CacheCreationInputTokens + v.Usage.CacheReadInputTokens
		ev.OutputTokens = v.Usage.OutputTokens
	}
}

// parseEnvelope reads exactly one JSON object. Unlike the planner it does not
// fish an object out of surrounding text: an agent run over hostile content is
// the last place to guess which of several objects is the real one.
func parseEnvelope(b []byte) (envelope, error) {
	var v envelope
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(b)))
	if err := dec.Decode(&v); err != nil {
		return envelope{}, fmt.Errorf("the CLI output is not one JSON envelope: %w", err)
	}
	if dec.More() {
		return envelope{}, errors.New("the CLI printed more than one JSON value")
	}
	return v, nil
}

// describeFailure summarises a non-zero exit. It never echoes a JSON envelope
// (the output of an agent that read hostile content): only the CLI's verdict
// fields, or the first line of a non-JSON failure.
func describeFailure(out []byte, err error) string {
	var v envelope
	if json.Unmarshal(bytes.TrimSpace(out), &v) == nil && (v.Subtype != "" || v.APIErrorStatus != "") {
		return fmt.Sprintf("exit %v, CLI reported %s %s", exitCode(err), v.Subtype, v.APIErrorStatus)
	}
	// A CLI that fails before it reaches the model (a flag it rejects, a
	// schema it cannot load) says why on its first line, and that line is the
	// CLI's own text. Only the first line, bounded, is kept; the caller
	// redacts it before it is recorded like any other run message.
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if len(first) > 200 {
		first = first[:200] + "…"
	}
	if first == "" {
		return fmt.Sprintf("exit %v with no output", exitCode(err))
	}
	return fmt.Sprintf("exit %v: %s", exitCode(err), first)
}

func exitCode(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return strconv.Itoa(ee.ExitCode())
	}
	return err.Error()
}
