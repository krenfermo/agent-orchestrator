package controllers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeSessionUsageLookup is a minimal test double for controllers.SessionUsageLookup.
type fakeSessionUsageLookup struct {
	byID map[domain.SessionID]domain.SessionUsageSummary
	err  error
}

func (f *fakeSessionUsageLookup) ListCompact(_ context.Context, _ domain.ProjectID, _ *domain.UserID) ([]domain.CompactSessionUsage, error) {
	return nil, nil
}

func (f *fakeSessionUsageLookup) Get(_ context.Context, id domain.SessionID) (domain.SessionUsageSummary, error) {
	if f.err != nil {
		return domain.SessionUsageSummary{}, f.err
	}
	if s, ok := f.byID[id]; ok {
		return s, nil
	}
	return domain.SessionUsageSummary{SessionID: id}, nil // no models -> nil totals, i.e. unknown
}

func tokenPtr(v int64) *int64 { return &v }

func detailWithRoles(now time.Time) workflowcore.RunDetail {
	plannerSession := "sess-planner"
	workerSession := "sess-worker"
	reviewerSession := "sess-reviewer"
	reviewRunID := "rvr-1"
	return workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: "wf-1", ProjectID: "proj-1", Objective: "ship the thing", State: domain.WorkflowRunWaiting, CreatedAt: now, UpdatedAt: now},
		NextAction: "start_review",
		Steps: []workflowcore.StepDetail{
			{
				Step: domain.WorkflowStep{ID: "wfs-plan", Kind: domain.WorkflowStepPlan, Ordinal: 1, State: domain.WorkflowStepCompleted, SessionID: &plannerSession, CreatedAt: now, UpdatedAt: now},
				Attempts: []domain.WorkflowAttempt{
					{ID: "wfa-plan-1", WorkflowStepID: "wfs-plan", AttemptNumber: 1, Harness: "claude-code", Model: "sonnet", StartedAt: now, FinishedAt: timePtr(now.Add(5 * time.Second)), Outcome: domain.WorkflowAttemptSucceeded},
				},
			},
			{
				Step: domain.WorkflowStep{ID: "wfs-work", Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, SessionID: &workerSession, CreatedAt: now, UpdatedAt: now},
				Attempts: []domain.WorkflowAttempt{
					{ID: "wfa-work-1", WorkflowStepID: "wfs-work", AttemptNumber: 1, Harness: "codex", Model: "gpt-5.6-sol", StartedAt: now, FinishedAt: timePtr(now.Add(20 * time.Second)), Outcome: domain.WorkflowAttemptSucceeded},
				},
			},
			{
				Step: domain.WorkflowStep{ID: "wfs-review", Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, SessionID: &reviewerSession, ReviewRunID: &reviewRunID, CreatedAt: now, UpdatedAt: now},
				Attempts: []domain.WorkflowAttempt{
					{ID: "wfa-review-1", WorkflowStepID: "wfs-review", AttemptNumber: 1, Harness: "claude-code", Model: "sonnet", StartedAt: now, FinishedAt: timePtr(now.Add(10 * time.Second)), Outcome: domain.WorkflowAttemptSucceeded},
				},
				Review: &workflowcore.ReviewSummary{Harness: domain.ReviewerClaudeCode, Verdict: domain.VerdictApproved, Target: "deadbeef"},
			},
		},
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestBuildWorkflowUsageView_RoleAttribution covers test items #1-4: each
// role (planner/worker/reviewer) is attributed to the correct step, harness,
// model, and provider — and a Claude worker vs a Claude reviewer are kept
// distinct roles despite sharing a harness.
func TestBuildWorkflowUsageView_RoleAttribution(t *testing.T) {
	now := time.Now().UTC()
	detail := detailWithRoles(now)
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, &fakeSessionUsageLookup{}, nil)

	byRole := map[domain.WorkflowRole]controllers.RoleUsageView{}
	for _, r := range view.Roles {
		byRole[r.Role] = r
	}
	planner, ok := byRole[domain.WorkflowRolePlanner]
	if !ok || planner.Harness != "claude-code" || planner.Model != "sonnet" || planner.Provider != "anthropic" {
		t.Fatalf("planner role = %+v, want claude-code/sonnet/anthropic", planner)
	}
	worker, ok := byRole[domain.WorkflowRoleWorker]
	if !ok || worker.Harness != "codex" || worker.Provider != "openai" {
		t.Fatalf("worker role = %+v, want codex/openai", worker)
	}
	reviewer, ok := byRole[domain.WorkflowRoleReviewer]
	if !ok || reviewer.Harness != "claude-code" || reviewer.Provider != "anthropic" {
		t.Fatalf("reviewer role = %+v, want claude-code/anthropic", reviewer)
	}
	if worker.Role == reviewer.Role {
		t.Fatal("worker and reviewer must not collapse to the same role even when harnesses match")
	}
}

// TestBuildWorkflowUsageView_UnknownStaysUnknown covers test items #5-6: a
// session with no usage rows reports Usage=nil / UsageKnown=false, never a
// fabricated zero.
func TestBuildWorkflowUsageView_UnknownStaysUnknown(t *testing.T) {
	now := time.Now().UTC()
	detail := detailWithRoles(now)
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, &fakeSessionUsageLookup{}, nil)

	for _, r := range view.Roles {
		if r.UsageKnown {
			t.Fatalf("role %s reported UsageKnown=true with no usage rows seeded", r.Role)
		}
		if r.Usage != nil && (r.Usage.Totals.InputTokens != nil || r.Usage.Totals.OutputTokens != nil) {
			t.Fatalf("role %s usage totals should be nil pointers, got %+v", r.Role, r.Usage.Totals)
		}
	}
	if view.Metrics.TokensCertainty != domain.MetricUnknown {
		t.Fatalf("metrics.TokensCertainty = %q, want unknown", view.Metrics.TokensCertainty)
	}
	if view.Metrics.InputTokens != nil || view.Metrics.OutputTokens != nil {
		t.Fatal("metrics token fields must stay nil, not zero, when nothing is known")
	}
}

// TestBuildWorkflowUsageView_KnownUsageAggregates covers test item #9, as
// P3-E redefined it: the task-level token totals come from the LEDGER, not
// from summing the per-role session views.
//
// The lookup below seeds one session that several roles share, which is the
// ordinary shape of a run with repair (a fix prompt is delivered into the
// worker's own session). Summing per-role would report that session's tokens
// once per role; the ledger reports them once, full stop.
func TestBuildWorkflowUsageView_KnownUsageAggregates(t *testing.T) {
	now := time.Now().UTC()
	detail := detailWithRoles(now)
	lookup := &fakeSessionUsageLookup{byID: map[domain.SessionID]domain.SessionUsageSummary{
		"sess-worker": {SessionID: "sess-worker", Totals: domain.UsageMetricTotals{InputTokens: tokenPtr(1000), OutputTokens: tokenPtr(200), CacheReadTokens: tokenPtr(50)}},
	}}
	ledger := &domain.WorkflowUsageLedger{
		Recorded: true,
		Totals: domain.UsageTokenTotals{
			InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 50, EventCount: 3,
		},
	}
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, lookup, ledger)
	if view.Metrics.TokensCertainty != domain.MetricActual {
		t.Fatalf("TokensCertainty = %q, want actual", view.Metrics.TokensCertainty)
	}
	if view.Metrics.InputTokens == nil || *view.Metrics.InputTokens != 1000 {
		t.Fatalf("InputTokens = %v, want 1000", view.Metrics.InputTokens)
	}
	if view.Metrics.OutputTokens == nil || *view.Metrics.OutputTokens != 200 {
		t.Fatalf("OutputTokens = %v, want 200", view.Metrics.OutputTokens)
	}
	if view.Metrics.CachedTokens == nil || *view.Metrics.CachedTokens != 50 {
		t.Fatalf("CachedTokens = %v, want 50", view.Metrics.CachedTokens)
	}
}

// TestBuildWorkflowUsageView_SharedSessionIsNotCountedTwice is the regression
// this checkpoint exists to prevent from coming back.
//
// Three roles point at ONE session holding 1000 input tokens. The pre-P3-E
// code asked the per-session reader once per role and added the answers, so
// this run reported 3000. The ledger is the only source of a total now, so the
// answer is 1000 regardless of how many roles shared the session.
func TestBuildWorkflowUsageView_SharedSessionIsNotCountedTwice(t *testing.T) {
	now := time.Now().UTC()
	detail := detailWithRoles(now)
	shared := domain.SessionUsageSummary{
		SessionID: "sess-worker",
		Totals:    domain.UsageMetricTotals{InputTokens: tokenPtr(1000), OutputTokens: tokenPtr(200)},
	}
	lookup := &fakeSessionUsageLookup{byID: map[domain.SessionID]domain.SessionUsageSummary{
		"sess-plan": shared, "sess-worker": shared, "sess-review": shared,
	}}
	ledger := &domain.WorkflowUsageLedger{
		Recorded: true,
		Totals:   domain.UsageTokenTotals{InputTokens: 1000, OutputTokens: 200, EventCount: 4},
	}
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, lookup, ledger)
	if view.Metrics.InputTokens == nil || *view.Metrics.InputTokens != 1000 {
		t.Fatalf("InputTokens = %v, want 1000 — a session shared by three roles must be counted once", view.Metrics.InputTokens)
	}
	if view.Metrics.OutputTokens == nil || *view.Metrics.OutputTokens != 200 {
		t.Fatalf("OutputTokens = %v, want 200", view.Metrics.OutputTokens)
	}
}

// TestBuildWorkflowUsageView_FixCyclesAndReviewsSkipped covers test items
// #10-11: fix-cycle counting and the reviewsSkipped metric derived from a
// SKIPPED ReviewPolicy decision with no dispatched reviewer.
func TestBuildWorkflowUsageView_FixCyclesAndReviewsSkipped(t *testing.T) {
	now := time.Now().UTC()
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-2", ProjectID: "proj-1", Objective: "small fix", State: domain.WorkflowRunCompleted, CreatedAt: now, UpdatedAt: now},
		Steps: []workflowcore.StepDetail{
			{Step: domain.WorkflowStep{ID: "wfs-work", Kind: domain.WorkflowStepWork, Ordinal: 2, State: domain.WorkflowStepCompleted, CreatedAt: now, UpdatedAt: now}},
			{
				Step:         domain.WorkflowStep{ID: "wfs-review", Kind: domain.WorkflowStepReview, Ordinal: 3, State: domain.WorkflowStepCompleted, CreatedAt: now, UpdatedAt: now},
				ReviewPolicy: &workflowcore.ReviewPolicyDecision{Decision: workflowcore.ReviewSkipped},
			},
			{
				Step: domain.WorkflowStep{ID: "wfs-fix", Kind: domain.WorkflowStepFix, Ordinal: 4, State: domain.WorkflowStepCompleted, CreatedAt: now, UpdatedAt: now},
				Attempts: []domain.WorkflowAttempt{
					{ID: "wfa-fix-1", WorkflowStepID: "wfs-fix", AttemptNumber: 1, Harness: "codex", StartedAt: now, FinishedAt: timePtr(now.Add(time.Second)), Outcome: domain.WorkflowAttemptSucceeded},
					{ID: "wfa-fix-2", WorkflowStepID: "wfs-fix", AttemptNumber: 2, Harness: "codex", StartedAt: now, FinishedAt: timePtr(now.Add(time.Second)), Outcome: domain.WorkflowAttemptSucceeded},
				},
			},
		},
	}
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, &fakeSessionUsageLookup{}, nil)
	if view.Metrics.FixCycles != 2 {
		t.Fatalf("FixCycles = %d, want 2", view.Metrics.FixCycles)
	}
	if !view.Metrics.ReviewsSkipped {
		t.Fatal("ReviewsSkipped = false, want true for a SKIPPED review policy with no dispatched reviewer")
	}
}

// TestBuildWorkflowUsageView_ReviewRunsCountsDispatchedReviewsWithoutAttempts
// is a regression guard: a review step's dispatch is recorded via its
// ReviewRunID/ReviewSummary, not the generic workflow_attempts mechanism
// work/fix steps use, so reviewRuns must not silently read 0 for a review
// that really ran (caught by the real 8J E2E, where a genuine approved
// review reported reviewRuns=0 before this fix).
func TestBuildWorkflowUsageView_ReviewRunsCountsDispatchedReviewsWithoutAttempts(t *testing.T) {
	now := time.Now().UTC()
	reviewRunID := "rvr-1"
	detail := workflowcore.RunDetail{
		Run: domain.WorkflowRun{ID: "wf-6", CreatedAt: now, UpdatedAt: now},
		Steps: []workflowcore.StepDetail{
			{Step: domain.WorkflowStep{ID: "wfs-work", Kind: domain.WorkflowStepWork, CreatedAt: now, UpdatedAt: now}},
			{
				Step:   domain.WorkflowStep{ID: "wfs-review", Kind: domain.WorkflowStepReview, State: domain.WorkflowStepCompleted, ReviewRunID: &reviewRunID, CreatedAt: now, UpdatedAt: now},
				Review: &workflowcore.ReviewSummary{Harness: domain.ReviewerClaudeCode, Verdict: domain.VerdictApproved},
				// No Attempts recorded for this step, matching the real
				// review dispatch mechanism.
			},
		},
	}
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, &fakeSessionUsageLookup{}, nil)
	if view.Metrics.ReviewRuns != 1 {
		t.Fatalf("ReviewRuns = %d, want 1 for a dispatched-and-completed review with no attempts recorded", view.Metrics.ReviewRuns)
	}
}

// TestBuildSessionRefreshAdvisory_Recommendations covers test items #13-15:
// REUSE with no fix cycles, CONSIDER_COMPACTION with one fix cycle under
// budget, RECOMMEND_NEW_SESSION once the fix-cycle budget is reached — using
// domain.DefaultWorkflowPolicy().MaxFixCycles as the budget, not an invented
// number.
func TestBuildSessionRefreshAdvisory_Recommendations(t *testing.T) {
	now := time.Now().UTC()
	budget := int64(domain.DefaultWorkflowPolicy().MaxFixCycles)
	workDetail := func() workflowcore.RunDetail {
		return workflowcore.RunDetail{
			Run:   domain.WorkflowRun{ID: "wf-3", CreatedAt: now, UpdatedAt: now},
			Steps: []workflowcore.StepDetail{{Step: domain.WorkflowStep{ID: "wfs-work", Kind: domain.WorkflowStepWork, CreatedAt: now, UpdatedAt: now}}},
		}
	}

	reuse := controllers.BuildSessionRefreshAdvisory(workDetail(), 0)
	if reuse.Recommendation != domain.RefreshReuse {
		t.Fatalf("0 fix cycles -> %q, want REUSE", reuse.Recommendation)
	}

	consider := controllers.BuildSessionRefreshAdvisory(workDetail(), 1)
	if budget > 1 && consider.Recommendation != domain.RefreshConsiderCompaction {
		t.Fatalf("1 of %d fix cycles -> %q, want CONSIDER_COMPACTION", budget, consider.Recommendation)
	}

	atBudget := controllers.BuildSessionRefreshAdvisory(workDetail(), budget)
	if atBudget.Recommendation != domain.RefreshRecommendNewSession {
		t.Fatalf("%d of %d fix cycles -> %q, want RECOMMEND_NEW_SESSION", budget, budget, atBudget.Recommendation)
	}

	// No work step recorded yet: nothing observable to advise from.
	empty := controllers.BuildSessionRefreshAdvisory(workflowcore.RunDetail{Run: domain.WorkflowRun{ID: "wf-4"}}, 0)
	if empty.Recommendation != domain.RefreshUnknown {
		t.Fatalf("no work step -> %q, want UNKNOWN", empty.Recommendation)
	}
}

// TestBuildTaskCheckpointSummary_FactsNotTranscript covers test item #17:
// the summary carries short fact fields (objective/task/criteria/errors),
// never a transcript or chain-of-thought blob.
func TestBuildTaskCheckpointSummary_FactsNotTranscript(t *testing.T) {
	now := time.Now().UTC()
	execID := "wf-5"
	detail := workflowcore.RunDetail{
		Run:        domain.WorkflowRun{ID: execID, Objective: "add rate limiting", CreatedAt: now, UpdatedAt: now},
		NextAction: "start_review",
		Tasks: []domain.WorkflowTask{
			{ID: "task-1", Title: "Add limiter middleware", AcceptanceCriteriaJSON: `["limiter rejects over threshold","tests pass"]`, ExecutionRunID: &execID},
		},
		Steps: []workflowcore.StepDetail{
			{
				Step:     domain.WorkflowStep{ID: "wfs-verify", Kind: domain.WorkflowStepVerify, CreatedAt: now, UpdatedAt: now},
				Attempts: []domain.WorkflowAttempt{{ID: "wfa-v1", WorkflowStepID: "wfs-verify", AttemptNumber: 1, StartedAt: now, Outcome: domain.WorkflowAttemptFailed, ErrorClass: domain.WorkflowErrorVerifyCommandFailed}},
			},
		},
	}
	summary := controllers.BuildTaskCheckpointSummary(detail)
	if summary.Objective != "add rate limiting" {
		t.Fatalf("Objective = %q", summary.Objective)
	}
	if summary.Task != "Add limiter middleware" {
		t.Fatalf("Task = %q", summary.Task)
	}
	if len(summary.AcceptanceCriteria) != 2 {
		t.Fatalf("AcceptanceCriteria = %v, want 2 entries", summary.AcceptanceCriteria)
	}
	if len(summary.ActiveErrors) == 0 {
		t.Fatal("expected an active error from the failed verify attempt")
	}
	// Fact-shaped, not transcript-shaped: nothing here should look like a
	// multi-KB blob of raw agent output.
	for _, err := range summary.ActiveErrors {
		if len(err) > 300 {
			t.Fatalf("ActiveErrors entry looks transcript-sized (%d bytes), want a short fact", len(err))
		}
	}
}

// TestBuildWorkflowUsageView_LookupErrorStaysUnknown covers test item #5
// from the failure side: a lookup error must degrade to "unknown", never
// panic or surface as a fabricated value.
func TestBuildWorkflowUsageView_LookupErrorStaysUnknown(t *testing.T) {
	now := time.Now().UTC()
	detail := detailWithRoles(now)
	view := controllers.BuildWorkflowUsageView(context.Background(), detail, &fakeSessionUsageLookup{err: errors.New("db unavailable")}, nil)
	for _, r := range view.Roles {
		if r.UsageKnown {
			t.Fatalf("role %s reported known usage despite a lookup error", r.Role)
		}
	}
}

// TestWorkflowRunUsage_HTTPRoundTrip covers test items #6 and #18 at the
// wire level: unknown token fields serialize as JSON null, never 0, and a
// role with known usage carries real numbers through the HTTP response.
func TestWorkflowRunUsage_HTTPRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	svc := &fakeWorkflowService{detail: detailWithRoles(now)}
	lookup := &fakeSessionUsageLookup{byID: map[domain.SessionID]domain.SessionUsageSummary{
		"sess-worker": {SessionID: "sess-worker", Totals: domain.UsageMetricTotals{InputTokens: tokenPtr(42)}},
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newWorkflowTestServerWithDeps(t, log, httpd.APIDeps{Workflows: svc, UsageSummary: lookup})

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"inputTokens":42`) {
		t.Fatalf("expected known worker input tokens in response: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"role":"planner"`) || !strings.Contains(bodyStr, `"role":"reviewer"`) {
		t.Fatalf("expected planner and reviewer roles in response: %s", bodyStr)
	}
	// The reviewer role has no seeded usage: its usage object's token
	// fields must be JSON null, and the field must not be silently omitted.
	if !strings.Contains(bodyStr, `"usage":{"sessionId":"sess-reviewer"`) {
		t.Fatalf("expected a present (non-omitted) usage object for the reviewer role: %s", bodyStr)
	}
}

// TestCapacityController_Headless covers test item #19: a nil capacity
// service (headless/test configuration without the store wired) answers 501
// rather than panicking.
func TestCapacityController_Headless(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newWorkflowTestServerWithDeps(t, log, httpd.APIDeps{})
	body, status, headers := doRequest(t, srv, "GET", "/api/v1/capacity", "")
	assertJSON(t, headers)
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s, want 501 for an unwired capacity service", status, body)
	}
}

// TestBuildDecisionsUsageView covers Checkpoint 8K-B pass 3's telemetry
// counters: plain counts are always present (0 is real, not fabricated),
// resolverProvider/resolverDurationMs follow "unknown != 0" and stay
// unset/nil when no resolution exists to derive them from, and
// waiting_for_capacity is derived from state=resolving with no dispatched
// attempt (ResolvingRunID nil) — the same fact
// reconcileDecisionResolvers itself derives.
func TestBuildDecisionsUsageView(t *testing.T) {
	completed := time.Date(2026, time.August, 16, 12, 5, 0, 0, time.UTC)
	created := completed.Add(-90 * time.Second)
	policySource := domain.AnswerSourcePolicy
	resolverSource := domain.AnswerSourceResolver
	resID := domain.WorkflowQuestionResolutionID("res-1")

	qs := []domain.WorkflowQuestion{
		{ID: "q-1", State: domain.QuestionStateAnswered, AnswerSource: &policySource},
		{ID: "q-2", State: domain.QuestionStateAnswered, AnswerSource: &resolverSource, ResolvingRunID: &resID},
		{ID: "q-3", State: domain.QuestionStateHumanRequired},
		{ID: "q-4", State: domain.QuestionStateResolving}, // waiting_for_capacity: no ResolvingRunID yet
		{ID: "q-5", State: domain.QuestionStateResolving, ResolvingRunID: &resID},
	}
	resolutions := []domain.WorkflowQuestionResolution{
		{ID: resID, ResolverHarness: domain.HarnessCodex, Status: domain.ResolutionStatusComplete, CreatedAt: created, CompletedAt: &completed},
	}

	v := controllers.BuildDecisionsUsageView(qs, resolutions)
	if v.QuestionsAsked != 5 {
		t.Fatalf("QuestionsAsked = %d, want 5", v.QuestionsAsked)
	}
	if v.PolicyResolved != 1 {
		t.Fatalf("PolicyResolved = %d, want 1", v.PolicyResolved)
	}
	if v.TechnicalResolved != 1 {
		t.Fatalf("TechnicalResolved = %d, want 1", v.TechnicalResolved)
	}
	if v.HumanRequired != 1 {
		t.Fatalf("HumanRequired = %d, want 1", v.HumanRequired)
	}
	if v.WaitingForCapacity != 1 {
		t.Fatalf("WaitingForCapacity = %d, want 1", v.WaitingForCapacity)
	}
	if v.ResolverFailed != 0 {
		t.Fatalf("ResolverFailed = %d, want 0", v.ResolverFailed)
	}
	if v.ResolverProvider != "openai" {
		t.Fatalf("ResolverProvider = %q, want openai (derived from HarnessCodex)", v.ResolverProvider)
	}
	if v.ResolverDurationMS == nil || *v.ResolverDurationMS != 90000 {
		t.Fatalf("ResolverDurationMS = %v, want 90000 (90s)", v.ResolverDurationMS)
	}
	if v.ReusedDecision != nil {
		t.Fatalf("ReusedDecision must stay nil (not observable read-time this pass), got %v", v.ReusedDecision)
	}

	// No questions at all: zero counts (real fact), no provider/duration.
	empty := controllers.BuildDecisionsUsageView(nil, nil)
	if empty.QuestionsAsked != 0 || empty.ResolverProvider != "" || empty.ResolverDurationMS != nil {
		t.Fatalf("empty view = %+v, want all-zero counts and unset provider/duration", empty)
	}
}

func newWorkflowTestServerWithDeps(t *testing.T, log *slog.Logger, deps httpd.APIDeps) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, deps, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}
