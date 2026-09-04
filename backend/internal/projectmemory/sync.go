package projectmemory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// sync.go — P2-B's lifecycle trigger and its single-flight (§2, §3).
//
// P2-A deliberately left Index/Sync with no automatic caller, because *when* to
// spend a scan is a cost question. This file answers it, and the answer has to
// survive the shape of a real workflow: a Planner, a Worker, a Reviewer and a
// Repair Agent all reach for the same repository at the same commit within
// seconds of each other. Four boundaries must not become four scans.
//
// Three rules make that safe:
//
//   - **One real sync per (repo, commit).** Concurrent callers on the same
//     authoritative state coalesce onto one in-flight pass; the rest wait for
//     its result and reuse it. A caller arriving after it finished sees the
//     freshness marker and does no work at all.
//   - **Generation fencing survives coalescing.** A waiter reuses a result only
//     if the result is for the commit it asked about. If HEAD moved while the
//     pass ran, the stale result is not promoted to anybody — the waiter is told
//     what actually happened and decides for itself.
//   - **A sync never blocks a dispatch.** Every wait is bounded by
//     Config.SyncTimeout, and every failure yields a degraded-but-usable answer
//     with a stated reason. Memory is an optimisation; a dispatch that cannot
//     have it proceeds on legacy context.
//
// The single-flight is in-process. That is the correct scope, not a shortcut:
// the durable cross-process guard already exists one layer down, in the
// generation-conditioned pass claim (`ClaimProjectMemoryIndexPass` succeeds
// only from a terminal phase). A second daemon racing this one loses that
// claim and is told so; this map exists to stop *one* daemon's four roles from
// queueing behind each other for a pass that is already running.

// SyncKind is what a freshness check actually had to do. It is the field an
// operator reads to answer "is the warm path warm".
type SyncKind string

// Sync kinds.
const (
	// SyncNone means memory was already at the authoritative commit. This is
	// the warm path, and it costs one indexed row read.
	SyncNone SyncKind = "none"
	// SyncIncremental means a change set was applied — only the paths a diff
	// named were read.
	SyncIncremental SyncKind = "incremental"
	// SyncFull means a full bounded pass ran: either the repository had never
	// been indexed, or no trustworthy change set could be obtained.
	SyncFull SyncKind = "full"
	// SyncCoalesced means another caller's sync was in flight for this exact
	// state and its result was reused.
	SyncCoalesced SyncKind = "coalesced"
	// SyncSkipped means no sync was attempted or none could complete. Reason
	// says why, and the caller proceeds on whatever memory already exists.
	SyncSkipped SyncKind = "skipped"
)

// Freshness is the outcome of a lifecycle freshness check.
type Freshness struct {
	// RepoID identifies the repository this describes.
	RepoID string
	// Kind is what the check had to do.
	Kind SyncKind
	// Reason explains a skip or a fallback, and is empty otherwise.
	Reason string
	// Generation and IndexedCommit are the memory's provenance afterwards.
	Generation    int64
	IndexedCommit string
	// Current reports whether memory is PROVABLY at the commit the caller
	// asked about. It requires a commit to compare: a checkout that reports
	// none can never set it, because there is nothing to prove currency
	// against.
	Current bool
	// Usable reports that a pass has completed and memory may be served. It is
	// the weaker, and more widely applicable, claim: a project with no commit
	// (a scratch directory, a checkout with no history) still gets memory, it
	// simply cannot prove that memory is current and so re-syncs each time.
	Usable bool
	// Duration is how long the check took, including any wait.
	Duration time.Duration
	// FilesRead counts paths this check actually opened. It is the honest
	// measure of what the warm path costs, and on a no-op sync it is zero.
	FilesRead int
	// Graph is what the code-graph sync did, when one is wired. A zero value
	// with Attempted false means no structural graph is configured, which is
	// the pre-phase behaviour and not a degradation.
	Graph GraphFreshness
}

// Healthy reports whether memory may be served for this dispatch.
//
// It is Usable rather than Current on purpose. Currency is the stronger claim
// and is what lets the warm path skip work; usability is what lets memory
// exist at all for a repository whose commit AO cannot read. Withholding
// memory from such a project would be failing closed against a condition that
// is not a staleness — the facts are as valid as any others, AO just has to
// re-confirm them rather than assume them.
func (f Freshness) Healthy() bool { return f.Usable }

// syncKey is the authority a sync is identified by. Two callers share a sync
// only when all four parts agree — a different commit is a different sync, and
// coalescing across commits is exactly the bug generation fencing exists to
// prevent.
type syncKey struct {
	project domain.ProjectID
	repoID  string
	commit  string
	branch  string
}

func (k syncKey) String() string {
	return fmt.Sprintf("%s/%s@%s(%s)", k.project, k.repoID, orNone(k.commit), orNone(k.branch))
}

// syncFlight is one in-flight sync other callers may join.
type syncFlight struct {
	done   chan struct{}
	result Freshness
}

// Syncer keeps a project's memory in step with its repositories at the
// lifecycle boundaries, coalescing concurrent demand.
type Syncer struct {
	svc  *Service
	cfg  Config
	now  func() time.Time
	head func(ctx context.Context, repoPath string) (commit, branch string)
	// linkedWorktree is the P2-E guard, injectable so a test can exercise the
	// refusal without building a real linked worktree.
	linkedWorktree func(ctx context.Context, path string) (string, bool)
	log            *slog.Logger
	mu             sync.Mutex
	flight         map[syncKey]*syncFlight
	// stats are process-lifetime counters an operator surface can read to see
	// whether coalescing is doing anything.
	stats SyncStats
}

// SyncStats counts what the syncer did over the process's lifetime.
type SyncStats struct {
	Checks      int64
	Coalesced   int64
	NoOp        int64
	Incremental int64
	Full        int64
	Skipped     int64
	TimedOut    int64
	// WorktreeRefused counts freshness checks that named a linked worktree
	// instead of a repository root. It is a counter rather than only a log
	// line because a non-zero value is a caller bug, not a repository state --
	// see EnsureFresh.
	WorktreeRefused int64
}

// NewSyncer builds the lifecycle syncer over a memory service.
func NewSyncer(svc *Service, cfg Config) *Syncer {
	return &Syncer{
		svc:            svc,
		cfg:            cfg,
		now:            func() time.Time { return time.Now().UTC() },
		head:           HeadOf,
		linkedWorktree: LinkedWorktreeOf,
		log:            svc.log,
		flight:         map[syncKey]*syncFlight{},
	}
}

// Stats returns a snapshot of the process-lifetime counters.
func (s *Syncer) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// EnsureFresh is the entry point every lifecycle boundary calls.
//
// It is deliberately the only entry point. Planner, Worker, Reviewer and
// Repair all ask the same question — "is memory current for this repository at
// this commit" — and giving each its own trigger would give each its own
// opinion about when a scan is worth it. They differ in what they do with the
// answer, not in how they get it.
//
// It never returns an error. Every failure is a Freshness with a reason and
// Current=false, because there is no failure of memory that should stop a
// dispatch.
func (s *Syncer) EnsureFresh(ctx context.Context, projectID domain.ProjectID, repoPath string) Freshness {
	started := s.now()
	s.bump(func(st *SyncStats) { st.Checks++ })

	if !s.cfg.Mode.Enabled() {
		return Freshness{Kind: SyncSkipped, Reason: "project memory is switched off", Duration: s.now().Sub(started)}
	}
	canonical, err := canonicalRepoPath(repoPath)
	if err != nil {
		s.bump(func(st *SyncStats) { st.Skipped++ })
		return Freshness{
			Kind: SyncSkipped, Duration: s.now().Sub(started),
			Reason: "the repository path could not be resolved: " + err.Error(),
		}
	}

	// P2-E: a linked worktree is not a repository, and canonical memory is
	// never derived from one.
	//
	// This is the LAST line of defence rather than the fix -- the fix is that
	// callers pass the canonical repository root (see wfmemory's reviewer
	// path). It lives here because EnsureFresh is the single funnel every
	// canonical index passes through, so one check here closes the whole
	// class: no future caller can mint a second repo_id for a workspace by
	// passing the wrong path, which is exactly how the P2-D production gate
	// found 288 canonical facts derived from unintegrated ao/* branches.
	//
	// It refuses rather than silently re-pointing at the parent repository.
	// Re-pointing would be a guess about which project the caller meant, and
	// this subsystem's whole argument is that it does not guess: the caller
	// holds the project record and is the authority on its root.
	if parent, linked := s.linkedWorktree(ctx, canonical); linked {
		s.bump(func(st *SyncStats) { st.WorktreeRefused++; st.Skipped++ })
		if s.log != nil {
			s.log.Warn("project memory: refused to index a linked worktree as a repository",
				"worktree", canonical, "repository", parent)
		}
		return Freshness{
			Kind: SyncSkipped, Duration: s.now().Sub(started),
			Reason: fmt.Sprintf(
				"%s is a linked worktree of %s, not a repository; canonical memory is only ever derived from the repository root",
				canonical, orNone(parent)),
		}
	}

	repoID := domain.ProjectMemoryRepoID(canonical)

	commit, branch := s.head(ctx, canonical)
	key := syncKey{project: projectID, repoID: repoID, commit: commit, branch: branch}

	// The warm path, and the one that has to be cheap: a single indexed row
	// read that answers "already there" without touching the filesystem.
	if fresh, ok := s.alreadyCurrent(ctx, projectID, repoID, commit); ok {
		s.bump(func(st *SyncStats) { st.NoOp++ })
		// Project memory is current; the code graph may still not be, because
		// it can be enabled, rebuilt or purged independently. Its own warm
		// path is one row read, so asking is nearly free and never asking
		// would leave a project permanently unindexed after memory warmed.
		fresh.Graph = s.syncGraph(ctx, projectID, canonical, repoID, commit, branch)
		fresh.Duration = s.now().Sub(started)
		return fresh
	}

	flight, leader := s.join(key)
	if !leader {
		s.bump(func(st *SyncStats) { st.Coalesced++ })
		return s.wait(ctx, flight, key, started)
	}
	defer s.leave(key, flight)

	result := s.runSync(ctx, projectID, canonical, repoID, commit, branch)
	result.Duration = s.now().Sub(started)
	flight.result = result
	close(flight.done)
	return result
}

// alreadyCurrent answers the warm case from durable state alone.
func (s *Syncer) alreadyCurrent(
	ctx context.Context, projectID domain.ProjectID, repoID, commit string,
) (Freshness, bool) {
	if commit == "" {
		// Without a commit AO cannot prove currency, so it does not claim it.
		return Freshness{}, false
	}
	state, found, err := s.svc.repo.GetProjectMemoryIndexState(ctx, projectID, repoID)
	if err != nil || !found {
		return Freshness{}, false
	}
	if state.IndexedCommit != commit || state.Phase != domain.IndexPhaseIdle {
		return Freshness{}, false
	}
	return Freshness{
		RepoID: repoID, Kind: SyncNone, Generation: state.Generation,
		IndexedCommit: state.IndexedCommit, Current: true, Usable: true,
	}, true
}

// join registers this caller against the in-flight sync for key, reporting
// whether it is the one that must perform it.
func (s *Syncer) join(key syncKey) (*syncFlight, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.flight[key]; ok {
		return existing, false
	}
	flight := &syncFlight{done: make(chan struct{})}
	s.flight[key] = flight
	return flight, true
}

// leave releases the leader's slot, but only if it still holds it — a slot
// already replaced belongs to a newer sync and must not be removed.
func (s *Syncer) leave(key syncKey, flight *syncFlight) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.flight[key]; ok && cur == flight {
		delete(s.flight, key)
	}
}

// wait blocks for the leader's result, bounded by the sync timeout and the
// caller's own context.
//
// A waiter that times out is NOT an error: it proceeds with whatever memory
// already exists, and the leader keeps working for whoever comes next. That is
// the difference between an optimisation and a dependency.
func (s *Syncer) wait(ctx context.Context, flight *syncFlight, key syncKey, started time.Time) Freshness {
	timer := time.NewTimer(s.cfg.SyncTimeout)
	defer timer.Stop()
	select {
	case <-flight.done:
		result := flight.result
		result.Kind = SyncCoalesced
		result.Duration = s.now().Sub(started)
		// FilesRead belongs to the caller that actually read them. A waiter
		// that reused a result read nothing, and reporting otherwise would
		// double-count the cost of one pass across four roles.
		result.FilesRead = 0
		if result.Reason == "" {
			result.Reason = "reused the sync already in flight for " + key.String()
		}
		return result
	case <-timer.C:
		s.bump(func(st *SyncStats) { st.TimedOut++ })
		return Freshness{
			RepoID: key.repoID, Kind: SyncSkipped, Duration: s.now().Sub(started),
			Reason: fmt.Sprintf("waited %s for an in-flight sync and gave up; dispatching on existing context", s.cfg.SyncTimeout),
		}
	case <-ctx.Done():
		return Freshness{
			RepoID: key.repoID, Kind: SyncSkipped, Duration: s.now().Sub(started),
			Reason: "the caller's context ended while waiting for a sync",
		}
	}
}

// runSync performs the real work, under the configured timeout.
//
// The timeout is applied to a DERIVED context rather than the caller's, so a
// sync that overruns is abandoned without cancelling the dispatch that
// triggered it.
func (s *Syncer) runSync(
	ctx context.Context, projectID domain.ProjectID, repoPath, repoID, commit, branch string,
) Freshness {
	syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.SyncTimeout)
	defer cancel()

	fresh := s.runMemorySync(syncCtx, ctx, projectID, repoPath, repoID, commit, branch)
	// The graph is brought to the same commit AFTER memory and never instead
	// of it. It runs on the same derived context, so it inherits the timeout
	// and the detachment from the caller; and its result is attached rather
	// than merged, because a graph that failed must not make memory look
	// unhealthy.
	fresh.Graph = s.syncGraph(syncCtx, projectID, repoPath, repoID, commit, branch)
	return fresh
}

// runMemorySync is the project-memory half of a sync.
func (s *Syncer) runMemorySync(
	syncCtx, ctx context.Context, projectID domain.ProjectID, repoPath, repoID, commit, branch string,
) Freshness {

	out, err := s.svc.Sync(syncCtx, projectID, repoPath, commit, branch)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		s.bump(func(st *SyncStats) { st.TimedOut++ })
		return Freshness{
			RepoID: repoID, Kind: SyncSkipped,
			Reason: fmt.Sprintf("sync exceeded %s and was abandoned; dispatching on existing context", s.cfg.SyncTimeout),
		}
	case err != nil:
		s.bump(func(st *SyncStats) { st.Skipped++ })
		return Freshness{
			RepoID: repoID, Kind: SyncSkipped,
			Reason: "sync failed (" + err.Error() + "); dispatching on existing context",
		}
	}

	fresh := Freshness{
		RepoID:        repoID,
		Generation:    out.Generation,
		IndexedCommit: out.IndexedCommit,
		FilesRead:     out.PathsRead,
		Reason:        out.FallbackReason,
	}
	switch {
	case out.Skipped:
		// Another process holds the repository, or memory was already there.
		// Either way this caller did no work, and whether it may treat memory
		// as current is decided from durable state rather than assumed.
		s.bump(func(st *SyncStats) { st.Skipped++ })
		fresh.Kind = SyncSkipped
		if fresh.Reason == "" {
			fresh.Reason = out.SkipReason
		}
		if state, found, err := s.svc.repo.GetProjectMemoryIndexState(ctx, projectID, repoID); err == nil && found {
			fresh.Generation = state.Generation
			fresh.IndexedCommit = state.IndexedCommit
			fresh.Current = commit != "" && state.IndexedCommit == commit && state.Phase == domain.IndexPhaseIdle
			fresh.Usable = indexCompleted(state)
		}
		return fresh
	case out.FellBackToFullIndex:
		s.bump(func(st *SyncStats) { st.Full++ })
		fresh.Kind = SyncFull
	default:
		s.bump(func(st *SyncStats) { st.Incremental++ })
		fresh.Kind = SyncIncremental
	}
	fresh.Current = commit != "" && fresh.IndexedCommit == commit
	if state, found, err := s.svc.repo.GetProjectMemoryIndexState(ctx, projectID, repoID); err == nil && found {
		fresh.Usable = indexCompleted(state)
		if fresh.Generation == 0 {
			fresh.Generation = state.Generation
		}
	}
	return fresh
}

// indexCompleted reports whether a pass has finished for this repository, and
// therefore whether its memory may be served.
//
// It is the durable answer, not an inference from a sync's return value: a
// caller that coalesced, timed out, or lost the pass claim to another process
// still needs to know whether there is usable memory, and only the row can say.
func indexCompleted(state domain.ProjectMemoryIndexState) bool {
	return state.Generation > 0 && state.Phase == domain.IndexPhaseIdle && !state.CompletedAt.IsZero()
}

func (s *Syncer) bump(fn func(*SyncStats)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.stats)
}

// InvalidatePaths retires the memory a task's own mutation disproved (§12).
//
// It is called at a workflow boundary the moment AO observes that a task
// changed files, so a Reviewer or a Repair Agent arriving next is never handed
// a summary of a file as it was before the work. It marks rather than
// re-derives: re-deriving is the next sync's job, and marking is what makes the
// window between them safe.
func (s *Syncer) InvalidatePaths(
	ctx context.Context, projectID domain.ProjectID, repoPath string, paths []string, reason string,
) (int64, error) {
	if !s.cfg.Mode.Enabled() || len(paths) == 0 {
		return 0, nil
	}
	return s.svc.Invalidate(ctx, projectID, repoPath, paths, reason)
}
