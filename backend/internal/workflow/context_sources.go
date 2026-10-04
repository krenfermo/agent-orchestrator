package workflow

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// withContextSources stamps the daemon's effective context-decorator state
// (Frente 3 / 3C) into a policy being frozen at run creation. It is evidence
// of which arm of a memory A/B a run belongs to; no decision reads it back.
// A coordinator built without the fact (tests, older wiring) leaves the zero
// value, which reads as "not recorded" rather than as "off".
func (c *Coordinator) withContextSources(policy domain.WorkflowPolicy) domain.WorkflowPolicy {
	policy.ContextSources = c.contextSources
	return policy
}
