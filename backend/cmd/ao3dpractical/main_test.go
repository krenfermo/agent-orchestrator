package main

import (
	"encoding/json"
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
