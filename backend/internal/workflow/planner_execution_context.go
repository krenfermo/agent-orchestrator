package workflow

import (
	stdctx "context"
	"sync"
)

// plannerExecution owns the contexts in-flight planner subprocesses run on.
//
// A plan generation is durable work that takes minutes, but every entry point
// into it carries a context that belongs to something far shorter-lived: the
// HTTP handler passes r.Context() (dead as soon as the client disconnects or
// navigates away) and the wake poller passes its own cycle context. Inheriting
// either one means a planner call can be killed by a browser tab, and its
// deadline -- whatever it happens to be -- silently overrides the adapter's own
// bounded budget.
//
// So the planner gets a context AO owns. It is not context.Background(): it
// still ends when the daemon shuts down (lifetime), when this run is cancelled
// (cancelInFlightPlanner), or when the adapter's own timeout expires. What it
// no longer ends on is a transport or tick that has nothing to do with whether
// the plan is still wanted.
type plannerExecution struct {
	mu       sync.Mutex
	lifetime stdctx.Context
	// inFlight is keyed by run and then by a per-call token, because func
	// values are not comparable: a release must be able to drop exactly its
	// own cancel and leave any concurrent call for the same run alone.
	inFlight map[string]map[int64]stdctx.CancelFunc
	nextID   int64
}

// SetExecutionLifetime hands the coordinator the daemon's own lifetime
// context, so durable work it starts on AO's behalf still stops when the
// daemon does. Called once at boot, before the wake poller starts. Unset (the
// test default) means only per-run cancellation and the adapter's own timeout
// bound a planner call, which is exactly the behavior a test wants.
func (c *Coordinator) SetExecutionLifetime(ctx stdctx.Context) {
	c.plannerExec.mu.Lock()
	defer c.plannerExec.mu.Unlock()
	c.plannerExec.lifetime = ctx
}

// plannerExecutionContext derives the context one planner call runs on and
// returns it with a release func the caller must always invoke.
//
// The returned context keeps ctx's VALUES (trace ids, request-scoped metadata
// every decorator in the planner chain reads) while dropping its cancellation
// and deadline -- context.WithoutCancel is precisely that operation.
func (c *Coordinator) plannerExecutionContext(ctx stdctx.Context, runID string) (stdctx.Context, func()) {
	exec, cancel := stdctx.WithCancel(stdctx.WithoutCancel(ctx))

	c.plannerExec.mu.Lock()
	lifetime := c.plannerExec.lifetime
	if c.plannerExec.inFlight == nil {
		c.plannerExec.inFlight = map[string]map[int64]stdctx.CancelFunc{}
	}
	if c.plannerExec.inFlight[runID] == nil {
		c.plannerExec.inFlight[runID] = map[int64]stdctx.CancelFunc{}
	}
	c.plannerExec.nextID++
	token := c.plannerExec.nextID
	c.plannerExec.inFlight[runID][token] = cancel
	c.plannerExec.mu.Unlock()

	stopLifetimeWatch := func() bool { return false }
	if lifetime != nil {
		// AfterFunc fires immediately if lifetime is already done, so a call
		// made during shutdown is cancelled rather than started unbounded.
		stopLifetimeWatch = stdctx.AfterFunc(lifetime, cancel)
	}

	var once sync.Once
	return exec, func() {
		once.Do(func() {
			stopLifetimeWatch()
			c.forgetPlannerCancel(runID, token)
			cancel()
		})
	}
}

// cancelInFlightPlanner stops any planner subprocess still running for this
// run. Cancelling a run must stop the work it is paying for, and a planner
// call detached from the caller's context would otherwise keep running to its
// own timeout after the run it belongs to is already terminal.
func (c *Coordinator) cancelInFlightPlanner(runID string) {
	c.plannerExec.mu.Lock()
	cancels := c.plannerExec.inFlight[runID]
	delete(c.plannerExec.inFlight, runID)
	c.plannerExec.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (c *Coordinator) forgetPlannerCancel(runID string, token int64) {
	c.plannerExec.mu.Lock()
	defer c.plannerExec.mu.Unlock()
	delete(c.plannerExec.inFlight[runID], token)
	if len(c.plannerExec.inFlight[runID]) == 0 {
		delete(c.plannerExec.inFlight, runID)
	}
}
