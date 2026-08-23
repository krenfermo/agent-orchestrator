package postrunqa_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/hookutil"
	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
)

// The real components must satisfy the collector's source contracts with no
// adapter in between; that is the whole point of shaping them after the
// existing structured sources.
var (
	_ postrunqa.BranchLockSource  = (*branchlock.Manager)(nil)
	_ postrunqa.HookLauncherProbe = hookutil.InspectLauncher
	_ postrunqa.GitSource         = (ports.WorkspacePreflighter)(nil)
)

var (
	collectedAt = time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	endedAt     = collectedAt.Add(-5 * time.Minute)
)

const finalReport = "Implemented the collector.\nRan `go test ./...`: PASS.\nLeft nothing behind."

// --- fakes ------------------------------------------------------------------

type fakeExecutions struct {
	exec postrunqa.Execution
	ok   bool
	err  error
}

func (f fakeExecutions) LoadExecution(context.Context, string) (postrunqa.Execution, bool, error) {
	return f.exec, f.ok, f.err
}

type fakeGit struct {
	byRepo map[string]ports.WorkspacePreflight
	err    error
}

func (f fakeGit) PreflightRepository(_ context.Context, repoPath, _ string) (ports.WorkspacePreflight, error) {
	if f.err != nil {
		return ports.WorkspacePreflight{}, f.err
	}
	return f.byRepo[repoPath], nil
}

type fakeLocks struct {
	byRun     map[string][]domain.BranchLock
	bySession map[string][]domain.BranchLock
	statuses  []branchlock.LockStatus
	runErr    error
	inspecErr error
}

func (f fakeLocks) HeldByRun(_ context.Context, runID string) ([]domain.BranchLock, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.byRun[runID], nil
}

func (f fakeLocks) HeldBySession(_ context.Context, sessionID string) ([]domain.BranchLock, error) {
	return f.bySession[sessionID], nil
}

func (f fakeLocks) Inspect(context.Context) ([]branchlock.LockStatus, error) {
	if f.inspecErr != nil {
		return nil, f.inspecErr
	}
	return f.statuses, nil
}

type fakeWakes struct {
	schedules []postrunqa.WakeScheduleRecord
	err       error
}

func (f fakeWakes) PendingWakes(context.Context, string) ([]postrunqa.WakeScheduleRecord, error) {
	return f.schedules, f.err
}

type fakeProcesses struct {
	records []postrunqa.ProcessRecord
	err     error
}

func (f fakeProcesses) ProcessRecords(context.Context, string) ([]postrunqa.ProcessRecord, error) {
	return f.records, f.err
}

type fakeRuntimeErrors struct {
	records []postrunqa.RuntimeErrorRecord
	err     error
}

func (f fakeRuntimeErrors) RuntimeErrors(context.Context, string) ([]postrunqa.RuntimeErrorRecord, error) {
	return f.records, f.err
}

type fakeSessions struct {
	sessions map[domain.SessionID]domain.SessionRecord
	facts    map[domain.SessionID]domain.SessionCleanupRecord
	err      error
}

func (f fakeSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	if f.err != nil {
		return domain.SessionRecord{}, false, f.err
	}
	rec, ok := f.sessions[id]
	return rec, ok, nil
}

func (f fakeSessions) GetSessionCleanupFacts(_ context.Context, id domain.SessionID) (domain.SessionCleanupRecord, bool, error) {
	if f.err != nil {
		return domain.SessionCleanupRecord{}, false, f.err
	}
	rec, ok := f.facts[id]
	return rec, ok, nil
}

// --- fixtures ---------------------------------------------------------------

// cleanFixture is an execution that finished and left nothing behind: every
// repository committed, every lock released, the hook launcher pointing at an
// installed binary, no wake still open, every command exited 0, every session
// torn down.
func cleanFixture(t *testing.T) postrunqa.EvidenceDeps {
	t.Helper()
	dataDir, _ := writeHookLauncher(t, false)
	zero := 0
	return postrunqa.EvidenceDeps{
		Executions: fakeExecutions{ok: true, exec: postrunqa.Execution{
			ID:               "wf-exec-1",
			WorkflowRunID:    "wf-exec-1",
			SessionIDs:       []string{"ses-1"},
			Repositories:     []postrunqa.RepositoryTarget{{RepoPath: "/repo", Branch: "feat/x"}},
			DataDir:          dataDir,
			EndedAt:          endedAt,
			FinalAgentReport: finalReport,
		}},
		Git: fakeGit{byRepo: map[string]ports.WorkspacePreflight{
			"/repo": {RepoPath: "/repo", ConfiguredBranch: "feat/x", CurrentBranch: "feat/x", HeadSHA: "abc123"},
		}},
		BranchLocks:  fakeLocks{},
		HookLauncher: hookutil.InspectLauncher,
		Wakes:        fakeWakes{},
		Processes: fakeProcesses{records: []postrunqa.ProcessRecord{
			{Label: "go test ./...", ExitCode: &zero, EndedAt: endedAt},
		}},
		RuntimeErrors: fakeRuntimeErrors{},
		Sessions: fakeSessions{
			sessions: map[domain.SessionID]domain.SessionRecord{"ses-1": {ID: "ses-1", IsTerminated: true}},
			facts: map[domain.SessionID]domain.SessionCleanupRecord{"ses-1": {
				SessionID:            "ses-1",
				RuntimeReleasedAt:    endedAt,
				WorkspaceDisposition: domain.DispositionRemoved,
			}},
		},
		Clock: func() time.Time { return collectedAt },
	}
}

// writeHookLauncher installs a launcher shim under a fresh data dir. When
// stale, the shim is pinned to a Go build-cache path that has already been
// swept -- the fd9b87fae defect.
func writeHookLauncher(t *testing.T, stale bool) (dataDir, target string) {
	t.Helper()
	root := t.TempDir()
	dataDir = filepath.Join(root, "data")

	if stale {
		target = filepath.Join(root, "go-build2455361342", "b001", "exe", "ao")
	} else {
		target = filepath.Join(root, "install", "ao-daemon")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	if _, err := hookutil.EnsureLauncherFor(dataDir, func() (string, error) { return target, nil }); err != nil {
		t.Fatal(err)
	}
	if stale {
		// EnsureLauncher parked its own copy of the ephemeral binary; the
		// stale case is the shim a previous, unfixed build left behind, so
		// rewrite it to name the temp path directly and then sweep the cache.
		shim := filepath.Join(hookutil.LauncherDir(dataDir), hookutil.LauncherName())
		script := "#!/bin/sh\nexec " + hookutil.ShellQuote(target) + " \"$@\"\n"
		if err := os.WriteFile(shim, []byte(script), 0o700); err != nil { //nolint:gosec // the shim must be executable
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(root, "go-build2455361342")); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(hookutil.StableBinaryPath(dataDir))
	}
	return dataDir, target
}

func collect(t *testing.T, deps postrunqa.EvidenceDeps) postrunqa.EvidenceReport {
	t.Helper()
	c, err := postrunqa.NewEvidenceCollector(deps)
	if err != nil {
		t.Fatal(err)
	}
	report, err := c.Collect(context.Background(), "wf-exec-1")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return report
}

// --- tests ------------------------------------------------------------------

func TestCollect_CleanFixtureReportsNoAnomalies(t *testing.T) {
	report := collect(t, cleanFixture(t))

	if report.ExecutionID != "wf-exec-1" || report.WorkflowRunID != "wf-exec-1" {
		t.Fatalf("execution identity = %q/%q", report.ExecutionID, report.WorkflowRunID)
	}
	if !report.CollectedAt.Equal(collectedAt) {
		t.Fatalf("CollectedAt = %v, want the injected clock %v", report.CollectedAt, collectedAt)
	}
	if !report.ExecutionEnded {
		t.Fatal("ExecutionEnded = false for an execution with an EndedAt")
	}
	if len(report.SourceErrors) != 0 {
		t.Fatalf("SourceErrors = %+v, want none", report.SourceErrors)
	}
	if report.HasAnomalies() {
		t.Fatalf("clean fixture reported anomalies: %v", report.Anomalies())
	}

	// Every source still has to have been READ, not merely silent.
	if len(report.Git) != 1 || report.Git[0].RepoPath != "/repo" || report.Git[0].HeadSHA != "abc123" {
		t.Fatalf("git evidence = %+v, want one clean repository", report.Git)
	}
	if len(report.BranchLocks) != 0 {
		t.Fatalf("branch lock evidence = %+v, want none held", report.BranchLocks)
	}
	if !report.HookLauncher.Probed || !report.HookLauncher.TargetPresent {
		t.Fatalf("hook launcher evidence = %+v, want a probed, present target", report.HookLauncher)
	}
	if len(report.Processes) != 1 || report.Processes[0].Anomaly != "" {
		t.Fatalf("process evidence = %+v, want one clean exit", report.Processes)
	}
	if len(report.Sessions) != 1 || !report.Sessions[0].RuntimeReleased {
		t.Fatalf("session evidence = %+v, want one released session", report.Sessions)
	}
	if len(report.RuntimeErrors) != 0 {
		t.Fatalf("runtime error evidence = %+v, want none", report.RuntimeErrors)
	}
}

func TestCollect_DirtyFixturePopulatesEverySource(t *testing.T) {
	dataDir, staleTarget := writeHookLauncher(t, true)
	exit1 := 1
	deps := cleanFixture(t)
	deps.HookLauncher = hookutil.InspectLauncher
	deps.Executions = fakeExecutions{ok: true, exec: postrunqa.Execution{
		ID:               "wf-exec-1",
		WorkflowRunID:    "wf-exec-1",
		SessionIDs:       []string{"ses-1"},
		Repositories:     []postrunqa.RepositoryTarget{{RepoPath: "/repo", Branch: "feat/x"}},
		DataDir:          dataDir,
		EndedAt:          endedAt,
		FinalAgentReport: finalReport,
	}}
	deps.Git = fakeGit{byRepo: map[string]ports.WorkspacePreflight{
		"/repo": {
			RepoPath:         "/repo",
			ConfiguredBranch: "feat/x",
			CurrentBranch:    "main",
			Dirty:            true,
			Changes: []ports.WorkspaceChange{
				{Path: "internal/postrunqa/evidence.go", Status: " M"},
				{Path: "scratch.txt", Status: "??"},
			},
		},
	}}
	leaked := domain.BranchLock{
		ID: "lock-1", LockKey: "/repo\x1ffeat/x", RepoPath: "/repo", Branch: "feat/x",
		WorkflowRunID: "wf-exec-1", State: domain.BranchLockHeld, AcquiredAt: endedAt.Add(-time.Hour),
	}
	deps.BranchLocks = fakeLocks{
		byRun: map[string][]domain.BranchLock{"wf-exec-1": {leaked}},
		statuses: []branchlock.LockStatus{{
			Lock:      leaked,
			Retention: branchlock.Retention{Decision: branchlock.RetentionRelease, Reason: "stale: workflow run is completed"},
		}},
	}
	deps.Wakes = fakeWakes{schedules: []postrunqa.WakeScheduleRecord{{
		ID: "wake-1", StepID: "step-9", Reason: "branch_lock", Status: "pending",
		ScheduledAt: collectedAt.Add(-30 * time.Minute), AttemptCount: 4, LastError: "branch lock: already held",
	}}}
	deps.Processes = fakeProcesses{records: []postrunqa.ProcessRecord{
		{Label: "go build ./...", ExitCode: &exit1, EndedAt: endedAt, StderrTail: "undefined: Collect"},
		{Label: "npm run dev", Running: true},
	}}
	deps.Sessions = fakeSessions{
		sessions: map[domain.SessionID]domain.SessionRecord{"ses-1": {ID: "ses-1", IsTerminated: true}},
		facts: map[domain.SessionID]domain.SessionCleanupRecord{"ses-1": {
			SessionID:            "ses-1",
			WorkspaceDisposition: domain.DispositionFailed,
			AttemptCount:         3,
			FailureCode:          "worktree_remove_failed",
		}},
	}

	report := collect(t, deps)

	if len(report.SourceErrors) != 0 {
		t.Fatalf("SourceErrors = %+v, want none: every source answered", report.SourceErrors)
	}

	t.Run("git", func(t *testing.T) {
		if len(report.Git) != 1 {
			t.Fatalf("git evidence = %+v, want one repository", report.Git)
		}
		g := report.Git[0]
		if !g.Dirty || len(g.Changes) != 2 {
			t.Fatalf("git evidence = %+v, want the dirty-file diff carried through", g)
		}
		if !strings.Contains(g.Anomaly, "2 uncommitted change(s)") {
			t.Fatalf("git anomaly = %q, want it to name the uncommitted changes", g.Anomaly)
		}
		if !strings.Contains(g.Anomaly, `"main"`) {
			t.Fatalf("git anomaly = %q, want it to name the wrong checked-out branch", g.Anomaly)
		}
	})

	t.Run("branch lock", func(t *testing.T) {
		if len(report.BranchLocks) != 1 {
			t.Fatalf("branch lock evidence = %+v, want the leaked lock", report.BranchLocks)
		}
		l := report.BranchLocks[0]
		if !l.Leaked {
			t.Fatalf("branch lock evidence = %+v, want Leaked", l)
		}
		if l.RetentionDecision != branchlock.RetentionRelease {
			t.Fatalf("retention decision = %q, want the manager's own %q", l.RetentionDecision, branchlock.RetentionRelease)
		}
		if !strings.Contains(l.Anomaly, "still held by workflow wf-exec-1") {
			t.Fatalf("branch lock anomaly = %q, want it to name the holder", l.Anomaly)
		}
		if !strings.Contains(l.Anomaly, "stale: workflow run is completed") {
			t.Fatalf("branch lock anomaly = %q, want the manager's staleness reason", l.Anomaly)
		}
	})

	t.Run("hook launcher", func(t *testing.T) {
		h := report.HookLauncher
		if !h.Probed || !h.Present || !h.Executable {
			t.Fatalf("hook launcher evidence = %+v, want a present, executable shim", h)
		}
		if h.Target != staleTarget || !h.TargetEphemeral || h.TargetPresent {
			t.Fatalf("hook launcher evidence = %+v, want a missing ephemeral target %q", h, staleTarget)
		}
		if !strings.Contains(h.Anomaly, "ephemeral Go build-cache binary") {
			t.Fatalf("hook launcher anomaly = %q, want it to name the stale build-cache target", h.Anomaly)
		}
	})

	t.Run("wake", func(t *testing.T) {
		if len(report.Wakes) != 1 {
			t.Fatalf("wake evidence = %+v, want the open schedule", report.Wakes)
		}
		w := report.Wakes[0]
		if w.StepID != "step-9" || w.Reason != "branch_lock" || w.AttemptCount != 4 {
			t.Fatalf("wake evidence = %+v, want the scheduler's own fields", w)
		}
		if !w.Overdue {
			t.Fatal("wake Overdue = false for a schedule 30m past its scheduled time")
		}
		if !strings.Contains(w.Anomaly, "still pending after the execution ended") {
			t.Fatalf("wake anomaly = %q, want the past-expected-end statement", w.Anomaly)
		}
		if !strings.Contains(w.Anomaly, "overdue by 30m0s") {
			t.Fatalf("wake anomaly = %q, want the overdue statement", w.Anomaly)
		}
		if !strings.Contains(w.Anomaly, "retrying after 4 attempt(s)") {
			t.Fatalf("wake anomaly = %q, want the retry statement", w.Anomaly)
		}
	})

	t.Run("process", func(t *testing.T) {
		if len(report.Processes) != 2 {
			t.Fatalf("process evidence = %+v, want both records", report.Processes)
		}
		if !strings.Contains(report.Processes[0].Anomaly, "exited with status 1") {
			t.Fatalf("process anomaly = %q, want the exit status", report.Processes[0].Anomaly)
		}
		if report.Processes[0].StderrTail != "undefined: Collect" {
			t.Fatalf("stderr tail = %q, want it carried through", report.Processes[0].StderrTail)
		}
		if !strings.Contains(report.Processes[1].Anomaly, "still running after the execution ended") {
			t.Fatalf("process anomaly = %q, want the survivor statement", report.Processes[1].Anomaly)
		}
	})

	t.Run("session", func(t *testing.T) {
		if len(report.Sessions) != 1 {
			t.Fatalf("session evidence = %+v, want the one session", report.Sessions)
		}
		s := report.Sessions[0]
		if !s.Found || !s.Terminated || !s.CleanupRecorded {
			t.Fatalf("session evidence = %+v, want a terminated session with cleanup facts", s)
		}
		if s.RuntimeReleased {
			t.Fatal("RuntimeReleased = true with a zero RuntimeReleasedAt")
		}
		if !strings.Contains(s.Anomaly, "runtime was never confirmed released") {
			t.Fatalf("session anomaly = %q, want the runtime statement", s.Anomaly)
		}
		if !strings.Contains(s.Anomaly, "workspace teardown failed") {
			t.Fatalf("session anomaly = %q, want the workspace disposition", s.Anomaly)
		}
		if !strings.Contains(s.Anomaly, "worktree_remove_failed") {
			t.Fatalf("session anomaly = %q, want the failure code", s.Anomaly)
		}
	})

	if got := len(report.Anomalies()); got < 6 {
		t.Fatalf("Anomalies() returned %d entries (%v), want one per dirty source", got, report.Anomalies())
	}
}

// The gate's downstream classification compares what the agent SAID against
// what the sources show, so the text must survive collection unedited.
func TestCollect_CapturesFinalAgentReportVerbatim(t *testing.T) {
	report := collect(t, cleanFixture(t))

	if report.FinalAgentReport != finalReport {
		t.Fatalf("FinalAgentReport = %q, want the agent's text verbatim %q", report.FinalAgentReport, finalReport)
	}
}

func TestCollect_UnknownExecution(t *testing.T) {
	c, err := postrunqa.NewEvidenceCollector(postrunqa.EvidenceDeps{Executions: fakeExecutions{ok: false}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Collect(context.Background(), "wf-nope")

	if !errors.Is(err, postrunqa.ErrUnknownExecution) {
		t.Fatalf("Collect() error = %v, want ErrUnknownExecution", err)
	}
}

func TestCollect_RequiresAnExecutionSourceAndAnID(t *testing.T) {
	if _, err := postrunqa.NewEvidenceCollector(postrunqa.EvidenceDeps{}); err == nil {
		t.Fatal("NewEvidenceCollector accepted deps with no execution source")
	}
	c, err := postrunqa.NewEvidenceCollector(postrunqa.EvidenceDeps{Executions: fakeExecutions{ok: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Collect(context.Background(), "  "); err == nil {
		t.Fatal("Collect accepted an empty execution id")
	}
}

// One unreadable source must not cost the gate the evidence the others had,
// and must never read as "that source reported nothing".
func TestCollect_FailingSourceIsRecordedAndCollectionContinues(t *testing.T) {
	deps := cleanFixture(t)
	deps.Git = fakeGit{err: errors.New("repository is gone")}
	deps.Wakes = fakeWakes{err: errors.New("wake store unavailable")}

	report := collect(t, deps)

	if len(report.SourceErrors) != 2 {
		t.Fatalf("SourceErrors = %+v, want one per failing source", report.SourceErrors)
	}
	sources := map[postrunqa.EvidenceSource]bool{}
	for _, e := range report.SourceErrors {
		sources[e.Source] = true
	}
	if !sources[postrunqa.SourceGit] || !sources[postrunqa.SourceWake] {
		t.Fatalf("SourceErrors = %+v, want git and wake attributed", report.SourceErrors)
	}
	if len(report.Sessions) != 1 {
		t.Fatalf("session evidence = %+v, want the healthy sources still collected", report.Sessions)
	}
	if report.FinalAgentReport != finalReport {
		t.Fatal("the final agent report was dropped when a source failed")
	}
}

// A live execution has not "left behind" anything yet: an open wake and a held
// lock are how a running execution is supposed to look.
func TestCollect_LiveExecutionDoesNotReportLeftovers(t *testing.T) {
	deps := cleanFixture(t)
	dataDir, _ := writeHookLauncher(t, false)
	deps.Executions = fakeExecutions{ok: true, exec: postrunqa.Execution{
		ID:            "wf-exec-1",
		WorkflowRunID: "wf-exec-1",
		SessionIDs:    []string{"ses-1"},
		DataDir:       dataDir,
	}}
	held := domain.BranchLock{ID: "lock-1", LockKey: "k", RepoPath: "/repo", Branch: "feat/x", WorkflowRunID: "wf-exec-1", State: domain.BranchLockHeld}
	deps.BranchLocks = fakeLocks{
		byRun:    map[string][]domain.BranchLock{"wf-exec-1": {held}},
		statuses: []branchlock.LockStatus{{Lock: held, Retention: branchlock.Retention{Decision: branchlock.RetentionKeep, Reason: "owner is running"}}},
	}
	deps.Wakes = fakeWakes{schedules: []postrunqa.WakeScheduleRecord{{
		ID: "wake-1", Reason: "capacity_reset", Status: "pending", ScheduledAt: collectedAt.Add(time.Hour),
	}}}
	deps.Sessions = fakeSessions{sessions: map[domain.SessionID]domain.SessionRecord{"ses-1": {ID: "ses-1"}}}

	report := collect(t, deps)

	if report.ExecutionEnded {
		t.Fatal("ExecutionEnded = true for an execution with a zero EndedAt")
	}
	if report.HasAnomalies() {
		t.Fatalf("live execution reported anomalies: %v", report.Anomalies())
	}
	if len(report.BranchLocks) != 1 || report.BranchLocks[0].Leaked {
		t.Fatalf("branch lock evidence = %+v, want it recorded but not leaked", report.BranchLocks)
	}
}

// A source that is simply not wired contributes nothing -- and, unlike a
// source that failed, is not a SourceError.
func TestCollect_UnwiredSourcesAreSilent(t *testing.T) {
	c, err := postrunqa.NewEvidenceCollector(postrunqa.EvidenceDeps{
		Executions: fakeExecutions{ok: true, exec: postrunqa.Execution{ID: "wf-exec-1", FinalAgentReport: finalReport}},
		Clock:      func() time.Time { return collectedAt },
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := c.Collect(context.Background(), "wf-exec-1")
	if err != nil {
		t.Fatal(err)
	}

	if len(report.SourceErrors) != 0 {
		t.Fatalf("SourceErrors = %+v, want none for unwired sources", report.SourceErrors)
	}
	if report.HookLauncher.Probed {
		t.Fatal("HookLauncher.Probed = true with no probe wired")
	}
	if report.HasAnomalies() {
		t.Fatalf("unwired collector reported anomalies: %v", report.Anomalies())
	}
	if report.FinalAgentReport != finalReport {
		t.Fatal("the final agent report was dropped")
	}
}

// The acceptance case for a stale hook-bin symlink: the shim resolves and is
// executable, but the binary behind the link is gone, so every hook this
// execution's successor fires would die.
func TestCollect_StaleHookBinSymlinkSurfacesAsAnomaly(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	binary := filepath.Join(root, "build", "ao-daemon")
	link := filepath.Join(root, "bin", "ao")
	for _, dir := range []string{filepath.Dir(binary), filepath.Dir(link)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	if err := os.Symlink(binary, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := hookutil.EnsureLauncherFor(dataDir, func() (string, error) { return link, nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(binary); err != nil {
		t.Fatal(err)
	}

	deps := cleanFixture(t)
	deps.Executions = fakeExecutions{ok: true, exec: postrunqa.Execution{
		ID: "wf-exec-1", WorkflowRunID: "wf-exec-1", DataDir: dataDir, EndedAt: endedAt,
	}}
	deps.Sessions = nil

	report := collect(t, deps)

	h := report.HookLauncher
	if !h.Probed || !h.Present || h.Target != link {
		t.Fatalf("hook launcher evidence = %+v, want a present shim naming %q", h, link)
	}
	if h.TargetPresent {
		t.Fatal("TargetPresent = true for a dangling symlink target")
	}
	if !strings.Contains(h.Anomaly, "launcher target does not exist") {
		t.Fatalf("hook launcher anomaly = %q, want it to name the missing target", h.Anomaly)
	}
	if !report.HasAnomalies() {
		t.Fatal("HasAnomalies() = false with a broken hook launcher")
	}
}

// The daemon and the runtimes record their own failures; those records are
// evidence like any other, and the collector must surface them rather than
// leave the gate reading "nothing left behind" for an execution whose runtime
// was erroring throughout.
func TestCollect_RuntimeErrorsSurfaceAsAnomalies(t *testing.T) {
	deps := cleanFixture(t)
	deps.RuntimeErrors = fakeRuntimeErrors{records: []postrunqa.RuntimeErrorRecord{
		{Component: "runtime.tmux", Code: "PANE_GONE", Message: "pane disappeared mid-send", Level: postrunqa.RuntimeLevelError, Count: 4},
		{Component: "lifecycle.reaper", Code: "REAP_TIMEOUT", Message: "reap pass timed out", Level: postrunqa.RuntimeLevelFatal, Count: 1},
	}}

	report := collect(t, deps)

	if len(report.RuntimeErrors) != 2 {
		t.Fatalf("runtime error evidence = %+v, want 2", report.RuntimeErrors)
	}
	// Sorted by component, so two collections of the same state produce the
	// same report.
	if report.RuntimeErrors[0].Component != "lifecycle.reaper" {
		t.Fatalf("runtime errors are not ordered by component: %+v", report.RuntimeErrors)
	}
	if !report.RuntimeErrors[0].Fatal() {
		t.Fatalf("Fatal() = false for a fatal record: %+v", report.RuntimeErrors[0])
	}
	for _, e := range report.RuntimeErrors {
		if e.Anomaly == "" {
			t.Fatalf("every runtime error record is an anomaly, but %+v carries none", e)
		}
	}
	if !strings.Contains(report.RuntimeErrors[1].Anomaly, "4 times") {
		t.Fatalf("the occurrence count belongs in the prose: %q", report.RuntimeErrors[1].Anomaly)
	}
	if !strings.Contains(strings.Join(report.Anomalies(), "\n"), "runtime lifecycle.reaper") {
		t.Fatalf("runtime errors are missing from Anomalies(): %v", report.Anomalies())
	}
}

// A runtime-error source that cannot be read is a SourceError, never a report
// that quietly claims the daemon logged nothing.
func TestCollect_FailingRuntimeErrorSourceIsRecorded(t *testing.T) {
	deps := cleanFixture(t)
	deps.RuntimeErrors = fakeRuntimeErrors{err: errors.New("log store is offline")}

	report := collect(t, deps)

	var found bool
	for _, e := range report.SourceErrors {
		if e.Source == postrunqa.SourceRuntimeLog && strings.Contains(e.Message, "log store is offline") {
			found = true
		}
	}
	if !found {
		t.Fatalf("SourceErrors = %+v, want the runtime log failure recorded", report.SourceErrors)
	}
	if len(report.RuntimeErrors) != 0 {
		t.Fatalf("a failed source must contribute no evidence: %+v", report.RuntimeErrors)
	}
}
