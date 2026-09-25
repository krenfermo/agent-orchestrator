package workflow

import "sync"

// review_dispatch_inflight.go -- which review dispatch claims THIS process is
// executing right now (Frente 3 / 3D preflight).
//
// A dispatched outbox claim is durable, and the recovery path that re-enters
// a `dispatched` review step (adoptReviewOrMarkAmbiguous) exists for one
// situation: the dispatch that claimed it is gone -- the daemon crashed or
// restarted between the claim and the launch record. It used to be reached
// ALSO while the claiming dispatch was alive and still launching (provisioning
// the reviewer's context, starting its session): a wake re-entered the step,
// probed a reviewer that did not exist yet, declared "no reviewer launch was
// ever recorded", failed the review run and released the claim -- and the
// live dispatch then launched a reviewer for a review AO had already failed.
//
// The distinction the recovery path needs is exact, not a time window: is the
// dispatch that owns this claim generation running in this process? This set
// answers it. It is in memory on purpose: after a restart it is empty, which is
// precisely the case recovery exists for, so the durable recovery behaves
// exactly as before.
type reviewDispatchInFlight struct {
	mu sync.Mutex
	// generations maps an outbox entry id to the claim generation a live
	// dispatch in this process holds.
	generations map[string]string
}

// begin records that this process is executing the dispatch holding
// (entryID, generation) and returns the function that ends it.
func (r *reviewDispatchInFlight) begin(entryID, generation string) func() {
	r.mu.Lock()
	if r.generations == nil {
		r.generations = map[string]string{}
	}
	r.generations[entryID] = generation
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.generations[entryID] == generation {
			delete(r.generations, entryID)
		}
		r.mu.Unlock()
	}
}

// running reports whether a live dispatch in this process holds exactly this
// claim generation. A released and re-claimed entry is a different generation
// and is not covered by an older dispatch.
func (r *reviewDispatchInFlight) running(entryID, generation string) bool {
	if generation == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.generations[entryID] == generation
}
