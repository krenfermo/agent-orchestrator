package workflow

import "sync"

// review_dispatch_inflight.go -- which review dispatch claims THIS process is
// executing right now (Frente 3 / 3D preflight).
//
// A dispatched outbox claim is durable, and the recovery path that re-enters
// a `dispatched` review step (adoptReviewOrMarkAmbiguous) exists for one
// situation: the dispatch that claimed it is gone -- the daemon crashed or
// restarted between the claim and the launch record. It used to be reached
// ALSO while the claiming dispatch was alive: a wake re-entered the step,
// probed a reviewer that did not exist yet, declared "no reviewer launch was
// ever recorded", failed the review run and released the claim -- and the live
// dispatch then launched a reviewer for a review AO had already failed.
//
// THE KEY IS THE CLAIM, NOT THE ENTRY. A claim is (outbox entry, dispatch
// generation), and the generation is minted by the dispatch's own durable
// AUTHORIZED record BEFORE it contends for the row, so every contender holds a
// distinct key. That is what makes it safe to RESERVE the key before the CAS:
//
//   - The winner's key is present from before the row can read `dispatched`,
//     so no pass can observe the durable claim without also seeing it live.
//     (Registering after the CAS left exactly that interval open.)
//   - A loser's key names a generation the row never holds, so it protects
//     nothing and is dropped when the loser returns.
//   - Generations never share a key: a reclaim by N+1 is protected only by
//     N+1's own reservation, and N ending can neither remove nor stand in for
//     it.
//
// It is in memory on purpose: after a restart it is empty, which is precisely
// the case recovery exists for, so durable recovery behaves exactly as before.
type reviewDispatchInFlight struct {
	mu     sync.Mutex
	claims map[reviewClaimKey]int
}

type reviewClaimKey struct {
	entryID    string
	generation string
}

// reserve marks the claim (entryID, generation) as executed by this process
// and returns the function that ends it. It is called BEFORE the claim CAS.
func (r *reviewDispatchInFlight) reserve(entryID, generation string) func() {
	key := reviewClaimKey{entryID: entryID, generation: generation}
	r.mu.Lock()
	if r.claims == nil {
		r.claims = map[reviewClaimKey]int{}
	}
	r.claims[key]++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if r.claims[key] <= 1 {
			delete(r.claims, key)
		} else {
			r.claims[key]--
		}
		r.mu.Unlock()
	}
}

// running reports whether a live dispatch in this process holds exactly this
// claim. The caller passes the generation the durable row names as its owner;
// any other generation's reservation is irrelevant to it.
func (r *reviewDispatchInFlight) running(entryID, generation string) bool {
	if entryID == "" || generation == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claims[reviewClaimKey{entryID: entryID, generation: generation}] > 0
}
