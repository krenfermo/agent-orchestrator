package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// compaction_economics_test.go -- P7.2B2.
//
// THE TWO TESTS THAT MATTER MOST ARE THE FIRST TWO, and they are deliberately
// contradictory pairs: a SKIP verdict while the policy compacts, and a COMPACT
// verdict while the policy does not. Together they are the proof that the shadow
// gate is not wired to anything. Everything else here is about the record.

// fakeRateCard is the optional UsageRateCard port. `ok` false is a model no rate
// card covers, which is a first-class state and not a failure.
type fakeRateCard struct {
	view domain.ModelRateView
	ok   bool
}

func (f *fakeRateCard) RateView(modelID string) (domain.ModelRateView, bool) {
	if !f.ok {
		return domain.ModelRateView{}, false
	}
	view := f.view
	view.ModelID = modelID
	return view, true
}

// fakeCompactionObservations is the per-session compaction observation port.
type fakeCompactionObservations struct {
	accounting domain.CompactionAccounting
	err        error
}

func (f *fakeCompactionObservations) SessionCompactionAccounting(_ context.Context, _ domain.SessionID) (domain.CompactionAccounting, error) {
	return f.accounting, f.err
}

// fakeSummaryPriors is the cross-session summary prior, which production leaves
// unwired in P7.2B2. The tests wire it so the arithmetic can be exercised end to
// end; the absence of a production implementation is a data-availability fact,
// not an arithmetic one.
type fakeSummaryPriors struct {
	observations []domain.CompactionSummarySessionObservation
	err          error
}

func (f *fakeSummaryPriors) CompactionSummaryObservations(_ context.Context, _, _ string) ([]domain.CompactionSummarySessionObservation, error) {
	return f.observations, f.err
}

// shadowRates is the embedded catalog's Opus 5 row with both cache-write
// lifetimes, the same figures the pure domain tests use.
func shadowRates() domain.ModelRateView {
	return domain.ModelRateView{
		InputPerMTok: 5.00, OutputPerMTok: 25.00, CacheReadPerMTok: 0.50,
		CacheWrite5mPerMTok: 6.25, CacheWrite1hPerMTok: 10.00,
		Currency: "USD", Source: "anthropic-list-price", Version: "2026-09-12",
	}
}

// workerSeries builds a run's worker-side call series.
//
// baseCalls calls in cycle 0, all on one model, all creating cache at the
// ONE-HOUR lifetime -- which is what every session AO has ever metered actually
// did. The first call carries the stable prefix as its cache read; the last one
// carries the conversation the compaction would read.
func workerSeries(baseCalls int, prefix, lastContext int64, at time.Time) []domain.SessionCallObservation {
	out := make([]domain.SessionCallObservation, 0, baseCalls)
	for i := 0; i < baseCalls; i++ {
		tokens := domain.UsageTokenTotals{
			InputTokens:      4_000,
			CacheReadTokens:  3_500,
			CacheWriteTokens: 500,
			OutputTokens:     200,
			EventCount:       1,
			CacheCreation:    domain.CacheCreationSplit{Ephemeral1hTokens: 500},
		}
		switch i {
		case 0:
			tokens.CacheReadTokens = prefix
			tokens.InputTokens = prefix + 500
		case baseCalls - 1:
			tokens.InputTokens = lastContext
			tokens.CacheReadTokens = lastContext - 500
		}
		out = append(out, domain.SessionCallObservation{
			Role:       domain.WorkflowRoleWorker,
			Cycle:      0,
			ModelID:    "claude-opus-5",
			ObservedAt: at.Add(time.Duration(i) * time.Second),
			Tokens:     tokens,
		})
	}
	return out
}

type shadowFixture struct {
	c      *workflowcore.Coordinator
	store  *fakeStore
	sender *fakeCompactingSender
	runID  string
}

type shadowSetup struct {
	contextTokens     int64
	compactionEnabled bool
	series            func(at time.Time) []domain.SessionCallObservation
	rateCard          *fakeRateCard
	observations      *fakeCompactionObservations
	priors            *fakeSummaryPriors
	pricer            workflowcore.UsagePricer
	maxFixCycles      int
	// contextThreshold overrides the strategy profile's context-per-call line,
	// the same per-run lever the canary reached for when it froze 60,000. It is
	// how a test reaches a COMPACT decision on a conversation smaller than the
	// Task profile's 150,000 without pretending the conversation is bigger.
	contextThreshold int64
}

// newShadowFixture drives a run to a dispatched fix cycle with the shadow gate's
// ports installed. Mirrors newCompactionFixtureWithPolicy; it cannot reuse it
// because the shadow ports are constructor dependencies.
func newShadowFixture(t *testing.T, setup shadowSetup) shadowFixture {
	t.Helper()
	return newShadowFixtureWith(t, setup, nil)
}

// newShadowFixtureWith is newShadowFixture with a hook that reaches the fake
// store before the run is driven, so a failure can be injected into a path the
// fixture itself exercises.
func newShadowFixtureWith(t *testing.T, setup shadowSetup, configureStore func(*fakeStore)) shadowFixture {
	t.Helper()
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeCompactingSender{supported: true}

	contextFacts := &fakeContextFacts{}
	if setup.contextTokens > 0 {
		contextFacts.reading = domain.SessionContextReading{
			Observable: true, ProviderCalls: 118,
			LastContextTokens: setup.contextTokens, PeakContextTokens: setup.contextTokens,
		}
	}

	store := newFakeStore()
	if configureStore != nil {
		configureStore(store)
	}
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	deps := workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts,
		SessionContextFacts: contextFacts,
		WorkspaceFacts:      workspaceFacts, ReviewRuns: reviewRuns,
		ReviewerLauncher: launcher, MessageSender: sender, Clock: clk.Now,
		NewID:       func() string { idSeq++; return fmt.Sprintf("cid%d", idSeq) },
		UsagePricer: setup.pricer,
	}
	if setup.rateCard != nil {
		deps.UsageRateCard = setup.rateCard
	}
	if setup.observations != nil {
		deps.SessionCompactionObservations = setup.observations
	}
	if setup.priors != nil {
		deps.CompactionSummaryPriors = setup.priors
	}
	c := workflowcore.New(deps)

	ctx := context.Background()
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// The worker series has to exist BEFORE the fix cycle is dispatched: the
	// gate reads calls the decision could already have seen, and a series
	// installed afterwards is the future.
	if setup.series != nil {
		store.workerCalls[created.Run.ID] = setup.series(clk.Now().Add(-time.Hour))
	}
	policy := domain.DefaultWorkflowPolicy()
	policy.SessionCompactionEnabled = setup.compactionEnabled
	policy.Strategy = domain.ExecutionStrategySelection{Effective: domain.ExecutionStrategyTask}
	if setup.maxFixCycles > 0 {
		policy.MaxFixCycles = setup.maxFixCycles
	}
	if setup.contextThreshold > 0 {
		usage := policy.EffectiveUsageBudgetPolicy()
		usage.WorkflowContextPerCallWarnTokens = setup.contextThreshold
		policy.Usage = usage
	}
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	if _, err := store.UpdateWorkflowRunPolicySnapshot(ctx, created.Run.ID, string(snapshot), clk.Now()); err != nil {
		t.Fatalf("UpdateWorkflowRunPolicySnapshot: %v", err)
	}

	driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	return shadowFixture{c: c, store: store, sender: sender, runID: created.Run.ID}
}

// shadowVerdicts reads back every verdict recorded for a run.
func shadowVerdicts(t *testing.T, store *fakeStore, runID string) []domain.CompactionEconomicsRecord {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var out []domain.CompactionEconomicsRecord
	for _, cp := range cps {
		if cp.DurablePhase != workflowcore.ShadowCompactionVerdictPhaseForTest() {
			continue
		}
		rec, ok := workflowcore.DecodeShadowCompactionVerdictForTest(cp.RetryState)
		if !ok {
			t.Fatalf("a %s checkpoint did not decode: %q", cp.DurablePhase, cp.RetryState)
		}
		out = append(out, rec)
	}
	return out
}

// profitableSetup is the shape whose economics are strongly positive: a large
// conversation, a small post-compaction size, and a long base cycle to forecast
// from. It exists so a test can produce a COMPACT verdict on demand.
func profitableSetup(enabled bool) shadowSetup {
	boundaryAt := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	return shadowSetup{
		contextTokens:     300_000,
		compactionEnabled: enabled,
		series: func(at time.Time) []domain.SessionCallObservation {
			return workerSeries(118, 26_009, 293_224, at)
		},
		rateCard: &fakeRateCard{view: shadowRates(), ok: true},
		observations: &fakeCompactionObservations{accounting: domain.CompactionAccounting{
			Boundaries: []domain.CompactionBoundary{{PostTokens: 10_956, ObservedAt: &boundaryAt}},
		}},
		priors: &fakeSummaryPriors{observations: summaryPriorCohort()},
	}
}

// summaryPriorCohort is the minimum cohort the summary-cost rule accepts: THREE
// INDEPENDENT sessions, each observed strictly before the fixture's decision
// instant of 2026-08-13T12:00:00Z.
//
// Three is not decoration. One session is a point estimate wearing a statistic's
// name, and three boundaries of one session are still one sample -- they share a
// harness, a project and a task shape, and the quantity being estimated moves with
// all three. The first figure is the canary's own measured residual, 16,986 over 2
// compactions; the other two are plausible neighbours, and the estimator takes the
// conservative maximum of the three rather than their mean.
func summaryPriorCohort() []domain.CompactionSummarySessionObservation {
	at := func(d time.Duration) *time.Time {
		t := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC).Add(d)
		return &t
	}
	return []domain.CompactionSummarySessionObservation{
		{SessionID: "prior-session-1", Compactions: 2, UnattributedOutputTokens: 16_986, ObservedAt: at(-72 * time.Hour)},
		{SessionID: "prior-session-2", Compactions: 1, UnattributedOutputTokens: 7_400, ObservedAt: at(-48 * time.Hour)},
		{SessionID: "prior-session-3", Compactions: 1, UnattributedOutputTokens: 6_900, ObservedAt: at(-24 * time.Hour)},
	}
}

// THE FIRST HALF OF THE SEPARATION PROOF.
//
// The shadow gate says SKIP. The policy says COMPACT. The compaction HAPPENS.
//
// The verdict is recorded and the conversation is replaced anyway, because
// nothing reads the verdict. If this test ever fails by finding zero compaction
// requests, the shadow gate has become enforcement.
func TestShadowSkipDoesNotPreventACompactionThePolicyWants(t *testing.T) {
	setup := profitableSetup(true)
	// One change makes the economics negative: the session has made few calls,
	// so there is almost nothing to amortize into.
	setup.series = func(at time.Time) []domain.SessionCallObservation {
		return workerSeries(16, 37_379, 85_847, at)
	}
	boundaryAt := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	setup.observations = &fakeCompactionObservations{accounting: domain.CompactionAccounting{
		Boundaries: []domain.CompactionBoundary{{PostTokens: 13_240, ObservedAt: &boundaryAt}},
	}}
	setup.contextTokens = 85_847
	// The canary's own frozen override. Without it a conversation of 85,847 is
	// below the Task profile's 150,000 line and the lifecycle never reaches
	// COMPACT, so there would be nothing for the shadow gate to shadow.
	setup.contextThreshold = 60_000

	fx := newShadowFixture(t, setup)

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want exactly 1", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictSkip {
		t.Fatalf("verdict = %s/%s, want skip (the fixture is the canary's own losing shape)",
			verdicts[0].Verdict, verdicts[0].Reason)
	}
	// AND THE COMPACTION STILL HAPPENED.
	if fx.sender.compactCalls != 1 {
		t.Fatalf("compaction requests = %d, want 1: a SKIP verdict must not withhold a compaction the policy asked for",
			fx.sender.compactCalls)
	}
	if verdicts[0].WouldHaveActed {
		t.Error("wouldHaveActed must be false: this cohort is not enforcement")
	}
	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok || !decision.CompactionRequested {
		t.Error("the lifecycle audit must still record that the conversation was asked to compact")
	}
}

// THE SECOND HALF OF THE SEPARATION PROOF.
//
// The shadow gate says COMPACT. The policy says no. NOTHING COMPACTS.
//
// With the per-run knob off, maybeCompactBeforeFix refuses however positive the
// economics are. A COMPACT verdict is a recommendation nobody asked for and
// nobody acts on.
func TestShadowCompactDoesNotCauseACompactionThePolicyRefuses(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(false))

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want exactly 1", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictCompact {
		t.Fatalf("verdict = %s/%s, want compact on a strongly profitable shape",
			verdicts[0].Verdict, verdicts[0].Reason)
	}
	// AND NOTHING COMPACTED.
	if fx.sender.compactCalls != 0 {
		t.Fatalf("compaction requests = %d, want 0: a COMPACT verdict must not cause a compaction the policy refused",
			fx.sender.compactCalls)
	}
	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok {
		t.Fatal("no fix_worker lifecycle decision recorded")
	}
	if decision.CompactionRequested {
		t.Error("audit must not claim a compaction that was never asked for")
	}
	// The fix cycle was still delivered, whole.
	if fx.sender.calls != 1 {
		t.Fatalf("fix prompt deliveries = %d, want exactly 1", fx.sender.calls)
	}
}

// A record is written even with the policy knob OFF. That is how evidence
// accrues on real runs without spending anything.
func TestAShadowVerdictIsRecordedWithTheKnobOff(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(false))
	if got := len(shadowVerdicts(t, fx.store, fx.runID)); got != 1 {
		t.Fatalf("shadow verdicts with the knob off = %d, want 1", got)
	}
}

// Exactly once per cycle, surviving repeated re-entry. The cohort's every
// statistic depends on one record per decision: a duplicate double-counts and a
// missing one is visibly missing.
func TestAShadowVerdictIsRecordedAtMostOncePerCycle(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(true))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := fx.c.GetRun(ctx, fx.runID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	if got := len(shadowVerdicts(t, fx.store, fx.runID)); got != 1 {
		t.Fatalf("shadow verdicts after repeated polling = %d, want still 1", got)
	}
}

// The marker is versioned and cycle-terminated. Without the gate version a
// re-evaluation under new arithmetic would be mistaken for a duplicate; without
// the terminator cycle 1's marker is a prefix of cycle 10's payload.
func TestTheShadowVerdictMarkerIsVersionedAndNotAPrefix(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(true))
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var payload string
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.ShadowCompactionVerdictPhaseForTest() {
			payload = cp.RetryState
			if cp.PayloadVersion != domain.CompactionEconomicsPayloadVersion {
				t.Errorf("payloadVersion = %q, want %q", cp.PayloadVersion, domain.CompactionEconomicsPayloadVersion)
			}
			// Written at RUN level with the step inside the payload, so it
			// cannot shadow a fix_dispatched read for the same step at the same
			// simulated tick.
			if cp.WorkflowStepID != nil {
				t.Errorf("workflow_step_id = %v, want nil so this record cannot shadow a step-scoped read", *cp.WorkflowStepID)
			}
		}
	}
	if payload == "" {
		t.Fatal("no shadow verdict checkpoint found")
	}
	if !strings.Contains(payload, `"gate":"`+domain.CompactionEconomicsGateVersion+`"`) {
		t.Errorf("payload does not carry the gate version: %q", payload)
	}
	if !strings.Contains(payload, `"step":"`) || !strings.Contains(payload, `"cycle":`) {
		t.Errorf("payload does not carry the step and cycle the marker matches on: %q", payload)
	}
}

// Without a rate card the verdict is UNKNOWN and names the model as the reason.
// No cost is invented, and the fix cycle is delivered exactly as before.
func TestAnUnpricedModelRecordsUnknownAndChangesNothing(t *testing.T) {
	setup := profitableSetup(true)
	setup.rateCard = &fakeRateCard{ok: false}
	fx := newShadowFixture(t, setup)

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want 1", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictUnknown || verdicts[0].Reason != domain.CompactionReasonPricingUnknown {
		t.Errorf("verdict = %s/%s, want unknown/pricing_unknown", verdicts[0].Verdict, verdicts[0].Reason)
	}
	if verdicts[0].PricingStatus != domain.CompactionPricingUnpricedModel {
		t.Errorf("pricingStatus = %q, want unpriced_model", verdicts[0].PricingStatus)
	}
	if verdicts[0].EstimatedCompactionCostMicros != 0 || verdicts[0].EstimatedFutureSavingsMicros != 0 {
		t.Error("a cost was invented for a model with no rate")
	}
	// Unchanged behaviour: the policy said compact, so the compaction happened.
	if fx.sender.compactCalls != 1 {
		t.Errorf("compaction requests = %d, want 1", fx.sender.compactCalls)
	}
}

// Every shadow port absent is the production default for a coordinator built
// before P7.2B2. Nothing breaks and nothing is claimed.
func TestWithNoShadowPortsTheVerdictIsUnknownAndBehaviourIsUnchanged(t *testing.T) {
	setup := profitableSetup(true)
	setup.rateCard, setup.observations, setup.priors = nil, nil, nil
	fx := newShadowFixture(t, setup)

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want 1 (a verdict AO could not compute is still a fact about the cohort)", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictUnknown {
		t.Errorf("verdict = %s/%s, want unknown", verdicts[0].Verdict, verdicts[0].Reason)
	}
	if fx.sender.compactCalls != 1 || fx.sender.calls != 1 {
		t.Errorf("compactions=%d deliveries=%d, want 1 and 1", fx.sender.compactCalls, fx.sender.calls)
	}
}

// The summary prior is the one port production leaves unwired, so this is the
// verdict a live shadow deployment actually produces today. Pinned so that the
// day it changes, somebody has to say so.
func TestWithoutASummaryPriorTheVerdictIsSummaryCostUnknown(t *testing.T) {
	setup := profitableSetup(true)
	setup.priors = nil
	fx := newShadowFixture(t, setup)

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want 1", len(verdicts))
	}
	if verdicts[0].Reason != domain.CompactionReasonSummaryCostUnknown {
		t.Errorf("reason = %s, want summary_cost_unknown", verdicts[0].Reason)
	}
	// And everything shadow CAN calibrate without a compaction is still on the
	// record: pricing, the cache lifetime, the call forecast, the structure.
	if verdicts[0].PricingStatus != domain.CompactionPricingPriced {
		t.Errorf("pricingStatus = %q, want priced", verdicts[0].PricingStatus)
	}
	if verdicts[0].CacheWriteLifetimePriced != domain.CacheWriteLifetime1h {
		t.Errorf("cacheWriteLifetimePriced = %q, want 1h", verdicts[0].CacheWriteLifetimePriced)
	}
	if verdicts[0].EstimatedRemainingCalls <= 0 {
		t.Errorf("estimatedRemainingCalls = %d, want the forecast to be recorded anyway", verdicts[0].EstimatedRemainingCalls)
	}
	if verdicts[0].ContextBeforeTokens <= 0 || verdicts[0].StablePrefixTokens <= 0 {
		t.Error("the observed inputs must be recorded even when an estimate is missing")
	}
}

// A read that fails must not break a workflow. The gate records what it could
// and the fix cycle is delivered.
func TestAShadowReadFailureIsNonFatal(t *testing.T) {
	setup := profitableSetup(true)
	setup.observations = &fakeCompactionObservations{err: fmt.Errorf("boom")}
	setup.priors = &fakeSummaryPriors{err: fmt.Errorf("boom")}
	fx := newShadowFixture(t, setup)

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want 1", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictUnknown {
		t.Errorf("verdict = %s/%s, want unknown after a read failure", verdicts[0].Verdict, verdicts[0].Reason)
	}
	if fx.sender.calls != 1 {
		t.Errorf("fix prompt deliveries = %d, want 1; shadow telemetry must never cost a delivery", fx.sender.calls)
	}
	detail, err := fx.c.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.NextAction == "needs_attention" {
		t.Error("a shadow telemetry failure must never escalate a run to needs_attention")
	}
}

// THE DELIVERY IS BYTE-IDENTICAL WHATEVER THE VERDICT.
//
// Three configurations producing three different verdicts, and the prompt that
// reaches the session is the same bytes in all three.
func TestTheShadowVerdictChangesNothingAboutDelivery(t *testing.T) {
	configs := []struct {
		name  string
		setup shadowSetup
		want  domain.CompactionEconomicVerdict
	}{
		{"compact", profitableSetup(true), domain.CompactionVerdictCompact},
		{"unknown", func() shadowSetup { s := profitableSetup(true); s.rateCard = &fakeRateCard{ok: false}; return s }(),
			domain.CompactionVerdictUnknown},
		{"skip", func() shadowSetup {
			s := profitableSetup(true)
			s.series = func(at time.Time) []domain.SessionCallObservation {
				return workerSeries(16, 37_379, 85_847, at)
			}
			return s
		}(), domain.CompactionVerdictSkip},
		// NOTE: this one keeps contextTokens at 300,000 so the lifecycle still
		// reaches COMPACT; only the call series is the canary's, which is what
		// makes the economics negative.
	}
	var reference string
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			fx := newShadowFixture(t, cfg.setup)
			verdicts := shadowVerdicts(t, fx.store, fx.runID)
			if len(verdicts) != 1 {
				t.Fatalf("shadow verdicts = %d, want 1", len(verdicts))
			}
			if verdicts[0].Verdict != cfg.want {
				t.Fatalf("verdict = %s/%s, want %s", verdicts[0].Verdict, verdicts[0].Reason, cfg.want)
			}
			if fx.sender.calls != 1 {
				t.Fatalf("fix prompt deliveries = %d, want 1", fx.sender.calls)
			}
			if reference == "" {
				reference = fx.sender.lastMsg
				return
			}
			if fx.sender.lastMsg != reference {
				t.Errorf("the delivered fix prompt differs under a %s verdict; the shadow gate must not touch it", cfg.want)
			}
		})
	}
}

// The payload carries no content. Asserted against a run whose objective and
// findings are deliberately full of things that must never be persisted here.
func TestTheShadowVerdictPayloadCarriesNoContent(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(true))
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	var payload string
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.ShadowCompactionVerdictPhaseForTest() {
			payload = cp.RetryState
		}
	}
	if payload == "" {
		t.Fatal("no shadow verdict checkpoint found")
	}
	// The run's own objective, the fix prompt, the fact pack and the review
	// findings all exist in this fixture. None may appear.
	for _, forbidden := range []string{
		"ship the thing", "SessionContextPack", "Objective", "Acceptance", "findings",
		"/ws/wf", "ao/wf", compactionFocusFragment,
	} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("the shadow payload contains %q:\n%s", forbidden, payload)
		}
	}
	// And the decoded record's every enum is a closed value.
	rec, ok := workflowcore.DecodeShadowCompactionVerdictForTest(payload)
	if !ok {
		t.Fatal("the payload did not decode")
	}
	if !rec.Verdict.Valid() || !rec.Reason.Valid() || !rec.ContextAfterBasis.Valid() ||
		!rec.SummaryBasis.Valid() || !rec.RemainingCallsBasis.Valid() {
		t.Error("the payload carries a value outside a closed enum")
	}
}

// compactionFocusFragment is a distinctive phrase from the compaction focus text
// AO sends the harness. It is prompt text and must never reach a verdict.
const compactionFocusFragment = "still unresolved"

// A REUSE decision is not evaluated at all: there is no compaction to be
// economic about, and a record for one would pad every statistic computed over
// the cohort.
func TestNoVerdictIsRecordedWhenTheLifecycleDoesNotSayCompact(t *testing.T) {
	setup := profitableSetup(true)
	setup.contextTokens = 0 // unobserved -> REUSE
	fx := newShadowFixture(t, setup)

	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok {
		t.Fatal("no fix_worker lifecycle decision recorded")
	}
	if decision.Action == domain.LifecycleCompact {
		t.Fatalf("fixture precondition: wanted a non-compact decision, got %q", decision.Action)
	}
	if got := len(shadowVerdicts(t, fx.store, fx.runID)); got != 0 {
		t.Errorf("shadow verdicts = %d, want none for a %s decision", got, decision.Action)
	}
}

// --- failure injection -------------------------------------------------------
//
// The rule under test in every case below is the same: a shadow gate that breaks
// costs a data point and never a delivery, a compaction, or a run's state.

// The gate's ONE read fails. The verdict is UNKNOWN, and everything the run was
// going to do, it does.
func TestAFailedUsageReadCostsAVerdictAndNothingElse(t *testing.T) {
	setup := profitableSetup(true)
	fx := newShadowFixtureWith(t, setup, func(store *fakeStore) {
		store.workerCallsErr = fmt.Errorf("ledger unavailable")
	})

	verdicts := shadowVerdicts(t, fx.store, fx.runID)
	if len(verdicts) != 1 {
		t.Fatalf("shadow verdicts = %d, want 1: a verdict AO could not compute is still a fact", len(verdicts))
	}
	if verdicts[0].Verdict != domain.CompactionVerdictUnknown {
		t.Errorf("verdict = %s/%s, want unknown", verdicts[0].Verdict, verdicts[0].Reason)
	}
	if verdicts[0].EstimatedCompactionCostMicros != 0 {
		t.Error("a cost was computed from a failed read")
	}
	if fx.sender.compactCalls != 1 {
		t.Errorf("compaction requests = %d, want 1", fx.sender.compactCalls)
	}
	if fx.sender.calls != 1 {
		t.Errorf("fix prompt deliveries = %d, want 1", fx.sender.calls)
	}
}

// The verdict's own checkpoint write fails. Nothing is recorded, nothing else
// changes, and the run is NOT escalated.
func TestAFailedVerdictWriteIsNonFatalAndNeverEscalates(t *testing.T) {
	setup := profitableSetup(true)
	fx := newShadowFixtureWith(t, setup, func(store *fakeStore) {
		store.checkpointWriteErr = func(cp domain.WorkflowCheckpoint) error {
			if cp.DurablePhase == workflowcore.ShadowCompactionVerdictPhaseForTest() {
				return fmt.Errorf("disk full")
			}
			return nil
		}
	})

	if got := len(shadowVerdicts(t, fx.store, fx.runID)); got != 0 {
		t.Errorf("shadow verdicts = %d, want 0: the write failed", got)
	}
	// And every pre-existing behaviour is intact.
	if fx.sender.compactCalls != 1 {
		t.Errorf("compaction requests = %d, want 1: a lost shadow record must not cost a compaction", fx.sender.compactCalls)
	}
	if fx.sender.calls != 1 {
		t.Errorf("fix prompt deliveries = %d, want 1", fx.sender.calls)
	}
	decision, ok := fixLifecycleDecision(t, fx.store, fx.runID)
	if !ok || decision.Action != domain.LifecycleCompact {
		t.Error("the lifecycle decision must still be recorded and still be compact")
	}
	detail, err := fx.c.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.NextAction == "needs_attention" {
		t.Error("a failed shadow write must never escalate a run")
	}
}

// THE CLASSIFICATION, END TO END. A recorded verdict must not become the run's
// latest checkpoint phase: it records something ABOUT the run, and the lifecycle
// derivation reads that field.
func TestARecordedVerdictDoesNotBecomeTheRunsLatestPhase(t *testing.T) {
	fx := newShadowFixture(t, profitableSetup(true))
	if got := len(shadowVerdicts(t, fx.store, fx.runID)); got != 1 {
		t.Fatalf("shadow verdicts = %d, want 1", got)
	}
	detail, err := fx.c.GetRun(context.Background(), fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if detail.LatestCheckpointPhase == workflowcore.ShadowCompactionVerdictPhaseForTest() {
		t.Errorf("latestCheckpointPhase = %q: a verdict nobody reads renamed the run's last activity",
			detail.LatestCheckpointPhase)
	}
}
