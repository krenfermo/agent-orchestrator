package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// ownership_test.go — the tmux side of AO's ownership proof, against a SCRIPTED
// tmux rather than a call-counting queue.
//
// The distinction matters. Every previous test of this boundary ran against a
// fake that answered whatever the test wanted next, which cannot express the one
// property the ownership model rests on: that a session and its ownership token
// become visible in the SAME operation. The runner below models tmux's actual
// semantics — sessions carry environment, panes carry a dead flag, unset
// variables exit non-zero — so the ordering is testable rather than assumed.

type scriptedSession struct {
	env map[string]string
	// instanceID is tmux's `#{session_id}` — the `$N` handle allocated once per
	// session and never reused. A replacement under the same NAME gets a new
	// one, which is the only way AO can tell two incarnations apart.
	instanceID string
	// panePID is tmux's pane_pid: the ROOT process of the pane, i.e. the
	// `$SHELL -c "<launch command>"` AO started.
	panePID int
	// rootCommand is that root process's own command line. It starts as the
	// launch shell and, when the workload exits, is REPLACED IN PLACE by the
	// keep-alive the launch command execs — same pid, new command, no children.
	rootCommand string
	// children are the live child processes of panePID. The workload is one of
	// them; anything the workload itself runs is a grandchild and is modelled
	// too, because the whole point is that a reviewer running `cat` must not be
	// confused with AO's `cat` keep-alive.
	children []processEntry
}

type scriptedTmux struct {
	sessions map[string]*scriptedSession
	// dropOwnerEnv models a tmux that does not honour `new-session -e` (an older
	// server, a dropped variable): the session is created, the token is not.
	dropOwnerEnv bool
	// serverDown models an unreachable server: nothing can be learned at all.
	serverDown bool
	// refuseKill models a teardown that fails, leaving the session running.
	refuseKill bool
	// downAfterOwnerRead models the server disappearing mid-teardown, so AO
	// cannot determine whether it destroyed anything.
	downAfterOwnerRead bool
	// psErr models an unreadable process table: uncertainty, never death.
	// afterCreate fires the instant new-session returns, so a test can replace
	// the session before AO's NEXT command — the window a name-based
	// created-instance lookup would fall into.
	afterCreate func()
	// afterPanePIDRead fires once the pane pid has been answered, so a test can
	// destroy the instance while its processes linger in the global table.
	afterPanePIDRead func()
	// stale are processes that remain visible after their session is gone.
	stale []processEntry
	// garbleCreateOutput models new-session succeeding without reporting a
	// usable session id.
	garbleCreateOutput bool
	// panePIDText, when set, is what `#{pane_pid}` answers instead of the
	// session's real pid. It models the two shapes tmux actually produces for a
	// pane AO cannot inspect: an EMPTY value (a session whose active pane is
	// being torn down, the state behind the production `invalid pane pid ""`
	// failure) and a non-numeric one.
	panePIDText *string
	// afterOwnerRead fires once the ownership read has been answered, so a test
	// can restore the original incarnation and produce a true ABA: the name is
	// surrendered, a stranger answers one query, and the original returns before
	// the next one.
	afterOwnerRead func()
	// beforeReadiness fires when the final liveness probe is issued, which is
	// the last point at which a replacement could still be handed back as a
	// successful Create.
	beforeReadiness func()
	psErr           bool
	pidSeq          int
	instanceSeq     int
	beforeNextRead  func()
	destroyed       []string
}

func newScriptedTmux() *scriptedTmux {
	return &scriptedTmux{sessions: map[string]*scriptedSession{}}
}

func (s *scriptedTmux) nextPID() int {
	s.pidSeq += 1000
	return s.pidSeq
}

func (s *scriptedTmux) nextInstance() string {
	s.instanceSeq++
	return "$" + strconv.Itoa(s.instanceSeq)
}

// replaceSession models a NAME changing hands: the old incarnation is gone and a
// new one — a different session entirely — now answers to the same name.
func (s *scriptedTmux) replaceSession(id, owner string) *scriptedSession {
	sess := &scriptedSession{
		env: map[string]string{}, panePID: s.nextPID(), instanceID: s.nextInstance(),
		rootCommand: "/bin/sh -c stranger",
	}
	if owner != "" {
		sess.env[ownerEnvKey] = owner
	}
	sess.children = []processEntry{{pid: s.nextPID(), ppid: sess.panePID, command: "stranger"}}
	s.sessions[id] = sess
	return sess
}

// psTable renders the whole scripted process table in `ps -ww -axo pid=,ppid=,args=`
// form: every session's root process plus its live descendants.
func (s *scriptedTmux) psTable() []byte {
	var b strings.Builder
	for _, sess := range s.sessions {
		fmt.Fprintf(&b, "%d %d %s\n", sess.panePID, 1, sess.rootCommand)
		for _, c := range sess.children {
			fmt.Fprintf(&b, "%d %d %s\n", c.pid, c.ppid, c.command)
		}
	}
	// Processes of sessions that are already gone but still listed.
	for _, c := range s.stale {
		fmt.Fprintf(&b, "%d %d %s\n", c.pid, c.ppid, c.command)
	}
	return []byte(b.String())
}

func (s *scriptedTmux) Run(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	// The process-table probe is not a tmux call at all.
	if name == "ps" {
		if s.psErr {
			return nil, &exec.ExitError{}
		}
		return s.psTable(), nil
	}
	// Strip the `-L <socket>` prefix the runtime always prepends.
	if len(args) >= 2 && args[0] == "-L" {
		args = args[2:]
	}
	if s.serverDown {
		return []byte("no server running on /tmp/tmux-501/ao"), &exec.ExitError{}
	}
	if len(args) == 0 {
		return nil, nil
	}
	// rawTarget is what the command actually addressed; lookup resolves it to a
	// session whether it is a NAME or an instance id.
	rawTarget := func() string {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-t" {
				t := strings.TrimPrefix(args[i+1], "=")
				// Strip any window/pane suffix ("$3:0.0", "name:0.0").
				if j := strings.LastIndex(t, ":"); j > 0 {
					t = t[:j]
				}
				return t
			}
		}
		return ""
	}
	// target returns the NAME of the session a command addressed, resolving an
	// instance id to whichever session currently holds it — and to nothing at
	// all if that incarnation is gone, which is the property under test.
	target := func() string {
		t := rawTarget()
		if !strings.HasPrefix(t, "$") {
			return t
		}
		for name, sess := range s.sessions {
			if sess.instanceID == t {
				return name
			}
		}
		return "\x00missing"
	}
	switch args[0] {
	case "new-session":
		id := ""
		defer func() {}()
		env := map[string]string{}
		for i := 0; i < len(args)-1; i++ {
			switch args[i] {
			case "-s":
				id = args[i+1]
			case "-e":
				k, v, _ := strings.Cut(args[i+1], "=")
				if !s.dropOwnerEnv {
					env[k] = v
				}
			}
		}
		s.sessions[id] = &scriptedSession{
			env: env, panePID: s.nextPID(), instanceID: s.nextInstance(),
			rootCommand: "/bin/sh -c cd /tmp/ws || exit; claude -p review; exec cat >/dev/null",
		}
		// A freshly created session is running its workload.
		s.sessions[id].children = []processEntry{
			{pid: s.nextPID(), ppid: s.sessions[id].panePID, command: "claude -p review"},
		}
		// `-P -F "#{session_id}"` prints the instance id of the new session.
		created := s.sessions[id].instanceID
		if s.garbleCreateOutput {
			created = ""
		}
		if s.afterCreate != nil {
			hook := s.afterCreate
			s.afterCreate = nil
			hook()
		}
		return []byte(created + "\n"), nil
	case "display-message":
		// The pane-pid probe targets "<id>:0.0"; strip the pane suffix.
		name := target()
		if i := strings.Index(name, ":"); i >= 0 {
			name = name[:i]
		}
		sess, ok := s.sessions[name]
		if !ok {
			return []byte("can't find session"), &exec.ExitError{}
		}
		// The replacement hook fires between reads, so a test can swap the
		// session behind a name at exactly the moment AO is combining facts
		// about it. Deterministic: no sleeps, no goroutines.
		defer func() {
			if s.beforeNextRead != nil {
				hook := s.beforeNextRead
				s.beforeNextRead = nil
				hook()
			}
		}()
		switch args[len(args)-1] {
		case "#{pane_pid}":
			if s.afterPanePIDRead != nil {
				hook := s.afterPanePIDRead
				s.afterPanePIDRead = nil
				defer hook()
			}
			if s.panePIDText != nil {
				return []byte(*s.panePIDText + "\n"), nil
			}
			return []byte(strconv.Itoa(sess.panePID) + "\n"), nil
		case "#{session_id}":
			return []byte(sess.instanceID + "\n"), nil
		case "#{session_id}\t#{pane_pid}":
			return []byte(sess.instanceID + "\t" + strconv.Itoa(sess.panePID) + "\n"), nil
		}
		return []byte("/tmp/ws\n"), nil
	case "set-option":
		if _, ok := s.sessions[target()]; !ok {
			return []byte("can't find session"), &exec.ExitError{}
		}
		return nil, nil
	case "has-session":
		if s.beforeReadiness != nil {
			hook := s.beforeReadiness
			s.beforeReadiness = nil
			hook()
		}
		if _, ok := s.sessions[target()]; !ok {
			return []byte("can't find session: " + target()), &exec.ExitError{}
		}
		return nil, nil
	case "show-environment":
		sess, ok := s.sessions[target()]
		if !ok {
			return []byte("can't find session: " + target()), &exec.ExitError{}
		}
		if s.downAfterOwnerRead {
			// Everything after this read is unanswerable.
			defer func() { s.serverDown = true }()
		}
		if s.afterOwnerRead != nil {
			hook := s.afterOwnerRead
			s.afterOwnerRead = nil
			defer hook()
		}
		key := args[len(args)-1]
		v, ok := sess.env[key]
		if !ok {
			return []byte("unknown variable: " + key), &exec.ExitError{}
		}
		return []byte(key + "=" + v + "\n"), nil
	case "list-panes":
		sess, ok := s.sessions[target()]
		if !ok {
			return []byte("can't find session: " + target()), &exec.ExitError{}
		}
		return []byte(strconv.Itoa(sess.panePID) + "\n"), nil
	case "kill-session":
		// An instance-targeted kill ($N) may only reach that exact incarnation.
		if strings.HasPrefix(rawTarget(), "$") {
			if s.refuseKill {
				return []byte("failed to kill session"), &exec.ExitError{}
			}
			for name, sess := range s.sessions {
				if sess.instanceID != rawTarget() {
					continue
				}
				delete(s.sessions, name)
				s.destroyed = append(s.destroyed, rawTarget())
				return nil, nil
			}
			return []byte("can't find session: " + rawTarget()), &exec.ExitError{}
		}
		if s.refuseKill {
			return []byte("failed to kill session"), &exec.ExitError{}
		}
		if _, ok := s.sessions[target()]; !ok {
			return []byte("can't find session"), &exec.ExitError{}
		}
		delete(s.sessions, target())
		s.destroyed = append(s.destroyed, target())
		return nil, nil
	}
	return nil, nil
}

func newScriptedRuntime() (*Runtime, *scriptedTmux) {
	st := newScriptedTmux()
	r := New(Options{Binary: "tmux-test", Timeout: time.Second, Shell: "/bin/sh"})
	r.runner = st
	r.enterDelay = 0
	r.reapSessions = (&recordingReaper{}).reap
	return r, st
}

// TestCreate_OwnershipIsAttachedByTheCreationCommandItself is the invariant the
// whole recovery protocol rests on: there is no instant at which the session
// exists without its ownership token.
func TestCreate_OwnershipIsAttachedByTheCreationCommandItself(t *testing.T) {
	r, st := newScriptedRuntime()

	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     domain.SessionID("rev-1"),
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"echo", "hi"},
		Owner:         "ao-reviewer:rev-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The token must have arrived on the new-session command — not on any later
	// call. A `set-option` afterwards would reintroduce the window this test
	// exists to forbid.
	sess, ok := st.sessions["rev-1"]
	if !ok {
		t.Fatalf("session was not created")
	}
	if got := sess.env["AO_SESSION_OWNER"]; got != "ao-reviewer:rev-1" {
		t.Fatalf("owner env = %q, want ao-reviewer:rev-1 (attached at creation)", got)
	}
	if h.InstanceID != sess.instanceID {
		t.Fatalf("handle InstanceID = %q, want the id the creation command returned (%q)",
			h.InstanceID, sess.instanceID)
	}
	facts, exists, err := r.SessionFacts(context.Background(), h)
	if err != nil || !exists || !facts.OwnerKnown || facts.Owner != "ao-reviewer:rev-1" {
		t.Fatalf("SessionFacts = (%+v, %t, %v), want the ownership token", facts, exists, err)
	}
}

// TestCreate_UnstampableSessionIsDestroyedAndCreationFails is F-1's direct
// regression: a live session AO cannot identify must never be handed back as a
// success, because nothing afterwards can adopt or terminate it.
func TestCreate_UnstampableSessionIsDestroyedAndCreationFails(t *testing.T) {
	r, st := newScriptedRuntime()
	st.dropOwnerEnv = true // the session is created; the token is not retained

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "rev-2",
		WorkspacePath: "/tmp/ws",
		Argv:          []string{"echo", "hi"},
		Owner:         "ao-reviewer:rev-2",
	})
	if err == nil {
		t.Fatal("Create returned a handle to a session it could not stamp; that session is unrecoverable")
	}
	if !strings.Contains(err.Error(), "ownership token") {
		t.Fatalf("error = %v, want it to name the ownership token", err)
	}
	if _, alive := st.sessions["rev-2"]; alive {
		t.Fatal("the unstampable session was left running; it is exactly the orphan AO can never kill")
	}
	if len(st.destroyed) == 0 {
		t.Fatal("the partially-created session was never destroyed")
	}
}

// TestSessionOwner_ClassifiesEveryAnswerDistinctly pins the four things a read
// of the token can mean. Collapsing any two of them is how a probe learns to
// lie.
func TestSessionOwner_ClassifiesEveryAnswerDistinctly(t *testing.T) {
	r, st := newScriptedRuntime()

	// Marked with AO's token.
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "rev-3", WorkspacePath: "/tmp/ws", Argv: []string{"echo"}, Owner: "ao-reviewer:rev-3",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f, _, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-3"}); err != nil || !f.OwnerKnown || f.Owner != "ao-reviewer:rev-3" {
		t.Fatalf("marked session = (%+v,%v)", f, err)
	}

	// Present but unmarked: NOT an error, and NOT ownership.
	st.sessions["stray"] = &scriptedSession{env: map[string]string{}, panePID: st.nextPID(), instanceID: st.nextInstance(), rootCommand: "/bin/sh -i"}
	if f, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "stray"}); err != nil || !exists || f.OwnerKnown {
		t.Fatalf("unmarked session = (%+v,%t,%v), want present but unowned", f, exists, err)
	}

	// Absent session: AO learned nothing, so this must be an ERROR and never be
	// mistaken for "unmarked".
	if _, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "gone"}); exists || err != nil {
		t.Fatalf("a missing session = (exists=%t, err=%v), want cleanly absent", exists, err)
	}

	// Unreachable server: likewise an error.
	st.serverDown = true
	if _, _, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-3"}); err == nil {
		t.Fatal("an unreachable server reported as unmarked")
	}
}

// runningWorkload puts a session in the state a live reviewer produces: the
// launch shell at pane_pid with the reviewer as its child.
func (s *scriptedTmux) runningWorkload(id string) *scriptedSession {
	sess := s.sessions[id]
	sess.rootCommand = "/bin/sh -c cd /tmp/ws || exit; claude -p review; exec cat >/dev/null"
	sess.children = []processEntry{{pid: s.nextPID(), ppid: sess.panePID, command: "claude -p review"}}
	return sess
}

// workloadExited models what production actually does when the reviewer
// finishes: the launch command's trailing `exec` REPLACES the shell at pane_pid
// with the keep-alive. Same pid, new command, and no children left.
func (s *scriptedTmux) workloadExited(id string) {
	sess := s.sessions[id]
	sess.rootCommand = "cat"
	sess.children = nil
}

// TestPaneProcessAlive_ALiveReviewerRunningCatIsNotDead is the blocker Codex
// found, and it is the reason foreground command names were abandoned entirely.
//
// A reviewer inspecting code legitimately runs `cat somefile`. tmux then reports
// `cat` as the pane's foreground command — identical to AO's own keep-alive. Any
// probe reading that name would classify a WORKING reviewer as exited, destroy
// its session, and launch a replacement over live work.
func TestPaneProcessAlive_ALiveReviewerRunningCatIsNotDead(t *testing.T) {
	for _, running := range []string{"cat internal/workflow/review_dispatch.go", "bash -c go test ./...", "sh", "git diff", "cat"} {
		t.Run(running, func(t *testing.T) {
			r, st := newScriptedRuntime()
			if _, err := r.Create(context.Background(), ports.RuntimeConfig{
				SessionID: "rev-live", WorkspacePath: "/tmp/ws", Argv: []string{"claude"},
				Owner: "ao-reviewer:rev-live",
			}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			sess := st.runningWorkload("rev-live")
			// The reviewer runs something of its own. It is a GRANDCHILD of the
			// pane root, which is exactly why it cannot be confused with the
			// keep-alive that would sit AT the pane root.
			reviewerPID := sess.children[0].pid
			sess.children = append(sess.children, processEntry{
				pid: st.nextPID(), ppid: reviewerPID, command: running,
			})

			alive, known, err := r.PaneProcessAlive(context.Background(), ports.RuntimeHandle{ID: "rev-live"})
			if err != nil {
				t.Fatalf("PaneProcessAlive: %v", err)
			}
			if !alive || !known {
				t.Fatalf("a live reviewer running %q was classified (alive=%t known=%t) — "+
					"AO would destroy it and launch a replacement over active work", running, alive, known)
			}
		})
	}
}

// The reverse: an actually-finished reviewer must be proven exited even though
// its tmux session is still very much registered.
func TestPaneProcessAlive_RealReviewerExitIsProvenEvenThoughTheSessionSurvives(t *testing.T) {
	r, st := newScriptedRuntime()
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "rev-exit", WorkspacePath: "/tmp/ws", Argv: []string{"claude"},
		Owner: "ao-reviewer:rev-exit",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st.runningWorkload("rev-exit")
	if alive, _, _ := r.PaneProcessAlive(context.Background(), ports.RuntimeHandle{ID: "rev-exit"}); !alive {
		t.Fatal("precondition: the reviewer should start out alive")
	}

	st.workloadExited("rev-exit")

	// The session is still registered — that is the trap the weaker probe fell into.
	if ok, _ := r.IsAlive(context.Background(), ports.RuntimeHandle{ID: "rev-exit"}); !ok {
		t.Fatal("precondition: the session should survive the reviewer")
	}
	alive, known, err := r.PaneProcessAlive(context.Background(), ports.RuntimeHandle{ID: "rev-exit"})
	if err != nil || !known {
		t.Fatalf("PaneProcessAlive = (%t,%t,%v), want a definite answer", alive, known, err)
	}
	if alive {
		t.Fatal("a finished reviewer was reported as running")
	}
}

// A reviewer that has not started yet looks the same as one that finished, if
// you only count children. It must be UNKNOWN, never EXITED — otherwise a probe
// firing microseconds after launch reclaims a reviewer that was merely booting.
func TestPaneProcessAlive_StartingUpIsUncertainNotDead(t *testing.T) {
	r, st := newScriptedRuntime()
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "rev-boot", WorkspacePath: "/tmp/ws", Argv: []string{"claude"},
		Owner: "ao-reviewer:rev-boot",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The launch shell is still exporting environment; nothing started yet.
	st.sessions["rev-boot"].children = nil

	alive, known, err := r.PaneProcessAlive(context.Background(), ports.RuntimeHandle{ID: "rev-boot"})
	if err != nil {
		t.Fatalf("PaneProcessAlive: %v", err)
	}
	if alive {
		t.Fatal("a reviewer that has not started was reported as running")
	}
	if known {
		t.Fatal("a still-starting reviewer was reported as PROVEN exited; it would be reclaimed mid-launch")
	}
}

// An unreadable process table is uncertainty, never death.
func TestPaneProcessAlive_UnreadableProcessTableIsUnknown(t *testing.T) {
	r, st := newScriptedRuntime()
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: "rev-ps", WorkspacePath: "/tmp/ws", Argv: []string{"claude"},
		Owner: "ao-reviewer:rev-ps",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st.psErr = true

	alive, known, _ := r.PaneProcessAlive(context.Background(), ports.RuntimeHandle{ID: "rev-ps"})
	if alive || known {
		t.Fatalf("PaneProcessAlive = (%t,%t), want an admitted non-answer", alive, known)
	}
}

// isKeepAliveCommand inspects the PANE ROOT's command line, so nothing a
// workload runs can satisfy it.
func TestIsKeepAliveCommand_OnlyMatchesTheExecdSentinel(t *testing.T) {
	if !isKeepAliveCommand("cat") {
		t.Fatal("the exec'd keep-alive was not recognised")
	}
	if !isKeepAliveCommand("/bin/cat") {
		t.Fatal("an absolute-path keep-alive was not recognised")
	}
	for _, notSentinel := range []string{
		"cat somefile.go",           // a reviewer reading a file
		"cat -n internal/x.go",      // ditto, with flags
		"/bin/sh -c cd /ws; claude", // the launch shell
		"claude -p review",
		"",
	} {
		if isKeepAliveCommand(notSentinel) {
			t.Fatalf("%q was mistaken for AO's keep-alive", notSentinel)
		}
	}
}

// TestFailUnownableCreate_NeverLeavesALiveUnownedSession is BLOCKER 2.
func TestFailUnownableCreate_NeverLeavesALiveUnownedSession(t *testing.T) {
	t.Run("ownership fails and destroy succeeds is an ordinary failure", func(t *testing.T) {
		r, st := newScriptedRuntime()
		st.dropOwnerEnv = true

		_, err := r.Create(context.Background(), ports.RuntimeConfig{
			SessionID: "rev-c1", WorkspacePath: "/tmp/ws",
			Argv: []string{"echo"}, Owner: "ao-reviewer:rev-c1",
		})
		if err == nil {
			t.Fatal("Create succeeded without ownership")
		}
		if errors.Is(err, ports.ErrRuntimeOrphanedSession) {
			t.Fatalf("a cleanly torn-down session was reported as an orphan: %v", err)
		}
		if _, live := st.sessions["rev-c1"]; live {
			t.Fatal("session survived")
		}
	})

	t.Run("ownership fails and the session survives teardown is an ORPHAN", func(t *testing.T) {
		r, st := newScriptedRuntime()
		st.dropOwnerEnv = true
		st.refuseKill = true // kill-session fails; the session stays up

		_, err := r.Create(context.Background(), ports.RuntimeConfig{
			SessionID: "rev-c2", WorkspacePath: "/tmp/ws",
			Argv: []string{"echo"}, Owner: "ao-reviewer:rev-c2",
		})
		if err == nil {
			t.Fatal("Create succeeded with a live unowned session behind it")
		}
		if !errors.Is(err, ports.ErrRuntimeOrphanedSession) {
			t.Fatalf("a surviving unowned session was reported as a clean failure: %v", err)
		}
		if _, live := st.sessions["rev-c2"]; !live {
			t.Fatal("precondition: the session should have survived")
		}
	})

	t.Run("ownership fails and cleanup truth is unknown is an ORPHAN", func(t *testing.T) {
		r, st := newScriptedRuntime()
		st.dropOwnerEnv = true
		st.downAfterOwnerRead = true // the server goes away mid-teardown

		_, err := r.Create(context.Background(), ports.RuntimeConfig{
			SessionID: "rev-c3", WorkspacePath: "/tmp/ws",
			Argv: []string{"echo"}, Owner: "ao-reviewer:rev-c3",
		})
		if err == nil {
			t.Fatal("Create succeeded despite an undeterminable teardown")
		}
		if !errors.Is(err, ports.ErrRuntimeOrphanedSession) {
			t.Fatalf("an undeterminable teardown resolved into a clean failure: %v", err)
		}
	})
}

// ---- session-incarnation coherence ---------------------------------------

func createReviewer(t *testing.T, r *Runtime, st *scriptedTmux, name string) *scriptedSession {
	t.Helper()
	if _, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: domain.SessionID(name), WorkspacePath: "/tmp/ws",
		Argv: []string{"claude"}, Owner: "ao-reviewer:" + name,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return st.runningWorkload(name)
}

// SessionFacts must never return facts assembled from two different sessions.
// This is BLOCKER 1 at the layer that can actually detect it.
func TestSessionFacts_RefusesToCombineTwoIncarnations(t *testing.T) {
	r, st := newScriptedRuntime()
	createReviewer(t, r, st, "rev-race")

	// The name changes hands partway through the observation.
	st.beforeNextRead = func() { st.replaceSession("rev-race", "somebody-elses-token") }

	facts, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-race"})

	// The invariant is not "an error is returned" — it is that no fact about the
	// STRANGER is ever presented as a fact about the session AO asked about.
	// Instance-targeting makes that structural: the reads address `$N`, which
	// the replacement was never given, so they can only fail or report absence.
	if exists && facts.Owner == "somebody-elses-token" {
		t.Fatalf("facts from the replacement were returned as facts about this session: %+v", facts)
	}
	if exists && err == nil && facts.OwnerKnown {
		t.Fatalf("facts were returned across a session replacement: %+v", facts)
	}
}

// ABA: the name is surrendered to a stranger and RECLAIMED before the end of the
// observation. A start/end name comparison cannot see this — both ends agree
// while a fact in the middle came from somebody else. Instance-targeting can.
func TestSessionFacts_ABAReplacementCannotContributeFacts(t *testing.T) {
	r, st := newScriptedRuntime()
	original := createReviewer(t, r, st, "rev-aba")
	originalInstance := original.instanceID

	// Mid-observation the name goes to a stranger and then comes back to a
	// session carrying the ORIGINAL instance id.
	// A → B before the ownership read, then B → A again immediately after it.
	// A start/end instance comparison sees A at both ends and concludes nothing
	// happened; the ownership fact in the middle came from B.
	st.beforeNextRead = func() {
		st.replaceSession("rev-aba", "somebody-elses-token")
		st.afterOwnerRead = func() {
			restored := st.replaceSession("rev-aba", "ao-reviewer:rev-aba")
			restored.instanceID = originalInstance
			restored.children = []processEntry{
				{pid: st.nextPID(), ppid: restored.panePID, command: "claude -p review"},
			}
		}
	}

	facts, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-aba"})
	if exists && err == nil && facts.Owner == "somebody-elses-token" {
		t.Fatal("the stranger's ownership token was attributed to AO's session")
	}
	if exists && facts.InstanceID != originalInstance {
		t.Fatalf("InstanceID = %q, want %q", facts.InstanceID, originalInstance)
	}
}

// A coherent read still succeeds, and reports the incarnation it is about.
func TestSessionFacts_CoherentReadCarriesItsInstanceID(t *testing.T) {
	r, st := newScriptedRuntime()
	sess := createReviewer(t, r, st, "rev-ok")

	facts, exists, err := r.SessionFacts(context.Background(), ports.RuntimeHandle{ID: "rev-ok"})
	if err != nil || !exists {
		t.Fatalf("SessionFacts = (%+v, %t, %v)", facts, exists, err)
	}
	if facts.InstanceID != sess.instanceID {
		t.Fatalf("InstanceID = %q, want %q", facts.InstanceID, sess.instanceID)
	}
	if !facts.OwnerKnown || facts.Owner != "ao-reviewer:rev-ok" {
		t.Fatalf("owner = (%q,%t)", facts.Owner, facts.OwnerKnown)
	}
	if !facts.WorkloadKnown || !facts.WorkloadAlive {
		t.Fatalf("workload = (alive=%t known=%t), want a live reviewer", facts.WorkloadAlive, facts.WorkloadKnown)
	}
}

// DestroyInstance must be unable to reach a replacement holding the same name.
// This is BLOCKER 2 at the layer that performs the destruction.
func TestDestroyInstance_CannotReachAReplacementUnderTheSameName(t *testing.T) {
	r, st := newScriptedRuntime()
	verified := createReviewer(t, r, st, "rev-kill")
	verifiedID := verified.instanceID

	// AO's session exits; a stranger takes the freed name.
	replacement := st.replaceSession("rev-kill", "somebody-elses-token")
	if replacement.instanceID == verifiedID {
		t.Fatal("precondition: the replacement must be a different incarnation")
	}

	// The kill still names the instance AO verified.
	if err := r.DestroyInstance(context.Background(), verifiedID); err != nil {
		t.Fatalf("DestroyInstance on a vanished instance should be a no-op: %v", err)
	}
	if _, alive := st.sessions["rev-kill"]; !alive {
		t.Fatal("THE STRANGER'S SESSION WAS DESTROYED by an id-targeted kill")
	}
	if st.sessions["rev-kill"].instanceID != replacement.instanceID {
		t.Fatal("the replacement was swapped out")
	}
}

// And an id-targeted kill does destroy the right incarnation when it is there.
func TestDestroyInstance_DestroysTheExactVerifiedIncarnation(t *testing.T) {
	r, st := newScriptedRuntime()
	sess := createReviewer(t, r, st, "rev-kill-ok")

	if err := r.DestroyInstance(context.Background(), sess.instanceID); err != nil {
		t.Fatalf("DestroyInstance: %v", err)
	}
	if _, alive := st.sessions["rev-kill-ok"]; alive {
		t.Fatal("the verified incarnation survived")
	}
}

// The failed-create cleanup is itself a read-then-act-by-name path, so it too
// must target the instance it created: a stranger that takes the freed name
// before teardown must survive.
func TestFailUnownableCreate_CleanupCannotKillAReplacement(t *testing.T) {
	r, st := newScriptedRuntime()
	st.dropOwnerEnv = true

	// The unownable session vanishes and a stranger takes its name, between
	// AO capturing the instance and AO tearing it down.
	var replacement *scriptedSession
	st.beforeNextRead = func() {
		replacement = st.replaceSession("rev-cr", "somebody-elses-token")
	}

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: domain.SessionID("rev-cr"), WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"}, Owner: "ao-reviewer:rev-cr",
	})
	if err == nil {
		t.Fatal("Create succeeded without ownership")
	}
	if replacement == nil {
		t.Skip("the replacement hook did not fire on this command sequence")
	}
	if _, alive := st.sessions["rev-cr"]; !alive {
		t.Fatal("THE REPLACEMENT WAS DESTROYED by the failed-create cleanup")
	}
	if st.sessions["rev-cr"].instanceID != replacement.instanceID {
		t.Fatal("the replacement incarnation was swapped out")
	}
	for _, d := range st.destroyed {
		if d == replacement.instanceID {
			t.Fatalf("destroyed the replacement instance %s", d)
		}
	}
}

// The created instance must come from the CREATION COMMAND. Discovering it
// afterwards by name is a window: the session can vanish and a stranger take the
// name before the look-up, and AO would then adopt the stranger's id as "the
// session I just made" — and destroy it during cleanup.
func TestCreate_CreatedInstanceComesFromTheCreationCommandNotALaterLookup(t *testing.T) {
	r, st := newScriptedRuntime()
	st.dropOwnerEnv = true // force the failure path, so cleanup runs

	// The moment new-session returns, AO's session vanishes and a stranger
	// takes the name — before AO issues any other command.
	var replacement *scriptedSession
	st.afterCreate = func() {
		replacement = st.replaceSession("rev-anchor", "somebody-elses-token")
	}

	_, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: domain.SessionID("rev-anchor"), WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"}, Owner: "ao-reviewer:rev-anchor",
	})
	if err == nil {
		t.Fatal("Create succeeded over a replaced session")
	}
	if replacement == nil {
		t.Fatal("precondition: the replacement was never created")
	}
	if _, alive := st.sessions["rev-anchor"]; !alive {
		t.Fatal("THE REPLACEMENT WAS DESTROYED — the created instance was resolved by name")
	}
	if st.sessions["rev-anchor"].instanceID != replacement.instanceID {
		t.Fatal("the replacement incarnation was swapped out")
	}
	for _, d := range st.destroyed {
		if d == replacement.instanceID {
			t.Fatalf("destroyed the replacement instance %s", d)
		}
	}
}

// Final readiness must be proven of the created instance. A name-only liveness
// check is the last place a replacement can be handed back as success: the name
// is occupied, the check passes, and Create returns a handle to somebody else's
// session. Exercised without an ownership token, so the readiness check is the
// only thing standing between the replacement and a successful return.
func TestCreate_FinalReadinessIsProvenOfTheCreatedInstance(t *testing.T) {
	r, st := newScriptedRuntime()

	// The replacement happens at the LAST possible moment: setup has completed
	// against AO's own session, and the name changes hands just as the final
	// readiness probe is issued.
	var replacement *scriptedSession
	st.beforeReadiness = func() {
		replacement = st.replaceSession("rev-ready", "")
	}

	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: domain.SessionID("rev-ready"), WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"},
	})
	if replacement == nil {
		t.Fatal("precondition: the replacement was never created")
	}
	if err == nil {
		t.Fatalf("Create returned success (handle %+v) for a session it did not create", h)
	}
	if h.InstanceID == replacement.instanceID {
		t.Fatal("Create handed back the replacement's instance")
	}
}

// BLOCKER 2: the global process table lags. A pane's processes stay visible for
// a moment after its session is destroyed, so liveness read from `ps` alone can
// describe a session that no longer exists — while a stranger already holds its
// name.
func TestSessionFacts_StaleProcessTableCannotResurrectAGoneInstance(t *testing.T) {
	r, st := newScriptedRuntime()
	createReviewer(t, r, st, "rev-stale")
	original := st.sessions["rev-stale"]
	originalInstance := original.instanceID
	stalePID := original.panePID

	// After the pane pid is read, the instance is destroyed and a stranger takes
	// the name — but the original's processes are still in the table.
	st.afterPanePIDRead = func() {
		replacement := st.replaceSession("rev-stale", "somebody-elses-token")
		// The gone instance's processes linger in the global listing.
		st.stale = append(st.stale, processEntry{pid: stalePID, ppid: 1, command: "/bin/sh -c gone"},
			processEntry{pid: stalePID + 1, ppid: stalePID, command: "claude -p review"})
		_ = replacement
	}

	facts, exists, err := r.SessionFacts(context.Background(),
		ports.RuntimeHandle{ID: "rev-stale", InstanceID: originalInstance})

	if exists && err == nil && facts.WorkloadAlive {
		t.Fatalf("stale process-tree facts resurrected a destroyed instance: %+v", facts)
	}
	if exists && facts.InstanceID != originalInstance {
		t.Fatalf("facts about a different incarnation were returned: %+v", facts)
	}
}

// BLOCKER 3: new-session succeeds but reports no usable instance id. Something
// is running that AO cannot name — and destroying by the reusable name would
// kill whoever holds it now.
func TestCreate_UnparseableInstanceIsOrphanedNeverDestroyedByName(t *testing.T) {
	r, st := newScriptedRuntime()
	st.garbleCreateOutput = true

	// The session AO made exits and a stranger takes the name.
	var replacement *scriptedSession
	st.afterCreate = func() {
		replacement = st.replaceSession("rev-garble", "somebody-elses-token")
	}

	h, err := r.Create(context.Background(), ports.RuntimeConfig{
		SessionID: domain.SessionID("rev-garble"), WorkspacePath: "/tmp/ws",
		Argv: []string{"echo"}, Owner: "ao-reviewer:rev-garble",
	})
	if err == nil {
		t.Fatalf("Create returned success (%+v) without an instance id", h)
	}
	if !errors.Is(err, ports.ErrRuntimeOrphanedSession) {
		t.Fatalf("err = %v, want ErrRuntimeOrphanedSession — an unnameable live session is an orphan, "+
			"not a clean failure", err)
	}
	if replacement == nil {
		t.Fatal("precondition: the replacement was never created")
	}
	if _, alive := st.sessions["rev-garble"]; !alive {
		t.Fatal("THE REPLACEMENT WAS DESTROYED by name-based cleanup")
	}
	if st.sessions["rev-garble"].instanceID != replacement.instanceID {
		t.Fatal("the replacement incarnation was swapped out")
	}
	if len(st.destroyed) != 0 {
		t.Fatalf("destroyed %v despite having no instance to target", st.destroyed)
	}
}
