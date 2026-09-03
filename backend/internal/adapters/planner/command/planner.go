package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// defaultTimeout is the floor every planner call gets regardless of prompt
// size -- unchanged from the pre-8P-E.10 fixed timeout, so a small objective
// times out no later than it always has.
const defaultTimeout = 3 * time.Minute

// defaultMaxTimeout bounds how far scaledTimeout will stretch the deadline
// for a large prompt+context payload (Checkpoint 8P-E.10). This is a bound,
// not a blind global bump: small objectives still get defaultTimeout.
const defaultMaxTimeout = 12 * time.Minute

// bytesPerExtraMinute is the input half of the timeout budget: every full
// multiple of this many bytes of prompt+context payload earns the call one
// more minute, up to MaxTimeout. Chosen conservatively -- MEDUSA-sized
// prompts (tens of KB of objective text plus repository context) land well
// inside a handful of extra minutes rather than saturating the cap on
// ordinary objectives.
const bytesPerExtraMinute = 40 * 1024

// perStepAllowance is the OUTPUT half of the timeout budget: how much time a
// single planned step is expected to cost the provider to produce.
//
// Input size alone was the wrong knob, and wf-80dc9f12 is why. That run's
// planner call sent an 8 KB payload -- far below one bytesPerExtraMinute --
// and so ran on the flat 3-minute floor, yet the identical invocation
// reproduced outside AO took 5m36s: six provider turns and ~40k output tokens
// spent writing a 12-step plan with acceptance criteria and structured verify
// blocks for each step. What made the call slow was what it had to WRITE, and
// the request already declares that ceiling in MaxSteps. The measured run
// averaged ~28s per emitted step, so 30s per step is a calibrated allowance
// rather than a guess -- and it is still an allowance, not a floor: a planner
// that finishes in six seconds returns in six seconds.
const perStepAllowance = 30 * time.Second

// maxParseRetries is how many additional attempts (beyond the first) the
// adapter makes when the planner subprocess itself succeeds (no timeout, no
// exec error) but its stdout could not be turned into a plan envelope. A
// single retry is enough to absorb a one-off flake (e.g. a transient stray
// line mixed into stdout) without masking a genuinely broken invocation --
// see Generate's retry loop for why timeouts and command failures are never
// retried here.
const maxParseRetries = 1

const parseRetryBackoff = 500 * time.Millisecond

// outputSnippetLimit bounds how much of a failed subprocess's raw output is
// embedded in the returned error. Enough to carry a provider's plain-text
// error message (rate-limit/auth banners are short) without ever logging a
// full malformed plan body.
const outputSnippetLimit = 500

// Planner runs an external command-line agent to produce a plan. Binary and
// Model identify the provider invocation; the zero value is not usable, since
// Generate refuses an empty Binary.
type Planner struct {
	Binary  string
	Model   string
	Timeout time.Duration
	// MaxTimeout bounds scaledTimeout's expansion for large payloads. Zero
	// (the common case, set once in wiring) defaults to defaultMaxTimeout.
	MaxTimeout time.Duration

	// Logger receives one structured line per attempt carrying the budget it
	// was given, the payload sizes it sent and how it ended -- the evidence
	// wf-80dc9f12 had no way to leave behind. Optional: nil logs nothing, and
	// the same evidence still rides back on the error either way.
	Logger *slog.Logger

	// runCommand executes the planner subprocess and returns its combined
	// stdout+stderr. Nil (the production default) runs the real binary via
	// exec.CommandContext; tests inject a fake to exercise timeout scaling,
	// envelope extraction, and retry behavior without a real CLI.
	runCommand func(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error)
}

// logAttempt emits one line per attempt at the level its outcome deserves.
func (p Planner) logAttempt(evidence workflowcore.PlannerAttemptEvidence, err error) {
	if p.Logger == nil {
		return
	}
	if err == nil {
		p.Logger.Info("planner attempt completed", evidence.LogArgs()...)
		return
	}
	p.Logger.Warn("planner attempt failed", append(evidence.LogArgs(), "err", err)...)
}

// Descriptor returns the provider and model this planner will record on the
// plan it generates, so a run can be attributed without invoking anything.
func (p Planner) Descriptor() (string, string) {
	model := p.Model
	if model == "" {
		model = "sonnet"
	}
	return "anthropic", model
}

// scaledTimeout computes one attempt's bounded budget from the two things
// that actually drive a planner call's duration: how much it must read
// (payloadSize) and how much it must write (maxSteps). Checkpoint 8P-E.10
// added the first term because MEDUSA-class prompts were timing out at a flat
// 3 minutes; the bootstrap repair adds the second because wf-80dc9f12 showed a
// SMALL prompt timing out at that same flat 3 minutes while the provider was
// still writing its twelfth step.
//
// It stays a bound, not a blind global bump: base is unchanged, max is
// unchanged, and a small objective planned into a handful of steps still gets
// a budget close to the floor it always had.
func scaledTimeout(base, ceiling time.Duration, payloadSize, maxSteps int) time.Duration {
	if base <= 0 {
		base = defaultTimeout
	}
	if ceiling <= 0 {
		ceiling = defaultMaxTimeout
	}
	if ceiling < base {
		ceiling = base
	}
	t := base + time.Duration(payloadSize/bytesPerExtraMinute)*time.Minute
	if maxSteps > 0 {
		t += time.Duration(maxSteps) * perStepAllowance
	}
	if t > ceiling {
		t = ceiling
	}
	return t
}

// Generate invokes the planner binary for one request and parses its output
// into a PlannerResponse. It returns an error rather than a partial plan: a
// plan AO cannot fully parse is not a plan it may execute.
func (p Planner) Generate(ctx context.Context, req workflowcore.PlannerRequest) (workflowcore.PlannerResponse, error) {
	if p.Binary == "" {
		return workflowcore.PlannerResponse{}, fmt.Errorf("planner binary is required")
	}
	contextJSON, _ := json.Marshal(req.Context)
	prompt := fmt.Sprintf(`Act only as a software master planner. You cannot use tools, edit files, run commands, commit, push, or claim work is complete.
Decompose the objective into small, independently verifiable implementation units. Preserve a simple dependency DAG and no more than %d steps; for a small objective prefer 2-4 substantial steps and do not plan work already present in the repository context. Every step must require a durable code, test, or documentation change; never create a verification-only step, and instead include final checks in the last implementation step. Every step needs concrete acceptance criteria and safe structured verification checks. Verification commands are executable plus argument arrays, never shell snippets. Do not use shells, destructive commands, deployment tools, git mutation commands, absolute paths, or paths outside the workspace.
For each step also declare the repository scope you expect it to WRITE: "files" for the workspace-relative files you already know it must change, and "packages" for the directories it will work inside when the exact files are not yet knowable. Declare only what that step writes, never what it merely reads, and omit either list rather than guessing -- these drive conflict detection between steps, so an invented path serializes work that did not need serializing.
For each step also declare "writeIntent": "mutating" when the step is expected to change the workspace (the normal case), or "read_only" when the step's accepted outcome REQUIRES the workspace to be left unchanged -- a verification, inspection or audit step that only runs existing commands and reports what it found. Declare "read_only" only when an unchanged worktree is the success condition: AO enforces it by comparing the worktree against the state the worker was handed, so a step that turns out to need an edit will stop for a person instead of completing. When in doubt declare "mutating".
Two steps that write the same file are treated as a probable write conflict. If two steps genuinely can share a path safely, say so on one of them with "safeWriteOverlaps": [{"with": "<other step id>", "paths": [...], "reason": "<why it is safe>"}]. Omit "paths" to waive the whole overlap with that step. Never waive an overlap you are not certain about: the reason is stored and reviewed, and an unmarked overlap staying a conflict is the safe outcome.

Objective: %s

Conservative repository context:
%s`, req.MaxSteps, req.Objective, string(contextJSON))
	schema := fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["version","objective","summary","steps"],"properties":{"version":{"const":"v1"},"objective":{"type":"string"},"summary":{"type":"string"},"steps":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","additionalProperties":false,"required":["id","title","description","dependencies","acceptanceCriteria","verify","writeIntent"],"properties":{"id":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},"dependencies":{"type":"array","items":{"type":"string"}},"acceptanceCriteria":{"type":"array","minItems":1,"items":{"type":"string"}},"writeIntent":{"enum":["mutating","read_only"]},"files":{"type":"array","items":{"type":"string"}},"packages":{"type":"array","items":{"type":"string"}},"safeWriteOverlaps":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["with","reason"],"properties":{"with":{"type":"string"},"paths":{"type":"array","items":{"type":"string"}},"reason":{"type":"string"}}}},"verify":{"type":"object","additionalProperties":false,"required":["commands","files"],"properties":{"commands":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["command","args","workingDirectory","timeoutSeconds","requiredExitCode","retrySafe"],"properties":{"command":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"workingDirectory":{"type":"string"},"timeoutSeconds":{"type":"integer","minimum":1,"maximum":3600},"requiredExitCode":{"type":"integer"},"retrySafe":{"type":"boolean"}}}},"files":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["path","exists"],"properties":{"path":{"type":"string"},"exists":{"type":"boolean"},"exactContent":{"type":"string"},"sha256":{"type":"string"}}}}}}}}}}}`, req.MaxSteps)
	model := p.Model
	if model == "" {
		model = "sonnet"
	}
	args := []string{"--print", "--output-format", "json", "--json-schema", schema, "--tools", "", "--permission-mode", "plan", "--no-session-persistence", "--model", model, prompt}
	env := mergeEnv(os.Environ(), req.RuntimeEnv)
	timeout := scaledTimeout(p.Timeout, p.MaxTimeout, len(prompt)+len(contextJSON), req.MaxSteps)
	// Shape of this attempt, recorded whatever happens to it. Sizes only --
	// never the prompt, the objective, a document body or an env value.
	shape := workflowcore.PlannerAttemptEvidence{
		CalculatedTimeoutMS: timeout.Milliseconds(),
		EffectiveTimeoutMS:  timeout.Milliseconds(),
		ObjectiveBytes:      len(req.Objective),
		ContextBytes:        len(contextJSON),
		PayloadBytes:        len(prompt) + len(contextJSON),
		DocumentCount:       len(req.Context.Documents),
		MaxSteps:            req.MaxSteps,
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		shape.HasParentDeadline = true
		shape.ParentDeadlineMS = remaining.Milliseconds()
		if remaining < timeout {
			shape.EffectiveTimeoutMS = remaining.Milliseconds()
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxParseRetries; attempt++ {
		if attempt > 0 {
			// Only reached for a parse-classified failure (see below) --
			// never for a timeout or a subprocess/exec error, both of which
			// return immediately. A fresh per-attempt deadline of the full
			// scaled timeout is deliberate: a flaky single-line stdout glitch
			// on attempt 1 says nothing about how long attempt 2 will take.
			select {
			case <-ctx.Done():
				return workflowcore.PlannerResponse{}, lastErr
			case <-time.After(parseRetryBackoff):
			}
		}
		plan, provider, respModel, evidence, err := p.attempt(ctx, args, req.Project.Path, env, timeout, model, shape)
		p.logAttempt(evidence, err)
		if err == nil {
			return workflowcore.PlannerResponse{
				Plan: plan, Provider: provider, Model: respModel, Evidence: evidence,
			}, nil
		}
		err = &workflowcore.PlannerAttemptError{Evidence: evidence, Err: err}
		// F2: a result that parsed but cannot be reconciled with the
		// invocation is retry-eligible for the same reason a malformed one is
		// -- it is a fact about this attempt, not a verdict about the
		// objective, and re-running the planner is the thing most likely to
		// recover the plan the provider actually produced.
		if !errors.Is(err, ports.ErrPlannerOutputMalformed) && !errors.Is(err, ports.ErrPlannerResultInconsistent) {
			// Timeout, missing binary, non-zero exit, etc. -- never worth
			// retrying blind: a slow provider stays slow, a missing binary
			// stays missing, and a real permission/capacity error should
			// surface to master_coordinator's classifier immediately rather
			// than being delayed behind a doomed retry.
			return workflowcore.PlannerResponse{}, err
		}
		lastErr = err
	}
	return workflowcore.PlannerResponse{}, lastErr
}

// attempt runs exactly one planner subprocess invocation and parses its
// output. Split out of Generate so the bounded retry loop above has a single
// place that decides whether an error is retry-eligible.
func (p Planner) attempt(ctx context.Context, args []string, dir string, env []string, timeout time.Duration, model string, shape workflowcore.PlannerAttemptEvidence) (workflowcore.MasterPlan, string, string, workflowcore.PlannerAttemptEvidence, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	run := p.runCommand
	if run == nil {
		run = runRealCommand
	}
	started := time.Now()
	b, err := run(callCtx, p.Binary, args, dir, env)
	evidence := shape
	evidence.DurationMS = time.Since(started).Milliseconds()
	// The zero MasterPlan is structural: fail mirrors attempt's own return
	// signature so callers can `return fail(...)`, and a failed attempt has no
	// plan by definition.
	//nolint:unparam // result 0 is always the zero plan; the signature must match attempt's.
	fail := func(class string, err error) (workflowcore.MasterPlan, string, string, workflowcore.PlannerAttemptEvidence, error) {
		evidence.Classification = class
		return workflowcore.MasterPlan{}, "", "", evidence, err
	}
	if err != nil {
		// A dead caller context and an expired planner budget both surface
		// here as "context deadline exceeded", and telling them apart is
		// exactly what wf-80dc9f12's postmortem could not do. The classifier
		// asks the PARENT first: if the caller's context is already done, the
		// planner's own budget was never what stopped this attempt. Both stay
		// ErrPlannerTimeout so master_coordinator keeps retrying rather than
		// permanently invalidating a plan over a daemon shutdown.
		if parentErr := ctx.Err(); parentErr != nil {
			return fail(workflowcore.PlannerAttemptParentCancelled, fmt.Errorf("planner interrupted by caller: %w: %w", ports.ErrPlannerTimeout, parentErr))
		}
		if callCtx.Err() != nil {
			return fail(workflowcore.PlannerAttemptTimeout, fmt.Errorf("planner timeout: %w: %w", ports.ErrPlannerTimeout, callCtx.Err()))
		}
		return fail(workflowcore.PlannerAttemptCommandFailed, fmt.Errorf("planner command: %w: %s", err, strings.TrimSpace(snippet(b))))
	}

	envelope, envErr := extractEnvelope(b)
	if envErr != nil {
		return fail(workflowcore.PlannerAttemptMalformed, fmt.Errorf("planner parse envelope: %w: %w: output=%q", ports.ErrPlannerOutputMalformed, envErr, snippet(b)))
	}
	raw := envelope.StructuredOutput
	evidence.ResultSource = "structured_output"
	if len(raw) == 0 && envelope.Result != "" {
		raw = []byte(envelope.Result)
		evidence.ResultSource = "result"
	}
	evidence.StructuredOutputBytes = len(envelope.StructuredOutput)
	evidence.ResultBytes = len(envelope.Result)
	evidence.PlanBytes = len(raw)
	if len(raw) == 0 {
		return fail(workflowcore.PlannerAttemptMalformed, fmt.Errorf("planner output missing structured plan: %w: output=%q", ports.ErrPlannerOutputMalformed, snippet(b)))
	}
	var plan workflowcore.MasterPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return fail(workflowcore.PlannerAttemptMalformed, fmt.Errorf("planner parse plan: %w: %w: output=%q", ports.ErrPlannerOutputMalformed, err, snippet(raw)))
	}
	// Usage is read BEFORE the consistency verdict, because the verdict needs
	// it and because a rejected attempt still spent every one of these tokens:
	// the caller meters both outcomes off this same evidence.
	if usage, usedModel, ok := plannerUsageFromEnvelope(envelope, model); ok {
		evidence.Usage = &usage
		evidence.UsageModel = usedModel
	}
	// F2: the plan parsed, which before this said nothing about whether it is
	// the plan the provider actually produced. A schema-shaped placeholder
	// parses perfectly. See consistency.go for what each signal is and why the
	// token-ratio one is last and weakest.
	var reportedOutputTokens int64
	if evidence.Usage != nil {
		reportedOutputTokens = evidence.Usage.OutputTokens
	}
	if reason := resultConsistency(envelope, len(raw), reportedOutputTokens); reason != "" {
		evidence.ConsistencySignal = reason
		return fail(workflowcore.PlannerAttemptResultInconsistent,
			fmt.Errorf("planner result inconsistent: %w: %s", ports.ErrPlannerResultInconsistent, reason))
	}
	evidence.Classification = workflowcore.PlannerAttemptOK
	return plan, "anthropic", model, evidence, nil
}

type plannerEnvelope struct {
	StructuredOutput json.RawMessage `json:"structured_output"`
	Result           string          `json:"result"`
	// IsError and Subtype are the CLI's own verdict on the invocation. A
	// print-mode envelope that says it failed must never be mined for a plan:
	// before F2 these were not read at all, so an error envelope carrying a
	// stub structured_output would have been accepted as a plan and executed.
	IsError bool   `json:"is_error"`
	Subtype string `json:"subtype"`
	// Usage is the print-mode CLI's own token report for this call. It is the
	// ONLY way the planner's spend can be observed: the invocation runs under
	// --no-session-persistence, so it writes no transcript for the usage
	// pipeline to discover. Absent on a CLI that does not report it, which the
	// caller renders as unknown rather than as zero.
	Usage *plannerUsage `json:"usage"`
	// ModelUsage is the per-model breakdown some CLI versions emit alongside
	// Usage. Preferred when present because it names the model the tokens were
	// actually spent on rather than the one AO asked for.
	ModelUsage map[string]plannerUsage `json:"modelUsage"`
}

// plannerUsage mirrors the print-mode usage block. Field names follow the
// provider's wire shape, not AO's.
type plannerUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// normalize folds one usage block into workflow's token vector, applying the
// same convention the transcript parser uses: InputTokens is the TOTAL input,
// with the cached halves counted inside it rather than beside it, so a planner
// call and a worker turn are summable without double counting.
func (u plannerUsage) normalize() (workflowcore.PlannerTokenUsage, bool) {
	uncached := u.InputTokens
	if uncached < 0 || u.OutputTokens < 0 || u.CacheCreationInputTokens < 0 || u.CacheReadInputTokens < 0 {
		return workflowcore.PlannerTokenUsage{}, false
	}
	total := uncached + u.CacheCreationInputTokens + u.CacheReadInputTokens
	if total == 0 && u.OutputTokens == 0 {
		return workflowcore.PlannerTokenUsage{}, false
	}
	return workflowcore.PlannerTokenUsage{
		InputTokens:         total,
		UncachedInputTokens: uncached,
		CacheWriteTokens:    u.CacheCreationInputTokens,
		CacheReadTokens:     u.CacheReadInputTokens,
		OutputTokens:        u.OutputTokens,
	}, true
}

// plannerUsageFromEnvelope picks the usage this call actually reported, and the
// model it names. It returns ok=false rather than a zeroed vector when the CLI
// reported nothing: a planner whose spend AO cannot see is unknown, not free.
func plannerUsageFromEnvelope(env plannerEnvelope, requestedModel string) (workflowcore.PlannerTokenUsage, string, bool) {
	// A per-model block is authoritative about WHICH model was billed; the
	// aggregate block is not. Prefer it, and take the one that actually
	// reported tokens.
	for model, usage := range env.ModelUsage {
		if normalized, ok := usage.normalize(); ok {
			return normalized, model, true
		}
	}
	if env.Usage != nil {
		if normalized, ok := env.Usage.normalize(); ok {
			return normalized, requestedModel, true
		}
	}
	return workflowcore.PlannerTokenUsage{}, "", false
}

// extractEnvelope parses the planner subprocess's combined output into its
// JSON envelope (Checkpoint 8P-E.10). The CLI is expected to print exactly
// one JSON object, but production evidence (MEDUSA workflow runs) showed
// occasional non-JSON leading/trailing text -- e.g. a stray banner or
// warning line -- surrounding an otherwise well-formed envelope. The fast
// path tries the raw bytes first; only on failure does it fall back to
// extracting the first balanced top-level `{...}` object and retrying, so a
// genuinely malformed body (no valid JSON object anywhere, or a truncated
// one) still fails loudly instead of being coerced into something plausible.
func extractEnvelope(b []byte) (plannerEnvelope, error) {
	var envelope plannerEnvelope
	if err := json.Unmarshal(b, &envelope); err == nil {
		return envelope, nil
	}
	obj, ok := firstBalancedJSONObject(b)
	if !ok {
		return plannerEnvelope{}, fmt.Errorf("no JSON object found in output")
	}
	if err := json.Unmarshal(obj, &envelope); err != nil {
		return plannerEnvelope{}, err
	}
	return envelope, nil
}

// firstBalancedJSONObject scans b for the first top-level `{...}` object,
// respecting string literals and escapes so braces inside string values
// (e.g. a step's description) never throw off the match. ok=false means no
// balanced object was found -- e.g. truncated output or no '{' at all.
func firstBalancedJSONObject(b []byte) ([]byte, bool) {
	start := -1
	depth := 0
	inString := false
	escaped := false
	for i, c := range b {
		if start == -1 {
			if c == '{' {
				start = i
				depth = 1
			}
			continue
		}
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : i+1], true
			}
		}
	}
	return nil, false
}

// snippet trims command output down to outputSnippetLimit bytes for inclusion
// in an error message. The bound is fixed rather than a parameter: every caller
// is building the same kind of diagnostic, and a per-call limit would only be a
// way for one of them to quietly log more than the others.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > outputSnippetLimit {
		return s[:outputSnippetLimit] + "…"
	}
	return s
}

func runRealCommand(ctx context.Context, binary string, args []string, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	// Checkpoint 8P-B.1: before this checkpoint the planner subprocess
	// silently inherited the daemon's own real environment (Go's exec.Cmd
	// default with Env left nil) -- the starkest of the five launch-path
	// gaps this checkpoint closes. req.RuntimeEnv (nil unless a workflow
	// owner with a connected profile was resolved) overrides on top of the
	// real inherited env, same override-wins convention as every other
	// launch path.
	cmd.Env = env
	return cmd.CombinedOutput()
}

// mergeEnv overlays overrides onto base ("KEY=VALUE" pairs), overrides
// winning on key collision. Never mutates process-global env.
func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, override := overrides[key]; override {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}
