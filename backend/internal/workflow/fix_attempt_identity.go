package workflow

import (
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_attempt_identity.go — which row belongs to which fix cycle.
//
// The fix lane used to answer that by counting: a step with N attempt rows was
// a step whose cycle N had been dispatched. That is not an identity, and P3-D
// §7 is the bill for it. A count cannot say which row a cycle owns, so:
//
//   - a review cycle cancelled or superseded after minting its attempt left a
//     row with no backing lifecycle, NULL outcome and NULL error class, and
//     nothing could prove what it was or close it;
//   - the reaper, asked to reconcile that row, had only "the newest one" and a
//     timestamp to go on — neither of which is authority;
//   - and any row added to the step for another reason shifted every later
//     cycle's answer by one.
//
// fixAttemptID (fix_dispatch.go) replaces the count with a derivation. This file
// is what reads it back: the predicates the dispatch path asks before minting,
// and the classification the reaper asks before acting.
//
// The legacy half is deliberately unglamorous. Rows written before the
// derivation exists cannot be associated with a cycle by any evidence AO holds
// — a timestamp is a coincidence, not a proof — so they are never claimed to
// be. They are named legacy, they suppress a duplicate mint (the conservative
// direction: no second dispatch), and they are never reported as a proven
// active attempt for a cycle. P3-D §8: fail closed, do not invent association.

// fixAttemptIDPrefix marks a row whose id derives from a cycle identity. Its
// absence is exactly what makes a row legacy: the id is the evidence, so a row
// that cannot have one has none.
const fixAttemptIDPrefix = "wfa-fix-"

// fixCycleKeyPrefix marks the RECOVERABLE half of the identity, carried in the
// attempt row's `model` column.
//
// A hash proves a row belongs to a cycle you already hold; it cannot tell you
// WHICH cycle a row you found belongs to, and that second question is the one
// the reaper and the recovery projection actually ask. So the identity is
// written down as well as hashed, and the row answers for itself without
// joining to anything — which matters most in the exact window this change is
// about, where the attempt exists and the checkpoint that would have named it
// does not (the row is created first, by design).
//
// `model` is where it goes because that is already this column's job for an
// attempt with no provider model to record: verify attempts carry their target
// key there (verify.go). A fix attempt's harness is the worker's real harness
// and its model is otherwise empty, so nothing is displaced.
const fixCycleKeyPrefix = "fix:"

// fixCycleKey is the durable, parseable name of one fix cycle: the review run
// that authorized it, and its cycle number.
func fixCycleKey(reviewRunID string, cycleNumber int) string {
	return fixCycleKeyPrefix + reviewRunID + ":c" + strconv.Itoa(cycleNumber)
}

// parseFixCycleKey reads a cycle key back. It answers false for anything that
// is not one — an empty column, a provider model, a shape it does not
// recognise — because a key AO cannot parse is not a key it may act on.
func parseFixCycleKey(key string) (reviewRunID string, cycleNumber int, ok bool) {
	if !strings.HasPrefix(key, fixCycleKeyPrefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(key, fixCycleKeyPrefix)
	i := strings.LastIndex(rest, ":c")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+2:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return rest[:i], n, true
}

// derivedFixAttempt reports whether this row carries a derivable identity at
// all, without saying which cycle it belongs to.
func derivedFixAttempt(a domain.WorkflowAttempt) bool {
	return strings.HasPrefix(a.ID, fixAttemptIDPrefix)
}

// FixAttemptCycle is the cycle one attempt row names, recovered from the row
// itself. Ok is false for a legacy row, which names none.
func FixAttemptCycle(a domain.WorkflowAttempt) (reviewRunID string, cycleNumber int, ok bool) {
	if !derivedFixAttempt(a) {
		return "", 0, false
	}
	return parseFixCycleKey(a.Model)
}

// findAttemptByID is identity lookup, and the only lookup the fix lane may use
// to answer "does this cycle have its row". Position ("the latest one") is what
// this file exists to abolish.
func findAttemptByID(attempts []domain.WorkflowAttempt, id string) (domain.WorkflowAttempt, bool) {
	for _, a := range attempts {
		if a.ID == id {
			return a, true
		}
	}
	return domain.WorkflowAttempt{}, false
}

// legacyFixCycleAttempt is the upgrade path, and nothing more.
//
// It answers only for a step that has NO derived row for the cycle in hand and
// that the old count-based predicate would have called already-dispatched. In
// that shape a pre-identity binary minted a row for this cycle and this binary
// cannot prove which one — so the newest underived row is bound as the best
// available answer rather than a second one being minted over it. Minting is
// the dangerous direction (a duplicate fix dispatch); binding is not.
//
// It is never used to CLAIM the row belongs to the cycle. Nothing derives from
// it, the reaper classifies such rows as legacy on their id alone, and a step
// with no underived rows gets no answer at all.
func legacyFixCycleAttempt(
	attempts []domain.WorkflowAttempt, fixStepID, reviewRunID string, cycleNumber int,
) (domain.WorkflowAttempt, bool) {
	if _, derived := findAttemptByID(attempts, fixAttemptID(fixStepID, reviewRunID, cycleNumber)); derived {
		return domain.WorkflowAttempt{}, false
	}
	if int64(len(attempts)) < int64(cycleNumber) {
		return domain.WorkflowAttempt{}, false
	}
	for i := len(attempts) - 1; i >= 0; i-- {
		if !derivedFixAttempt(attempts[i]) {
			return attempts[i], true
		}
	}
	return domain.WorkflowAttempt{}, false
}

// fixCycleHasAttempt is the dispatch guard: has this cycle already been given a
// row, by this binary or by an older one.
//
// The legacy disjunct keeps an in-flight cycle written before the derivation
// existed from being dispatched a second time on upgrade. It is strictly more
// conservative than either rule alone — every cycle the old predicate called
// dispatched is still called dispatched — which is the only safe direction for
// a guard whose false answer sends a second prompt to a live worker.
func fixCycleHasAttempt(
	attempts []domain.WorkflowAttempt, fixStepID, reviewRunID string, cycleNumber int,
) bool {
	if _, ok := findAttemptByID(attempts, fixAttemptID(fixStepID, reviewRunID, cycleNumber)); ok {
		return true
	}
	_, legacy := legacyFixCycleAttempt(attempts, fixStepID, reviewRunID, cycleNumber)
	return legacy
}

// FixAttemptAuthority is what the reaper and the recovery projection are
// allowed to conclude about one fix attempt row.
//
// It is a classification of EVIDENCE, not a lifecycle state: the same row reads
// differently once its cycle is superseded, and nothing here is derived from
// how long ago the row was written.
type FixAttemptAuthority string

const (
	// FixAttemptActive is a row whose identity derives from the cycle that is
	// currently authorized, and which has not concluded. This is the only value
	// that means "work may be in flight here".
	FixAttemptActive FixAttemptAuthority = "active"
	// FixAttemptConcluded is a row with an outcome. History, and inert.
	FixAttemptConcluded FixAttemptAuthority = "concluded"
	// FixAttemptSuperseded is a derived row whose cycle is no longer the
	// authorized one — the review that minted it was cancelled or replaced. It
	// has no outcome and must be terminalized; what it must never be is read as
	// an active attempt (P3-D §10/§28).
	FixAttemptSuperseded FixAttemptAuthority = "superseded"
	// FixAttemptLegacyUnproven is an unconcluded row with no derivable
	// identity. AO cannot prove which cycle it belongs to and refuses to guess.
	// Never active authority; never silently closed either.
	FixAttemptLegacyUnproven FixAttemptAuthority = "legacy_unproven"
)

// FixAuthority is the fix cycle that holds authority on one step right now:
// the review run whose findings AO would act on, and the cycle number under it.
//
// A zero ReviewRunID means no cycle is authorized at all — the review that
// minted whatever rows exist has been cancelled or replaced and nothing has
// taken its place. That is not "unknown": it is the positive answer that every
// unconcluded row on this step belongs to a cycle nobody authorizes, which is
// precisely the state P3-D §28 is about.
type FixAuthority struct {
	ReviewRunID string
	CycleNumber int
	// Known distinguishes "AO read the authority and there is none" from "AO
	// could not read it". Nothing may be terminalized on the second.
	Known bool
}

// ClassifyFixAttempt is the single reading of one fix attempt row against the
// cycle that currently holds authority on its step.
//
// Every branch answers from evidence the row itself carries. An authority AO
// could not read (Known false) can only ever produce `active` for an
// unconcluded row — the conservative answer, and the one that keeps a live fix
// worker's attempt from being closed under it because a review lookup failed.
func ClassifyFixAttempt(a domain.WorkflowAttempt, authority FixAuthority) FixAttemptAuthority {
	if a.Outcome != "" {
		return FixAttemptConcluded
	}
	reviewRunID, cycleNumber, ok := FixAttemptCycle(a)
	if !ok {
		return FixAttemptLegacyUnproven
	}
	if !authority.Known {
		return FixAttemptActive
	}
	if authority.ReviewRunID == reviewRunID && authority.CycleNumber == cycleNumber {
		return FixAttemptActive
	}
	return FixAttemptSuperseded
}
