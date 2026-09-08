package workflow

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestReviewRiskTierClassifiesEveryReason is the guard against a reason being
// added to ReviewPolicy's vocabulary and quietly falling through the tier
// tables. Every reason must be in exactly one table; an unclassified one is
// treated as high, which is safe, but it should be a deliberate classification
// rather than an accident, so this test names them all.
func TestReviewRiskTierClassifiesEveryReason(t *testing.T) {
	t.Parallel()
	all := []ReviewReason{
		ReasonAuthOrSecurityPath, ReasonPaymentsOrBillingPath, ReasonMigrationOrSchemaPath,
		ReasonConcurrencyPath, ReasonInfraOrCICDPath, ReasonPublicAPIPath,
		ReasonDestructiveIntent, ReasonDependencyConfigChange, ReasonLargeOrMultiModule,
		ReasonPriorProviderAttempts, ReasonAmbiguousAcceptance, ReasonInsufficientVerify,
		ReasonDocsOnlyChange, ReasonExactContentSingleFile, ReasonDefaultConservative,
		ReasonNoChangedFiles,
	}
	for _, r := range all {
		n := 0
		for _, set := range []map[ReviewReason]struct{}{
			highRiskReviewReasons, standardRiskReviewReasons, lowRiskReviewReasons,
		} {
			if inReasonSet(set, r) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("reason %q appears in %d tier tables, want exactly 1", r, n)
		}
	}
}

// TestReviewRiskTierLowMatchesSkip pins the equivalence the whole "depth none
// cannot skip a review AO would otherwise have run" argument rests on: the low
// tier is exactly the set of decisions EvaluateReviewPolicy already SKIPS.
//
// If a future edit makes a REQUIRED decision come back low, or a SKIPPED one
// come back above low, this fails and the design note in
// docs/proportional-review-depth.md has to be revisited rather than silently
// becoming untrue.
func TestReviewRiskTierLowMatchesSkip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		facts ReviewRiskFacts
	}{
		{"docs only with verify coverage", ReviewRiskFacts{
			ChangedFilePaths: []string{"docs/a.md", "docs/b.md"}, ChangedFileCount: 2,
			AcceptanceCriteria: []string{"x"}, VerifyCommandCount: 1,
		}},
		{"single file with an exact content check", ReviewRiskFacts{
			ChangedFilePaths: []string{"pkg/thing.txt"}, ChangedFileCount: 1,
			AcceptanceCriteria: []string{"x"}, VerifyFileCheckCount: 1,
			HasExactContentCheckForSoleChangedFile: true,
		}},
		{"ordinary code change", ReviewRiskFacts{
			ChangedFilePaths: []string{"pkg/thing.go"}, ChangedFileCount: 1,
			AcceptanceCriteria: []string{"x"}, VerifyCommandCount: 1,
		}},
		{"auth path", ReviewRiskFacts{
			ChangedFilePaths: []string{"internal/auth/token.go"}, ChangedFileCount: 1,
			AcceptanceCriteria: []string{"x"}, VerifyCommandCount: 1,
		}},
		{"no verify coverage", ReviewRiskFacts{
			ChangedFilePaths: []string{"pkg/thing.go"}, ChangedFileCount: 1,
			AcceptanceCriteria: []string{"x"},
		}},
		{"nothing observed", ReviewRiskFacts{AcceptanceCriteria: []string{"x"}, VerifyCommandCount: 1}},
	} {
		decision := EvaluateReviewPolicy(tc.facts)
		tier, _ := ReviewRiskTierFor(decision)
		wantLow := decision.Decision == ReviewSkipped
		gotLow := tier == domain.ReviewRiskLow
		if wantLow != gotLow {
			t.Errorf("%s: decision=%q reasons=%v tier=%q — low tier and SKIPPED must coincide",
				tc.name, decision.Decision, decision.Reasons, tier)
		}
	}
}

func TestReviewRiskTierIsTheMaximumOverReasons(t *testing.T) {
	t.Parallel()
	// One auth path among many docs files is still high risk.
	decision := ReviewPolicyDecision{
		Decision: ReviewRequired,
		Reasons:  []ReviewReason{ReasonDocsOnlyChange, ReasonAuthOrSecurityPath, ReasonLargeOrMultiModule},
	}
	tier, reasons := ReviewRiskTierFor(decision)
	if tier != domain.ReviewRiskHigh {
		t.Fatalf("tier = %q, want high", tier)
	}
	if len(reasons) != 3 {
		t.Errorf("every reason must be carried onto the decision, got %v", reasons)
	}
}

func TestReviewRiskTierFailsSafe(t *testing.T) {
	t.Parallel()
	// A reason nobody classified.
	tier, _ := ReviewRiskTierFor(ReviewPolicyDecision{
		Decision: ReviewRequired, Reasons: []ReviewReason{"a_reason_from_the_future"},
	})
	if tier != domain.ReviewRiskHigh {
		t.Errorf("an unclassified reason gave tier %q, want high", tier)
	}
	// A decision that explains nothing.
	tier, _ = ReviewRiskTierFor(ReviewPolicyDecision{Decision: ReviewRequired})
	if tier != domain.ReviewRiskHigh {
		t.Errorf("a decision with no reasons gave tier %q, want high", tier)
	}
	// REQUIRED can never come back low, whatever the reasons claim.
	tier, _ = ReviewRiskTierFor(ReviewPolicyDecision{
		Decision: ReviewRequired, Reasons: []ReviewReason{ReasonDocsOnlyChange},
	})
	if tier == domain.ReviewRiskLow {
		t.Error("a REQUIRED decision must never resolve to the low tier")
	}
}

// Payments must be non-degradable, which starts with ReviewPolicy seeing them
// at all.
func TestPaymentsPathIsRequiredAndHighRisk(t *testing.T) {
	t.Parallel()
	decision := EvaluateReviewPolicy(ReviewRiskFacts{
		ChangedFilePaths:   []string{"internal/billing/stripe_webhook.go"},
		ChangedFileCount:   1,
		AcceptanceCriteria: []string{"x"},
		VerifyCommandCount: 1,
	})
	if decision.Decision != ReviewRequired {
		t.Fatalf("decision = %q, want required", decision.Decision)
	}
	found := false
	for _, r := range decision.Reasons {
		if r == ReasonPaymentsOrBillingPath {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %v, want %q among them", decision.Reasons, ReasonPaymentsOrBillingPath)
	}
	if tier, _ := ReviewRiskTierFor(decision); tier != domain.ReviewRiskHigh {
		t.Errorf("tier = %q, want high", tier)
	}
}

// The narrow patterns must not drag ordinary code into a full review.
func TestPaymentsPatternsDoNotOverMatch(t *testing.T) {
	t.Parallel()
	decision := EvaluateReviewPolicy(ReviewRiskFacts{
		ChangedFilePaths:   []string{"internal/workflow/dispatch.go"},
		ChangedFileCount:   1,
		AcceptanceCriteria: []string{"x"},
		VerifyCommandCount: 1,
	})
	for _, r := range decision.Reasons {
		if r == ReasonPaymentsOrBillingPath {
			t.Fatalf("an ordinary path matched the payments table: %v", decision.Reasons)
		}
	}
}

func TestReviewBodyRequestsEscalation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		body string
		want bool
	}{
		{"AO-ESCALATE: this touches the session token path", true},
		{"\n\n  AO-ESCALATE: leading whitespace is fine", true},
		{"The change looks fine but AO-ESCALATE: not on the first line", false},
		{"ao-escalate: lower case is not the marker", false},
		{"Ordinary findings.", false},
		{"", false},
	} {
		if got := reviewBodyRequestsEscalation(tc.body); got != tc.want {
			t.Errorf("reviewBodyRequestsEscalation(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// The deep prompt must stay byte-identical to the prompt AO built before review
// depth existed. Every depth other than light — including the zero value and an
// unrecognised one — must produce it, because a depth AO cannot read must never
// buy a cheaper review.
func TestOnlyLightDepthChangesThePrompt(t *testing.T) {
	t.Parallel()
	base := ReviewPromptInput{
		Objective: "make the thing work", AcceptanceCriteria: []string{"it works"},
		WorkerSessionID: "sess-1", Branch: "b", WorktreePath: "/w", BaseSHA: "base", HeadSHA: "fp",
		ReviewRunID: "rr-1",
	}
	deep := BuildReviewPrompt(base)
	for _, depth := range []domain.ReviewDepth{
		"", domain.ReviewDepthNone, domain.ReviewDepthDeep, domain.ReviewDepth("exhaustive"),
	} {
		in := base
		in.Depth = depth
		if got := BuildReviewPrompt(in); got != deep {
			t.Errorf("depth %q produced a different prompt from the deep one", depth)
		}
	}
	in := base
	in.Depth = domain.ReviewDepthLight
	if BuildReviewPrompt(in) == deep {
		t.Error("the light depth produced the deep prompt")
	}
}

// The light prompt must keep every guardrail the deep one has, tell the
// reviewer the diff is the evidence, and give it the escalation channel.
func TestLightPromptKeepsGuardrailsAndOffersEscalation(t *testing.T) {
	t.Parallel()
	prompt := BuildReviewPrompt(ReviewPromptInput{
		Objective: "o", AcceptanceCriteria: []string{"c"},
		WorkerSessionID: "sess-1", Branch: "b", WorktreePath: "/w", BaseSHA: "base", HeadSHA: "fp",
		ReviewRunID:      "rr-1",
		Depth:            domain.ReviewDepthLight,
		RiskTier:         domain.ReviewRiskStandard,
		RiskReasons:      []string{"default_conservative_required"},
		ChangedPaths:     []string{"pkg/a.go", "pkg/b.go"},
		ChangedFileCount: 2,
		VerifyCommands:   []string{"go test ./pkg/..."},
	})
	for _, want := range []string{
		"Do NOT modify any file in this worktree.",
		"Do NOT stage, commit, push, merge, rebase, or switch branches.",
		"Do NOT open, create, or otherwise interact with a pull request",
		"gh command",
		"ao review submit sess-1 --run rr-1 --verdict approved",
		"ao review submit sess-1 --run rr-1 --verdict changes_requested --body <path>",
		domain.ReviewEscalationMarker,
		"is NOT evidence",
		"pkg/a.go",
		"go test ./pkg/...",
		"default_conservative_required",
		"standard risk",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the light prompt is missing %q", want)
		}
	}
}

// An unobservable workspace must read as "AO could not look", never as "nothing
// changed": a reviewer told a change is empty when AO simply could not observe
// it is a reviewer AO misled.
func TestLightPromptIsHonestAboutMissingEvidence(t *testing.T) {
	t.Parallel()
	prompt := BuildReviewPrompt(ReviewPromptInput{
		Objective: "o", WorkerSessionID: "s", ReviewRunID: "r",
		Depth: domain.ReviewDepthLight,
	})
	if !strings.Contains(prompt, "could not observe a changed-file list") {
		t.Error("a missing changed-file list must be stated, not rendered as an empty list")
	}
	if !strings.Contains(prompt, "none is declared for this task") {
		t.Error("an absent verification plan must be stated so the reviewer can weigh it")
	}
	if !strings.Contains(prompt, "unclassified") {
		t.Error("an unrecorded risk tier must read as unclassified, never as low")
	}
}

// The changed-path list is bounded, and the count it reports is the real one.
func TestLightPromptBoundsTheChangedPathList(t *testing.T) {
	t.Parallel()
	paths := make([]string, lightReviewMaxChangedPaths+7)
	for i := range paths {
		paths[i] = "pkg/file" + string(rune('a'+i%26)) + ".go"
	}
	prompt := BuildReviewPrompt(ReviewPromptInput{
		Objective: "o", WorkerSessionID: "s", ReviewRunID: "r",
		Depth: domain.ReviewDepthLight, ChangedPaths: paths, ChangedFileCount: len(paths),
	})
	if !strings.Contains(prompt, "and 7 more") {
		t.Error("the prompt must say how many paths it withheld")
	}
	if !strings.Contains(prompt, "(57 observed in the worktree)") {
		t.Errorf("the prompt must report the real count, not the rendered one:\n%s",
			prompt[:min(len(prompt), 1200)])
	}
}
