package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_findings_evidence.go — the durable, non-secret proof that a fix cycle's
// prompt actually carried the reviewer's complete findings.
//
// The gap this closes is not a propagation bug. maybeDispatchFix reads
// reviewRun.EffectiveBody() and BuildFixPrompt interpolates it verbatim, which
// is the same accessor and the same bytes GetRun renders as the step's
// FindingsSummary — so the UI and the fix worker have always been fed from one
// source. What was missing was the ability to SAY so afterwards. The dispatch
// ledger recorded how big the prompt was and which transport carried it, and
// nothing whatsoever about the findings inside it, so an operator looking at an
// idle fix worker could not tell "the worker was given the findings and chose
// not to edit" from "the worker was handed an empty findings block".
//
// Everything here is derived from bytes AO already holds and is bounded on the
// way out: a digest, a length, a count, and a short head snippet no longer than
// the findings preview the API already returns. No prompt is persisted.

// findingsSnippetMaxLen bounds the human-readable head of the findings recorded
// on a dispatch. It is deliberately shorter than reviewFindingsSummaryMaxLen —
// this field exists to let a person recognize WHICH findings travelled, not to
// become a second copy of them.
const findingsSnippetMaxLen = 200

// FixFindingsEvidence is what AO can prove, from durable state alone, about the
// reviewer output a single fix cycle was dispatched with.
//
// It is written on the pre-delivery intent record (strictly before Send) and
// again on the dispatch record, so it survives a restart in either window and
// is what a recovered dispatch carries forward unchanged.
type FixFindingsEvidence struct {
	// Source names where the findings text came from: a reviewer's review run,
	// or AO's own local verification output on the verify->fix re-entry path.
	Source string `json:"source,omitempty"`
	// ReviewRunID is the review run the fix cycle is bound to. On the verify
	// re-entry path this is still the review run the fix step is attached to,
	// while Source says the findings themselves came from verification.
	ReviewRunID string `json:"reviewRunId,omitempty"`
	// ReviewVerdict is the effective verdict that authorized this fix cycle.
	ReviewVerdict string `json:"reviewVerdict,omitempty"`
	// ReviewTargetSHA is the commit the review ran against.
	ReviewTargetSHA string `json:"reviewTargetSha,omitempty"`
	// Bytes is the length of the findings text in the prompt.
	Bytes int `json:"bytes"`
	// Count is how many discrete findings CountReviewFindings could see. It is
	// observability only and never gates a decision — see its doc comment.
	Count int `json:"count"`
	// Digest is sha256 over the exact findings bytes. Two dispatches carrying
	// the same digest carried the same findings; a stale generation feeding a
	// newer attempt is visible as a digest that does not match the review run's
	// current body.
	Digest string `json:"digest,omitempty"`
	// Embedded is the load-bearing field: whether the exact findings bytes
	// appear verbatim in the prompt that was about to be delivered. It is
	// computed against the FINAL prompt — after any context pack was prepended
	// — so it answers the operator's actual question rather than the builder's.
	Embedded bool `json:"embedded"`
	// Snippet is a bounded, whitespace-collapsed head of the findings, so a
	// person can recognize them without opening the review run.
	Snippet string `json:"snippet,omitempty"`
}

// FixFindingsSource values. Deliberately strings on the wire so an older
// record deserializes into something readable rather than a zero enum.
const (
	FixFindingsSourceReview       = "review_run"
	FixFindingsSourceVerification = "verification"
)

// fixFindingsRef is what a dispatch caller knows about the findings it built
// its prompt from. It travels alongside the prompt through dispatchFixStep so
// the evidence can be computed at the one point that sees the final bytes.
//
// It carries the findings body rather than re-reading it, because the whole
// claim being recorded is about the text that went INTO this prompt — a second
// read could legitimately return something newer and would prove nothing.
type fixFindingsRef struct {
	Source    string
	Body      string
	ReviewRun domain.ReviewRun
}

// reviewFindingsRef is the ordinary review-driven case: the findings are the
// review run's own effective body.
func reviewFindingsRef(rr domain.ReviewRun) fixFindingsRef {
	return fixFindingsRef{Source: FixFindingsSourceReview, Body: rr.EffectiveBody(), ReviewRun: rr}
}

// verifyFindingsRef is the verify->fix re-entry case: the findings are AO's own
// rendered verification output, dispatched against the review run the fix step
// is bound to.
func verifyFindingsRef(rr domain.ReviewRun, body string) fixFindingsRef {
	return fixFindingsRef{Source: FixFindingsSourceVerification, Body: body, ReviewRun: rr}
}

// evidence computes the durable record against the FINAL prompt bytes.
func (f fixFindingsRef) evidence(prompt string) FixFindingsEvidence {
	ev := FixFindingsEvidence{
		Source:          f.Source,
		ReviewRunID:     f.ReviewRun.ID,
		ReviewVerdict:   string(f.ReviewRun.EffectiveVerdict()),
		ReviewTargetSHA: f.ReviewRun.TargetSHA,
		Bytes:           len(f.Body),
		Count:           CountReviewFindings(f.Body),
		Snippet:         findingsSnippet(f.Body),
	}
	if f.Body != "" {
		ev.Digest = FindingsDigest(f.Body)
		ev.Embedded = strings.Contains(prompt, f.Body)
	}
	return ev
}

// FindingsDigest is sha256 over the exact findings bytes, hex-encoded. It is
// the identity of a findings payload: stable, non-reversible, and comparable
// across a restart or between a review run and the dispatch that consumed it.
func FindingsDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// CountReviewFindings counts the discrete findings a free-text review body
// appears to contain.
//
// A review body is prose, not a schema — AO has never had a structured finding
// type, and inventing one here would mean rewriting the reviewer contract for
// the sake of a number. So this is an explicitly bounded heuristic: it counts
// top-level list items (`-`, `*`, `+`, `1.`, `1)`) and markdown headings, and
// falls back to 1 for a non-empty body with no such markers, because findings
// that exist are never zero findings.
//
// It is observability ONLY. Nothing in dispatch, delivery, recovery or
// verification reads it, and no decision may be keyed on it — a miscount must
// never be able to change what a worker receives.
func CountReviewFindings(body string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		// Indented markers are sub-points of the item above them, not findings
		// of their own. Counting them is how "one finding with three notes"
		// becomes "four findings".
		if len(line)-len(trimmed) >= 2 {
			continue
		}
		if isFindingMarker(trimmed) {
			n++
		}
	}
	if n == 0 && strings.TrimSpace(body) != "" {
		return 1
	}
	return n
}

func isFindingMarker(s string) bool {
	switch {
	case s == "":
		return false
	case strings.HasPrefix(s, "#"):
		// A heading needs a space after its hashes; `#3 is wrong` is prose.
		rest := strings.TrimLeft(s, "#")
		return rest != s && strings.HasPrefix(rest, " ")
	case strings.HasPrefix(s, "- "), strings.HasPrefix(s, "* "), strings.HasPrefix(s, "+ "):
		return true
	}
	// `12.` / `12)` ordered-list markers.
	digits := 0
	for digits < len(s) && unicode.IsDigit(rune(s[digits])) {
		digits++
	}
	if digits == 0 || digits+1 >= len(s) {
		return false
	}
	return (s[digits] == '.' || s[digits] == ')') && s[digits+1] == ' '
}

// findingsSnippet renders a bounded, single-line head of the findings.
func findingsSnippet(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	if s == "" {
		return ""
	}
	if len(s) > findingsSnippetMaxLen {
		return s[:findingsSnippetMaxLen] + "…"
	}
	return s
}
