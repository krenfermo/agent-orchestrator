package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3c_autonomous_decision_test.go — P3-C's second half: a decision AO takes for
// itself has to be durably distinguishable from one a person gave, and it has to
// reach Shared Task Knowledge as that.
//
// The property under test throughout is the DISTINCTION, not the automation.
// Recording an automatic decision as a human one is the single most damaging
// thing this vocabulary could do — every downstream reader treats a human answer
// as authoritative outright — so most of what follows checks that the two never
// collapse into each other.

// lowRiskTechnicalPaneText is a real technical, reversible choice: where a
// helper lives. It is exactly the shape auto_decide_low_risk exists for, and it
// matches none of the escalation classes.
func lowRiskTechnicalPaneText() string {
	return "Should I extract this into a helper in utils.go or inline it here?\n" +
		"❯ 1. Extract into utils.go\n" +
		"  2. Inline it here\n"
}

// destructivePaneText is the shape no autonomy mode may ever decide.
func destructivePaneText() string {
	return "Should I delete the production database rows first?\n" +
		"❯ 1. Yes\n" +
		"  2. No\n"
}

// setAutonomyMode freezes a run's question-autonomy policy the way run creation
// does, by rewriting its policy snapshot — going through the store rather than a
// helper keeps the test honest about what the production readers actually read.
func setAutonomyMode(t *testing.T, store *sqlite.Store, runID string, mode domain.QuestionAutonomyMode) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): ok=%v err=%v", runID, ok, err)
	}
	var policy domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &policy); err != nil {
		t.Fatal(err)
	}
	frozen := policy.EffectiveAutonomyPolicy()
	frozen.Mode = mode
	policy.Autonomy = frozen
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.UpdateWorkflowRunPolicySnapshot(ctx, runID, string(snapshot), time.Now().UTC()); err != nil || !ok {
		t.Fatalf("set autonomy mode: ok=%v err=%v", ok, err)
	}
}

// completeResolutionFor stands in for the real read-only Decision Resolver
// session: it mints the durable resolution row the resolver would have been
// launched under, points the question at it, and records the answer through the
// SAME store transition the `ao decision resolve` callback uses.
//
// It fabricates no question state — the question was classified and moved to
// `resolving` by production code — and it never writes the answer onto the
// question itself. That transition is observeResolutionStep's, and it is
// precisely what these tests are checking.
func completeResolutionFor(t *testing.T, store *sqlite.Store, q domain.WorkflowQuestion, answer, reason string, evidence []string) domain.WorkflowQuestionResolution {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	session := domain.SessionID("sess-resolver-" + string(q.ID))
	res, err := store.InsertWorkflowQuestionResolution(ctx, domain.WorkflowQuestionResolution{
		ID:                 domain.WorkflowQuestionResolutionID("wqr-" + string(q.ID)),
		WorkflowQuestionID: q.ID,
		WorkflowRunID:      q.WorkflowRunID,
		ResolverHarness:    domain.AgentHarness("codex"),
		ResolverSessionID:  &session,
		Status:             domain.ResolutionStatusRunning,
		CreatedAt:          now,
		UpdatedAt:          now,
	})
	if err != nil {
		t.Fatalf("insert resolution: %v", err)
	}
	if _, err := store.SetWorkflowQuestionResolvingRunID(ctx, string(q.ID), strPtrLocal(string(res.ID))); err != nil {
		t.Fatalf("point question at resolution: %v", err)
	}
	certainty := domain.QuestionCertaintyActual
	ok, err := store.TransitionResolutionStatus(ctx, string(res.ID),
		domain.ResolutionStatusRunning, domain.ResolutionStatusComplete,
		answer, reason, evidence, &certainty, false, now, &now)
	if err != nil || !ok {
		t.Fatalf("complete resolution: ok=%v err=%v", ok, err)
	}
	updated, _, err := store.GetWorkflowQuestionResolution(ctx, string(res.ID))
	if err != nil {
		t.Fatalf("read back resolution: %v", err)
	}
	return updated
}

func strPtrLocal(s string) *string { return &s }

func onlyQuestion(t *testing.T, store *sqlite.Store, runID string) domain.WorkflowQuestion {
	t.Helper()
	qs, err := store.ListWorkflowQuestionsByRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowQuestionsByRun: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("len(questions) = %d, want exactly 1: %+v", len(qs), qs)
	}
	return qs[0]
}

// §1/§6: a low-risk technical ambiguity under auto_decide_low_risk is routed to
// the resolver, is NOT a human prompt, and durably records WHICH policy sent it
// there.
func TestLowRiskAmbiguityIsAutoDecidedAndRecordsItsPolicy(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	if q.Classification != domain.QuestionClassificationAutoResolvable {
		t.Fatalf("classification = %q, want auto_resolvable (reason: %s)", q.Classification, q.ClassificationReason)
	}
	if q.State != domain.QuestionStateResolving {
		t.Fatalf("state = %q, want resolving — a low-risk choice must not become a human prompt", q.State)
	}
	if q.AutonomyMode != domain.QuestionAutonomyAutoDecideLowRisk {
		t.Fatalf("autonomyMode = %q, want auto_decide_low_risk", q.AutonomyMode)
	}
	if !strings.Contains(q.ClassificationReason, "auto-decided under autonomy policy") {
		t.Fatalf("the classification reason does not say AO decided this: %q", q.ClassificationReason)
	}
}

// §1: ask_always is untouched. The same question still goes to a person, and
// nothing records an autonomy policy against it.
func TestAskAlwaysStillPromptsTheHumanForTheSameQuestion(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	if q.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %q, want human_required under the default ask_always", q.State)
	}
	if q.AutonomyMode != "" {
		t.Fatalf("autonomyMode = %q, want empty: no policy authorized anything", q.AutonomyMode)
	}
}

// §6/§7: no autonomy mode reaches past the classifier's own refusals. A
// destructive question is a person's under full_autonomy too.
func TestDestructiveQuestionStillEscalatesUnderFullAutonomy(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, destructivePaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyFullAutonomy)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	if q.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %q, want human_required for a destructive question", q.State)
	}
	if q.AutonomyMode != "" {
		t.Fatalf("a destructive question recorded autonomy policy %q", q.AutonomyMode)
	}
}

// §1: the answer AO produced for itself is durably `autonomous` — never
// `human`, and never the plain `resolver` a discovery question gets.
func TestAutonomousAnswerIsDurablyDistinguishedFromHumanAndResolver(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	completeResolutionFor(t, store, q, "Extract into utils.go",
		"utils.go already holds the sibling helpers this one belongs with", []string{"utils.go"})

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (observe): %v", err)
	}
	answered := onlyQuestion(t, store, runID)
	if answered.State != domain.QuestionStateAnswered {
		t.Fatalf("state = %q, want answered", answered.State)
	}
	if answered.AnswerSource == nil {
		t.Fatal("an answered question with no recorded source")
	}
	if *answered.AnswerSource != domain.AnswerSourceAutonomous {
		t.Fatalf("answerSource = %q, want autonomous", *answered.AnswerSource)
	}
	if !answered.AnswerSource.Automatic() {
		t.Fatal("an autonomous answer did not report itself as automatic")
	}
	if answered.AnswerText != "Extract into utils.go" {
		t.Fatalf("answerText = %q", answered.AnswerText)
	}
}

// §1: the discovery-shape path keeps its own source. Widening `autonomous` to
// cover it would lose exactly the distinction this checkpoint added.
func TestDiscoveryShapedQuestionStaysResolverNotAutonomous(t *testing.T) {
	ctx := context.Background()
	pane := "Which existing helper should I use for this?\n❯ 1. formatPath\n  2. cleanPath\n"
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, pane)
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	// Deliberately the DEFAULT policy: this question needs no autonomy to be
	// auto-resolvable, and must not be labelled as though it did.
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	if q.Classification != domain.QuestionClassificationAutoResolvable {
		t.Fatalf("classification = %q, want auto_resolvable", q.Classification)
	}
	if q.AutonomyMode != "" {
		t.Fatalf("a discovery-shaped question recorded autonomy policy %q", q.AutonomyMode)
	}
	completeResolutionFor(t, store, q, "formatPath", "formatPath already does this", nil)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (observe): %v", err)
	}
	answered := onlyQuestion(t, store, runID)
	if answered.AnswerSource == nil || *answered.AnswerSource != domain.AnswerSourceResolver {
		t.Fatalf("answerSource = %v, want resolver", answered.AnswerSource)
	}
}

// §4/§12: exactly once. Repeated observation passes — which is what a restart
// and every board poll produce — answer the question one time and never twice.
func TestAutonomousResolutionHappensExactlyOnceAcrossRepeatedPasses(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, sender, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	completeResolutionFor(t, store, q, "Extract into utils.go", "sibling helpers live there", nil)

	answeredAt := time.Time{}
	for pass := 1; pass <= 5; pass++ {
		if _, err := coord.GetRun(ctx, runID); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		got := onlyQuestion(t, store, runID)
		if got.AnsweredAt == nil {
			t.Fatalf("pass %d: the question is not answered", pass)
		}
		if answeredAt.IsZero() {
			answeredAt = *got.AnsweredAt
			continue
		}
		if !got.AnsweredAt.Equal(answeredAt) {
			t.Fatalf("pass %d: the answer was rewritten (%s -> %s)", pass, answeredAt, *got.AnsweredAt)
		}
	}
	// One question, one answer, one delivery — however many times AO looks.
	if sender.calls != 1 {
		t.Fatalf("the answer was delivered %d times, want exactly 1", sender.calls)
	}
}

// §12: a restart between the resolution and the task's completion changes
// nothing. The coordinator is rebuilt over the same store, which is what a
// restart is from this package's point of view.
func TestAutonomousDecisionSurvivesARestartWithoutRepeating(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, sender, _, clock := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	completeResolutionFor(t, store, q, "Extract into utils.go", "sibling helpers live there", nil)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (observe): %v", err)
	}
	first := onlyQuestion(t, store, runID)

	// The restart: a brand-new coordinator over the same durable rows.
	rebooted := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: sessionFacts,
		QuestionsStore: store, MessageSender: sender, Clock: clock.Now,
	})
	for pass := 1; pass <= 3; pass++ {
		if _, err := rebooted.GetRun(ctx, runID); err != nil {
			t.Fatalf("post-restart pass %d: %v", pass, err)
		}
	}
	after := onlyQuestion(t, store, runID)
	if after.State != domain.QuestionStateAnswered {
		t.Fatalf("post-restart state = %q, want answered", after.State)
	}
	if after.AnswerSource == nil || *after.AnswerSource != domain.AnswerSourceAutonomous {
		t.Fatalf("post-restart answerSource = %v, want autonomous", after.AnswerSource)
	}
	if !after.AnsweredAt.Equal(*first.AnsweredAt) {
		t.Fatal("the restart re-answered the question")
	}
	if after.AnswerText != first.AnswerText {
		t.Fatalf("the restart changed the answer: %q -> %q", first.AnswerText, after.AnswerText)
	}
	if sender.calls != 1 {
		t.Fatalf("the answer was delivered %d times across a restart, want exactly 1", sender.calls)
	}
}

// §13: while AO is deciding, nobody is asked — and the summary says which of
// the two automatic paths is running, because only one of them has a policy a
// person may want to change.
func TestAdviceWhileAutoDecidingAsksNobody(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	advice, err := coord.AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if advice.RequiresHuman {
		t.Fatalf("a question AO is deciding for itself asked for a human: %+v", advice)
	}
	if advice.AutomaticAction != workflowcore.AutoActionResolveQuestion || !advice.AutomaticActionActive {
		t.Fatalf("automaticAction = %q active=%v, want resolve_question active",
			advice.AutomaticAction, advice.AutomaticActionActive)
	}
	if !strings.Contains(advice.Summary, "low-risk") {
		t.Fatalf("the summary does not say AO is taking a low-risk decision: %q", advice.Summary)
	}
	if advice.RecommendedAction != "" {
		t.Fatalf("AO recommended %q while deciding for itself", advice.RecommendedAction)
	}
}

// §13: and a question that is genuinely a person's still interrupts them.
func TestAdviceForANonAutoDecidableQuestionAsksTheHuman(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, destructivePaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyFullAutonomy)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	advice, err := coord.AdviceFor(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !advice.RequiresHuman {
		t.Fatalf("a destructive question did not interrupt anybody: %+v", advice)
	}
	if advice.ReasonCode != workflowcore.ReasonQuestionHumanRequired {
		t.Fatalf("reasonCode = %q, want question_human_required", advice.ReasonCode)
	}
	if advice.Explanation == "" {
		t.Fatal("the human was asked with nothing explaining what is needed")
	}
}

// failingSender refuses every write, the way a provider TUI guard does while
// its session is blocked awaiting the user.
type failingSender struct{ calls int }

func (f *failingSender) Send(_ context.Context, _ domain.SessionID, _ string, _ *ports.SpawnAttachment) error {
	f.calls++
	return errors.New("sessionguard: write suppressed while the session is blocked")
}

// P3-C: a delivery that cannot happen must not make the run unreadable.
//
// This ran on the READ path and returned its error, so a worker session that
// refused the write took the run's detail page, its advice AND its recovery
// assessment down with it — a 500 on every one, in exactly the state where a
// person most needs to read the run. Found by the P3-C closing smoke: a real
// blocked Claude worker suppressed the write, and GET /workflows/{id} answered
// 500 from then on.
func TestARefusedDeliveryDoesNotMakeTheRunUnreadable(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, _, _ := newQuestionsFixture(t, lowRiskTechnicalPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	setAutonomyMode(t, store, runID, domain.QuestionAutonomyAutoDecideLowRisk)
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	q := onlyQuestion(t, store, runID)
	completeResolutionFor(t, store, q, "Extract into utils.go", "sibling helpers live there", nil)

	// A coordinator whose worker session refuses every write.
	sender := &failingSender{}
	blocked := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: sessionFacts,
		QuestionsStore: store, MessageSender: sender,
	})
	if _, err := blocked.GetRun(ctx, runID); err != nil {
		t.Fatalf("a refused delivery made the run unreadable: %v", err)
	}
	if _, err := blocked.AdviceFor(ctx, runID); err != nil {
		t.Fatalf("a refused delivery made the advice unreadable: %v", err)
	}
	if sender.calls == 0 {
		t.Fatal("no delivery was attempted, so the refusal was never exercised")
	}

	// The answer is durable and still owed: nothing was lost, and the next
	// sweep retries it.
	after := onlyQuestion(t, store, runID)
	if after.State != domain.QuestionStateAnswered {
		t.Fatalf("state = %q, want answered", after.State)
	}
	if after.Delivered {
		t.Fatal("a refused delivery was recorded as delivered")
	}

	// And a sender that works delivers it, without the answer having changed.
	working := &fakeMessageSender{}
	recovered := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: sessionFacts,
		QuestionsStore: store, MessageSender: working,
	})
	if _, err := recovered.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun after recovery: %v", err)
	}
	if !onlyQuestion(t, store, runID).Delivered {
		t.Fatal("the retry did not deliver the pending answer")
	}
	if working.calls != 1 {
		t.Fatalf("the answer was delivered %d times, want exactly 1", working.calls)
	}
}
