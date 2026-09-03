package workflow_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p3c_dialog_delivery_test.go — P3-C's last blocker, at the seam where a
// durable answer becomes a keystroke.
//
// The fake below is not a stub: it implements the SAME select semantics the
// real Claude Code prompt has — a cursor that moves on Up/Down and an Enter
// that confirms whatever row the cursor is on. That is what makes these tests
// meaningful, because the failure this whole capability prevents is "Enter
// confirmed a different option", and only a fake that can actually confirm the
// wrong option can prove it does not.

// fakeSelectTUI is a terminal showing Claude's select prompt.
type fakeSelectTUI struct {
	mu       sync.Mutex
	prompt   string
	options  []string
	cursor   int
	answered int  // 1-based option confirmed, 0 while unanswered
	gone     bool // the prompt has been dismissed
	keys     []ports.DialogKey
	// swapAfterKeys replaces the prompt once this many keys have arrived, so a
	// test can move the dialog under a delivery in flight.
	swapAfterKeys int
	swapPrompt    string
	failEveryKey  bool // every keystroke errors, modelling a persistently refusing pane
	// unreadable renders a screen that still shows a prompt's furniture and no
	// parseable list — an unknown layout, or a repaint caught half-drawn. It is
	// what P3-D's third observation state exists for, and it must never be read
	// as an empty screen.
	unreadable bool
}

func newFakeSelectTUI() *fakeSelectTUI {
	return &fakeSelectTUI{
		prompt:  "What should the new helper file be named?",
		options: []string{"pathutil.go", "pathhelpers.go", "Type something.", "Chat about this"},
	}
}

// pane renders the prompt in the exact shape the real incident produced:
// numbered options each followed by an indented description line, with only the
// cursor row carrying the glyph.
func (f *fakeSelectTUI) pane() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gone {
		return "⏺ Working on it.\n> \n"
	}
	if f.unreadable {
		// Furniture, no list: the shape a layout AO does not understand leaves.
		return "⏺ I need to ask you a question before writing anything.\n" +
			"────────────────────────────────────────────\n" + f.prompt + "\n" +
			"❯ 1. \n" +
			"Enter to select · ↑/↓ to navigate · Esc to cancel"
	}
	out := "⏺ I need to ask you a question before writing anything.\n" +
		"────────────────────────────────────────────\n" + f.prompt + "\n"
	for i, o := range f.options {
		marker := "  "
		if i == f.cursor {
			marker = "❯ "
		}
		out += marker + strconv.Itoa(i+1) + ". " + o + "\n"
		out += "     Choose " + o + "\n"
	}
	return out + "Enter to select · ↑/↓ to navigate · Esc to cancel"
}

func (f *fakeSelectTUI) press(k ports.DialogKey) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, k)
	if f.failEveryKey {
		return errContext
	}
	if f.swapAfterKeys > 0 && len(f.keys) == f.swapAfterKeys {
		f.prompt = f.swapPrompt
		f.cursor = 0
	}
	switch k {
	case ports.KeyDown:
		if f.cursor < len(f.options)-1 {
			f.cursor++
		}
	case ports.KeyUp:
		if f.cursor > 0 {
			f.cursor--
		}
	case ports.KeyEnter:
		// The hazard, faithfully: Enter confirms the CURSOR, not an intent.
		f.answered = f.cursor + 1
		f.gone = true
	}
	return nil
}

func (f *fakeSelectTUI) confirmed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answered
}

func (f *fakeSelectTUI) pressed() []ports.DialogKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.DialogKey(nil), f.keys...)
}

var errContext = &deliveryError{"tmux refused the key"}

type deliveryError struct{ s string }

func (e *deliveryError) Error() string { return e.s }

// tuiRuntime is the pane reader and key sender over the fake TUI.
type tuiRuntime struct{ tui *fakeSelectTUI }

func (r tuiRuntime) GetOutput(_ context.Context, _ ports.RuntimeHandle, _ int) (string, error) {
	return r.tui.pane(), nil
}

func (r tuiRuntime) SendKeys(_ context.Context, _ ports.RuntimeHandle, keys []ports.DialogKey) error {
	for _, k := range keys {
		if err := r.tui.press(k); err != nil {
			return err
		}
	}
	return nil
}

// dialogFixture wires a coordinator whose worker session is blocked on the
// fake prompt, with one answered-but-undelivered question against it.
type dialogFixture struct {
	coord   *workflowcore.Coordinator
	store   *sqlite.Store
	tui     *fakeSelectTUI
	facts   *fakeSessionFacts
	session domain.SessionID
	runID   string
	stepID  string
	qID     string
	clk     *fakeClock
}

// setSessionActivity flips the worker session's observed activity, which is how
// a test distinguishes "the agent consumed the answer and is waiting again"
// from "the agent is gone".
func (f *dialogFixture) setSessionActivity(t *testing.T, state domain.ActivityState) {
	t.Helper()
	rec, ok, err := f.facts.GetSession(context.Background(), f.session)
	if err != nil || !ok {
		t.Fatalf("read session: ok=%v err=%v", ok, err)
	}
	rec.Activity = domain.Activity{State: state}
	f.facts.put(rec)
}

func newDialogFixture(t *testing.T, answerText, answerRef string, source domain.AnswerSource) *dialogFixture {
	t.Helper()
	ctx := context.Background()
	tui := newFakeSelectTUI()
	rt := tuiRuntime{tui: tui}

	store := sqlitetest.MustOpen(t)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	facts := newFakeSessionFacts()
	clk := &fakeClock{t: time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)}
	coord := workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store, SessionFacts: facts,
		QuestionsStore: store, PaneReader: rt, DialogKeys: rt,
		Clock: clk.Now,
	})
	runID, stepID, sessionID := seedRunningWorkStep(ctx, t, coord, store, facts, domain.ActivityBlocked)

	stepIDVal := domain.WorkflowStepID(stepID)
	q, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID: "wfq-dialog", WorkflowRunID: domain.WorkflowRunID(runID), WorkflowStepID: &stepIDVal,
		SessionID: &sessionID, AskingHarness: domain.HarnessClaudeCode,
		Fingerprint: "fp-dialog", QuestionText: tui.prompt,
		Certainty: domain.QuestionCertaintyActual, Classification: domain.QuestionClassificationAutoResolvable,
		State: domain.QuestionStateResolving, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	ok, err := store.AnswerWorkflowQuestion(ctx, string(q.ID),
		domain.QuestionStateResolving, domain.QuestionStateAnswered,
		source, answerText, answerRef, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("answer question: ok=%v err=%v", ok, err)
	}
	return &dialogFixture{coord: coord, store: store, tui: tui, facts: facts,
		session: sessionID, runID: runID, stepID: stepID, qID: string(q.ID), clk: clk}
}

func (f *dialogFixture) question(t *testing.T) domain.WorkflowQuestion {
	t.Helper()
	return f.questionByID(t, f.qID)
}

func (f *dialogFixture) questionByID(t *testing.T, id string) domain.WorkflowQuestion {
	t.Helper()
	qs, err := f.store.ListWorkflowQuestionsByRun(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("read questions: %v", err)
	}
	for _, q := range qs {
		if string(q.ID) == id {
			return q
		}
	}
	t.Fatalf("question %s not found among %d rows", id, len(qs))
	return domain.WorkflowQuestion{}
}

// parkOn stops the run on one canonical reason, exactly the way a stop site
// does: a checkpoint whose durable_phase IS the reason, and the run in
// needs_attention.
func (f *dialogFixture) parkOn(t *testing.T, reason string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-park-" + reason, WorkflowRunID: f.runID, ProjectID: "p",
		DurablePhase: reason, PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed stop checkpoint: %v", err)
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("read run: ok=%v err=%v", ok, err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		return
	}
	if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID, run.State,
		domain.WorkflowRunNeedsAttention, time.Now().UTC()); err != nil {
		t.Fatalf("park run: %v", err)
	}
}

// workStep reads the run's work step back.
func (f *dialogFixture) workStep(t *testing.T) domain.WorkflowStep {
	t.Helper()
	steps, err := f.store.ListWorkflowSteps(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("list steps: %v", err)
	}
	for _, s := range steps {
		if s.ID == f.stepID {
			return s
		}
	}
	t.Fatalf("work step %s not found", f.stepID)
	return domain.WorkflowStep{}
}

// hasStopPhase reports whether the run's ledger carries a checkpoint whose
// durable phase is this stop reason -- which is how every stop site in the
// package records one.
func (f *dialogFixture) hasStopPhase(t *testing.T, phase string) bool {
	t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(context.Background(), f.runID)
	if err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			return true
		}
	}
	return false
}

// parkWorkStep moves the work step out of `running` the way
// blockedOnHumanDecision does when a worker stops on a prompt.
func (f *dialogFixture) parkWorkStep(t *testing.T) {
	t.Helper()
	if _, err := f.store.UpdateWorkflowStepState(context.Background(), f.stepID,
		domain.WorkflowStepRunning, domain.WorkflowStepWaiting, time.Now().UTC()); err != nil {
		t.Fatalf("park work step: %v", err)
	}
}

// seedUndeliveredAnswer adds a second answered-but-undelivered row against the
// same blocked session, which is exactly what a crash between the keystrokes
// and the receipt leaves behind.
func (f *dialogFixture) seedUndeliveredAnswer(t *testing.T, id, answerText, answerRef string) {
	t.Helper()
	ctx := context.Background()
	base := f.question(t)
	q, _, err := f.store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID: domain.WorkflowQuestionID(id), WorkflowRunID: base.WorkflowRunID,
		WorkflowStepID: base.WorkflowStepID, SessionID: base.SessionID,
		AskingHarness: domain.HarnessClaudeCode, Fingerprint: "fp-" + id,
		QuestionText: base.QuestionText, Certainty: domain.QuestionCertaintyActual,
		Classification: domain.QuestionClassificationAutoResolvable,
		State:          domain.QuestionStateResolving, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed crash-window question: %v", err)
	}
	ok, err := f.store.AnswerWorkflowQuestion(ctx, string(q.ID),
		domain.QuestionStateResolving, domain.QuestionStateAnswered,
		domain.AnswerSourceAutonomous, answerText, answerRef, time.Now().UTC())
	if err != nil || !ok {
		t.Fatalf("answer crash-window question: ok=%v err=%v", ok, err)
	}
}

// THE HEADLINE. A durable answer naming option 2 confirms option 2 — not the
// option the cursor happened to start on.
func TestStructuredDeliverySelectsTheExactOption(t *testing.T) {
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	if _, err := f.coord.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.tui.confirmed(); got != 2 {
		t.Fatalf("the agent confirmed option %d, want 2", got)
	}
	if !f.question(t).Delivered {
		t.Fatal("the answer was applied but never recorded as delivered")
	}
	// And it got there by moving, not by typing.
	for _, k := range f.tui.pressed() {
		if !k.Valid() {
			t.Fatalf("an invalid key reached the terminal: %q", k)
		}
	}
}

// A human answer travels the identical path (§7). The only thing that differs
// anywhere is the durable answer_source.
func TestHumanAndAutonomousAnswersUseTheSameDelivery(t *testing.T) {
	for _, source := range []domain.AnswerSource{domain.AnswerSourceHuman, domain.AnswerSourceAutonomous} {
		t.Run(string(source), func(t *testing.T) {
			f := newDialogFixture(t, "pathhelpers.go", "2", source)
			if _, err := f.coord.GetRun(context.Background(), f.runID); err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got := f.tui.confirmed(); got != 2 {
				t.Fatalf("%s answer confirmed option %d, want 2", source, got)
			}
			q := f.question(t)
			if !q.Delivered {
				t.Fatalf("%s answer was not recorded as delivered", source)
			}
			if q.AnswerSource == nil || *q.AnswerSource != source {
				t.Fatalf("answerSource = %v, want %s", q.AnswerSource, source)
			}
		})
	}
}

// A resolver's free-form answer names the LABEL and no id. It must still land
// on the right row (§6's label fallback).
func TestAnswerWithOnlyALabelStillSelectsTheRightOption(t *testing.T) {
	f := newDialogFixture(t, "pathhelpers.go", "", domain.AnswerSourceAutonomous)
	if _, err := f.coord.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.tui.confirmed(); got != 2 {
		t.Fatalf("confirmed option %d, want 2", got)
	}
}

// §5: a prompt that changed under the delivery is never answered with the old
// decision — and, critically, nothing is confirmed at all.
func TestAChangedDialogIsNeverAnsweredWithAnOldDecision(t *testing.T) {
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	// The prompt is replaced after the first navigation key.
	f.tui.mu.Lock()
	f.tui.swapAfterKeys = 1
	f.tui.swapPrompt = "Should I delete the production database?"
	f.tui.mu.Unlock()

	if _, err := f.coord.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.tui.confirmed(); got != 0 {
		t.Fatalf("AO confirmed option %d on a prompt that had changed underneath it", got)
	}
	if f.question(t).Delivered {
		t.Fatal("a refused delivery was recorded as delivered")
	}
}

// §12/§13 boundary B: the keys landed and the process died before the receipt.
// The next pass must NOT press anything again — the absent prompt is the proof
// the answer was consumed.
func TestARedeliveryAfterAnAnsweredPromptPressesNothing(t *testing.T) {
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	ctx := context.Background()
	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	before := len(f.tui.pressed())
	if f.tui.confirmed() != 2 {
		t.Fatalf("setup did not answer the prompt")
	}
	// The crash window, reproduced as the state it actually leaves behind: an
	// answered question against this session with no delivery receipt, and a
	// prompt that is already gone because the keys landed before the process
	// died. A redelivery must recognise that and press nothing.
	f.seedUndeliveredAnswer(t, "wfq-crash", "pathhelpers.go", "2")
	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun after the crash window: %v", err)
	}
	if got := len(f.tui.pressed()); got != before {
		t.Fatalf("the redelivery pressed %d extra keys; an absent prompt must be answered by nobody", got-before)
	}
	if !f.questionByID(t, "wfq-crash").Delivered {
		t.Fatal("the redelivery did not record the delivery it could prove had happened")
	}
	if f.tui.confirmed() != 2 {
		t.Fatalf("the redelivery changed the confirmed option to %d", f.tui.confirmed())
	}
}

// §13 boundary C: a key sequence that fails midway leaves the dialog
// NAVIGATED BUT UNCONFIRMED. Nothing is confirmed blind after a partial
// interaction.
func TestAPartialInteractionNeverConfirmsBlind(t *testing.T) {
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	f.tui.mu.Lock()
	f.tui.failEveryKey = true // the pane refuses every key, on every retry
	f.tui.mu.Unlock()

	if _, err := f.coord.GetRun(context.Background(), f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.tui.confirmed(); got != 0 {
		t.Fatalf("AO confirmed option %d after a failed keystroke", got)
	}
	if f.question(t).Delivered {
		t.Fatal("a failed interaction was recorded as delivered")
	}
}

// A provider AO cannot answer structurally is reported, never typed into.
func TestAnUnsupportedProviderIsNotTypedInto(t *testing.T) {
	if workflowcore.SupportsStructuredDialogResponse(domain.HarnessCodex) {
		t.Fatal("codex claims structured dialog support it has not demonstrated")
	}
	if !workflowcore.SupportsStructuredDialogResponse(domain.HarnessClaudeCode) {
		t.Fatal("claude-code lost its structured dialog support")
	}
}

// §17: while a decision is on its way to the agent, AO is still working — and
// says which of the two automatic waits it is in.
func TestAdviceWhileAnAnswerIsBeingDelivered(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
		Questions: []domain.WorkflowQuestion{{
			ID: "wfq-1", State: domain.QuestionStateAnswered, Delivered: false,
			AskingHarness: domain.HarnessClaudeCode,
			QuestionText:  "which helper?", AnswerText: "pathutil.go",
		}},
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.RequiresHuman {
		t.Fatalf("a decision in transit asked for a human: %+v", a)
	}
	if a.AutomaticAction != workflowcore.AutoActionDeliverQuestionResponse || !a.AutomaticActionActive {
		t.Fatalf("automaticAction = %q active=%v, want deliver_question_response active",
			a.AutomaticAction, a.AutomaticActionActive)
	}
	if !strings.Contains(a.Summary, "sending it to the agent") {
		t.Fatalf("summary does not say the decision is on its way: %q", a.Summary)
	}
}

// §15/§17: a provider whose prompts AO cannot answer must not read as "AO is
// handling this" forever. It becomes a person's, honestly.
func TestAdviceBecomesHumanWhenTheProviderCannotBeAnswered(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
		Questions: []domain.WorkflowQuestion{{
			ID: "wfq-1", State: domain.QuestionStateAnswered, Delivered: false,
			AskingHarness: domain.HarnessCodex,
			QuestionText:  "which helper?", AnswerText: "pathutil.go",
		}},
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if !a.RequiresHuman {
		t.Fatalf("an undeliverable decision still read as AO's work: %+v", a)
	}
	if a.AutomaticActionBlockedReason != "provider_cannot_be_answered" {
		t.Fatalf("blocked reason = %q, want provider_cannot_be_answered", a.AutomaticActionBlockedReason)
	}
}

// A delivered answer is not a pending one: the run goes back to reading as
// ordinary work.
func TestADeliveredAnswerIsNoLongerAWait(t *testing.T) {
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-1", State: domain.WorkflowRunRunning, CreatedAt: time.Now().Add(-time.Hour)},
		Steps: singleTaskSteps(domain.WorkflowStepRunning, domain.WorkflowStepPending,
			domain.WorkflowStepPending, domain.WorkflowStepPending),
		Questions: []domain.WorkflowQuestion{{
			ID: "wfq-1", State: domain.QuestionStateAnswered, Delivered: true,
			AskingHarness: domain.HarnessClaudeCode, AnswerText: "pathutil.go",
		}},
	}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.AutomaticAction != workflowcore.AutoActionNone {
		t.Fatalf("a delivered answer still reads as an automatic action: %q", a.AutomaticAction)
	}
	if a.Category != workflowcore.AdviceNoActionRequired {
		t.Fatalf("category = %q, want no_action_required", a.Category)
	}
}

// §12: an absent prompt proves delivery only while the agent is still there.
//
// The same observation — "no dialog on screen" — describes two opposite things:
// an agent that consumed the answer and moved on, and an agent that DIED
// holding the question. Recording the second as delivered files an answer
// nobody received, which is the same lie as marking delivery because keys were
// written. Found by the P3-C closing smoke, on a worker that exited first.
func TestAnAbsentPromptOnADeadSessionIsNotADelivery(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	// The prompt is gone and the session is no longer waiting for anybody.
	f.tui.mu.Lock()
	f.tui.gone = true
	f.tui.mu.Unlock()
	f.setSessionActivity(t, domain.ActivityExited)

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.question(t).Delivered {
		t.Fatal("an answer was recorded as delivered to a session that had already gone")
	}
	if f.tui.confirmed() != 0 {
		t.Fatal("something was confirmed on a dead session")
	}
}

// And the live counterpart: an absent prompt on a session still waiting IS the
// redelivery receipt.
func TestAnAbsentPromptOnALiveSessionIsADelivery(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	f.tui.mu.Lock()
	f.tui.gone = true
	f.tui.mu.Unlock()

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !f.question(t).Delivered {
		t.Fatal("a consumed answer on a live session was not recorded as delivered")
	}
}

// A run parked on worker_blocked is un-parked by the delivery that resolved it.
//
// worker_blocked is a human-owned stop and AO must never clear one by guessing.
// Here it does not guess: it answered that exact prompt and re-read the screen
// to confirm the question was gone. Leaving the run parked afterwards strands a
// worker that is already back at work — which the P3-C closing smoke observed,
// with the file the worker went on to write sitting in the repository while the
// run still read "needs a decision".
func TestDeliveryUnparksARunBlockedOnTheAnsweredPrompt(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	f.parkOn(t, workflowcore.ReasonWorkerBlocked)

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.tui.confirmed() != 2 {
		t.Fatalf("the prompt was not answered (confirmed %d)", f.tui.confirmed())
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("read run: ok=%v err=%v", ok, err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked on a prompt AO answered and watched disappear")
	}
}

// And a REFUSED delivery clears nothing. The un-park is licensed by the proof
// that AO answered the prompt and watched it disappear; without that proof the
// run stays exactly as parked as it was.
func TestARefusedDeliveryClearsNoStop(t *testing.T) {
	ctx := context.Background()
	// An answer naming an option the prompt does not offer: nothing to select,
	// so nothing is pressed and nothing is proven.
	f := newDialogFixture(t, "somethingelse.go", "9", domain.AnswerSourceAutonomous)
	f.parkOn(t, workflowcore.ReasonWorkerBlocked)

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.tui.confirmed() != 0 {
		t.Fatalf("an unmatchable answer still confirmed option %d", f.tui.confirmed())
	}
	if f.question(t).Delivered {
		t.Fatal("a refused delivery was recorded as delivered")
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("read run: ok=%v err=%v", ok, err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("state = %s: a refused delivery un-parked the run", run.State)
	}
}

// §15/§17: a pending delivery must never MASK the run's own stop.
//
// A worker that died holding the question leaves an answer that will never be
// delivered. Reporting "AO is sending the decision to the agent" about a session
// nobody is left to send it to hides the real reason the run stopped, which is
// the misreport the whole Advisor exists to end. Observed live in the P3-C
// closing smoke, on a run parked on worker_dispatch_ambiguous.
func TestAPendingDeliveryDoesNotMaskTheRunsOwnStop(t *testing.T) {
	detail := stoppedOn(workflowcore.ReasonWorkerDispatchAmbiguous)
	detail.Questions = []domain.WorkflowQuestion{{
		ID: "wfq-1", State: domain.QuestionStateAnswered, Delivered: false,
		AskingHarness: domain.HarnessClaudeCode, AnswerText: "pathutil.go",
	}}
	a := adviceFor(detail, directBranchPlacement(), workflowcore.RepairPlan{})
	if a.AutomaticAction == workflowcore.AutoActionDeliverQuestionResponse {
		t.Fatalf("a pending delivery masked the run's own stop: %+v", a)
	}
	if !a.RequiresHuman || a.ReasonCode != workflowcore.ReasonWorkerDispatchAmbiguous {
		t.Fatalf("the run's real stop was not reported: %+v", a)
	}
}

// P3-D smoke B: the same un-park, on the other delivery route.
//
// A delivery recorded because the prompt was ALREADY gone carries the same
// durable fact as one AO pressed the key for — this question's answer has been
// handed over — so it owes the same un-park. Until it did, a real run stranded
// exactly the way the P3-C smoke's did: the worker took the autonomous
// decision, implemented it, ran the acceptance checks and reported its turn
// finished, while the run still read "AO stopped and needs a decision" and no
// review was ever dispatched.
func TestADeliveryRecordedFromAnAbsentPromptAlsoUnparksTheRun(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	// The prompt is already gone before AO ever looks: the redelivery shape,
	// and the shape a parser that cannot read the screen produces.
	f.tui.mu.Lock()
	f.tui.gone = true
	f.tui.mu.Unlock()
	f.parkOn(t, workflowcore.ReasonWorkerBlocked)

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if f.tui.confirmed() != 0 {
		t.Fatalf("something was pressed on a screen with no prompt on it (confirmed %d)", f.tui.confirmed())
	}
	if !f.question(t).Delivered {
		t.Fatal("precondition: an absent prompt on a live session is recorded as delivered")
	}
	run, ok, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil || !ok {
		t.Fatalf("read run: ok=%v err=%v", ok, err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked on a prompt whose answer AO recorded as delivered")
	}
}

// P3-D smoke B, the other half: un-parking the RUN is not enough.
//
// blockedOnHumanDecision parks both the run and the step it was working on, and
// work observation runs exclusively over a step in `running`. A delivery that
// resumed the run and left the step at `waiting` therefore produced a run that
// reads as working and a worker nobody ever looks at again — which is where the
// real smoke ended, with the worker's change on disk and neither the heartbeat
// nor an explicit Continue able to reach it.
func TestDeliveryAlsoReturnsTheBlockedStepToRunning(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	f.parkOn(t, workflowcore.ReasonWorkerBlocked)
	f.parkWorkStep(t)

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got := f.workStep(t).State; got != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want running: an answered worker must be observable again", got)
	}
}

// P3-D smoke B: the answer delivered as TEXT also has to converge.
//
// The structured path un-parks inline because it holds the proof at the moment
// it acts. The text path does not: when it writes, the agent has not read the
// message yet. So its un-park is a separate observation on a later pass, and
// without one the run stayed parked on worker_blocked forever while its worker
// finished the task and went idle — which is where two real smoke runs ended,
// with the change on disk and no review ever dispatched.
//
// The proof required here is BOTH halves: the answer is delivered, and the
// session has left the needs-input state. A worker still sitting on a prompt has
// consumed nothing, and un-parking it would be AO deciding a question was
// resolved because it had written something at a screen.
func TestADeliveredTextAnswerUnparksOnceTheWorkerResumes(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	// The prompt is gone and the answer is already recorded as delivered: the
	// state the text path leaves behind.
	f.tui.mu.Lock()
	f.tui.gone = true
	f.tui.mu.Unlock()
	if _, err := f.store.MarkWorkflowQuestionDelivered(ctx, f.qID, time.Now().UTC()); err != nil {
		t.Fatalf("seed delivered: %v", err)
	}
	f.parkOn(t, workflowcore.ReasonWorkerBlocked)
	f.parkWorkStep(t)

	// While the worker is still waiting on input, nothing is cleared: the
	// answer may be sitting unread in its composer.
	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun (worker still blocked): %v", err)
	}
	run, _, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatal("a run was un-parked while its worker was still waiting on input")
	}

	// The worker consumes it and goes back to work. Now the stop is over.
	f.setSessionActivity(t, domain.ActivityIdle)
	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("GetRun (worker resumed): %v", err)
	}
	run, _, err = f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		t.Fatal("the run is still parked on a prompt whose answer was delivered and consumed")
	}
	if got := f.workStep(t).State; got != domain.WorkflowStepRunning {
		t.Fatalf("work step state = %q, want running: the worker must be observable again", got)
	}
}

// P3-D §14 — an unreadable prompt is retried, bounded, and then said out loud
// under its own name.
//
// The three properties, in order: the first unreadable observation changes
// nothing (a redraw resolves itself); the answer is never marked delivered
// (that is the whole point of separating this from `dialog_gone`); and once the
// window has passed the run stops on `provider_dialog_unreadable` rather than
// on `worker_blocked`, because the decision has already been made and sending
// somebody to make it again would be a lie about what is wrong.
func TestAnUnreadablePromptIsRetriedThenStopsUnderItsOwnName(t *testing.T) {
	ctx := context.Background()
	f := newDialogFixture(t, "pathhelpers.go", "2", domain.AnswerSourceAutonomous)
	// A screen AO cannot interpret: prompt furniture, no readable list.
	f.tui.mu.Lock()
	f.tui.unreadable = true
	f.tui.mu.Unlock()

	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("first GetRun: %v", err)
	}
	if f.question(t).Delivered {
		t.Fatal("an answer was recorded as delivered against a screen AO could not read")
	}
	if f.tui.confirmed() != 0 {
		t.Fatal("a key was pressed at a screen AO could not read")
	}
	// The run may well be parked on `worker_blocked` already -- its worker IS
	// sitting on a prompt. What must NOT be on the ledger yet is this file's own
	// stop: one unreadable observation is a redraw, and calling it a fault would
	// make every repaint an incident.
	if f.hasStopPhase(t, workflowcore.ReasonProviderDialogUnreadable) {
		t.Fatal("the first unreadable observation raised the stop; a redraw must be allowed to settle")
	}

	// Past the retry window, still unreadable.
	f.clk.t = f.clk.t.Add(10 * time.Minute)
	if _, err := f.coord.GetRun(ctx, f.runID); err != nil {
		t.Fatalf("second GetRun: %v", err)
	}
	if f.question(t).Delivered {
		t.Fatal("the answer was delivered after the window expired; nothing became readable")
	}
	run, _, err := f.store.GetWorkflowRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the retries are spent", run.State)
	}
	// Under its OWN name: not dialog_gone, and not worker_blocked. The decision
	// is already made, so a stop that tells somebody to go and decide it would
	// be describing a different problem than the one AO actually has.
	if !f.hasStopPhase(t, workflowcore.ReasonProviderDialogUnreadable) {
		t.Fatal("the run did not stop on provider_dialog_unreadable")
	}
}
