package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// project_memory_authority.go — the P2-D authority axis: can AO still PROVE
// that what licensed this fact is in force?
//
// P2-A's ProjectMemoryState answers a different question, and the two are
// deliberately not merged. State is about EVIDENCE: do this fact's source
// files still look the way they did when it was derived. Authority is about
// LICENCE: is the thing that made this fact the project's knowledge — an
// integration, a verification, a repository identity — still something AO
// holds a durable record of.
//
// They move independently. A decision whose files nobody has touched
// (state = valid) loses its authority the instant the integration that
// promoted it turns out never to have happened. A module summary whose files
// changed (state = stale) keeps a perfectly good promotion authority that an
// operator still needs to see. One column with one vocabulary would give
// "why is this not being served" a single slot for two different answers.
//
// The safety property is that they COMPOSE by AND, never by OR:
//
//	servable  ==  state == valid  &&  authority == authoritative
//
// So every authority value other than the default withholds the fact, and a
// future value added to this vocabulary is withheld before anyone writes the
// code that decides what to do about it. Fail closed is the default case, not
// a branch someone has to remember to write.

// MemoryAuthority says whether a fact's licence is still provable.
type MemoryAuthority string

// Memory authorities.
const (
	// AuthorityAuthoritative means every proof this fact's provenance kind
	// requires was checked and held. It is the only value that is served.
	AuthorityAuthoritative MemoryAuthority = "authoritative"
	// AuthorityUnprovable means a proof the fact needs is missing, broken, or
	// contradicted: the promotion cannot be shown to have happened, the
	// repository identity changed under it, or the generation that produced it
	// has been superseded. The fact is kept — it is the cheapest starting
	// point for re-deriving the current one — and is never served as current.
	//
	// It is NOT MemoryStateInvalidated. Invalidation is a claim about the
	// evidence ("the file this described is gone"); this is a claim about the
	// licence ("AO can no longer show this was ever the project's"). Only the
	// second can be repaired by finding the missing durable row, which is what
	// `ao memory validate` does.
	AuthorityUnprovable MemoryAuthority = "unprovable"
	// AuthorityLegacyUnprovable means the fact was written before P2-D and
	// carries none of the provenance this model requires.
	//
	// It is a separate value from AuthorityUnprovable because the two deserve
	// different operator responses and different retention. An unprovable fact
	// is a fact whose proof BROKE and that is worth investigating. A legacy
	// fact never had one, which is not an incident — it is the expected state
	// of every row an upgraded install already had, and the honest thing to do
	// with it is to withhold it and offer a bounded rebuild, never to fabricate
	// the provenance it lacks (P2-D §21).
	AuthorityLegacyUnprovable MemoryAuthority = "legacy_unprovable"
)

// Provable reports whether a fact carrying this authority may be served as
// current. Exactly one value can, which is the fail-closed rule.
func (a MemoryAuthority) Provable() bool { return a == AuthorityAuthoritative }

// Valid reports whether the authority is one this build writes.
func (a MemoryAuthority) Valid() bool {
	switch a {
	case AuthorityAuthoritative, AuthorityUnprovable, AuthorityLegacyUnprovable:
		return true
	default:
		return false
	}
}

// Memory authority reason classes. Every non-authoritative row carries one of
// these as the first token of AuthorityReason, so an operator surface can
// group by cause without parsing prose, and the prose after it can stay free
// to say something specific.
//
// The vocabulary is closed on purpose: a reason nobody can enumerate is a
// reason nobody can build an alert on.
const (
	// ReasonProvenanceMissing — the fact names no durable row that could
	// license it. A promotion with no mutation-provenance record, a task
	// outcome with no task.
	ReasonProvenanceMissing = "memory_provenance_missing"
	// ReasonSourceDrift — the fact's sources moved in a way that broke the
	// provenance rather than merely the content (a source that escaped the
	// repository, a digest that cannot be recomputed at all).
	ReasonSourceDrift = "memory_source_drift"
	// ReasonGenerationStale — a newer generation has superseded the one that
	// produced this fact, and the fact was not re-confirmed by it.
	ReasonGenerationStale = "memory_generation_stale"
	// ReasonRepoIdentityChanged — the repository at this path is not the
	// repository this fact was derived from. The single most dangerous case
	// this file exists for: without it, a different project checked out at an
	// old path inherits the old project's knowledge.
	ReasonRepoIdentityChanged = "memory_repo_identity_changed"
	// ReasonPromotionUnprovable — the fact claims canonical authority and AO
	// cannot show the work reached the repository's own history.
	ReasonPromotionUnprovable = "memory_promotion_unprovable"
	// ReasonLegacyNoProvenance — written before P2-D. See
	// AuthorityLegacyUnprovable.
	ReasonLegacyNoProvenance = "memory_legacy_no_provenance"
	// ReasonSupersededSourceChanged — P2-D §11. A decision that superseded an
	// earlier one has itself become unprovable. The predecessor does NOT come
	// back; the project simply has no current answer on the subject until a
	// revalidation produces one.
	ReasonSupersededSourceChanged = "memory_superseded_source_changed"
)

// MemoryAuthorityReason renders a reason class plus its detail into the single
// string the row stores. The class is first and is followed by ": ", so
// splitting on the first colon always recovers it.
func MemoryAuthorityReason(class, detail string) string {
	class = strings.TrimSpace(class)
	detail = strings.TrimSpace(detail)
	switch {
	case class == "":
		return detail
	case detail == "":
		return class
	default:
		return class + ": " + detail
	}
}

// MemoryAuthorityReasonClass recovers the class from a stored reason. An empty
// or classless reason yields "", which reads as "no class was recorded" rather
// than being mapped onto the nearest known one.
func MemoryAuthorityReasonClass(reason string) string {
	reason = strings.TrimSpace(reason)
	if idx := strings.IndexByte(reason, ':'); idx > 0 {
		return strings.TrimSpace(reason[:idx])
	}
	if strings.HasPrefix(reason, "memory_") {
		return reason
	}
	return ""
}

// MemoryProvenanceKind says how a fact was derived, and therefore WHICH proof
// applies to it. Validation reads this rather than inferring from the item
// type, because two facts of the same type can have arrived by different
// routes: a decision the indexer lifted out of a document and a decision a
// task recorded are both decisions, and only the second has a task to prove.
type MemoryProvenanceKind string

// Memory provenance kinds.
const (
	// ProvenanceRepoDerivation — an indexing pass read files in the repository
	// and derived this. Its proof is source paths + source digest + source
	// commit + repository identity.
	ProvenanceRepoDerivation MemoryProvenanceKind = "repo_derivation"
	// ProvenanceTaskOutcome — a finished task's bounded facts. Its proof is
	// the task, its verified head, and (for canonical) a promotion authority.
	ProvenanceTaskOutcome MemoryProvenanceKind = "task_outcome"
	// ProvenanceWorkflowKnowledge — a decision or a risk lifted out of a
	// durable workflow row (an amendment, a review thread, a QA gate). Its
	// proof is that row plus the task that produced it.
	ProvenanceWorkflowKnowledge MemoryProvenanceKind = "workflow_knowledge"
	// ProvenanceLegacy — written before P2-D and carrying none of the above.
	ProvenanceLegacy MemoryProvenanceKind = "legacy"
)

// Valid reports whether the kind is one this build writes.
func (k MemoryProvenanceKind) Valid() bool {
	switch k {
	case ProvenanceRepoDerivation, ProvenanceTaskOutcome, ProvenanceWorkflowKnowledge, ProvenanceLegacy:
		return true
	default:
		return false
	}
}

// RepoIdentity is a repository's durable identity, independent of where it is
// checked out (P2-D §9).
//
// ProjectMemoryRepoID hashes the absolute path, which answers "which memory
// row do I address" and cannot answer the two questions P2-D needs:
//
//	same repository, moved path   -> memory should follow it
//	different repository, same path -> memory must NOT be inherited
//
// A path-derived id gets the first one wrong safely (the moved checkout looks
// like a repository AO has never seen, and re-indexes) and the second one
// wrong DANGEROUSLY: the new project silently inherits the old one's
// conventions, decisions and risks and is never told.
//
// So identity is derived from what git itself considers durable, in
// descending order of authority:
//
//  1. The first remote URL, normalized. Two checkouts of one upstream are one
//     repository however they are laid out, and this is the signal an operator
//     would use themselves.
//  2. Failing that, the ROOT COMMIT of the current history. A repository with
//     no remote still has an immutable first commit, and two unrelated
//     projects effectively never share one.
//  3. Failing both, the empty string — which is "AO cannot identify this
//     repository", and never matches anything, including another empty string.
//
// The empty string is load-bearing. RepoIdentityMatches treats an unknown
// identity as "cannot prove same" rather than "same by default", which is what
// makes a repository AO cannot identify fail closed instead of inheriting.
type RepoIdentity string

// String renders the identity.
func (r RepoIdentity) String() string { return string(r) }

// Known reports whether an identity was actually derived.
func (r RepoIdentity) Known() bool { return strings.TrimSpace(string(r)) != "" }

// NewRepoIdentity builds an identity from the durable signals a caller could
// read. Both arguments may be empty; the result is empty exactly when neither
// could be read, which is the honest "unidentified" answer.
//
// The remote wins when present, and the kind is part of the hash, so a
// repository that gains a remote later produces a DIFFERENT identity than it
// did from its root commit alone. That is deliberate and it is the safe
// direction: it reads as "this is a repository AO has not indexed", which
// costs one re-index, rather than as a silent match.
func NewRepoIdentity(remoteURL, rootCommit string) RepoIdentity {
	if u := NormalizeRemoteURL(remoteURL); u != "" {
		return repoIdentityOf("remote", u)
	}
	if c := strings.TrimSpace(rootCommit); c != "" {
		return repoIdentityOf("root", strings.ToLower(c))
	}
	return ""
}

func repoIdentityOf(kind, value string) RepoIdentity {
	sum := sha256.Sum256([]byte(kind + "\x00" + value))
	return RepoIdentity(fmt.Sprintf("%s_%s", kind, hex.EncodeToString(sum[:])[:24]))
}

// RepoIdentityMatches reports whether two identities provably name the same
// repository.
//
// An unknown identity on either side is NOT a match. That is the whole point:
// "AO could not read the identity" and "the identities agree" are different
// facts, and only the second may license serving a fact derived under the
// other one.
func RepoIdentityMatches(a, b RepoIdentity) bool {
	return a.Known() && b.Known() && a == b
}

// RepoIdentityCompatible reports whether a fact recorded under `recorded` may
// still be treated as being about the repository now observed as `observed`.
//
// It is deliberately weaker than RepoIdentityMatches in exactly one case, and
// that case is worth stating plainly because it is the one place this model
// accepts something it cannot prove:
//
//	recorded known,   observed known, equal    -> compatible
//	recorded known,   observed known, differ   -> NOT compatible  (the dangerous case)
//	recorded known,   observed unknown         -> NOT compatible
//	recorded unknown, observed known           -> NOT compatible
//	recorded unknown, observed unknown         -> COMPATIBLE
//
// The last row is the concession. A project that is not a git checkout at all
// — a scratch directory, a plain folder AO was pointed at — has no durable
// identity, has never had one, and never will. Refusing it would not make
// anything safer: it would make canonical memory impossible for every non-git
// project, permanently, in exchange for detecting a substitution that AO has
// no signal for either way. When neither side is identifiable, the only
// evidence available is the path, which is precisely the identity P2-A already
// used and which nothing here makes worse.
//
// Every row that CAN indicate a substitution — one side identifiable, or two
// identifiable and different — refuses. So the dangerous case (a different
// repository checked out where an old one was) is caught whenever it is
// detectable at all, which is the strongest true statement available.
func RepoIdentityCompatible(recorded, observed RepoIdentity) bool {
	if !recorded.Known() && !observed.Known() {
		return true
	}
	return RepoIdentityMatches(recorded, observed)
}

// NormalizeRemoteURL reduces the forms of one remote to a single string, so
// ssh, https and scp-style spellings of one repository are one identity.
//
// It is deliberately conservative: it strips what is provably presentation
// (scheme, credentials, port, a trailing ".git", a trailing slash, case in the
// host) and touches nothing else. A transformation that guessed — stripping a
// path prefix, mapping one host to another — would merge two repositories that
// are not the same, and merging is the failure this whole file exists to
// prevent.
func NormalizeRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// scp-style: git@host:owner/repo.git
	if !strings.Contains(s, "://") {
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		s = strings.Replace(s, ":", "/", 1)
	} else {
		s = s[strings.Index(s, "://")+3:]
		if at := strings.LastIndex(s, "@"); at >= 0 {
			s = s[at+1:]
		}
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	host, path, found := strings.Cut(s, "/")
	if !found {
		return strings.ToLower(s)
	}
	if h, _, hasPort := strings.Cut(host, ":"); hasPort {
		host = h
	}
	return strings.ToLower(host) + "/" + path
}

// MemoryPromotionProof is what a promotion path has PROVEN about one task's
// work, handed to the memory subsystem instead of a bare commit string.
//
// P2-C promoted on two inferences. Direct-branch work was canonical because
// the project's execution MODE said direct branch; worktree work was canonical
// because a caller passed a non-empty SHA. Neither is a durable record that
// the work is in the repository's own history, and both are things a
// misconfiguration or a stale callback can make true without the work being
// there.
//
// So the promotion path now hands over a proof, and the memory subsystem's job
// is to record it, not to second-guess it. The split matters: the promotion
// path is the only place holding the workflow rows the proof is made of, and
// the memory package is the only place that knows what a canonical row is. A
// proof that says Provable=false still travels — it carries the REASON, which
// is what turns "this fact is missing" into "this fact is withheld because AO
// could not show the worktree was ever integrated".
type MemoryPromotionProof struct {
	// Provable reports whether the work is durably part of the repository.
	// False is a complete, useful answer, not an error.
	Provable bool
	// ReasonClass and Detail explain a refusal. ReasonClass is one of the
	// memory_* constants above, so an operator surface can group refusals
	// without parsing prose.
	ReasonClass string
	Detail      string

	// MutationProvenanceID is the workflow_mutation_provenance row this proof
	// rests on -- the `integrated` boundary for worktree work, the `verified`
	// boundary for direct-branch work. Empty means no durable row licensed
	// this, which is by itself enough to make Provable false.
	MutationProvenanceID string
	// VerifiedCommit is what verification passed on; IntegratedCommit is the
	// commit the work became part of. For direct-branch work they are the same
	// commit, and recording it twice is deliberate: it says "there was no
	// separate integration", which is different from "the integration was not
	// recorded".
	VerifiedCommit   string
	IntegratedCommit string
	// RepoIdentity is the repository the proof was observed against.
	RepoIdentity RepoIdentity
	// Placement and Method say which proof was applied, so a later reader can
	// check the reasoning rather than only its conclusion.
	Placement WorkflowMutationPlacement
	Method    WorkflowIntegrationMethod
}

// Authority renders the proof as the licence a promoted fact should carry.
func (p MemoryPromotionProof) Authority() MemoryAuthority {
	if p.Provable {
		return AuthorityAuthoritative
	}
	return AuthorityUnprovable
}

// AuthorityReason renders the proof as the stored reason string. It is empty
// for a provable promotion, matching the invariant that an authoritative row
// carries no reason.
func (p MemoryPromotionProof) AuthorityReason() string {
	if p.Provable {
		return ""
	}
	class := p.ReasonClass
	if class == "" {
		class = ReasonPromotionUnprovable
	}
	return MemoryAuthorityReason(class, p.Detail)
}

// UnprovablePromotion builds a refusal carrying its reason.
func UnprovablePromotion(class, detail string) MemoryPromotionProof {
	return MemoryPromotionProof{Provable: false, ReasonClass: class, Detail: detail}
}
