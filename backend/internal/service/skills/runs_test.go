package skills_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// runs_test.go pins the durable SkillRun contract (runs.go) against the REAL
// SQLite store, with an executor that behaves like the runner where it matters:
// it blocks until released and stops when its context is cancelled.

// blockingExecutor wraps recordingExecutor so a test can hold a run in
// "running" and observe what cancellation and shutdown do to it.
type blockingExecutor struct {
	*recordingExecutor
	block    bool
	started  chan string
	release  chan struct{}
	calls    atomic.Int32
	sawRunID sync.Map
}

func newBlockingExecutor(block bool) *blockingExecutor {
	return &blockingExecutor{
		recordingExecutor: &recordingExecutor{attestation: fullyAttested(), report: sampleReport()},
		block:             block,
		started:           make(chan string, 16),
		release:           make(chan struct{}),
	}
}

func (b *blockingExecutor) RunStaticScan(
	ctx context.Context, authority skillrunner.ImageAuthority, req skillrunner.StaticScanRequest,
) (skillrunner.StaticScanReport, error) {
	b.calls.Add(1)
	b.sawRunID.Store(req.RunID, true)
	if b.block {
		b.started <- req.RunID
		select {
		case <-b.release:
		case <-ctx.Done():
			return skillrunner.StaticScanReport{}, ctx.Err()
		}
	}
	return b.recordingExecutor.RunStaticScan(ctx, authority, req)
}

func sampleReport() skillrunner.StaticScanReport {
	return skillrunner.StaticScanReport{
		SchemaVersion: "ao.static-scan.report/v1",
		Tool:          string(skillrunner.ToolStaticScan),
		Coverage:      skillrunner.ScanCoverage{FilesStaged: 3, FilesScanned: 2},
		Findings: []skillrunner.ScanFinding{
			{RuleID: "AOSS-001", Severity: "high", Category: "injection", Title: "shell exec",
				Path: "src/a.go", Line: 12, Recommendation: "avoid", Confidence: "possible"},
			{RuleID: "AOSS-006", Severity: "medium", Category: "secrets", Title: "hardcoded credential name",
				Path: "src/b.go", Line: 3, Recommendation: "move to config", Confidence: "possible"},
		},
	}
}

type recordingReaper struct {
	mu   sync.Mutex
	runs []string
}

func (r *recordingReaper) ReapRun(_ context.Context, runID, _, _ string) (skillrunner.ReapReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	return skillrunner.ReapReport{}, nil
}

type runRig struct {
	f      fixture
	svc    *skills.Service
	auth   *skills.ImageAuthority
	scope  skillimage.Scope
	exec   *blockingExecutor
	reaper *recordingReaper
}

func newRunRig(t *testing.T, block, approve bool, owner string) runRig {
	t.Helper()
	exec := newBlockingExecutor(block)
	f, _, auth, scope := newRunFixture(t, exec.recordingExecutor, approve)
	reaper := &recordingReaper{}
	svc := skills.New(f.store, f.dataDir,
		skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithDurableRuns(f.store, owner, reaper, nil))
	t.Cleanup(func() { svc.CloseRuns(5 * time.Second) })
	return runRig{f: f, svc: svc, auth: auth, scope: scope, exec: exec, reaper: reaper}
}

func (r runRig) start(t *testing.T, key string) skills.SkillRun {
	t.Helper()
	run, _, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{
		RunRequest: runRequest(r.scope), IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	return run
}

func (r runRig) waitTerminal(t *testing.T, runID string) skills.SkillRunDetail {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, err := r.svc.GetRun(context.Background(), r.scope.ProjectID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.State.Terminal() {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state; last %q", runID, d.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartRun_SucceedsAsynchronouslyAndPersistsTheResult(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	run := r.start(t, "")
	if run.State != store.SkillRunQueued && run.State != store.SkillRunRunning && !run.State.Terminal() {
		t.Fatalf("StartRun returned state %q", run.State)
	}
	if run.OwnerInstance != "aod-owner-1" || run.SkillVersion != r.scope.Version || run.ModeID != "static-code" {
		t.Fatalf("run identity not recorded: %+v", run.SkillRunRecord)
	}
	d := r.waitTerminal(t, run.ID)
	if d.State != store.SkillRunSucceeded {
		t.Fatalf("state %q (%s: %s), want succeeded", d.State, d.ErrorCode, d.ErrorMessage)
	}
	if d.Integrity != "verified" || d.Report == nil || d.ReportSHA256 == "" {
		t.Fatalf("report integrity %q, report %v, sha %q", d.Integrity, d.Report != nil, d.ReportSHA256)
	}
	if len(d.Findings) != 2 || d.FindingCount != 2 || d.Findings[0].RuleID != "AOSS-001" || d.Findings[1].Line != 3 {
		t.Fatalf("findings not persisted in order: %+v", d.Findings)
	}
	if d.ImageDigest == "" || d.ApprovalID == "" || d.StartedAt == nil || d.FinishedAt == nil {
		t.Fatalf("runner evidence or timestamps missing: %+v", d.SkillRunRecord)
	}
	if len(d.Capabilities) == 0 || len(d.Controls) == 0 {
		t.Fatalf("granted capabilities %v / runner controls %v not recorded", d.Capabilities, d.Controls)
	}
	// The executor was told which durable run it belongs to.
	if _, ok := r.exec.sawRunID.Load(run.ID); !ok {
		t.Fatal("the executor was not given the run id (container label / staging name)")
	}
	// It survives a new service over the same database: that is the point.
	fresh := skills.New(r.f.store, r.f.dataDir, skills.WithDurableRuns(r.f.store, "aod-owner-2", nil, nil))
	d2, err := fresh.GetRun(context.Background(), r.scope.ProjectID, run.ID)
	if err != nil || d2.State != store.SkillRunSucceeded || d2.Integrity != "verified" {
		t.Fatalf("after restart: %v state=%q integrity=%q", err, d2.State, d2.Integrity)
	}
	list, err := fresh.ListRuns(context.Background(), r.scope.ProjectID, 0)
	if err != nil || len(list) != 1 || list[0].ID != run.ID || list[0].ReportJSON != nil {
		t.Fatalf("history: %v %+v", err, list)
	}
}

func TestStartRun_IdempotencyKeyReturnsTheSameRun(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	first := r.start(t, "click-1")
	r.waitTerminal(t, first.ID)
	again, created, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{
		RunRequest: runRequest(r.scope), IdempotencyKey: "click-1",
	})
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("retry with the same key: err=%v created=%v id=%s want %s", err, created, again.ID, first.ID)
	}
	if n := r.exec.calls.Load(); n != 1 {
		t.Fatalf("executor ran %d times, want exactly 1", n)
	}
}

func TestStartRun_ADoubleClickReachesTheInFlightRun(t *testing.T) {
	r := newRunRig(t, true, true, "aod-owner-1")
	first := r.start(t, "")
	<-r.exec.started
	second, created, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: runRequest(r.scope)})
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("second click: err=%v created=%v id=%s want %s", err, created, second.ID, first.ID)
	}
	close(r.exec.release)
	if d := r.waitTerminal(t, first.ID); d.State != store.SkillRunSucceeded {
		t.Fatalf("state %q", d.State)
	}
	if n := r.exec.calls.Load(); n != 1 {
		t.Fatalf("executor ran %d times, want exactly 1", n)
	}
	// Once terminal, a new request is a new run.
	third := r.start(t, "")
	if third.ID == first.ID {
		t.Fatal("a request after the run ended returned the finished run instead of a new one")
	}
	r.waitTerminal(t, third.ID)
}

func TestCancelRun_StopsARunningRun(t *testing.T) {
	r := newRunRig(t, true, true, "aod-owner-1")
	run := r.start(t, "")
	<-r.exec.started
	if _, err := r.svc.CancelRun(context.Background(), r.scope.ProjectID, run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	d := r.waitTerminal(t, run.ID)
	if d.State != store.SkillRunCancelled || d.ErrorCode != skills.RunErrCancelled || d.ReportSHA256 != "" {
		t.Fatalf("state %q code %q sha %q", d.State, d.ErrorCode, d.ReportSHA256)
	}
	if _, err := r.svc.CancelRun(context.Background(), r.scope.ProjectID, run.ID); apiCode(t, err) != "SKILL_RUN_ALREADY_TERMINAL" {
		t.Fatalf("cancelling a finished run: %v", err)
	}
}

func TestStartRun_ABoundaryRefusalIsRefusedNotFailed(t *testing.T) {
	r := newRunRig(t, false, false, "aod-owner-1") // no approved image
	run := r.start(t, "")
	d := r.waitTerminal(t, run.ID)
	if d.State != store.SkillRunRefused || d.ErrorCode != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("state %q code %q, want refused SKILL_IMAGE_NOT_APPROVED", d.State, d.ErrorCode)
	}
}

func TestStartRun_AnExecutionErrorIsFailed(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	r.exec.err = errors.New("docker: container exited 137")
	d := r.waitTerminal(t, r.start(t, "").ID)
	if d.State != store.SkillRunFailed || d.ErrorCode == "" || d.Report != nil {
		t.Fatalf("state %q code %q", d.State, d.ErrorCode)
	}
}

func TestStartRun_ARefusalBeforeAcceptanceCreatesNoRun(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	other := r.f.seedProject(t, "not-activated")
	_, _, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: other, SkillID: "security-audit", ModeID: "static-code", Actor: admin, ActorPermissions: adminPerms(),
	}})
	if err == nil {
		t.Fatal("a run was accepted for a project that never enabled the skill")
	}
	if list, _ := r.svc.ListRuns(context.Background(), other, 0); len(list) != 0 {
		t.Fatalf("a refused request left %d run(s)", len(list))
	}
	if n := r.exec.calls.Load(); n != 0 {
		t.Fatalf("executor ran %d times", n)
	}
}

func TestGetRun_IsScopedToTheProject(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	run := r.start(t, "")
	r.waitTerminal(t, run.ID)
	other := r.f.seedProject(t, "someone-else")
	if _, err := r.svc.GetRun(context.Background(), other, run.ID); apiCode(t, err) != "SKILL_RUN_NOT_FOUND" {
		t.Fatalf("another project read the run: %v", err)
	}
	if _, err := r.svc.CancelRun(context.Background(), other, run.ID); apiCode(t, err) != "SKILL_RUN_NOT_FOUND" {
		t.Fatalf("another project cancelled the run: %v", err)
	}
}

func TestGetRun_ATamperedReportIsReportedAsAMismatch(t *testing.T) {
	r := newRunRig(t, false, true, "aod-owner-1")
	run := r.start(t, "")
	r.waitTerminal(t, run.ID)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(r.f.dataDir, "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE skill_runs SET report_json = replace(report_json, 'AOSS-001', 'AOSS-999') WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	d, err := r.svc.GetRun(context.Background(), r.scope.ProjectID, run.ID)
	if err != nil || d.Integrity != "mismatch" || d.Report != nil {
		t.Fatalf("tampered report: err=%v integrity=%q report served=%v", err, d.Integrity, d.Report != nil)
	}
}

func TestReconcileRuns_EndsAnotherInstancesRunsAsInterrupted(t *testing.T) {
	// A daemon accepts a run and dies while it is running.
	dead := newRunRig(t, true, true, "aod-dead")
	run := dead.start(t, "")
	<-dead.exec.started

	// The next daemon boots over the same database.
	reaper := &recordingReaper{}
	next := skills.New(dead.f.store, dead.f.dataDir,
		skills.WithSkillExecutor(newBlockingExecutor(false), dead.auth, dead.f.store, "", ""),
		skills.WithDurableRuns(dead.f.store, "aod-next", reaper, nil))
	n, err := next.ReconcileRuns(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("ReconcileRuns: n=%d err=%v", n, err)
	}
	d, err := next.GetRun(context.Background(), dead.scope.ProjectID, run.ID)
	if err != nil || d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrInterrupted {
		t.Fatalf("interrupted run: err=%v state=%q code=%q", err, d.State, d.ErrorCode)
	}
	if len(reaper.runs) != 1 || reaper.runs[0] != run.ID {
		t.Fatalf("reaper asked for %v, want exactly [%s]", reaper.runs, run.ID)
	}
	// The dead owner's executor, if it ever wakes, cannot overwrite the verdict.
	close(dead.exec.release)
	time.Sleep(100 * time.Millisecond)
	if d, _ := next.GetRun(context.Background(), dead.scope.ProjectID, run.ID); d.State != store.SkillRunFailed {
		t.Fatalf("a stale executor moved an interrupted run to %q", d.State)
	}
	// A second reconcile has nothing to do.
	if n, _ := next.ReconcileRuns(context.Background()); n != 0 {
		t.Fatalf("second reconcile ended %d runs", n)
	}
}

func TestCloseRuns_AShutdownDuringARunIsRecorded(t *testing.T) {
	r := newRunRig(t, true, true, "aod-owner-1")
	run := r.start(t, "")
	<-r.exec.started
	r.svc.CloseRuns(5 * time.Second)
	d, err := r.svc.GetRun(context.Background(), r.scope.ProjectID, run.ID)
	if err != nil || d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrShutdown {
		t.Fatalf("after shutdown: err=%v state=%q code=%q", err, d.State, d.ErrorCode)
	}
	if _, _, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: runRequest(r.scope)}); apiCode(t, err) != "SKILL_RUNS_SHUTTING_DOWN" {
		t.Fatalf("a run was accepted after shutdown: %v", err)
	}
}

func TestStartRun_WithoutDurableRunsRefuses(t *testing.T) {
	exec := newBlockingExecutor(false)
	_, svc, _, scope := newRunFixture(t, exec.recordingExecutor, true)
	if _, _, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: runRequest(scope)}); apiCode(t, err) != "SKILL_RUNS_UNAVAILABLE" {
		t.Fatalf("StartRun without durable runs: %v", err)
	}
	_ = domain.ProjectID("")
}
