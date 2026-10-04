package workflow

import "testing"

// Generation N in flight, N+1 after a valid reclaim: each claim is its own
// key. N can neither protect, remove nor hide N+1, and N+1 cannot overwrite
// N's state.
func TestReviewDispatchInFlightKeysAreGenerationScoped(t *testing.T) {
	var r reviewDispatchInFlight
	endN := r.reserve("entry-1", "gen-N")
	if !r.running("entry-1", "gen-N") {
		t.Fatal("N must be live once reserved")
	}
	if r.running("entry-1", "gen-N+1") {
		t.Fatal("N's reservation must not protect N+1")
	}
	endN1 := r.reserve("entry-1", "gen-N+1")
	if !r.running("entry-1", "gen-N") || !r.running("entry-1", "gen-N+1") {
		t.Fatal("N+1 must not overwrite or hide N, nor N hide N+1")
	}
	endN()
	if r.running("entry-1", "gen-N") {
		t.Fatal("N must be gone after it ends")
	}
	if !r.running("entry-1", "gen-N+1") {
		t.Fatal("N ending must not remove N+1")
	}
	endN() // an extra end of N is harmless
	if !r.running("entry-1", "gen-N+1") {
		t.Fatal("a repeated end of N must not remove N+1")
	}
	endN1()
	if r.running("entry-1", "gen-N+1") {
		t.Fatal("N+1 must be gone after it ends")
	}
	if r.running("entry-1", "") || r.running("", "gen-N") {
		t.Fatal("an empty entry or generation is never live")
	}
	endA := r.reserve("entry-2", "gen-N")
	if r.running("entry-1", "gen-N") {
		t.Fatal("claims of different entries are independent")
	}
	endA()
}
