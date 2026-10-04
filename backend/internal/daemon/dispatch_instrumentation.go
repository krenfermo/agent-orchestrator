package daemon

import (
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter/wfrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory/wfdispatch"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory/wfmemory"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// dispatch_instrumentation.go — Frente 3 / 3B: one composition step for every
// decorator that shapes or observes what an agent is told.
//
// THE BUG THIS EXISTS TO PREVENT. The three decorators (baseline evidence,
// context router, project memory) wrap the workflow ports they know:
// Planner, Spawner, ReviewerLauncher. P5-A 2C then gave workers a launcher of
// their own, workflowWorkerLauncher, which TRANSPORTS through a Spawner -- but
// it was constructed with the raw session manager before the decorators ran,
// and the coordinator prefers an injected WorkerLauncher over its own
// Spawner-backed default. So from 2026-09-08 every worker (and both Repair
// Agents, which dispatch as workers) went out around all three decorators:
// AO_MEMORY_MODE and AO_CONTEXT_ROUTER reached the Planner and the Reviewer
// and silently never reached the role that explores the repository most.
//
// THE FIX IS ORDER, NOT A WORKER-SPECIFIC WRAPPER. The Spawner is the one seam
// through which a worker's context is attached (SpawnConfig.IssueContext); the
// launcher adds identity, not context. So the launcher is bound to the Spawner
// AFTER every decorator has wrapped it, which makes a worker receive exactly
// what the default spawner path would have given it, through the same code.
// A future decorator added to Instrument below reaches workers for free.
//
// dispatchSurfaces (below) and its test make the class of bug loud: every
// Deps field that launches or messages an agent must be listed as either
// context-decorated or exempt with a stated reason, so a new launcher cannot
// be added without someone deciding whether it receives project context.

// instrumentAgentDispatch applies every dispatch decorator, then binds the
// surfaces that transport through another surface to the decorated one.
//
// Each decorator is a no-op when its switch is off (nil recorder, nil router,
// nil provisioner), so the default daemon still runs byte-for-byte the
// undecorated pipeline -- memory and routing stay OFF by default.
func instrumentAgentDispatch(
	deps workflowcore.Deps,
	recorder *projectmemory.Recorder,
	router *contextrouter.Router,
	memory wfmemory.Provisioner,
	log *slog.Logger,
) workflowcore.Deps {
	// Order is load-bearing and unchanged from before 3B: the baseline
	// observes what is finally sent, the router budgets what a dispatch holds,
	// and memory runs last so it sees the payload as it will be sent.
	deps = wfdispatch.Instrument(deps, recorder, log)
	deps = wfrouter.Instrument(deps, router, log)
	deps = wfmemory.Instrument(deps, memory, log)
	return bindWorkerTransport(deps)
}

// spawnerTransport is a WorkerLauncher whose launch goes out through a
// workflowcore.Spawner. withSpawner returns a copy that uses the given one;
// it never mutates the receiver, so a launcher shared by a test or a second
// wiring is not rebound behind its back.
type spawnerTransport interface {
	workflowcore.WorkerLauncher
	withSpawner(workflowcore.Spawner) workflowcore.WorkerLauncher
}

// bindWorkerTransport points a Spawner-backed worker launcher at deps.Spawner
// as it stands now -- i.e. after decoration. A launcher that does not
// transport through a Spawner is left alone; dispatchSurfaces classifies it.
func bindWorkerTransport(deps workflowcore.Deps) workflowcore.Deps {
	if deps.Spawner == nil {
		return deps
	}
	if t, ok := deps.WorkerLauncher.(spawnerTransport); ok {
		deps.WorkerLauncher = t.withSpawner(deps.Spawner)
	}
	return deps
}

func (l *workflowWorkerLauncher) withSpawner(s workflowcore.Spawner) workflowcore.WorkerLauncher {
	bound := *l
	bound.spawner = s
	return &bound
}

// surfaceClass says how a dispatch surface relates to project context.
type surfaceClass string

const (
	// surfaceDecorated: the context decorators wrap this field directly.
	surfaceDecorated surfaceClass = "decorated"
	// surfaceTransported: this field launches through a decorated surface and
	// is re-bound to it by bindWorkerTransport.
	surfaceTransported surfaceClass = "transported"
	// surfaceExempt: deliberately receives no repository memory; the reason
	// is recorded beside it.
	surfaceExempt surfaceClass = "exempt"
)

// dispatchSurface records one classification and why.
type dispatchSurface struct {
	class  surfaceClass
	reason string
}

// dispatchSurfaces classifies every workflowcore.Deps field that starts,
// re-starts or messages an agent process. The wiring test fails when Deps
// gains such a field that is not listed here, or when a listed "decorated" or
// "transported" field turns out not to be.
var dispatchSurfaces = map[string]dispatchSurface{
	"Planner":          {surfaceDecorated, "plan generation; receives the pack as a document"},
	"Spawner":          {surfaceDecorated, "the seam a worker's context is attached through (IssueContext)"},
	"ReviewerLauncher": {surfaceDecorated, "receives the pack as its standing system prompt"},
	"WorkerLauncher":   {surfaceTransported, "adds identity, transports through Spawner"},
	"MessageSender": {surfaceExempt,
		"fix/follow-up messages go into an existing, already-provisioned worker session; " +
			"re-attaching memory per message would duplicate it (observed by the baseline only)"},
	"Verifier": {surfaceExempt, "runs a verify command; no model, no context"},
	"IncidentAgents": {surfaceExempt,
		"diagnostic agent gets AO's own bounded incident pack and cannot read the repository; " +
			"incident repair runs in an isolated workspace -- repository memory there is a later phase"},
	"DecisionResolverLauncher": {surfaceExempt,
		"a read-only resolver pane answering one pending agent question (8K-B); it does not start repository work"},
	"Switcher": {surfaceExempt,
		"failover re-launches an EXISTING session on another harness through session_manager's switching saga; " +
			"it assembles no new context. Whether the switched harness keeps the original spawn's pack is a residual (3B docs)"},
}
