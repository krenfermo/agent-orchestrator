package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// repair_artifact.go — resolving what a repair is a repair OF, before there is
// anything to repair it with.
//
// THE INCIDENT (wf-f951be84 -> wf-3af3c533, 2026-09). Reviewer 5debbc2f
// returned changes_requested on a task branch carrying 2,444 lines of new
// implementation. The fix worker terminated after 77 seconds having changed
// nothing, so AO launched a repair — and LaunchRepair created it with
// CreateTaskRun, which is an ordinary top-level run: parent_workflow_id NULL,
// planned_task_id NULL. workerBaseRef's predecessor (masterTaskBaseRef) returns
// "" for a run with no parent, so the session manager fell back to the
// project's default branch and cut the repair's worktree from origin/main.
//
// The repairer was given a checkout that did not contain either file the
// findings named. It correctly changed nothing, and AO parked the run on
// `worker_turn_produced_nothing` — a sentence about the worker, for a workspace
// AO had chosen.
//
// THE RULE. A repair targeting an existing artifact is cut from THAT ARTIFACT's
// exact commit or it is not cut at all. The project's default branch is not
// authority for it, and the absence of an answer is not permission to
// substitute one. Everything below is the resolution of that commit from
// durable facts, and the refusal when there is none.
//
// # Why a fresh worktree at the origin's HEAD, rather than the origin's own
//
// Both were available. A repair reusing the origin's checkout would see
// uncommitted work too, and would need no promotion afterwards — which is what
// the direct-branch path already does by moving the branch LOCK rather than the
// tree (repair_branch_cession.go). It also puts two agents' lifecycles on one
// working directory, where the fence is a proof rather than a property of the
// filesystem, and P1-E's placement model deliberately has no operation that
// re-points a running agent at a different checkout.
//
// So the isolated case takes the other option: a separate branch and worktree
// cut from the origin's exact committed head, with the origin's own checkout
// untouched, and an explicit promotion of the repaired candidate back onto the
// origin's branch afterwards (repair_promotion.go). The cost is that the
// artifact has to BE a commit, which is why an uncommitted artifact is a
// refusal here rather than a base AO picks the nearest commit for.

// ErrRepairArtifactUnavailable is the fail-closed refusal: the origin has
// produced an artifact and AO cannot establish which commit it is, so it will
// not create a repair at all.
//
// It is deliberately not a fallback. Every fallback available here — the
// project default branch, the origin's base commit, the newest commit on some
// branch — produces a checkout that looks exactly like a correct one and does
// not contain the code under review, which is the incident.
var ErrRepairArtifactUnavailable = errors.New("workflow: AO cannot establish the artifact this repair would be repairing, so it will not create one")

// repairWorkspaceResolvedPhase records, on the REPAIR run, that its checkout
// was materialised and does contain the artifact it was pinned to. It is the
// evidence §12 of the brief asks for: before a repair's empty turn may be
// called `worker_turn_produced_nothing`, AO must be able to show the worker was
// looking at the right code.
const repairWorkspaceResolvedPhase = "repair_workspace_resolved"

// repairWorkspaceMismatchPhase is the same question answered the other way.
const repairWorkspaceMismatchPhase = "repair_workspace_mismatch"

// repairChangedFilesLimit bounds the changed-file list carried into the repair
// context pack. It is evidence for a person and for a prompt, not an index.
const repairChangedFilesLimit = 60

// RepairGit is the narrow git surface artifact resolution and promotion need.
//
// It is a port for the same reason CommitHistory is (approved_head_recovery.go):
// the coordinator asks it three questions and naming them is what keeps "the
// workflow engine can run git" from being true in any broader sense. The
// default implementation shells out; a test injects its own.
type RepairGit interface {
	// CommitExists reports whether rev names a commit object that is present
	// in repoPath. It is how a reconstructed base is PROVEN rather than
	// assumed.
	CommitExists(ctx stdctx.Context, repoPath, rev string) (bool, error)
	// ChangedFiles lists the paths that differ between base and head.
	ChangedFiles(ctx stdctx.Context, repoPath, base, head string) ([]string, error)
	// Contains reports whether ancestor is reachable from descendant. It is
	// what makes "this checkout holds the artifact" answerable for a worker
	// that has already committed on top of it -- equality alone would call a
	// working repair a mismatch the moment it did its job.
	Contains(ctx stdctx.Context, repoPath, ancestor, descendant string) (bool, error)
	// FastForward advances the branch checked out at worktreePath to head. It
	// must refuse unless the worktree is clean and its HEAD is exactly
	// expectedHead — a promotion that overwrote somebody's uncommitted work, or
	// that landed on a tree that had moved, is not a promotion.
	FastForward(ctx stdctx.Context, worktreePath, expectedHead, head string) error
}

func (c *Coordinator) repairGit() RepairGit {
	if c.repairGitPort != nil {
		return c.repairGitPort
	}
	return execRepairGit{}
}

type execRepairGit struct{}

func (execRepairGit) CommitExists(ctx stdctx.Context, repoPath, rev string) (bool, error) {
	if strings.TrimSpace(repoPath) == "" || strings.TrimSpace(rev) == "" {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "cat-file", "-e", rev+"^{commit}")
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("workflow: probing commit %s in %s: %w", rev, repoPath, err)
	}
	return true, nil
}

func (execRepairGit) ChangedFiles(ctx stdctx.Context, repoPath, base, head string) ([]string, error) {
	if strings.TrimSpace(repoPath) == "" || strings.TrimSpace(base) == "" || strings.TrimSpace(head) == "" {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "diff", "--name-only", base+".."+head).Output()
	if err != nil {
		return nil, fmt.Errorf("workflow: diffing %s..%s in %s: %w", base, head, repoPath, err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

func (execRepairGit) Contains(ctx stdctx.Context, repoPath, ancestor, descendant string) (bool, error) {
	if strings.TrimSpace(repoPath) == "" || strings.TrimSpace(ancestor) == "" || strings.TrimSpace(descendant) == "" {
		return false, nil
	}
	err := exec.CommandContext(ctx, "git", "-C", repoPath, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("workflow: testing whether %s contains %s in %s: %w", descendant, ancestor, repoPath, err)
}

func (execRepairGit) FastForward(ctx stdctx.Context, worktreePath, expectedHead, head string) error {
	if strings.TrimSpace(worktreePath) == "" || strings.TrimSpace(head) == "" {
		return fmt.Errorf("%w: a promotion needs a worktree and a commit", ErrInvalid)
	}
	current, err := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("workflow: reading HEAD of %s: %w", worktreePath, err)
	}
	if got := strings.TrimSpace(string(current)); expectedHead != "" && got != expectedHead {
		return fmt.Errorf("%w: %s is at %s, not the %s the repair was cut from",
			ErrInvalid, worktreePath, got, expectedHead)
	}
	status, err := exec.CommandContext(ctx, "git", "-C", worktreePath, "status", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("workflow: reading the status of %s: %w", worktreePath, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("%w: %s has uncommitted changes, so a repaired candidate must not be fast-forwarded over it",
			ErrInvalid, worktreePath)
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", worktreePath, "merge", "--ff-only", head).CombinedOutput(); err != nil {
		return fmt.Errorf("workflow: fast-forwarding %s to %s: %w: %s", worktreePath, head, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveRepairArtifact freezes the identity of the artifact a repair of
// `target` would be repairing.
//
// It reads only, and it never returns an error: an artifact it cannot establish
// is an ANSWER — an unresolved authority carrying the refusal and the sentence
// that explains it — because "AO cannot prove what this repair would work on"
// is a true and useful statement about the run, and the caller has to record it
// either way.
func (c *Coordinator) resolveRepairArtifact(ctx stdctx.Context, target RunDetail, generation int) domain.RepairArtifactAuthority {
	a := domain.RepairArtifactAuthority{
		OriginRunID:     target.Run.ID,
		OriginProjectID: target.Run.ProjectID,
		Generation:      generation,
		At:              c.clock(),
	}
	if target.Run.PlannedTaskID != nil {
		a.OriginTaskID = *target.Run.PlannedTaskID
	}
	// refuse is the ONE way out of this function without an artifact, and it
	// always names the same refusal: every failure below is "AO cannot
	// establish the artifact". The one other refusal --
	// repair_artifact_uncommitted -- is established inside the workspace read,
	// which is the only place that can see the difference.
	refuse := func(format string, args ...any) domain.RepairArtifactAuthority {
		a.Resolved = false
		a.HasArtifact = true
		a.Refusal = domain.RepairArtifactUnavailable
		a.Detail = fmt.Sprintf(format, args...)
		return a
	}

	workStep, hasWork := lastStepOfKind(target, domain.WorkflowStepWork)
	if hasWork {
		a.OriginStepID = workStep.ID
		if workStep.SessionID != nil {
			a.OriginSessionID = *workStep.SessionID
		}
	}
	if fixStep, ok := lastStepOfKind(target, domain.WorkflowStepFix); ok {
		a.FixStepID = fixStep.ID
		if spent, known := c.fixCyclesSpent(ctx, target.Run.ID, fixStep.ID); known {
			a.FixCycle = spent
		}
	}
	c.annotateRepairReview(ctx, target, &a)

	// A run that has provably never executed has no artifact to be authority
	// over. That is not a refusal: the repair genuinely does start from the
	// project's default branch, and saying so explicitly is what keeps this
	// case distinguishable from the incident's silent fallback.
	if !hasWork || !c.runHasExecuted(ctx, workStep) {
		a.Resolved = true
		a.HasArtifact = false
		a.Source = domain.RepairArtifactNone
		return a
	}
	a.HasArtifact = true

	placement, hasPlacement := c.repairOriginPlacement(ctx, target.Run)
	switch {
	case hasPlacement:
		a.Placement = placement.Type
		a.PlacementGeneration = placement.PlacementGeneration
		a.RepoPath = placement.RepoPath
		a.OriginBranch = placement.ExecutionBranch
		a.OriginWorktreePath = placement.WorktreePath
		a.WorktreeRecordID = placement.WorktreeRecordID
		a.MergeTarget = placement.MergeTarget
	case c.runIDPlacementIsDirectBranch(ctx, target.Run.ID):
		a.Placement = domain.PlacementDirectBranch
	default:
		a.Placement = domain.PlacementIsolatedWorktree
	}
	if refused := c.fillRepairWorkspaceFacts(ctx, target, workStep, &a); refused {
		// A refusal established inside the workspace read is the precise one --
		// "the work is uncommitted" rather than "there is no commit" -- and it
		// must not be overwritten by the coarser check below.
		return a
	}

	if a.RepoPath == "" {
		a.RepoPath = c.projectPathFor(ctx, target.Run.ProjectID)
	}
	if a.RepoPath == "" {
		return refuse(
			"AO has no durable record of which repository run %s wrote in, so it cannot cut a repair checkout from it", target.Run.ID)
	}

	// A direct-branch artifact is the user's own working tree. There is nothing
	// to cut and nothing to pin: the repair takes the BRANCH LOCK instead, and
	// works in the same checkout the origin did (repair_branch_cession.go).
	if a.Placement == domain.PlacementDirectBranch {
		if a.OriginBranch == "" {
			return refuse(
				"run %s executes on the project's own checkout and AO cannot name the branch it holds", target.Run.ID)
		}
		a.Resolved = true
		a.Source = domain.RepairArtifactSharedCheckout
		return a
	}

	if a.OriginBranch == "" {
		return refuse(
			"AO has no durable record of the branch run %s wrote its work to", target.Run.ID)
	}
	if a.BaseSHA == "" {
		return refuse(
			"AO cannot establish a committed head for branch %s, so there is no commit to cut a repair checkout from", a.OriginBranch)
	}
	// The pin has to be real in the repository the checkout will be cut from.
	// A base AO cannot find is the incident again one level lower: `git
	// worktree add` would fall back through its own candidate list and produce
	// a plausible, wrong tree.
	switch exists, err := c.repairGit().CommitExists(ctx, a.RepoPath, a.BaseSHA); {
	case err != nil:
		return refuse(
			"AO could not read %s to confirm that %s is present in it (%v)", a.RepoPath, shortSHA(a.BaseSHA), err)
	case !exists:
		return refuse(
			"commit %s is not present in %s, so a repair checkout cannot be cut from it", shortSHA(a.BaseSHA), a.RepoPath)
	}

	a.Resolved = true
	a.ChangedFiles = c.repairArtifactChangedFiles(ctx, a)
	return a
}

// fillRepairWorkspaceFacts establishes the base commit, preferring a live
// reading of the origin's own checkout and falling back to the durable ledger
// when that checkout is gone.
//
// The two paths answer different questions and both are needed. The live
// reading is the only thing that can see UNCOMMITTED work, which is a refusal
// rather than a base — a repair cut from the last commit of a tree whose real
// change is still dirty would be handed a tree missing exactly the change under
// review. The ledger is the only thing left when a worktree has been cleaned
// up, and it is trustworthy for the opposite reason: AO wrote those commits
// down itself, and the branch still holds them.
func (c *Coordinator) fillRepairWorkspaceFacts(
	ctx stdctx.Context, target RunDetail, workStep domain.WorkflowStep, a *domain.RepairArtifactAuthority,
) (refused bool) {
	if cp, ok, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, workStep.ID); err == nil && ok {
		if a.OriginWorktreePath == "" {
			a.OriginWorktreePath = cp.WorktreePath
		}
		if a.OriginBranch == "" {
			a.OriginBranch = cp.Branch
		}
	}
	if rec, ok := c.repairOriginWorktreeRecord(ctx, target.Run); ok {
		if a.RepoPath == "" {
			a.RepoPath = rec.RepoPath
		}
		if a.OriginWorktreePath == "" {
			a.OriginWorktreePath = rec.Path
		}
		if a.OriginBranch == "" {
			a.OriginBranch = rec.Branch
		}
		if a.MergeTarget == "" {
			a.MergeTarget = rec.TargetBranch
		}
		if a.WorktreeRecordID == "" {
			a.WorktreeRecordID = rec.TaskID
		}
	}
	if a.Placement == domain.PlacementDirectBranch {
		return false
	}

	if c.workspaceFacts != nil && a.OriginWorktreePath != "" {
		obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
			Path:      a.OriginWorktreePath,
			Branch:    a.OriginBranch,
			SessionID: domain.SessionID(a.OriginSessionID),
			ProjectID: domain.ProjectID(target.Run.ProjectID),
		})
		if err == nil && obs.HeadSHA != "" {
			if dirty := significantChangePaths(obs); len(dirty) > 0 {
				a.Resolved = false
				a.HasArtifact = true
				a.Refusal = domain.RepairArtifactUncommitted
				a.Detail = fmt.Sprintf(
					"run %s still holds its work as uncommitted changes in %s (%s), and a second checkout cannot be cut from state that is not a commit",
					target.Run.ID, a.OriginWorktreePath, boundedPathList(dirty, 5))
				a.ChangedFiles = boundedRepairPaths(dirty)
				a.BaseSHA = ""
				return true
			}
			a.BaseSHA = obs.HeadSHA
			a.Source = domain.RepairArtifactObserved
			return false
		}
	}

	// The checkout is gone or unreadable. The commit AO recorded for this task
	// is still on its branch, and that is enough to reconstruct from.
	if sha := c.ledgerCommittedHead(ctx, target.Run.ID); sha != "" {
		a.BaseSHA = sha
		a.Source = domain.RepairArtifactReconstructed
	}
	return false
}

// significantChangePaths is the uncommitted work that would be LOST by cutting
// a repair checkout from the origin's committed head. Ephemeral build/cache
// artifacts are excluded by the same policy the workspace fingerprint uses, so
// a stray __pycache__ never refuses a repair.
func significantChangePaths(obs ports.WorkspaceObservation) []string {
	var out []string
	for _, ch := range obs.Changes {
		if IsEphemeralArtifactPath(ch.Path) {
			continue
		}
		out = append(out, ch.Path)
	}
	sort.Strings(out)
	return out
}

// ledgerCommittedHead folds a run's ledger for the newest commit AO itself
// recorded for it.
//
// It reads ONLY the two phases that record a commit -- the local-commit phase
// both the direct-branch and isolated paths write, and the no-change phase that
// records the head a task finished at with nothing to commit. Every other
// checkpoint is excluded on purpose: the HeadSHA column is shared with the
// review dispatch trail, which stores a 64-character WORKSPACE FINGERPRINT in
// it. "The newest checkpoint carrying a head" therefore returns a fingerprint
// for most runs, and a fingerprint used as a base commit is a repair cut from
// nothing at all. The same strict read is what task_integration_route.go and
// task_integration_baseline.go already do.
func (c *Coordinator) ledgerCommittedHead(ctx stdctx.Context, runID string) string {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return ""
	}
	sha := ""
	for _, cp := range cps {
		switch cp.DurablePhase {
		case autonomousLocalCommitPhase:
			if head := strings.TrimSpace(cp.HeadSHA); head != "" {
				sha = head
				continue
			}
			// Older rows carried the commit only in the sentence.
			if _, after, found := strings.Cut(cp.NextAction, "local_commit_created: "); found {
				if fields := strings.Fields(after); len(fields) > 0 {
					sha = fields[0]
				}
			}
		case isolatedNoChangePhase:
			if head := strings.TrimSpace(cp.HeadSHA); head != "" {
				sha = head
			}
		}
	}
	return sha
}

// repairOriginPlacement reads the origin's frozen execution placement without
// freezing one. Resolution must never mint a placement: it is a read of what
// the origin already decided, and a run with none simply has less evidence.
func (c *Coordinator) repairOriginPlacement(ctx stdctx.Context, run domain.WorkflowRun) (domain.ExecutionPlacement, bool) {
	if c.placements == nil {
		return domain.ExecutionPlacement{}, false
	}
	scope := placementScopeFor(run)
	placement, ok, err := c.placements.GetLiveExecutionPlacement(ctx, scope.runID, scope.taskID, scope.stepID)
	if err != nil || !ok || !placement.Valid() {
		return domain.ExecutionPlacement{}, false
	}
	return placement, true
}

// repairOriginWorktreeRecord finds the AO-owned worktree record for the
// origin's own task, which proves an isolated placement and carries the repo,
// the branch, the base and the target.
func (c *Coordinator) repairOriginWorktreeRecord(ctx stdctx.Context, run domain.WorkflowRun) (domain.TaskWorktreeRecord, bool) {
	if c.taskWorktreeRecords == nil {
		return domain.TaskWorktreeRecord{}, false
	}
	recs, err := c.taskWorktreeRecords.ListTaskWorktreesByRun(ctx, run.ID)
	if err != nil {
		return domain.TaskWorktreeRecord{}, false
	}
	want := ""
	if run.PlannedTaskID != nil {
		want = *run.PlannedTaskID
	}
	for _, rec := range recs {
		if want == "" || rec.TaskID == want {
			return rec, true
		}
	}
	return domain.TaskWorktreeRecord{}, false
}

// annotateRepairReview attaches the review cycle that asked for the repair:
// which review, what it concluded, which commit it judged, and a digest of the
// findings themselves.
//
// The findings BODY is deliberately not stored here — a digest, a byte count
// and a finding count are what make "the repair was given the same findings the
// fix cycle was" checkable without this record becoming a second copy of them.
// The body is read live when the repair's objective is built (repair_agent.go),
// from the same accessor the fix dispatch uses.
func (c *Coordinator) annotateRepairReview(ctx stdctx.Context, target RunDetail, a *domain.RepairArtifactAuthority) {
	if c.reviewRuns == nil {
		return
	}
	step, ok := lastStepOfKind(target, domain.WorkflowStepReview)
	if !ok || step.ReviewRunID == nil || *step.ReviewRunID == "" {
		return
	}
	rr, found, err := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID)
	if err != nil || !found {
		return
	}
	a.ReviewRunID = rr.ID
	a.ReviewVerdict = string(rr.EffectiveVerdict())
	a.ReviewTargetKey = rr.TargetSHA
	if body := rr.EffectiveBody(); body != "" {
		a.FindingsDigest = FindingsDigest(body)
		a.FindingsBytes = len(body)
		a.FindingsCount = CountReviewFindings(body)
	}
}

// repairArtifactChangedFiles lists what the artifact actually changes relative
// to the commit its own branch was cut from. It is context for the repairer and
// evidence for a person; it fences nothing, so a git failure yields no list
// rather than a refusal.
func (c *Coordinator) repairArtifactChangedFiles(ctx stdctx.Context, a domain.RepairArtifactAuthority) []string {
	base := a.MergeTarget
	if base == "" {
		return nil
	}
	files, err := c.repairGit().ChangedFiles(ctx, a.RepoPath, base, a.BaseSHA)
	if err != nil {
		return nil
	}
	return boundedRepairPaths(files)
}

// boundedRepairPaths caps an artifact's changed-file list. It is separate from
// task_memory.go's boundedPaths because the two carry different budgets for
// different readers: a memory anchor is a handful of paths, a repair context
// pack is what a person and a repairer read to find the code under review.
func boundedRepairPaths(paths []string) []string {
	if len(paths) <= repairChangedFilesLimit {
		return paths
	}
	return paths[:repairChangedFilesLimit]
}

func boundedPathList(paths []string, n int) string {
	if len(paths) > n {
		return strings.Join(paths[:n], ", ") + fmt.Sprintf(" and %d more", len(paths)-n)
	}
	return strings.Join(paths, ", ")
}

// lastStepOfKind is the newest step of one kind in a run's chain. Newest rather
// than first because a chain can carry more than one of a kind after a retry,
// and the artifact belongs to the latest.
func lastStepOfKind(detail RunDetail, kind domain.WorkflowStepKind) (domain.WorkflowStep, bool) {
	var found domain.WorkflowStep
	ok := false
	for _, sd := range detail.Steps {
		if sd.Step.Kind == kind {
			found, ok = sd.Step, true
		}
	}
	return found, ok
}

// ---------------------------------------------------------------------------
// Reading the authority back
// ---------------------------------------------------------------------------

// repairRunOriginPayload is the repair run's own origin marker. The first two
// fields are the pre-existing shape every other reader parses
// (board_projection.go, branch_cession_chain.go); Origin is the addition, and
// an older row simply decodes with a zero authority.
type repairRunOriginPayload struct {
	OriginRunID string                         `json:"originRunId"`
	Generation  int                            `json:"generation"`
	Origin      domain.RepairArtifactAuthority `json:"origin,omitzero"`
}

// repairArtifactFor reads back the frozen authority of a repair run.
//
// Not-ok means this run is not a repair, or its marker predates artifact
// authority. Both are answers a caller must handle rather than defaults it may
// paper over: the second is precisely a repair created before this fix, whose
// checkout came from the project default and must not be described as a repair
// of anything in particular.
func (c *Coordinator) repairArtifactFor(ctx stdctx.Context, runID string) (domain.RepairArtifactAuthority, bool) {
	_, authority, ok := c.repairOriginMarker(ctx, runID)
	return authority, ok
}

// repairOriginMarker folds a run's ledger ONCE for both questions the launch
// path asks of it: is this a repair run at all, and what artifact was it frozen
// against. They are one scan because they are one row -- asking them separately
// meant reading the whole ledger twice per dispatch for an answer that is
// almost always "not a repair".
func (c *Coordinator) repairOriginMarker(ctx stdctx.Context, runID string) (isRepair bool, authority domain.RepairArtifactAuthority, hasAuthority bool) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		// Fail-safe in the same direction isRepairRun fails: an unreadable
		// ledger reads as an ordinary run. It must not read as a repair whose
		// authority is missing, because that is a refusal, and refusing every
		// dispatch on a transient read error would park runs that are fine.
		return false, domain.RepairArtifactAuthority{}, false
	}
	for _, cp := range cps {
		if cp.DurablePhase != repairRunOriginPhase {
			continue
		}
		isRepair = true
		if cp.RetryState == "" {
			continue
		}
		var body repairRunOriginPayload
		if json.Unmarshal([]byte(cp.RetryState), &body) != nil {
			continue
		}
		if !body.Origin.Valid() {
			continue
		}
		return true, body.Origin, true
	}
	return isRepair, domain.RepairArtifactAuthority{}, false
}

// repairBaseRef is the base a repair run's worker checkout must be cut from,
// and the refusal when there is one.
//
// ok=false with a non-empty refusal means this IS a repair run and AO cannot
// point it at its artifact — the dispatch must not proceed. ok=false with an
// empty refusal means the run is not a repair run at all, or is one that needs
// no pin (direct branch, or an origin that never executed), and the ordinary
// base-ref rules apply.
func (c *Coordinator) repairBaseRef(ctx stdctx.Context, run domain.WorkflowRun) (base string, refusal domain.RepairArtifactAuthority, isPinned bool) {
	isRepair, authority, ok := c.repairOriginMarker(ctx, run.ID)
	if !isRepair {
		return "", domain.RepairArtifactAuthority{}, false
	}
	if !ok {
		// A repair run whose authority cannot be read is the incident's own
		// shape: it would be cut from the project default and look correct.
		// Refuse rather than launch.
		return "", domain.RepairArtifactAuthority{
			Refusal: domain.RepairArtifactUnavailable,
			Detail: fmt.Sprintf(
				"repair run %s carries no readable record of the artifact it was created to repair, so AO will not launch a worker into a checkout it cannot vouch for", run.ID),
		}, false
	}
	if !authority.PinsCheckout() {
		return "", domain.RepairArtifactAuthority{}, false
	}
	return authority.BaseRef(), authority, true
}

// workerBaseRef is the single answer to "what is this run's worker checkout cut
// from", for every kind of run.
//
// Two authorities, in order, and neither of them is project configuration:
//
//	a REPAIR run is cut from the exact commit of the artifact it was created to
//	repair, which is the whole of repair artifact authority;
//	a master child task is cut from its master's accumulated integration state
//	(Checkpoint 8M.1), so a dependency's code reaches it.
//
// An empty answer means "the project's default branch is correct for this run",
// which is true of an ordinary task and is the pre-existing behaviour. It is
// never how a pinned repair ends up: repairArtifactBlocks refuses that dispatch
// before it reaches here.
func (c *Coordinator) workerBaseRef(ctx stdctx.Context, run domain.WorkflowRun) string {
	if base, _, pinned := c.repairBaseRef(ctx, run); pinned {
		return base
	}
	return c.masterTaskBaseRef(ctx, run)
}

// repairArtifactBlocks refuses to launch a repair worker into a checkout AO
// cannot vouch for, and parks the repair run saying why.
//
// It returns false for every run that is not a repair, and for a repair whose
// authority reads cleanly -- which is every ordinary dispatch, at the cost of
// one ledger read that isRepairRun was already going to do.
func (c *Coordinator) repairArtifactBlocks(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) bool {
	_, refusal, pinned := c.repairBaseRef(ctx, run)
	if pinned || refusal.Refusal == "" {
		return false
	}
	now := c.clock()
	stepID := step.ID
	payload, err := json.Marshal(refusal)
	if err != nil {
		payload = []byte("{}")
	}
	// One row per condition, not one per pass. Every entry point into dispatch
	// re-derives this refusal and it is a standing condition, so minting a
	// checkpoint each time is the write storm checkpoint_authority.go was
	// written about -- and it would also make the newest stop a fresh
	// duplicate of itself on every boot.
	if reason, _, ok := c.stopReason(ctx, run); !ok || reason != ReasonRepairArtifactUnavailable {
		c.recordAttentionStopWithState(ctx, run, &stepID, ReasonRepairArtifactUnavailable, refusal.Detail, string(payload))
	}
	if run.State == domain.WorkflowRunRunning || run.State == domain.WorkflowRunWaiting || run.State == domain.WorkflowRunPending {
		if _, uerr := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); uerr != nil && c.log != nil {
			c.log.Warn("workflow: repair artifact refusal state transition failed", "run", run.ID, "err", uerr)
		}
	}
	if c.log != nil {
		c.log.Warn("workflow: refusing to dispatch a repair worker into an unproven workspace",
			"run", run.ID, "step", step.ID, "detail", refusal.Detail)
	}
	return true
}

// ---------------------------------------------------------------------------
// Proving the checkout after the fact
// ---------------------------------------------------------------------------

// recordRepairWorkspaceEvidence writes down whether a repair's materialised
// checkout actually contains the artifact it was pinned to.
//
// This is §12 of the brief. `worker_turn_produced_nothing` is a judgement about
// a WORKER, and AO may only reach for it once it can show the worker was
// looking at the right code. The evidence is written at confirmation time --
// before the worker has done anything -- so it describes the workspace rather
// than the outcome, and it is derived from the observation confirmation has
// already paid for rather than from a second read that could disagree with it.
func (c *Coordinator) recordRepairWorkspaceEvidence(
	ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep,
	sessionID, branch, worktreePath, observedHead string,
) {
	authority, ok := c.repairArtifactFor(ctx, run.ID)
	if !ok || !authority.PinsCheckout() {
		return
	}
	matched := false
	var detail string
	switch {
	case worktreePath == "":
		detail = "the repair's worker session names no workspace path, so AO cannot show it holds the artifact under repair"
	case observedHead == "":
		detail = fmt.Sprintf("AO could not read a commit from the repair's checkout at %s", worktreePath)
	case observedHead == authority.BaseSHA:
		matched = true
		detail = fmt.Sprintf("repair checkout %s is at %s, the artifact of run %s on %s",
			worktreePath, shortSHA(authority.BaseSHA), authority.OriginRunID, authority.OriginBranch)
	default:
		// A checkout that has moved PAST the pin still holds it: the repairer
		// committing on top of the artifact is the thing this whole file exists
		// to make possible, and calling that a mismatch would flag a working
		// repair. What is a mismatch is a checkout the artifact is not in at
		// all, which is exactly the origin/main tree of the incident.
		contains, cerr := c.repairGit().Contains(ctx, worktreePath, authority.BaseSHA, observedHead)
		switch {
		case cerr != nil:
			detail = fmt.Sprintf("AO could not tell whether the repair's checkout at %s contains %s (%v)",
				worktreePath, shortSHA(authority.BaseSHA), cerr)
		case contains:
			matched = true
			detail = fmt.Sprintf("repair checkout %s is at %s, which contains %s, the artifact of run %s on %s",
				worktreePath, shortSHA(observedHead), shortSHA(authority.BaseSHA), authority.OriginRunID, authority.OriginBranch)
		default:
			detail = fmt.Sprintf("repair checkout %s is at %s, which does not contain %s, the artifact of run %s on %s",
				worktreePath, shortSHA(observedHead), shortSHA(authority.BaseSHA), authority.OriginRunID, authority.OriginBranch)
		}
	}
	phase := repairWorkspaceMismatchPhase
	if matched {
		phase = repairWorkspaceResolvedPhase
	}
	payload, err := json.Marshal(struct {
		Resolved     bool   `json:"repairArtifactResolved"`
		BaseSHA      string `json:"repairBaseSha"`
		ObservedSHA  string `json:"repairObservedSha,omitempty"`
		Workspace    string `json:"repairWorkspace,omitempty"`
		OriginRunID  string `json:"originRunId"`
		OriginTaskID string `json:"originTaskId,omitempty"`
		OriginBranch string `json:"originBranch,omitempty"`
		Generation   int    `json:"generation"`
		Detail       string `json:"detail,omitempty"`
	}{
		Resolved: matched, BaseSHA: authority.BaseSHA, ObservedSHA: observedHead, Workspace: worktreePath,
		OriginRunID: authority.OriginRunID, OriginTaskID: authority.OriginTaskID,
		OriginBranch: authority.OriginBranch, Generation: authority.Generation, Detail: detail,
	})
	if err != nil {
		return
	}
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		SessionID:      stringPtrOrNil(sessionID),
		Branch:         branch,
		WorktreePath:   worktreePath,
		BaseSHA:        authority.BaseSHA,
		HeadSHA:        observedHead,
		NextAction:     phase + ": " + detail,
		DurablePhase:   phase,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		c.log.Warn("workflow: repair workspace evidence not recorded", "run", run.ID, "err", err)
	}
	if !matched && c.log != nil {
		c.log.Warn("workflow: a repair worker was launched into a checkout that does not hold the artifact under repair",
			"run", run.ID, "step", step.ID, "detail", detail)
	}
}

// repairWorkspaceProven answers "was this repair's worker looking at the right
// code?" from the evidence above.
//
// Three answers, and the third is why it is a tri-state rather than a bool:
// proven, DISPROVEN (AO chose the wrong workspace, which is AO's defect), and
// unknown (this is not a repair run, or nothing recorded evidence). Only the
// middle one may change what a stopped run is told.
type repairWorkspaceVerdict int

const (
	repairWorkspaceUnknown repairWorkspaceVerdict = iota
	repairWorkspaceProven
	repairWorkspaceDisproven
)

func (c *Coordinator) repairWorkspaceProven(ctx stdctx.Context, runID string) (repairWorkspaceVerdict, string) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return repairWorkspaceUnknown, ""
	}
	verdict, detail := repairWorkspaceUnknown, ""
	for _, cp := range cps {
		switch cp.DurablePhase {
		case repairWorkspaceResolvedPhase:
			verdict, detail = repairWorkspaceProven, cp.NextAction
		case repairWorkspaceMismatchPhase:
			verdict, detail = repairWorkspaceDisproven, cp.NextAction
		}
	}
	return verdict, detail
}

// reclassifyRepairWorkspaceStop rewrites `worker_turn_produced_nothing` for a
// repair whose checkout provably did not hold the artifact under repair.
//
// §12 of the brief, stated as the rule it enforces: the produced-nothing guard
// stays exactly as it is, and it may only be reached once AO can show the
// worker was operating on the correct artifact. A worker that changed nothing
// in the RIGHT tree is a judgement about the work, which is a person's. A
// worker that changed nothing in a tree AO cut from the wrong commit is not a
// judgement about anything -- it is AO's own workspace error, and the sentence
// a person reads must say so rather than asking them to decide whether an empty
// turn was correct for a task the agent could not see.
//
// It changes nothing for a run that is not a repair, for a repair whose
// workspace was proven, and for every decision that is not this one.
func (c *Coordinator) reclassifyRepairWorkspaceStop(ctx stdctx.Context, run domain.WorkflowRun, decision WorkStepDecision) WorkStepDecision {
	if decision.AttentionReason != ReasonWorkerTurnProducedNothing {
		return decision
	}
	verdict, detail := c.repairWorkspaceProven(ctx, run.ID)
	if verdict != repairWorkspaceDisproven {
		return decision
	}
	decision.AttentionReason = ReasonRepairWorkspaceMismatch
	decision.NextAction = "repair_workspace_mismatch: the worker reported its turn finished and changed nothing, but its checkout does not hold the artifact this repair was created for — " + detail
	if c.log != nil {
		c.log.Warn("workflow: an empty repair turn was reclassified as an AO workspace error",
			"run", run.ID, "detail", detail)
	}
	return decision
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
