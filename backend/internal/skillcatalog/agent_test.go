package skillcatalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// agent_test.go pins the 2C additions to the catalog (ADR 0010): the per-mode
// executor, the host-agent control set, and the trust the host agent needs.

func hostAgent() RunnerAttestation {
	return RunnerAttestation{RunnerID: "host-agent/claude-code", Controls: hostAgentControls()}
}

func TestExecutor_AbsentMeansToolSoExistingManifestsKeepTheirMeaning(t *testing.T) {
	m, err := decode(t, validManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Modes[0].EffectiveExecutor(); got != ExecutorTool {
		t.Fatalf("executor = %q, want tool", got)
	}
}

func TestExecutor_Validation(t *testing.T) {
	withExecutor := func(e string) string {
		return strings.Replace(validManifestYAML, "    guide: modes/quick.md", "    guide: modes/quick.md\n    executor: "+e, 1)
	}
	if m, err := decode(t, withExecutor("agent")); err != nil || m.Modes[0].EffectiveExecutor() != ExecutorAgent {
		t.Fatalf("agent: %v", err)
	}
	if _, err := decode(t, withExecutor("tool")); err != nil {
		t.Fatalf("tool: %v", err)
	}
	if _, err := decode(t, withExecutor("shell")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("an unknown executor was accepted: %v", err)
	}
	// An agent mode's only product is a report; one that cannot write one is
	// a manifest that makes no sense.
	noReport := strings.Replace(withExecutor("agent"),
		"    capabilities: [repo.read, report.write]\n    approval: per_activation\n    guide",
		"    capabilities: [repo.read]\n    approval: per_activation\n    guide", 1)
	if _, err := decode(t, noReport); !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), "report.write") {
		t.Fatalf("an agent mode without report.write was accepted: %v", err)
	}
}

func agentAuth(t *testing.T, caps []Capability, runner RunnerAttestation, trusted bool) (Decision, error) {
	t.Helper()
	m := testManifest(t)
	m.Capabilities = caps
	m.Modes[0].Capabilities = caps
	// Strict enough that approval is never the blocker: these tests are about
	// which CONTROLS carry a capability.
	m.Authorization.Approval = ApprovalPerRun
	m.Modes[0].Approval = ApprovalPerRun
	return Authorize(AuthorizationRequest{
		Manifest: m, ModeID: m.Modes[0].ID,
		Grant:              Grant{Capabilities: caps},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage},
		Runner:             runner,
		PackageTrusted:     trusted,
	})
}

// repo.read and report.write -- and nothing else -- are carried by a host
// agent for a trusted package.
func TestAuthorize_HostAgentCarriesOnlyReadingAndReporting(t *testing.T) {
	if _, err := agentAuth(t, []Capability{CapRepoRead, CapReportWrite}, hostAgent(), true); err != nil {
		t.Fatalf("read+report on a host agent: %v", err)
	}
	for _, c := range []Capability{CapDepsRead, CapRepoWrite, CapProcessExec, CapNetEgress, CapSecretsRead} {
		d, err := agentAuth(t, []Capability{CapRepoRead, CapReportWrite, c}, hostAgent(), true)
		if !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("%s was carried by a host agent", c)
		}
		if d.Denials[0].Capability != c || d.Denials[0].Reason != DenyMissingControl {
			t.Fatalf("%s denial = %+v", c, d.Denials)
		}
	}
}

// Whose instructions the host agent follows is part of the authorization.
func TestAuthorize_HostAgentRefusesAnUntrustedPackage(t *testing.T) {
	d, err := agentAuth(t, []Capability{CapRepoRead, CapReportWrite}, hostAgent(), false)
	if !errors.Is(err, ErrCapabilityDenied) || d.Denials[0].Reason != DenyPackageNotTrusted {
		t.Fatalf("decision = %+v err = %v", d, err)
	}
	// report.write needs no control, so it is not what is refused.
	if len(d.Denials) != 1 || d.Denials[0].Capability != CapRepoRead {
		t.Fatalf("denials = %+v", d.Denials)
	}
}

// The container path is exactly as strict as it was: its attestation never
// includes a host-agent control, trust is irrelevant to it, and a runner that
// mixes the two sets does not get repo.read from half of each.
func TestAuthorize_ContainerPathUnchangedAndSetsDoNotMix(t *testing.T) {
	if _, err := agentAuth(t, []Capability{CapRepoRead, CapReportWrite}, confiningRunner(), false); err != nil {
		t.Fatalf("the container path now depends on package trust: %v", err)
	}
	half := RunnerAttestation{RunnerID: "mixed", Controls: []Control{
		ControlFilesystemIsolation, ControlProcessIsolation, // half the container set
		ControlStagedReadOnlyCopy, ControlAgentToolConfinement, // half the host set
	}}
	d, err := agentAuth(t, []Capability{CapRepoRead}, half, true)
	if !errors.Is(err, ErrCapabilityDenied) || d.Denials[0].MissingControl != ControlScrubbedEnvironment {
		t.Fatalf("a mixed attestation carried repo.read or named the wrong blocker: %+v", d)
	}
	if !half.IsHostAgent() || confiningRunner().IsHostAgent() {
		t.Fatal("IsHostAgent is derived from the controls")
	}
}

func TestControlsFor_ReportsTheSetTheEnvironmentIsJudgedBy(t *testing.T) {
	spec, _ := CapRepoRead.Spec()
	if got := spec.ControlsFor(hostAgent()); len(got) != 4 || got[0] != ControlStagedReadOnlyCopy {
		t.Fatalf("host agent: %v", got)
	}
	if got := spec.ControlsFor(confiningRunner()); got[0] != ControlFilesystemIsolation {
		t.Fatalf("container: %v", got)
	}
	egress, _ := CapNetEgress.Spec()
	if got := egress.ControlsFor(hostAgent()); got[0] != ControlFilesystemIsolation {
		t.Fatalf("a capability with no host path must be judged by the container set: %v", got)
	}
}

// MatchesBuiltin is the only evidence of "builtin" a host agent acts on. A
// manifest that merely CLAIMS origin builtin, or the builtin's own guides
// under an edited manifest, are not the builtin.
func TestMatchesBuiltin(t *testing.T) {
	src := filepath.Join("packages", "security-audit")
	copyOf := func(t *testing.T) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "pkg")
		if err := CopyPackage(src, dir); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	if ok, err := MatchesBuiltin("security-audit", copyOf(t)); err != nil || !ok {
		t.Fatalf("an exact copy is the builtin: ok=%v err=%v", ok, err)
	}

	edited := copyOf(t)
	manifest := filepath.Join(edited, ManifestFileName)
	b, _ := os.ReadFile(manifest) //nolint:gosec // test.
	if err := os.WriteFile(manifest, []byte(strings.Replace(string(b), "riskLevel: low", "riskLevel: medium", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := MatchesBuiltin("security-audit", edited); ok {
		t.Fatal("an edited manifest over the builtin's files was taken for the builtin")
	}

	extra := copyOf(t)
	if err := os.WriteFile(filepath.Join(extra, "modes", "extra.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := MatchesBuiltin("security-audit", extra); ok {
		t.Fatal("a package with an extra file was taken for the builtin")
	}

	missing := copyOf(t)
	if err := os.Remove(filepath.Join(missing, "modes", "authz-review.md")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := MatchesBuiltin("security-audit", missing); ok {
		t.Fatal("a package missing a file was taken for the builtin")
	}

	if ok, _ := MatchesBuiltin("not-a-builtin", copyOf(t)); ok {
		t.Fatal("an unknown id matched")
	}
}

func TestSecurityAudit_AuthzReviewIsTheOneAgentMode(t *testing.T) {
	pkg, err := LoadPackage(securityAuditDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range pkg.Manifest.Modes {
		want := ExecutorTool
		if m.ID == "authz-review" {
			want = ExecutorAgent
		}
		if m.EffectiveExecutor() != want {
			t.Fatalf("mode %s executor = %s, want %s", m.ID, m.EffectiveExecutor(), want)
		}
	}
	schema, err := os.ReadFile(filepath.Join(securityAuditDir, CanonicalFindingsSchemaRef))
	if err != nil || string(schema) != string(CanonicalFindingsSchema()) {
		t.Fatal("the canonical schema is not the package's schema")
	}
}
