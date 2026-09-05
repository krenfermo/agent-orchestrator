package projectmemory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// reconciler.go -- P4-G's automatic initial index.
//
// WHY A RECONCILER AND NOT AN EVENT. The obvious design is to fire an "index
// this" job when a project is imported. It is the wrong one here, for reasons
// that are all versions of the same reason: a queue is state that can be lost,
// and every requirement in the brief is a requirement about not losing it.
//
//   - restart-safe: a queue entry written before a crash has to be replayed
//     after it. A reconciler has nothing to replay -- it re-derives what needs
//     doing from the durable rows every tick.
//   - idempotent: a queue can enqueue the same project twice. A reconciler
//     asks "does this need indexing" and a project that does not is skipped,
//     however many times it is asked.
//   - crash-mid-index safe: a build interrupted by a crash leaves phase
//     'building' forever. The existing reclaim path takes it over; the
//     reconciler simply sees it as in-flight until then and stays off it.
//   - never blocks import: nothing in the import path calls this at all. A
//     project becomes indexable by EXISTING, not by anybody remembering to
//     announce it.
//
// The same loop covers the initial index and the incremental refresh, because
// they are the same question asked at different times: pending and stale are
// both "the graph does not describe the checkout", and GraphSync already
// decides between an incremental pass and a full build the way a dispatch
// would. P4-G adds no second opinion about that.

// DefaultReconcileInterval is how often the reconciler asks whether anything
// needs indexing. It is deliberately slow: the work it schedules is measured
// in seconds to minutes, the answer changes only when somebody commits or
// imports, and a tight loop would spend more time asking than working.
const DefaultReconcileInterval = 60 * time.Second

// DefaultMaxPerTick bounds how many repositories one tick will index.
//
// This is the "bounded" requirement, and one is the right number rather than a
// timid one: indexing is CPU- and IO-heavy, an installation that just imported
// twenty projects must not try to index twenty repositories at once on
// somebody's laptop, and a queue that drains one per tick still drains. Raising
// it trades a faster cold start for a less responsive machine.
const DefaultMaxPerTick = 1

// ReconcilerConfig configures the automatic indexer.
type ReconcilerConfig struct {
	Interval   time.Duration
	MaxPerTick int
	Logger     *slog.Logger
}

// ProjectLister is the slice of the project registry the reconciler needs.
// Narrow on purpose: it decides what to index, never what a project is.
type ProjectLister interface {
	ListProjects(ctx context.Context) ([]domain.ProjectRecord, error)
}

// Reconciler keeps every project's code graph current without anybody asking.
type Reconciler struct {
	svc        *Service
	projects   ProjectLister
	interval   time.Duration
	maxPerTick int
	log        *slog.Logger

	// mu guards lastAttempt, which is read and written only by the single
	// reconcile goroutine today; the mutex is there so Stats can be read from
	// a handler without a race if this is ever surfaced.
	mu sync.Mutex
	// lastAttempt records when a repository was last attempted, so a
	// repository that keeps coming back stale (an active branch, somebody
	// committing every minute) cannot monopolise every tick.
	lastAttempt map[string]time.Time
}

// NewReconciler builds the automatic indexer over the memory service and the
// project registry.
func NewReconciler(svc *Service, projects ProjectLister, cfg ReconcilerConfig) *Reconciler {
	r := &Reconciler{
		svc: svc, projects: projects,
		interval: cfg.Interval, maxPerTick: cfg.MaxPerTick, log: cfg.Logger,
		lastAttempt: map[string]time.Time{},
	}
	if r.interval <= 0 {
		r.interval = DefaultReconcileInterval
	}
	if r.maxPerTick <= 0 {
		r.maxPerTick = DefaultMaxPerTick
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

// Start runs an immediate pass followed by interval passes until ctx is
// cancelled. The returned channel closes after the goroutine exits, matching
// every other AO poller.
//
// A nil Reconciler starts nothing and returns a closed channel, so a daemon
// built without a code graph does not have to guard the call site.
func (r *Reconciler) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if r == nil || r.svc == nil || r.svc.graph == nil || r.projects == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		r.reconcile(ctx)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.reconcile(ctx)
			}
		}
	}()
	return done
}

// ReconcileOnce runs a single pass synchronously. It exists so the automatic
// indexer can be tested for the properties that matter -- that it indexes what
// nobody asked it to, and that it then leaves an unchanged repository alone --
// without a test having to wait out a ticker.
func (r *Reconciler) ReconcileOnce(ctx context.Context) { r.reconcile(ctx) }

// reconcile performs one pass: find what needs indexing, index up to the
// per-tick bound, stop.
func (r *Reconciler) reconcile(ctx context.Context) {
	projects, err := r.projects.ListProjects(ctx)
	if err != nil {
		r.log.Debug("project intelligence reconcile could not list projects", "error", err)
		return
	}
	indexed := 0
	for _, project := range projects {
		if ctx.Err() != nil {
			return
		}
		if indexed >= r.maxPerTick {
			return
		}
		if !project.ArchivedAt.IsZero() {
			// An archived project is not work anybody is doing.
			continue
		}
		if r.reconcileProject(ctx, domain.ProjectID(project.ID)) {
			indexed++
		}
	}
}

// reconcileProject indexes one project's repositories if any need it, and
// reports whether it did any work. Errors are logged and swallowed: a
// repository that cannot be indexed must not stop the rest of the
// installation from being indexed, and the failure is already durable in the
// row's last_error where the UI will show it.
func (r *Reconciler) reconcileProject(ctx context.Context, id domain.ProjectID) bool {
	states, err := r.svc.graph.StatusAll(ctx, id)
	if err != nil {
		r.log.Debug("project intelligence reconcile could not read graph status",
			"project", id, "error", err)
		return false
	}
	// A project with no row at all has never been indexed. StatusAll returns
	// nothing for it, so "pending" has to be inferred from the absence rather
	// than read from a state -- this is the initial-index case, and it is the
	// one the whole file exists for.
	if len(states) == 0 {
		return r.index(ctx, id, "", IntelligencePending)
	}
	for _, state := range states {
		if ctx.Err() != nil {
			return false
		}
		derived := intelligenceState(state, graphDrift(ctx, state))
		if !needsIndex(derived) {
			continue
		}
		if r.index(ctx, id, state.RepoPath, derived) {
			return true
		}
	}
	return false
}

// index runs one sync, subject to the per-repository cooldown.
func (r *Reconciler) index(ctx context.Context, id domain.ProjectID, repoPath string, why IntelligenceState) bool {
	key := string(id) + "\x00" + repoPath
	if !r.claim(key) {
		return false
	}
	started := time.Now()
	// full=false: GraphSync itself decides between an incremental pass and a
	// full build, using the same rule a dispatch uses. The reconciler
	// deliberately holds no second opinion about that -- one rule, one place.
	result, err := r.svc.GraphSync(ctx, id, repoPath, false)
	if err != nil {
		r.log.Warn("project intelligence indexing failed",
			"project", id, "repo", repoPath, "reason", why, "error", err)
		return true
	}
	r.log.Info("project intelligence indexed",
		"project", id, "repo", result.RepoPath, "reason", why, "kind", result.Kind,
		"files", result.Files, "symbols", result.Symbols, "edges", result.Edges,
		"parsed", result.FilesParsed, "reused", result.FilesReused,
		"duration", time.Since(started))
	return true
}

// claim enforces a per-repository cooldown of one interval.
//
// Without it a repository somebody is actively committing to would be stale
// again by the next tick and would take every tick forever, and no other
// project would ever reach its initial index. The cooldown is what makes the
// per-tick bound a fair share rather than a queue one repository can hold.
func (r *Reconciler) claim(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastAttempt[key]; ok && time.Since(last) < r.interval {
		return false
	}
	r.lastAttempt[key] = time.Now()
	return true
}
