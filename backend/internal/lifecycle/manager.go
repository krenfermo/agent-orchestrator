// Package lifecycle implements the synchronous reducer that writes durable
// session lifecycle facts. It deliberately keeps the session model small:
// activity_state plus an is_terminated bit are the only persisted status-like
// facts on the session row.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

type sessionStore interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	UpdateSession(ctx context.Context, rec domain.SessionRecord) error
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	// UpdateSessionFromActivitySignal is a narrow, owner-generation-fenced
	// write. It returns false when a concurrent lifecycle/agent-switch boundary
	// made the reducer's previously read session stale.
	UpdateSessionFromActivitySignal(ctx context.Context, rec domain.SessionRecord) (bool, error)
	// ListSessions returns every session in a project. The dispatcher reads it
	// to resolve the current orchestrator at delivery time.
	ListSessions(ctx context.Context, project domain.ProjectID) ([]domain.SessionRecord, error)
	// ListPRsBySession returns every PR row tracked for the session. The
	// reducer reads it to apply the multi-PR completion rule (terminate only
	// when no open PR remains and at least one merged) and to suppress
	// merge-conflict nudges on PRs stacked behind an open parent.
	ListPRsBySession(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error)
	GetPR(ctx context.Context, prURL string) (domain.PullRequest, bool, error)
	// ListPRReviews and ListPRComments return the effective rows committed by
	// the SCM observer, including each item's preserved injection decision.
	ListPRReviews(ctx context.Context, prURL string) ([]domain.PullRequestReview, error)
	ListPRComments(ctx context.Context, prURL string) ([]domain.PullRequestComment, error)
	// GetPRLastNudgeSignature / UpdatePRLastNudgeSignature persist the
	// reaction-dedup map so nudges survive a daemon restart.
	GetPRLastNudgeSignature(ctx context.Context, prURL string) (string, error)
	UpdatePRLastNudgeSignature(ctx context.Context, prURL, payload string) error
}

// controllerEpochStore is the atomic persistence primitive used by
// CommitControllerEpoch. It stays optional on the broad lifecycle store so
// focused reducer fakes do not need controller-transition methods; production
// SQLite implements it.
type controllerEpochStore interface {
	CommitSessionControllerEpoch(
		context.Context,
		domain.SessionID,
		domain.SessionMode,
		domain.SessionMode,
		string,
		time.Time,
	) (bool, error)
}

// agentSwitchSourceStopStore and agentSwitchTargetActivationStore are the
// atomic persistence primitives used at the two agent-switch ownership
// boundaries. They remain optional so focused lifecycle reducer fakes do not
// need to implement the agent-switch saga; production SQLite implements both.
type agentSwitchSourceStopStore interface {
	ConfirmAgentSwitchSourceStopped(context.Context, domain.AgentSwitchSourceStopConfirmation) (bool, error)
}

type agentSwitchTargetActivationStore interface {
	ActivateAgentSwitchTarget(context.Context, domain.AgentSwitchTargetActivation) (bool, error)
}

// notificationSink is the optional lifecycle-to-notification-producer boundary.
// sessionFactSink is the optional lifecycle-to-producer boundary. Lifecycle
// hands it durable session-level facts it has just written or just read; the
// producer decides whether any of them is worth notifying about. Lifecycle
// deliberately holds the ports interface, not the service type: it reduces
// facts, it does not own notification policy.
type sessionFactSink interface {
	Record(ctx context.Context, fact ports.SessionFact) error
}

type notificationSink interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
	// Resolve closes notifications whose underlying issue went away. It is the
	// only way a notification leaves the unresolved list: there is no manual
	// user-facing resolve action.
	Resolve(ctx context.Context, res ports.NotificationResolution) error
}

// projectConfigLoader resolves a project's config so MarkTerminated can check
// the ContainerReap opt-out before reaping. A load failure must not fall
// through to reaping - see ports.ContainerReaper below.
type projectConfigLoader interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

type sessionTerminator interface {
	Kill(ctx context.Context, id domain.SessionID) (bool, error)
}

type sessionUsageFinalizer interface {
	FinalizeSession(
		ctx context.Context,
		id domain.SessionID,
		expectedRuntimeLaunchID string,
		expectedSessionRevision time.Time,
	) error
}

type sessionUsageReactivator interface {
	ReactivateSession(ctx context.Context, id domain.SessionID, expectedRuntimeLaunchID string) error
}

type sessionOperationGate interface {
	SessionMutationInProgress(id domain.SessionID) bool
}

type pendingLaunch struct {
	launchID string
	ready    chan struct{}
}

// Option customizes a Manager.
type Option func(*Manager)

// WithNotificationSink wires lifecycle notification intents to a write-side producer.
func WithNotificationSink(sink notificationSink) Option {
	return func(m *Manager) { m.notifications = sink }
}

// WithSessionFactNotifier wires lifecycle's durable session-level facts to the
// notification producer. Without it the reducer still runs and still writes
// every fact; only the session-scoped notifications are skipped.
func WithSessionFactNotifier(sink sessionFactSink) Option {
	return func(m *Manager) { m.sessionFacts = sink }
}

// WithTelemetry wires lifecycle activity transitions to the shared telemetry sink.
func WithTelemetry(sink ports.EventSink) Option {
	return func(m *Manager) { m.telemetry = sink }
}

// WithContainerReaper wires the container leg of #2652: MarkTerminated will
// force-remove the terminated session's ao.session-labeled Docker containers,
// unless the project opts out via ProjectConfig.ContainerReap.Disabled.
func WithContainerReaper(reaper ports.ContainerReaper, projects projectConfigLoader) Option {
	return func(m *Manager) {
		m.containers = reaper
		m.projects = projects
	}
}

// branchLockCoordinator is the direct-branch execution-lock surface lifecycle
// drives at a task session's turn boundaries (Checkpoint 8P-E.14A). Satisfied
// by the daemon's *branchlock.Manager adapter.
//
// Both halves are here because both halves are turn-scoped: the release at the
// end of a turn is only safe if the next turn takes the lock again.
type branchLockCoordinator interface {
	// AcquireForSession takes the locks the session's project requires for a
	// new turn. A no-op for a project that is not in direct-branch mode, for a
	// session that already holds them, and for a workflow run's current worker.
	AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error)
	// ReleaseSession frees every lock the session owns in its own right. Locks
	// a workflow run owns while this session is its worker are not the
	// session's and are never touched.
	ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error)
}

// WithActiveSteering supplies the adapter-provided active-turn steering
// capability (see ports.ActiveTurnSteerer). Without it the reducer assumes no
// harness can be steered mid-turn.
func WithActiveSteering(pred func(domain.AgentHarness) bool) Option {
	return func(m *Manager) {
		if pred != nil {
			m.steerActive = pred
		}
	}
}

// Manager reduces runtime, activity, spawn, and termination observations into durable session facts.
// It also owns agent nudges caused by PR observations, including merge-conflict, CI-failure, and review-feedback prompts.
type Manager struct {
	store sessionStore
	// guard is the shared pane-write primitive every reaction nudge goes
	// through (see sessionguard). Nil when no messenger was wired: reaction
	// nudges become no-ops but the reducer still runs.
	guard         *sessionguard.Guard
	notifications notificationSink
	// sessionFacts receives session-level facts (a pending decision, a spent
	// repair budget) after the durable state behind them is committed.
	sessionFacts sessionFactSink
	// completionTerminator is late-bound because Session Manager itself depends
	// on this lifecycle reducer. It is required before the SCM observer starts.
	completionTerminator sessionTerminator
	// usageFinalizer is late-bound because the usage pipeline is optional. It
	// receives terminal intent before is_terminated makes the session ineligible
	// for normal source discovery.
	usageFinalizer   sessionUsageFinalizer
	usageReactivator sessionUsageReactivator
	containers       ports.ContainerReaper
	projects         projectConfigLoader
	operationGateMu  sync.RWMutex
	operationGate    sessionOperationGate
	branchLocksMu    sync.RWMutex
	branchLocks      branchLockCoordinator

	mu        sync.Mutex
	window    time.Duration
	clock     func() time.Time
	react     reactionState
	telemetry ports.EventSink
	// flights tracks, per session, the in-flight tool executions and the
	// pending permission dialog's identity (see toolFlight). Guarded by mu.
	flights map[domain.SessionID]*toolFlight
	// pendingLaunches closes the small ordering gap between starting a supervised
	// process and durably recording its generation in MarkSpawned. A hook from
	// that exact generation waits on ready instead of being discarded as stale.
	// This coordination is intentionally memory-only: a daemon crash leaves the
	// durable session exited, so the user can safely retry the resume.
	pendingLaunches map[domain.SessionID]pendingLaunch
	// steerActive reports whether a harness can safely receive a write during an
	// active turn (input steers the run) rather than only while idle. Supplied by
	// the agent adapter via WithActiveSteering; the default answers false, so an
	// unknown harness is only written to while idle.
	steerActive func(domain.AgentHarness) bool
}

// New builds a Lifecycle Manager over the session store it writes and the messenger it uses for agent nudges.
func New(store sessionStore, messenger ports.AgentMessenger, opts ...Option) *Manager {
	// UTC so activity-driven LastActivityAt/UpdatedAt match spawn-stamped
	// timestamps (the session manager clock is UTC too); a local clock here left
	// `ao session get` showing created in UTC but updated in local time. A
	// WithClock option may still override this in tests.
	clock := func() time.Time { return time.Now().UTC() }
	m := &Manager{
		store:           store,
		window:          defaultRecentActivityWindow,
		clock:           clock,
		react:           newReactionState(),
		flights:         map[domain.SessionID]*toolFlight{},
		pendingLaunches: map[domain.SessionID]pendingLaunch{},
		steerActive:     func(domain.AgentHarness) bool { return false },
	}
	if messenger != nil {
		m.guard = sessionguard.New(store, messenger, nil)
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// SetCompletionTerminator wires merge completion to the same teardown path as
// an explicit user kill.
func (m *Manager) SetCompletionTerminator(terminator sessionTerminator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completionTerminator = terminator
}

// SetUsageFinalizer wires termination and relaunches to usage collection.
// Telemetry failures never block the lifecycle transition.
func (m *Manager) SetUsageFinalizer(finalizer sessionUsageFinalizer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usageFinalizer = finalizer
	m.usageReactivator, _ = finalizer.(sessionUsageReactivator)
}

// SetSessionInputLease late-binds Session Manager's pane-input authority into
// lifecycle's guarded reaction writes. Lifecycle is constructed first during
// daemon boot, so constructor injection would create a dependency cycle.
func (m *Manager) SetSessionInputLease(lease sessionguard.InputLease) {
	if m.guard != nil {
		m.guard.SetInputLease(lease)
	}
}

// SetBranchLocks wires the direct-branch execution lock into the two places
// lifecycle owns a task session's ownership of its branch:
//
//   - MarkTerminated frees every lock the terminated session owns.
//   - Turn boundaries (ApplyActivitySignal) take the lock when a turn starts and
//     give it back when the turn ends.
//
// Both live here rather than in session_manager for the same reason the
// container reaper does: these are the places every path converges. Termination
// converges on MarkTerminated — Kill, spawn failure, retire-for-replacement,
// daemon-shutdown teardown, cleanup, and the PR/issue-driven termination in
// reactions.go that never touches session_manager at all. Turn boundaries
// converge on the activity signal, which is reported the same way whether the
// prompt came from AO or from a human typing straight into the pane.
//
// Late-bound because the daemon builds the lifecycle manager before the branch
// lock manager exists.
func (m *Manager) SetBranchLocks(locks branchLockCoordinator) {
	m.branchLocksMu.Lock()
	m.branchLocks = locks
	m.branchLocksMu.Unlock()
}

func (m *Manager) currentBranchLocks() branchLockCoordinator {
	m.branchLocksMu.RLock()
	defer m.branchLocksMu.RUnlock()
	return m.branchLocks
}

// acquireSessionBranchLocks takes the session's branch for a turn that is
// starting.
//
// Best-effort by construction, and the reason is worth stating plainly: the
// signal that a turn started is a hook that fires *after* the prompt was
// submitted, so there is nothing left to refuse. A conflict here means another
// task or workflow took the branch while this session was idle; AO logs that
// truthfully and the session keeps running, rather than pretending it owns a
// branch it does not. The refusal that actually protects the branch is the one
// at task start (session_manager.acquireSessionBranchLocks), which happens
// before any agent exists.
func (m *Manager) acquireSessionBranchLocks(ctx context.Context, projectID domain.ProjectID, id domain.SessionID) {
	locks := m.currentBranchLocks()
	if locks == nil {
		return
	}
	acquired, err := locks.AcquireForSession(ctx, projectID, id)
	if err != nil {
		slog.Default().Warn(
			"lifecycle: branch lock acquire at turn start failed; session is working without the execution lock",
			"session", id, "project", projectID, "err", err,
		)
		return
	}
	if len(acquired) > 0 {
		slog.Default().Info("lifecycle: acquired session branch locks for new turn", "session", id, "count", len(acquired))
	}
}

// releaseSessionBranchLocks frees a session's own execution locks, at the end
// of a turn or when the session terminates.
// Best-effort and never returned: a release failure must not turn a terminated
// session into an error, and boot reconciliation releases any lock whose owning
// session is terminated or has no turn in flight anyway
// (branchlock/retention.go), so a miss is self-healing rather than a
// permanently occupied branch.
func (m *Manager) releaseSessionBranchLocks(ctx context.Context, id domain.SessionID, reason string) {
	locks := m.currentBranchLocks()
	if locks == nil {
		return
	}
	n, err := locks.ReleaseSession(ctx, string(id), reason)
	if err != nil {
		slog.Default().Warn("lifecycle: branch lock release failed", "session", id, "err", err)
		return
	}
	if n > 0 {
		slog.Default().Info("lifecycle: released session branch locks", "session", id, "count", n, "reason", reason)
	}
}

// SetSessionOperationGate prevents observation-driven terminal facts from
// racing AO's deliberate provider replacement/relaunch operations.
func (m *Manager) SetSessionOperationGate(gate sessionOperationGate) {
	m.operationGateMu.Lock()
	m.operationGate = gate
	m.operationGateMu.Unlock()
}

func (m *Manager) sessionMutationInProgress(id domain.SessionID) bool {
	m.operationGateMu.RLock()
	gate := m.operationGate
	m.operationGateMu.RUnlock()
	return gate != nil && gate.SessionMutationInProgress(id)
}

// PrepareLaunch registers a supervised generation before the runtime starts.
// Hooks from that exact generation wait until MarkSpawned commits the generation
// instead of racing the old durable generation and being discarded as stale.
func (m *Manager) PrepareLaunch(id domain.SessionID, launchID string) error {
	launchID = strings.TrimSpace(launchID)
	if launchID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if pending, ok := m.pendingLaunches[id]; ok {
		if pending.launchID == launchID {
			return nil
		}
		return fmt.Errorf("lifecycle: session %q already has launch %q in progress", id, pending.launchID)
	}
	m.pendingLaunches[id] = pendingLaunch{launchID: launchID, ready: make(chan struct{})}
	return nil
}

// CancelLaunch releases hooks waiting on a generation whose runtime failed to
// start. Once released, normal generation fencing discards those signals.
func (m *Manager) CancelLaunch(id domain.SessionID, launchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishLaunchLocked(id, strings.TrimSpace(launchID))
}

// ReleaseLaunch publishes that the caller has durably committed the prepared
// generation. Hooks waiting behind PrepareLaunch may now re-read the session
// row and pass normal generation fencing. Unlike MarkSpawned, this does not
// rewrite session ownership; agent switching commits that ownership together
// with its saga state in the AgentSwitchStore transaction.
func (m *Manager) ReleaseLaunch(id domain.SessionID, launchID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finishLaunchLocked(id, strings.TrimSpace(launchID))
}

func (m *Manager) finishLaunchLocked(id domain.SessionID, launchID string) {
	if launchID == "" {
		return
	}
	pending, ok := m.pendingLaunches[id]
	if !ok || pending.launchID != launchID {
		return
	}
	delete(m.pendingLaunches, id)
	close(pending.ready)
}

func (m *Manager) mutate(ctx context.Context, id domain.SessionID, fn func(domain.SessionRecord, time.Time) (domain.SessionRecord, bool)) error {
	m.mu.Lock()
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		m.mu.Unlock()
		return err
	}
	now := m.clock()
	next, changed := fn(rec, now)
	if !changed {
		m.mu.Unlock()
		return nil
	}
	next.UpdatedAt = now
	if err := m.store.UpdateSession(ctx, next); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	// Notification side effects run outside the reducer lock, like the activity
	// path does: a slow notification store must never stall lifecycle writes.
	m.resolveNotifications(ctx, needsInputResolutions(rec, next, now)...)
	return nil
}

// needsInputResolutions reports the needs-input notification a session write
// just made stale. A session that stopped waiting on the user — because the
// input arrived, or because the session ended — has nothing left to resolve.
func needsInputResolutions(prev, next domain.SessionRecord, now time.Time) []ports.NotificationResolution {
	if !prev.Activity.State.NeedsInput() {
		return nil
	}
	if next.Activity.State.NeedsInput() && !next.IsTerminated {
		return nil
	}
	return []ports.NotificationResolution{{
		Type:       domain.NotificationNeedsInput,
		SessionID:  next.ID,
		ResolvedAt: now,
	}}
}

// ApplyRuntimeObservation only writes when runtime liveness is unambiguous. A
// failed probe or liveness disagreement is ignored. Runtime death keeps the
// existing recent-activity guard; supervised workload death is independently
// fenced by the launch generation and never terminates the runtime.
func (m *Manager) ApplyRuntimeObservation(ctx context.Context, id domain.SessionID, f ports.RuntimeFacts) error {
	matchesLaunch := func(cur domain.SessionRecord) bool {
		currentLaunch := cur.Metadata.RuntimeLaunchID
		return currentLaunch == "" || f.LaunchID == currentLaunch
	}
	var (
		finalizer           sessionUsageFinalizer
		terminationLaunch   string
		terminationRevision time.Time
		shouldTerminate     bool
	)
	if err := m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated || !matchesLaunch(cur) {
			return cur, false
		}
		currentLaunch := cur.Metadata.RuntimeLaunchID
		if currentLaunch != "" && f.Runtime == ports.ProbeAlive && f.Workload == ports.ProbeDead {
			if cur.Activity.State == domain.ActivityExited {
				return cur, false
			}
			next := cur
			next.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: timeOr(f.ObservedAt, now)}
			delete(m.flights, id)
			return next, true
		}
		if !runtimeClearlyDead(f, cur.Activity, now, m.window) {
			return cur, false
		}
		if m.sessionMutationInProgress(id) {
			return cur, false
		}
		finalizer = m.usageFinalizer
		terminationLaunch = currentLaunch
		terminationRevision = cur.UpdatedAt
		shouldTerminate = true
		return cur, false
	}); err != nil || !shouldTerminate {
		return err
	}

	finalizeSessionUsage(ctx, id, terminationLaunch, terminationRevision, finalizer)

	terminated := false
	err := m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
		if cur.IsTerminated || !cur.UpdatedAt.Equal(terminationRevision) ||
			cur.Metadata.RuntimeLaunchID != terminationLaunch || !matchesLaunch(cur) ||
			!runtimeClearlyDead(f, cur.Activity, now, m.window) || m.sessionMutationInProgress(id) {
			return cur, false
		}
		next := cur
		next.IsTerminated = true
		next.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: timeOr(f.ObservedAt, now)}
		// Reaper-driven death (crash/SIGKILL) never fires a session-end hook,
		// so this is the last chance to release the session's tool-flight
		// state; a leaked entry would otherwise persist for the daemon's life
		// (later observations return early on cur.IsTerminated). Runs under
		// m.mu — mutate holds it across this callback.
		delete(m.flights, id)
		terminated = true
		return next, true
	})
	if err != nil {
		return err
	}
	if terminated {
		// Route reaper-observed death through the same container-reap hook as
		// every other terminal path (#2652): a crash/SIGKILL detected by the
		// runtime reaper must not leave the session's Docker containers behind
		// just because it never called MarkTerminated directly.
		m.reapSessionContainers(ctx, id)
	}
	return nil
}

// ApplyActivitySignal records an authoritative agent activity signal and any
// native agent session id carried alongside it. Metadata-only hooks leave the
// existing activity and first-signal facts untouched.
func (m *Manager) ApplyActivitySignal(ctx context.Context, id domain.SessionID, s ports.ActivitySignal) error {
	// Direct-branch execution ownership is turn-scoped (Checkpoint 8P-E.14A),
	// and this is where a turn is observed to start and end. The action itself
	// runs after every return path below has released m.mu — it does repository
	// I/O and a durable write of its own, and must never do either under the
	// reducer's lock.
	var branchLockAction func()
	defer func() {
		if branchLockAction != nil {
			branchLockAction()
		}
	}()
	s.AgentSessionID = strings.TrimSpace(s.AgentSessionID)
	s.LatestUserPrompt = strings.TrimSpace(s.LatestUserPrompt)
	s.LatestAssistantUpdate = strings.TrimSpace(s.LatestAssistantUpdate)
	s.TranscriptPath = strings.TrimSpace(s.TranscriptPath)
	s.LaunchID = strings.TrimSpace(s.LaunchID)
	s.ControllerGeneration = strings.TrimSpace(s.ControllerGeneration)
	// A response or Stop hook produced by AO's optional source handoff request
	// may contain last_assistant_message without echoing the internal prompt.
	// From collection through source teardown, do not let that coordination
	// response replace the latest user-facing assistant update used by the
	// target continuation. The semantic outcome may already be received,
	// rejected, timed out, or failed before the provider emits its final Stop
	// hook. not_attempted/unavailable do not prove an internal request landed,
	// so a legitimate source update remains user-facing in those states.
	if s.LatestAssistantUpdate != "" {
		if switchStore, ok := m.store.(ports.AgentSwitchStore); ok {
			if active, found, err := switchStore.GetActiveAgentSwitch(ctx, id); err == nil && found {
				internalRequestMayHaveLanded := false
				switch active.AgentHandoffStatus {
				case domain.AgentHandoffRequested, domain.AgentHandoffReceived, domain.AgentHandoffTimedOut,
					domain.AgentHandoffFailed, domain.AgentHandoffRejected:
					internalRequestMayHaveLanded = true
				}
				if internalRequestMayHaveLanded {
					switch active.State {
					case domain.AgentSwitchPreparingHandoff, domain.AgentSwitchStoppingSource:
						s.LatestAssistantUpdate = ""
					}
				}
			}
		}
	}
	if !s.Valid && s.AgentSessionID == "" && s.LatestUserPrompt == "" && s.LatestAssistantUpdate == "" && s.TranscriptPath == "" {
		return nil
	}
	if s.LaunchID != "" {
		if err := m.stagePendingAgentSwitchNativeMetadata(ctx, id, s); err != nil {
			return err
		}
	}
	var intent *ports.NotificationIntent
	var completionIntent *ports.NotificationIntent
	m.mu.Lock()
	for {
		pending, ok := m.pendingLaunches[id]
		if !ok || s.LaunchID == "" || pending.launchID != s.LaunchID {
			break
		}
		ready := pending.ready
		m.mu.Unlock()
		select {
		case <-ready:
			m.mu.Lock()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ports.ErrSessionNotFound, id)
	}
	now := m.clock()
	if rec.IsTerminated {
		delete(m.flights, id)
		m.mu.Unlock()
		return nil
	}
	if rec.Metadata.RuntimeLaunchID != "" && s.LaunchID != rec.Metadata.RuntimeLaunchID {
		m.mu.Unlock()
		return nil
	}
	if s.ControllerGeneration != "" &&
		(domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat ||
			s.ControllerGeneration != rec.Metadata.ControllerGeneration) {
		m.mu.Unlock()
		return nil
	}
	if !s.ExpectedUpdatedAt.IsZero() &&
		!rec.UpdatedAt.Equal(s.ExpectedUpdatedAt) {
		m.mu.Unlock()
		return nil
	}
	// An explicit prompt submission is proof that an agent was relaunched in the
	// preserved shell. Other same-generation callbacks may have been delayed
	// behind the process-exit report and cannot resurrect an exited workload.
	if rec.Activity.State == domain.ActivityExited && s.Valid && s.State != domain.ActivityExited &&
		(s.State != domain.ActivityActive || s.Event != "user-prompt-submit") {
		m.mu.Unlock()
		return nil
	}
	// The signal now provably belongs to this session's current launch, so its
	// turn boundary is authoritative and the branch follows it. This is decided
	// from the RAW signal, before the tool-precedence fold below: precedence
	// decides what the displayed state should be, not whether a turn ended, and
	// a turn-completion event is never suppressed by it anyway (it is a turn
	// boundary — see isTurnBoundaryEvent). Deciding here also means a repeated
	// stop on an already-idle row still hands the branch back, instead of being
	// dropped by the same-state early return.
	switch {
	case turnCompleted(s):
		branchLockAction = func() {
			m.releaseSessionBranchLocks(ctx, id, "task turn ended ("+s.Event+")")
		}
	case turnStarted(s):
		projectID := rec.ProjectID
		branchLockAction = func() { m.acquireSessionBranchLocks(ctx, projectID, id) }
	}
	// The same authoritative boundary is also what proves a task DID the work
	// it was given, which is the fact the board needs to say "Completed"
	// instead of "Idle" once the session goes quiet. It is decided from the
	// raw signal for the same reasons the branch decision above is, and it is
	// folded into rec here so every write path below carries it.
	//
	// Two directions, and both matter. A reported completion stamps the
	// receipt. Work going back in flight clears it, so the receipt always
	// describes the CURRENT quiet period: a task that was told to do more, or
	// that has stopped to ask the user something, is not a finished task, and
	// must not fall back to Completed if it later goes quiet without saying so
	// again.
	//
	// Tasks only. An orchestrator is a standing conversation partner, not a unit
	// of work: its turns end all day long and none of them is a task being
	// finished, so "Completed" would be the wrong word for it and Idle —
	// waiting for you — remains the right one. The test is against orchestrator
	// rather than for worker because worker is what an unset kind means, in the
	// column default and everywhere else.
	completionChanged := false
	switch {
	case turnSucceeded(s) && rec.Kind != domain.KindOrchestrator:
		if rec.TurnCompletedAt.IsZero() {
			rec.TurnCompletedAt = timeOr(s.Timestamp, now)
			completionChanged = true
		}
	case s.Valid && s.State.WorkInFlight():
		if !rec.TurnCompletedAt.IsZero() {
			rec.TurnCompletedAt = time.Time{}
			completionChanged = true
		}
	}
	// Event-tagged signals fold through the session's tool-flight state first:
	// they may be suppressed (state write skipped) by the blocked-precedence
	// rule, while their tracking side effects still land. Untagged signals
	// (old CLIs, adapters without tool identity) pass through untouched —
	// last-writer-wins, exactly as before.
	metadataChanged := (s.AgentSessionID != "" && rec.Metadata.AgentSessionID != s.AgentSessionID) ||
		(s.LatestUserPrompt != "" && rec.Metadata.LatestUserPrompt != s.LatestUserPrompt) ||
		(s.LatestAssistantUpdate != "" && rec.Metadata.LatestAssistantUpdate != s.LatestAssistantUpdate) ||
		(s.TranscriptPath != "" && rec.Metadata.NativeTranscriptPath != s.TranscriptPath)
	if s.Valid {
		s = m.applyToolPrecedenceLocked(id, rec.Activity.State, s)
	}
	if !s.Valid && !metadataChanged {
		m.mu.Unlock()
		return nil
	}
	if !s.Valid {
		applyActivityMetadata(&rec.Metadata, s)
		rec.UpdatedAt = now
		_, err := m.store.UpdateSessionFromActivitySignal(ctx, rec)
		m.mu.Unlock()
		return err
	}
	if metadataChanged {
		// Fold metadata into rec before copying it into next below, so the
		// activity and resume handle land in one store update.
		applyActivityMetadata(&rec.Metadata, s)
	}
	prevState := rec.Activity.State
	prevAt := rec.Activity.LastActivityAt
	act := domain.Activity{State: s.State, LastActivityAt: timeOr(s.Timestamp, now)}
	sameState := sameActivity(rec.Activity, act)
	// A same-state repeat is still a write when it is the FIRST signal for
	// this spawn: the receipt itself is a durable fact (it clears the
	// no_signal display status). Hook deliveries are best-effort, so the
	// first to ARRIVE may match the seeded state — e.g. a turn's "active"
	// POST is lost and its Stop hook lands idle on the idle-seeded row.
	if sameState && !rec.FirstSignalAt.IsZero() {
		// completionChanged joins the list because a Stop that lands on an
		// already-idle row is exactly how a finished task usually reports
		// itself: the turn's "active" POST may never have arrived, or an
		// untagged idle beat the Stop to the row. Dropping it here as a
		// same-state repeat would throw away the only proof the work is done.
		if metadataChanged || completionChanged || s.Event == "user-prompt-submit" {
			rec.UpdatedAt = now
			applied, err := m.store.UpdateSessionFromActivitySignal(ctx, rec)
			// Captured under the lock, emitted after it: the receipt is only
			// news when this write is the one that stamped it.
			completed := completionChanged && !rec.TurnCompletedAt.IsZero()
			m.mu.Unlock()
			if err != nil {
				return err
			}
			if !applied {
				return nil
			}
			if completed {
				m.emitNotification(ctx, taskCompletionIntent(rec))
			}
			return m.acknowledgeAgentSwitchTarget(ctx, id, s, now)
		}
		m.mu.Unlock()
		return nil
	}
	next := rec
	next.Activity = act
	if next.FirstSignalAt.IsZero() {
		next.FirstSignalAt = timeOr(s.Timestamp, now)
	}
	if s.State == domain.ActivityExited {
		// The agent process can exit while the managed tmux session remains
		// alive and inspectable. Do not infer session termination from this
		// hook; a runtime observation or explicit lifecycle action owns that
		// fact. No tool/permission correlation survives an agent process exit.
		delete(m.flights, id)
	}
	next.UpdatedAt = now
	applied, err := m.store.UpdateSessionFromActivitySignal(ctx, next)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !applied {
		m.mu.Unlock()
		return nil
	}
	// Transition into the needs-input family (waiting_input or blocked) pings
	// the user; an in-family escalation (waiting_input -> blocked) does not
	// re-notify — the user was already pinged once for this pause.
	if !rec.Activity.State.NeedsInput() && next.Activity.State.NeedsInput() && !next.IsTerminated {
		intent = &ports.NotificationIntent{
			Type:               domain.NotificationNeedsInput,
			SessionID:          next.ID,
			ProjectID:          next.ProjectID,
			CreatedAt:          next.Activity.LastActivityAt,
			SessionDisplayName: next.DisplayName,
		}
	}
	// A task that just reported it finished the work it was given. The trigger
	// is the completion receipt being stamped by THIS write — the same durable
	// fact the Completed status reads — not a generic idle or stop event, and
	// not a receipt that was already there. Because the receipt is cleared the
	// moment work goes back in flight, each finished turn raises this once and
	// a restart re-reading an unchanged row raises nothing.
	if completionChanged && !next.TurnCompletedAt.IsZero() {
		completionIntent = taskCompletionIntent(next)
	}
	// The session came to rest on a pending DECISION -- a tool-permission or
	// approval dialog. That is the one state automation must never write into,
	// so a person genuinely has to answer it, and it is reported however the
	// session got there: directly from active or idle (the shape a
	// permission-request hook produces) or by escalating in-family out of
	// waiting_input. The needs-input intent above is untouched by this: it
	// still fires exactly once on family ENTRY and stays silent in-family, so
	// no extra legacy ping is introduced on either path.
	questionFact := humanQuestionFact(rec, next)
	// Leaving the needs-input family is the user answering: the notification
	// that pinged them has nothing left to resolve.
	resolutions := needsInputResolutions(rec, next, now)
	waitingEvents := m.waitingInputEvents(next, prevState, prevAt, now)
	m.mu.Unlock()
	if err := m.acknowledgeAgentSwitchTarget(ctx, id, s, now); err != nil {
		return err
	}
	for _, ev := range waitingEvents {
		m.emitTelemetry(ctx, ev)
	}
	m.emitNotification(ctx, intent)
	m.emitNotification(ctx, completionIntent)
	if questionFact != nil {
		m.recordSessionFact(ctx, *questionFact)
	}
	m.resolveNotifications(ctx, resolutions...)
	return nil
}

// humanQuestionFact reports a genuine ENTRY into blocked -- any prior state to
// blocked, which covers both the direct active/idle -> blocked move a
// permission-request hook makes and the waiting_input -> blocked escalation.
//
// Restricting it to the escalation, as an earlier draft did, left the COMMON
// path with no record at all: a permission prompt moves a session straight from
// active or idle into blocked and never passes through waiting_input, so the
// pending-decision notification was never raised for it.
//
// It is scoped to the pause by the activity timestamp the session row now
// durably holds. Only an entry qualifies: a blocked -> blocked repeat is folded
// upstream on the same state and, on the first signal for a spawn where it is
// not, prev is already blocked and is rejected here. So one pause maps to one
// notification, and a restart re-reading the same stored timestamp replays to
// the row it already produced.
func humanQuestionFact(prev, next domain.SessionRecord) *ports.SessionFact {
	if next.IsTerminated ||
		next.Activity.State != domain.ActivityBlocked ||
		prev.Activity.State == domain.ActivityBlocked {
		return nil
	}
	return &ports.SessionFact{
		Kind:               ports.SessionFactHumanQuestion,
		SessionID:          next.ID,
		ProjectID:          next.ProjectID,
		ScopeID:            ports.PauseScopeID(next.Activity.LastActivityAt),
		SessionDisplayName: next.DisplayName,
		ObservedAt:         next.Activity.LastActivityAt,
	}
}

// taskCompletionIntent builds the "this task finished" notification from a
// session row whose completion receipt was just stamped. The dedupe key is the
// receipt timestamp itself, so the event it names is one specific finished
// turn: a later turn on the same session gets its own notification, and a
// replay of this one gets none.
func taskCompletionIntent(rec domain.SessionRecord) *ports.NotificationIntent {
	return &ports.NotificationIntent{
		Type:               domain.NotificationTaskCompleted,
		SessionID:          rec.ID,
		ProjectID:          rec.ProjectID,
		CreatedAt:          rec.TurnCompletedAt,
		SessionDisplayName: rec.DisplayName,
		DedupeKey:          string(rec.ID) + "@" + rec.TurnCompletedAt.UTC().Format(time.RFC3339Nano),
	}
}

// stagePendingAgentSwitchNativeMetadata persists provider-assigned startup
// identity while a target hook is waiting behind PrepareLaunch. It deliberately
// updates only the switch's retained native-session row, never the source-owned
// session row. Once ownership transfers, ReleaseLaunch lets the same hook apply
// its normal activity/metadata update. If the daemon crashes first, recovery
// can still prove the target conversation is resumable.
func (m *Manager) stagePendingAgentSwitchNativeMetadata(ctx context.Context, id domain.SessionID, s ports.ActivitySignal) error {
	if s.AgentSessionID == "" && s.TranscriptPath == "" {
		return nil
	}
	store, ok := m.store.(ports.AgentSwitchStore)
	if !ok {
		return nil
	}
	sw, found, err := store.GetActiveAgentSwitch(ctx, id)
	if err != nil {
		return err
	}
	if !found || sw.State != domain.AgentSwitchStartingTarget || string(sw.TargetGenerationID) != s.LaunchID || sw.TargetNativeSessionRef == nil {
		return nil
	}
	native, found, err := store.GetAgentNativeSession(ctx, *sw.TargetNativeSessionRef)
	if err != nil {
		return err
	}
	if !found || native.AOSessionID != id || native.Harness != sw.TargetHarness || native.LastGenerationID != sw.TargetGenerationID {
		return nil
	}
	changed := false
	if s.AgentSessionID != "" && native.NativeSessionID != s.AgentSessionID {
		if native.NativeSessionID != "" {
			return fmt.Errorf("lifecycle: target native session identity changed from %q to %q", native.NativeSessionID, s.AgentSessionID)
		}
		native.NativeSessionID = s.AgentSessionID
		changed = true
	}
	if s.TranscriptPath != "" && native.TranscriptPath != s.TranscriptPath {
		native.TranscriptPath = s.TranscriptPath
		changed = true
	}
	if !changed {
		return nil
	}
	updated, err := store.UpdateAgentNativeSession(ctx, native, sw.TargetGenerationID)
	if err != nil {
		return err
	}
	if !updated {
		return errors.New("lifecycle: target native session metadata changed concurrently")
	}
	return nil
}

func (m *Manager) acknowledgeAgentSwitchTarget(ctx context.Context, id domain.SessionID, signal ports.ActivitySignal, at time.Time) error {
	if !signal.Valid || signal.State != domain.ActivityActive || signal.Event != "user-prompt-submit" || signal.LaunchID == "" {
		return nil
	}
	store, ok := m.store.(ports.AgentSwitchStore)
	if !ok {
		return nil
	}
	sw, found, err := store.GetActiveAgentSwitch(ctx, id)
	if err != nil {
		return fmt.Errorf("lifecycle: read active agent switch acknowledgement for %s: %w", id, err)
	}
	if !found || sw.State != domain.AgentSwitchDelivering {
		return nil
	}
	_, err = store.AcknowledgeAgentSwitchTarget(ctx, sw.ID, id, domain.AgentGenerationID(signal.LaunchID), at)
	if err != nil {
		return fmt.Errorf("lifecycle: acknowledge agent switch %s target: %w", sw.ID, err)
	}
	return nil
}

// toolFlight tracks one session's in-flight tool executions and the pending
// permission dialog's identity, so a sticky `blocked` is cleared by the post
// of the exact approved tool — and by nothing else tool-shaped. Answering a
// permission dialog fires no hook of its own, so the approved tool's
// post-tool-use is the earliest observable "the decision was resolved"
// signal; but tool hooks also fire for parallel subagents on the same
// session, whose traffic must never clear a dialog that is still on screen.
// In-memory only: a daemon restart loses it and the session degrades to
// clearing at the next turn boundary — fail-safe staleness, never a spurious
// clear.
type toolFlight struct {
	// inflight maps toolUseID -> toolName for pre-tool-use signals whose post
	// has not arrived yet.
	inflight map[string]string
	// blockedCandidate is the tool-use id of the UNIQUE in-flight tool bearing
	// the dialog's tool_name when it appeared — the tool whose post proves the
	// dialog was answered. Empty when no in-flight tool matched, or when the
	// match was ambiguous (two same-name tools in flight: the permission
	// payload carries no tool_use_id to disambiguate, so a sibling's post must
	// NOT be mistaken for the approval). Either way, empty means nothing
	// tool-shaped may clear the block and it lifts only at a turn boundary.
	blockedCandidate string
}

// maxInflightTools caps a session's in-flight map so lost posts cannot grow
// it without bound; hitting the cap resets the map, degrading that session to
// turn-boundary clearing (fail-safe).
const maxInflightTools = 128

// isToolUseEvent reports whether the AO hook event is one of the tool-use
// trio whose signals must not demote a sticky state on their own.
func isToolUseEvent(event string) bool {
	return event == "pre-tool-use" || isPostToolUseEvent(event)
}

func isPostToolUseEvent(event string) bool {
	// post-tool-use-fail is retained for Kimchi hook files installed before the
	// adapter switched to AO's canonical failure event name.
	return event == "post-tool-use" || event == "post-tool-use-failure" || event == "post-tool-use-fail"
}

// isTurnBoundaryEvent reports the events that reliably mean the pending
// dialog is gone: a prompt cannot be submitted while a dialog holds the
// composer, and a turn cannot end (or the session exit) with one on screen.
func isTurnBoundaryEvent(event string) bool {
	return event == "user-prompt-submit" || event == "stop" || event == "session-end" ||
		event == "process-exited" || event == "chat.controller.stopped"
}

// turnCompleted reports whether a signal is an agent telling AO that its turn
// is over: a turn-boundary event that leaves no work in flight.
//
// Both halves are load-bearing (Checkpoint 8P-E.14A). The event half is what
// makes this "the agent said it finished" rather than "the agent looks quiet":
// an untagged idle signal from an old CLI, a runtime probe, or any state write
// that is not a reported turn boundary is deliberately not a completion, and a
// task that is merely between tool calls keeps its branch. The state half is
// what keeps a turn that paused on the user (waiting_input / blocked) mid-turn:
// a human owes it an answer, the task is still going, and its branch is still
// its own.
//
// user-prompt-submit is a turn boundary too, but of the opposite kind — see
// turnStarted.
func turnCompleted(s ports.ActivitySignal) bool {
	if !s.Valid || s.State.WorkInFlight() {
		return false
	}
	return isTurnBoundaryEvent(s.Event) && s.Event != "user-prompt-submit"
}

// turnStarted reports whether a signal is an agent beginning a new turn — the
// moment a task session needs its branch back.
func turnStarted(s ports.ActivitySignal) bool {
	return s.Valid && s.Event == "user-prompt-submit" && s.State == domain.ActivityActive
}

// turnSucceeded reports whether a signal is the agent stating that the work it
// was given is finished: it answered, it has nothing in flight, and it is
// waiting for whatever comes next. This is the durable evidence behind the
// Completed status (SessionRecord.TurnCompletedAt).
//
// It is deliberately narrower than turnCompleted, which also covers the ways an
// agent STOPS EXISTING. session-end, process-exited and chat.controller.stopped
// are teardown: the process went away, which is a reason to hand the branch
// back but says nothing about whether the work got done — and each of them
// lands activity=exited, which the status derivation surfaces as Exited on its
// own. What remains are the two events a harness emits to say "turn over, I am
// still here": the Stop-class hook every TUI adapter installs, and the Chat
// driver's turn-completed projection.
//
// The honest limit: a user interrupt also produces a Stop hook, and no harness
// distinguishes the two on the wire. What AO can prove is that the agent
// reported the end of its turn rather than merely going quiet, and that is the
// line this draws. Everything that IS distinguishable — a crash, a kill, a
// cancellation, a question back to the user, a failing check — is already a
// different state before the derivation ever reaches Completed.
func turnSucceeded(s ports.ActivitySignal) bool {
	if !s.Valid || s.State != domain.ActivityIdle {
		return false
	}
	return s.Event == "stop" || s.Event == "chat.turn.completed"
}

// applyToolPrecedenceLocked folds an event-tagged activity signal through the
// session's tool-flight state and decides whether its state write may
// proceed. Returned signal with Valid=false means "suppressed": the tracking
// side effects have landed but the state must not change. Signals without an
// Event pass through untouched — the compatibility contract for old CLIs and
// for adapters that don't tag their signals (their last-writer-wins semantics
// are pinned by tests). Caller must hold m.mu.
func (m *Manager) applyToolPrecedenceLocked(id domain.SessionID, cur domain.ActivityState, s ports.ActivitySignal) ports.ActivitySignal {
	if s.Event == "" {
		return s
	}
	suppressed := s
	suppressed.Valid = false

	fl := m.flights[id]
	ensure := func() *toolFlight {
		if fl == nil {
			fl = &toolFlight{inflight: map[string]string{}}
			m.flights[id] = fl
		}
		return fl
	}

	// Tracking side effects happen regardless of what the state decision is.
	switch s.Event {
	case "pre-tool-use":
		if s.ToolUseID != "" {
			f := ensure()
			if len(f.inflight) >= maxInflightTools {
				f.inflight = map[string]string{}
			}
			f.inflight[s.ToolUseID] = s.ToolName
		}
	case "post-tool-use", "post-tool-use-failure", "post-tool-use-fail":
		if fl != nil {
			delete(fl.inflight, s.ToolUseID)
		}
	}

	switch {
	case s.State == domain.ActivityBlocked:
		// Entering (or re-asserting) blocked: snapshot the dialog's identity.
		// permission-request carries the blocking tool_name; the Notification
		// duplicate does not and must not wipe an existing snapshot.
		//
		// The permission hook payload does not carry the blocking tool's
		// tool_use_id (only its name), so we can only identify the blocking
		// tool unambiguously when EXACTLY ONE in-flight tool bears that name.
		// With two same-name tools in flight (a batch of Bash calls, one of
		// them the one at the dialog), a sibling's post could otherwise clear
		// a still-open dialog — the exact permission-bypass this change exists
		// to prevent. So we correlate only in the unique case; otherwise no
		// candidate is recorded and the block clears only at a turn boundary
		// (fail-closed).
		f := ensure()
		// Recompute only when this signal identifies a dialog. Claude can emit an
		// identity-less Notification duplicate after permission-request; that
		// duplicate must not erase the candidate captured by the first signal.
		if s.ToolUseID != "" || s.ToolName != "" {
			f.blockedCandidate = ""
		}
		if s.ToolUseID != "" {
			// If the blocking signal carries a tool_use_id that is in the
			// inflight map, use it directly — this is more precise than a
			// name match and handles adapters whose notification payloads
			// use a different tool_name casing than their PreToolUse/PostToolUse
			// payloads (e.g. Kimchi: "bash" in notification vs "Bash" in hooks).
			if _, ok := f.inflight[s.ToolUseID]; ok {
				f.blockedCandidate = s.ToolUseID
			}
		}
		if f.blockedCandidate == "" && s.ToolName != "" {
			for useID, name := range f.inflight {
				if name != s.ToolName {
					continue
				}
				if f.blockedCandidate != "" {
					// A second same-name tool: ambiguous, fail closed by
					// leaving no candidate (only a turn boundary clears).
					f.blockedCandidate = ""
					break
				}
				f.blockedCandidate = useID
			}
		}
		return s

	case cur == domain.ActivityBlocked:
		// Paused on a decision: only a turn boundary or the correlated post
		// may change the state.
		switch {
		case isTurnBoundaryEvent(s.Event):
			delete(m.flights, id)
			return s
		case isPostToolUseEvent(s.Event) &&
			fl != nil && fl.blockedCandidate != "" && s.ToolUseID == fl.blockedCandidate:
			// The single unambiguous blocking tool finished: the dialog was
			// answered. Clear the candidate so a later dialog in the same turn
			// starts from a clean slate.
			fl.blockedCandidate = ""
			return s
		default:
			// Subagent/sibling tool traffic (including a same-name sibling when
			// the block was ambiguous), notification sub-types (idle_prompt,
			// agent_completed), and anything else that is not proof the dialog
			// closed.
			return suppressed
		}

	case cur.IsSticky() && isToolUseEvent(s.Event):
		// waiting_input: background tool traffic must not clear the "waiting
		// on the user" marker; only an explicit user/turn signal does.
		return suppressed

	default:
		if isTurnBoundaryEvent(s.Event) {
			delete(m.flights, id)
		}
		return s
	}
}

func (m *Manager) waitingInputEvents(next domain.SessionRecord, prevState domain.ActivityState, prevAt, now time.Time) []ports.TelemetryEvent {
	if m.telemetry == nil {
		return nil
	}
	projectID := next.ProjectID
	sessionID := next.ID
	var events []ports.TelemetryEvent
	// Entry/exit is measured on the needs-input family boundary (waiting_input
	// or blocked): the event names stay waiting_input_* for dashboard
	// continuity, the payload state distinguishes the two, and an in-family
	// transition emits neither event so dwell covers the whole pause.
	if !prevState.NeedsInput() && next.Activity.State.NeedsInput() && !next.IsTerminated {
		events = append(events, ports.TelemetryEvent{
			Name:       "ao.session.waiting_input_entered",
			Source:     "lifecycle",
			OccurredAt: now.UTC(),
			Level:      ports.TelemetryLevelInfo,
			ProjectID:  &projectID,
			SessionID:  &sessionID,
			Payload: map[string]any{
				"state": string(next.Activity.State),
			},
		})
	}
	if prevState.NeedsInput() && !next.Activity.State.NeedsInput() {
		payload := map[string]any{
			"state":     string(next.Activity.State),
			"dwell_ms":  now.Sub(prevAt).Milliseconds(),
			"exited_to": string(next.Activity.State),
		}
		events = append(events, ports.TelemetryEvent{
			Name:       "ao.session.waiting_input_exited",
			Source:     "lifecycle",
			OccurredAt: now.UTC(),
			Level:      ports.TelemetryLevelInfo,
			ProjectID:  &projectID,
			SessionID:  &sessionID,
			Payload:    payload,
		})
	}
	return events
}

func (m *Manager) emitTelemetry(ctx context.Context, ev ports.TelemetryEvent) {
	if m.telemetry == nil {
		return
	}
	m.telemetry.Emit(ctx, ev)
}

func (m *Manager) emitNotification(ctx context.Context, intent *ports.NotificationIntent) {
	if intent == nil || m.notifications == nil {
		return
	}
	if err := m.notifications.Notify(ctx, *intent); err != nil {
		slog.Default().Warn("lifecycle: notification failed", "session", intent.SessionID, "type", intent.Type, "err", err)
	}
}

// recordSessionFact hands one durable session-level fact to the notification
// producer. Best-effort like emitNotification: the fact is already persisted by
// its own authority, so failing to notify about it must never fail the
// lifecycle write that observed it.
func (m *Manager) recordSessionFact(ctx context.Context, fact ports.SessionFact) {
	if m.sessionFacts == nil || fact.Kind == "" {
		return
	}
	if err := m.sessionFacts.Record(ctx, fact); err != nil {
		slog.Default().Warn(
			"lifecycle: session fact notification failed",
			"session", fact.SessionID, "fact", fact.Kind, "err", err,
		)
	}
}

// resolveNotifications closes notifications the just-written facts made stale.
// Best-effort like emitNotification: a failed resolve must never fail the
// lifecycle write that produced it.
func (m *Manager) resolveNotifications(ctx context.Context, resolutions ...ports.NotificationResolution) {
	if m.notifications == nil {
		return
	}
	for _, res := range resolutions {
		if err := m.notifications.Resolve(ctx, res); err != nil {
			slog.Default().Warn(
				"lifecycle: notification resolve failed",
				"session", res.SessionID, "pr", res.PRURL, "type", res.Type, "err", err,
			)
		}
	}
}

// MarkSpawned marks a newly spawned or restored session live and stores runtime/workspace handles.
func (m *Manager) MarkSpawned(ctx context.Context, id domain.SessionID, metadata domain.SessionMetadata) error {
	launchID := strings.TrimSpace(metadata.RuntimeLaunchID)
	reactivator, err := func() (sessionUsageReactivator, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		defer m.finishLaunchLocked(id, launchID)
		rec, ok, err := m.store.GetSession(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("lifecycle: MarkSpawned for unknown session %q", id)
		}
		now := m.clock()
		rec.IsTerminated = false
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
		// Each spawn/restore must re-prove its hook pipeline: clear the receipt so
		// a relaunch with broken hooks degrades to no_signal instead of inheriting
		// a stale "signals worked once" fact.
		rec.FirstSignalAt = time.Time{}
		rec.Metadata = mergeMetadata(rec.Metadata, metadata)
		rec.UpdatedAt = now
		if err := m.store.UpdateSession(ctx, rec); err != nil {
			return nil, err
		}
		return m.usageReactivator, nil
	}()
	if err != nil {
		return err
	}
	reactivateSessionUsage(ctx, id, launchID, reactivator)
	return nil
}

// CommitControllerEpoch atomically changes which controller owns a live
// session. Session Manager coordinates the external-process saga, but only
// Lifecycle Manager is allowed to write the durable controller/activity facts.
// A false result means the expected source controller no longer owns the row.
// startFresh is accepted only with an empty native id; Session Manager sets it
// after an adapter proved the reserved id has no persisted conversation.
func (m *Manager) CommitControllerEpoch(
	ctx context.Context,
	id domain.SessionID,
	source, target domain.SessionMode,
	nativeConversationID string,
	startFresh bool,
) (bool, error) {
	if !source.Valid() || !target.Valid() || source == target {
		return false, fmt.Errorf("lifecycle: invalid controller epoch %q -> %q", source, target)
	}
	nativeConversationID = strings.TrimSpace(nativeConversationID)
	if nativeConversationID == "" && !startFresh {
		return false, fmt.Errorf("lifecycle: controller epoch for %q has no native conversation id", id)
	}
	if nativeConversationID != "" && startFresh {
		return false, fmt.Errorf("lifecycle: fresh controller epoch for %q also supplied a native conversation id", id)
	}
	writer, ok := m.store.(controllerEpochStore)
	if !ok {
		return false, fmt.Errorf("lifecycle: controller epoch persistence is unavailable")
	}

	m.mu.Lock()
	previous, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		m.mu.Unlock()
		return false, err
	}
	if !found {
		m.mu.Unlock()
		return false, fmt.Errorf("%w: %s", ports.ErrSessionNotFound, id)
	}
	if previous.IsTerminated || domain.NormalizeSessionMode(previous.Mode) != source {
		m.mu.Unlock()
		return false, nil
	}
	now := m.clock()
	changed, err := writer.CommitSessionControllerEpoch(
		ctx, id, source, target, nativeConversationID, now,
	)
	if err != nil || !changed {
		m.mu.Unlock()
		return changed, err
	}

	// Mirror the atomic store write for lifecycle side effects. MarkSpawned will
	// clear FirstSignalAt and attach the target's process generation once the new
	// controller is actually live.
	next := previous
	next.Mode = target
	next.Metadata.RuntimeHandleID = ""
	next.Metadata.RuntimeLaunchID = ""
	next.Metadata.AgentSessionID = nativeConversationID
	next.Metadata.ProviderConversationID = nativeConversationID
	next.Metadata.ControllerGeneration = ""
	next.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: now}
	next.UpdatedAt = now
	delete(m.flights, id)
	resolutions := needsInputResolutions(previous, next, now)
	waitingEvents := m.waitingInputEvents(
		next, previous.Activity.State, previous.Activity.LastActivityAt, now,
	)
	m.mu.Unlock()

	for _, ev := range waitingEvents {
		m.emitTelemetry(ctx, ev)
	}
	m.resolveNotifications(ctx, resolutions...)
	return true, nil
}

// ConfirmAgentSwitchSourceStopped records that the source process is gone and
// moves the switch saga across the source-stop boundary in the same store
// transaction. Session Manager coordinates the process; Lifecycle Manager owns
// the durable activity-state write.
func (m *Manager) ConfirmAgentSwitchSourceStopped(
	ctx context.Context,
	confirmation domain.AgentSwitchSourceStopConfirmation,
) (bool, error) {
	writer, ok := m.store.(agentSwitchSourceStopStore)
	if !ok {
		return false, fmt.Errorf("lifecycle: agent-switch source-stop persistence is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return writer.ConfirmAgentSwitchSourceStopped(ctx, confirmation)
}

// ActivateAgentSwitchTarget atomically transfers the session owner to the
// target process and advances the switch saga. Keeping this command on
// Lifecycle Manager preserves the canonical write boundary without splitting
// the store's all-or-nothing transaction.
func (m *Manager) ActivateAgentSwitchTarget(
	ctx context.Context,
	activation domain.AgentSwitchTargetActivation,
) (bool, error) {
	writer, ok := m.store.(agentSwitchTargetActivationStore)
	if !ok {
		return false, fmt.Errorf("lifecycle: agent-switch target activation persistence is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return writer.ActivateAgentSwitchTarget(ctx, activation)
}

// MarkTerminated marks a session terminated. Runtime/workspace teardown is the
// caller's responsibility (see session_manager.Manager.Kill); this also reaps the
// session's Docker containers via the optional ContainerReaper (#2652) as its one
// built-in external side effect.
func (m *Manager) MarkTerminated(ctx context.Context, id domain.SessionID) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rec, ok, err := m.store.GetSession(ctx, id)
		if err != nil || !ok {
			return err
		}
		if rec.IsTerminated {
			m.releaseSessionBranchLocks(ctx, id, "task session terminated")
			m.reapSessionContainers(ctx, id)
			return nil
		}

		launchID := rec.Metadata.RuntimeLaunchID
		sessionRevision := rec.UpdatedAt
		m.mu.Lock()
		finalizer := m.usageFinalizer
		m.mu.Unlock()
		finalizeSessionUsage(ctx, id, launchID, sessionRevision, finalizer)

		const (
			terminationChanged = iota
			terminationApplied
			terminationAlreadyApplied
			terminationLaunchChanged
		)
		outcome := terminationChanged
		err = m.mutate(ctx, id, func(cur domain.SessionRecord, now time.Time) (domain.SessionRecord, bool) {
			switch {
			case cur.IsTerminated:
				outcome = terminationAlreadyApplied
				return cur, false
			case cur.Metadata.RuntimeLaunchID != launchID:
				outcome = terminationLaunchChanged
				return cur, false
			case !cur.UpdatedAt.Equal(sessionRevision):
				return cur, false
			default:
				cur.IsTerminated = true
				cur.Activity = domain.Activity{State: domain.ActivityExited, LastActivityAt: now}
				delete(m.flights, id) // runs under m.mu (mutate holds it)
				outcome = terminationApplied
				return cur, true
			}
		})
		if err != nil {
			return err
		}
		switch outcome {
		case terminationApplied, terminationAlreadyApplied:
			m.releaseSessionBranchLocks(ctx, id, "task session terminated")
			m.reapSessionContainers(ctx, id)
			return nil
		case terminationLaunchChanged:
			return fmt.Errorf("lifecycle: runtime launch changed while terminating session %q", id)
		default:
			// A same-launch activity transition changed UpdatedAt after usage was
			// finalized. Retry from a fresh snapshot so termination and usage
			// finalization commit against the same durable revision.
			continue
		}
	}
}

// reapSessionContainers is the container leg of #2652 (the container-owning
// counterpart to session_manager.Manager's cleanupAgentWorkspace): every
// MarkTerminated call - Kill, daemon-shutdown teardown, Cleanup,
// RetireForReplacement, and tracker-driven termination - funnels through
// here, so this single hook covers every terminal-state path rather than
// only explicit ao session kill. Best-effort: logged on failure, never
// returned, matching the rest of AO's terminal-state teardown. A project-load
// error skips reaping rather than guessing - the package's stated bias is to
// spare on ambiguity, not to reap on it.
func (m *Manager) reapSessionContainers(ctx context.Context, id domain.SessionID) {
	if m.containers == nil {
		return
	}
	if m.projects != nil {
		rec, ok, err := m.store.GetSession(ctx, id)
		if err != nil || !ok {
			slog.Default().Warn("lifecycle: container reap: session lookup failed, skipping", "session", id, "err", err)
			return
		}
		project, ok, err := m.projects.GetProject(ctx, string(rec.ProjectID))
		if err != nil || !ok {
			slog.Default().Warn("lifecycle: container reap: project lookup failed or missing, skipping rather than guessing", "session", id, "project", rec.ProjectID, "err", err)
			return
		}
		if project.Config.ContainerReap.Disabled {
			return
		}
	}
	removed, err := m.containers.ReapSessionContainers(ctx, id)
	if err != nil {
		slog.Default().Warn("lifecycle: container reap failed", "session", id, "err", err)
		return
	}
	if removed > 0 {
		slog.Default().Info("lifecycle: reaped session containers", "session", id, "removed", removed)
	}
}

func finalizeSessionUsage(
	ctx context.Context,
	id domain.SessionID,
	expectedRuntimeLaunchID string,
	expectedSessionRevision time.Time,
	finalizer sessionUsageFinalizer,
) {
	if finalizer == nil {
		return
	}
	if err := finalizer.FinalizeSession(ctx, id, expectedRuntimeLaunchID, expectedSessionRevision); err != nil {
		slog.Default().Warn("lifecycle: finalize session usage before termination", "session", id, "err", err)
	}
}

func reactivateSessionUsage(
	ctx context.Context,
	id domain.SessionID,
	expectedRuntimeLaunchID string,
	reactivator sessionUsageReactivator,
) {
	if reactivator == nil {
		return
	}
	if err := reactivator.ReactivateSession(ctx, id, expectedRuntimeLaunchID); err != nil {
		slog.Default().Warn("lifecycle: reactivate session usage after launch", "session", id, "err", err)
	}
}

// sameActivity reports whether two activity signals describe the same state.
// LastActivityAt is intentionally ignored: same-state repeats (e.g. a stream
// of idle notifications) must not rewrite UpdatedAt or fan out a CDC event.
// LastActivityAt now marks when this state was first entered since the last
// transition, which is the timestamp a UI actually wants.
func sameActivity(a, b domain.Activity) bool {
	return a.State == b.State
}

func mergeMetadata(base, in domain.SessionMetadata) domain.SessionMetadata {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&base.Branch, in.Branch)
	set(&base.WorkspacePath, in.WorkspacePath)
	set(&base.WorkspaceRepoPath, in.WorkspaceRepoPath)
	set(&base.RuntimeHandleID, in.RuntimeHandleID)
	base.RuntimeLaunchID = in.RuntimeLaunchID
	// Assigned rather than set, and assigned TOGETHER with the launch id above,
	// for the reason Runtime GC depends on: the incarnation and the ownership
	// token describe ONE launch generation, and the token is only meaningful
	// against the launch it was minted for (domain.RuntimeOwnedBySession). A
	// `set`-style merge would carry a previous launch's incarnation forward
	// past a relaunch, which is precisely the stale-handle adoption P1-D §C
	// exists to prevent — and leaving them out altogether, as this function did
	// until now, left every session row with an empty incarnation and an empty
	// token however carefully spawn had computed them. Runtime GC reads exactly
	// those two columns to prove a terminated worker's runtime is AO's own
	// (runtimegc.Sweeper.sessionCandidates), so dropping them here made every
	// worker and reviewer permanently unprovable and reclaimed nothing in
	// production, while the sweeper's own tests passed against records built by
	// hand. The seam is now covered by
	// runtime_ownership_persistence_test.go, which carries the incident detail.
	base.RuntimeInstanceID = in.RuntimeInstanceID
	base.RuntimeOwnerToken = in.RuntimeOwnerToken
	set(&base.AgentSessionID, in.AgentSessionID)
	set(&base.Prompt, in.Prompt)
	set(&base.LatestUserPrompt, in.LatestUserPrompt)
	set(&base.LatestAssistantUpdate, in.LatestAssistantUpdate)
	set(&base.NativeTranscriptPath, in.NativeTranscriptPath)
	set(&base.BrowserCapabilityVerifier, in.BrowserCapabilityVerifier)
	// The chat controller's resume handle. Without this a restart has no thread to
	// resume and the conversation is stranded — the provider still holds it, but
	// AO no longer knows its id.
	set(&base.ProviderConversationID, in.ProviderConversationID)
	// Assigned rather than set: a relaunch rotates the generation, and the whole
	// point is that the new value replaces the old one so events from the
	// controller this one superseded can be told apart.
	base.ControllerGeneration = in.ControllerGeneration
	return base
}

func applyActivityMetadata(meta *domain.SessionMetadata, signal ports.ActivitySignal) {
	if signal.AgentSessionID != "" {
		meta.AgentSessionID = signal.AgentSessionID
	}
	if signal.LatestUserPrompt != "" {
		meta.LatestUserPrompt = signal.LatestUserPrompt
	}
	if signal.LatestAssistantUpdate != "" {
		meta.LatestAssistantUpdate = signal.LatestAssistantUpdate
	}
	if signal.TranscriptPath != "" {
		meta.NativeTranscriptPath = signal.TranscriptPath
	}
}
