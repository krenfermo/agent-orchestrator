package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// branch_cession_chain.go — a branch handed down a chain of repairs has to be
// able to come back up it.
//
// THE INCIDENT (wf-c4c84f52, holding blk-dc8e9a89 on
// feat/engineering-control-center). Three runs, in this relation:
//
//	wf-724a1e97   the origin task, parked, waiting to adopt a newer head
//	  wf-f5025a7c   its repair generation 2, parked for a person
//	    wf-c4c84f52   THAT repair's own repair, parked for a person,
//	                  holding the branch
//
// Every one of them is stopped on a human-owned decision. None of them can
// write. And the branch stayed with the deepest one indefinitely, because
// retention's answer for "a parked owner that left uncommitted work in the
// repository" is — correctly — to hold the lock rather than let a second
// workflow write over it. Correct, and terminal: the only way out was a person
// pressing Cancel, which is precisely the dependency this work exists to
// remove.
//
// P1-D §L already knew how to move a branch to a repair and back again, but it
// modelled ONE hop. returnBranchLockFromRepair walks the origin's own ledger
// for cessions the origin made, so an origin whose repair ceded the branch
// onward sees nothing to return: the ledger row that describes the second hop
// lives on the middle run, and nothing was reading it. Ownership effectively
// ended at whichever run received it last.
//
// WHAT THIS FILE ADDS is the missing structure: a cession is a LINK, links form
// a durable stack, and the stack unwinds one link per reconciliation, each link
// under exactly the same proof the single hop always required.
//
//	C is proven unable to write, and C is the exact owner  ->  C gives it to B
//	B is proven unable to write, and B is the exact owner  ->  B gives it to A
//	only then does A hold its own branch again
//
// It never jumps C -> A. The relation each hop restores is the one the ledger
// records, and a hop whose provenance cannot be read is refused with a reason
// rather than guessed at.
//
// # Two kinds of link, and why the second one exists
//
// A CEDED link is a branch_lock_ceded_to_repair row: the previous owner held
// the lock and handed it over. It is the strongest evidence there is, and it is
// what the fold prefers wherever it exists.
//
// A CUSTODY link is the shape the real incident actually has, and pretending
// otherwise would have shipped a fix for a chain that does not exist. When
// wf-f5025a7c was created, wf-724a1e97 held no lock to cede — generation 1's
// repair had completed minutes earlier and released it — so the repair took the
// branch through the ordinary queue, in its own name. There is no cession row
// for that hop and there never will be. What there IS, durably and on both
// sides:
//
//	the repair run carries workflow_repair_run_origin naming its origin and
//	  generation, written before it was ever started;
//	the origin carries the workflow_repair_dispatched intent for that same
//	  generation, naming that same repair run;
//	the origin's own checkpoints record it working that exact branch in that
//	  exact repository, which is what makes the branch the origin's to get back
//	  rather than one the repair found lying around;
//	the lock was acquired after the repair run existed, so it belongs to the
//	  repair episode and not to something older that happens to be held.
//
// All four, or there is no custody link. That is deliberately not "the repair's
// parent_workflow_id says so": a run relation is not a branch relation, and the
// branch ledger is what has to prove a branch claim. Where the four hold, the
// repair holds the branch on its origin's behalf in exactly the sense a cession
// would have recorded, and handing it back is a transfer between the two runs
// the ledger already binds together — never a release into the open, and never
// a seizure from a third party.
//
// # What is never relaxed
//
// The holder must be PROVEN quiescent by repair_quiescence.go's eight-clause
// proof, re-derived at the moment of the fold. The holder must be the EXACT
// current owner: same lock id, same branch, same repository, same owner token
// as the row this pass read, still held. The transfer is the same
// compare-and-set cession has always used, conditioned on who holds it right
// now, so two daemons racing produce one transfer and a stale pass produces
// none. Anything unreadable, unprovable or ambiguous fails closed with a named
// reason.
//
// # Ordering, and why the record comes second here
//
// cedeBranchLockToRepair writes its ledger row BEFORE the transfer, because the
// dangerous crash there is a branch that moved with nothing to explain it. A
// RETURN has the opposite hazard. Writing "returned" first and crashing would
// close the cession in the ledger while the branch is still with the repair,
// and cededBranchLocks would then never list it again: a permanent leak, exactly
// the incident, one restart earlier. So the fold transfers first and records
// second, and the bookkeeping the crash skipped is completed by the next pass
// from the lock table, which is the authority on who holds what. A crash before
// the CAS leaves nothing at all, which is the other correct direction.
//
// # Bounded, and stepwise on purpose
//
// One link per chain per reconciliation. No recursion, no cascade, no
// transaction spanning several transfers: each fold is one CAS plus one row,
// and the next link becomes foldable only once the previous one is durably
// done. The chain walk itself is depth-capped and cycle-guarded, so a
// malformed ledger costs a refusal rather than a loop.

// maxBranchCessionDepth caps the walk. Real chains are one or two links; the
// anti-recursion guard in repair_agent.go means new ones cannot exceed one. The
// cap is here so a corrupt or adversarial ledger cannot turn a reconciliation
// pass into an unbounded walk, not because a legitimate chain could be deep.
const maxBranchCessionDepth = 8

// The two link kinds. Persisted in the fold record so a later reader can tell
// which evidence a transfer rested on.
const (
	branchCessionKindCeded   = "ceded"
	branchCessionKindCustody = "custody"
)

// branchCustodyReturnedPhase records the fold of a CUSTODY link. It is a
// separate phase from branchLockReturnedPhase on purpose: that phase closes a
// cession row, and a custody link has no cession row to close. Conflating them
// would make cededBranchLocks think a cession it never saw had been returned.
const branchCustodyReturnedPhase = "branch_custody_returned_to_origin"

// Blocked reasons. These are AO's own vocabulary, stable enough for the API and
// the UI to key on, and every one of them means "no transfer happened".
const (
	// branchCessionReturnable is the empty reason: the deepest link can fold.
	branchCessionReturnable = ""
	// branchCessionHolderCanWrite: the current holder failed the quiescence
	// proof. The detail carries the clause that refused.
	branchCessionHolderCanWrite = "holder_can_still_write"
	// branchCessionLegacyUnprovable: a link in the chain cannot be proven from
	// durable evidence. This is the legacy case — a chain that predates the
	// anti-recursion guard, whose provenance was never fully written down. It
	// is reported, never guessed around, and never resolved by cancelling
	// anything.
	branchCessionLegacyUnprovable = "legacy_unprovable_branch_cession"
	// branchCessionOwnerNotAuthoritative: the run the branch would go back to
	// is terminal or cancelled, so there is no authority to return it to. The
	// ordinary retention policy already releases a terminal owner's lock, so
	// refusing here leaks nothing; inventing a different owner would.
	branchCessionOwnerNotAuthoritative = "previous_owner_not_authoritative"
	// branchCessionBranchMovedOn: the lock is no longer where the ledger says
	// it is — released, or held by somebody outside this chain. Nothing is
	// taken from whoever holds it now.
	branchCessionBranchMovedOn = "branch_moved_on"
)

// BranchCessionChain is the read projection of one branch's cession stack. It
// is what the API reports so a person (and later the UI) can see why a branch
// is where it is, without a redesign: who holds it, how far from its origin,
// whether AO can bring it back by itself, and if not, which fact is missing.
//
// Re-derived on every read from durable rows. Nothing here is stored.
type BranchCessionChain struct {
	// OriginRunID is the run at the top of the stack — the one the branch
	// unwinds back to, one link at a time.
	OriginRunID string
	// CurrentHolderRunID is the run that holds the lock right now.
	CurrentHolderRunID string
	// Depth is the number of links between the origin and the holder: 1 for a
	// single cession, 2 for the repair-of-repair shape.
	Depth int
	// Returnable reports that the deepest link can be folded now.
	Returnable bool
	// BlockedReason is one of the constants above, empty when Returnable.
	BlockedReason string
	// Detail is AO's own sentence about that reason, including the quiescence
	// clause that refused when there is one.
	Detail string
	// LockID/Branch/RepoPath name what is actually held.
	LockID   string
	Branch   string
	RepoPath string
	// Kind is the deepest link's evidence: ceded or custody.
	Kind string
}

// branchCessionLink is one hop, resolved against durable evidence.
type branchCessionLink struct {
	Kind string
	// Record is the transfer as the ledger has it. For a ceded link it is the
	// stored row; for a custody link it is derived from the repair binding and
	// the lock, and is what the fold record will persist.
	Record branchCessionRecord
	// PreviousOwnerRunID is where the branch goes back to; HolderRunID is who
	// has it now (or was given it by this link).
	PreviousOwnerRunID string
	HolderRunID        string
}

// branchCessionChain is one lock's resolved stack, outermost link first.
type branchCessionChain struct {
	OriginRunID string
	LockID      string
	Links       []branchCessionLink
	// Blocked and Detail are set when the walk itself could not be completed.
	Blocked string
	Detail  string
}

// reconcileBranchCessionChain folds at most one link of each chain rooted at
// this run.
//
// It is the automatic entry point: converge() calls it on every parked run, and
// boot reconciliation calls converge(). Nobody presses anything, and a pass that
// can prove nothing changes nothing.
func (c *Coordinator) reconcileBranchCessionChain(ctx stdctx.Context, origin domain.WorkflowRun) {
	if c.branchLocks == nil {
		return
	}
	if _, ok := c.branchLocks.(branchLockCeder); !ok {
		return
	}
	for _, chain := range c.branchCessionChains(ctx, origin) {
		c.foldDeepestBranchCessionLink(ctx, chain)
	}
}

// foldDeepestBranchCessionLink returns one branch one step up its own chain.
//
// Every refusal below is a return, never a fallback: there is no path through
// this function that moves a lock on anything less than the full proof.
func (c *Coordinator) foldDeepestBranchCessionLink(ctx stdctx.Context, chain branchCessionChain) {
	if len(chain.Links) == 0 || chain.Blocked != branchCessionReturnable {
		return
	}
	link := chain.Links[len(chain.Links)-1]
	holder, found, err := c.store.GetWorkflowRun(ctx, link.HolderRunID)
	if err != nil || !found {
		return
	}
	previous, found, err := c.store.GetWorkflowRun(ctx, link.PreviousOwnerRunID)
	if err != nil || !found || previous.State.Terminal() {
		// Nothing to hand back to. Refused rather than redirected: the branch
		// stays where it is and retention, which already releases a terminal
		// owner's lock, is the rule that applies to a terminal previous owner.
		return
	}

	// The proof, re-derived now. proveRepairQuiescent is the same eight clauses
	// the single-hop fold has always required, asked about THIS link's holder
	// as a repair of THIS link's previous owner — which is also what fences a
	// stale generation, since its clause (8) refuses any intent that is not the
	// previous owner's current one.
	intent, ok := c.repairIntentForLink(ctx, link)
	if !ok {
		return
	}
	proof := c.proveRepairQuiescent(ctx, previous, intent, holder)
	if !proof.Quiescent {
		return
	}

	// The holder must be the EXACT owner of the exact lock this link describes,
	// read fresh from the lock table rather than from the ledger row that
	// started the walk.
	lock, ok := c.heldLockByID(ctx, link.HolderRunID, link.Record.LockID)
	if !ok {
		// It moved while this pass was thinking. The next pass re-derives from
		// wherever it actually is; nothing is taken from its current holder.
		return
	}
	if !branchCessionLockMatches(lock, link.Record) {
		if c.log != nil {
			c.log.Warn("workflow: refusing to fold a branch cession whose lock no longer matches its record",
				"lock", lock.ID, "holder", link.HolderRunID, "branch", lock.Branch)
		}
		return
	}
	if link.Kind == branchCessionKindCustody && !c.custodyLockIsThisRepairEpisode(ctx, lock, holder) {
		return
	}

	ceder, ok := c.branchLocks.(branchLockCeder)
	if !ok {
		return
	}
	// The transfer, then the record. A crash between them leaves the branch
	// already back with its previous owner and no row saying so, which the next
	// pass completes from the lock table (completeBranchCessionBookkeeping).
	// The reverse order would close the cession over a branch that never moved.
	moved, err := ceder.Cede(ctx, lock.ID, link.HolderRunID, link.PreviousOwnerRunID, "")
	if err != nil {
		if c.log != nil {
			c.log.Warn("workflow: folding a branch cession failed", "lock", lock.ID,
				"from", link.HolderRunID, "to", link.PreviousOwnerRunID, "err", err)
		}
		return
	}
	if !moved {
		// Somebody else got there first — the other daemon in the race, or a
		// release. Exactly one transfer happened; this pass is the one that did
		// not make it, and it writes nothing.
		return
	}
	c.recordBranchCessionFold(ctx, previous, link, proof)
	if c.log != nil {
		c.log.Info("workflow: a branch came back one link of its cession chain",
			"lock", lock.ID, "branch", lock.Branch, "from", link.HolderRunID,
			"to", link.PreviousOwnerRunID, "kind", link.Kind, "origin", chain.OriginRunID,
			"proof", proof.Reason)
	}
}

// recordBranchCessionFold writes the durable account of one fold.
//
// The checkpoint id is DERIVED from the lock, the holder and the generation
// rather than minted, so the row is exactly-once by primary key: a second
// writer — a racing daemon completing bookkeeping, a restart re-deriving the
// same facts — collides instead of appending a second account of one transfer.
func (c *Coordinator) recordBranchCessionFold(
	ctx stdctx.Context, previous domain.WorkflowRun, link branchCessionLink, proof repairQuiescence,
) {
	rec := link.Record
	rec.Kind = link.Kind
	rec.At = c.clock()
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	phase := branchLockReturnedPhase
	if link.Kind == branchCessionKindCustody {
		phase = branchCustodyReturnedPhase
	}
	detail := fmt.Sprintf(
		"branch %s came back to run %s from %s run %s, which is parked for a person and proven unable to write (%s)",
		rec.Branch, link.PreviousOwnerRunID, link.Kind, link.HolderRunID, proof.Reason)
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             branchCessionFoldID(rec.LockID, link.HolderRunID, rec.RepairGeneration),
		WorkflowRunID:  previous.ID,
		ProjectID:      previous.ProjectID,
		DurablePhase:   phase,
		NextAction:     detail,
		PayloadVersion: "v1",
		RetryState:     string(payload),
		Branch:         rec.Branch,
		WorktreePath:   rec.RepoPath,
		CreatedAt:      c.clock(),
	}); err != nil && c.log != nil {
		// The transfer happened and is visible in the lock table; only the
		// audit line is missing, and the next pass writes it.
		c.log.Warn("workflow: recording a branch cession fold failed",
			"lock", rec.LockID, "run", previous.ID, "err", err)
	}
}

// branchCessionFoldID is the derived, collision-by-design identity of one
// fold's record.
func branchCessionFoldID(lockID, holderRunID string, generation int) string {
	return fmt.Sprintf("wfc-cessionfold-%s-%s-g%d", lockID, holderRunID, generation)
}

// ---------------------------------------------------------------------------
// Resolving a chain.
// ---------------------------------------------------------------------------

// branchCessionChains resolves every chain rooted at this run, one per lock.
//
// Roots come from two places, and only these two:
//
//	the run's own outstanding cession rows — branches it demonstrably handed to
//	  a repair and has not been given back;
//	its CURRENT repair generation's custody of a branch that is the run's own,
//	  which is the shape a repair that acquired the branch itself produces.
//
// From each root the walk follows outstanding cession rows written by the
// holder, so a repair that ceded onward is followed to whoever actually has the
// branch now.
func (c *Coordinator) branchCessionChains(ctx stdctx.Context, origin domain.WorkflowRun) []branchCessionChain {
	roots := c.cededChainRoots(ctx, origin)
	seenLock := map[string]bool{}
	for _, root := range roots {
		seenLock[root.Record.LockID] = true
	}
	for _, root := range c.custodyChainRoots(ctx, origin) {
		if seenLock[root.Record.LockID] {
			// A cession row exists for this lock: it is the stronger evidence
			// and custody never overrides it.
			continue
		}
		seenLock[root.Record.LockID] = true
		roots = append(roots, root)
	}
	sort.SliceStable(roots, func(i, j int) bool { return roots[i].Record.LockID < roots[j].Record.LockID })

	out := make([]branchCessionChain, 0, len(roots))
	for _, root := range roots {
		out = append(out, c.walkBranchCessionChain(ctx, origin, root))
	}
	return out
}

// walkBranchCessionChain follows one root down to whoever holds the branch now,
// and decides whether the deepest link can be folded.
func (c *Coordinator) walkBranchCessionChain(
	ctx stdctx.Context, origin domain.WorkflowRun, root branchCessionLink,
) branchCessionChain {
	chain := branchCessionChain{OriginRunID: origin.ID, LockID: root.Record.LockID, Links: []branchCessionLink{root}}
	visited := map[string]bool{origin.ID: true, root.HolderRunID: true}
	for depth := 1; depth < maxBranchCessionDepth; depth++ {
		last := chain.Links[len(chain.Links)-1]
		next, ok := c.outstandingCessionOfLock(ctx, last.HolderRunID, chain.LockID)
		if !ok {
			break
		}
		if visited[next.HolderRunID] {
			chain.Blocked = branchCessionLegacyUnprovable
			chain.Detail = fmt.Sprintf("branch %s's cession ledger loops back to run %s, so who holds it is not provable",
				chain.LockID, next.HolderRunID)
			return chain
		}
		visited[next.HolderRunID] = true
		chain.Links = append(chain.Links, next)
	}
	if _, ok := c.outstandingCessionOfLock(ctx, chain.Links[len(chain.Links)-1].HolderRunID, chain.LockID); ok {
		chain.Blocked = branchCessionLegacyUnprovable
		chain.Detail = fmt.Sprintf("branch %s has been ceded more than %d times; AO will not walk further",
			chain.LockID, maxBranchCessionDepth)
		return chain
	}
	c.classifyDeepestLink(ctx, &chain)
	return chain
}

// classifyDeepestLink decides, without changing anything, whether the deepest
// link can fold — and if not, which fact is missing. It is the read side of
// foldDeepestBranchCessionLink and deliberately asks the same questions in the
// same order, so what the API reports and what the fold does cannot disagree.
func (c *Coordinator) classifyDeepestLink(ctx stdctx.Context, chain *branchCessionChain) {
	link := chain.Links[len(chain.Links)-1]
	lock, held := c.heldLockByID(ctx, link.HolderRunID, chain.LockID)
	if !held {
		chain.Blocked = branchCessionBranchMovedOn
		chain.Detail = fmt.Sprintf("run %s no longer holds branch lock %s", link.HolderRunID, chain.LockID)
		return
	}
	if !branchCessionLockMatches(lock, link.Record) {
		chain.Blocked = branchCessionBranchMovedOn
		chain.Detail = fmt.Sprintf("branch lock %s no longer matches the transfer recorded for it", chain.LockID)
		return
	}
	previous, found, err := c.store.GetWorkflowRun(ctx, link.PreviousOwnerRunID)
	if err != nil || !found {
		chain.Blocked = branchCessionLegacyUnprovable
		chain.Detail = fmt.Sprintf("AO cannot read run %s, which branch %s would go back to", link.PreviousOwnerRunID, lock.Branch)
		return
	}
	if previous.State.Terminal() {
		chain.Blocked = branchCessionOwnerNotAuthoritative
		chain.Detail = fmt.Sprintf("run %s is %s, so it is not an authority this branch can be returned to",
			previous.ID, previous.State)
		return
	}
	holder, found, err := c.store.GetWorkflowRun(ctx, link.HolderRunID)
	if err != nil || !found {
		chain.Blocked = branchCessionLegacyUnprovable
		chain.Detail = fmt.Sprintf("AO cannot read run %s, which holds branch %s", link.HolderRunID, lock.Branch)
		return
	}
	intent, ok := c.repairIntentForLink(ctx, link)
	if !ok {
		chain.Blocked = branchCessionLegacyUnprovable
		chain.Detail = fmt.Sprintf(
			"no durable repair intent binds run %s to run %s, so AO cannot prove branch %s was handed over legitimately",
			link.PreviousOwnerRunID, link.HolderRunID, lock.Branch)
		return
	}
	if link.Kind == branchCessionKindCustody && !c.custodyLockIsThisRepairEpisode(ctx, lock, holder) {
		chain.Blocked = branchCessionLegacyUnprovable
		chain.Detail = fmt.Sprintf(
			"branch %s was already held when repair run %s was created, so AO cannot prove the repair took it on its origin's behalf",
			lock.Branch, holder.ID)
		return
	}
	if proof := c.proveRepairQuiescent(ctx, previous, intent, holder); !proof.Quiescent {
		chain.Blocked = branchCessionHolderCanWrite
		chain.Detail = proof.Reason
		return
	}
	chain.Blocked = branchCessionReturnable
}

// cededChainRoots is the strong root: cessions this run made and has not been
// given back.
func (c *Coordinator) cededChainRoots(ctx stdctx.Context, origin domain.WorkflowRun) []branchCessionLink {
	records := c.outstandingCessionsFrom(ctx, origin.ID)
	out := make([]branchCessionLink, 0, len(records))
	for _, rec := range records {
		out = append(out, branchCessionLink{
			Kind: branchCessionKindCeded, Record: rec,
			PreviousOwnerRunID: rec.FromRunID, HolderRunID: rec.ToRunID,
		})
	}
	return out
}

// custodyChainRoots is the derived root, and the four facts it insists on are
// the whole of its safety. See this file's header for why it exists at all.
func (c *Coordinator) custodyChainRoots(ctx stdctx.Context, origin domain.WorkflowRun) []branchCessionLink {
	intents := c.repairIntents(ctx, origin.ID)
	if len(intents) == 0 {
		return nil
	}
	// (1) the CURRENT generation only. A superseded repair's custody is
	// reconcileRepairOutcome's business, and acting on it here would be acting
	// on an authority the lifecycle has already replaced.
	intent := intents[len(intents)-1]
	if intent.RepairRunID == "" {
		return nil
	}
	repairRun, found, err := c.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !found || repairRun.State.Terminal() {
		return nil
	}
	// (2) the binding is two-sided: the repair run itself says whose repair it
	// is, and it agrees with the origin's intent. One side alone is a claim;
	// both sides is a relation.
	if !c.repairRunClaimsOrigin(ctx, intent.RepairRunID, origin.ID, intent.Generation) {
		return nil
	}
	// (3) the branch has to be the ORIGIN's own, proven from the origin's own
	// durable branch facts. This is what stops a repair's unrelated lock from
	// being pulled toward a run that never worked that branch.
	identities := c.durableBranchIdentities(ctx, origin.ID)
	if len(identities) == 0 {
		return nil
	}

	// The candidate locks are the ones the repair still holds, plus the ones it
	// has ceded onward and not been given back — because the second kind is
	// exactly the repair-of-repair shape, where the branch the origin is owed
	// is currently two runs away.
	candidates := map[string]branchCessionRecord{}
	if held, herr := c.branchLocks.HeldByRun(ctx, intent.RepairRunID); herr == nil {
		for _, lock := range held {
			candidates[lock.ID] = branchCessionRecord{
				LockID: lock.ID, Branch: lock.Branch, RepoPath: lock.RepoPath,
			}
		}
	}
	for _, rec := range c.outstandingCessionsFrom(ctx, intent.RepairRunID) {
		candidates[rec.LockID] = branchCessionRecord{
			LockID: rec.LockID, Branch: rec.Branch, RepoPath: rec.RepoPath,
		}
	}

	out := make([]branchCessionLink, 0, len(candidates))
	for _, cand := range candidates {
		if !identities[branchIdentityKey(cand.Branch, cand.RepoPath)] {
			continue
		}
		out = append(out, branchCessionLink{
			Kind: branchCessionKindCustody,
			Record: branchCessionRecord{
				LockID: cand.LockID, FromRunID: origin.ID, ToRunID: intent.RepairRunID,
				RepairIntentID: intent.ID, RepairGeneration: intent.Generation,
				Branch: cand.Branch, RepoPath: cand.RepoPath, Kind: branchCessionKindCustody,
			},
			PreviousOwnerRunID: origin.ID, HolderRunID: intent.RepairRunID,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Record.LockID < out[j].Record.LockID })
	return out
}

// custodyLockIsThisRepairEpisode is custody's fourth fact: the lock was taken
// after the repair run existed.
//
// A lock older than the repair run cannot have been acquired by it on anybody's
// behalf, and treating one as custody would let an unrelated inherited lock be
// pulled up a chain it was never part of.
func (c *Coordinator) custodyLockIsThisRepairEpisode(ctx stdctx.Context, lock domain.BranchLock, repairRun domain.WorkflowRun) bool {
	_ = ctx
	if lock.AcquiredAt.IsZero() || repairRun.CreatedAt.IsZero() {
		return false
	}
	return !lock.AcquiredAt.Before(repairRun.CreatedAt)
}

// repairRunClaimsOrigin reads the repair run's own origin marker and requires
// it to name this origin and this generation — and to be the ONLY thing it
// names.
//
// Unanimity, not existence, because a custody link is a claim on a branch and
// the question is whose repair this run is, not whether somebody once said it
// was ours. A run carrying a marker that names a different origin, or one whose
// payload cannot be read, has provenance AO cannot resolve: it is refused, and
// the chain reports legacy_unprovable_branch_cession rather than picking the
// marker that happens to agree.
func (c *Coordinator) repairRunClaimsOrigin(ctx stdctx.Context, repairRunID, originID string, generation int) bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, repairRunID)
	if err != nil {
		return false
	}
	claimed := false
	for _, cp := range cps {
		if cp.DurablePhase != repairRunOriginPhase {
			continue
		}
		var body struct {
			OriginRunID string `json:"originRunId"`
			Generation  int    `json:"generation"`
		}
		if json.Unmarshal([]byte(cp.RetryState), &body) != nil || body.OriginRunID == "" {
			return false
		}
		if body.OriginRunID != originID || body.Generation != generation {
			return false
		}
		claimed = true
	}
	return claimed
}

// repairIntentForLink finds the previous owner's repair intent that this link
// rests on.
//
// For a ceded link it is the intent the cession row names; for a custody link
// it is the generation the custody root was built from. Either way it must
// still exist on the previous owner's ledger and must name this holder —
// proveRepairQuiescent then refuses it if it is no longer the current
// generation.
func (c *Coordinator) repairIntentForLink(ctx stdctx.Context, link branchCessionLink) (domain.RepairIntent, bool) {
	for _, intent := range c.repairIntents(ctx, link.PreviousOwnerRunID) {
		if intent.RepairRunID != link.HolderRunID {
			continue
		}
		if link.Record.RepairIntentID != "" && intent.ID != link.Record.RepairIntentID {
			continue
		}
		if link.Record.RepairGeneration != 0 && intent.Generation != link.Record.RepairGeneration {
			continue
		}
		return intent, true
	}
	return domain.RepairIntent{}, false
}

// ---------------------------------------------------------------------------
// The ledger reads the walk is built from.
// ---------------------------------------------------------------------------

// outstandingCessionsFrom folds one run's cession ledger into the transfers
// that are still out: ceded, and not recorded back.
//
// Unlike cededBranchLocks it is not scoped to a single repair generation,
// because a chain walk is about a BRANCH's whereabouts, not about one
// generation's bookkeeping.
func (c *Coordinator) outstandingCessionsFrom(ctx stdctx.Context, runID string) []branchCessionRecord {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return nil
	}
	// Matched by IDENTITY, not by order. Two rows written inside one clock tick
	// sort by id, and "the return happened after the cession" is then a
	// statement about two strings rather than about two events — which is a
	// silent way to resurrect a cession that was already given back. A return
	// closes the cession for the same lock, the same holder and the same
	// generation, whichever order the rows come back in.
	ceded := map[string]branchCessionRecord{}
	returned := map[string]bool{}
	for _, cp := range cps {
		switch cp.DurablePhase {
		case branchLockCededPhase, branchLockReturnedPhase:
		default:
			continue
		}
		var rec branchCessionRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) != nil || rec.LockID == "" {
			continue
		}
		key := branchCessionKey(rec)
		if cp.DurablePhase == branchLockCededPhase {
			ceded[key] = rec
		} else {
			returned[key] = true
		}
	}
	out := make([]branchCessionRecord, 0, len(ceded))
	for key, rec := range ceded {
		if returned[key] {
			continue
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LockID < out[j].LockID })
	return out
}

// outstandingCessionOfLock is the walk's single step: did this holder cede this
// exact lock onward, and not get it back?
func (c *Coordinator) outstandingCessionOfLock(ctx stdctx.Context, holderRunID, lockID string) (branchCessionLink, bool) {
	for _, rec := range c.outstandingCessionsFrom(ctx, holderRunID) {
		if rec.LockID != lockID {
			continue
		}
		return branchCessionLink{
			Kind: branchCessionKindCeded, Record: rec,
			PreviousOwnerRunID: rec.FromRunID, HolderRunID: rec.ToRunID,
		}, true
	}
	return branchCessionLink{}, false
}

// heldLockByID reads one lock from the run that is supposed to hold it.
//
// It deliberately asks "what does this run hold" rather than "where is this
// lock": the answer AO needs is about ownership at this instant, and a lock the
// named run does not hold is not this link's lock however the row reads.
func (c *Coordinator) heldLockByID(ctx stdctx.Context, runID, lockID string) (domain.BranchLock, bool) {
	if c.branchLocks == nil {
		return domain.BranchLock{}, false
	}
	held, err := c.branchLocks.HeldByRun(ctx, runID)
	if err != nil {
		return domain.BranchLock{}, false
	}
	for _, lock := range held {
		if lock.ID == lockID {
			return lock, true
		}
	}
	return domain.BranchLock{}, false
}

// branchCessionLockMatches is the identity check the CAS cannot express.
//
// The compare-and-set itself is conditioned on (lock id, still held, current
// owner run), which is already ABA-proof in this schema: lock ids are minted
// per acquisition and a released row is never returned to `held`, so a lock id
// that is still held is still the same episode of the same lock. What this adds
// is that the ROW still describes the transfer the ledger recorded — same
// branch, same repository, same ownership kind — so a fold can never move a
// lock the record was not about.
func branchCessionLockMatches(lock domain.BranchLock, rec branchCessionRecord) bool {
	if lock.ID != rec.LockID || lock.State != domain.BranchLockHeld {
		return false
	}
	if rec.Branch != "" && lock.Branch != rec.Branch {
		return false
	}
	if rec.RepoPath != "" && lock.RepoPath != rec.RepoPath {
		return false
	}
	return lock.OwnershipKind.WithDefault() == domain.BranchLockOwnershipDirectBranch
}

// durableBranchIdentities is the set of (branch, repository) pairs this run's
// own ledger shows it working.
//
// Checkpoints carry branch and worktree path as first-class columns, written by
// the ordinary dispatch path, so this is the run's own durable account of which
// branch is its. It is evidence, not inference: a run with no such row gets an
// empty set and no custody link is ever built for it.
func (c *Coordinator) durableBranchIdentities(ctx stdctx.Context, runID string) map[string]bool {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, cp := range cps {
		if strings.TrimSpace(cp.Branch) != "" && strings.TrimSpace(cp.WorktreePath) != "" {
			out[branchIdentityKey(cp.Branch, cp.WorktreePath)] = true
		}
		// A cession this run made names the branch and repository it held in
		// the payload rather than in the columns, and is the same evidence.
		if cp.DurablePhase != branchLockCededPhase {
			continue
		}
		var rec branchCessionRecord
		if json.Unmarshal([]byte(cp.RetryState), &rec) == nil && rec.FromRunID == runID && rec.Branch != "" && rec.RepoPath != "" {
			out[branchIdentityKey(rec.Branch, rec.RepoPath)] = true
		}
	}
	return out
}

func branchIdentityKey(branch, repoPath string) string { return branch + "\x00" + repoPath }

// branchCessionKey is one transfer's identity: which lock, to whom, under which
// repair generation. It is what pairs a return with the cession it closes.
func branchCessionKey(rec branchCessionRecord) string {
	return fmt.Sprintf("%s|%s|%d", rec.LockID, rec.ToRunID, rec.RepairGeneration)
}

// ---------------------------------------------------------------------------
// Bookkeeping the crash window skipped.
// ---------------------------------------------------------------------------

// completeBranchCessionBookkeeping closes cession rows whose branch has already
// left the repair, without transferring anything.
//
// Two situations produce one: a crash between the CAS and the record, and a
// lock released out from under a cession (a repair run cancelled, retention
// reclaiming it). Both leave the ledger claiming an outstanding cession over a
// branch the repair no longer has — which clause (6) of the quiescence proof
// then refuses forever, because it cannot prove who owns it now.
//
// It writes only when the lock table has already settled the question, and it
// writes the same derived-id row the fold would have written, so completing
// bookkeeping twice is one row and never a second transfer.
func (c *Coordinator) completeBranchCessionBookkeeping(ctx stdctx.Context, run domain.WorkflowRun) {
	if c.branchLocks == nil {
		return
	}
	for _, rec := range c.outstandingCessionsFrom(ctx, run.ID) {
		if _, stillTheirs := c.heldLockByID(ctx, rec.ToRunID, rec.LockID); stillTheirs {
			continue
		}
		_, back := c.heldLockByID(ctx, rec.FromRunID, rec.LockID)
		if !back {
			// The repair does not have it and neither do we. That is only a
			// closed cession if the branch has left the chain altogether — if
			// the repair CEDED IT ONWARD, this row is the still-live root of a
			// chain that is two hops deep, and closing it would erase the only
			// evidence of the hop the fold has to walk back through.
			if _, onward := c.outstandingCessionOfLock(ctx, rec.ToRunID, rec.LockID); onward {
				continue
			}
		}
		detail := fmt.Sprintf("branch %s is no longer held by repair run %s; this cession is closed on the ledger",
			rec.Branch, rec.ToRunID)
		if back {
			detail = fmt.Sprintf("branch %s is back with run %s; completing the record of a transfer that already happened",
				rec.Branch, rec.FromRunID)
		}
		closed := rec
		closed.At = c.clock()
		payload, err := json.Marshal(closed)
		if err != nil {
			continue
		}
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             branchCessionFoldID(rec.LockID, rec.ToRunID, rec.RepairGeneration),
			WorkflowRunID:  run.ID,
			ProjectID:      run.ProjectID,
			DurablePhase:   branchLockReturnedPhase,
			NextAction:     detail,
			PayloadVersion: "v1",
			RetryState:     string(payload),
			Branch:         rec.Branch,
			WorktreePath:   rec.RepoPath,
			CreatedAt:      c.clock(),
		}); err != nil && c.log != nil {
			c.log.Debug("workflow: could not complete branch cession bookkeeping",
				"run", run.ID, "lock", rec.LockID, "err", err)
		}
	}
}

// ---------------------------------------------------------------------------
// The read projection.
// ---------------------------------------------------------------------------

// branchCessionChainFor is what the API reports for one run: the deepest chain
// rooted at it, or nothing when no branch of its is out.
//
// One chain, not a list, because the question a person is asking is "where is
// my branch and why is it not back" — and a run with several repositories out
// at once is a shape that does not occur today (a repair is created for one
// stop, on one project). The deepest is reported so the answer is never the
// hop that is already settled.
func (c *Coordinator) branchCessionChainFor(ctx stdctx.Context, run domain.WorkflowRun) *BranchCessionChain {
	if c.branchLocks == nil {
		return nil
	}
	var best *BranchCessionChain
	for _, chain := range c.branchCessionChains(ctx, run) {
		if len(chain.Links) == 0 {
			continue
		}
		deepest := chain.Links[len(chain.Links)-1]
		view := &BranchCessionChain{
			OriginRunID:        chain.OriginRunID,
			CurrentHolderRunID: deepest.HolderRunID,
			Depth:              len(chain.Links),
			Returnable:         chain.Blocked == branchCessionReturnable,
			BlockedReason:      chain.Blocked,
			Detail:             chain.Detail,
			LockID:             chain.LockID,
			Branch:             deepest.Record.Branch,
			RepoPath:           deepest.Record.RepoPath,
			Kind:               deepest.Kind,
		}
		if betterCessionChainView(best, view) {
			best = view
		}
	}
	return best
}

// betterCessionChainView ranks two chains for the single answer the API gives.
//
// A branch that is actually out ranks above one whose row is merely stale: a
// run can carry a settled cession from an old generation (its lock long
// released) alongside a live one, and reporting the settled row would tell a
// person their branch is fine when it is two repairs away. Live first, then the
// deeper chain, which is the one whose deepest hop is not yet folded.
func betterCessionChainView(best, candidate *BranchCessionChain) bool {
	if best == nil {
		return true
	}
	bestLive := best.BlockedReason != branchCessionBranchMovedOn
	candidateLive := candidate.BlockedReason != branchCessionBranchMovedOn
	if bestLive != candidateLive {
		return candidateLive
	}
	return candidate.Depth > best.Depth
}
