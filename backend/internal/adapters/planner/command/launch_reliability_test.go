package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// launch_reliability_test.go covers the wf-7f8cc736 incident: a real objective
// stopped before planning on "The planner could not be started. Check the
// planner provider's auth and installation, then retry planning." with durable
// evidence that said the subprocess had exited and nothing about why.
//
// These tests run a REAL subprocess wherever the behaviour under test is about
// a real subprocess. The scripts below stand in for the provider CLI precisely
// so a missing/expired local Claude installation cannot make the suite pass or
// fail for the wrong reason -- the thing being tested is AO's handling of what
// a provider does, not the provider.

// fakeCLI writes an executable shell script and returns its path.
func fakeCLI(t *testing.T, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake provider CLI is a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	// The fake CLI's directory FIRST, then the ordinary system directories the
	// script's own shell builtins and tools need. A daemon always has a real
	// PATH; pinning it to the fixture directory alone would test a shell
	// without /bin rather than the adapter.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/bin"+string(os.PathListSeparator)+"/usr/bin")
	return path
}

func plannerRequest(t *testing.T) workflowcore.PlannerRequest {
	t.Helper()
	return workflowcore.PlannerRequest{
		Objective: "add a line to the readme",
		Project:   domain.ProjectRecord{ID: "p", Path: t.TempDir()},
		Context:   workflowcore.PlannerContext{Version: workflowcore.PlannerContextVersion},
		MaxSteps:  3,
	}
}

// errorEnvelope renders a print-mode failure envelope with the SAME field
// order the real CLI uses: the metrics block first, the verdict fields at the
// end. That order is the incident -- the old adapter kept the first 500 bytes
// of this and threw the rest away.
func errorEnvelope(t *testing.T, subtype, apiStatus, result string) string {
	t.Helper()
	// Hand-built rather than marshalled from a map: the FIELD ORDER is the
	// fixture. encoding/json sorts map keys, which would put is_error near the
	// front and quietly stop reproducing the incident.
	quoted := func(v string) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	padding := strings.Repeat("0123456789", 120) // pushes the verdict past 500 bytes
	out := `{"duration_api_ms":0,"stop_reason":"stop_sequence",` +
		`"session_id":"8dbf1669-173e-46d7-be01-85244329c270","total_cost_usd":0,` +
		`"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,` +
		`"cache_read_input_tokens":0,"iterations":[],"padding":"` + padding + `"},` +
		`"is_error":true,"subtype":` + quoted(subtype) +
		`,"api_error_status":` + quoted(apiStatus) +
		`,"result":` + quoted(result) + `}`
	// The verdict fields must genuinely sit beyond the old 500-byte window,
	// or this fixture would not reproduce the incident it exists for.
	if idx := strings.Index(out, `"is_error"`); idx <= outputSnippetLimit {
		t.Fatalf("fixture is not representative: is_error at byte %d, inside the old %d-byte snippet", idx, outputSnippetLimit)
	}
	if !json.Valid([]byte(out)) {
		t.Fatal("fixture envelope is not valid JSON")
	}
	return out
}

func realPlanner(binary string) Planner {
	return Planner{Binary: binary, Model: "sonnet", Timeout: 20 * time.Second, MaxTimeout: 20 * time.Second}
}

// TestGenerate_BinaryMissing_IsTypedAndNeverSpawns is §3's first distinction:
// "provider binary missing" must be its own answer, decided before anything is
// launched.
func TestGenerate_BinaryMissing_IsTypedAndNeverSpawns(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	p := realPlanner("claude-that-does-not-exist")
	_, err := p.Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerBinaryMissing) {
		t.Fatalf("err=%v, want ErrPlannerBinaryMissing", err)
	}
	if !strings.Contains(err.Error(), "claude-that-does-not-exist") {
		t.Fatalf("error must name the binary AO looked for: %v", err)
	}
	ev, ok := workflowcore.PlannerEvidenceFrom(err)
	if !ok || ev.Classification != workflowcore.PlannerAttemptBinaryMissing {
		t.Fatalf("evidence=%+v ok=%v, want classification %q", ev, ok, workflowcore.PlannerAttemptBinaryMissing)
	}
	if ev.DurationMS != 0 {
		t.Fatalf("a refused launch spends no time: durationMs=%d", ev.DurationMS)
	}
}

// TestGenerate_BinaryFoundButNotExecutable_IsNotReportedAsMissing keeps §3's
// "binary found but unusable" separate from "binary missing" in the text a
// person reads, even though both are permanent.
func TestGenerate_BinaryFoundButNotExecutable_IsNotReportedAsMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", t.TempDir())
	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerBinaryMissing) {
		t.Fatalf("err=%v, want ErrPlannerBinaryMissing", err)
	}
	if !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("error must say the installation is unusable rather than absent: %v", err)
	}
}

// TestGenerate_ProfileDirectoryUnreadable_IsItsOwnFailure is the TrustedLocal
// incident's shape: an isolated runtime home the provider cannot read is a
// configuration fault, not "auth is broken".
func TestGenerate_ProfileDirectoryUnreadable_IsItsOwnFailure(t *testing.T) {
	fakeCLI(t, "claude", "exit 0\n")
	t.Setenv("HOME", filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerRuntimeHomeUnreadable) {
		t.Fatalf("err=%v, want ErrPlannerRuntimeHomeUnreadable", err)
	}
	if !strings.Contains(err.Error(), "HOME=") {
		t.Fatalf("error must name the variable and directory it checked: %v", err)
	}
}

// TestGenerate_RuntimeEnvOverrideDecidesPreflight proves the preflight reads
// the environment the SUBPROCESS will get, not the daemon's. A preflight that
// used os.Getenv would bless a launch that is about to run against an
// overridden home that does not exist -- which is exactly how an isolated
// runtime home hides a broken profile until the provider fails.
func TestGenerate_RuntimeEnvOverrideDecidesPreflight(t *testing.T) {
	fakeCLI(t, "claude", "exit 0\n")
	t.Setenv("HOME", t.TempDir()) // the daemon's own HOME is perfectly fine

	req := plannerRequest(t)
	req.RuntimeEnv = map[string]string{"HOME": filepath.Join(t.TempDir(), "isolated-home-that-was-never-prepared")}
	_, err := realPlanner("claude").Generate(context.Background(), req)
	if !errors.Is(err, ports.ErrPlannerRuntimeHomeUnreadable) {
		t.Fatalf("err=%v, want the OVERRIDDEN home to decide the preflight", err)
	}
}

// TestGenerate_ProviderErrorEnvelope_CarriesTheProviderReason is the core
// regression. The provider exits 1 and explains itself at the END of a long
// envelope; the failure AO records must contain that explanation.
func TestGenerate_ProviderErrorEnvelope_CarriesTheProviderReason(t *testing.T) {
	env := errorEnvelope(t, "error_during_execution", "401", "Invalid API key · Please run /login")
	fakeCLI(t, "claude", "echo '"+env+"'\nexit 1\n")
	t.Setenv("HOME", t.TempDir())

	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if err == nil {
		t.Fatal("want a failure")
	}
	if !strings.Contains(err.Error(), "Invalid API key") {
		t.Fatalf("the provider's own reason must survive into the durable error, got: %v", err)
	}
	if !errors.Is(err, ports.ErrPlannerAuthRequired) {
		t.Fatalf("err=%v, want ErrPlannerAuthRequired", err)
	}
	ev, _ := workflowcore.PlannerEvidenceFrom(err)
	if ev.ExitCode != 1 {
		t.Fatalf("exitCode=%d, want 1", ev.ExitCode)
	}
	if ev.ProviderSubtype != "error_during_execution" || ev.ProviderErrorStatus != "401" {
		t.Fatalf("evidence must carry the envelope's verdict fields: %+v", ev)
	}
	if ev.BinaryPath == "" {
		t.Fatalf("evidence must record which executable was run: %+v", ev)
	}
}

// TestGenerate_RateLimitEnvelope_IsLeftForTheSharedClassifier: a provider that
// is merely busy must NOT be claimed as a launch fault here, or the
// coordinator would retry it on the planner's small budget instead of parking
// it for capacity like every other provider call.
func TestGenerate_RateLimitEnvelope_IsLeftForTheSharedClassifier(t *testing.T) {
	env := errorEnvelope(t, "error_during_execution", "429", "Claude AI usage limit reached; try again later")
	fakeCLI(t, "claude", "echo '"+env+"'\nexit 1\n")
	t.Setenv("HOME", t.TempDir())

	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if errors.Is(err, ports.ErrPlannerAuthRequired) {
		t.Fatalf("a rate limit must never be reported as an auth failure: %v", err)
	}
	if !errors.Is(err, ports.ErrPlannerLaunchFailed) {
		t.Fatalf("err=%v, want the residual launch sentinel", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "usage limit") {
		t.Fatalf("the rate-limit text must reach the shared classifier: %v", err)
	}
}

// TestGenerate_ExitsImmediatelyWithNoOutput_IsRetryableNotAuth is §3's last
// distinction: "provider command launches but exits immediately".
func TestGenerate_ExitsImmediatelyWithNoOutput_IsRetryableNotAuth(t *testing.T) {
	fakeCLI(t, "claude", "exit 3\n")
	t.Setenv("HOME", t.TempDir())

	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerLaunchFailed) {
		t.Fatalf("err=%v, want ErrPlannerLaunchFailed", err)
	}
	ev, _ := workflowcore.PlannerEvidenceFrom(err)
	if ev.Classification != workflowcore.PlannerAttemptExitedEarly {
		t.Fatalf("classification=%q, want %q", ev.Classification, workflowcore.PlannerAttemptExitedEarly)
	}
	if ev.ExitCode != 3 {
		t.Fatalf("exitCode=%d, want 3", ev.ExitCode)
	}
}

// TestGenerate_UnsupportedInvocation_IsPermanent covers a CLI that refuses the
// flags AO's planner needs -- a version problem no retry fixes.
func TestGenerate_UnsupportedInvocation_IsPermanent(t *testing.T) {
	fakeCLI(t, "claude", "echo \"error: unknown option '--json-schema'\" >&2\nexit 2\n")
	t.Setenv("HOME", t.TempDir())

	_, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerUnsupportedInvocation) {
		t.Fatalf("err=%v, want ErrPlannerUnsupportedInvocation", err)
	}
}

// TestGenerate_SuccessfulRealSubprocess_RecordsLaunchProvenance is the
// successful preflight: a real process, a real plan, and the provenance a
// future incident will need.
func TestGenerate_SuccessfulRealSubprocess_RecordsLaunchProvenance(t *testing.T) {
	plan := `{"version":"v1","objective":"o","summary":"s","steps":[{"id":"1","title":"t","description":"d","dependencies":[],"acceptanceCriteria":["a"],"writeIntent":"mutating","verify":{"commands":[],"files":[]}}]}`
	env := fmt.Sprintf(`{"is_error":false,"subtype":"success","structured_output":%s}`, plan)
	cli := fakeCLI(t, "claude", "echo '"+env+"'\n")
	home := t.TempDir()
	t.Setenv("HOME", home)

	resp, err := realPlanner("claude").Generate(context.Background(), plannerRequest(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(resp.Plan.Steps) != 1 {
		t.Fatalf("plan=%+v", resp.Plan)
	}
	if resp.Evidence.BinaryPath != cli {
		t.Fatalf("binaryPath=%q, want %q", resp.Evidence.BinaryPath, cli)
	}
	if resp.Evidence.ProfileVar != "HOME" || resp.Evidence.ProfileDir != home {
		t.Fatalf("evidence must record the resolved profile: %+v", resp.Evidence)
	}
}

// TestBoundedOutput_KeepsTheTail is the unit-level statement of the incident:
// the diagnostic window must not discard the end of the output, because that
// is where a provider puts its verdict.
func TestBoundedOutput_KeepsTheTail(t *testing.T) {
	out := []byte(strings.Repeat("A", 2000) + "FATAL: the actual reason")
	got := boundedOutput(out)
	if !strings.Contains(got, "FATAL: the actual reason") {
		t.Fatalf("bounded output dropped the tail: %q", got)
	}
	if len(got) > providerReasonLimit+64 {
		t.Fatalf("bounded output is unbounded: %d bytes", len(got))
	}
}

// TestResolveExecutable_UsesTheSubprocessPath: os/exec.LookPath reads the
// daemon's PATH, which is the wrong one whenever a runtime env overrides it.
func TestResolveExecutable_UsesTheSubprocessPath(t *testing.T) {
	cli := fakeCLI(t, "claude", "exit 0\n")
	t.Setenv("PATH", "/nonexistent-daemon-path")

	got, err := resolveExecutable("claude", []string{"PATH=" + filepath.Dir(cli)})
	if err != nil {
		t.Fatalf("resolveExecutable: %v", err)
	}
	if got != cli {
		t.Fatalf("got %q, want %q", got, cli)
	}
	if _, err := resolveExecutable("claude", []string{"PATH="}); !errors.Is(err, ports.ErrPlannerBinaryMissing) {
		t.Fatalf("an empty subprocess PATH must be a binary-missing failure, got %v", err)
	}
}

// TestProfileDir_PrefersTheProviderOverride keeps the Codex planner on the same
// contract as the Claude one (§5): whichever binary AO_PLANNER_BIN names, the
// directory that decides its credentials is the one that is checked.
func TestProfileDir_PrefersTheProviderOverride(t *testing.T) {
	env := []string{"HOME=/home/u", "CLAUDE_CONFIG_DIR=/cfg/claude", "CODEX_HOME=/cfg/codex"}
	if k, v := profileDir("claude", env); k != "CLAUDE_CONFIG_DIR" || v != "/cfg/claude" {
		t.Fatalf("claude -> %s=%s", k, v)
	}
	if k, v := profileDir("/usr/local/bin/codex", env); k != "CODEX_HOME" || v != "/cfg/codex" {
		t.Fatalf("codex -> %s=%s", k, v)
	}
	if k, v := profileDir("some-other-planner", env); k != "HOME" || v != "/home/u" {
		t.Fatalf("unknown provider -> %s=%s", k, v)
	}
}

// TestGenerate_FallsBackToWellKnownInstallLocation is the asymmetry that made
// the planner the least reliable launch in AO: every agent adapter resolves its
// CLI through binaryutil (PATH, then ~/.local/bin, Homebrew, the Node version
// managers, the native installer), while the planner handed a bare name to exec
// and inherited whatever PATH the daemon was started with. A daemon started
// from the desktop app gets launchd's minimal PATH — so workers launched and
// the planner, alone, could not find the same binary.
func TestGenerate_FallsBackToWellKnownInstallLocation(t *testing.T) {
	plan := `{"version":"v1","objective":"o","summary":"s","steps":[{"id":"1","title":"t","description":"d","dependencies":[],"acceptanceCriteria":["a"],"writeIntent":"mutating","verify":{"commands":[],"files":[]}}]}`
	installed := fakeCLI(t, "claude", "echo '"+fmt.Sprintf(`{"is_error":false,"subtype":"success","structured_output":%s}`, plan)+"'\n")
	// The subprocess PATH is a directory that does NOT contain the CLI, which
	// is exactly the launchd case.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	p := realPlanner("claude")
	p.ResolveFallback = func(context.Context) (string, error) { return installed, nil }
	resp, err := p.Generate(context.Background(), plannerRequest(t))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.Evidence.BinaryPath != installed {
		t.Fatalf("binaryPath=%q, want the fallback-resolved %q", resp.Evidence.BinaryPath, installed)
	}
}

// TestGenerate_FallbackDoesNotMaskAGenuinelyMissingProvider: a fallback that
// also comes up empty must still produce the typed binary-missing failure, not
// a launch against an empty path.
func TestGenerate_FallbackDoesNotMaskAGenuinelyMissingProvider(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	p := realPlanner("claude")
	p.ResolveFallback = func(context.Context) (string, error) {
		return "", errors.New("agent: binary not found on PATH")
	}
	_, err := p.Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerBinaryMissing) {
		t.Fatalf("err=%v, want ErrPlannerBinaryMissing", err)
	}
}
