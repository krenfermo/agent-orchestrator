package workflow

import (
	stdctx "context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// repair_context.go — what a repair agent is actually told.
//
// THE GAP THE INCIDENT EXPOSED. P1-B's objective carried "AO's own durable
// vocabulary and nothing else": the condition, the run, the step, the
// guardrails. That was a deliberate choice and it was defensible while the
// repair ran in a checkout that held the work — "read the failing checks and
// the existing changes in this worktree" was an instruction the agent could
// actually follow.
//
// wf-3af3c533 followed it, in a checkout identical to origin/main. There were
// no existing changes to read and no failing checks to find, and the objective
// gave it nothing else to go on: not the review that requested changes, not the
// two findings that named two specific files, not the commit under review, not
// even which branch the work was on. The prompt was internally consistent and
// carried no information whatsoever about the defect.
//
// So the pack now carries the EVIDENCE as well as the vocabulary:
//
//	the artifact           branch, commit, and the files it changed
//	the review             its id, its verdict, and its findings verbatim
//	the failure            the verification output when one failed
//	the acceptance         what "done" means for the task being repaired
//
// WHAT IS STILL EXCLUDED, and why the exclusion list got shorter rather than
// disappearing. No provider output, no session transcript, no reasoning of any
// kind from the worker that failed — those are chain-of-thought and they are
// not evidence. What travels is what AO already shows a person on the run page:
// a reviewer's published findings, and a verification's own stdout/stderr tails.
// Every byte of it is bounded, and the digest of the findings is recorded on the
// artifact authority so "the repair was given the same findings the fix cycle
// was" is checkable afterwards rather than asserted here.

// repairFindingsMaxBytes bounds the reviewer findings carried into a repair
// objective. It is generous — the whole point is that the repairer can act on
// them — but it is a bound: a review body is free text from a model, and an
// unbounded interpolation is an unbounded prompt.
const repairFindingsMaxBytes = 16000

// repairVerifyOutputMaxBytes bounds the verification output carried with it.
const repairVerifyOutputMaxBytes = 8000

// repairContext is everything the repair objective interpolates, gathered once
// from durable state so the objective builder stays a pure function of it.
type repairContext struct {
	// Findings is the reviewer's own text, verbatim and bounded.
	Findings string
	// FindingsTruncated says the text above is not all of it, so the repairer
	// is told rather than left to assume a complete list.
	FindingsTruncated bool
	// VerifyOutput is AO's own rendered verification failure, when the
	// condition being repaired is a failed verification.
	VerifyOutput string
	// AcceptanceCriteria is what "done" means for the ORIGIN task -- the
	// contract the repaired artifact still has to satisfy.
	AcceptanceCriteria []string
}

// buildRepairContext assembles the evidence pack for one repair.
//
// Read-only and best-effort per field: a piece of evidence AO cannot obtain is
// simply absent from the prompt, never fabricated and never a reason to refuse
// the repair. The one thing a repair may NOT proceed without is its artifact,
// and that refusal happens before this is ever called.
func (c *Coordinator) buildRepairContext(ctx stdctx.Context, target RunDetail, artifact domain.RepairArtifactAuthority) repairContext {
	pack := repairContext{}
	if artifact.ReviewRunID != "" && c.reviewRuns != nil {
		if rr, ok, err := c.reviewRuns.GetReviewRun(ctx, artifact.ReviewRunID); err == nil && ok {
			body := rr.EffectiveBody()
			if len(body) > repairFindingsMaxBytes {
				body = body[:repairFindingsMaxBytes]
				pack.FindingsTruncated = true
			}
			pack.Findings = body
		}
	}
	if result, ok, err := c.latestVerifyResult(ctx, target.Run.ID); err == nil && ok && !result.Passed {
		out := renderVerifyFindings(result)
		if len(out) > repairVerifyOutputMaxBytes {
			out = out[:repairVerifyOutputMaxBytes] + "\n[truncated]\n"
		}
		pack.VerifyOutput = out
	}
	if artifactPlan, err := c.planArtifactForRun(ctx, target.Run); err == nil {
		pack.AcceptanceCriteria = artifactPlan.AcceptanceCriteria
	}
	return pack
}

// renderRepairArtifact is the "what you are looking at" section: the branch,
// the commit, and the files this artifact changes.
//
// It is written even when the repair works in the project's own checkout,
// because "you are on the branch the work is on" is exactly as load-bearing a
// statement as "you are on a checkout cut from this commit" — and the incident
// is what happens when neither is said at all.
func renderRepairArtifact(b *strings.Builder, a domain.RepairArtifactAuthority) {
	if !a.HasArtifact {
		b.WriteString("This task has not produced any work yet, so this checkout is the project's default branch.\n\n")
		return
	}
	b.WriteString("THE CODE UNDER REPAIR\n")
	if a.OriginBranch != "" {
		fmt.Fprintf(b, "Branch: %s\n", a.OriginBranch)
	}
	switch {
	case a.Placement == domain.PlacementDirectBranch:
		b.WriteString("Your workspace IS the repository this work lives in, on the branch above.\n")
	case a.BaseSHA != "":
		fmt.Fprintf(b, "Commit: %s\nYour workspace was cut from exactly this commit, so the work under review is already in it.\n",
			a.BaseSHA)
	}
	if len(a.ChangedFiles) > 0 {
		fmt.Fprintf(b, "Files this task changed (%d):\n", len(a.ChangedFiles))
		for _, f := range a.ChangedFiles {
			b.WriteString("- " + f + "\n")
		}
	}
	b.WriteString("\n")
}

func renderRepairReview(b *strings.Builder, a domain.RepairArtifactAuthority, pack repairContext) {
	if pack.Findings == "" {
		return
	}
	b.WriteString("THE REVIEWER'S FINDINGS\n")
	if a.ReviewRunID != "" {
		fmt.Fprintf(b, "Review %s returned %s.\n", a.ReviewRunID, a.ReviewVerdict)
	}
	b.WriteString("\n" + pack.Findings + "\n")
	if pack.FindingsTruncated {
		b.WriteString("\n[the findings above are truncated; open the review for the rest]\n")
	}
	b.WriteString("\n")
}

func renderRepairVerification(b *strings.Builder, pack repairContext) {
	if pack.VerifyOutput == "" {
		return
	}
	b.WriteString("THE FAILING VERIFICATION\n\n")
	b.WriteString(pack.VerifyOutput)
	b.WriteString("\n")
}

func renderRepairAcceptance(b *strings.Builder, pack repairContext) {
	if len(pack.AcceptanceCriteria) == 0 {
		return
	}
	b.WriteString("WHAT THE ORIGINAL TASK STILL HAS TO SATISFY\n")
	for _, crit := range pack.AcceptanceCriteria {
		b.WriteString("- " + crit + "\n")
	}
	b.WriteString("\n")
}
