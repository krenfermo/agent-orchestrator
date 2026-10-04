package lifecycle

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// recordRepairOutcome reports what AO's automatic repair loop for one problem
// just did.
//
// Both outcomes are reported, including the boring one. AO retrying is a real
// session-level fact, and handing it over explicitly -- rather than simply not
// calling anything -- is what makes "AO does not interrupt you while it is
// repairing" a policy stated in one place (service/notification's policyFor)
// instead of a silence spread across call sites.
//
// A suppressed nudge is neither outcome: nothing reached the worker and the
// attempt was not spent, so there is no repair fact to report yet.
func (m *Manager) recordRepairOutcome(
	ctx context.Context,
	rec domain.SessionRecord,
	prURL, key string,
	maxAttempts int,
	outcome sendOnceOutcome,
) {
	if m.sessionFacts == nil || !outcome.accounted() {
		return
	}
	kind := ports.SessionFactRepairAttempted
	detail := ""
	if outcome == sendOnceExhausted {
		kind = ports.SessionFactRepairExhausted
		detail = fmt.Sprintf("AO tried %d times.", maxAttempts)
	}
	m.recordSessionFact(ctx, ports.SessionFact{
		Kind:      kind,
		SessionID: rec.ID,
		ProjectID: rec.ProjectID,
		// The reaction key plus the budget it was given is the repair loop's
		// durable identity: the key already names the problem and the PR, and
		// both live in the attempt map persisted on the PR row.
		ScopeID:            ports.RepairScopeID(key, maxAttempts),
		PRURL:              prURL,
		Detail:             detail,
		SessionDisplayName: rec.DisplayName,
		ObservedAt:         m.clock(),
	})
}
