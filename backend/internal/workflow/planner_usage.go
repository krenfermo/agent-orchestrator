package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// planner_usage.go — metering the one provider-backed role that has no
// transcript.
//
// THE PLANNER IS NOT IN-PROCESS. adapters/planner/command shells out to
// `claude --print --output-format json ... --no-session-persistence`. That is a
// real Anthropic call spending real tokens on every objective AO plans. The
// `--no-session-persistence` flag is why it cannot be metered the way a worker
// is: it writes no JSONL, so the usage pipeline has nothing to discover and
// nothing to tail. Its spend is stated exactly once, in the JSON envelope the
// adapter already parses, and this file carries that fact to the ledger.
//
// A FAILED PLANNER CALL STILL SPENT TOKENS. A timeout after the provider has
// generated most of a plan is billed like any other call, so the recording path
// runs on both outcomes — the evidence carrying the usage is returned on success
// and wrapped in PlannerAttemptError on failure, and both are handled here.

// PlannerUsageRecorder is the narrow write this file needs.
// *usage.Collector satisfies it.
type PlannerUsageRecorder interface {
	RecordDirectUsage(ctx stdctx.Context, report PlannerUsageReport) error
}

// PlannerUsageReport is one planner invocation's provider-reported spend.
type PlannerUsageReport struct {
	Subject    domain.UsageSubject
	Harness    domain.AgentHarness
	ModelID    string
	Tokens     domain.UsageTokenMetrics
	EventKey   string
	ObservedAt time.Time
}

// recordPlannerUsage stores what one planner invocation reported spending.
//
// Best-effort, like every other observation in this package: a planner call that
// produced a usable plan must not be failed because its telemetry could not be
// written. What it must never do is invent a figure — an invocation that
// reported nothing records nothing, and the read model shows the planner's spend
// as unknown rather than as zero.
func (c *Coordinator) recordPlannerUsage(
	ctx stdctx.Context,
	subject domain.UsageSubject,
	provider, model string,
	evidence PlannerAttemptEvidence,
) {
	if c == nil || c.plannerUsage == nil || evidence.Usage == nil || !subject.Valid() {
		return
	}
	harness := plannerHarnessFor(provider)
	if harness == "" {
		return
	}
	usedModel := strings.TrimSpace(evidence.UsageModel)
	if usedModel == "" {
		usedModel = strings.TrimSpace(model)
	}
	u := *evidence.Usage
	report := PlannerUsageReport{
		Subject: subject,
		Harness: harness,
		ModelID: usedModel,
		Tokens: domain.UsageTokenMetrics{
			InputTokens:         u.InputTokens,
			UncachedInputTokens: u.UncachedInputTokens,
			CacheReadTokens:     u.CacheReadTokens,
			CacheWriteTokens:    u.CacheWriteTokens,
			OutputTokens:        u.OutputTokens,
		},
		EventKey:   plannerUsageEventKey(subject, usedModel, u),
		ObservedAt: c.clock(),
	}
	if err := c.plannerUsage.RecordDirectUsage(ctx, report); err != nil && c.log != nil {
		c.log.Warn("planner usage not recorded", "subject", subject.String(), "err", err)
	}
}

// plannerUsageEventKey is the invocation's exactly-once identity.
//
// Derived from the subject (which already carries the run and the attempt
// number) plus the reported figures, and never from a clock — so re-recording
// the same invocation, after a retry or a restart, re-derives the same key and
// inserts nothing. Including the figures means a genuinely different report for
// the same invocation is a different event rather than being silently swallowed
// by the first one.
func plannerUsageEventKey(subject domain.UsageSubject, model string, u PlannerTokenUsage) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		subject.Kind.String(), subject.ID, model,
		itoa(u.InputTokens), itoa(u.UncachedInputTokens), itoa(u.CacheReadTokens),
		itoa(u.CacheWriteTokens), itoa(u.OutputTokens),
	}, "\x00")))
	return "planner-" + hex.EncodeToString(sum[:16])
}

// plannerHarnessFor maps the planner's reported provider onto the harness whose
// usage pipeline can meter it. An unknown provider yields "" and records
// nothing: AO would rather report a planner's spend as unknown than file it
// under a harness it guessed.
func plannerHarnessFor(provider string) domain.AgentHarness {
	switch strings.TrimSpace(provider) {
	case "anthropic":
		return domain.HarnessClaudeCode
	case "openai":
		return domain.HarnessCodex
	default:
		return ""
	}
}

func itoa(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%10]
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
