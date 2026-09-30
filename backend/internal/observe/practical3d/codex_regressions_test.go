package practical3d

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regressions for the real Codex implementation review (#3 #5 #6 #7 #10 #12 #14).

func TestLedgerHashChainAndRegistryAnchor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	res, _ := f.mustRun(t)
	path := filepath.Join(res.Root, "ledger.jsonl")
	if err := VerifyLedgerChain(path); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o400 {
		t.Fatalf("finished ledger mode %v, want read-only", info.Mode().Perm())
	}
	r, err := DecideRun(res.Root, true, time.Now())
	if err != nil || r.Verdict != "GO" {
		t.Fatalf("DecideRun on the untouched run: %v %s", err, r.Verdict)
	}
	// Tamper: edit one line (keeps JSON valid and cardinalities intact).
	raw, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	lines[10] = bytes.Replace(lines[10], []byte(`"timestamp":"`), []byte(`"timestamp":"`), 1)
	lines[10] = append(lines[10][:len(lines[10])-1], []byte(`,"reason":"x"}`)...)
	_ = os.Chmod(path, 0o600)
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLedgerChain(path); err == nil {
		t.Fatal("edited ledger line kept the chain")
	}
	if _, err := DecideRun(res.Root, true, time.Now()); err == nil {
		t.Fatal("DecideRun accepted a ledger that differs from its registry anchor")
	}
	if r := EvaluateLedgerFile(f.m, path, time.Now()); r.Verdict != "NO_GO" || r.LineageValid {
		t.Fatal("broken chain decided GO")
	}
}

func TestDecideRunRefusesUnregisteredCopies(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	res, _ := f.mustRun(t)
	copyRoot := filepath.Join(filepath.Dir(res.Root), "copy")
	if err := os.Mkdir(copyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"envelope.json", "ledger.jsonl"} {
		b, _ := os.ReadFile(filepath.Join(res.Root, name))
		if err := os.WriteFile(filepath.Join(copyRoot, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := DecideRun(copyRoot, true, time.Now()); err == nil {
		t.Fatal("unregistered run copy decided")
	}
}

func TestArtifactRootContainment(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sha256"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256Hex([]byte("outside"))
	if err := os.Symlink(outside, filepath.Join(root, "sha256", digest)); err != nil {
		t.Fatal(err)
	}
	if _, err := (DirArtifactResolver{Root: root}).ReadDigest(digest); err == nil {
		t.Fatal("symlink escaping the artifact root followed")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := (DirArtifactResolver{Root: root}).ReadArtifact("link.bin"); err == nil {
		t.Fatal("treatment symlink escaping the artifact root followed")
	}
	home, _ := os.UserHomeDir()
	if _, err := (DirArtifactResolver{Root: filepath.Join(home, ".ao", "data")}).ReadDigest(digest); err == nil || !errors.Is(err, ErrUnsafeRoot) {
		t.Fatalf("production artifact root accepted: %v", err)
	}
}

func TestTeardownFailureStopsTheBatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	o := f.options(t)
	target := f.m.Randomization.Schedule[4]
	o.Workspaces = failingFinalize{sample: target.SampleID}
	res, err := Run(context.Background(), f.m, o)
	if err != nil {
		t.Fatal(err)
	}
	states := countStates(res.Report)
	if states[StateBlocked] != 35 || states[StateMalformedResult] != 1 || !res.Report.LineageValid || res.Report.Verdict != "NO_GO" {
		t.Fatalf("teardown failure did not stop the batch: %v", states)
	}
}

func TestPerAttemptEnvironmentDivergenceIsMalformed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "A", ArmOff)
	calls := 0
	f.observe = func(ctx context.Context) (EnvironmentInputs, error) {
		e := f.env
		if p, ok := PositionFromContext(ctx); ok && p.SampleID == target.SampleID {
			calls++
			if calls == 2 { // PRE_START ok, first attempt diverges, restored afterwards
				e.TaskToolVersions = []VersionInput{{Component: "go", Version: "other", BinarySHA256: sha256Hex([]byte("go"))}}
			}
		}
		return e, nil
	}
	res, _ := f.mustRun(t)
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
		t.Fatal("mid-position environment change that was restored before PRE_TERMINAL accepted")
	}
}

func TestSeedMustBeLowercaseHex(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	r := m.Randomization
	r.SeedHex = strings.ToUpper(strings.Repeat("ab", 32))
	if _, err := GenerateSchedule(r); err == nil {
		t.Fatal("uppercase seed accepted")
	}
}

func TestProviderMetadataMustAttestAccount(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	target := firstPosition(f.m, "A", ArmOff)
	f.respond = func(p Position, n int) (ProviderResponse, error) {
		r := success(50, 10)
		if p.SampleID == target.SampleID {
			r.ProviderMetadata = []byte(`{"account_ref_sha256":"` + strings.Repeat("0", 64) + `"}`)
		}
		return r, nil
	}
	res, _ := f.mustRun(t)
	if positionBySample(res.Report, target.SampleID).State != StateMalformedResult {
		t.Fatal("attempt from another account accepted")
	}
}

func TestRunnerFailureAfterStartPublishesNOGO(t *testing.T) {
	t.Parallel()
	m, _, _ := testManifest(t)
	root := filepath.Join(t.TempDir(), "run")
	ledger, abs, err := CreateRunDirectory(root, m, EnvelopeMetadata{}, true)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := ExperimentID(m)
	canonical, _ := CanonicalManifest(m)
	reg := OpenRegistry(filepath.Dir(abs))
	p := m.Randomization.Schedule[0]
	for _, e := range []Event{
		{Type: EventManifest, ExperimentID: id, ManifestSHA256: id, Manifest: canonical},
		{Type: EventPreflight, ExperimentID: id, ObservedDigest: m.ExecutionEnvironment.ExpectedExecutionEnvironmentDigest},
		{Type: EventSampleStart, ExperimentID: id, SampleID: p.SampleID, PositionIndex: p.PositionIndex, TaskID: p.TaskID, Arm: p.Arm},
	} {
		if err := ledger.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	r := &runner{m: m, o: RunnerOptions{Now: time.Now}, id: id, root: abs, ledger: ledger, reg: reg, started: true}
	res, err := r.failAfterStart(errors.New("disk full"))
	if err == nil || res.Report.Verdict != "NO_GO" || res.Report.LineageValid {
		t.Fatalf("failure after start: err=%v report=%+v", err, res.Report.ReasonCode)
	}
	if _, statErr := os.Stat(filepath.Join(abs, "report.json")); statErr != nil {
		t.Fatal("no report published after a runner failure")
	}
	entries, _ := reg.Entries()
	if len(entries) != 1 || entries[0].Kind != "RUNNER_FAILURE_AFTER_START" || entries[0].LedgerSHA256 == "" {
		t.Fatalf("registry: %+v", entries)
	}
}
