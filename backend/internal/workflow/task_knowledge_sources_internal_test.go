package workflow

import (
	stdctx "context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/postrunqa"
)

// task_knowledge_sources_internal_test.go — what may become a Decision or a
// Risk, and what may not.
//
// Every test here is about an EXCLUSION as much as an inclusion, because the
// failure mode this file guards against is not "AO recorded too little". It is
// AO stating, as the project's durable knowledge, something no durable row
// actually says: a risk from a review that was superseded, a risk from
// breakage the task did not cause, a decision from an answer nobody
// authorized.
//
// The fakes below embed the port interface and implement only the methods the
// derivation is allowed to call. A derivation that reaches for anything else
// panics, which is the point: the set of rows these paths read is part of the
// contract, not an implementation detail.

type fakeKnowledgeStore struct {
	Store
	steps       []domain.WorkflowStep
	checkpoints map[string]domain.WorkflowCheckpoint
}

func (f fakeKnowledgeStore) ListWorkflowSteps(stdctx.Context, string) ([]domain.WorkflowStep, error) {
	return f.steps, nil
}

func (f fakeKnowledgeStore) GetLatestWorkflowCheckpointByStep(
	_ stdctx.Context, stepID string,
) (domain.WorkflowCheckpoint, bool, error) {
	cp, ok := f.checkpoints[stepID]
	return cp, ok, nil
}

type fakeKnowledgeReviews struct {
	ReviewRuns
	runs []domain.ReviewRun
}

func (f fakeKnowledgeReviews) ListReviewRunsBySession(stdctx.Context, domain.SessionID) ([]domain.ReviewRun, error) {
	return f.runs, nil
}

type fakeKnowledgePlans struct {
	masterPlanStore
	amendments []domain.WorkflowTaskCriterionAmendment
	tasks      []domain.WorkflowTask
}

func (f fakeKnowledgePlans) ListWorkflowTaskCriterionAmendments(
	stdctx.Context, string,
) ([]domain.WorkflowTaskCriterionAmendment, error) {
	return f.amendments, nil
}

func (f fakeKnowledgePlans) ListWorkflowTasks(stdctx.Context, string) ([]domain.WorkflowTask, error) {
	return f.tasks, nil
}

type fakeKnowledgeQuestions struct {
	QuestionsStore
	questions   []domain.WorkflowQuestion
	resolutions map[string]domain.WorkflowQuestionResolution
}

func (f fakeKnowledgeQuestions) ListWorkflowQuestionsByRun(stdctx.Context, string) ([]domain.WorkflowQuestion, error) {
	return f.questions, nil
}

func (f fakeKnowledgeQuestions) GetCurrentResolutionForQuestion(
	_ stdctx.Context, questionID string,
) (domain.WorkflowQuestionResolution, bool, error) {
	res, ok := f.resolutions[questionID]
	return res, ok, nil
}

type fakeQAGate struct {
	qa postrunqa.QARun
	ok bool
}

func (f fakeQAGate) LatestQARunForSubject(
	_ stdctx.Context, _ postrunqa.SubjectKind, _ string,
) (postrunqa.QARun, bool, error) {
	return f.qa, f.ok, nil
}

type fakeReviewThreads struct {
	byPR map[string][]domain.PullRequestReviewThread
}

func (f fakeReviewThreads) ListPRReviewThreads(
	_ stdctx.Context, prURL string,
) ([]domain.PullRequestReviewThread, error) {
	return f.byPR[prURL], nil
}

// knowledgeRun is the run every test here derives from: one planned task, one
// work step, one worker session.
func knowledgeRun() domain.WorkflowRun {
	taskID, parent := "task-1", "wf-master"
	return domain.WorkflowRun{
		ID: "wf-child", ProjectID: "proj-1",
		PlannedTaskID: &taskID, ParentWorkflowID: &parent,
	}
}

func knowledgeSteps() []domain.WorkflowStep {
	session := "sess-1"
	return []domain.WorkflowStep{{
		ID: "step-work", Kind: domain.WorkflowStepWork, SessionID: &session,
	}}
}

func topics(risks []TaskRiskFact) []string {
	out := make([]string, 0, len(risks))
	for _, r := range risks {
		out = append(out, r.Topic)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- risks -------------------------------------------------------------------

// TestAcceptedReviewerFindingsBecomeRisks is the inclusion half: a QA finding
// the gate itself calls blocking, an unresolved reviewer thread, and a review
// that requested changes and was never answered each become one risk.
func TestAcceptedReviewerFindingsBecomeRisks(t *testing.T) {
	run := knowledgeRun()
	c := &Coordinator{
		store: fakeKnowledgeStore{steps: knowledgeSteps()},
		reviewRuns: fakeKnowledgeReviews{runs: []domain.ReviewRun{{
			ID: "rr-1", PRURL: "https://example.test/pr/1", TargetSHA: "abcdef1234567890",
			Harness: "claude", Verdict: domain.VerdictChangesRequested,
			CreatedAt: time.Unix(100, 0),
		}}},
		reviewThreads: fakeReviewThreads{byPR: map[string][]domain.PullRequestReviewThread{
			"https://example.test/pr/1": {{ThreadID: "th-1", Path: "internal/store/store.go", Line: 42}},
		}},
		qaGate: fakeQAGate{ok: true, qa: postrunqa.QARun{
			Phase: postrunqa.PhaseNeedsAttention, Result: postrunqa.ResultNeedsAttention,
			Findings: []postrunqa.Finding{{
				Source: "go vet ./...", Signal: "internal/store: unreachable code",
				Signature:   "vet|internal/store|unreachable",
				Attribution: postrunqa.AttributionNew, Severity: postrunqa.SeverityBlocker,
				Scope: postrunqa.ScopeInScope, Verification: postrunqa.VerificationEvidence,
			}},
		}},
	}

	risks, resolved := c.durableTaskRisks(stdctx.Background(), run)
	if len(resolved) != 0 {
		t.Fatalf("nothing was resolved, yet %v was reported closed", resolved)
	}
	got := topics(risks)
	for _, want := range []string{
		"qa-finding:vet|internal/store|unreachable",
		"review-thread:th-1",
		"review-changes-requested:rr-1",
	} {
		if !contains(got, want) {
			t.Errorf("a durable finding produced no risk: want topic %q in %v", want, got)
		}
	}
	for _, r := range risks {
		if r.Kind != domain.KnowledgeKindRisk {
			t.Errorf("risk %q has kind %q", r.Topic, r.Kind)
		}
		if strings.TrimSpace(r.Statement) == "" {
			t.Errorf("risk %q has no statement", r.Topic)
		}
	}
	// The one risk whose source PROVES a file says so; the others do not
	// invent one.
	for _, r := range risks {
		switch r.Topic {
		case "review-thread:th-1":
			if !reflect.DeepEqual(r.Evidence, []string{"internal/store/store.go"}) {
				t.Errorf("a thread risk lost the path the reviewer named: %v", r.Evidence)
			}
		default:
			if len(r.Evidence) != 0 {
				t.Errorf("risk %q invented evidence %v", r.Topic, r.Evidence)
			}
		}
	}
}

// TestStaleAndSupersededFindingsAreNotRisks is the exclusion half, and it is
// the test that matters most: every row below is a finding that exists, and
// none of them is a risk this task leaves behind.
func TestStaleAndSupersededFindingsAreNotRisks(t *testing.T) {
	run := knowledgeRun()
	c := &Coordinator{
		store: fakeKnowledgeStore{steps: knowledgeSteps()},
		reviewRuns: fakeKnowledgeReviews{runs: []domain.ReviewRun{
			// Superseded: a replacement took authority, so this verdict is
			// evidence and never a decision.
			{ID: "rr-old", PRURL: "https://example.test/pr/1", Verdict: domain.VerdictChangesRequested,
				SupersededBy: "rr-new", CreatedAt: time.Unix(100, 0)},
			// Closed out with no verdict at all, and a late verdict that a
			// replacement already overrode.
			{ID: "rr-stale", PRURL: "https://example.test/pr/1", Status: domain.ReviewRunCancelled,
				LateVerdict: domain.VerdictChangesRequested, SupersededBy: "rr-new",
				CreatedAt: time.Unix(101, 0)},
			// Never concluded.
			{ID: "rr-running", PRURL: "https://example.test/pr/1", Status: domain.ReviewRunRunning,
				CreatedAt: time.Unix(102, 0)},
		}},
		reviewThreads: fakeReviewThreads{byPR: map[string][]domain.PullRequestReviewThread{
			"https://example.test/pr/1": {{ThreadID: "th-done", Path: "a.go", Resolved: true}},
		}},
		qaGate: fakeQAGate{ok: true, qa: postrunqa.QARun{
			Phase: postrunqa.PhaseNeedsAttention, Result: postrunqa.ResultNeedsAttention,
			Findings: []postrunqa.Finding{
				// Already true before this task ran.
				{Signal: "pre-existing lint failure", Signature: "baseline",
					Attribution: postrunqa.AttributionBaseline, Severity: postrunqa.SeverityBlocker,
					Scope: postrunqa.ScopeInScope, Verification: postrunqa.VerificationEvidence},
				// Somebody else's repository.
				{Signal: "another project has a dirty tree", Signature: "elsewhere",
					Attribution: postrunqa.AttributionNew, Severity: postrunqa.SeverityBlocker,
					Scope: postrunqa.ScopeOutOfScope, Verification: postrunqa.VerificationEvidence},
				// The agent's own prose, with nothing structured behind it.
				{Signal: "the agent said something may be wrong", Signature: "prose",
					Attribution: postrunqa.AttributionNew, Severity: postrunqa.SeverityBlocker,
					Scope: postrunqa.ScopeInScope, Verification: postrunqa.VerificationReportOnly},
				// Not a defect.
				{Signal: "a note worth keeping", Signature: "info",
					Attribution: postrunqa.AttributionNew, Severity: postrunqa.SeverityInfo,
					Scope: postrunqa.ScopeInScope, Verification: postrunqa.VerificationEvidence},
			},
		}},
	}

	risks, _ := c.durableTaskRisks(stdctx.Background(), run)
	if len(risks) != 0 {
		t.Fatalf("stale, superseded, baseline, out-of-scope and unverified findings produced %d risks: %v",
			len(risks), topics(risks))
	}
}

// TestResolvedFindingsResolveTheirRisk is the other half of a lifecycle: a
// source that reports a finding as fixed must CLOSE the risk it raised, not
// merely stop mentioning it. Falling silent would leave an earlier task's risk
// active forever.
func TestResolvedFindingsResolveTheirRisk(t *testing.T) {
	run := knowledgeRun()
	c := &Coordinator{
		store: fakeKnowledgeStore{steps: knowledgeSteps()},
		reviewRuns: fakeKnowledgeReviews{runs: []domain.ReviewRun{
			{ID: "rr-1", PRURL: "https://example.test/pr/1", Verdict: domain.VerdictChangesRequested,
				CreatedAt: time.Unix(100, 0)},
			{ID: "rr-2", PRURL: "https://example.test/pr/1", Verdict: domain.VerdictApproved,
				CreatedAt: time.Unix(200, 0)},
		}},
		reviewThreads: fakeReviewThreads{byPR: map[string][]domain.PullRequestReviewThread{
			"https://example.test/pr/1": {{ThreadID: "th-1", Path: "internal/store/store.go", Resolved: true}},
		}},
		qaGate: fakeQAGate{ok: true, qa: postrunqa.QARun{
			Phase: postrunqa.PhaseClean, Result: postrunqa.ResultClean,
			Findings: []postrunqa.Finding{{
				Source: "go build ./...", Signal: "internal/store: build failure",
				Signature: "build|internal/store", Attribution: postrunqa.AttributionNew,
				Severity: postrunqa.SeverityBlocker, Scope: postrunqa.ScopeInScope,
				Verification: postrunqa.VerificationEvidence,
			}},
		}},
	}

	risks, resolved := c.durableTaskRisks(stdctx.Background(), run)
	if len(risks) != 0 {
		t.Fatalf("findings a durable source reports as fixed were raised as open risks: %v", topics(risks))
	}
	for _, want := range []string{
		"qa-finding:build|internal/store",
		"review-thread:th-1",
		"review-changes-requested:rr-1",
	} {
		if !contains(resolved, want) {
			t.Errorf("a fixed finding did not resolve its risk: want %q in %v", want, resolved)
		}
	}
}

// TestRiskDerivationIsDeterministic covers the duplicate completion callback
// and the restart: the same durable rows must produce byte-identical facts, so
// a second recording addresses the same memory rows instead of a second copy.
func TestRiskDerivationIsDeterministic(t *testing.T) {
	run := knowledgeRun()
	c := &Coordinator{
		store: fakeKnowledgeStore{steps: knowledgeSteps()},
		reviewRuns: fakeKnowledgeReviews{runs: []domain.ReviewRun{
			{ID: "rr-1", PRURL: "https://example.test/pr/1", Verdict: domain.VerdictChangesRequested,
				CreatedAt: time.Unix(100, 0)},
		}},
		reviewThreads: fakeReviewThreads{byPR: map[string][]domain.PullRequestReviewThread{
			"https://example.test/pr/1": {{ThreadID: "th-1", Path: "a.go"}, {ThreadID: "th-2", Path: "b.go"}},
		}},
		planStore: fakeKnowledgePlans{amendments: []domain.WorkflowTaskCriterionAmendment{{
			TaskID: "task-1", OriginalCriterion: "the tree stays dirty",
			AmendedCriterion: "the tree is committed", Disposition: domain.WorkflowTaskCriterionAmended,
			Reason: "the state it described was committed in 70296042b", ApprovedBy: "a human",
			CreatedAt: time.Unix(300, 0),
		}}},
	}

	firstRisks, firstResolved := c.durableTaskRisks(stdctx.Background(), run)
	secondRisks, secondResolved := c.durableTaskRisks(stdctx.Background(), run)
	if !reflect.DeepEqual(firstRisks, secondRisks) || !reflect.DeepEqual(firstResolved, secondResolved) {
		t.Fatal("a second pass over the same durable rows produced different risks")
	}
	firstDecisions := c.durableTaskDecisions(stdctx.Background(), run)
	secondDecisions := c.durableTaskDecisions(stdctx.Background(), run)
	if !reflect.DeepEqual(firstDecisions, secondDecisions) {
		t.Fatal("a second pass over the same durable rows produced different decisions")
	}
}

// TestNoDurableSourceProducesNoRisks: with every source switched off, the
// derivation contributes nothing rather than falling back to prose.
func TestNoDurableSourceProducesNoRisks(t *testing.T) {
	risks, resolved := (&Coordinator{}).durableTaskRisks(stdctx.Background(), knowledgeRun())
	if len(risks) != 0 || len(resolved) != 0 {
		t.Fatalf("a coordinator with no sources invented %d risks and %d resolutions", len(risks), len(resolved))
	}
}

// --- decisions ---------------------------------------------------------------

// TestDurablePlanRationaleBecomesDecision: the amendment ledger and the
// question ledger are the two durable rationales AO holds, and both become
// decisions with the reason the row recorded.
func TestDurablePlanRationaleBecomesDecision(t *testing.T) {
	run := knowledgeRun()
	human := domain.AnswerSourceHuman
	c := &Coordinator{
		planStore: fakeKnowledgePlans{amendments: []domain.WorkflowTaskCriterionAmendment{
			{TaskID: "task-1", OriginalCriterion: "the postrunqa files stay uncommitted",
				Disposition: domain.WorkflowTaskCriterionObsolete,
				Reason:      "the state it describes was committed in 70296042b",
				ApprovedBy:  "a human", CreatedAt: time.Unix(100, 0)},
			// Another task's amendment, in the same plan.
			{TaskID: "task-9", OriginalCriterion: "something else",
				Disposition: domain.WorkflowTaskCriterionObsolete, Reason: "not this task's",
				ApprovedBy: "a human", CreatedAt: time.Unix(100, 0)},
		}},
		questionsStore: fakeKnowledgeQuestions{questions: []domain.WorkflowQuestion{{
			ID: "q-1", Fingerprint: "fp-1", State: domain.QuestionStateAnswered,
			QuestionText: "should the queue be a table or a JSON blob?",
			AnswerText:   "a table", AnswerSource: &human,
		}}},
	}

	decisions := c.durableTaskDecisions(stdctx.Background(), run)
	if len(decisions) != 2 {
		t.Fatalf("%d decisions, want 2 (one amendment, one answered question): %+v", len(decisions), decisions)
	}
	amendment := decisions[0]
	if !strings.HasPrefix(amendment.Topic, "acceptance-criterion:task-1:") {
		t.Errorf("the amendment decision is filed under %q", amendment.Topic)
	}
	if !strings.Contains(amendment.Rationale, "70296042b") ||
		!strings.Contains(amendment.Rationale, "a human") {
		t.Errorf("the amendment decision lost its durable reason or its approver: %q", amendment.Rationale)
	}
	if !strings.Contains(amendment.Statement, "no longer applies") {
		t.Errorf("an obsolete criterion was stated as %q", amendment.Statement)
	}
	question := decisions[1]
	if question.Topic != "question:fp-1" {
		t.Errorf("the answered question is filed under %q, want its durable fingerprint", question.Topic)
	}
	if !strings.Contains(question.Statement, "a table") {
		t.Errorf("the answer did not reach the decision: %q", question.Statement)
	}
}

// TestTheLatestAmendmentOfOneCriterionIsTheCurrentDecision: two amendments of
// the same criterion are one subject decided twice, and memory keeps the first
// statement it is handed for a subject. Handing it the newest is what makes
// the current answer current.
func TestTheLatestAmendmentOfOneCriterionIsTheCurrentDecision(t *testing.T) {
	c := &Coordinator{planStore: fakeKnowledgePlans{amendments: []domain.WorkflowTaskCriterionAmendment{
		{TaskID: "task-1", OriginalCriterion: "c", AmendedCriterion: "the first answer",
			Disposition: domain.WorkflowTaskCriterionAmended, Reason: "first", ApprovedBy: "h",
			CreatedAt: time.Unix(100, 0)},
		{TaskID: "task-1", OriginalCriterion: "c", AmendedCriterion: "the second answer",
			Disposition: domain.WorkflowTaskCriterionAmended, Reason: "second", ApprovedBy: "h",
			CreatedAt: time.Unix(200, 0)},
	}}}

	decisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(decisions) == 0 || !strings.Contains(decisions[0].Statement, "the second answer") {
		t.Fatalf("the newest amendment is not the first decision offered: %+v", decisions)
	}
	if decisions[0].Topic != decisions[1].Topic {
		t.Fatalf("two amendments of one criterion are two subjects: %q vs %q",
			decisions[0].Topic, decisions[1].Topic)
	}
}

// TestUnauthorizedOrMissingRationaleFabricatesNoDecision is the negative bar.
// Nothing below is a decision AO may state, and each is a different reason.
func TestUnauthorizedOrMissingRationaleFabricatesNoDecision(t *testing.T) {
	run := knowledgeRun()
	resolver := domain.AnswerSourceResolver
	human := domain.AnswerSourceHuman
	c := &Coordinator{
		// The plan holds a task with a description and acceptance criteria and
		// no persisted rationale. That is not a decision, and inventing one
		// from the task's own prose is exactly what P2-C forbids.
		planStore: fakeKnowledgePlans{tasks: []domain.WorkflowTask{{
			ID: "task-1", Title: "add the queue", Description: "because the store needs one",
		}}},
		questionsStore: fakeKnowledgeQuestions{
			questions: []domain.WorkflowQuestion{
				// Never answered.
				{ID: "q-open", Fingerprint: "fp-open", State: domain.QuestionStatePending,
					QuestionText: "which storage engine?"},
				// Answered by a resolver whose attempt asked for a human.
				{ID: "q-unsure", Fingerprint: "fp-unsure", State: domain.QuestionStateAnswered,
					QuestionText: "which storage engine?", AnswerText: "probably sqlite",
					AnswerSource: &resolver},
				// Answered by a resolver whose attempt AO cannot read at all.
				{ID: "q-orphan", Fingerprint: "fp-orphan", State: domain.QuestionStateAnswered,
					QuestionText: "which storage engine?", AnswerText: "sqlite",
					AnswerSource: &resolver},
				// A human answer with no text.
				{ID: "q-empty", Fingerprint: "fp-empty", State: domain.QuestionStateAnswered,
					QuestionText: "which storage engine?", AnswerSource: &human},
			},
			resolutions: map[string]domain.WorkflowQuestionResolution{
				"q-unsure": {Status: domain.ResolutionStatusComplete, RequiresHuman: true},
			},
		},
	}

	if decisions := c.durableTaskDecisions(stdctx.Background(), run); len(decisions) != 0 {
		t.Fatalf("AO fabricated %d decisions from rows that authorize none: %+v", len(decisions), decisions)
	}
}

// TestCompletedResolverAnswerCarriesItsOwnReason: a resolver answer IS
// authoritative once its attempt completed without asking for a human, and the
// rationale is the resolver's own recorded summary rather than a sentence
// composed here.
func TestCompletedResolverAnswerCarriesItsOwnReason(t *testing.T) {
	resolver := domain.AnswerSourceResolver
	c := &Coordinator{questionsStore: fakeKnowledgeQuestions{
		questions: []domain.WorkflowQuestion{{
			ID: "q-1", Fingerprint: "fp-1", State: domain.QuestionStateAnswered,
			QuestionText: "which storage engine?", AnswerText: "sqlite", AnswerSource: &resolver,
		}},
		resolutions: map[string]domain.WorkflowQuestionResolution{
			"q-1": {Status: domain.ResolutionStatusComplete, ReasonSummary: "the repository already ships sqlite"},
		},
	}}

	decisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(decisions) != 1 {
		t.Fatalf("%d decisions, want 1", len(decisions))
	}
	if decisions[0].Rationale != "the repository already ships sqlite" {
		t.Errorf("rationale = %q, want the resolver's own recorded summary", decisions[0].Rationale)
	}
}

// TestStatementsStayBounded: a durable row with an unusually long text becomes
// a short fact, never an unbounded one — one task must not be able to fill a
// later task's whole context.
func TestStatementsStayBounded(t *testing.T) {
	long := strings.Repeat("a very long criterion ", 200)
	c := &Coordinator{planStore: fakeKnowledgePlans{amendments: []domain.WorkflowTaskCriterionAmendment{{
		TaskID: "task-1", OriginalCriterion: "c", AmendedCriterion: long,
		Disposition: domain.WorkflowTaskCriterionAmended, Reason: "why", ApprovedBy: "h",
		CreatedAt: time.Unix(100, 0),
	}}}}

	decisions := c.durableTaskDecisions(stdctx.Background(), knowledgeRun())
	if len(decisions) != 1 {
		t.Fatalf("%d decisions, want 1", len(decisions))
	}
	if len(decisions[0].Statement) > maxKnowledgeStatement {
		t.Fatalf("statement is %d bytes, want at most %d", len(decisions[0].Statement), maxKnowledgeStatement)
	}
}
