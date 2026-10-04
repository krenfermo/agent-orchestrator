package runtimegc_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// gc_test.go — the safety model, stated as tests.
//
// Almost every case here is a REFUSAL: a session AO must not destroy, and the
// proof it is missing. That balance is the point. A GC whose tests are mostly
// "it deleted the thing" is a GC nobody should run on a machine with work on
// it.

// fakeRuntime is a runtime whose sessions can be manipulated between calls,
// which is how the ABA cases are constructed: the name stays, the incarnation
// changes underneath.
type fakeRuntime struct {
	mu       sync.Mutex
	sessions map[string]ports.RuntimeSessionSummary // by instance id
	// factsErr and destroyErr inject failures per instance.
	factsErr   map[string]error
	destroyErr map[string]error
	listErr    error
	destroyed  []string
	// onDestroy runs after a successful destroy, so a test can model a
	// replacement taking the same name.
	onDestroy func(instance string)
	// afterList runs once, immediately after a listing is returned. It is how
	// the ABA races are modelled honestly: the world changes between the
	// moment the sweep decides and the moment it acts, which is exactly the
	// window the InstanceID discipline exists to close.
	afterList func()
}

func newFakeRuntime(sessions ...ports.RuntimeSessionSummary) *fakeRuntime {
	r := &fakeRuntime{
		sessions:   map[string]ports.RuntimeSessionSummary{},
		factsErr:   map[string]error{},
		destroyErr: map[string]error{},
	}
	for _, s := range sessions {
		r.sessions[s.InstanceID] = s
	}
	return r
}

func (r *fakeRuntime) ListSessions(context.Context) ([]ports.RuntimeSessionSummary, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]ports.RuntimeSessionSummary, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	if after := r.afterList; after != nil {
		r.afterList = nil
		r.mu.Unlock()
		after()
		r.mu.Lock()
	}
	return out, nil
}

// SessionFacts resolves by INSTANCE when one is given, and by name otherwise —
// exactly like the real adapter, so a name whose incarnation has changed
// answers with the new one.
func (r *fakeRuntime) SessionFacts(_ context.Context, handle ports.RuntimeHandle) (ports.SessionFacts, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.factsErr[handle.InstanceID]; err != nil {
		return ports.SessionFacts{}, false, err
	}
	if s, ok := r.sessions[handle.InstanceID]; ok {
		return ports.SessionFacts{InstanceID: s.InstanceID, Owner: s.Owner, OwnerKnown: s.OwnerKnown}, true, nil
	}
	// The incarnation is gone. If something else now holds the NAME, report
	// that — this is the ABA the sweeper has to notice.
	for _, s := range r.sessions {
		if s.ID == handle.ID {
			return ports.SessionFacts{InstanceID: s.InstanceID, Owner: s.Owner, OwnerKnown: s.OwnerKnown}, true, nil
		}
	}
	return ports.SessionFacts{}, false, nil
}

func (r *fakeRuntime) DestroyInstance(_ context.Context, instanceID string) error {
	r.mu.Lock()
	if err := r.destroyErr[instanceID]; err != nil {
		r.mu.Unlock()
		return err
	}
	delete(r.sessions, instanceID)
	r.destroyed = append(r.destroyed, instanceID)
	onDestroy := r.onDestroy
	r.mu.Unlock()
	if onDestroy != nil {
		onDestroy(instanceID)
	}
	return nil
}

func (r *fakeRuntime) alive(instance string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.sessions[instance]
	return ok
}

// fakeClaims and fakeRuns are the durable halves.
type fakeClaims struct {
	outstanding []domain.CapacityClaim
	held        []domain.CapacityClaim
	err         error
}

func (f *fakeClaims) ListOutstandingCapacityClaims(context.Context) ([]domain.CapacityClaim, error) {
	return f.outstanding, f.err
}
func (f *fakeClaims) ListHeldCapacityClaims(context.Context) ([]domain.CapacityClaim, error) {
	return f.held, f.err
}
func (f *fakeClaims) ListCapacityClaimsForRun(_ context.Context, runID string) ([]domain.CapacityClaim, error) {
	var out []domain.CapacityClaim
	for _, c := range f.outstanding {
		if c.WorkflowRunID == runID {
			out = append(out, c)
		}
	}
	return out, f.err
}

type fakeRuns struct {
	runs map[string]domain.WorkflowRun
	err  error
}

func (f *fakeRuns) GetWorkflowRun(_ context.Context, id string) (domain.WorkflowRun, bool, error) {
	if f.err != nil {
		return domain.WorkflowRun{}, false, f.err
	}
	run, ok := f.runs[id]
	return run, ok, nil
}
func (f *fakeRuns) ListWorkflowRuns(context.Context, string) ([]domain.WorkflowRun, error) {
	out := make([]domain.WorkflowRun, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r)
	}
	return out, f.err
}

func owned(name, instance, owner string) ports.RuntimeSessionSummary {
	return ports.RuntimeSessionSummary{ID: name, InstanceID: instance, Owner: owner, OwnerKnown: true}
}

func unowned(name, instance string) ports.RuntimeSessionSummary {
	return ports.RuntimeSessionSummary{ID: name, InstanceID: instance}
}

func sweeper(rt *fakeRuntime, claims *fakeClaims, runs *fakeRuns) *runtimegc.Sweeper {
	return &runtimegc.Sweeper{
		Inventory: rt, Facts: rt, Claims: claims, Runs: runs,
		Now: func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) },
	}
}

func findingFor(t *testing.T, report runtimegc.Report, instance string) runtimegc.Finding {
	t.Helper()
	for _, f := range report.Findings {
		if f.InstanceID == instance {
			return f
		}
	}
	t.Fatalf("no finding for %s in %+v", instance, report.Findings)
	return runtimegc.Finding{}
}

// Matrix 25/26/27/28/34/35: what an inventory scan may and may not reclaim.
func TestInventorySweepReclaimsOnlyProvenOwnership(t *testing.T) {
	rt := newFakeRuntime(
		// 25/34: AO's own, no live claim -> an orphan it may reclaim.
		owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"),
		// 27/28/35: no ownership token. This covers the operator's own
		// session, a stranger's, and one whose marker AO simply could not
		// read -- all three are the same answer, and it is "leave it alone".
		unowned("someones-shell", "$2"),
	)
	claims := &fakeClaims{}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$1"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("an AO-owned orphan was %s, want cleaned: %+v", got.Disposition, got)
	}
	if got := findingFor(t, report, "$2"); got.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("a session with no ownership token was %s, want unprovable: %+v", got.Disposition, got)
	}
	if rt.alive("$1") {
		t.Fatal("the orphan survived")
	}
	if !rt.alive("$2") {
		t.Fatal("a session AO could not prove it owned was destroyed")
	}
	if report.Cleaned != 1 || report.SkippedUnprovable != 1 {
		t.Fatalf("report = %+v, want 1 cleaned and 1 unprovable", report)
	}
	// Every finding says WHY, which is the whole audit requirement.
	for _, f := range report.Findings {
		if f.Reason == "" {
			t.Fatalf("a finding with no reason: %+v", f)
		}
	}
}

// Matrix 26/31: a live claim protects its runtime absolutely.
func TestHeldClaimProtectsItsRuntime(t *testing.T) {
	rt := newFakeRuntime(owned("ao-worker-1", "$10", "ao-worker:w-1"))
	claim := domain.CapacityClaim{
		ID: "cap-1", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-1", RuntimeHandle: "ao-worker-1", RuntimeInstanceID: "$10",
		DispatchKey: "k1", LifecycleGeneration: 1,
	}
	claims := &fakeClaims{held: []domain.CapacityClaim{claim}, outstanding: []domain.CapacityClaim{claim}}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{
		"wf-1": {ID: "wf-1", State: domain.WorkflowRunRunning},
	}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$10"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a runtime a held claim is paying for was %s, want live: %+v", got.Disposition, got)
	}
	if !rt.alive("$10") {
		t.Fatal("GC destroyed a runtime that a live capacity claim was paying for")
	}
}

// Matrix 30: session-name reuse. A stale classification must not destroy the
// session that took the name afterwards.
func TestReusedSessionNameIsNeverDestroyedByAStaleSweep(t *testing.T) {
	// The sweep lists $20 and classifies it as an orphan. THEN, before it can
	// act, that session exits and a brand-new one takes the same name.
	rt := newFakeRuntime(owned("ao-reviewer-7", "$20", "ao-reviewer:rv-7"))
	rt.afterList = func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		delete(rt.sessions, "$20")
		rt.sessions["$21"] = owned("ao-reviewer-7", "$21", "ao-reviewer:rv-7")
	}
	claims := &fakeClaims{}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// The verdict was about $20. The name now answers for $21, so the sweep
	// destroys nothing -- a kill addressed to a NAME would have taken the
	// replacement instead.
	got := findingFor(t, report, "$20")
	if got.Disposition != runtimegc.DispositionForeign {
		t.Fatalf("a candidate whose name was reused mid-sweep was %s, want foreign: %+v", got.Disposition, got)
	}
	if !rt.alive("$21") {
		t.Fatal("a replacement session holding the same name was destroyed by a stale classification")
	}
	if len(rt.destroyed) != 0 {
		t.Fatalf("destroyed %v; nothing should have been", rt.destroyed)
	}
}

// The same hazard from the other direction: a candidate derived from a durable
// claim, whose name has since been taken by a different incarnation.
func TestClaimCandidateWhoseNameWasReusedIsRefused(t *testing.T) {
	// A finished run's claim names $30. That incarnation is gone, and a
	// DIFFERENT run's live worker has since taken the same session name.
	rt := newFakeRuntime(owned("ao-worker-9", "$31", "ao-worker:w-9"))
	stale := domain.CapacityClaim{
		ID: "cap-9", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-9", RuntimeHandle: "ao-worker-9", RuntimeInstanceID: "$30",
		DispatchKey: "k9", LifecycleGeneration: 1,
	}
	live := domain.CapacityClaim{
		ID: "cap-10", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-10", RuntimeHandle: "ao-worker-9", RuntimeInstanceID: "$31",
		DispatchKey: "k10", LifecycleGeneration: 1,
	}
	claims := &fakeClaims{
		held:        []domain.CapacityClaim{stale, live},
		outstanding: []domain.CapacityClaim{stale, live},
	}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{
		"wf-9":  {ID: "wf-9", State: domain.WorkflowRunCompleted},
		"wf-10": {ID: "wf-10", State: domain.WorkflowRunRunning},
	}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	got := findingFor(t, report, "$30")
	if got.Disposition != runtimegc.DispositionForeign {
		t.Fatalf("a candidate whose name was reused was %s, want foreign: %+v", got.Disposition, got)
	}
	if !rt.alive("$31") {
		t.Fatal("the incarnation that took the name was destroyed")
	}
}

// Matrix 32/33: a runtime whose run has ended is reclaimed, and one already
// gone is simply reported absent.
func TestTerminalRunRuntimesAreReclaimedAndAbsentOnesReported(t *testing.T) {
	rt := newFakeRuntime(owned("ao-worker-5", "$40", "ao-worker:w-5"))
	terminal := domain.CapacityClaim{
		ID: "cap-5", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-5", RuntimeHandle: "ao-worker-5", RuntimeInstanceID: "$40",
		DispatchKey: "k5", LifecycleGeneration: 2,
	}
	// A claim naming a runtime that no longer exists at all.
	gone := domain.CapacityClaim{
		ID: "cap-6", Kind: domain.ExecutionKindReviewer, State: domain.CapacityClaimHeld,
		WorkflowRunID: "wf-5", RuntimeHandle: "ao-reviewer-5", RuntimeInstanceID: "$41",
		DispatchKey: "k6", LifecycleGeneration: 1,
	}
	claims := &fakeClaims{
		held:        []domain.CapacityClaim{terminal, gone},
		outstanding: []domain.CapacityClaim{terminal, gone},
	}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{
		"wf-5": {ID: "wf-5", State: domain.WorkflowRunCancelled},
	}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$40"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("a terminal run's runtime was %s, want cleaned: %+v", got.Disposition, got)
	}
	if got := findingFor(t, report, "$41"); got.Disposition != runtimegc.DispositionAbsent {
		t.Fatalf("an already-gone runtime was %s, want absent: %+v", got.Disposition, got)
	}
	if rt.alive("$40") {
		t.Fatal("the terminal run's runtime survived")
	}
}

// Matrix 40: a dry run classifies exactly as a real sweep and destroys nothing.
func TestDryRunClassifiesWithoutDestroying(t *testing.T) {
	build := func() (*fakeRuntime, *fakeClaims, *fakeRuns) {
		return newFakeRuntime(
				owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"),
				unowned("someones-shell", "$2"),
			),
			&fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}}
	}
	dryRT, dryClaims, dryRuns := build()
	dry, err := sweeper(dryRT, dryClaims, dryRuns).Sweep(context.Background(), runtimegc.Options{DryRun: true, Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRT.destroyed) != 0 {
		t.Fatalf("a dry run destroyed %v", dryRT.destroyed)
	}
	if dry.Cleaned != 0 || dry.Candidates != 1 {
		t.Fatalf("dry run report = %+v, want 1 candidate and 0 cleaned", dry)
	}
	if got := findingFor(t, dry, "$1"); got.Disposition != runtimegc.DispositionCandidate {
		t.Fatalf("dry run classified the orphan as %s, want candidate", got.Disposition)
	}

	// The real sweep reaches the same classification, which is what makes the
	// dry run a preview rather than a different algorithm.
	realRT, realClaims, realRuns := build()
	actual, err := sweeper(realRT, realClaims, realRuns).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if actual.Candidates != dry.Candidates || actual.SkippedUnprovable != dry.SkippedUnprovable {
		t.Fatalf("dry run and real sweep disagreed: dry=%+v real=%+v", dry, actual)
	}
	if actual.Cleaned != 1 {
		t.Fatalf("the real sweep cleaned %d, want 1", actual.Cleaned)
	}
}

// Matrix 38: repeated sweeps converge. The second finds nothing to do.
func TestRepeatedSweepsAreIdempotent(t *testing.T) {
	rt := newFakeRuntime(
		owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"),
		unowned("someones-shell", "$2"),
	)
	s := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}})
	ctx := context.Background()

	first, err := s.Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.Sweep(ctx, runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Cleaned != 1 {
		t.Fatalf("first sweep cleaned %d, want 1", first.Cleaned)
	}
	if second.Cleaned != 0 || third.Cleaned != 0 {
		t.Fatalf("later sweeps cleaned %d and %d; nothing new existed to clean", second.Cleaned, third.Cleaned)
	}
	// The session AO could not prove is reported every time, never destroyed.
	if second.SkippedUnprovable != 1 || third.SkippedUnprovable != 1 {
		t.Fatalf("the unprovable session stopped being reported: %+v %+v", second, third)
	}
	if len(rt.destroyed) != 1 {
		t.Fatalf("destroyed %v across three sweeps, want exactly one", rt.destroyed)
	}
}

// Matrix 39: one broken candidate does not abort the others. This is §W's rule
// at the resource level.
func TestOneBrokenCandidateDoesNotStopTheSweep(t *testing.T) {
	rt := newFakeRuntime(
		owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"),
		owned("ao-reviewer-2", "$2", "ao-reviewer:rv-2"),
		owned("ao-reviewer-3", "$3", "ao-reviewer:rv-3"),
	)
	rt.factsErr["$2"] = errors.New("the runtime would not answer about this session")
	rt.destroyErr["$3"] = errors.New("kill failed")

	report, err := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatalf("a per-candidate failure aborted the whole sweep: %v", err)
	}
	if got := findingFor(t, report, "$1"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("the healthy candidate was %s, want cleaned", got.Disposition)
	}
	for _, instance := range []string{"$2", "$3"} {
		if got := findingFor(t, report, instance); got.Disposition != runtimegc.DispositionError {
			t.Fatalf("%s was %s, want error", instance, got.Disposition)
		}
		if got := findingFor(t, report, instance); got.Err == "" {
			t.Fatalf("%s recorded no error text", instance)
		}
	}
	if report.Errors != 2 || report.Cleaned != 1 {
		t.Fatalf("report = %+v, want 1 cleaned and 2 errors", report)
	}
	// The one that could not be read is still alive, which is the point.
	if !rt.alive("$2") || !rt.alive("$3") {
		t.Fatal("a candidate AO could not handle was destroyed anyway")
	}
}

// Matrix 27: a runtime AO cannot reach is unknown, and unknown is not dead.
func TestUnreachableRuntimeIsNeverDestroyed(t *testing.T) {
	rt := newFakeRuntime(owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"))
	rt.factsErr["$1"] = ports.ErrRuntimeUnavailable

	report, err := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$1"); got.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("an unreachable runtime was %s, want unprovable: %+v", got.Disposition, got)
	}
	if !rt.alive("$1") {
		t.Fatal("a runtime AO could not reach was destroyed")
	}
}

// Matrix 36: an inventory that cannot be read narrows the sweep; it does not
// fail it, and it does not license anything.
func TestUnreadableInventoryDoesNotAbortOrLicenseAnything(t *testing.T) {
	rt := newFakeRuntime(owned("ao-reviewer-1", "$1", "ao-reviewer:rv-1"))
	rt.listErr = errors.New("tmux is not answering")

	report, err := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatalf("an unreadable inventory failed the sweep: %v", err)
	}
	if len(report.Findings) != 0 || len(rt.destroyed) != 0 {
		t.Fatalf("an unreadable inventory produced findings or destroyed something: %+v %v", report.Findings, rt.destroyed)
	}
}

// Matrix 41/42: capacity and GC ordering. A runtime is destroyed only after
// its authority is safe, and a release does not create a runtime AO then
// wrongly protects.
func TestReleaseAndGCOrderingCannotProduceAFalseFreeSlot(t *testing.T) {
	rt := newFakeRuntime(owned("ao-worker-1", "$50", "ao-worker:w-1"))
	// The claim is RELEASED but its run is still running: the runtime is not
	// GC's to take, because the run may still be using what it started.
	released := domain.CapacityClaim{
		ID: "cap-50", Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimReleased,
		WorkflowRunID: "wf-50", RuntimeHandle: "ao-worker-1", RuntimeInstanceID: "$50",
		DispatchKey: "k50", LifecycleGeneration: 1,
	}
	claims := &fakeClaims{outstanding: nil, held: nil}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{
		"wf-50": {ID: "wf-50", State: domain.WorkflowRunRunning},
	}}
	_ = released

	// With no outstanding claim naming it, the runtime is only reachable
	// through the inventory -- where its ownership token makes it a candidate.
	// That is correct and deliberate: the claim is gone, so nothing is paying
	// for it, and an AO-owned session nothing is paying for IS an orphan.
	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$50"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("an AO-owned runtime with no claim paying for it was %s, want cleaned: %+v", got.Disposition, got)
	}

	// And the reverse: while the claim IS held, the same runtime is protected
	// even though the inventory would otherwise call it an orphan.
	rt2 := newFakeRuntime(owned("ao-worker-1", "$51", "ao-worker:w-1"))
	heldClaim := released
	heldClaim.State = domain.CapacityClaimHeld
	heldClaim.RuntimeInstanceID = "$51"
	claims2 := &fakeClaims{held: []domain.CapacityClaim{heldClaim}, outstanding: []domain.CapacityClaim{heldClaim}}
	report2, err := sweeper(rt2, claims2, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report2, "$51"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a runtime with a held claim was %s, want live", got.Disposition)
	}
	if !rt2.alive("$51") {
		t.Fatal("a runtime whose claim was held got destroyed")
	}
}

// Matrix 43/44/45: reviewer, worker and repair runtimes are all reclaimed by
// the same predicates -- there is no per-kind exception.
func TestEveryExecutionKindIsReclaimedBySameProof(t *testing.T) {
	rt := newFakeRuntime(
		owned("ao-reviewer-x", "$60", "ao-reviewer:rv-x"),
		owned("ao-worker-x", "$61", "ao-worker:w-x"),
		owned("ao-repair-x", "$62", "ao-repair:r-x"),
	)
	var outstanding []domain.CapacityClaim
	for i, kind := range []domain.ExecutionKind{
		domain.ExecutionKindReviewer, domain.ExecutionKindWorker, domain.ExecutionKindRepair,
	} {
		outstanding = append(outstanding, domain.CapacityClaim{
			ID: "cap-" + string(kind), Kind: kind, State: domain.CapacityClaimHeld,
			WorkflowRunID: "wf-60", RuntimeInstanceID: []string{"$60", "$61", "$62"}[i],
			RuntimeHandle: []string{"ao-reviewer-x", "ao-worker-x", "ao-repair-x"}[i],
			DispatchKey:   "k" + string(kind), LifecycleGeneration: 1,
		})
	}
	claims := &fakeClaims{held: outstanding, outstanding: outstanding}
	runs := &fakeRuns{runs: map[string]domain.WorkflowRun{
		"wf-60": {ID: "wf-60", State: domain.WorkflowRunCompleted},
	}}

	report, err := sweeper(rt, claims, runs).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 3 {
		t.Fatalf("cleaned %d of 3 finished runtimes: %+v", report.Cleaned, report.Findings)
	}
}

// Matrix 46: a legacy runtime with no ownership proof fails closed, even
// though it is on AO's own server.
func TestLegacyRuntimeWithoutOwnershipProofFailsClosed(t *testing.T) {
	rt := newFakeRuntime(unowned("ao-legacy-session", "$70"))
	report, err := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	got := findingFor(t, report, "$70")
	if got.Disposition != runtimegc.DispositionUnprovable || got.Class != runtimegc.OrphanUnprovableOwnership {
		t.Fatalf("a legacy session with no ownership proof was %s/%s, want unprovable: %+v", got.Disposition, got.Class, got)
	}
	if !rt.alive("$70") {
		t.Fatal("a legacy session AO could not prove it owned was destroyed")
	}
}

// A sweeper with no facts reader can classify but must never claim to have
// cleaned anything.
func TestSweeperWithoutFactsReaderCleansNothing(t *testing.T) {
	rt := newFakeRuntime(owned("ao-reviewer-1", "$80", "ao-reviewer:rv-1"))
	s := &runtimegc.Sweeper{Inventory: rt, Claims: &fakeClaims{}, Runs: &fakeRuns{runs: map[string]domain.WorkflowRun{}}}
	report, err := s.Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 || !rt.alive("$80") {
		t.Fatalf("a sweeper with no way to prove anything cleaned %d: %+v", report.Cleaned, report)
	}
}
