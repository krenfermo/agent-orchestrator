package usage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type fakeExplorationStore struct {
	run          domain.WorkflowRun
	found        bool
	observations []store.RunToolObservation
	calls        []store.RunExplorationCall
	steps        []domain.WorkflowStep
	attempts     map[string][]domain.WorkflowAttempt
	checkpoints  []domain.WorkflowCheckpoint
	providers    []domain.ProviderAttempt
	reviews      map[string]domain.ReviewRun
}

func (f *fakeExplorationStore) ListRunToolObservations(context.Context, string) ([]store.RunToolObservation, error) {
	return f.observations, nil
}
func (f *fakeExplorationStore) ListRunExplorationCalls(context.Context, string) ([]store.RunExplorationCall, error) {
	return f.calls, nil
}
func (f *fakeExplorationStore) GetWorkflowRun(context.Context, string) (domain.WorkflowRun, bool, error) {
	return f.run, f.found, nil
}
func (f *fakeExplorationStore) ListWorkflowSteps(context.Context, string) ([]domain.WorkflowStep, error) {
	return f.steps, nil
}
func (f *fakeExplorationStore) ListWorkflowAttempts(_ context.Context, stepID string) ([]domain.WorkflowAttempt, error) {
	return f.attempts[stepID], nil
}
func (f *fakeExplorationStore) ListWorkflowCheckpoints(context.Context, string) ([]domain.WorkflowCheckpoint, error) {
	return f.checkpoints, nil
}
func (f *fakeExplorationStore) ListProviderAttemptsForRun(context.Context, string) ([]domain.ProviderAttempt, error) {
	return f.providers, nil
}
func (f *fakeExplorationStore) GetReviewRun(_ context.Context, id string) (domain.ReviewRun, bool, error) {
	r, ok := f.reviews[id]
	return r, ok, nil
}

var explBase = time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

func at(sec int) *time.Time {
	t := explBase.Add(time.Duration(sec) * time.Second)
	return &t
}

func i64(v int64) *int64 { return &v }

func worker(harness string) store.RunToolObservation {
	return store.RunToolObservation{
		Role: domain.WorkflowRoleWorker, Subject: domain.SessionSubject("s1"), Harness: harness,
		Origin: domain.OriginAgentExploration, PathScope: domain.ToolPathNone,
		AttributionBasis: domain.AttributionExact,
	}
}

func read(sec int, path string, bytes int64) store.RunToolObservation {
	o := worker("claude-code")
	o.Op, o.ToolName, o.ObservedAt, o.Ordinal = domain.ToolOpRead, "Read", at(sec), int64(sec)
	o.PathScope, o.Path, o.ResultBytes = domain.ToolPathProject, path, i64(bytes)
	return o
}

func op(sec int, kind domain.ToolOp, scope domain.ToolPathScope, path string) store.RunToolObservation {
	o := worker("claude-code")
	o.Op, o.ObservedAt, o.Ordinal, o.PathScope, o.Path = kind, at(sec), int64(sec), scope, path
	o.ResultBytes = i64(10)
	return o
}

func callAt(sec int, input, cached, output int64) store.RunExplorationCall {
	return store.RunExplorationCall{
		Role: domain.WorkflowRoleWorker, Subject: domain.SessionSubject("s1"), Harness: "claude-code",
		ModelID: "claude-opus-5", ObservedAt: at(sec),
		Tokens: domain.UsageTokenTotals{InputTokens: input, CacheReadTokens: cached, OutputTokens: output, EventCount: 1},
	}
}

func val(t *testing.T, name string, m domain.ExplorationMetric, want int64, basis domain.ExplorationBasis) {
	t.Helper()
	if m.Basis != basis || m.Value == nil || *m.Value != want {
		var got any = "nil"
		if m.Value != nil {
			got = *m.Value
		}
		t.Fatalf("%s = %v (%s), want %d (%s)", name, got, m.Basis, want, basis)
	}
}

func unavail(t *testing.T, name string, m domain.ExplorationMetric) {
	t.Helper()
	if m.Basis != domain.ExplorationUnavailable || m.Value != nil {
		t.Fatalf("%s = %v (%s), want unavailable with nil value", name, m.Value, m.Basis)
	}
}

func TestExplorationAggregatesOneClaudeAgent(t *testing.T) {
	prompt := worker("claude-code")
	prompt.Origin, prompt.Op, prompt.ResultBytes, prompt.ObservedAt = domain.OriginAOContext, domain.ToolOpPrompt, i64(400), at(0)
	injected := worker("claude-code")
	injected.Origin, injected.Op, injected.ResultBytes, injected.ObservedAt = domain.OriginHarnessContext, domain.ToolOpInjected, i64(600), at(0)
	secret := op(6, domain.ToolOpRead, domain.ToolPathSecret, "")
	secret.ResultBytes = nil
	f := &fakeExplorationStore{
		found: true,
		run:   domain.WorkflowRun{ID: "wf", ProjectID: "p1", State: domain.WorkflowRunCompleted, CreatedAt: explBase, CompletedAt: at(120)},
		observations: []store.RunToolObservation{
			prompt, injected,
			read(1, "a.go", 100),
			op(2, domain.ToolOpSearch, domain.ToolPathProject, "."),
			read(3, "a.go", 100), // duplicate read
			read(4, "b.go", 50),
			op(5, domain.ToolOpCommandExplore, domain.ToolPathNone, ""),
			secret,
			op(10, domain.ToolOpEdit, domain.ToolPathProject, "a.go"),
			read(11, "c.go", 20), // after the first edit
		},
		calls: []store.RunExplorationCall{
			callAt(0, 1000, 0, 10), callAt(5, 2000, 1500, 20), callAt(9, 3000, 2500, 30), callAt(12, 3500, 3000, 40),
		},
	}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Recorded || len(got.Agents) != 1 {
		t.Fatalf("recorded=%v agents=%d, want one recorded agent", got.Recorded, len(got.Agents))
	}
	a := got.Agents[0]
	val(t, "fileReads", a.FileReads, 5, domain.ExplorationObserved)
	val(t, "uniqueFilesRead", a.UniqueFilesRead, 3, domain.ExplorationObserved)
	val(t, "repeatedReads", a.RepeatedReads, 1, domain.ExplorationObserved)
	val(t, "searches", a.Searches, 1, domain.ExplorationObserved)
	val(t, "exploreCommands", a.ExploreCommands, 1, domain.ExplorationDerived)
	val(t, "explorationOps", a.ExplorationOps, 6, domain.ExplorationObserved)
	val(t, "opsBeforeFirstEdit", a.OpsBeforeFirstEdit, 6, domain.ExplorationObserved)
	val(t, "callsBeforeFirstEdit", a.CallsBeforeFirstEdit, 3, domain.ExplorationDerived)
	val(t, "edits", a.Edits, 1, domain.ExplorationObserved)
	val(t, "uniqueFilesEdited", a.UniqueFilesEdited, 1, domain.ExplorationObserved)
	val(t, "modelCalls", a.ModelCalls, 4, domain.ExplorationObserved)
	val(t, "inputTokens", a.InputTokens, 9500, domain.ExplorationObserved)
	val(t, "cachedInputTokens", a.CachedInputTokens, 7000, domain.ExplorationObserved)
	val(t, "firstCallInput", a.FirstCallInput, 1000, domain.ExplorationObserved)
	val(t, "repoBytes", a.RepoBytesObserved, 280, domain.ExplorationObserved)
	val(t, "unobservedResults", a.UnobservedResults, 1, domain.ExplorationObserved)
	val(t, "aoBytes", a.AOContextBytes, 400, domain.ExplorationDerived)
	val(t, "harnessBytes", a.HarnessContextBytes, 600, domain.ExplorationDerived)
	val(t, "activeSpan", a.ActiveSpanMS, 12000, domain.ExplorationDerived)
	if a.PathScopes[domain.ToolPathSecret] != 1 {
		t.Fatalf("secret reads must be counted, never named: %v", a.PathScopes)
	}
	if a.ExplorationRatio.Basis != domain.ExplorationDerived || a.ExplorationRatio.Value == nil {
		t.Fatalf("exploration ratio = %+v, want derived", a.ExplorationRatio)
	}
	if len(a.TopFiles) == 0 || a.TopFiles[0].Path != "a.go" || a.TopFiles[0].Reads != 2 {
		t.Fatalf("top files = %+v, want a.go read twice first", a.TopFiles)
	}
	val(t, "totals.fileReads", got.Totals.FileReads, 5, domain.ExplorationObserved)
	if !got.Quality.Completed {
		t.Fatal("quality must report the completed run")
	}
	val(t, "quality.duration", got.Quality.DurationMS, 120000, domain.ExplorationObserved)
}

func TestExplorationShellEditIsADerivedFirstEdit(t *testing.T) {
	// The real Claude fixture run did everything in Bash: explore, then
	// `sed -i`. The first edit exists, but only by inference.
	prompt := worker("claude-code")
	prompt.Origin, prompt.Op, prompt.ResultBytes, prompt.Ordinal = domain.OriginAOContext, domain.ToolOpPrompt, i64(4000), 0
	f := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf"},
		observations: []store.RunToolObservation{
			prompt,
			op(1, domain.ToolOpCommandExplore, domain.ToolPathNone, ""),
			op(2, domain.ToolOpCommandExplore, domain.ToolPathNone, ""),
			op(3, domain.ToolOpCommandEdit, domain.ToolPathNone, ""),
			op(4, domain.ToolOpCommandExplore, domain.ToolPathNone, ""),
		},
		calls: []store.RunExplorationCall{callAt(0, 40000, 0, 1), callAt(3, 41000, 39000, 1)}}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents[0]
	val(t, "edits", a.Edits, 0, domain.ExplorationObserved)
	val(t, "shellEdits", a.ShellEdits, 1, domain.ExplorationDerived)
	val(t, "unattributedCommands", a.UnattributedCommands, 4, domain.ExplorationObserved)
	if !strings.Contains(a.FileReads.Method, "LOWER BOUND") {
		t.Fatalf("file reads must be labelled a lower bound when shell commands explored: %q", a.FileReads.Method)
	}
	val(t, "opsBeforeFirstEdit", a.OpsBeforeFirstEdit, 2, domain.ExplorationDerived)
	val(t, "harnessTokensFirstCall", a.HarnessTokensFirstCall, 39000, domain.ExplorationDerived)
}

func codexObs(sec int, kind domain.ToolOp, tool string, scope domain.ToolPathScope, path string) store.RunToolObservation {
	o := op(sec, kind, scope, path)
	o.Harness, o.ToolName = "codex", tool
	if tool == "codex_parsed_cmd" || tool == "codex_file_change" {
		o.ResultBytes = nil
	}
	return o
}

func TestExplorationOlderCodexWithoutItemsIsUnavailable(t *testing.T) {
	c := callAt(1, 100, 50, 5)
	c.Harness = "codex"
	f := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf", State: domain.WorkflowRunRunning},
		calls: []store.RunExplorationCall{c}, observations: []store.RunToolObservation{
			codexObs(2, domain.ToolOpCommand, "exec", domain.ToolPathNone, ""),
		}}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents[0]
	for name, m := range map[string]domain.ExplorationMetric{
		"fileReads": a.FileReads, "uniqueFilesRead": a.UniqueFilesRead, "searches": a.Searches,
		"explorationOps": a.ExplorationOps, "repoBytes": a.RepoBytesObserved,
	} {
		unavail(t, name, m)
	}
	val(t, "toolCalls", a.ToolCalls, 1, domain.ExplorationObserved)
	val(t, "modelCalls", a.ModelCalls, 1, domain.ExplorationObserved)
	if a.ExplorationRatio.Basis != domain.ExplorationUnavailable || a.ExplorationRatio.Value != nil {
		t.Fatalf("codex ratio = %+v, want unavailable", a.ExplorationRatio)
	}
	unavail(t, "quality.duration", got.Quality.DurationMS)
}

func TestExplorationCurrentCodexParsedCommandsAreObservedNotExtraCalls(t *testing.T) {
	f := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf"},
		observations: []store.RunToolObservation{
			codexObs(1, domain.ToolOpCommand, "exec", domain.ToolPathNone, ""),
			codexObs(1, domain.ToolOpRead, "codex_parsed_cmd", domain.ToolPathProject, "internal/money/money.go"),
			codexObs(1, domain.ToolOpSearch, "codex_parsed_cmd", domain.ToolPathProject, "internal"),
			codexObs(2, domain.ToolOpCommand, "exec", domain.ToolPathNone, ""),
			codexObs(2, domain.ToolOpEdit, "codex_file_change", domain.ToolPathProject, "internal/money/money.go"),
			codexObs(3, domain.ToolOpCommand, "exec", domain.ToolPathNone, ""),
			codexObs(3, domain.ToolOpRead, "codex_parsed_cmd", domain.ToolPathProject, "internal/money/money.go"),
			codexObs(4, domain.ToolOpCommand, "exec", domain.ToolPathNone, ""),
			codexObs(4, domain.ToolOpCommand, "codex_parsed_cmd", domain.ToolPathNone, ""), // Codex could not parse it
		}}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents[0]
	val(t, "toolCalls", a.ToolCalls, 4, domain.ExplorationObserved) // items are not calls
	val(t, "commands", a.Commands, 4, domain.ExplorationObserved)   // nor are unparsed items extra commands
	val(t, "fileReads", a.FileReads, 2, domain.ExplorationObserved)
	val(t, "uniqueFilesRead", a.UniqueFilesRead, 1, domain.ExplorationObserved)
	val(t, "repeatedReads", a.RepeatedReads, 1, domain.ExplorationObserved)
	val(t, "searches", a.Searches, 1, domain.ExplorationObserved)
	val(t, "opsBeforeFirstEdit", a.OpsBeforeFirstEdit, 2, domain.ExplorationObserved)
	val(t, "uniqueFilesEdited", a.UniqueFilesEdited, 1, domain.ExplorationObserved)
	val(t, "unattributedCommands", a.UnattributedCommands, 1, domain.ExplorationObserved)
	unavail(t, "repoBytes", a.RepoBytesObserved)
}

func TestExplorationMissingProviderDataIsUnavailableNotZero(t *testing.T) {
	f := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf"},
		observations: []store.RunToolObservation{read(1, "a.go", 10)}}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	a := got.Agents[0]
	unavail(t, "modelCalls", a.ModelCalls)
	unavail(t, "inputTokens", a.InputTokens)
	unavail(t, "firstCallInput", a.FirstCallInput)
	unavail(t, "callsBeforeFirstEdit", a.CallsBeforeFirstEdit)

	empty := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf"}}
	got, err = NewExplorationReader(empty).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	if got.Recorded || len(got.Agents) != 0 {
		t.Fatalf("a run with no facts must be unrecorded, got %+v", got)
	}
	unavail(t, "totals.modelCalls", got.Totals.ModelCalls)

	_, err = NewExplorationReader(&fakeExplorationStore{}).WorkflowRun(context.Background(), "nope")
	if !errors.Is(err, ErrExplorationRunNotFound) {
		t.Fatalf("unknown run err = %v, want ErrExplorationRunNotFound", err)
	}
}

func TestExplorationSeparatesAgentsByRoleAndSubject(t *testing.T) {
	reviewer := read(20, "a.go", 5)
	reviewer.Role, reviewer.Subject = domain.WorkflowRoleReviewer, domain.RuntimePaneSubject("rr-1")
	fix := read(30, "a.go", 5)
	fix.Role, fix.Cycle = domain.WorkflowRoleFixWorker, 1
	f := &fakeExplorationStore{found: true, run: domain.WorkflowRun{ID: "wf"},
		observations: []store.RunToolObservation{read(1, "a.go", 5), reviewer, fix}}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 3 {
		t.Fatalf("agents = %d, want worker, reviewer and fix cycle 1 apart", len(got.Agents))
	}
	if got.Agents[0].Role != domain.WorkflowRoleWorker || got.Agents[1].Role != domain.WorkflowRoleReviewer || got.Agents[2].Cycle != 1 {
		t.Fatalf("agents out of order: %+v", got.Agents)
	}
	// Across agents the same path is read three times; the run total must
	// count it as one distinct file read three times.
	val(t, "totals.uniqueFilesRead", got.Totals.UniqueFilesRead, 1, domain.ExplorationObserved)
	val(t, "totals.fileReads", got.Totals.FileReads, 3, domain.ExplorationObserved)
}

func TestExplorationQualitySignals(t *testing.T) {
	rr := "rr-2"
	fixStep := "step-fix"
	f := &fakeExplorationStore{
		found: true,
		run:   domain.WorkflowRun{ID: "wf", State: domain.WorkflowRunFailed, CreatedAt: explBase},
		steps: []domain.WorkflowStep{{ID: "step-work", Kind: domain.WorkflowStepWork}, {ID: "step-review", Kind: domain.WorkflowStepReview, ReviewRunID: &rr}, {ID: fixStep, Kind: domain.WorkflowStepFix}},
		attempts: map[string][]domain.WorkflowAttempt{
			"step-work":   {{Outcome: domain.WorkflowAttemptFailed}, {Outcome: domain.WorkflowAttemptSucceeded}},
			"step-review": {{Outcome: domain.WorkflowAttemptSucceeded}},
		},
		checkpoints: []domain.WorkflowCheckpoint{
			{ID: "c1", DurablePhase: "verify_result", CreatedAt: explBase, RetryState: `{"passed":false,"checks":[{"passed":false},{"passed":true}]}`},
			{ID: "c2", DurablePhase: "verify_result", CreatedAt: explBase.Add(time.Minute), RetryState: `{"passed":true,"checks":[{"passed":true},{"passed":true},{"passed":true}]}`},
			{ID: "c3", DurablePhase: "fix_dispatched", WorkflowStepID: &fixStep, RetryState: `{"cycleNumber":1}`},
			{ID: "c4", DurablePhase: "fix_dispatched", WorkflowStepID: &fixStep, RetryState: `{"cycleNumber":1}`}, // replayed
			{ID: "c5", DurablePhase: "fix_dispatched", WorkflowStepID: &fixStep, RetryState: `{"cycleNumber":2}`},
			{ID: "c6", DurablePhase: "review_done", ReviewRunID: strPtr("rr-1")},
		},
		providers: []domain.ProviderAttempt{{Ordinal: 1}, {Ordinal: 2}},
		reviews: map[string]domain.ReviewRun{
			"rr-1": {ID: "rr-1", Verdict: domain.ReviewVerdict("changes_requested"), CreatedAt: explBase},
			"rr-2": {ID: "rr-2", Verdict: domain.ReviewVerdict("approved"), CreatedAt: explBase.Add(time.Hour)},
		},
	}
	got, err := NewExplorationReader(f).WorkflowRun(context.Background(), "wf")
	if err != nil {
		t.Fatal(err)
	}
	q := got.Quality
	if q.Completed || q.FinalState != domain.WorkflowRunFailed {
		t.Fatalf("final state = %q completed=%v", q.FinalState, q.Completed)
	}
	val(t, "attempts", q.Attempts, 3, domain.ExplorationObserved)
	val(t, "failedAttempts", q.FailedAttempts, 1, domain.ExplorationObserved)
	val(t, "retries", q.Retries, 1, domain.ExplorationObserved)
	val(t, "failovers", q.ProviderFailovers, 1, domain.ExplorationObserved)
	val(t, "verifyRuns", q.VerifyRuns, 2, domain.ExplorationObserved)
	if q.VerifyPassed == nil || !*q.VerifyPassed {
		t.Fatalf("verify passed = %v, want the LATEST result (true)", q.VerifyPassed)
	}
	val(t, "checksPassed", q.ChecksPassed, 3, domain.ExplorationObserved)
	val(t, "fixCycles", q.FixCycles, 2, domain.ExplorationObserved)
	val(t, "reviewRuns", q.ReviewRuns, 2, domain.ExplorationObserved)
	if q.FinalReviewVerdict != "approved" {
		t.Fatalf("final verdict = %q, want the latest review's", q.FinalReviewVerdict)
	}
}

func strPtr(s string) *string { return &s }
