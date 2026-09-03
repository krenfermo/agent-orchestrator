package workflow

import (
	stdctx "context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// usage_attribution.go — the write side of P3-E's role attribution.
//
// WHAT A WINDOW IS. When AO hands a role a session, it records the instant.
// From that instant until the next window on the same session, tokens spent in
// that session belong to that role. Nothing closes a window: the next opening
// is the close, which is why a crash between "opened" and anything else can
// neither lose an end nor invent one.
//
// WHY IT IS OPENED HERE AND NOT IN THE USAGE COLLECTOR. The collector reads
// provider transcripts; it has never heard of a planner or a fix cycle. The
// coordinator is the only component that knows a dispatch's role, task,
// attempt and cycle at the moment the dispatch happens — and "at the moment"
// matters, because a role recorded after the fact would be recorded after the
// tokens it is meant to explain.
//
// WHY IT NEVER FAILS A DISPATCH. Attribution is observability. A run that
// cannot record which role spent a token must still do the work; the failure
// is logged and the tokens land in the previous window, which the read model
// then reports as an approximate attribution. Losing a workflow to a telemetry
// write would be a far worse outcome than a slightly coarser breakdown.

// usageAttributionStore is the narrow write contract this file needs. It is
// obtained by type assertion on the coordinator's Store, matching how
// masterPlanStore is wired: a daemon whose store predates P3-E simply records
// no windows rather than failing to construct.
type usageAttributionStore interface {
	OpenUsageAttributionWindow(ctx stdctx.Context, window domain.UsageAttributionWindow) error
}

// usageWindowSpec is one dispatch's attribution facts.
type usageWindowSpec struct {
	// Subject is the surface the tokens will be spent on, and it MUST be the
	// same subject the surface itself binds under:
	//
	//   worker / repair   session:<ao session id>
	//   reviewer          runtime_pane:<review run id>
	//   decision resolver runtime_pane:<resolution id>
	//   planner           planner_invocation:<invocation id>
	//
	// If the two ever disagree the surface's events resolve to no window, which
	// is why every caller derives both from the same durable authority rather
	// than from a handle or a process.
	Subject domain.UsageSubject
	// SessionID is the shorthand a session-backed dispatch uses; it is folded
	// into Subject when Subject is unset, so the worker path reads exactly as
	// it did before subjects existed.
	SessionID string
	Role      domain.WorkflowRole
	Run       domain.WorkflowRun
	StepID    string
	TaskID    string
	AttemptID string
	// AttemptOrdinal is the failover hop (1 = preferred provider). It keeps a
	// failed attempt's tokens from being folded into its successor's.
	AttemptOrdinal int64
	// Cycle is 0 for base execution and the fix-cycle number for repair, which
	// is what lets a run report "base 40k, repair +18k".
	Cycle    int64
	Harness  string
	Provider string
	Model    string
	OpenedAt time.Time
}

// openUsageWindow records one role's claim on a session's timeline.
func (c *Coordinator) openUsageWindow(ctx stdctx.Context, spec usageWindowSpec) {
	if c == nil || c.usageWindows == nil {
		return
	}
	subject := spec.Subject
	if !subject.Valid() {
		subject = domain.SessionSubject(domain.SessionID(strings.TrimSpace(spec.SessionID)))
	}
	if !subject.Valid() || spec.Role == "" {
		return
	}
	openedAt := spec.OpenedAt
	if openedAt.IsZero() {
		openedAt = c.clock()
	}
	openedAt = openedAt.UTC()

	provider := spec.Provider
	if provider == "" {
		provider = domain.ProviderForHarness(domain.AgentHarness(spec.Harness))
	}
	taskID := spec.TaskID
	if taskID == "" && spec.Run.PlannedTaskID != nil {
		taskID = *spec.Run.PlannedTaskID
	}
	parent := ""
	if spec.Run.ParentWorkflowID != nil {
		parent = *spec.Run.ParentWorkflowID
	}

	window := domain.UsageAttributionWindow{
		DedupeKey:           usageWindowDedupeKey(spec, subject),
		SubjectKind:         subject.Kind,
		SessionID:           subject.ID,
		ProjectID:           spec.Run.ProjectID,
		WorkflowRunID:       spec.Run.ID,
		ParentWorkflowRunID: parent,
		TaskID:              taskID,
		WorkflowStepID:      spec.StepID,
		AttemptID:           spec.AttemptID,
		AttemptOrdinal:      spec.AttemptOrdinal,
		Cycle:               spec.Cycle,
		Role:                spec.Role,
		Harness:             spec.Harness,
		Provider:            provider,
		Model:               spec.Model,
		OpenedAt:            openedAt,
		CreatedAt:           c.clock().UTC(),
	}
	if err := c.usageWindows.OpenUsageAttributionWindow(ctx, window); err != nil && c.log != nil {
		c.log.Warn("usage attribution window not recorded",
			"run", spec.Run.ID, "step", spec.StepID, "role", string(spec.Role), "err", err)
	}
}

// usageWindowDedupeKey derives the window's identity from the DURABLE
// obligation only — never from a clock. A dispatch replayed by failover,
// resume, or a wake therefore re-opens the same window instead of splitting a
// role's tokens across two, and a restart mid-dispatch cannot double-count.
func usageWindowDedupeKey(spec usageWindowSpec, subject domain.UsageSubject) string {
	parts := []string{
		spec.Run.ID,
		spec.StepID,
		spec.AttemptID,
		strconv.FormatInt(spec.AttemptOrdinal, 10),
		strconv.FormatInt(spec.Cycle, 10),
		string(spec.Role),
		string(subject.Kind),
		subject.ID,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "uaw-" + hex.EncodeToString(sum[:16])
}

// plannerUsageSubject names one planner invocation.
//
// The planner is NOT an in-process call: adapters/planner/command shells out to
// `claude --print`, which spends real Anthropic tokens. It has no AO session and
// -- because it runs under `--no-session-persistence` -- no transcript either,
// so its usage is read from the print-mode response envelope it already returns
// and recorded against this subject.
//
// The invocation id includes the run and the attempt, so a retried planner call
// is its own subject rather than an heir to the previous attempt's tokens.
func plannerUsageSubject(runID string, attempt int) domain.UsageSubject {
	return domain.PlannerInvocationSubject(runID + "#" + strconv.Itoa(attempt))
}
