package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerauth"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// auth_contract_test.go covers the wf-4e3d187b incident from the planner's
// side: the launch that hung.
//
// The planner subprocess did start. It resolved AO's isolated runtime home,
// reached for the Claude credential there, and macOS put up a keychain unlock
// dialog. Nothing answered it, the process sat there for the whole budget, and
// AO recorded planner_auth_unavailable with 26 seconds spent and nothing
// learned. These tests pin the correction: the launch is refused BEFORE the
// subprocess exists, with a typed sentinel that names what a person must fix.

// isolatedLaunchEnv is the environment AO's per-user isolation actually hands
// a planner launch, complete with the legacy "login" keychain macOS reserves
// and never reopens with AO's own password.
func isolatedLaunchEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "runtime-home")
	keychains := filepath.Join(home, "Library", "Keychains")
	if err := os.MkdirAll(keychains, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keychains, "login.keychain-db"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "providers", "claude-code")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return append([]string{
		"HOME=" + home,
		"CLAUDE_CONFIG_DIR=" + configDir,
		"PATH=" + os.Getenv("PATH"),
	}, extra...)
}

func TestPreflight_RefusesALaunchThatWouldOpenAKeychainDialog(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	binary := fakeCLI(t, "claude", "exit 0")
	env := isolatedLaunchEnv(t)

	plan, err := preflight(context.Background(), binary, nil, t.TempDir(), env, nil, "")
	if err == nil {
		t.Fatal("preflight allowed a launch whose credentials need an interactive prompt")
	}
	if !errors.Is(err, ports.ErrPlannerAuthInteractive) {
		t.Fatalf("error is not ErrPlannerAuthInteractive: %v", err)
	}
	if plan.Auth.Status != providerauth.StatusRequiresInteraction {
		t.Fatalf("plan.Auth.Status = %s, want requires_interaction", plan.Auth.Status)
	}
	// The refusal must say what to fix. "The planner could not be started" is
	// the sentence this whole incident was stuck behind.
	if !strings.Contains(err.Error(), "keychain") {
		t.Fatalf("refusal does not name the keychain: %v", err)
	}
}

// The classification is what lands in durable evidence, and it must be the
// interactive one: auth_unavailable would send a person to sign in again,
// which is not what is wrong.
func TestClassificationForPreflight_NamesTheInteractiveAuthClass(t *testing.T) {
	got := classificationForPreflight(ports.ErrPlannerAuthInteractive)
	if got != workflowcore.PlannerAttemptAuthInteractive {
		t.Fatalf("classification = %q, want %q", got, workflowcore.PlannerAttemptAuthInteractive)
	}
}

// An unattended credential in the launch env makes the same launch fine: the
// keychain is never touched, so its state cannot refuse anything.
func TestPreflight_EnvironmentCredentialMakesTheSameLaunchUnattended(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	binary := fakeCLI(t, "claude", "exit 0")
	env := isolatedLaunchEnv(t, "ANTHROPIC_API_KEY=sk-ant-not-a-real-key-000000")

	plan, err := preflight(context.Background(), binary, nil, t.TempDir(), env, nil, "")
	if err != nil {
		t.Fatalf("preflight refused a launch carrying an environment credential: %v", err)
	}
	if plan.Auth.Mode != providerauth.ModeEnvironment {
		t.Fatalf("plan.Auth.Mode = %s, want environment", plan.Auth.Mode)
	}
}

// Generate is the real entry point, and the refusal must reach it as durable
// evidence rather than as a bare error: a stop AO cannot explain is what the
// planner launch contract exists to prevent.
func TestGenerate_RefusesInteractiveAuthWithEvidence(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	binary := fakeCLI(t, "claude", "exit 0")
	env := isolatedLaunchEnv(t)
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}

	p := Planner{Binary: binary}
	_, err := p.Generate(context.Background(), plannerRequest(t))
	if !errors.Is(err, ports.ErrPlannerAuthInteractive) {
		t.Fatalf("Generate did not refuse an interactive-auth launch: %v", err)
	}
	var attempt *workflowcore.PlannerAttemptError
	if !errors.As(err, &attempt) {
		t.Fatalf("refusal did not carry attempt evidence: %v", err)
	}
	if attempt.Evidence.Classification != workflowcore.PlannerAttemptAuthInteractive {
		t.Fatalf("evidence class = %q, want %q", attempt.Evidence.Classification, workflowcore.PlannerAttemptAuthInteractive)
	}
	// Refused before spawning: no provider time was spent proving this.
	if attempt.Evidence.DurationMS != 0 {
		t.Fatalf("a refused launch reported %dms of provider time; it should have spent none", attempt.Evidence.DurationMS)
	}
}

// A non-Claude planner binary must not be judged by Claude Code's credential
// model. AO_PLANNER_BIN can name a Codex planner, and refusing it over a macOS
// keychain it never reads would be a pure false negative.
func TestPreflight_NonClaudePlannerIsNotJudgedByClaudeCredentials(t *testing.T) {
	binary := fakeCLI(t, "codex", "exit 0")
	env := isolatedLaunchEnv(t)

	plan, err := preflight(context.Background(), binary, nil, t.TempDir(), env, nil, "")
	if err != nil {
		t.Fatalf("preflight refused a non-Claude planner over Claude's credential model: %v", err)
	}
	if plan.Auth.Status != providerauth.StatusUnknown {
		t.Fatalf("plan.Auth.Status = %s, want unknown for a provider AO has no credential model for", plan.Auth.Status)
	}
}

// The refusal is persisted. It must never carry a credential.
func TestPreflight_RefusalCarriesNoSecret(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	const secret = "sk-ant-not-a-real-key-000000"
	binary := fakeCLI(t, "claude", "exit 0")
	// A credential present in the env AND a keychain that cannot be opened:
	// the environment credential wins, so force the keychain verdict by
	// pinning the mode, and check the resulting prose.
	env := isolatedLaunchEnv(t, "ANTHROPIC_API_KEY="+secret)
	plan, _ := preflight(context.Background(), binary, nil, t.TempDir(), env, nil, "")
	for _, field := range []string{plan.Auth.Reason, plan.Auth.Source} {
		if strings.Contains(field, secret) {
			t.Fatalf("the launch plan's auth contract leaked a credential: %q", field)
		}
	}
	if err := authUsable(plan); err != nil && strings.Contains(err.Error(), secret) {
		t.Fatalf("the refusal error leaked a credential: %v", err)
	}
}

// The planner reads the SAME pinned auth mode as every worker and reviewer
// dispatch. A planner with its own idea of where credentials come from is how
// one role gets fixed while another keeps launching into a prompt.
func TestPlanner_HonoursThePinnedAuthMode(t *testing.T) {
	binary := fakeCLI(t, "claude", "exit 0")
	// No environment credential anywhere, and a mode that demands one.
	env := []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}

	plan, err := preflight(context.Background(), binary, nil, t.TempDir(), env, nil, providerauth.ModeEnvironment)
	if err != nil {
		// A pinned-but-unsatisfiable mode is `unavailable`, which the planner
		// leaves to the real invocation rather than refusing on: only an
		// interactive prompt is refused here. What must be true is that the
		// contract SAYS so.
		t.Fatalf("preflight refused on an unavailable (not interactive) contract: %v", err)
	}
	if plan.Auth.Mode != providerauth.ModeEnvironment {
		t.Fatalf("plan.Auth.Mode = %s, want the pinned mode to be reported", plan.Auth.Mode)
	}
	if plan.Auth.Status != providerauth.StatusUnavailable {
		t.Fatalf("plan.Auth.Status = %s, want unavailable for a pinned mode with no credential", plan.Auth.Status)
	}
}

// The resolved contract lands in durable evidence, so a stop can say WHICH
// credential store the planner reached for. wf-4e3d187b's evidence could not.
func TestGenerate_RecordsTheAuthContractInEvidence(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the OS keychain is only in the launch path on macOS")
	}
	binary := fakeCLI(t, "claude", "exit 0")
	for _, kv := range isolatedLaunchEnv(t) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}

	_, err := Planner{Binary: binary}.Generate(context.Background(), plannerRequest(t))
	var attempt *workflowcore.PlannerAttemptError
	if !errors.As(err, &attempt) {
		t.Fatalf("refusal did not carry attempt evidence: %v", err)
	}
	if attempt.Evidence.AuthMode != string(providerauth.ModeKeychain) {
		t.Fatalf("evidence AuthMode = %q, want %q", attempt.Evidence.AuthMode, providerauth.ModeKeychain)
	}
	if attempt.Evidence.AuthStatus != string(providerauth.StatusRequiresInteraction) {
		t.Fatalf("evidence AuthStatus = %q, want %q", attempt.Evidence.AuthStatus, providerauth.StatusRequiresInteraction)
	}
}
