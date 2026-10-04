package daemon

import (
	"context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// usage_subject_env.go — how a runtime pane learns which usage subject its
// tokens belong to.
//
// A reviewer and a decision resolver are not AO sessions, so nothing in their
// environment says whose ledger they are spending against. This one env var
// says it, and `ao hooks` (internal/cli/hooks.go) forwards the pane's own
// provider conversation id against it.
//
// The value is always a DURABLE AUTHORITY — a review run id, a question
// resolution id — never a pane name, a handle or a pid. That is what makes a
// second reviewer for the same step a different subject rather than an heir to
// the first one's tokens.
const usageSubjectEnvName = "AO_USAGE_SUBJECT"

// usageSubjectEnvValue renders a subject for the pane's environment, or ""
// when the subject is not one storage would accept. An unusable subject drops
// the variable entirely rather than exporting a malformed one: a pane with no
// subject reports no usage, which is an honest absence, whereas a pane with a
// broken subject would have its report rejected on every single hook.
func usageSubjectEnvValue(subject domain.UsageSubject) string {
	if !subject.Valid() {
		return ""
	}
	return strings.TrimSpace(subject.String())
}

// plannerUsageRecorder adapts the usage collector to workflow's planner-usage
// port. It is a shape adapter and nothing more: workflow states what the planner
// reported, the collector decides how it is stored.
type plannerUsageRecorder struct{ collector *usagesvc.Collector }

func (r plannerUsageRecorder) RecordDirectUsage(ctx context.Context, report workflowcore.PlannerUsageReport) error {
	if r.collector == nil {
		return nil
	}
	return r.collector.RecordDirectUsage(ctx, usagesvc.DirectUsageReport{
		Subject:    report.Subject,
		Harness:    report.Harness,
		ModelID:    report.ModelID,
		Tokens:     report.Tokens,
		EventKey:   report.EventKey,
		ObservedAt: report.ObservedAt,
	})
}

// plannerUsageRecorderFor returns the port, or nil when usage collection is off
// for this daemon. A nil recorder records nothing and the planner's spend reads
// as unknown -- never as zero.
func plannerUsageRecorderFor(collector *usagesvc.Collector) workflowcore.PlannerUsageRecorder {
	if collector == nil {
		return nil
	}
	return plannerUsageRecorder{collector: collector}
}
