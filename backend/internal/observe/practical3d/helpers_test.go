package practical3d

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testSeed = "2222222222222222222222222222222222222222222222222222222222222222"

func testManifest(t *testing.T) (Manifest, EnvironmentInputs, TechnicalArtifacts) {
	t.Helper()
	m, env, art, err := NewTechnicalFixture(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	return m, env, art
}

func measuredRole(task string) Role {
	if task == "C" {
		return RoleReviewer
	}
	return RoleWorker
}

// fixture configures one in-process run. Defaults produce 40 COMPLETED
// positions that satisfy every GO condition: in A-C ASSISTED makes one call
// and explores less; D is identical in both arms.
type fixture struct {
	m         Manifest
	env       EnvironmentInputs
	art       TechnicalArtifacts
	calls     func(Position) int
	request   func(Position, int) Representation
	respond   func(Position, int) (ProviderResponse, error)
	result    func(Position) ExecutionResult
	oracle    func(Position) OracleResult
	observe   func(ctx context.Context) (EnvironmentInputs, error)
	preflight func() ([]CellRepresentation, error)
	execute   func(context.Context, PositionContext, *ObservedClient) (ExecutionResult, error)
	now       func() time.Time
	mini      int
}

func newFixture(t *testing.T) *fixture {
	m, env, art := testManifest(t)
	f := &fixture{m: m, env: env, art: art}
	f.calls = func(p Position) int {
		if p.Arm == ArmAssisted && p.TaskID != "D" {
			return 1
		}
		return 2
	}
	f.request = func(p Position, _ int) Representation {
		return TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
	}
	f.respond = func(Position, int) (ProviderResponse, error) { return success(50, 10), nil }
	f.result = func(p Position) ExecutionResult {
		explore := int64(5)
		if p.Arm == ArmAssisted && p.TaskID != "D" {
			explore = 4
		}
		r := ExecutionResult{TerminalState: StateCompleted, ExplorationCalls: explore, DistinctFilesRead: explore, MilestoneObserved: true}
		if p.TaskID == "C" {
			r.Findings = []Finding{FindingFor(1, f.m.Q6Oracle.MandatoryDefects[0])}
		}
		return r
	}
	f.oracle = func(p Position) OracleResult { return goodOracle(f.m, p.TaskID) }
	f.observe = func(context.Context) (EnvironmentInputs, error) { return f.env, nil }
	f.preflight = func() ([]CellRepresentation, error) { return TechnicalPreflight(f.m, f.art.Attachment), nil }
	return f
}

func success(uncached, cached int64) ProviderResponse {
	in := uncached + cached
	return ProviderResponse{Output: json.RawMessage(`{"ok":true}`), Outcome: OutcomeSuccess, InputTokens: &in, CachedInputTokens: &cached, UncachedInputTokens: &uncached, ProviderMetadata: json.RawMessage(`{"account_ref_sha256":"` + sha256Hex([]byte("fixture-zero")) + `"}`), TerminalMetadata: json.RawMessage(`{}`)}
}

func outcome(o RequestOutcome, uncached int64) ProviderResponse {
	r := success(uncached, 0)
	r.Outcome = o
	return r
}

func goodOracle(m Manifest, task string) OracleResult {
	q4 := ""
	for _, x := range m.Q4Oracle.TaskOracles {
		if x.TaskID == task {
			q4 = x.HiddenTestManifestSHA256
		}
	}
	return OracleResult{Q1ExitCode: 0, Q1VerifyCommandSHA256: m.Q1Oracle.VerifyCommandSHA256, Q4Passed: true, Q4TaskOracleSHA256: q4, Q4CommandSHA256: m.Q4Oracle.CommandSHA256}
}

type fixtureTransport struct {
	f  *fixture
	mu sync.Mutex
	n  map[string]int
}

func (t *fixtureTransport) Do(_ context.Context, r TransportRequest) (ProviderResponse, error) {
	t.mu.Lock()
	t.n[r.Position.SampleID]++
	n := t.n[r.Position.SampleID]
	t.mu.Unlock()
	return t.f.respond(r.Position, n)
}

type fixtureExecutor struct{ f *fixture }

func (e fixtureExecutor) Preflight(context.Context, Manifest) ([]CellRepresentation, error) {
	return e.f.preflight()
}

func (e fixtureExecutor) Execute(ctx context.Context, pc PositionContext, c *ObservedClient) (ExecutionResult, error) {
	if e.f.execute != nil {
		return e.f.execute(ctx, pc, c)
	}
	p := pc.Position
	for i := 1; i <= e.f.calls(p); i++ {
		if _, err := c.Call(ProviderRequest{Role: measuredRole(p.TaskID), CallClass: CallInitial, Representation: e.f.request(p, i)}); err != nil {
			var ce *CallError
			if errors.As(err, &ce) {
				return ExecutionResult{TerminalState: StateFailedWorker}, nil
			}
			return ExecutionResult{}, err
		}
	}
	return e.f.result(p), nil
}

// fastWorkspaces skips the per-position process-table scan in unit tests;
// TestTeardownDetectsLeftoverPositionProcess covers the real scan.
type fastWorkspaces struct{}

func (fastWorkspaces) Prepare(ctx context.Context, m Manifest, p Position, w PositionWorkspace) error {
	return EmptyWorkspaceManager{}.Prepare(ctx, m, p, w)
}

func (fastWorkspaces) Finalize(context.Context, Manifest, Position, PositionWorkspace) error {
	return nil
}

type fixtureOracle struct{ f *fixture }

func (o fixtureOracle) Evaluate(_ context.Context, pc PositionContext, _ ExecutionResult) (OracleResult, error) {
	return o.f.oracle(pc.Position), nil
}

type countingObserver struct {
	f  *fixture
	mu sync.Mutex
	n  int
}

func (o *countingObserver) Observe(ctx context.Context) (EnvironmentInputs, error) {
	o.mu.Lock()
	o.n++
	o.mu.Unlock()
	return o.f.observe(ctx)
}

func (f *fixture) options(t *testing.T) RunnerOptions {
	t.Helper()
	artifacts := t.TempDir()
	if err := f.art.Write(artifacts); err != nil {
		t.Fatal(err)
	}
	o := RunnerOptions{
		Root:              filepath.Join(t.TempDir(), "run"),
		AllowExplicitTemp: true,
		Environment:       &countingObserver{f: f},
		Artifacts:         DirArtifactResolver{Root: artifacts},
		Transport:         &fixtureTransport{f: f, n: map[string]int{}},
		Executor:          fixtureExecutor{f: f},
		Oracle:            fixtureOracle{f: f},
		Workspaces:        fastWorkspaces{},
		Now:               f.now,
		Sleep:             func(context.Context, time.Duration) error { return nil },
	}
	if f.mini > 0 {
		o.MiniE2E, o.MiniPositions = true, f.mini
	} else {
		o.allowTechnicalFullRun = true
	}
	return o
}

// run executes the fixture and returns the result and the final ledger.
func (f *fixture) run(t *testing.T) (RunResult, []Event, error) {
	t.Helper()
	res, err := Run(context.Background(), f.m, f.options(t))
	if res.Root == "" {
		return res, nil, err
	}
	events, lerr := ReadLedger(filepath.Join(res.Root, "ledger.jsonl"))
	if lerr != nil {
		t.Fatal(lerr)
	}
	return res, events, err
}

func (f *fixture) mustRun(t *testing.T) (RunResult, []Event) {
	t.Helper()
	res, events, err := f.run(t)
	if err != nil {
		t.Fatal(err)
	}
	return res, events
}

// goLedger is a complete ledger whose decision is GO.
func goLedger(t *testing.T) (Manifest, []Event) {
	t.Helper()
	f := newFixture(t)
	res, events := f.mustRun(t)
	if res.Report.Verdict != "GO" {
		t.Fatalf("baseline is not GO: %s %s %+v", res.Report.ReasonCode, res.Report.Reason, firstError(res.Report))
	}
	return f.m, events
}

func firstError(r Report) string {
	for _, p := range r.Positions {
		if len(p.Errors) > 0 {
			return p.Position.TaskID + string(p.Position.Arm) + ": " + strings.Join(p.Errors, "; ")
		}
	}
	return strings.Join(r.LineageErrors, "; ")
}

func eventsFor(events []Event, sample string, typ EventType) []int {
	var out []int
	for i, e := range events {
		if e.SampleID == sample && e.Type == typ {
			out = append(out, i)
		}
	}
	return out
}

func positionBySample(r Report, s string) PositionReport {
	for _, p := range r.Positions {
		if p.Position.SampleID == s {
			return p
		}
	}
	return PositionReport{}
}

func firstPosition(m Manifest, task string, arm Arm) Position {
	for _, p := range m.Randomization.Schedule {
		if p.TaskID == task && p.Arm == arm {
			return p
		}
	}
	return Position{}
}

func cloneEvents(events []Event) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		raw, _ := json.Marshal(e)
		_ = json.Unmarshal(raw, &out[i])
	}
	return out
}

func without(events []Event, idx int) []Event {
	out := append([]Event{}, events[:idx]...)
	return append(out, events[idx+1:]...)
}

func insertAt(events []Event, idx int, e Event) []Event {
	out := append([]Event{}, events[:idx]...)
	out = append(out, e)
	return append(out, events[idx:]...)
}

func countStates(r Report) map[TerminalState]int {
	out := map[TerminalState]int{}
	for _, p := range r.Positions {
		out[p.State]++
	}
	return out
}
