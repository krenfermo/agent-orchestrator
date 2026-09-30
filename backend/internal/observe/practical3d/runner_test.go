package practical3d

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFortyCompletedWithThresholdsSatisfiedIsGO(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	res, events := f.mustRun(t)
	r := res.Report
	if r.Verdict != "GO" || r.ReasonCode != ReasonAllConditions || !r.LineageValid || !r.AllCompleted {
		t.Fatalf("verdict=%s %s: %s", r.Verdict, r.ReasonCode, firstError(r))
	}
	if len(r.Positions) != 40 || countStates(r)[StateCompleted] != 40 {
		t.Fatalf("positions=%d states=%v", len(r.Positions), countStates(r))
	}
	counts := map[EventType]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	if counts[EventManifest] != 1 || counts[EventPreflight] != 1 || counts[EventSampleStart] != 40 || counts[EventPositionTerminal] != 40 || counts[EventEnvironment] != 80 || counts[EventDecision] != 1 {
		t.Fatalf("event counts=%v", counts)
	}
	if counts[EventAttemptDispatched] != counts[EventAttemptFinalized] || counts[EventAttemptDispatched] != 5*(2+1)*3+10*2 {
		t.Fatalf("attempt counts=%v", counts)
	}
	// Re-deciding the stored ledger (with its DECISION line) is deterministic.
	again := Evaluate(f.m, events, r.GeneratedAt)
	if again.Verdict != "GO" || again.ReasonCode != r.ReasonCode {
		t.Fatalf("re-decide differs: %s %s", again.Verdict, again.Reason)
	}
	// Envelope identity and the frozen artifacts are on disk.
	env, m, err := ReadEnvelope(filepath.Join(res.Root, "envelope.json"))
	if err != nil || env.ExperimentID != r.ExperimentID || env.ManifestSHA256 != r.ExperimentID {
		t.Fatalf("envelope: %v", err)
	}
	if _, err := ExperimentID(m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Root, "artifacts", "sha256", f.m.TreatmentMapping[0].ASSISTED.AttachmentSHA256)); err != nil {
		t.Fatalf("attachment blob not frozen into the run: %v", err)
	}
	if !r.ResidualConfounder.Present || len(r.ResidualConfounder.ByPositionInOrder) != 40 || len(r.ResidualConfounder.ByTaskArm) != 8 {
		t.Fatalf("RESIDUAL_CONFOUNDER publication incomplete: %+v", r.ResidualConfounder)
	}
	p := r.Positions[0]
	if p.Diagnostics.CachedInputTokens == 0 || p.Diagnostics.M1Total != p.Diagnostics.CachedInputTokens+p.Diagnostics.UncachedInputTokens {
		t.Fatalf("diagnostic accounting: %+v", p.Diagnostics)
	}
	for _, x := range r.Positions {
		if x.Position.TaskID != "C" && x.Metrics.Q6.Applicable {
			t.Fatal("Q6 must be NA outside task C")
		}
	}
	raw, _ := json.Marshal(r.Positions[0].Metrics)
	if !strings.Contains(string(raw), `"Q6":"NA"`) && r.Positions[0].Position.TaskID != "C" {
		t.Fatalf("Q6 NA literal missing: %s", raw)
	}
}

func TestRetryAndPartialAccountingCountEveryAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "A", ArmOff)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.SampleID == target.SampleID && n == 1 {
			return outcome(OutcomeRetryable, 7), nil
		}
		return success(50, 10), nil
	}
	res, events := f.mustRun(t)
	pr := positionBySample(res.Report, target.SampleID)
	if pr.State != StateCompleted || pr.Metrics.M2 != 3 || pr.Metrics.M1U != 107 {
		t.Fatalf("retry accounting state=%s M2=%d M1u=%d errors=%v", pr.State, pr.Metrics.M2, pr.Metrics.M1U, pr.Errors)
	}
	var retry Event
	for _, i := range eventsFor(events, target.SampleID, EventAttemptDispatched) {
		if events[i].CallClass == CallRetry {
			retry = events[i]
		}
	}
	if retry.RetryIndex != 1 || retry.RetryCause != OutcomeRetryable || retry.CallIndex != 2 {
		t.Fatalf("retry dispatch metadata: %+v", retry)
	}
	if pr.Diagnostics.ProviderErrors[string(OutcomeRetryable)] != 1 {
		t.Fatalf("provider error not preserved: %v", pr.Diagnostics.ProviderErrors)
	}
}

func TestProviderFailuresKeepTheirEnumAndPreventGO(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		seq   []RequestOutcome
		state TerminalState
	}{
		{"retry exhausted", []RequestOutcome{OutcomeRetryable, OutcomeRetryable}, StateProviderRetryExhausted},
		{"rate limited", []RequestOutcome{OutcomeRateLimited, OutcomeRateLimited}, StateProviderRateLimited},
		{"policy", []RequestOutcome{OutcomePolicyFailure}, StateProviderPolicyFailure},
		{"partial stream terminal", []RequestOutcome{OutcomeTerminalFailure}, StateProviderTerminalFailure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			target := firstPosition(f.m, "B", ArmAssisted)
			f.respond = func(p Position, n int) (ProviderResponse, error) {
				if p.SampleID == target.SampleID && n <= len(tc.seq) {
					return outcome(tc.seq[n-1], 9), nil
				}
				return success(50, 10), nil
			}
			res, _ := f.mustRun(t)
			pr := positionBySample(res.Report, target.SampleID)
			if pr.State != tc.state || res.Report.Verdict != "NO_GO" || res.Report.ReasonCode != ReasonNotAllCompleted {
				t.Fatalf("state=%s verdict=%s errors=%v", pr.State, res.Report.Verdict, pr.Errors)
			}
			if pr.Metrics.M3 != 1 || pr.Metrics.Q1 || pr.Metrics.Q4 || pr.Metrics.M1U != 1000 || pr.Metrics.M2 != 10 {
				t.Fatalf("failure not imputed to caps/zero quality: %+v", pr.Metrics)
			}
			if pr.Diagnostics.Attempts != len(tc.seq) || pr.Diagnostics.UncachedInputTokens != int64(9*len(tc.seq)) {
				t.Fatalf("failed attempts must still be materialized for diagnostics: %+v", pr.Diagnostics)
			}
		})
	}
}

func TestSanctionStopsBatchAndBlocksRemainingPositions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := f.m.Randomization.Schedule[6]
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.SampleID == target.SampleID {
			return outcome(OutcomeSanction, 3), nil
		}
		return success(50, 10), nil
	}
	res, events := f.mustRun(t)
	states := countStates(res.Report)
	if states[StateProviderSanction] != 1 || states[StateBlocked] != 33 || states[StateCompleted] != 6 || res.Report.Verdict != "NO_GO" {
		t.Fatalf("states=%v verdict=%s lineage=%v", states, res.Report.Verdict, res.Report.LineageErrors)
	}
	if !res.Report.LineageValid {
		t.Fatalf("a recorded stop keeps lineage valid: %v", res.Report.LineageErrors)
	}
	for _, p := range res.Report.Positions[7:] {
		if p.Metrics.M3 != 1 || p.Metrics.Q1 || p.Metrics.Q4 {
			t.Fatal("blocked positions must be imputed")
		}
	}
	stops := 0
	for _, e := range events {
		if e.Type == EventBatchStop {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("BATCH_STOP count=%d", stops)
	}
}

func TestTreatmentViolationsAtRuntimeAreMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]func(f *fixture, target Position){
		"OFF with attachment": func(f *fixture, target Position) {
			f.request = func(p Position, i int) Representation {
				r := TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
				if p.SampleID == target.SampleID {
					r.ProjectMemoryAttachment = &Attachment{BytesBase64: encodeBase64(f.art.Attachment), SHA256: sha256Hex(f.art.Attachment), Version: "fixture-v1", Origin: "PROJECT_MEMORY"}
				}
				return r
			}
		},
		"OFF with attachment span in payload": func(f *fixture, target Position) {
			f.request = func(p Position, i int) Representation {
				r := TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
				if p.SampleID == target.SampleID {
					b, _ := json.Marshal(map[string]string{"prompt": "context: " + string(f.art.Attachment)})
					r.Payload = b
				}
				return r
			}
		},
		"external context true": func(f *fixture, target Position) {
			f.request = func(p Position, i int) Representation {
				r := TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
				r.ExternalContext = p.SampleID == target.SampleID
				return r
			}
		},
		"context source drift": func(f *fixture, target Position) {
			f.request = func(p Position, i int) Representation {
				r := TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
				if p.SampleID == target.SampleID {
					states := append([]ContextSource{}, r.ContextSourceStates...)
					states[4].State = "EQUALIZED"
					r.ContextSourceStates = states
				}
				return r
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			target := firstPosition(f.m, "A", ArmOff)
			mutate(f, target)
			res, events := f.mustRun(t)
			pr := positionBySample(res.Report, target.SampleID)
			if pr.State != StateMalformedResult || res.Report.Verdict != "NO_GO" {
				t.Fatalf("state=%s verdict=%s", pr.State, res.Report.Verdict)
			}
			if len(eventsFor(events, target.SampleID, EventAttemptDispatched)) != 0 {
				t.Fatal("a treatment violation reached the transport")
			}
		})
	}
	t.Run("ASSISTED wrong attachment", func(t *testing.T) {
		f := newFixture(t)
		target := firstPosition(f.m, "C", ArmAssisted)
		f.request = func(p Position, i int) Representation {
			r := TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, p.Arm, measuredRole(p.TaskID), CallInitial)
			if p.SampleID == target.SampleID {
				other := []byte("a different attachment")
				r.ProjectMemoryAttachment = &Attachment{BytesBase64: encodeBase64(other), SHA256: sha256Hex(other), Version: "fixture-v1", Origin: "PROJECT_MEMORY"}
			}
			return r
		}
		res, _ := f.mustRun(t)
		if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
			t.Fatal("wrong ASSISTED attachment accepted")
		}
	})
	t.Run("ASSISTED missing attachment", func(t *testing.T) {
		f := newFixture(t)
		target := firstPosition(f.m, "C", ArmAssisted)
		f.request = func(p Position, i int) Representation {
			return TechnicalRepresentation(f.m, f.art.Attachment, p.TaskID, ArmOff, measuredRole(p.TaskID), CallInitial)
		}
		res, _ := f.mustRun(t)
		if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
			t.Fatal("ASSISTED without attachment accepted")
		}
	})
}

func TestUnknownRoleUnmappedClassAndRetryCallsAreMalformed(t *testing.T) {
	t.Parallel()
	for name, req := range map[string]ProviderRequest{
		"unknown role":        {Role: "intruder", CallClass: CallInitial},
		"role outside flow":   {Role: RolePlanner, CallClass: CallInitial},
		"unmapped call class": {Role: RoleWorker, CallClass: CallSummary},
		"executor retry":      {Role: RoleWorker, CallClass: CallRetry},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			target := firstPosition(f.m, "A", ArmOff)
			f.execute = func(ctx context.Context, pc PositionContext, c *ObservedClient) (ExecutionResult, error) {
				if pc.Position.SampleID == target.SampleID {
					r := req
					r.Representation = f.request(pc.Position, 1)
					_, err := c.Call(r)
					return f.result(pc.Position), err
				}
				return fixtureExecutor{f: &fixture{m: f.m, art: f.art, calls: f.calls, request: f.request, result: f.result}}.Execute(ctx, pc, c)
			}
			res, _ := f.mustRun(t)
			if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
				t.Fatal("out-of-mapping request accepted")
			}
		})
	}
}

func TestRoleCapExceededIsMalformed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "D", ArmOff)
	f.calls = func(p Position) int {
		if p.SampleID == target.SampleID {
			return 11 // CALL_CAP_ROLE[D,worker] = 10
		}
		if p.Arm == ArmAssisted && p.TaskID != "D" {
			return 1
		}
		return 2
	}
	res, _ := f.mustRun(t)
	pr := positionBySample(res.Report, target.SampleID)
	if pr.State != StateMalformedResult || !strings.Contains(strings.Join(pr.Errors, ";"), "call cap") {
		t.Fatalf("cap bypass: state=%s errors=%v", pr.State, pr.Errors)
	}
	if pr.Metrics.M2 != 10 {
		t.Fatalf("cap violation must impute caps, got M2=%d", pr.Metrics.M2)
	}
}

func TestTokenCapIsPerRoleAndNotCompensated(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "A", ArmOff)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.SampleID == target.SampleID {
			return success(600, 0), nil // 2 calls × 600 > TOKEN_CAP_ROLE[A,worker]=1000
		}
		return success(50, 10), nil
	}
	res, _ := f.mustRun(t)
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
		t.Fatal("token cap exceeded but accepted")
	}
}

func TestMissingAccountingAtRuntimeIsMalformed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "A", ArmAssisted)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		r := success(50, 10)
		if p.SampleID == target.SampleID {
			r.UncachedInputTokens = nil
		}
		return r, nil
	}
	res, events := f.mustRun(t)
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
		t.Fatal("missing accounting accepted")
	}
	fin := events[eventsFor(events, target.SampleID, EventAttemptFinalized)[0]]
	if len(fin.MissingAccounting) != 1 || fin.MissingAccounting[0] != "uncached_input_tokens" {
		t.Fatalf("finalization does not record MISSING: %+v", fin.MissingAccounting)
	}
}

func TestTransportErrorIsFinalizedAndMalformed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "B", ArmOff)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.SampleID == target.SampleID {
			return ProviderResponse{}, errors.New("connection reset")
		}
		return success(50, 10), nil
	}
	res, events := f.mustRun(t)
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
		t.Fatal("transport error accepted")
	}
	if len(eventsFor(events, target.SampleID, EventAttemptFinalized)) != 1 {
		t.Fatal("failed attempt was not finalized")
	}
}

func TestExecutorCannotClaimRunnerOwnedStates(t *testing.T) {
	t.Parallel()
	for _, claimed := range []TerminalState{StateProviderSanction, StateTimeout, StateBlocked, "WHATEVER", ""} {
		t.Run(string(claimed), func(t *testing.T) {
			f := newFixture(t)
			target := firstPosition(f.m, "A", ArmOff)
			base := f.result
			f.result = func(p Position) ExecutionResult {
				r := base(p)
				if p.SampleID == target.SampleID {
					r.TerminalState = claimed
				}
				return r
			}
			res, _ := f.mustRun(t)
			if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
				t.Fatalf("executor-claimed %q accepted", claimed)
			}
		})
	}
}

func TestPositionDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.m.Deadlines.PositionSeconds = 1
	target := f.m.Randomization.Schedule[0]
	f.execute = func(ctx context.Context, pc PositionContext, c *ObservedClient) (ExecutionResult, error) {
		if pc.Position.SampleID == target.SampleID {
			<-ctx.Done()
			return ExecutionResult{TerminalState: StateCompleted}, nil
		}
		return fixtureExecutor{f: &fixture{m: f.m, art: f.art, calls: f.calls, request: f.request, result: f.result}}.Execute(ctx, pc, c)
	}
	f.mini = 1
	f.m.Provider.ProviderID = TechnicalFixtureProviderID
	res, _ := f.mustRun(t)
	if res.Report.Positions[0].State != StateTimeout {
		t.Fatalf("state=%s", res.Report.Positions[0].State)
	}
}

func TestRoleDeadlineIsTimeout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clock := time.Unix(1000, 0)
	f.now = func() time.Time { clock = clock.Add(15 * time.Second); return clock }
	res, _ := f.mustRun(t)
	// Every two-call position exceeds the 20s role deadline on its second call.
	pr := positionBySample(res.Report, firstPosition(f.m, "A", ArmOff).SampleID)
	if pr.State != StateTimeout {
		t.Fatalf("role deadline not enforced: %s %v", pr.State, pr.Errors)
	}
}

func TestEnvironmentDigestMismatch(t *testing.T) {
	t.Parallel()
	t.Run("before batch is PRESTART_INVALID", func(t *testing.T) {
		f := newFixture(t)
		f.observe = func(int) (EnvironmentInputs, error) {
			e := f.env
			e.TaskToolVersions = []VersionInput{{Component: "go", Version: "other", BinarySHA256: sha256Hex([]byte("go"))}}
			return e, nil
		}
		res, events, err := f.run(t)
		if !errors.Is(err, ErrPrestartInvalid) || res.Report.ReasonCode != ReasonPrestartInvalid {
			t.Fatalf("err=%v report=%+v", err, res.Report)
		}
		for _, e := range events {
			if e.Type == EventSampleStart {
				t.Fatal("a position started after a prestart failure")
			}
		}
		if got := Evaluate(f.m, events, time.Now()); got.ReasonCode != ReasonPrestartInvalid {
			t.Fatalf("decide over a prestart-invalid ledger: %s", got.ReasonCode)
		}
	})
	t.Run("after first SAMPLE_START is MALFORMED for that position", func(t *testing.T) {
		f := newFixture(t)
		f.observe = func(n int) (EnvironmentInputs, error) {
			e := f.env
			if n == 6 { // preflight=1, pos1 pre=2 post=3, pos2 pre=4 post=5, pos3 pre=6
				e.TaskToolVersions = []VersionInput{{Component: "go", Version: "other", BinarySHA256: sha256Hex([]byte("go"))}}
			}
			return e, nil
		}
		res, events := f.mustRun(t)
		p3 := res.Report.Positions[2]
		if p3.State != StateMalformedResult || res.Report.Verdict != "NO_GO" {
			t.Fatalf("state=%s", p3.State)
		}
		if len(eventsFor(events, p3.Position.SampleID, EventAttemptDispatched)) != 0 {
			t.Fatal("position with a divergent environment was executed")
		}
		if countStates(res.Report)[StateCompleted] != 39 {
			t.Fatalf("states=%v", countStates(res.Report))
		}
	})
}

func TestPrestartFailuresNeverStartTheBatch(t *testing.T) {
	cases := map[string]func(f *fixture, o *RunnerOptions){
		"missing preflight representation": func(f *fixture, o *RunnerOptions) {
			f.preflight = func() ([]CellRepresentation, error) { return TechnicalPreflight(f.m, f.art.Attachment)[1:], nil }
		},
		"invalid preflight representation": func(f *fixture, o *RunnerOptions) {
			f.preflight = func() ([]CellRepresentation, error) {
				reps := TechnicalPreflight(f.m, f.art.Attachment)
				for i := range reps {
					if reps[i].Arm == ArmOff {
						reps[i].Representation.ProjectMemoryAttachment = &Attachment{BytesBase64: encodeBase64(f.art.Attachment), SHA256: sha256Hex(f.art.Attachment), Version: "fixture-v1", Origin: "PROJECT_MEMORY"}
					}
				}
				return reps, nil
			}
		},
		"attachment blob differs": func(f *fixture, o *RunnerOptions) {
			dir := o.Artifacts.(DirArtifactResolver).Root
			p := filepath.Join(dir, f.art.AttachmentRef)
			_ = os.Chmod(p, 0o600)
			_ = os.WriteFile(p, []byte("tampered"), 0o600)
		},
		"oracle artifact missing": func(f *fixture, o *RunnerOptions) {
			_ = os.Remove(filepath.Join(o.Artifacts.(DirArtifactResolver).Root, "sha256", f.m.Q1Oracle.VerifyCommandSHA256))
		},
		"invalid manifest": func(f *fixture, o *RunnerOptions) { f.m.N = 4 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AO_DATA_DIR", t.TempDir())
			f := newFixture(t)
			o := f.options(t)
			mutate(f, &o)
			res, err := Run(context.Background(), f.m, o)
			if !errors.Is(err, ErrPrestartInvalid) && !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("err=%v", err)
			}
			if res.Root == "" {
				return
			}
			events, _ := ReadLedger(filepath.Join(res.Root, "ledger.jsonl"))
			for _, e := range events {
				if e.Type == EventSampleStart {
					t.Fatal("SAMPLE_START after prestart failure")
				}
			}
			if events[len(events)-1].Type != EventDecision || events[len(events)-2].Type != EventPrestartInvalid {
				t.Fatalf("PRESTART_INVALID not recorded: %+v", events)
			}
		})
	}
}

func TestNoRerunReplacementOrRootReuse(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	f := newFixture(t)
	o := f.options(t)
	if _, err := Run(context.Background(), f.m, o); err != nil {
		t.Fatal(err)
	}
	// Same manifest (same experiment_id), fresh sibling root: refused.
	o2 := f.options(t)
	o2.Root = filepath.Join(filepath.Dir(o.Root), "second")
	if _, err := Run(context.Background(), f.m, o2); !errors.Is(err, ErrExperimentRegistered) {
		t.Fatalf("rerun of a registered experiment_id: %v", err)
	}
	// Existing root: refused even for a different manifest.
	m2, _, _, _ := NewTechnicalFixture(strings.Repeat("44", 32))
	f.m = m2
	o3 := f.options(t)
	o3.Root = o.Root
	if _, err := Run(context.Background(), f.m, o3); err == nil {
		t.Fatal("existing run root reused")
	}
	entries, err := OpenRegistry(filepath.Dir(o.Root)).Entries()
	if err != nil || len(entries) != 2 || entries[0].Type != "REGISTERED" || entries[1].Type != "RESULT" {
		t.Fatalf("registry entries=%+v err=%v", entries, err)
	}
}

func TestProductionAODataDirAlwaysRejected(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	production := filepath.Join(home, ".ao", "data")
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", "")
	for _, root := range []string{production, filepath.Join(production, "exp"), filepath.Join(home, ".ao", "exp"), filepath.Join(home, ".ao", "scratch", "frente3")} {
		if _, err := ValidateRunRoot(root, false); err == nil {
			t.Fatalf("root %s accepted", root)
		}
	}
	if _, err := ValidateRunRoot(filepath.Join(t.TempDir(), "run"), false); err == nil {
		t.Fatal("temp root accepted without explicit permission")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(production, link); err == nil {
		if _, err := ValidateRunRoot(filepath.Join(link, "run"), true); err == nil {
			t.Fatal("symlink into production accepted")
		}
	}
	t.Setenv("AO_DATA_DIR", production)
	if _, err := ValidateRunRoot(filepath.Join(t.TempDir(), "run"), true); err == nil {
		t.Fatal("inherited production AO_DATA_DIR accepted")
	}
	t.Setenv("AO_DATA_DIR", "")
	t.Setenv("AO_RUN_FILE", filepath.Join(home, ".ao", "running.json"))
	if _, err := ValidateRunRoot(filepath.Join(t.TempDir(), "run"), true); err == nil {
		t.Fatal("inherited production AO_RUN_FILE accepted")
	}
	t.Setenv("AO_RUN_FILE", "")
	t.Setenv("AO_DATA_DIR", filepath.Join(home, ".agent-orchestrator", "x"))
	if _, err := ValidateRunRoot(filepath.Join(t.TempDir(), "run"), true); err == nil {
		t.Fatal("AO_DATA_DIR under the reserved legacy store accepted")
	}
	t.Setenv("AO_DATA_DIR", production)
	f := newFixture(t)
	o := f.options(t)
	t.Setenv("AO_DATA_DIR", production)
	if _, err := Run(context.Background(), f.m, o); !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("runner accepted production AO_DATA_DIR: %v", err)
	}
}

func TestOfficialAndTechnicalRunsCannotCross(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	f := newFixture(t)
	o := f.options(t)
	o.allowTechnicalFullRun = false
	if _, err := Run(context.Background(), f.m, o); !errors.Is(err, ErrPrestartInvalid) {
		t.Fatalf("technical manifest ran as official: %v", err)
	}
	f.m.Provider.ProviderID = "real-provider"
	o = f.options(t)
	o.allowTechnicalFullRun, o.MiniE2E, o.MiniPositions = false, true, 1
	if _, err := Run(context.Background(), f.m, o); !errors.Is(err, ErrPrestartInvalid) {
		t.Fatalf("official manifest ran as mini-E2E: %v", err)
	}
}

func TestClientRefusesCallsAfterPositionEnd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	var leaked *ObservedClient
	f.execute = func(ctx context.Context, pc PositionContext, c *ObservedClient) (ExecutionResult, error) {
		leaked = c
		return fixtureExecutor{f: &fixture{m: f.m, art: f.art, calls: f.calls, request: f.request, result: f.result}}.Execute(ctx, pc, c)
	}
	f.mini = 1
	f.mustRun(t)
	if _, err := leaked.Call(ProviderRequest{Role: RoleWorker, CallClass: CallInitial}); err == nil {
		t.Fatal("client accepted a call after the position ended")
	}
}

func TestMiniE2EExecutesOnlyRequestedPositions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.mini = 2
	res, events := f.mustRun(t)
	states := countStates(res.Report)
	if states[StateCompleted] != 2 || states[StateBlocked] != 38 || res.Report.Verdict != "NO_GO" {
		t.Fatalf("states=%v verdict=%s", states, res.Report.Verdict)
	}
	starts := 0
	for _, e := range events {
		if e.Type == EventSampleStart {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("SAMPLE_START=%d", starts)
	}
	entries, _ := OpenRegistry(filepath.Dir(res.Root)).Entries()
	if entries[0].Kind != "TECHNICAL_MINI_E2E" {
		t.Fatalf("registry kind=%s", entries[0].Kind)
	}
}

func TestTeardownDetectsLeftoverPositionProcess(t *testing.T) {
	root := t.TempDir()
	w := PositionWorkspace{Root: root, AODataDir: filepath.Join(root, "ao-data")}
	if err := VerifyNoPositionProcesses(context.Background(), w); err != nil {
		t.Fatalf("clean position reported a process: %v", err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := VerifyNoPositionProcesses(context.Background(), w); err == nil {
		t.Fatal("leftover position process not detected")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err := VerifyNoPositionProcesses(context.Background(), w); err != nil {
		t.Fatalf("after teardown: %v", err)
	}
}

func TestTeardownFailureIsMalformed(t *testing.T) {
	f := newFixture(t)
	t.Setenv("AO_DATA_DIR", t.TempDir())
	o := f.options(t)
	target := f.m.Randomization.Schedule[4]
	o.Workspaces = failingFinalize{sample: target.SampleID}
	res, err := Run(context.Background(), f.m, o)
	if err != nil {
		t.Fatal(err)
	}
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult || res.Report.Verdict != "NO_GO" {
		t.Fatal("unverified teardown accepted")
	}
}

type failingFinalize struct{ sample string }

func (f failingFinalize) Prepare(ctx context.Context, m Manifest, p Position, w PositionWorkspace) error {
	return EmptyWorkspaceManager{}.Prepare(ctx, m, p, w)
}

func (f failingFinalize) Finalize(_ context.Context, _ Manifest, p Position, _ PositionWorkspace) error {
	if p.SampleID == f.sample {
		return errors.New("process still running")
	}
	return nil
}

func TestRegistryIsGlobalBelowScratch(t *testing.T) {
	t.Parallel()
	scratch, err := ScratchRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Join(scratch, "a", "run"), filepath.Join(scratch, "b", "c", "run")} {
		reg, err := registryFor(root)
		if err != nil || reg.Path() != filepath.Join(scratch, "registry.jsonl") {
			t.Fatalf("registry for %s = %s (%v); a different parent must not hide a prior run", root, reg.Path(), err)
		}
	}
}
