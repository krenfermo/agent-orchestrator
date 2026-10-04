package domain

import (
	"strings"
	"time"
)

// work_report.go — what a worker SAYS it did.
//
// P5-A phase 2 needs two different things to exist and it needs them to be
// impossible to confuse:
//
//   - what AO OBSERVED (commands AO ran itself through the verification
//     runtime, with exit codes AO read) — see workflow/pre_review_evidence.go;
//   - what the WORKER REPORTED (prose and claims produced by the same process
//     whose work is under review).
//
// This file is the second one, and every name in it says so. There is no field
// called Passed, no field called Verified, and no method that returns "the
// tests passed". A worker's claim about a test is a WorkReportTestClaim with an
// Outcome it CLAIMED, and the only thing AO ever does with it is show it to a
// reviewer clearly labelled as a claim, or compare it against what AO observed
// and record that the two disagreed.
//
// The reason for that severity is the failure this whole phase exists to
// prevent. "I ran the tests and they pass" is the single cheapest sentence for
// a model to emit and the single most expensive one to believe. A report is
// useful — it tells a reviewer where to look, what the worker thinks it did not
// finish, and which criteria it believes it addressed — and it is never, under
// any policy, a reason to review less.

// WorkReportVersion identifies the shape of a serialized report. A reader that
// does not recognise a version treats the report as absent rather than
// guessing at its fields.
const WorkReportVersion = "work-report/v1"

// Bounds. A report is a declaration attached to every review pack, so it has a
// ceiling for the same reason the evidence snapshot does: a bundle that can
// grow without limit becomes the reason a cheap review is expensive.
const (
	// WorkReportMaxSummaryBytes bounds the free-text summary.
	WorkReportMaxSummaryBytes = 4000
	// WorkReportMaxItems bounds each list (criteria, claims, limitations,
	// risks, follow-up, changed paths).
	WorkReportMaxItems = 50
	// WorkReportMaxItemBytes bounds one entry inside those lists.
	WorkReportMaxItemBytes = 500
)

// WorkReportClaimedOutcome is what the worker SAYS happened when it ran
// something. Deliberately not a boolean and deliberately not named Passed: the
// zero value is "the worker did not say", which is a different fact from "the
// worker said it failed".
type WorkReportClaimedOutcome string

const (
	// WorkReportOutcomeUnstated is the zero value: no claim was made.
	WorkReportOutcomeUnstated WorkReportClaimedOutcome = ""
	// WorkReportOutcomeClaimedPassed — the worker says this command succeeded.
	// AO never reads this as a pass.
	WorkReportOutcomeClaimedPassed WorkReportClaimedOutcome = "claimed_passed"
	// WorkReportOutcomeClaimedFailed — the worker says this command failed.
	// This one AO does act on, in the safe direction only: a worker that admits
	// a failure is never given a cheaper review for it.
	WorkReportOutcomeClaimedFailed WorkReportClaimedOutcome = "claimed_failed"
	// WorkReportOutcomeClaimedSkipped — the worker says it did not run this.
	WorkReportOutcomeClaimedSkipped WorkReportClaimedOutcome = "claimed_skipped"
)

// Valid reports whether o is a recognised claim.
func (o WorkReportClaimedOutcome) Valid() bool {
	switch o {
	case WorkReportOutcomeClaimedPassed, WorkReportOutcomeClaimedFailed, WorkReportOutcomeClaimedSkipped:
		return true
	default:
		return false
	}
}

// AdmitsFailure reports whether this claim is the worker saying something went
// wrong. It is the ONE direction in which a report may influence policy,
// because believing a confession costs nothing: the worst case is a fuller
// review of work that was in fact fine.
func (o WorkReportClaimedOutcome) AdmitsFailure() bool {
	return o == WorkReportOutcomeClaimedFailed
}

// WorkReportTestClaim is one command the worker says it ran, and what it says
// happened. ClaimedOutcome, never Outcome; Command, never VerifiedCommand.
type WorkReportTestClaim struct {
	Command        string                   `json:"command"`
	ClaimedOutcome WorkReportClaimedOutcome `json:"claimedOutcome,omitempty"`
	// Note is whatever the worker wanted to say about it. Prose.
	Note string `json:"note,omitempty"`
}

// WorkReportCriterion is the worker's claim about one acceptance criterion.
type WorkReportCriterion struct {
	Criterion string `json:"criterion"`
	// Addressed is the worker's CLAIM that it handled this criterion. It is
	// shown to a reviewer as a claim and is never a substitute for the
	// reviewer's own judgement about the same criterion.
	Addressed bool   `json:"addressed"`
	Note      string `json:"note,omitempty"`
}

// WorkReportReference points at the artifact the report is about, so a reader
// can tell whether the report describes the tree actually under review or an
// earlier one. AO fills FingerprintAtSubmission itself; the worker cannot
// compute AO's content fingerprint and is never asked to.
type WorkReportReference struct {
	Commit string `json:"commit,omitempty"`
	Branch string `json:"branch,omitempty"`
	// FingerprintAtSubmission is AO's OWN workspace fingerprint at the moment
	// the report was accepted. It is the only field in this struct AO trusts,
	// because it is the only one AO produced.
	FingerprintAtSubmission string `json:"fingerprintAtSubmission,omitempty"`
}

// WorkReport is the whole bounded, versioned declaration.
//
// Nothing in it is evidence. The type exists so that a reviewer gets the
// worker's account in a readable shape and so that AO can record, durably,
// that it had one — including the case where the account contradicts what AO
// observed, which is a fact worth keeping.
type WorkReport struct {
	Version string `json:"version"`
	// Summary is the worker's description of what it changed.
	Summary string `json:"summary,omitempty"`
	// ClaimedChangedPaths is the worker's list. AO has its own, observed from
	// the worktree, and prefers it everywhere; this one exists so a reviewer
	// can see a disagreement between the two.
	ClaimedChangedPaths []string              `json:"claimedChangedPaths,omitempty"`
	Criteria            []WorkReportCriterion `json:"criteria,omitempty"`
	// TestsReported is what the worker says it ran. See the type comment.
	TestsReported []WorkReportTestClaim `json:"testsReported,omitempty"`
	Limitations   []string              `json:"limitations,omitempty"`
	Risks         []string              `json:"risks,omitempty"`
	FollowUp      []string              `json:"followUp,omitempty"`
	Reference     WorkReportReference   `json:"reference,omitempty"`
	SubmittedAt   time.Time             `json:"submittedAt,omitempty"`
	// Truncated names the fields Normalize shortened, so a bounded report says
	// it is bounded instead of silently looking complete.
	Truncated []string `json:"truncated,omitempty"`
}

// Recorded reports whether r is a real report rather than a zero value.
func (r WorkReport) Recorded() bool { return r.Version == WorkReportVersion }

// AdmitsAnyFailure reports whether the worker itself said something went
// wrong: a claimed test failure, or any recorded limitation or risk.
//
// This is the only predicate on this type that policy is allowed to read, and
// it can only ever make a review DEEPER. See ReviewEvidenceRelief.
func (r WorkReport) AdmitsAnyFailure() bool {
	for _, c := range r.TestsReported {
		if c.ClaimedOutcome.AdmitsFailure() {
			return true
		}
	}
	return len(r.Limitations) > 0 || len(r.Risks) > 0
}

// UnaddressedCriteria lists the criteria the worker did NOT claim to have
// addressed. Also readable by policy, also only in the deepening direction.
func (r WorkReport) UnaddressedCriteria() []string {
	var out []string
	for _, c := range r.Criteria {
		if !c.Addressed {
			out = append(out, c.Criterion)
		}
	}
	return out
}

// Normalize trims, bounds and version-stamps a report submitted from outside.
// It never rejects content for being long — it truncates and says so — because
// a report is advisory and losing it entirely helps nobody.
func (r WorkReport) Normalize(now time.Time) WorkReport {
	out := r
	out.Version = WorkReportVersion
	if out.SubmittedAt.IsZero() {
		out.SubmittedAt = now
	}
	out.Truncated = nil

	if s, cut := clampText(strings.TrimSpace(out.Summary), WorkReportMaxSummaryBytes); cut {
		out.Summary = s
		out.Truncated = append(out.Truncated, "summary")
	} else {
		out.Summary = s
	}

	out.ClaimedChangedPaths, _ = clampList(out.ClaimedChangedPaths, &out.Truncated, "claimedChangedPaths")
	out.Limitations, _ = clampList(out.Limitations, &out.Truncated, "limitations")
	out.Risks, _ = clampList(out.Risks, &out.Truncated, "risks")
	out.FollowUp, _ = clampList(out.FollowUp, &out.Truncated, "followUp")

	if len(out.Criteria) > WorkReportMaxItems {
		out.Criteria = out.Criteria[:WorkReportMaxItems]
		out.Truncated = append(out.Truncated, "criteria")
	}
	for i := range out.Criteria {
		out.Criteria[i].Criterion, _ = clampText(strings.TrimSpace(out.Criteria[i].Criterion), WorkReportMaxItemBytes)
		out.Criteria[i].Note, _ = clampText(strings.TrimSpace(out.Criteria[i].Note), WorkReportMaxItemBytes)
	}

	if len(out.TestsReported) > WorkReportMaxItems {
		out.TestsReported = out.TestsReported[:WorkReportMaxItems]
		out.Truncated = append(out.Truncated, "testsReported")
	}
	for i := range out.TestsReported {
		out.TestsReported[i].Command, _ = clampText(strings.TrimSpace(out.TestsReported[i].Command), WorkReportMaxItemBytes)
		out.TestsReported[i].Note, _ = clampText(strings.TrimSpace(out.TestsReported[i].Note), WorkReportMaxItemBytes)
		if !out.TestsReported[i].ClaimedOutcome.Valid() {
			// An unrecognised outcome becomes "unstated", never "passed".
			out.TestsReported[i].ClaimedOutcome = WorkReportOutcomeUnstated
		}
	}
	return out
}

// clampText truncates s to at most max bytes on a rune boundary.
func clampText(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := s[:max]
	for len(cut) > 0 && !isRuneStart(cut[len(cut)-1]) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " \t\n") + "…", true
}

// isRuneStart reports whether b can begin a UTF-8 rune, used to avoid cutting
// a multi-byte character in half.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func clampList(in []string, truncated *[]string, label string) ([]string, bool) {
	cut := false
	if len(in) > WorkReportMaxItems {
		in = in[:WorkReportMaxItems]
		cut = true
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		t, _ := clampText(strings.TrimSpace(s), WorkReportMaxItemBytes)
		if t != "" {
			out = append(out, t)
		}
	}
	if cut {
		*truncated = append(*truncated, label)
	}
	if len(out) == 0 {
		return nil, cut
	}
	return out, cut
}
