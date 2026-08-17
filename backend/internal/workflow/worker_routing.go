package workflow

// plannedComplexityFacts builds a ReviewRiskFacts snapshot from a work
// step's PlanArtifact, BEFORE the worker has touched anything (checkpoint
// brief §5: "REUTILÍZALA o extrae una abstracción común. No crees un
// segundo classifier contradictorio"). It deliberately reuses the exact
// same ReviewRiskFacts/classifyTaskComplexity path review_policy.go already
// uses post-hoc (computeReviewRiskFacts, review_policy_dispatch.go) — the
// only difference is which facts are available at this point in time:
// ChangedFilePaths here is an *expected* proxy (the plan's own declared
// Verify.Files paths, i.e. the files the plan itself says it will check),
// not an *observed* one. This is a prediction used only to pick an initial
// worker harness; the real post-hoc classification at review time is
// unchanged and remains authoritative for ReviewPolicy itself.
func plannedComplexityFacts(artifact PlanArtifact, priorAttempts int) ReviewRiskFacts {
	facts := ReviewRiskFacts{
		ObjectiveText:             artifact.Objective,
		AcceptanceCriteria:        artifact.AcceptanceCriteria,
		AcceptanceCriteriaEmpty:   len(artifact.AcceptanceCriteria) == 0,
		VerifyCommandCount:        len(artifact.Verification.Commands),
		VerifyFileCheckCount:      len(artifact.Verification.Files),
		PriorWorkProviderAttempts: priorAttempts,
	}
	if n := len(artifact.Verification.Files); n > 0 {
		paths := make([]string, n)
		for i, fc := range artifact.Verification.Files {
			paths[i] = fc.Path
		}
		facts.ChangedFilePaths = paths
		facts.ChangedFileCount = n
		if n == 1 {
			fc := artifact.Verification.Files[0]
			if fc.ExactContent != nil || fc.SHA256 != "" {
				facts.HasExactContentCheckForSoleChangedFile = true
			}
		}
	}
	return facts
}

// EstimateWorkerComplexity is the pre-dispatch TaskComplexity estimate used
// to pick an initial worker harness (checkpoint brief §5/§7). It calls the
// same EvaluateReviewPolicy/classifyTaskComplexity ReviewPolicy already
// uses — no second, contradictory classifier.
func EstimateWorkerComplexity(artifact PlanArtifact, priorAttempts int) TaskComplexity {
	return EvaluateReviewPolicy(plannedComplexityFacts(artifact, priorAttempts)).Complexity
}
