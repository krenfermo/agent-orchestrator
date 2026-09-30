package practical3d

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestEmbeddedManifestSchemaDigestMatchesNormativeDocs(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../docs/frente3/06-benchmark-plan.md")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(raw, []byte("### 3.1 "))
	end := bytes.Index(raw, []byte("## 4. Vocabulario"))
	if start < 0 || end <= start {
		t.Fatal("normative schema sections not found")
	}
	section := norm.NFC.Bytes(bytes.ReplaceAll(raw[start:end], []byte("\r\n"), []byte("\n")))
	if got := sha256Hex(section); got != ExpectedManifestSchemaSHA256 {
		t.Fatalf("schema digest drift: got %s want %s", got, ExpectedManifestSchemaSHA256)
	}
}

func canonicalMap(t *testing.T, m Manifest) map[string]any {
	t.Helper()
	b, err := CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestCanonicalManifestStableAndRoundTrips(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	a, err := CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(a)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := CanonicalManifest(decoded)
	if !bytes.Equal(a, b) {
		t.Fatal("canonical manifest changed after round trip")
	}
	id1, _ := ExperimentID(m)
	id2, _ := ExperimentID(decoded)
	if id1 != id2 || id1 != sha256Hex(a) {
		t.Fatal("experiment_id is not SHA-256 of canonical bytes")
	}
	if bytes.ContainsAny(a, " \n\t") && !bytes.Contains(a, []byte("technical fixture")) {
		t.Fatal("canonical bytes contain insignificant whitespace")
	}
	// Keys are sorted: re-encoding the generic map (sorted by encoding/json) is identical.
	re, _ := canonicalWith(canonicalMap(t, m), manifestDecimals)
	if !bytes.Equal(re, a) {
		t.Fatal("canonical bytes are not key-sorted")
	}
}

func TestClosedSchemaRejectsUnknownMissingNullAndProhibitedFields(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	cases := map[string]func(map[string]any){
		"unknown root field": func(o map[string]any) { o["unknown"] = true },
		"missing zero-valued field": func(o map[string]any) {
			rb := o["retry_policy"].(map[string]any)["retry_budgets"].([]any)[0].(map[string]any)
			delete(rb, "retryable_max_retries")
		},
		"null field": func(o map[string]any) { o["TOKEN_CAP_ROLE"].([]any)[0].(map[string]any)["cap"] = nil },
		"missing N":  func(o map[string]any) { delete(o, "N") },
		"float N":    func(o map[string]any) { o["N"] = json.Number("5.0") },
		"exponent":   func(o map[string]any) { o["N"] = json.Number("5e0") },
		"OFF empty sha": func(o map[string]any) {
			o["treatment_mapping"].([]any)[0].(map[string]any)["OFF"].(map[string]any)["attachment_sha256"] = ""
		},
		"OFF with digest": func(o map[string]any) {
			o["treatment_mapping"].([]any)[0].(map[string]any)["OFF"].(map[string]any)["attachment_sha256"] = strings.Repeat("a", 64)
		},
		"threshold changed": func(o map[string]any) { o["thresholds"].(map[string]any)["m1u_max_ratio"] = json.Number("0.9") },
		"nested unknown":    func(o map[string]any) { o["provider"].(map[string]any)["extra"] = "x" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			obj := canonicalMap(t, m)
			mutate(obj)
			raw, _ := json.Marshal(obj)
			if _, err := DecodeManifest(raw); err == nil {
				t.Fatal("closed schema accepted the mutation")
			}
		})
	}
	dup := []byte(`{"N":5,"N":5}`)
	if _, err := DecodeManifest(dup); err == nil {
		t.Fatal("duplicate key accepted")
	}
}

func TestAnyFrozenFieldChangeChangesExperimentID(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	id, _ := ExperimentID(m)
	mutations := map[string]func(*Manifest){
		"client version":  func(x *Manifest) { x.Provider.ClientVersion = "2" },
		"attachment":      func(x *Manifest) { x.TreatmentMapping[0].ASSISTED.AttachmentSHA256 = sha256Hex([]byte("other")) },
		"primary defect":  func(x *Manifest) { x.Q6Oracle.PrimaryDefectID = "d2" },
		"retry budget":    func(x *Manifest) { x.RetryPolicy.RetryBudgets[0].RetryableMaxRetries = 2 },
		"env digest":      func(x *Manifest) { x.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest = strings.Repeat("b", 64) },
		"cap":             func(x *Manifest) { x.TokenCaps[0].Cap++ },
		"seed":            func(x *Manifest) { x.Randomization.SeedHex = strings.Repeat("33", 32) },
		"effective cfg":   func(x *Manifest) { x.InvocationConfigs[0].ModelVersion = "2" },
		"workflow":        func(x *Manifest) { x.Workflow.WorkerFlowVersion = "v2" },
		"deadline":        func(x *Manifest) { x.Deadlines.PositionSeconds++ },
		"context":         func(x *Manifest) { x.ContextSourceInventory[2].VerificationVersion = "v2" },
		"attempt schema":  func(x *Manifest) { x.Instrument.AttemptEventSchemaVersion = "v2" },
		"causal line":     func(x *Manifest) { x.Q6Oracle.MandatoryDefects[0].CausalLine = 2 },
		"position policy": func(x *Manifest) { x.PositionIsolation.LocalProcessTeardown = "x" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			x := deepCopyManifest(t, m)
			mutate(&x)
			id2, err := ExperimentID(x)
			if err != nil {
				t.Fatal(err)
			}
			if id2 == id {
				t.Fatal("frozen field change preserved experiment_id")
			}
		})
	}
}

func deepCopyManifest(t *testing.T, m Manifest) Manifest {
	t.Helper()
	b, err := CanonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	var out Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChaCha20KnownAnswer(t *testing.T) {
	t.Parallel()
	// RFC 8439 A.1 test vector #1: zero key, zero nonce, block counter 0.
	w, err := newChaChaWords(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.next(); got != 0xade0b876 {
		t.Fatalf("first LE keystream word = %#x, want 0xade0b876", got)
	}
	if got := w.next(); got != 0x903df1a0 {
		t.Fatalf("second LE keystream word = %#x, want 0x903df1a0", got)
	}
}

func TestScheduleReproducibleFortyPositionsFivePairsPerTask(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	a, err := GenerateSchedule(m.Randomization)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateSchedule(m.Randomization)
	if !reflect.DeepEqual(a, b) || len(a) != 40 {
		t.Fatalf("schedule unstable or length=%d", len(a))
	}
	seenSample := map[string]bool{}
	for i, p := range a {
		if p.PositionIndex != i+1 || seenSample[p.SampleID] || !validSHA256(p.SampleID) {
			t.Fatalf("position %d index/sample invalid", i)
		}
		seenSample[p.SampleID] = true
	}
	for ti, task := range taskOrder {
		offFirst := 0
		for pair := 1; pair <= 5; pair++ {
			i := ti*10 + (pair-1)*2
			x, y := a[i], a[i+1]
			if x.TaskID != task || y.TaskID != task || x.PairIndex != pair || y.PairIndex != pair || x.Arm == y.Arm || !reflect.DeepEqual(x.PairOrder, y.PairOrder) || x.PairOrder[0] != x.Arm || x.PairOrder[1] != y.Arm {
				t.Fatalf("task %s pair %d is not one consecutive OFF/ASSISTED pair: %+v %+v", task, pair, x, y)
			}
			if x.Arm == ArmOff {
				offFirst++
			}
		}
		if offFirst != 2 && offFirst != 3 {
			t.Fatalf("task %s OFF-first pairs=%d, want 2 or 3", task, offFirst)
		}
	}
	other := m.Randomization
	other.SeedHex = strings.Repeat("33", 32)
	c, _ := GenerateSchedule(other)
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seed produced the same schedule")
	}
}

func TestAlteredScheduleRejected(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Manifest){
		"swap":       func(m *Manifest) { s := m.Randomization.Schedule; s[0], s[1] = s[1], s[0] },
		"drop":       func(m *Manifest) { m.Randomization.Schedule = m.Randomization.Schedule[:39] },
		"replace id": func(m *Manifest) { m.Randomization.Schedule[5].SampleID = strings.Repeat("c", 64) },
		"flip order": func(m *Manifest) {
			o := m.Randomization.Schedule[0].PairOrder
			m.Randomization.Schedule[0].PairOrder = []Arm{o[1], o[0]}
		},
		"prng version": func(m *Manifest) { m.Randomization.PRNGVersion = "v2" },
		"extra row": func(m *Manifest) {
			m.Randomization.Schedule = append(m.Randomization.Schedule, m.Randomization.Schedule[0])
		},
		"balance label": func(m *Manifest) { m.Randomization.OrientationBalance = "any" },
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := testManifest(t)
			mutate(&m)
			if err := ValidateManifest(m); err == nil {
				t.Fatal("altered schedule accepted")
			}
		})
	}
}

func TestManifestPrestartRejections(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Manifest){
		"unknown role in retry budget": func(m *Manifest) { m.RetryPolicy.RetryBudgets[0].Role = "intruder" },
		"duplicate retry budget": func(m *Manifest) {
			m.RetryPolicy.RetryBudgets = append(m.RetryPolicy.RetryBudgets, m.RetryPolicy.RetryBudgets[0])
		},
		"missing retry budget": func(m *Manifest) { m.RetryPolicy.RetryBudgets = m.RetryPolicy.RetryBudgets[:1] },
		"two budgets for one role": func(m *Manifest) {
			b := m.RetryPolicy.RetryBudgets[0]
			b.RetryableMaxRetries = 7
			m.RetryPolicy.RetryBudgets[1] = b
		},
		"unknown role in CLOSED_ROLE_SET": func(m *Manifest) { m.ClosedRoleSet = append(m.ClosedRoleSet, "intruder") },
		"role set out of order":           func(m *Manifest) { m.ClosedRoleSet = []Role{RoleReviewer, RoleWorker} },
		"missing treatment cell":          func(m *Manifest) { m.TreatmentMapping = m.TreatmentMapping[1:] },
		"extra treatment cell": func(m *Manifest) {
			c := m.TreatmentMapping[0]
			c.CallClass = CallSummary
			m.TreatmentMapping = append(m.TreatmentMapping, c)
		},
		"OFF attachment": func(m *Manifest) { v := true; m.TreatmentMapping[0].OFF.AttachmentPresent = &v },
		"target role without attachment": func(m *Manifest) {
			for i := range m.TreatmentMapping {
				if m.TreatmentMapping[i].TaskID == "A" {
					v := false
					m.TreatmentMapping[i].ASSISTED = TreatmentArm{AttachmentPresent: &v}
				}
			}
		},
		"unused role positive cap": func(m *Manifest) { m.TokenCaps[1].Cap = 5 },
		"used role zero cap":       func(m *Manifest) { m.CallCaps[0].Cap = 0 },
		"caps out of order":        func(m *Manifest) { m.TokenCaps[0], m.TokenCaps[1] = m.TokenCaps[1], m.TokenCaps[0] },
		"M3 wrong role": func(m *Manifest) {
			m.M3Caps[0].CCap, m.M3Caps[0].FCap = 0, 0
			m.M3Caps[1].CCap, m.M3Caps[1].FCap = 1, 1
		},
		"sdk retries nonzero": func(m *Manifest) {
			m.InvocationConfigs[0].EffectiveConfig = json.RawMessage(strings.Replace(string(m.InvocationConfigs[0].EffectiveConfig), `"sdk_max_retries":0`, `"sdk_max_retries":2`, 1))
			c, _ := canonicalRaw(m.InvocationConfigs[0].EffectiveConfig, noDecimals)
			m.InvocationConfigs[0].EffectiveConfigSHA256 = sha256Hex(c)
		},
		"implicit default (missing key)": func(m *Manifest) {
			m.InvocationConfigs[0].EffectiveConfig = json.RawMessage(strings.Replace(string(m.InvocationConfigs[0].EffectiveConfig), `,"top_p":null`, ``, 1))
		},
		"effective config digest": func(m *Manifest) { m.InvocationConfigs[0].EffectiveConfigSHA256 = strings.Repeat("d", 64) },
		"unknown config schema":   func(m *Manifest) { m.InvocationConfigs[0].EffectiveConfigSchema = "vendor-default" },
		"env digest mismatch":     func(m *Manifest) { m.ExecutionEnvironment.Inputs.OSPlatformArch.Arch = "other" },
		"router on":               func(m *Manifest) { m.Router = "ON" },
		"N changed":               func(m *Manifest) { m.N = 6 },
		"NaN threshold":           func(m *Manifest) { m.Thresholds.M1UMaxRatio = math.NaN() },
		"threshold drift":         func(m *Manifest) { m.Thresholds.DNeutralUpper = 1.2 },
		"context source enabled":  func(m *Manifest) { m.ContextSourceInventory[2].State = "ENABLED" },
		"context source missing":  func(m *Manifest) { m.ContextSourceInventory = m.ContextSourceInventory[:8] },
		"external context on":     func(m *Manifest) { m.ExternalContext.Policy = "ENABLED" },
		"primary missing":         func(m *Manifest) { m.Q6Oracle.PrimaryDefectID = "" },
		"primary dangling":        func(m *Manifest) { m.Q6Oracle.PrimaryDefectID = "nope" },
		"duplicate defect id": func(m *Manifest) {
			m.Q6Oracle.MandatoryDefects = append(m.Q6Oracle.MandatoryDefects, m.Q6Oracle.MandatoryDefects[0])
		},
		"K changed":          func(m *Manifest) { m.Q6Oracle.K = 4 },
		"defect target":      func(m *Manifest) { m.Q6Oracle.MandatoryDefects[0].TargetSHA256 = strings.Repeat("e", 64) },
		"defect code":        func(m *Manifest) { m.Q6Oracle.MandatoryDefects[0].CauseCode = "WHATEVER" },
		"defect path escape": func(m *Manifest) { m.Q6Oracle.MandatoryDefects[0].File = "../x.go" },
		"schema digest":      func(m *Manifest) { m.ManifestSchemaSHA256 = strings.Repeat("f", 64) },
		"access boundary":    func(m *Manifest) { m.ProviderAccessBoundary = "DIRECT_SDK" },
		"isolation":          func(m *Manifest) { m.PositionIsolation.NewAODataDir = false },
		"missing deadline":   func(m *Manifest) { m.Deadlines.RoleSeconds = m.Deadlines.RoleSeconds[:1] },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m, _, _ := testManifest(t)
			m = deepCopyManifest(t, m)
			mutate(&m)
			if err := ValidateManifest(m); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}
