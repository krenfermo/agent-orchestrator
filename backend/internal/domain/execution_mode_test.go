package domain

import (
	"errors"
	"strings"
	"testing"
)

// The single most important backward-compatibility guarantee of this
// checkpoint: a project written before it has no execution mode stored, and
// must keep getting isolated worktrees.
func TestUnsetExecutionModeStaysIsolatedWorktree(t *testing.T) {
	var mode ExecutionMode
	if got := mode.WithDefault(); got != ExecutionIsolatedWorktree {
		t.Fatalf("empty execution mode resolves to %q, want isolated_worktree", got)
	}
	if mode.DirectBranch() {
		t.Fatal("empty execution mode reports direct branch")
	}
	if got := (ProjectConfig{}).EffectiveExecutionMode(); got != ExecutionIsolatedWorktree {
		t.Fatalf("empty config resolves to %q, want isolated_worktree", got)
	}
}

func TestResolveExecutionModeIgnoresDirectBranchForScratchProjects(t *testing.T) {
	cfg := ProjectConfig{ExecutionMode: ExecutionDirectBranch}
	if got := ResolveExecutionMode(ProjectKindScratch, cfg); got != ExecutionIsolatedWorktree {
		t.Fatalf("scratch project resolves to %q, want isolated_worktree (it has no repository)", got)
	}
	for _, kind := range []ProjectKind{"", ProjectKindSingleRepo, ProjectKindWorkspace} {
		if got := ResolveExecutionMode(kind, cfg); got != ExecutionDirectBranch {
			t.Fatalf("kind %q resolves to %q, want direct_branch", kind, got)
		}
	}
}

func TestGitPolicyDefaultsAreLocalOnly(t *testing.T) {
	got := (GitPolicy{}).WithDefaults()
	if got.LocalCommit != GitActionAutomatic {
		t.Fatalf("local commit default = %q, want automatic", got.LocalCommit)
	}
	if got.Push != GitActionNever {
		t.Fatalf("push default = %q, want never", got.Push)
	}
	if got.Merge != GitActionNever {
		t.Fatalf("merge default = %q, want never", got.Merge)
	}
}

func TestGitPolicyWithDefaultsPreservesExplicitChoices(t *testing.T) {
	got := (GitPolicy{LocalCommit: GitActionRequireApproval, Push: GitActionAutomatic}).WithDefaults()
	if got.LocalCommit != GitActionRequireApproval {
		t.Fatalf("local commit = %q, want the configured value preserved", got.LocalCommit)
	}
	if got.Push != GitActionAutomatic {
		t.Fatalf("push = %q, want the configured value preserved", got.Push)
	}
	if got.Merge != GitActionNever {
		t.Fatalf("merge = %q, want the unset field defaulted", got.Merge)
	}
}

// Direct branch has no AO-created work branch, so merge is not a setting that
// silently does nothing -- it is a hard never.
func TestEffectiveMergeIsNeverInDirectBranchMode(t *testing.T) {
	policy := GitPolicy{Merge: GitActionAutomatic}
	if got := policy.EffectiveMerge(ExecutionDirectBranch); got != GitActionNever {
		t.Fatalf("merge in direct-branch mode = %q, want never", got)
	}
	if got := policy.EffectiveMerge(ExecutionIsolatedWorktree); got != GitActionAutomatic {
		t.Fatalf("merge in worktree mode = %q, want the configured automatic", got)
	}
}

func TestProjectConfigValidateRejectsUnknownExecutionModeAndPolicy(t *testing.T) {
	if err := (ProjectConfig{ExecutionMode: "yolo"}).Validate(); err == nil {
		t.Fatal("unknown execution mode accepted")
	}
	if err := (ProjectConfig{Git: GitPolicy{Push: "sometimes"}}).Validate(); err == nil {
		t.Fatal("unknown git policy accepted")
	}
	if err := (ProjectConfig{ExecutionMode: ExecutionDirectBranch, Git: DefaultGitPolicy()}).Validate(); err != nil {
		t.Fatalf("valid direct-branch config rejected: %v", err)
	}
	// Unset is always valid: it means "use the default".
	if err := (ProjectConfig{}).Validate(); err != nil {
		t.Fatalf("empty config rejected: %v", err)
	}
}

func TestBranchLockKeyCanonicalizesPathsSoOneBranchIsOneLock(t *testing.T) {
	a := BranchLockKey("/repos/ao", "main")
	b := BranchLockKey("/repos/ao/", "main")
	c := BranchLockKey("/repos/ao/./", "main")
	if a != b || a != c {
		t.Fatalf("equivalent paths produced different lock keys: %q %q %q", a, b, c)
	}
	if a == BranchLockKey("/repos/ao", "other") {
		t.Fatal("different branches collapsed into one lock key")
	}
	if a == BranchLockKey("/repos/other", "main") {
		t.Fatal("different repositories collapsed into one lock key")
	}
}

func TestBranchLockConflictErrorNamesTheHolder(t *testing.T) {
	err := BranchLockConflictError{Holder: BranchLock{
		Branch: "feat/engineering-control-center", RepoPath: "/repos/ao", WorkflowRunID: "WF-7",
	}}
	msg := err.Error()
	for _, want := range []string{"feat/engineering-control-center", "/repos/ao", "WF-7"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("conflict message %q does not mention %q", msg, want)
		}
	}
	if !errors.Is(err, ErrBranchLockHeld) {
		t.Fatal("conflict does not unwrap to ErrBranchLockHeld")
	}
}
