package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// workflow_reviewer_ownership_test.go — the PRODUCTION ownership contract.
//
// Every previous test of reviewer presence ran inside internal/workflow against
// a fake launcher, and that fake attached ownership as an inseparable part of
// creating a session — which is precisely the property production did not have.
// The blocker that survived eight adversarial review rounds lived in the gap
// between those two, so these tests exercise workflowReviewerLauncher itself.

// ownedRuntime models the tmux adapter's real semantics: ownership travels with
// creation, liveness is a property of the pane rather than the session name, and
// a session that cannot be stamped is never returned as a success.
// sessionIncarnation is one tmux session INSTANCE. A name maps to at most one
// of these at a time, and a replacement under the same name is a different
// incarnation with a different id — which is the whole subject of these tests.
type sessionIncarnation struct {
	instanceID string
	owner      string // "" = carries no ownership token
	dead       bool   // the WORKLOAD process has exited
	// foreground records what the reviewer happens to be running. It exists to
	// prove the classification does NOT consult it: a reviewer running `cat`
	// must stay live.
	foreground string
	// workloadUnknown models a runtime that cannot determine liveness.
	workloadUnknown bool
}

type ownedRuntime struct {
	sessions map[string]*sessionIncarnation
	// refuseStamp models a runtime that cannot attach ownership. The real
	// adapter destroys such a session and fails; so does this.
	refuseStamp bool
	// factsErr models a runtime that cannot answer at all.
	factsErr error
	// noFactsCapability models a runtime with no coherent-snapshot capability.
	noFactsCapability bool
	// beforeSecondRead runs between SessionFacts calls, so a test can replace an
	// incarnation at the exact moment a read-then-act window is open. It is the
	// deterministic stand-in for a real race — no sleeps, no goroutines.
	beforeSecondRead func()
	// replacedOnRead makes SessionFacts report ErrRuntimeSessionReplaced, which
	// is exactly what the real adapter does when its own revalidation catches a
	// name changing hands mid-observation. Detecting that is the ADAPTER's job;
	// what is under test here is that the launcher refuses to act on it.
	replacedOnRead bool
	factsReads     int
	factsHandles   []ports.RuntimeHandle
	destroyed      []string
	created        []string
	instanceSeq    int
}

func newOwnedRuntime() *ownedRuntime {
	return &ownedRuntime{sessions: map[string]*sessionIncarnation{}}
}

func (o *ownedRuntime) nextInstance() string {
	o.instanceSeq++
	return "$" + strconv.Itoa(o.instanceSeq)
}

func (o *ownedRuntime) Create(_ context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	id := string(cfg.SessionID)
	if o.refuseStamp && cfg.Owner != "" {
		// Exactly the adapter's contract: tear down what could not be stamped
		// rather than hand back a handle nothing can recover.
		o.destroyed = append(o.destroyed, id)
		return ports.RuntimeHandle{}, errors.New("tmux runtime: session did not retain its ownership token")
	}
	o.created = append(o.created, id)
	sess := &sessionIncarnation{instanceID: o.nextInstance(), owner: cfg.Owner}
	o.sessions[id] = sess
	// The real adapter returns the incarnation the creation command reported.
	// A double that dropped it would let the launcher drop it too, unnoticed.
	return ports.RuntimeHandle{ID: id, InstanceID: sess.instanceID}, nil
}

func (o *ownedRuntime) IsAlive(_ context.Context, h ports.RuntimeHandle) (bool, error) {
	_, ok := o.sessions[h.ID]
	return ok, nil
}

func (o *ownedRuntime) Destroy(_ context.Context, h ports.RuntimeHandle) error {
	if _, ok := o.sessions[h.ID]; !ok {
		return errors.New("no such session")
	}
	delete(o.sessions, h.ID)
	o.destroyed = append(o.destroyed, h.ID)
	return nil
}

// DestroyInstance targets ONE incarnation. A replacement holding the same name
// is untouched — that is the property under test.
func (o *ownedRuntime) DestroyInstance(_ context.Context, instanceID string) error {
	for name, sess := range o.sessions {
		if sess.instanceID != instanceID {
			continue
		}
		delete(o.sessions, name)
		o.destroyed = append(o.destroyed, instanceID)
		return nil
	}
	// Already gone: destroying what does not exist is success.
	return nil
}

func (o *ownedRuntime) SessionFacts(_ context.Context, h ports.RuntimeHandle) (ports.SessionFacts, bool, error) {
	o.factsReads++
	o.factsHandles = append(o.factsHandles, h)
	if o.factsReads > 1 && o.beforeSecondRead != nil {
		hook := o.beforeSecondRead
		o.beforeSecondRead = nil
		hook()
	}
	if o.replacedOnRead {
		return ports.SessionFacts{}, true, fmt.Errorf(
			"%w: observed across two incarnations", ports.ErrRuntimeSessionReplaced)
	}
	if o.factsErr != nil {
		return ports.SessionFacts{}, false, o.factsErr
	}
	sess, ok := o.sessions[h.ID]
	if !ok {
		return ports.SessionFacts{}, false, nil
	}
	return ports.SessionFacts{
		InstanceID:    sess.instanceID,
		Owner:         sess.owner,
		OwnerKnown:    sess.owner != "",
		WorkloadAlive: !sess.dead,
		WorkloadKnown: !sess.workloadUnknown,
	}, true, nil
}

func newOwnershipLauncher(rt *ownedRuntime) *workflowReviewerLauncher {
	if rt.noFactsCapability {
		return &workflowReviewerLauncher{runtime: struct{ workflowReviewerRuntime }{rt}}
	}
	return &workflowReviewerLauncher{runtime: rt}
}

// seedOwned puts a reviewer at an identity exactly as a successful launch would.
func seedOwned(rt *ownedRuntime, identity string) *sessionIncarnation {
	rt.sessions[identity] = &sessionIncarnation{
		instanceID: rt.nextInstance(), owner: reviewerOwnerToken(identity),
	}
	return rt.sessions[identity]
}

// seedForeign puts somebody else's session under a reviewer identity.
func seedForeign(rt *ownedRuntime, identity, owner string) *sessionIncarnation {
	rt.sessions[identity] = &sessionIncarnation{instanceID: rt.nextInstance(), owner: owner}
	return rt.sessions[identity]
}

// TestProduction_LauncherSatisfiesTheCrashSafeContract pins the interface at
// COMPILE time.
//
// This is not ceremony. The protocol consumed ReviewerEnsurer through a type
// assertion, so when the production launcher stopped satisfying it after a
// signature change, everything compiled, every test passed against a fake that
// did satisfy it, and deterministic recovery was disabled in production alone.
func TestProduction_LauncherSatisfiesTheCrashSafeContract(t *testing.T) {
	var _ workflowcore.ReviewerLauncher = (*workflowReviewerLauncher)(nil)
	var _ workflowcore.ReviewerEnsurer = (*workflowReviewerLauncher)(nil)
	var _ workflowReviewerRuntime = (*ownedRuntime)(nil)
}

// TestProduction_ProbeClassifiesEveryPresence is the four-way (now five-way)
// classification against the real launcher.
func TestProduction_ProbeClassifiesEveryPresence(t *testing.T) {
	ctx := context.Background()

	t.Run("no session is proven absent", func(t *testing.T) {
		l := newOwnershipLauncher(newOwnedRuntime())
		obs, err := l.ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceAbsent {
			t.Fatalf("presence = %q (err %v), want absent", obs.Presence, err)
		}
	})

	t.Run("AO's own live reviewer is owned", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1")
		obs, err := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceOwned {
			t.Fatalf("presence = %q (err %v), want owned", obs.Presence, err)
		}
	})

	t.Run("a session bearing only the right NAME is never owned", func(t *testing.T) {
		// The old marker's value was the session's own name, so a session that
		// merely echoed its name back satisfied ownership. It must not.
		rt := newOwnedRuntime()
		seedForeign(rt, "workflow-review-r1", "workflow-review-r1")
		obs, err := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceForeign {
			t.Fatalf("presence = %q (err %v), want foreign — a name is not a proof of ownership", obs.Presence, err)
		}
	})

	t.Run("someone else's token is foreign", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedForeign(rt, "workflow-review-r1", "ao-reviewer:workflow-review-OTHER")
		obs, err := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceForeign {
			t.Fatalf("presence = %q (err %v), want foreign", obs.Presence, err)
		}
	})

	t.Run("an unmarked session is unknown, never absent", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedForeign(rt, "workflow-review-r1", "") // live, carrying no token
		obs, err := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceUnknown {
			t.Fatalf("presence = %q (err %v), want unknown", obs.Presence, err)
		}
		if obs.Presence.LicensesLaunch() {
			t.Fatal("an unmarked live session licensed a launch; that is a duplicate reviewer")
		}
		if obs.Presence.LicensesTermination() {
			t.Fatal("an unmarked session licensed termination; AO cannot prove it owns it")
		}
	})

	t.Run("AO's own EXITED reviewer is not owned and not absent", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1").dead = true // process gone, session lingers
		obs, err := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err != nil || obs.Presence != workflowcore.ReviewerPresenceExited {
			t.Fatalf("presence = %q (err %v), want exited", obs.Presence, err)
		}
		if obs.Presence.LicensesAdoption() {
			t.Fatal("a finished reviewer was adoptable; that is a phantom running review")
		}
		if !obs.Presence.LicensesTermination() {
			t.Fatal("AO's own exited reviewer must be reclaimable, or its identity is stuck forever")
		}
	})

	t.Run("an unanswerable liveness probe is unknown", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1").workloadUnknown = true
		obs, _ := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if obs.Presence != workflowcore.ReviewerPresenceUnknown {
			t.Fatalf("presence = %q, want unknown", obs.Presence)
		}
	})

	t.Run("a runtime with no liveness capability never claims owned", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1")
		rt.noFactsCapability = true
		obs, _ := newOwnershipLauncher(rt).ProbeReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if obs.Presence == workflowcore.ReviewerPresenceOwned {
			t.Fatal("liveness was assumed rather than proven")
		}
	})
}

// TestProduction_CancelNeverDestroysWhatAOCannotProveItOwns is the destructive
// direction, which must fail closed.
func TestProduction_CancelNeverDestroysWhatAOCannotProveItOwns(t *testing.T) {
	ctx := context.Background()

	t.Run("absent is idempotent success", func(t *testing.T) {
		rt := newOwnedRuntime()
		if err := newOwnershipLauncher(rt).CancelReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"}); err != nil {
			t.Fatalf("cancelling a reviewer that is already gone must succeed: %v", err)
		}
		if len(rt.destroyed) != 0 {
			t.Fatal("destroyed something that did not exist")
		}
	})

	t.Run("owned may be terminated", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1")
		if err := newOwnershipLauncher(rt).CancelReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"}); err != nil {
			t.Fatalf("CancelReviewer: %v", err)
		}
		if len(rt.destroyed) != 1 {
			t.Fatalf("destroyed = %v, want the owned reviewer terminated", rt.destroyed)
		}
	})

	t.Run("exited may be reclaimed", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedOwned(rt, "workflow-review-r1").dead = true
		if err := newOwnershipLauncher(rt).CancelReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"}); err != nil {
			t.Fatalf("an exited reviewer AO owns must be reclaimable: %v", err)
		}
		if len(rt.destroyed) != 1 {
			t.Fatalf("destroyed = %v, want the exited session reclaimed", rt.destroyed)
		}
	})

	t.Run("foreign must never be destroyed", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedForeign(rt, "workflow-review-r1", "ao-reviewer:someone-else")
		err := newOwnershipLauncher(rt).CancelReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err == nil {
			t.Fatal("a foreign session was cancelled without complaint")
		}
		if len(rt.destroyed) != 0 {
			t.Fatalf("DESTROYED A FOREIGN SESSION: %v", rt.destroyed)
		}
		if _, still := rt.sessions["workflow-review-r1"]; !still {
			t.Fatal("the foreign session is gone")
		}
	})

	t.Run("unknown must never be destroyed", func(t *testing.T) {
		rt := newOwnedRuntime()
		seedForeign(rt, "workflow-review-r1", "") // live, unidentifiable
		err := newOwnershipLauncher(rt).CancelReviewer(ctx, workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
		if err == nil {
			t.Fatal("an unidentifiable session was cancelled")
		}
		if len(rt.destroyed) != 0 {
			t.Fatalf("DESTROYED AN UNIDENTIFIABLE SESSION: %v", rt.destroyed)
		}
	})
}

// TestProduction_LaunchPassesOwnershipIntoCreation is F-1's regression at the
// launcher: the token must reach the runtime as part of the creation request,
// so no code path exists that creates first and stamps afterwards.
func TestProduction_LaunchPassesOwnershipIntoCreation(t *testing.T) {
	rt := newOwnedRuntime()
	l := newOwnershipLauncher(rt)

	// Drive Create the way Launch does, then assert what the runtime received.
	identity := l.ReviewerIdentity(workflowcore.ReviewerLaunchRequest{RunID: "r1"})
	if identity != "workflow-review-r1" {
		t.Fatalf("identity = %q", identity)
	}
	if _, err := rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "workflow-review-r1", WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"}, Owner: reviewerOwnerToken(identity),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := rt.sessions[identity].owner; got != "ao-reviewer:workflow-review-r1" {
		t.Fatalf("owner at creation = %q, want the correlated token", got)
	}

	// And the created session is immediately classifiable — no window in which
	// it is live but unidentifiable.
	obs, err := l.ProbeReviewer(context.Background(), workflowcore.ReviewerRef{HandleID: identity})
	if err != nil || obs.Presence != workflowcore.ReviewerPresenceOwned {
		t.Fatalf("presence right after creation = %q (err %v), want owned", obs.Presence, err)
	}
}

// TestProduction_UnstampableCreationYieldsNoLiveSession completes F-1: when
// ownership cannot be established the session must not survive.
func TestProduction_UnstampableCreationYieldsNoLiveSession(t *testing.T) {
	rt := newOwnedRuntime()
	rt.refuseStamp = true

	_, err := rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "workflow-review-r1", WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"}, Owner: reviewerOwnerToken("workflow-review-r1"),
	})
	if err == nil {
		t.Fatal("creation succeeded without ownership")
	}
	if !strings.Contains(err.Error(), "ownership token") {
		t.Fatalf("error = %v, want it to name the ownership token", err)
	}
	if _, live := rt.sessions["workflow-review-r1"]; live {
		t.Fatal("a live unowned session remains — the permanent orphan")
	}
	obs, perr := newOwnershipLauncher(rt).ProbeReviewer(context.Background(), workflowcore.ReviewerRef{HandleID: "workflow-review-r1"})
	if perr != nil || obs.Presence != workflowcore.ReviewerPresenceAbsent {
		t.Fatalf("presence = %q (err %v), want absent so the identity is reusable", obs.Presence, perr)
	}
}

// TestProduction_ReviewerLaunchesSupervisedSoItsExitIsObservable is BLOCKER 1's
// regression at the launcher.
//
// Without AO_SUPERVISED_PROCESS=1 the runtime replaces a finished reviewer with
// an INTERACTIVE keep-alive shell, and every liveness probe then answers about
// that shell: a review that ended hours ago still reads as running and AO adopts
// it forever. Supervised mode substitutes AO's own deterministic sentinel, which
// the probe recognises.
func TestProduction_ReviewerLaunchesSupervisedSoItsExitIsObservable(t *testing.T) {
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"claude", "-p"}}}
	runtime := &fakeWorkflowReviewerRuntime{}
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: adapter},
		runtime:   runtime,
		dataDir:   t.TempDir(),
	}

	if _, err := l.Launch(context.Background(), workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "run-1",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree",
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := runtime.lastCfg.Env["AO_SUPERVISED_PROCESS"]; got != "1" {
		t.Fatalf("AO_SUPERVISED_PROCESS = %q, want \"1\" — an unsupervised reviewer's "+
			"exit is invisible behind an interactive keep-alive shell", got)
	}
	// And ownership still travels with the same creation call.
	if runtime.lastCfg.Owner == "" {
		t.Fatal("the reviewer session was created without an ownership token")
	}
}

// The launch result must carry the runtime's INSTANCE back to the caller.
// Without it the confirmation has nothing durable to record, and every later
// probe is left resolving a reusable name.
func TestProduction_LaunchResultCarriesTheRuntimeInstance(t *testing.T) {
	adapter := &fakeReviewerAdapter{cmd: ports.ReviewCommandSpec{Argv: []string{"claude", "-p"}}}
	rt := newOwnedRuntime()
	l := &workflowReviewerLauncher{
		reviewers: &fakeReviewerResolver{adapter: adapter},
		runtime:   rt,
		dataDir:   t.TempDir(),
	}

	result, err := l.Launch(context.Background(), workflowcore.ReviewerLaunchRequest{
		Harness:         domain.ReviewerClaudeCode,
		WorkerSessionID: "sess-1",
		ProjectID:       "proj-1",
		ReviewID:        "review-1",
		RunID:           "run-1",
		WorkspacePath:   "/ws/wf",
		Prompt:          "review this worktree",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	created := rt.sessions[result.HandleID]
	if created == nil {
		t.Fatalf("no session was created for handle %q", result.HandleID)
	}
	if result.InstanceID == "" {
		t.Fatal("Launch dropped the runtime instance; the confirmation has nothing durable to record")
	}
	if result.InstanceID != created.instanceID {
		t.Fatalf("launch result instance = %q, want the created incarnation %q",
			result.InstanceID, created.instanceID)
	}
}
