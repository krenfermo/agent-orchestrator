package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func at() time.Time { return time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC) }

// passingEvidence is the one shape that can support relief: observed, all
// green, taken at `fp`, with a command that actually ran.
func passingEvidence(fp string) domain.PreReviewEvidence {
	zero := 0
	return domain.PreReviewEvidence{
		Version:              domain.PreReviewEvidenceVersion,
		Status:               domain.PreReviewEvidenceObserved,
		Fingerprint:          fp,
		FingerprintAfter:     fp,
		TargetKey:            "key-" + fp,
		PlanCommandCount:     1,
		ExecutedCommandCount: 1,
		Checks: []domain.PreReviewCheckRecord{
			{Kind: "command", Label: "go test ./internal/foo/...", Passed: true, ExitCode: &zero},
		},
	}
}

func relief(in domain.ReviewEvidenceReliefInput) domain.ReviewEvidenceRelief {
	in.Now = at()
	return domain.EvaluateReviewEvidenceRelief(in)
}

// The headline case: ordinary code, checks AO ran and watched pass, no reviewer
// required by the paths. This is the whole point of phase 2.
func TestStandardRiskWithObservedPassingEvidenceEarnsRelief(t *testing.T) {
	got := relief(domain.ReviewEvidenceReliefInput{
		Tier:                    domain.ReviewRiskStandard,
		ReviewTargetFingerprint: "fp-a",
		Evidence:                passingEvidence("fp-a"),
	})
	if !got.Granted {
		t.Fatalf("relief denied (%s); observed passing evidence on standard risk must earn it", got.Reason)
	}
	if got.Reason != domain.ReliefGranted {
		t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefGranted)
	}
	if got.EvidenceTargetKey != "key-fp-a" {
		t.Fatalf("the decision does not name the evidence it stands on: %q", got.EvidenceTargetKey)
	}
	if floor := domain.EvidenceRelievedMinimumDepth(domain.ReviewRiskStandard, got); floor != domain.ReviewDepthNone {
		t.Fatalf("relieved floor = %q, want none", floor)
	}
}

// High risk is non-degradable, and phase 2 must not have opened a back door
// into it. Perfect evidence buys nothing.
func TestHighRiskIsNeverRelievedByAnyEvidence(t *testing.T) {
	got := relief(domain.ReviewEvidenceReliefInput{
		Tier:                    domain.ReviewRiskHigh,
		ReviewTargetFingerprint: "fp-a",
		Evidence:                passingEvidence("fp-a"),
	})
	if got.Granted {
		t.Fatal("high risk was relieved by evidence; it is non-degradable")
	}
	if floor := domain.EvidenceRelievedMinimumDepth(domain.ReviewRiskHigh, got); floor != domain.ReviewDepthDeep {
		t.Fatalf("high-risk floor = %q, want deep", floor)
	}
	// And even a (bogus) granted relief must not lower a high floor: the
	// relieved-floor function refuses on the tier, not only on the flag.
	forged := domain.ReviewEvidenceRelief{Granted: true, Reason: domain.ReliefGranted}
	if floor := domain.EvidenceRelievedMinimumDepth(domain.ReviewRiskHigh, forged); floor != domain.ReviewDepthDeep {
		t.Fatalf("a forged relief lowered the high floor to %q", floor)
	}
	if floor := domain.EvidenceRelievedMinimumDepth("something-new", forged); floor != domain.ReviewDepthDeep {
		t.Fatalf("a forged relief lowered an unrecognised tier's floor to %q", floor)
	}
}

// Every way evidence can be less than "AO watched it pass" must deny relief.
// One table, because the property is one property: nothing short of an
// observed, attributable, complete pass counts.
func TestOnlyObservedPassingEvidenceRelieves(t *testing.T) {
	noCommands := passingEvidence("fp-a")
	noCommands.ExecutedCommandCount = 0
	noCommands.Checks = nil

	oneFailed := passingEvidence("fp-a")
	two := 2
	oneFailed.Checks = append(oneFailed.Checks, domain.PreReviewCheckRecord{
		Kind: "command", Label: "go vet ./...", Passed: false, ExitCode: &two,
	})
	oneFailed.ExecutedCommandCount = 2

	movedDuring := passingEvidence("fp-a")
	movedDuring.FingerprintAfter = "fp-b"

	contradicted := passingEvidence("fp-a")
	contradicted.ReportContradicted = true

	for _, tc := range []struct {
		name     string
		evidence domain.PreReviewEvidence
		want     domain.ReviewEvidenceReliefReason
	}{
		{"nothing recorded", domain.PreReviewEvidence{}, domain.ReliefDeniedNoEvidence},
		{"a check failed", func() domain.PreReviewEvidence {
			e := passingEvidence("fp-a")
			e.Status = domain.PreReviewEvidenceFailed
			return e
		}(), domain.ReliefDeniedNotObserved},
		{"it timed out", func() domain.PreReviewEvidence {
			e := passingEvidence("fp-a")
			e.Status = domain.PreReviewEvidenceTimedOut
			return e
		}(), domain.ReliefDeniedNotObserved},
		{"the runtime was unavailable", func() domain.PreReviewEvidence {
			e := passingEvidence("fp-a")
			e.Status = domain.PreReviewEvidenceUnavailable
			return e
		}(), domain.ReliefDeniedNotObserved},
		{"nothing was planned", func() domain.PreReviewEvidence {
			e := passingEvidence("fp-a")
			e.Status = domain.PreReviewEvidenceNotPlanned
			return e
		}(), domain.ReliefDeniedNotObserved},
		{"it could not be attributed", func() domain.PreReviewEvidence {
			e := passingEvidence("fp-a")
			e.Status = domain.PreReviewEvidenceUnattributed
			return e
		}(), domain.ReliefDeniedNotObserved},
		{"no command actually ran", noCommands, domain.ReliefDeniedNoCommands},
		{"a recorded check did not pass", oneFailed, domain.ReliefDeniedNotObserved},
		{"the tree moved while it ran", movedDuring, domain.ReliefDeniedFingerprint},
		{"the worker contradicted it", contradicted, domain.ReliefDeniedContradiction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := relief(domain.ReviewEvidenceReliefInput{
				Tier:                    domain.ReviewRiskStandard,
				ReviewTargetFingerprint: "fp-a",
				Evidence:                tc.evidence,
			})
			if got.Granted {
				t.Fatal("relief was granted on evidence that does not support it")
			}
			if got.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", got.Reason, tc.want)
			}
			// Whatever the reason, the floor must be phase 1's.
			if floor := domain.EvidenceRelievedMinimumDepth(domain.ReviewRiskStandard, got); floor != domain.ReviewDepthLight {
				t.Fatalf("floor = %q, want light", floor)
			}
		})
	}
}

// Evidence about a DIFFERENT tree is not evidence about this change. This is
// the property that makes a fix cycle end reuse and relief automatically.
func TestEvidenceForAnotherTreeNeverRelieves(t *testing.T) {
	for _, target := range []string{"fp-b", ""} {
		got := relief(domain.ReviewEvidenceReliefInput{
			Tier:                    domain.ReviewRiskStandard,
			ReviewTargetFingerprint: target,
			Evidence:                passingEvidence("fp-a"),
		})
		if got.Granted {
			t.Fatalf("relief granted with review target %q against evidence at fp-a", target)
		}
		if got.Reason != domain.ReliefDeniedFingerprint {
			t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefDeniedFingerprint)
		}
	}
}

// A REQUIRED review is a mandate. Green evidence does not convert it into a
// suggestion, even if a future edit made REQUIRED reachable below high.
func TestABlockingReviewReasonIsNeverRelieved(t *testing.T) {
	got := relief(domain.ReviewEvidenceReliefInput{
		Tier:                    domain.ReviewRiskStandard,
		ReviewTargetFingerprint: "fp-a",
		Evidence:                passingEvidence("fp-a"),
		HasReviewBlockingReason: true,
	})
	if got.Granted {
		t.Fatal("a review required by a doubt tests cannot answer was relieved by evidence")
	}
	if got.Reason != domain.ReliefDeniedBlockingReason {
		t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefDeniedBlockingReason)
	}
}

// The worker's report may only ever DEEPEN. A confession blocks relief; a
// glowing report buys nothing.
func TestWorkerReportOnlyEverDeepens(t *testing.T) {
	t.Run("a confession blocks relief", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			report domain.WorkReport
		}{
			{"claimed a test failed", domain.WorkReport{
				Version:       domain.WorkReportVersion,
				TestsReported: []domain.WorkReportTestClaim{{Command: "go test ./...", ClaimedOutcome: domain.WorkReportOutcomeClaimedFailed}},
			}},
			{"declared a limitation", domain.WorkReport{
				Version: domain.WorkReportVersion, Limitations: []string{"did not cover the windows path"},
			}},
			{"declared a risk", domain.WorkReport{
				Version: domain.WorkReportVersion, Risks: []string{"this may change the cache key"},
			}},
			{"left a criterion unaddressed", domain.WorkReport{
				Version:  domain.WorkReportVersion,
				Criteria: []domain.WorkReportCriterion{{Criterion: "handle empty input", Addressed: false}},
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got := relief(domain.ReviewEvidenceReliefInput{
					Tier:                    domain.ReviewRiskStandard,
					ReviewTargetFingerprint: "fp-a",
					Evidence:                passingEvidence("fp-a"),
					Report:                  tc.report,
				})
				if got.Granted {
					t.Fatal("relief granted despite the worker admitting a problem")
				}
				if got.Reason != domain.ReliefDeniedWorkerAdmission {
					t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefDeniedWorkerAdmission)
				}
			})
		}
	})

	// The other half, and the one that matters most: a worker SAYING the tests
	// passed is worth exactly nothing without AO having observed it.
	t.Run("a claimed pass is not evidence", func(t *testing.T) {
		glowing := domain.WorkReport{
			Version: domain.WorkReportVersion,
			Summary: "All done, everything passes, no issues at all.",
			TestsReported: []domain.WorkReportTestClaim{
				{Command: "go test ./...", ClaimedOutcome: domain.WorkReportOutcomeClaimedPassed},
			},
			Criteria: []domain.WorkReportCriterion{{Criterion: "handle empty input", Addressed: true}},
		}
		got := relief(domain.ReviewEvidenceReliefInput{
			Tier:                    domain.ReviewRiskStandard,
			ReviewTargetFingerprint: "fp-a",
			// AO observed NOTHING. Only the worker spoke.
			Evidence: domain.PreReviewEvidence{},
			Report:   glowing,
		})
		if got.Granted {
			t.Fatal("a worker's claim that the tests passed bought a cheaper review")
		}
		if got.Reason != domain.ReliefDeniedNoEvidence {
			t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefDeniedNoEvidence)
		}
	})
}

// Depth resolution with relief must keep every phase-1 guarantee. In
// particular a request may still only ever deepen.
func TestResolveReviewDepthWithEvidenceStillNeverDegrades(t *testing.T) {
	granted := domain.ReviewEvidenceRelief{Granted: true, Reason: domain.ReliefGranted}

	t.Run("master stays deep with relief granted", func(t *testing.T) {
		snap := domain.DefaultReviewDepthPolicy(domain.ExecutionStrategyMaster, at())
		for _, tier := range []domain.ReviewRiskTier{domain.ReviewRiskLow, domain.ReviewRiskStandard, domain.ReviewRiskHigh} {
			got := domain.ResolveReviewDepthWithEvidence(snap, tier, nil, granted, at())
			if got.Effective != domain.ReviewDepthDeep {
				t.Fatalf("master at tier %q resolved to %q, want deep", tier, got.Effective)
			}
		}
	})

	t.Run("an explicit deep request survives relief", func(t *testing.T) {
		snap := domain.ReviewDepthPolicySnapshot{
			Version: domain.ReviewDepthPolicyVersion, Requested: domain.ReviewDepthDeep, Source: domain.ReviewDepthExplicit,
		}
		got := domain.ResolveReviewDepthWithEvidence(snap, domain.ReviewRiskStandard, nil, granted, at())
		if got.Effective != domain.ReviewDepthDeep {
			t.Fatalf("effective = %q; relief must never lower a deeper REQUEST", got.Effective)
		}
	})

	t.Run("a none request on standard risk is honoured only with relief", func(t *testing.T) {
		snap := domain.ReviewDepthPolicySnapshot{
			Version: domain.ReviewDepthPolicyVersion, Requested: domain.ReviewDepthNone, Source: domain.ReviewDepthExplicit,
		}
		without := domain.ResolveReviewDepthWithEvidence(snap, domain.ReviewRiskStandard, nil, domain.ReviewEvidenceRelief{}, at())
		if without.Effective != domain.ReviewDepthLight {
			t.Fatalf("without relief effective = %q, want light (phase 1's clamp)", without.Effective)
		}
		if without.Reason != domain.ReviewDepthReasonClampedByRiskTier {
			t.Fatalf("without relief reason = %q, want the clamp", without.Reason)
		}
		with := domain.ResolveReviewDepthWithEvidence(snap, domain.ReviewRiskStandard, nil, granted, at())
		if with.Effective != domain.ReviewDepthNone {
			t.Fatalf("with relief effective = %q, want none", with.Effective)
		}
		if with.Reason != domain.ReviewDepthReasonEvidenceRelieved {
			t.Fatalf("reason = %q, want the decision to name the evidence", with.Reason)
		}
	})

	t.Run("with no relief it is phase 1 exactly", func(t *testing.T) {
		for _, tier := range []domain.ReviewRiskTier{domain.ReviewRiskLow, domain.ReviewRiskStandard, domain.ReviewRiskHigh, "unknown"} {
			for _, req := range []domain.ReviewDepth{domain.ReviewDepthNone, domain.ReviewDepthLight, domain.ReviewDepthDeep} {
				snap := domain.ReviewDepthPolicySnapshot{
					Version: domain.ReviewDepthPolicyVersion, Requested: req, Source: domain.ReviewDepthExplicit,
				}
				old := domain.ResolveReviewDepth(snap, tier, nil, at())
				got := domain.ResolveReviewDepthWithEvidence(snap, tier, nil, domain.ReviewEvidenceRelief{}, at())
				if old.Effective != got.Effective || old.Reason != got.Reason || old.Source != got.Source {
					t.Fatalf("tier %q request %q: phase 2 without relief diverged from phase 1 (%+v vs %+v)", tier, req, old, got)
				}
			}
		}
	})

	// A legacy snapshot still reads as deep, relief or no relief: a run created
	// before anybody could choose must not be retroactively cheapened.
	t.Run("a legacy snapshot stays deep", func(t *testing.T) {
		got := domain.ResolveReviewDepthWithEvidence(domain.ReviewDepthPolicySnapshot{}, domain.ReviewRiskStandard, nil, granted, at())
		if got.Effective != domain.ReviewDepthDeep {
			t.Fatalf("legacy snapshot resolved to %q, want deep", got.Effective)
		}
	})
}

// The status combinator must never let a later success erase an earlier
// problem, and must treat an unknown status as the most serious thing there is.
func TestDeeperEvidenceFailureKeepsTheWorstOutcome(t *testing.T) {
	if got := domain.DeeperEvidenceFailure(domain.PreReviewEvidenceFailed, domain.PreReviewEvidenceObserved); got != domain.PreReviewEvidenceFailed {
		t.Fatalf("a later pass erased an earlier failure: %q", got)
	}
	if got := domain.DeeperEvidenceFailure(domain.PreReviewEvidenceFailed, domain.PreReviewEvidenceUnattributed); got != domain.PreReviewEvidenceUnattributed {
		t.Fatalf("got %q, want unattributed to outrank a plain failure", got)
	}
	if got := domain.DeeperEvidenceFailure(domain.PreReviewEvidenceObserved, "a-status-from-the-future"); got == domain.PreReviewEvidenceObserved {
		t.Fatal("an unrecognised status was treated as harmless")
	}
}

// The report is bounded, versioned and never invents an outcome.
func TestWorkReportNormalizeBoundsAndNeverInventsAPass(t *testing.T) {
	long := strings.Repeat("x", domain.WorkReportMaxSummaryBytes*2)
	items := make([]string, domain.WorkReportMaxItems+10)
	for i := range items {
		items[i] = "limitation"
	}
	in := domain.WorkReport{
		Summary:     long,
		Limitations: items,
		TestsReported: []domain.WorkReportTestClaim{
			{Command: "  go   test ./...  ", ClaimedOutcome: "definitely-fine"},
		},
	}
	got := in.Normalize(at())

	if got.Version != domain.WorkReportVersion {
		t.Fatalf("version = %q", got.Version)
	}
	if len(got.Summary) > domain.WorkReportMaxSummaryBytes+4 {
		t.Fatalf("summary not bounded: %d bytes", len(got.Summary))
	}
	if len(got.Limitations) != domain.WorkReportMaxItems {
		t.Fatalf("limitations = %d, want %d", len(got.Limitations), domain.WorkReportMaxItems)
	}
	if len(got.Truncated) == 0 {
		t.Fatal("a bounded report must say it was bounded")
	}
	// The one that matters: an outcome AO does not recognise becomes
	// "unstated", never "claimed_passed".
	if got.TestsReported[0].ClaimedOutcome != domain.WorkReportOutcomeUnstated {
		t.Fatalf("an unrecognised claim became %q; it must never become a pass", got.TestsReported[0].ClaimedOutcome)
	}
	if got.SubmittedAt.IsZero() {
		t.Fatal("SubmittedAt was not stamped")
	}
}

// A master's plan is executed by child task runs. If those could be relieved,
// a master workstream would be entirely unreviewed while still reporting that
// it required a full independent review — so a child never earns relief,
// however good its evidence is.
func TestAChildRunIsNeverRelieved(t *testing.T) {
	got := relief(domain.ReviewEvidenceReliefInput{
		Tier:                    domain.ReviewRiskStandard,
		ReviewTargetFingerprint: "fp-a",
		Evidence:                passingEvidence("fp-a"),
		IsChildRun:              true,
	})
	if got.Granted {
		t.Fatal("a child run was relieved; that spends the master guarantee nobody chose to spend")
	}
	if got.Reason != domain.ReliefDeniedChildRun {
		t.Fatalf("reason = %q, want %q", got.Reason, domain.ReliefDeniedChildRun)
	}
	if floor := domain.EvidenceRelievedMinimumDepth(domain.ReviewRiskStandard, got); floor != domain.ReviewDepthLight {
		t.Fatalf("floor = %q, want light", floor)
	}
}

// The order of refusals is itself a contract: the recorded reason must name the
// most serious objection, not the last one evaluated, or a decision's
// explanation drifts as the checks are reordered.
func TestReliefNamesTheMostSeriousObjection(t *testing.T) {
	// Every objection at once. The tier is the gravest, so the tier must be
	// what the record says.
	got := relief(domain.ReviewEvidenceReliefInput{
		Tier:                    domain.ReviewRiskHigh,
		ReviewTargetFingerprint: "fp-b",
		Evidence:                domain.PreReviewEvidence{},
		HasReviewBlockingReason: true,
		IsChildRun:              true,
		Report:                  domain.WorkReport{Version: domain.WorkReportVersion, Risks: []string{"everything"}},
	})
	if got.Reason != domain.ReliefDeniedTier {
		t.Fatalf("reason = %q, want the tier refusal to win", got.Reason)
	}
}
