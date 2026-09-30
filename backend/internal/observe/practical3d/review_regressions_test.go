package practical3d

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Regressions for the independent implementation review (P0-1, P1-1, P1-2, P2).

func TestTokenOverflowCannotBypassCapsOrProduceGO(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.Arm == ArmAssisted && p.TaskID != "D" {
			return success(1<<62, 0), nil
		}
		return success(50, 10), nil
	}
	res, _ := f.mustRun(t)
	if res.Report.Verdict != "NO_GO" {
		t.Fatal("int64 overflow produced GO")
	}
	pr := positionBySample(res.Report, firstPosition(f.m, "A", ArmAssisted).SampleID)
	if pr.State != StateMalformedResult || pr.Metrics.M1U != 1000 {
		t.Fatalf("overflowing attempt: state=%s M1u=%d errors=%v", pr.State, pr.Metrics.M1U, pr.Errors)
	}
	m, _, _ := testManifest(t)
	m = deepCopyManifest(t, m)
	m.TokenCaps[0].Cap = MaxCap + 1
	if ValidateManifest(m) == nil {
		t.Fatal("unbounded cap accepted")
	}
}

func TestOffSpanDetectionHandlesHTMLCharactersAndOtherTasks(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	attachment := []byte("header line that is long enough to be searched\nfunc Allowed(role string) bool { return a < b && c > d }\n")
	spans := map[string][][]byte{allTasksSpans: attachmentSpans(attachment)}
	for _, line := range []string{"func Allowed(role string) bool { return a < b && c > d }", "header line that is long enough to be searched"} {
		payload, _ := json.Marshal(map[string]string{"prompt": "context:\n" + line})
		rep := Representation{Payload: payload, ContextSourceStates: m.ContextSourceInventory}
		// Task B OFF must also reject spans of any task's frozen attachment.
		if _, _, err := traceRepresentation(m, spans, "B", ArmOff, RoleWorker, CallInitial, rep); err == nil {
			t.Fatalf("OFF request carrying attachment line %q accepted", line)
		}
	}
	clean, _ := json.Marshal(map[string]string{"prompt": "unrelated"})
	if _, _, err := traceRepresentation(m, spans, "B", ArmOff, RoleWorker, CallInitial, Representation{Payload: clean, ContextSourceStates: m.ContextSourceInventory}); err != nil {
		t.Fatalf("clean OFF request rejected: %v", err)
	}
}

func TestCompletedWithoutMeasuredAttemptsIsMalformed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.calls = func(p Position) int {
		if p.Arm == ArmAssisted && p.TaskID != "D" {
			return 0
		}
		return 2
	}
	res, _ := f.mustRun(t)
	pr := positionBySample(res.Report, firstPosition(f.m, "A", ArmAssisted).SampleID)
	if res.Report.Verdict != "NO_GO" || pr.State != StateMalformedResult {
		t.Fatalf("zero-call COMPLETED accepted: %s %s", res.Report.Verdict, pr.State)
	}
}

func TestLedgerDispatchOrderMustFollowCallIndex(t *testing.T) {
	t.Parallel()
	_, r := mutateGO(t, func(m *Manifest, e []Event) []Event {
		s := firstPosition(*m, "A", ArmOff).SampleID
		d := eventsFor(e, s, EventAttemptDispatched)
		f := eventsFor(e, s, EventAttemptFinalized)
		// Relabel the two attempts so the later ledger pair claims call_index 1.
		for _, i := range []int{d[0], f[0]} {
			e[i].CallIndex = 2
		}
		for _, i := range []int{d[1], f[1]} {
			e[i].CallIndex = 1
		}
		return e
	})
	if r.Verdict != "NO_GO" || positionBySample(r, firstPosition(mustManifest(t), "A", ArmOff).SampleID).State != StateMalformedResult {
		t.Fatal("dispatch order inconsistent with call_index accepted")
	}
}

func mustManifest(t *testing.T) Manifest {
	m, _, _ := testManifest(t)
	return m
}

func TestContextInventoryTightening(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Manifest){
		"project_memory state": func(m *Manifest) { m.ContextSourceInventory[0].State = "EQUALIZED" },
		"EQUALIZED empty":      func(m *Manifest) { m.ExternalContext.Policy = "EQUALIZED" },
		"equalized not in inventory": func(m *Manifest) {
			m.ExternalContext = ExternalContext{Policy: "EQUALIZED", EqualizedSources: []EqualizedSource{{SourceID: "web", RepresentationSHA256: sha256Hex([]byte("w"))}}}
		},
	} {
		m, _, _ := testManifest(t)
		m = deepCopyManifest(t, m)
		mutate(&m)
		if ValidateManifest(m) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRegistryClaimIsAtomic(t *testing.T) {
	t.Parallel()
	reg := OpenRegistry(t.TempDir())
	id := sha256Hex([]byte("x"))
	var wg sync.WaitGroup
	wins := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wins <- reg.Claim(id) == nil
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for w := range wins {
		if w {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d concurrent claims succeeded", n)
	}
	if err := reg.Claim(id); !errors.Is(err, ErrExperimentRegistered) {
		t.Fatal(err)
	}
}

func TestDraftDecodingNeverFillsDefaults(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	obj := canonicalMap(t, m)
	delete(obj["retry_policy"].(map[string]any)["retry_budgets"].([]any)[0].(map[string]any), "rate_limited_max_retries")
	raw, _ := json.Marshal(obj)
	if _, err := DecodeDraftManifest(raw); err == nil {
		t.Fatal("draft with a missing field decoded")
	}
}

func TestLiveEnvironmentObserverDoesNotMutateManifest(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRACTICAL3D_TEST_ALLOW", "observed-value")
	expected := EnvironmentInputs{EffectiveEnvironmentConfigAllowlist: []ConfigInput{{Name: "PRACTICAL3D_TEST_ALLOW", EffectiveValueOrSHA256: "frozen"}}}
	o := LiveEnvironmentObserver{Expected: expected, AOBinaryPath: self}
	got, err := o.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if expected.EffectiveEnvironmentConfigAllowlist[0].EffectiveValueOrSHA256 != "frozen" || got.EffectiveEnvironmentConfigAllowlist[0].EffectiveValueOrSHA256 != "observed-value" {
		t.Fatal("Observe mutated the frozen inputs")
	}
}

func TestRefuseProductionOutputPath(t *testing.T) {
	t.Parallel()
	home, _ := os.UserHomeDir()
	if RefuseProductionPath(filepath.Join(home, ".ao", "data", "report.json")) == nil {
		t.Fatal("production output path accepted")
	}
	if err := RefuseProductionPath(filepath.Join(t.TempDir(), "r.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRetriedInitialCallStillCountsAsMeasured(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		if p.Arm == ArmAssisted && n == 1 {
			return outcome(OutcomeRetryable, 1), nil
		}
		return success(50, 10), nil
	}
	res, _ := f.mustRun(t)
	if pr := positionBySample(res.Report, firstPosition(f.m, "B", ArmAssisted).SampleID); pr.State != StateCompleted {
		t.Fatalf("retried-then-successful initial call rejected: %s %v", pr.State, pr.Errors)
	}
}

func TestLiveEnvironmentObserverKeepsEmptyListsPresent(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	e := EnvironmentInputs{RuntimeVersions: []VersionInput{}, ProviderClientCLIVersions: []VersionInput{}, TaskToolVersions: []VersionInput{}, RunnerInstrumentVersions: []VersionInput{}, EffectiveEnvironmentConfigAllowlist: []ConfigInput{}, AdditionalLocalConfiguration: []ConfigInput{}}
	got, err := LiveEnvironmentObserver{Expected: e, AOBinaryPath: self}.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.RuntimeVersions == nil || got.AdditionalLocalConfiguration == nil || got.EffectiveEnvironmentConfigAllowlist == nil {
		t.Fatal("empty lists became nil")
	}
}
