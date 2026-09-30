package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/observe/practical3d"
)

// TestMain lets the test binary act as the technical drivers the mini-E2E
// re-invokes through os.Executable().
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "technical-driver" {
		if err := technicalDriver(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			_, _ = os.Stderr.WriteString(err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestMiniE2EThroughCLIDrivers(t *testing.T) {
	t.Setenv("AO_DATA_DIR", t.TempDir())
	allowTempDecide = true
	defer func() { allowTempDecide = false }()
	var out strings.Builder
	if err := runMini(filepath.Join(t.TempDir(), "mini"), 2, true, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"MINI_E2E_OK official_experiment=UNSTARTED", "executed_positions=2 blocked=38 verdict=NO_GO", "ATTEMPT_DISPATCHED:4", "ATTEMPT_FINALIZED:4"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFreezeDrawsSeedOnceAndProducesAValidEnvelope(t *testing.T) {
	m, _, _, err := practical3d.NewTechnicalFixture(strings.Repeat("55", 32))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seeded, _ := json.Marshal(m)
	seededPath := filepath.Join(dir, "seeded.json")
	if err := os.WriteFile(seededPath, seeded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"freeze", "--draft", seededPath, "--out", filepath.Join(dir, "x.json")}, &strings.Builder{}); err == nil {
		t.Fatal("freeze accepted a draft that already carries a seed")
	}
	m.Randomization.SeedHex, m.Randomization.Schedule = "", nil
	draft, _ := json.Marshal(m)
	draftPath := filepath.Join(dir, "draft.json")
	if err := os.WriteFile(draftPath, draft, 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(dir, "envelope.json")
	var out strings.Builder
	if err := run([]string{"freeze", "--draft", draftPath, "--out", envPath, "--label", "test"}, &out); err != nil {
		t.Fatal(err)
	}
	env, frozen, err := practical3d.ReadEnvelope(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Randomization.SeedHex == strings.Repeat("55", 32) || len(frozen.Randomization.Schedule) != 40 || !strings.Contains(out.String(), env.ExperimentID) {
		t.Fatalf("freeze output: %s", out.String())
	}
	if err := run([]string{"freeze", "--draft", draftPath, "--out", envPath}, &strings.Builder{}); err == nil {
		t.Fatal("freeze overwrote an existing envelope")
	}
	if err := run([]string{"validate", "--envelope", envPath}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRefusesProductionAODataDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv("AO_DATA_DIR", filepath.Join(home, ".ao", "data"))
	for _, cmd := range []string{"run", "decide", "mini-e2e", "freeze", "validate"} {
		if err := run([]string{cmd}, &strings.Builder{}); err == nil || !strings.Contains(err.Error(), "production") {
			t.Fatalf("%s did not refuse production AO_DATA_DIR: %v", cmd, err)
		}
	}
}

// The official `run` executes the frozen schedule through the same real AO
// executor as mini-e2e-real; --plan proves it without starting anything.
func TestOfficialRunUsesTheRealAOExecutor(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env, err := practical3d.LiveEnvironmentObserver{Expected: practical3d.EnvironmentInputs{RuntimeVersions: []practical3d.VersionInput{{Component: "go"}}, ProviderClientCLIVersions: []practical3d.VersionInput{}, TaskToolVersions: []practical3d.VersionInput{{Component: "git"}}, RunnerInstrumentVersions: []practical3d.VersionInput{{Component: "ao3dpractical"}}, EffectiveEnvironmentConfigAllowlist: []practical3d.ConfigInput{}, AdditionalLocalConfiguration: []practical3d.ConfigInput{}}, AOBinaryPath: self}.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	env.AOCommit = strings.Repeat("a", 40)
	specs, hidden := map[string][]byte{}, map[string][]byte{}
	for _, task := range []string{"A", "B", "C", "D"} {
		b, _ := json.Marshal(practical3d.TaskSpec{Schema: practical3d.TaskSpecSchema, TaskID: task, Objective: "do " + task, ReviewDepth: "none", WriteIntent: "mutating", Verification: json.RawMessage(`{"commands":[]}`), OracleTask: task})
		specs[task], hidden[task] = b, []byte(`{"task":"`+task+`"}`)
	}
	m, _, err := practical3d.BuildRealMiniManifest(practical3d.RealMiniInputs{AOCommit: env.AOCommit, FixtureCommit: strings.Repeat("b", 40), ClaudeVersion: "2.1.285",
		PrimaryModel: "claude-sonnet-5-5", HelperModel: "claude-haiku-4-5", CodexModel: "gpt-5.6-sol", CodexVersion: "0.157.1", AccountRefs: map[string]string{"anthropic": strings.Repeat("1", 64), "openai": strings.Repeat("2", 64)}, Env: env, TaskSpecs: specs,
		Attachment: []byte("MEMORY FRESHNESS: CURRENT\n\n## pack\n"), AttachmentRef: "attachment-A.bin", OracleScript: []byte("#!/bin/bash\n"), HiddenManifests: hidden, VerifyCommand: strings.Join(practical3d.RealVerifyCommand, " "),
		FixtureSubtree: strings.Repeat("c", 64), ReviewTarget: []byte("diff"), ReviewFile: []byte("package orders\n\nfunc Quote() { code.Amount(total) }\n"), ReviewFilePath: "internal/orders/pricing.go", ReviewCausalLine: 3, IndexedCommit: strings.Repeat("b", 40), PackDigest: strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m.Randomization.SeedHex, m.Randomization.Schedule = "", nil
	draft, _ := json.Marshal(m)
	draftPath, envPath := filepath.Join(dir, "draft.json"), filepath.Join(dir, "envelope.json")
	if err := os.WriteFile(draftPath, draft, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"freeze", "--draft", draftPath, "--out", envPath}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	_, frozen, err := practical3d.ReadEnvelope(envPath)
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(dir, "run")
	var out strings.Builder
	if err := run([]string{"run", "--plan", "--envelope", envPath, "--run-root", runRoot, "--artifact-root", dir,
		"--tools", dir, "--fixture-repo", dir, "--oracle-dir", dir, "--real-claude", "/bin/echo", "--real-codex", "/bin/echo", "--ao-src", dir}, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if lines[0] != "executor=*practical3d.AORealExecutor oracle=practical3d.RealOracle workspaces=practical3d.GitWorkspaceManager transport=main.noTransport" {
		t.Fatalf("official run wiring: %s", lines[0])
	}
	if len(lines) != 42 {
		t.Fatalf("plan lines=%d", len(lines))
	}
	for i, p := range frozen.Randomization.Schedule {
		if want := fmt.Sprintf("position %d task=%s arm=%s sample=%s", p.PositionIndex, p.TaskID, p.Arm, p.SampleID); lines[i+1] != want {
			t.Fatalf("plan line %d = %q, want %q", i+1, lines[i+1], want)
		}
	}
	if _, err := os.Stat(runRoot); !os.IsNotExist(err) {
		t.Fatal("--plan created the run root")
	}
}
