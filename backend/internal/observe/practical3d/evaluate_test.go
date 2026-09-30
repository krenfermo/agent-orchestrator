package practical3d

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// mutateGO applies a ledger mutation to a GO ledger and decides it.
func mutateGO(t *testing.T, mutate func(m *Manifest, events []Event) []Event) (Manifest, Report) {
	t.Helper()
	m, events := goLedger(t)
	events = mutate(&m, cloneEvents(events))
	return m, Evaluate(m, events, time.Unix(3, 0))
}

func TestAttemptLifecycleAnomaliesAreMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]func(events []Event, s string) []Event{
		"dispatch without finalize": func(e []Event, s string) []Event { return without(e, eventsFor(e, s, EventAttemptFinalized)[0]) },
		"finalize without dispatch": func(e []Event, s string) []Event { return without(e, eventsFor(e, s, EventAttemptDispatched)[0]) },
		"duplicate dispatch": func(e []Event, s string) []Event {
			i := eventsFor(e, s, EventAttemptDispatched)[0]
			return insertAt(e, i+1, e[i])
		},
		"duplicate finalize": func(e []Event, s string) []Event {
			i := eventsFor(e, s, EventAttemptFinalized)[0]
			return insertAt(e, i+1, e[i])
		},
		"missing accounting (null)": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[0]].UncachedInputTokens = nil
			return e
		},
		"accounting recorded MISSING": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[0]].MissingAccounting = []string{"cached_input_tokens"}
			return e
		},
		"identity mismatch": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[0]].CallClass = CallRetry
			return e
		},
		"call_index gap": func(e []Event, s string) []Event {
			for _, i := range append(eventsFor(e, s, EventAttemptDispatched)[1:2], eventsFor(e, s, EventAttemptFinalized)[1:2]...) {
				e[i].CallIndex = 5
			}
			return e
		},
		"unknown role": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptDispatched)[0]].Role = "intruder"
			e[eventsFor(e, s, EventAttemptFinalized)[0]].Role = "intruder"
			return e
		},
		"model differs from mapping": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptDispatched)[0]].ModelVersion = "2"
			return e
		},
		"finalization after terminal": func(e []Event, s string) []Event {
			i := eventsFor(e, s, EventAttemptFinalized)[0]
			f := e[i]
			e = without(e, i)
			return insertAt(e, eventsFor(e, s, EventPositionTerminal)[0], f)
		},
		"missing PRE_TERMINAL observation": func(e []Event, s string) []Event {
			return without(e, eventsFor(e, s, EventEnvironment)[1])
		},
		"environment digest mismatch": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventEnvironment)[1]].ObservedDigest = sha256Hex([]byte("tool v2"))
			return e
		},
		"negative tokens": func(e []Event, s string) []Event {
			v := int64(-1)
			e[eventsFor(e, s, EventAttemptFinalized)[0]].InputTokens = &v
			return e
		},
		"cached+uncached != input": func(e []Event, s string) []Event {
			v := int64(999)
			e[eventsFor(e, s, EventAttemptFinalized)[0]].InputTokens = &v
			return e
		},
		"unknown outcome": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[0]].RequestOutcome = "MAYBE"
			return e
		},
		"missing provider metadata": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[0]].ProviderMetadata = nil
			return e
		},
		"forged retry without cause": func(e []Event, s string) []Event {
			for _, i := range append(eventsFor(e, s, EventAttemptDispatched)[1:2], eventsFor(e, s, EventAttemptFinalized)[1:2]...) {
				e[i].CallClass = CallRetry
				e[i].RetryIndex = 1
				e[i].RetryCause = OutcomeRetryable
				e[i].RetryChainID = e[eventsFor(e, s, EventAttemptDispatched)[0]].RetryChainID
			}
			return e
		},
		"COMPLETED with a failed final attempt": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventAttemptFinalized)[1]].RequestOutcome = OutcomeTerminalFailure
			return e
		},
		"Q1 null": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventPositionResult)[0]].Result.Q1 = nil
			return e
		},
		"milestone lost": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventPositionResult)[0]].Result.MilestoneObserved = false
			return e
		},
		"negative exploration": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventPositionResult)[0]].Result.ExplorationCalls = -3
			return e
		},
		"result/terminal disagree": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventPositionResult)[0]].Result.TerminalState = StateFailedWorker
			return e
		},
		"unknown terminal": func(e []Event, s string) []Event {
			e[eventsFor(e, s, EventPositionTerminal)[0]].TerminalState = "EXCLUDED"
			return e
		},
		"blocked without stop": func(e []Event, s string) []Event {
			out := []Event{}
			for _, x := range e {
				if x.SampleID != s || x.Type == EventPositionTerminal {
					out = append(out, x)
				}
			}
			out[eventsFor(out, s, EventPositionTerminal)[0]].TerminalState = StateBlocked
			return out
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var target Position
			m, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
				target = firstPosition(*m, "A", ArmOff) // two attempts, both SUCCESS
				return mutate(e, target.SampleID)
			})
			_ = m
			pr := positionBySample(r, target.SampleID)
			if pr.State != StateMalformedResult || r.Verdict != "NO_GO" {
				t.Fatalf("state=%s verdict=%s errors=%v lineage=%v", pr.State, r.Verdict, pr.Errors, r.LineageErrors)
			}
			if pr.Metrics.M3 != 1 || pr.Metrics.Q1 || pr.Metrics.Q4 {
				t.Fatalf("malformed position not imputed: %+v", pr.Metrics)
			}
		})
	}
}

func TestLineageViolationsAreNOGO(t *testing.T) {
	t.Parallel()
	cases := map[string]func(m *Manifest, e []Event) []Event{
		"missing terminal record": func(m *Manifest, e []Event) []Event {
			return without(e, eventsFor(e, m.Randomization.Schedule[3].SampleID, EventPositionTerminal)[0])
		},
		"duplicate terminal record": func(m *Manifest, e []Event) []Event {
			i := eventsFor(e, m.Randomization.Schedule[3].SampleID, EventPositionTerminal)[0]
			return insertAt(e, i+1, e[i])
		},
		"selective rerun appended": func(m *Manifest, e []Event) []Event {
			s := m.Randomization.Schedule[3].SampleID
			var rerun []Event
			for _, x := range e {
				if x.SampleID == s {
					rerun = append(rerun, x)
				}
			}
			return append(e[:len(e)-1], rerun...)
		},
		"position removed entirely": func(m *Manifest, e []Event) []Event {
			s := m.Randomization.Schedule[10].SampleID
			out := []Event{}
			for _, x := range e {
				if x.SampleID != s {
					out = append(out, x)
				}
			}
			return out
		},
		"out of schedule order": func(m *Manifest, e []Event) []Event {
			a, b := m.Randomization.Schedule[0].SampleID, m.Randomization.Schedule[1].SampleID
			var first, second, rest []Event
			for _, x := range e[2:] {
				switch x.SampleID {
				case a:
					first = append(first, x)
				case b:
					second = append(second, x)
				default:
					rest = append(rest, x)
				}
			}
			out := append([]Event{}, e[:2]...)
			out = append(out, second...)
			out = append(out, first...)
			return append(out, rest...)
		},
		"foreign experiment event": func(m *Manifest, e []Event) []Event {
			x := e[len(e)-2]
			x.ExperimentID = strings.Repeat("9", 64)
			return insertAt(e, len(e)-1, x)
		},
		"unscheduled sample": func(m *Manifest, e []Event) []Event {
			x := e[len(e)-2]
			x.SampleID = strings.Repeat("8", 64)
			return insertAt(e, len(e)-1, x)
		},
		"DECISION not last": func(m *Manifest, e []Event) []Event {
			return insertAt(e, 5, e[len(e)-1])
		},
		"second MANIFEST_FROZEN": func(m *Manifest, e []Event) []Event {
			return insertAt(e, len(e)-1, e[0])
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, r := mutateGO(t, mutate)
			if r.Verdict != "NO_GO" || r.LineageValid || r.ReasonCode != ReasonLineageInvalid {
				t.Fatalf("verdict=%s reason=%s lineage=%v", r.Verdict, r.ReasonCode, r.LineageValid)
			}
			if len(r.Positions) != 40 {
				t.Fatalf("normalization must keep 40 positions, got %d", len(r.Positions))
			}
		})
	}
}

func TestDecisionPreconditionsInNormativeOrder(t *testing.T) {
	t.Parallel()
	t.Run("not frozen", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event { return e[1:] })
		if r.ReasonCode != ReasonPrestartInvalid {
			t.Fatal(r.ReasonCode)
		}
	})
	t.Run("identity mismatch", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
			m.Provider.ClientVersion = "mutated"
			return e
		})
		if r.ReasonCode != ReasonIdentityMismatch {
			t.Fatal(r.ReasonCode)
		}
	})
	t.Run("embedded manifest differs but id kept", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
			e[0].Manifest = json.RawMessage(strings.Replace(string(e[0].Manifest), `"client_version":"1"`, `"client_version":"2"`, 1))
			return e
		})
		if r.ReasonCode != ReasonIdentityMismatch {
			t.Fatal(r.ReasonCode)
		}
	})
	t.Run("missing preflight", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event { return without(e, 1) })
		if r.ReasonCode != ReasonPrestartInvalid {
			t.Fatal(r.ReasonCode)
		}
	})
	t.Run("preflight digest differs", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
			e[1].ObservedDigest = sha256Hex([]byte("x"))
			return e
		})
		if r.ReasonCode != ReasonPrestartInvalid {
			t.Fatal(r.ReasonCode)
		}
	})
	t.Run("schedule invalid", func(t *testing.T) {
		_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
			s := m.Randomization.Schedule
			s[0], s[1] = s[1], s[0]
			id, _ := ExperimentID(*m)
			canonical, _ := CanonicalManifest(*m)
			for i := range e {
				e[i].ExperimentID = id
			}
			e[0].ManifestSHA256, e[0].Manifest = id, canonical
			return e
		})
		if r.ReasonCode != ReasonScheduleInvalid || r.Verdict != "NO_GO" {
			t.Fatalf("%s %s", r.ReasonCode, r.Reason)
		}
	})
}

func TestAnyFailureOrBlockPreventsGO(t *testing.T) {
	t.Parallel()
	for _, state := range []TerminalState{StateFailedWorker, StateFailedReviewer, StateMalformedResult, StateTimeout} {
		t.Run(string(state), func(t *testing.T) {
			var target string
			_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
				target = m.Randomization.Schedule[39].SampleID
				e[eventsFor(e, target, EventPositionResult)[0]].Result.TerminalState = state
				e[eventsFor(e, target, EventPositionTerminal)[0]].TerminalState = state
				return e
			})
			states := countStates(r)
			if r.Verdict != "NO_GO" || states[StateCompleted] != 39 || r.ReasonCode != ReasonNotAllCompleted {
				t.Fatalf("39 COMPLETED + 1 %s => %s %s %v", state, r.Verdict, r.ReasonCode, states)
			}
		})
	}
	t.Run("blocked after a recorded stop", func(t *testing.T) {
		f := newFixture(t)
		f.mini = 39
		res, _ := f.mustRun(t)
		states := countStates(res.Report)
		if res.Report.Verdict != "NO_GO" || states[StateCompleted] != 39 || states[StateBlocked] != 1 || !res.Report.LineageValid {
			t.Fatalf("%s %v", res.Report.Verdict, states)
		}
	})
}

func TestQualityDegradationIsNOGOEvenWithFortyCompleted(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"Q1", "Q4", "Q6"} {
		t.Run(which, func(t *testing.T) {
			_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
				task := "A"
				if which == "Q6" {
					task = "C"
				}
				s := firstPosition(*m, task, ArmAssisted).SampleID
				res := e[eventsFor(e, s, EventPositionResult)[0]].Result
				no := false
				switch which {
				case "Q1":
					res.Q1 = &no
				case "Q4":
					res.Q4 = &no
				case "Q6":
					res.Findings = []Finding{{Rank: 1, TargetSHA256: m.Q6Oracle.ReviewTargetSHA256, File: "fixture.go", FileSHA256: m.Q6Oracle.MandatoryDefects[0].FileSHA256, CausalLine: 2, DefectClass: "AUTHORIZATION_BYPASS", CauseCode: "MISSING_GUARD", ImpactCode: "UNAUTHORIZED_ACCESS"}}
				}
				return e
			})
			if countStates(r)[StateCompleted] != 40 || r.Verdict != "NO_GO" || r.ReasonCode != ReasonQuality {
				t.Fatalf("%s %s %v", r.Verdict, r.ReasonCode, countStates(r))
			}
		})
	}
}

func TestTaskDOutsideNeutralityIsNOGO(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.TaskID == "D" && p.Arm == ArmAssisted {
			return success(40, 10), nil // M1u ratio 0.8 < 0.90
		}
		return success(50, 10), nil
	}
	res, _ := f.mustRun(t)
	if res.Report.Verdict != "NO_GO" || res.Report.ReasonCode != ReasonNegativeControl {
		t.Fatalf("%s %s", res.Report.Verdict, res.Report.ReasonCode)
	}
}

func TestEfficiencyThresholdsAreInclusiveAndTiesDoNotImprove(t *testing.T) {
	t.Parallel()
	if !improves(85, 100, .85) || improves(86, 100, .85) || improves(0, 0, .85) || improves(1, 0, .85) {
		t.Fatal("improves() boundary semantics differ from 06 §6")
	}
	t.Run("tie in task B", func(t *testing.T) {
		f := newFixture(t)
		f.calls = func(p Position) int {
			if p.Arm == ArmAssisted && p.TaskID != "D" && p.TaskID != "B" {
				return 1
			}
			return 2
		}
		base := f.result
		f.result = func(p Position) ExecutionResult {
			r := base(p)
			if p.TaskID == "B" {
				r.ExplorationCalls, r.DistinctFilesRead = 5, 5
			}
			return r
		}
		res, _ := f.mustRun(t)
		if res.Report.Verdict != "NO_GO" || res.Report.ReasonCode != ReasonEfficiency {
			t.Fatalf("%s %s", res.Report.Verdict, res.Report.ReasonCode)
		}
	})
}

func TestQ6PrimaryDefectAndRankOne(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	d1 := m.Q6Oracle.MandatoryDefects[0]
	d2 := d1
	d2.DefectID, d2.CausalLine, d2.DefectClass = "d2", 1, "INPUT_VALIDATION"
	q := m.Q6Oracle
	q.MandatoryDefects = []MandatoryDefect{d1, d2}
	good := []Finding{FindingFor(1, d1), FindingFor(2, d2)}
	if !scoreQ6(q, good) {
		t.Fatal("exact findings failed Q6")
	}
	cases := map[string][]Finding{
		"incorrect rank1":        {FindingFor(1, d2), FindingFor(2, d1)},
		"missing mandatory":      {FindingFor(1, d1)},
		"duplicate finding":      {FindingFor(1, d1), FindingFor(2, d1)},
		"extra finding":          {FindingFor(1, d1), FindingFor(2, d2), func() Finding { f := FindingFor(3, d2); f.CausalLine = 2; return f }()},
		"wrong line":             {func() Finding { f := FindingFor(1, d1); f.CausalLine++; return f }(), FindingFor(2, d2)},
		"wrong code":             {func() Finding { f := FindingFor(1, d1); f.ImpactCode = "CRASH"; return f }(), FindingFor(2, d2)},
		"wrong target":           {func() Finding { f := FindingFor(1, d1); f.TargetSHA256 = strings.Repeat("0", 64); return f }(), FindingFor(2, d2)},
		"wrong file digest":      {func() Finding { f := FindingFor(1, d1); f.FileSHA256 = strings.Repeat("0", 64); return f }(), FindingFor(2, d2)},
		"no rank 1":              {FindingFor(2, d1), FindingFor(3, d2)},
		"duplicate rank":         {FindingFor(1, d1), FindingFor(1, d2)},
		"no findings":            {},
		"over K":                 {FindingFor(1, d1), FindingFor(2, d2), FindingFor(3, d2), FindingFor(4, d2)},
		"primary swapped by row": {FindingFor(2, d1), FindingFor(1, d2)},
	}
	for name, fs := range cases {
		if scoreQ6(q, fs) {
			t.Errorf("%s passed Q6", name)
		}
	}
	// primary_defect_id, not array order, designates the primary.
	q.MandatoryDefects = []MandatoryDefect{d2, d1}
	if !scoreQ6(q, good) {
		t.Fatal("array order changed the primary")
	}
}

func TestNormalizedDomainNeverAllowsNaNOrOutOfRange(t *testing.T) {
	t.Parallel()
	if !math.IsNaN(median(nil)) {
		t.Fatal("median of no values must not be a number")
	}
	if improves(math.NaN(), 1, .85) || improves(1, math.NaN(), .85) {
		t.Fatal("NaN improved")
	}
	m, _, _ := testManifest(t)
	if efficiencyOK(m, nil) {
		t.Fatal("empty data cannot satisfy efficiency")
	}
}
