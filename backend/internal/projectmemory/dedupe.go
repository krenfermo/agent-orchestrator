package projectmemory

import (
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// dedupe.go — not sending the same thing twice in two formats (P2-B §6).
//
// A dispatch assembled with memory carries three kinds of context: what AO
// already sent before P2-B (legacy documents, a pre-fetched issue body), the
// memory pack, and the task text. The first two overlap: AGENTS.md reaches the
// planner as a legacy document AND as an instruction/convention fact. Sending
// both is worse than sending either — it spends budget twice and invites the
// agent to reconcile two renderings of one file.
//
// The rule this file implements, and the reason it is conservative:
//
//   - **A legacy source is dropped only when memory demonstrably covers it.**
//     Coverage means a memory item names that exact path among its source
//     paths, the item is currently valid, and its digest matches the legacy
//     document AO is holding. All three, per document. Not "the mode is
//     preferred", not "there is a fact about that directory".
//   - **The digest check is what makes it safe.** A memory item derived at an
//     older commit describes a file that has since changed; its digest will not
//     match the document AO just read, and the document survives. So dedupe
//     cannot serve stale content in place of current content, which is the one
//     failure that would make this optimisation dangerous rather than merely
//     ineffective.
//   - **The task text is never touched.** It carries the instruction.
//
// In ModeAssisted nothing is ever dropped: the deduper still reports what it
// COULD have dropped, which is how an operator sees what enabling ModePreferred
// would buy before enabling it.

// LegacyDocument is one pre-existing context source a dispatch was going to
// send. It is the caller's own shape, reduced to what dedupe needs.
type LegacyDocument struct {
	// Path identifies the source. For a real file it is the repo-relative
	// path; for a synthetic source ("issue context") it is a label, and a
	// label never matches a memory item's source path, so synthetic sources
	// are never deduped.
	Path string
	// SHA256 is the digest of the content AO holds, when the caller computed
	// one. An empty digest means the caller cannot prove what it is holding,
	// and the document is kept.
	SHA256 string
	// Content is the body.
	Content string
}

// Bytes reports the document's cost.
func (d LegacyDocument) Bytes() int { return len(d.Content) }

// DedupeDecision is what the deduper concluded about one legacy document.
type DedupeDecision struct {
	Path string
	// Covered reports that memory demonstrably carries this document's
	// content: a valid item names the path and its digest matches.
	Covered bool
	// Dropped reports that it was actually removed, which requires Covered
	// AND a mode that permits replacement AND budget headroom for the
	// document allowance.
	Dropped bool
	// Bytes is what dropping it saved, or would have saved.
	Bytes int
	// Reason explains a document that was covered but kept.
	Reason string
}

// DedupeResult is the outcome over a whole document set.
type DedupeResult struct {
	// Kept are the documents the dispatch should still send, in the caller's
	// original order.
	Kept []LegacyDocument
	// Decisions is one entry per input document, for observability.
	Decisions []DedupeDecision
	// SavedBytes is what was actually removed.
	SavedBytes int
	// CoveredBytes is what memory covered, whether or not it was removed. In
	// ModeAssisted this is the size of the saving ModePreferred would unlock.
	CoveredBytes int
}

// Deduper decides which legacy documents a memory pack makes redundant.
type Deduper struct {
	mode MemoryMode
	// maxDocuments bounds how many documents may be replaced, from the role's
	// budget. It exists because replacement is the operation with real risk:
	// bounding it means a policy mistake costs some redundancy rather than a
	// dispatch's entire context.
	maxDocuments int
}

// NewDeduper builds a deduper for one role's dispatch.
func NewDeduper(mode MemoryMode, budget RoleBudget) *Deduper {
	return &Deduper{mode: mode, maxDocuments: budget.MaxDocuments}
}

// Apply decides each document against the pack.
func (d *Deduper) Apply(docs []LegacyDocument, pack ContextPack) DedupeResult {
	result := DedupeResult{Kept: make([]LegacyDocument, 0, len(docs))}
	if len(docs) == 0 {
		return result
	}
	covered := coverageIndex(pack)
	dropped := 0

	for _, doc := range docs {
		decision := DedupeDecision{Path: doc.Path, Bytes: doc.Bytes()}
		digest, isCovered := covered[normalizePath(doc.Path)]

		switch {
		case !isCovered:
			// Memory says nothing about this source.
		case doc.SHA256 == "":
			decision.Reason = "AO holds no digest for this document, so memory cannot be shown to match it"
		case digest == "":
			decision.Reason = "the memory fact carries no source digest to compare"
		case digest != DocumentDigest(doc.Path, doc.SHA256):
			decision.Reason = "the memory fact was derived from a different version of this file"
		default:
			decision.Covered = true
			result.CoveredBytes += decision.Bytes
		}

		switch {
		case !decision.Covered:
			result.Kept = append(result.Kept, doc)
		case !d.mode.MayReplace():
			decision.Reason = "memory covers this document, but mode " + string(d.mode) + " only adds and never replaces"
			result.Kept = append(result.Kept, doc)
		case dropped >= d.maxDocuments:
			decision.Reason = fmt.Sprintf("this role's budget allows replacing at most %d documents", d.maxDocuments)
			result.Kept = append(result.Kept, doc)
		default:
			decision.Dropped = true
			dropped++
			result.SavedBytes += decision.Bytes
		}
		result.Decisions = append(result.Decisions, decision)
	}
	return result
}

// coverageIndex maps a source path to the digest the pack's facts were derived
// from.
//
// Only facts the pack ACTUALLY CARRIES contribute. A fact that exists in the
// store but lost the budget cannot license dropping the document it summarises
// — that would replace a document with nothing at all, which is the one outcome
// worse than sending it twice.
//
// A path claimed by more than one fact keeps the first digest in the pack's
// deterministic order, and a path whose facts disagree about the digest is
// dropped from the index entirely: disagreement means AO cannot say which
// version memory describes, and the honest response is to keep the document.
func coverageIndex(pack ContextPack) map[string]string {
	index := map[string]string{}
	conflicting := map[string]bool{}
	for _, section := range pack.Sections {
		for _, sel := range section.Items {
			if !sel.BodyIncluded {
				// A fact reduced to its summary is not a stand-in for the
				// document it came from; it no longer carries the content.
				continue
			}
			digest := sourceDigestOf(sel.Item)
			if digest == "" {
				// An aggregate — a module census, a repository overview — names
				// several sources and summarises their COMBINATION. It cannot
				// license replacing any one of them, and it must not be allowed
				// to poison the entry a single-source fact would have made: it
				// simply contributes nothing.
				continue
			}
			for _, path := range sel.Item.SourcePaths {
				key := normalizePath(path)
				if conflicting[key] {
					continue
				}
				if prior, seen := index[key]; seen && prior != digest {
					delete(index, key)
					conflicting[key] = true
					continue
				}
				index[key] = digest
			}
		}
	}
	return index
}

// sourceDigestOf returns the provenance digest of a fact that stands for
// exactly one file.
//
// A single-source fact's SourceDigest is MemorySourceDigest over that one
// path, so it can be compared directly against a caller's document digest
// folded the same way (see DocumentDigest) without either side inverting a
// hash.
//
// A fact with several sources — or none — returns the empty string, and
// coverageIndex drops it. Such a fact summarises a combination and cannot
// stand in for any individual member of it.
func sourceDigestOf(item domain.ProjectMemoryItem) string {
	if len(item.SourcePaths) != 1 {
		return ""
	}
	return item.SourceDigest
}

// DocumentDigest folds a legacy document's own SHA-256 into the space memory
// stores its provenance in, so the two can be compared without either side
// learning the other's hashing scheme.
func DocumentDigest(path, sha256Hex string) string {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(sha256Hex) == "" {
		return ""
	}
	return domain.MemorySourceDigest(map[string]string{normalizePath(path): sha256Hex})
}

func normalizePath(p string) string {
	return strings.TrimPrefix(strings.TrimSpace(p), "./")
}

// SortedDecisions returns the decisions in path order, for stable operator
// output and stable tests.
func (r DedupeResult) SortedDecisions() []DedupeDecision {
	out := append([]DedupeDecision(nil), r.Decisions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
