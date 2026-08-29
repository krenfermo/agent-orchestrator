package runtimegc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
)

// worktree_test.go — P1-D §X / matrix 31-34.
//
// A worktree can hold the only copy of an agent's work, so the balance here is
// even more one-sided than the runtime sweep's: one case removes something, and
// every other case is a refusal with a reason.

type fakeWorktrees struct {
	records  []domain.TaskWorktreeRecord
	err      error
	released []string
	relErr   map[string]error
}

func (f *fakeWorktrees) ListTaskWorktrees(context.Context) ([]domain.TaskWorktreeRecord, error) {
	return f.records, f.err
}

func (f *fakeWorktrees) ReleaseTaskWorktree(_ context.Context, _, taskID string) error {
	if err := f.relErr[taskID]; err != nil {
		return err
	}
	f.released = append(f.released, taskID)
	return nil
}

func worktreeRecord(taskID string, state domain.TaskWorktreeState, integratedSHA string) domain.TaskWorktreeRecord {
	return domain.TaskWorktreeRecord{
		WorkflowRunID: "wf-1", TaskID: taskID, ProjectID: "p",
		RepoPath: "/repo", Path: "/data/worktrees/" + taskID, Branch: "ao/" + taskID,
		TargetBranch: "main", BaseSHA: "base", State: state, IntegratedSHA: integratedSHA,
	}
}

func worktreeSweeper(wt *fakeWorktrees, runState domain.WorkflowRunState) *runtimegc.Sweeper {
	return &runtimegc.Sweeper{
		Worktrees: wt, WorktreeGC: wt,
		Runs: &fakeRuns{runs: map[string]domain.WorkflowRun{
			"wf-1": {ID: "wf-1", State: runState},
		}},
	}
}

func worktreeFinding(t *testing.T, report runtimegc.Report, path string) runtimegc.Finding {
	t.Helper()
	for _, f := range report.Findings {
		if f.Handle == path {
			return f
		}
	}
	t.Fatalf("no finding for %s in %+v", path, report.Findings)
	return runtimegc.Finding{}
}

// Matrix 31/32: only work AO can prove has landed, from a run that has ended,
// is removed. Everything else is kept, and each refusal has its own reason.
func TestWorktreeSweepRemovesOnlyProvablyLandedWork(t *testing.T) {
	wt := &fakeWorktrees{records: []domain.TaskWorktreeRecord{
		// 32: integrated, with a commit, run terminal -> removable.
		worktreeRecord("t-integrated", domain.TaskWorktreeIntegrated, "sha-landed"),
		// 31: approved work that never integrated. The branch is the only copy.
		worktreeRecord("t-active", domain.TaskWorktreeActive, ""),
		// A deliberate "do not clean this up".
		worktreeRecord("t-preserved", domain.TaskWorktreePreserved, ""),
		// Claims integration but names no commit: unprovable, never removed.
		worktreeRecord("t-integrated-no-sha", domain.TaskWorktreeIntegrated, ""),
		// A failed attempt somebody still has to look at.
		worktreeRecord("t-failed", domain.TaskWorktreeFailed, ""),
	}}
	report, err := worktreeSweeper(wt, domain.WorkflowRunCompleted).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}

	if got := worktreeFinding(t, report, "/data/worktrees/t-integrated"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("landed work was %s, want cleaned: %+v", got.Disposition, got)
	}
	for _, kept := range []struct {
		path string
		want runtimegc.Disposition
	}{
		{"/data/worktrees/t-active", runtimegc.DispositionLive},
		{"/data/worktrees/t-preserved", runtimegc.DispositionLive},
		{"/data/worktrees/t-integrated-no-sha", runtimegc.DispositionUnprovable},
		{"/data/worktrees/t-failed", runtimegc.DispositionLive},
	} {
		got := worktreeFinding(t, report, kept.path)
		if got.Disposition != kept.want {
			t.Fatalf("%s was %s, want %s: %+v", kept.path, got.Disposition, kept.want, got)
		}
		if got.Reason == "" {
			t.Fatalf("%s was kept with no reason", kept.path)
		}
	}
	if len(wt.released) != 1 || wt.released[0] != "t-integrated" {
		t.Fatalf("released %v, want only the landed task", wt.released)
	}
}

// A live run protects every one of its worktrees, however integrated they look:
// review, a fix cycle or verification may still need the checkout.
func TestLiveRunProtectsEveryWorktree(t *testing.T) {
	wt := &fakeWorktrees{records: []domain.TaskWorktreeRecord{
		worktreeRecord("t-integrated", domain.TaskWorktreeIntegrated, "sha-landed"),
	}}
	report, err := worktreeSweeper(wt, domain.WorkflowRunRunning).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := worktreeFinding(t, report, "/data/worktrees/t-integrated"); got.Disposition != runtimegc.DispositionLive {
		t.Fatalf("a live run's worktree was %s, want live", got.Disposition)
	}
	if len(wt.released) != 0 {
		t.Fatalf("released %v while the run was still going", wt.released)
	}
}

// Matrix 34: a dry run removes nothing and classifies identically.
func TestWorktreeDryRunRemovesNothing(t *testing.T) {
	build := func() *fakeWorktrees {
		return &fakeWorktrees{records: []domain.TaskWorktreeRecord{
			worktreeRecord("t-integrated", domain.TaskWorktreeIntegrated, "sha-landed"),
			worktreeRecord("t-preserved", domain.TaskWorktreePreserved, ""),
		}}
	}
	dryWT := build()
	dry, err := worktreeSweeper(dryWT, domain.WorkflowRunCompleted).
		Sweep(context.Background(), runtimegc.Options{DryRun: true, Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dryWT.released) != 0 {
		t.Fatalf("a dry run released %v", dryWT.released)
	}
	if dry.Candidates != 1 || dry.Cleaned != 0 {
		t.Fatalf("dry report = %+v, want 1 candidate and 0 cleaned", dry)
	}

	realWT := build()
	actual, err := worktreeSweeper(realWT, domain.WorkflowRunCompleted).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if actual.Candidates != dry.Candidates || actual.SkippedLive != dry.SkippedLive {
		t.Fatalf("dry run and real sweep disagreed: dry=%+v real=%+v", dry, actual)
	}
	if actual.Cleaned != 1 {
		t.Fatalf("the real sweep cleaned %d, want 1", actual.Cleaned)
	}
}

// One worktree that will not release does not stop the others.
func TestOneBrokenWorktreeDoesNotStopTheSweep(t *testing.T) {
	wt := &fakeWorktrees{
		records: []domain.TaskWorktreeRecord{
			worktreeRecord("t-broken", domain.TaskWorktreeIntegrated, "sha-a"),
			worktreeRecord("t-fine", domain.TaskWorktreeIntegrated, "sha-b"),
		},
		relErr: map[string]error{"t-broken": errors.New("git worktree remove failed")},
	}
	report, err := worktreeSweeper(wt, domain.WorkflowRunCompleted).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatalf("one broken worktree aborted the sweep: %v", err)
	}
	if got := worktreeFinding(t, report, "/data/worktrees/t-broken"); got.Disposition != runtimegc.DispositionError || got.Err == "" {
		t.Fatalf("the broken worktree was %s (%q)", got.Disposition, got.Err)
	}
	if got := worktreeFinding(t, report, "/data/worktrees/t-fine"); got.Disposition != runtimegc.DispositionCleaned {
		t.Fatalf("the healthy worktree was %s, want cleaned", got.Disposition)
	}
	if len(wt.released) != 1 || wt.released[0] != "t-fine" {
		t.Fatalf("released %v", wt.released)
	}
}

// An unreadable record set narrows the sweep and licenses nothing.
func TestUnreadableWorktreeRecordsLicenseNothing(t *testing.T) {
	wt := &fakeWorktrees{err: errors.New("database unavailable")}
	report, err := worktreeSweeper(wt, domain.WorkflowRunCompleted).
		Sweep(context.Background(), runtimegc.Options{Trigger: "test"})
	if err != nil {
		t.Fatalf("an unreadable record set failed the sweep: %v", err)
	}
	if len(report.Findings) != 0 || len(wt.released) != 0 {
		t.Fatalf("findings=%+v released=%v", report.Findings, wt.released)
	}
}
