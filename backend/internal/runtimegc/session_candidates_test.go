package runtimegc_test

// The session-derived candidate sweep (P1-D §C), which had no test at all —
// which is how the production wiring defect behind incident wf-170b16ce's
// second half shipped unnoticed.
//
// The production shape it reproduces: 25 tmux sessions on AO's own server from
// workflows finished hours and days earlier, and a Runtime GC that had swept at
// boot and every 15 minutes since without reclaiming one of them. All 67
// session rows carried an empty runtime_instance_id and an empty
// runtime_owner_token, so sessionCandidates skipped every single one as
// "pre-P1-D" — including sessions created that same evening by a daemon built
// from the P1-D commit. The two tests at the end are the ones that would have
// caught it: they assert on the SHAPE of a row this sweep can act on, so a
// persistence path that silently stops producing that shape fails here.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

type fakeSessions struct {
	sessions []domain.SessionRecord
	err      error
}

func (f *fakeSessions) ListAllSessions(context.Context) ([]domain.SessionRecord, error) {
	return f.sessions, f.err
}

// ownedSession is a P1-D session record: AO created this exact incarnation for
// this launch, and the token proves it.
func ownedSession(id, instance, launch string, terminated bool) domain.SessionRecord {
	return domain.SessionRecord{
		ID: domain.SessionID(id), ProjectID: "agent-orchestrator", IsTerminated: terminated,
		Metadata: domain.SessionMetadata{
			RuntimeHandleID:   id,
			RuntimeInstanceID: instance,
			RuntimeLaunchID:   launch,
			RuntimeOwnerToken: domain.SessionRuntimeOwnerToken(domain.SessionID(id), launch),
		},
	}
}

// legacySession is what every row in the incident database looked like: a real
// session with no recorded incarnation and no recorded token.
func legacySession(id string, terminated bool) domain.SessionRecord {
	return domain.SessionRecord{
		ID: domain.SessionID(id), ProjectID: "agent-orchestrator", IsTerminated: terminated,
		Metadata: domain.SessionMetadata{RuntimeHandleID: id},
	}
}

func sessionSweeper(rt *fakeRuntime, sessions *fakeSessions) *runtimegc.Sweeper {
	s := sweeper(rt, &fakeClaims{}, &fakeRuns{runs: map[string]domain.WorkflowRun{}})
	s.Sessions = sessions
	return s
}

// A terminated session whose runtime AO can prove is its own is reclaimed.
// This is the worker case P1-C could not do at all and P1-D added.
func TestTerminatedOwnedSessionRuntimeIsReclaimed(t *testing.T) {
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-51", InstanceID: "$42",
		Owner:      domain.SessionRuntimeOwnerToken("agent-orchestrator-51", "launch-1"),
		OwnerKnown: true,
	})
	sessions := &fakeSessions{sessions: []domain.SessionRecord{
		ownedSession("agent-orchestrator-51", "$42", "launch-1", true),
	}}

	report, err := sessionSweeper(rt, sessions).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	got := findingFor(t, report, "$42")
	if got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("a terminated session AO provably owns was %s/%s (%s), want cleaned",
			got.Disposition, got.Class, got.Reason)
	}
	if rt.alive("$42") {
		t.Fatal("the runtime of a terminated, provably-owned session survived the sweep")
	}
}

// A session that is NOT terminated is live work. Age is not evidence.
func TestLiveOwnedSessionRuntimeIsNeverReclaimed(t *testing.T) {
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-52", InstanceID: "$43",
		Owner:      domain.SessionRuntimeOwnerToken("agent-orchestrator-52", "launch-1"),
		OwnerKnown: true,
	})
	sessions := &fakeSessions{sessions: []domain.SessionRecord{
		ownedSession("agent-orchestrator-52", "$43", "launch-1", false),
	}}

	report, err := sessionSweeper(rt, sessions).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := findingFor(t, report, "$43"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a live session was %s, want live", got.Disposition)
	}
	if !rt.alive("$43") {
		t.Fatal("destroyed the runtime of a session AO records as running")
	}
}

// A token that does not match the launch it is recorded against describes an
// incarnation AO cannot name. Unprovable, never destroyed.
func TestSessionWithMismatchedOwnershipTokenIsUnprovable(t *testing.T) {
	rt := newFakeRuntime(ports.RuntimeSessionSummary{ID: "agent-orchestrator-53", InstanceID: "$44",
		Owner: domain.SessionRuntimeOwnerToken("agent-orchestrator-53", "launch-2"), OwnerKnown: true})
	rec := ownedSession("agent-orchestrator-53", "$44", "launch-1", true)
	rec.Metadata.RuntimeOwnerToken = domain.SessionRuntimeOwnerToken("agent-orchestrator-53", "launch-9")
	sessions := &fakeSessions{sessions: []domain.SessionRecord{rec}}

	report, err := sessionSweeper(rt, sessions).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	got := findingFor(t, report, "$44")
	if got.Disposition != runtimegc.DispositionUnprovable {
		t.Fatalf("a session whose token contradicts its launch was %s, want unprovable", got.Disposition)
	}
	if !rt.alive("$44") {
		t.Fatal("destroyed a runtime whose ownership AO could not establish")
	}
}

// THE PRODUCTION SHAPE. Ten genuinely legacy sessions with no ownership proof,
// beside one the current code path provably owns. The legacy rows must be left
// strictly alone, and must not stop the provable one from being reclaimed —
// "one broken legacy candidate must not block cleanup of newer provably-owned
// candidates".
func TestLegacySessionsDoNotBlockReclaimingAProvableOne(t *testing.T) {
	summaries := []ports.RuntimeSessionSummary{{
		ID: "agent-orchestrator-51", InstanceID: "$42",
		Owner:      domain.SessionRuntimeOwnerToken("agent-orchestrator-51", "launch-1"),
		OwnerKnown: true,
	}}
	records := []domain.SessionRecord{ownedSession("agent-orchestrator-51", "$42", "launch-1", true)}
	legacyInstances := []string{}
	for i, name := range []string{
		"agent-orchestrator-40", "agent-orchestrator-41", "agent-orchestrator-42",
		"agent-orchestrator-43", "agent-orchestrator-45", "medusa-3", "medusa-4",
		"medusa-5", "medusa-6", "medusa-7",
	} {
		instance := "$" + string(rune('a'+i))
		legacyInstances = append(legacyInstances, instance)
		summaries = append(summaries, unowned(name, instance))
		records = append(records, legacySession(name, true))
	}
	rt := newFakeRuntime(summaries...)

	report, err := sessionSweeper(rt, &fakeSessions{sessions: records}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatalf("legacy sessions aborted the sweep: %v", err)
	}
	if got := findingFor(t, report, "$42"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("the provable candidate was %s (%s), want cleaned — ten unprovable neighbours must not block it",
			got.Disposition, got.Reason)
	}
	if rt.alive("$42") {
		t.Fatal("the provably-owned terminal runtime survived")
	}
	for _, instance := range legacyInstances {
		if !rt.alive(instance) {
			t.Fatalf("legacy session %s was destroyed without ownership proof", instance)
		}
		if got := findingFor(t, report, instance); got.Disposition != runtimegc.DispositionUnprovable {
			t.Fatalf("legacy session %s was %s, want unprovable", instance, got.Disposition)
		}
	}
	// Every legacy session is REPORTED, because an orphan nobody can see is how
	// orphans become permanent.
	if report.SkippedUnprovable != len(legacyInstances) {
		t.Fatalf("unprovable count = %d, want %d", report.SkippedUnprovable, len(legacyInstances))
	}
}

// ---- the defect itself ------------------------------------------------------

// A terminated session with an empty incarnation is exactly what every row in
// the incident database looked like, and the sweep can do nothing with it. This
// test does not assert a bug — it pins the CONSEQUENCE, so the two tests below
// have something to be measured against.
func TestSessionWithNoRecordedIncarnationYieldsNoCandidate(t *testing.T) {
	rt := newFakeRuntime(unowned("agent-orchestrator-40", "$1"))
	report, err := sessionSweeper(rt, &fakeSessions{
		sessions: []domain.SessionRecord{legacySession("agent-orchestrator-40", true)},
	}).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 0 || !rt.alive("$1") {
		t.Fatal("reclaimed a runtime on the strength of a session name alone")
	}
}

// The two routes to reclaiming a finished worker, and what each one needs.
//
// The recorded identity is not the only proof AO has: a session whose tmux
// marker is readable can also be reclaimed by the inventory pass, on the
// ownership token alone. That is why the leak in production was NOT simply
// "the columns were empty" — the leaked sessions were old enough to carry no
// marker either, so both routes were closed at once.
//
// What the recorded identity uniquely provides is the LIVENESS guard, which is
// the subject of the next test.
func TestTerminatedSessionIsReclaimableByEitherProof(t *testing.T) {
	full := ownedSession("agent-orchestrator-51", "$42", "launch-1", true)

	t.Run("recorded identity alone, marker unreadable", func(t *testing.T) {
		rt := newFakeRuntime(unowned("agent-orchestrator-51", "$42"))
		report, err := sessionSweeper(rt, &fakeSessions{sessions: []domain.SessionRecord{full}}).
			Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if report.Cleaned != 1 {
			t.Fatalf("cleaned = %d, want 1: the recorded identity is proof on its own", report.Cleaned)
		}
	})

	t.Run("marker alone, identity unrecorded", func(t *testing.T) {
		rec := full
		rec.Metadata.RuntimeInstanceID, rec.Metadata.RuntimeOwnerToken = "", ""
		rt := newFakeRuntime(ports.RuntimeSessionSummary{
			ID: "agent-orchestrator-51", InstanceID: "$42",
			Owner: full.Metadata.RuntimeOwnerToken, OwnerKnown: true,
		})
		report, err := sessionSweeper(rt, &fakeSessions{sessions: []domain.SessionRecord{rec}}).
			Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if report.Cleaned != 1 {
			t.Fatalf("cleaned = %d, want 1: a readable ownership marker is proof on its own", report.Cleaned)
		}
	})

	t.Run("neither proof: the incident's sessions", func(t *testing.T) {
		rec := full
		rec.Metadata.RuntimeInstanceID, rec.Metadata.RuntimeOwnerToken = "", ""
		rt := newFakeRuntime(unowned("agent-orchestrator-51", "$42"))
		report, err := sessionSweeper(rt, &fakeSessions{sessions: []domain.SessionRecord{rec}}).
			Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if report.Cleaned != 0 || !rt.alive("$42") {
			t.Fatal("reclaimed a runtime with neither a recorded identity nor a readable marker")
		}
	})
}

// ---- the protective half ----------------------------------------------------

// A live AO-owned session with no capacity claim — an interactive session a
// person is using, which never has one — must survive a sweep.
//
// This is the other, more dangerous consequence of the same empty columns.
// sessionCandidates is the ONLY pass that consults IsTerminated, and it needs
// both halves of the recorded identity to produce a finding at all. With them
// missing it produces nothing, so the instance never enters the sweep's `seen`
// set, and inventoryCandidates — which checks the ownership token and the
// capacity claims and NOTHING about terminality — classifies the very same
// session as OrphanUnreferenced, which is auto-cleanable.
//
// So the empty columns did not only stop GC reclaiming finished work; they
// removed the evidence that protects unfinished work. Production was shielded
// from that only because the sessions that had accumulated were old enough to
// carry no ownership marker either.
func TestLiveOwnedSessionWithNoClaimSurvivesTheInventorySweep(t *testing.T) {
	token := domain.SessionRuntimeOwnerToken("agent-orchestrator-60", "launch-1")
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-60", InstanceID: "$60", Owner: token, OwnerKnown: true,
	})
	// The durable truth: this session is running, and no claim pays for it.
	sessions := &fakeSessions{sessions: []domain.SessionRecord{
		ownedSession("agent-orchestrator-60", "$60", "launch-1", false),
	}}

	report, err := sessionSweeper(rt, sessions).Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.alive("$60") {
		t.Fatal("destroyed a live session that no capacity claim happened to reference")
	}
	if got := findingFor(t, report, "$60"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a live session was %s/%s (%s), want live", got.Disposition, got.Class, got.Reason)
	}
}

// The same live session, with the recorded identity MISSING — the production
// row shape. It must still survive, because the liveness guard reads the
// session record's terminality by NAME and does not depend on the identity
// columns at all.
//
// Without that guard this session is destroyed: it has a readable ownership
// token, no capacity claim (interactive sessions never have one), and produces
// no sessionCandidates finding to put it in the sweep's `seen` set, so the
// inventory pass calls it unreferenced and reclaims it. That is a live agent
// killed under a person mid-conversation, within 15 minutes of the sweep.
func TestLiveSessionWithNoRecordedIdentityStillSurvives(t *testing.T) {
	token := domain.SessionRuntimeOwnerToken("agent-orchestrator-61", "launch-1")
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-61", InstanceID: "$61", Owner: token, OwnerKnown: true,
	})
	rec := legacySession("agent-orchestrator-61", false) // running, identity unrecorded

	report, err := sessionSweeper(rt, &fakeSessions{sessions: []domain.SessionRecord{rec}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !rt.alive("$61") {
		t.Fatal("destroyed a running session because its row recorded no runtime identity")
	}
	if got := findingFor(t, report, "$61"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a running session was %s/%s (%s), want live", got.Disposition, got.Class, got.Reason)
	}
}

// The guard protects; it must never license. A session AO records as
// TERMINATED is still reclaimed on the same proofs as before.
func TestLivenessGuardDoesNotProtectATerminatedSession(t *testing.T) {
	token := domain.SessionRuntimeOwnerToken("agent-orchestrator-62", "launch-1")
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-62", InstanceID: "$62", Owner: token, OwnerKnown: true,
	})
	rec := legacySession("agent-orchestrator-62", true)

	report, err := sessionSweeper(rt, &fakeSessions{sessions: []domain.SessionRecord{rec}}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 1 || rt.alive("$62") {
		t.Fatalf("a terminated owned session was not reclaimed: %+v", report.Findings)
	}
}

// A session AO has no record of at all is unchanged: the guard adds no
// protection it cannot justify, and the ownership token still decides.
func TestLivenessGuardIsSilentAboutUnknownSessions(t *testing.T) {
	token := domain.SessionRuntimeOwnerToken("agent-orchestrator-63", "launch-1")
	rt := newFakeRuntime(ports.RuntimeSessionSummary{
		ID: "agent-orchestrator-63", InstanceID: "$63", Owner: token, OwnerKnown: true,
	})

	report, err := sessionSweeper(rt, &fakeSessions{}).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Cleaned != 1 {
		t.Fatalf("cleaned = %d, want 1: an owned session with no durable record is still an orphan", report.Cleaned)
	}
}
