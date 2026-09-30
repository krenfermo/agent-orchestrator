package practical3d

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Adversarial validation of the final attacks A–F (06 §6 table).

func TestAttackA_SwapPrimaryDefect(t *testing.T) {
	t.Parallel()
	m, events := goLedger(t)
	d1 := m.Q6Oracle.MandatoryDefects[0]
	d2 := d1
	d2.DefectID, d2.CausalLine = "d2", 1
	q := m.Q6Oracle
	q.MandatoryDefects = []MandatoryDefect{d1, d2}
	// A reviewer ranking the non-primary defect first scores Q6=0.
	if scoreQ6(q, []Finding{FindingFor(1, d2), FindingFor(2, d1)}) {
		t.Fatal("rank 1 on a non-primary defect passed")
	}
	// Re-pointing primary_defect_id after the run changes experiment_id.
	swapped := deepCopyManifest(t, m)
	swapped.Q6Oracle.PrimaryDefectID = "other"
	swapped.Q6Oracle.MandatoryDefects[0].DefectID = "other"
	if r := Evaluate(swapped, events, time.Now()); r.Verdict != "NO_GO" || r.ReasonCode != ReasonIdentityMismatch {
		t.Fatalf("primary swap: %s %s", r.Verdict, r.ReasonCode)
	}
}

func TestAttackB_DispatchWithoutFinalization(t *testing.T) {
	t.Parallel()
	m, events := goLedger(t)
	s := firstPosition(m, "B", ArmAssisted).SampleID
	events = without(cloneEvents(events), eventsFor(events, s, EventAttemptFinalized)[0])
	r := Evaluate(m, events, time.Now())
	if positionBySample(r, s).State != StateMalformedResult || r.Verdict != "NO_GO" {
		t.Fatal("dispatch without finalization accepted")
	}
}

func TestAttackC_DuplicateFinalization(t *testing.T) {
	t.Parallel()
	m, events := goLedger(t)
	s := firstPosition(m, "C", ArmOff).SampleID
	events = cloneEvents(events)
	i := eventsFor(events, s, EventAttemptFinalized)[0]
	dup := events[i]
	zero := int64(0)
	dup.UncachedInputTokens, dup.InputTokens, dup.CachedInputTokens = &zero, &zero, &zero // a "better" replacement
	events = insertAt(events, i+1, dup)
	r := Evaluate(m, events, time.Now())
	if positionBySample(r, s).State != StateMalformedResult || r.Verdict != "NO_GO" {
		t.Fatal("duplicate finalization accepted")
	}
}

func TestAttackD_TwoRetryBudgetsForOneRole(t *testing.T) {
	m, _, _ := testManifest(t)
	m = deepCopyManifest(t, m)
	b := m.RetryPolicy.RetryBudgets[0]
	b.RetryableMaxRetries = 9
	m.RetryPolicy.RetryBudgets = append(m.RetryPolicy.RetryBudgets, b)
	if err := ValidateManifest(m); err == nil {
		t.Fatal("two retry budgets for one role accepted")
	}
	// Replacing another role's row keeps cardinality but loses coverage.
	m2, _, _ := testManifest(t)
	m2 = deepCopyManifest(t, m2)
	m2.RetryPolicy.RetryBudgets[1] = b
	if err := ValidateManifest(m2); err == nil {
		t.Fatal("duplicate role with correct cardinality accepted")
	}
	f := newFixture(t)
	f.m = m
	t.Setenv("AO_DATA_DIR", t.TempDir())
	res, _, err := f.run(t)
	if err == nil || res.Report.ReasonCode != ReasonPrestartInvalid {
		t.Fatalf("runner started with ambiguous retry budgets: %v", err)
	}
}

func TestAttackE_ToolVersionDiffersBetweenArms(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	divergent := func(n int) (EnvironmentInputs, error) {
		e := f.env
		// Observation 1 is the batch preflight; then PRE_START/PRE_TERMINAL per position.
		if n >= 2 && f.m.Randomization.Schedule[(n-2)/2].Arm == ArmAssisted {
			e.TaskToolVersions = []VersionInput{{Component: "go", Version: "go1.27", BinarySHA256: sha256Hex([]byte("go1.27"))}}
		}
		return e, nil
	}
	f.observe = divergent
	res, events, err := f.run(t)
	if f.m.Randomization.Schedule[0].Arm == ArmAssisted {
		if err == nil || res.Report.ReasonCode != ReasonPrestartInvalid {
			t.Fatalf("divergence before the first SAMPLE_START must be PRESTART_INVALID: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Report.Positions {
		if p.Position.Arm == ArmAssisted && p.State != StateMalformedResult {
			t.Fatalf("ASSISTED position %d with a different tool version is %s", p.Position.PositionIndex, p.State)
		}
		if p.Position.Arm == ArmOff && p.State != StateCompleted {
			t.Fatalf("OFF position %d is %s", p.Position.PositionIndex, p.State)
		}
	}
	if res.Report.Verdict != "NO_GO" {
		t.Fatal("tool-version divergence produced GO")
	}
	// Forging the observed digests back to the expected value still leaves
	// the preflight contrast: the manifest's expected digest is common to both arms.
	if Evaluate(f.m, events, time.Now()).Verdict != "NO_GO" {
		t.Fatal("re-decision produced GO")
	}
}

func TestAttackF_KeepExperimentIDAfterAtoE(t *testing.T) {
	t.Parallel()
	m, events := goLedger(t)
	mutations := map[string]func(*Manifest){
		"A primary":       func(x *Manifest) { x.Q6Oracle.MandatoryDefects[0].DefectID, x.Q6Oracle.PrimaryDefectID = "p2", "p2" },
		"C lifecycle":     func(x *Manifest) { x.Instrument.AttemptEventSchemaVersion = "v2" },
		"D retry budgets": func(x *Manifest) { x.RetryPolicy.RetryBudgets[1].RateLimitedMaxRetries = 0 },
		"E tool version": func(x *Manifest) {
			x.ExecutionEnvironment.Inputs.TaskToolVersions = []VersionInput{{Component: "go", Version: "go1.27", BinarySHA256: sha256Hex([]byte("go1.27"))}}
			d, _ := EnvironmentDigest(x.ExecutionEnvironment.DigestSchemaVersion, x.ExecutionEnvironment.Inputs)
			x.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest = d
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			x := deepCopyManifest(t, m)
			mutate(&x)
			if err := ValidateManifest(x); err != nil {
				t.Fatalf("mutation should stay schema-valid to isolate identity: %v", err)
			}
			r := Evaluate(x, events, time.Now())
			if r.Verdict != "NO_GO" || r.ReasonCode != ReasonIdentityMismatch {
				t.Fatalf("%s kept the old experiment_id: %s %s", name, r.Verdict, r.ReasonCode)
			}
			// An envelope claiming the old ID for the mutated manifest is rejected too.
			canonical, _ := CanonicalManifest(x)
			dir := t.TempDir()
			env := `{"experiment_id":"` + events[0].ExperimentID + `","manifest_sha256":"` + events[0].ExperimentID + `","manifest":` + string(canonical) + `,"metadata":{"human_label":null,"human_version":null}}`
			path := filepath.Join(dir, "envelope.json")
			if err := os.WriteFile(path, []byte(env), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := ReadEnvelope(path); err == nil {
				t.Fatal("envelope with a stale experiment_id accepted")
			}
		})
	}
}
