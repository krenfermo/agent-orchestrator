package skillcatalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// completeRunner is the attestation a hypothetical runner would make if it
// provided EVERY control. Nothing AO ships can honestly return this; it exists
// so a test can isolate one variable at a time against a fully-equipped
// environment. internal/skillrunner's real attestation is a strict subset.
func completeRunner() RunnerAttestation {
	return RunnerAttestation{RunnerID: "test-complete", Controls: AllControls()}
}

func testManifest(t *testing.T) Manifest {
	t.Helper()
	m, err := decode(t, validManifestYAML)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	return m
}

func denialFor(d Decision, c Capability) (Denial, bool) {
	for _, den := range d.Denials {
		if den.Capability == c {
			return den, true
		}
	}
	return Denial{}, false
}

func TestAuthorize_GrantsWhenEverythingLinesUp(t *testing.T) {
	m := testManifest(t)
	decision, err := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "quick",
		Grant:              Grant{Capabilities: []Capability{CapRepoRead, CapReportWrite}},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
		Runner:             NoRunner(),
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !decision.Allowed() || len(decision.Granted) != 2 {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.EffectiveRisk != RiskLow {
		t.Fatalf("effective risk = %q, want low", decision.EffectiveRisk)
	}
}

// The default is refusal. A capability the project never granted is denied
// even when the manifest asks for it and the principal could have granted it.
func TestAuthorize_DeniesAnUngrantedCapability(t *testing.T) {
	m := testManifest(t)
	decision, err := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "quick",
		Grant:              Grant{Capabilities: []Capability{CapRepoRead}},
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
		Runner:             completeRunner(),
	})
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("err = %v, want ErrCapabilityDenied", err)
	}
	den, ok := denialFor(decision, CapReportWrite)
	if !ok || den.Reason != DenyNotGranted {
		t.Fatalf("denial = %#v ok=%v", den, ok)
	}
	// Fail-closed means nothing is granted when anything is denied.
	if len(decision.Granted) != 0 {
		t.Fatalf("granted %v despite a denial", decision.Granted)
	}
}

// An empty permission set is the unauthenticated case, and it must grant
// nothing rather than fall through to a default.
func TestAuthorize_DeniesWhenThePrincipalHoldsNoPermissions(t *testing.T) {
	m := testManifest(t)
	decision, _ := Authorize(AuthorizationRequest{
		Manifest: m,
		ModeID:   "quick",
		Grant:    Grant{Capabilities: []Capability{CapRepoRead, CapReportWrite}},
		Runner:   completeRunner(),
	})
	if decision.Allowed() {
		t.Fatal("a principal with no permissions was authorized")
	}
	for _, c := range []Capability{CapRepoRead, CapReportWrite} {
		den, ok := denialFor(decision, c)
		if !ok || den.Reason != DenyMissingPermission {
			t.Fatalf("%s denial = %#v ok=%v", c, den, ok)
		}
	}
}

// This is the load-bearing one: without an environment that attests the
// controls, every capability that needs containment is refused, and the
// refusal names the control. It is what makes "a manifest is not a security
// boundary" true in code rather than in a comment.
func TestAuthorize_DeniesControlRequiringCapabilitiesWithoutARunner(t *testing.T) {
	cases := []struct {
		cap Capability
	}{
		{CapRepoWrite}, {CapProcessExec}, {CapSecretsRead}, {CapNetEgress}, {CapNetActiveScan},
	}
	for _, tc := range cases {
		t.Run(string(tc.cap), func(t *testing.T) {
			m := testManifest(t)
			m.Capabilities = []Capability{tc.cap}
			m.Authorization.Approval = ApprovalPerTarget
			m.Modes = []Mode{{ID: "only", Capabilities: []Capability{tc.cap}, Approval: ApprovalPerTarget}}
			decision, err := Authorize(AuthorizationRequest{
				Manifest: m,
				ModeID:   "only",
				Grant:    Grant{Capabilities: []Capability{tc.cap}},
				SubjectPermissions: []domain.Permission{
					domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage,
				},
				Runner:            NoRunner(),
				AuthorizedTargets: []string{"staging.example.com:443"},
			})
			if !errors.Is(err, ErrCapabilityDenied) {
				t.Fatalf("err = %v, want ErrCapabilityDenied", err)
			}
			den, ok := denialFor(decision, tc.cap)
			if !ok || den.Reason != DenyMissingControl {
				t.Fatalf("denial = %#v ok=%v", den, ok)
			}
			if den.MissingControl == "" {
				t.Fatalf("the refusal must name the control: %#v", den)
			}
			if !strings.Contains(den.Detail, string(den.MissingControl)) {
				t.Fatalf("detail should name the control: %q", den.Detail)
			}
		})
	}
}

// The five blocked capabilities each need a DIFFERENT control beyond
// confinement. An environment that confines perfectly but implements none of
// those four still unblocks nothing -- which is exactly the position AO is in
// after the phase-3 prototype.
func TestAuthorize_ConfinementAloneUnblocksNothing(t *testing.T) {
	confined := RunnerAttestation{
		RunnerID: "container/docker",
		Controls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits, ControlEgressDenyAll,
		},
	}
	if !confined.Isolated() {
		t.Fatal("this environment should read as isolated")
	}
	if confined.EgressControlled() {
		t.Fatal("deny-all was mistaken for an allowlist")
	}
	wantMissing := map[Capability]Control{
		CapRepoWrite:     ControlWritableWorkspace,
		CapProcessExec:   ControlArbitraryProcessExecution,
		CapSecretsRead:   ControlScopedSecretDelivery,
		CapNetEgress:     ControlEgressAllowlist,
		CapNetActiveScan: ControlEgressAllowlist,
	}
	for capability, wantControl := range wantMissing {
		m := testManifest(t)
		m.Capabilities = []Capability{capability}
		m.Authorization.Approval = ApprovalPerTarget
		m.Modes = []Mode{{ID: "only", Capabilities: []Capability{capability}, Approval: ApprovalPerTarget}}
		decision, err := Authorize(AuthorizationRequest{
			Manifest: m, ModeID: "only",
			Grant: Grant{Capabilities: []Capability{capability}},
			SubjectPermissions: []domain.Permission{
				domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage,
			},
			Runner:            confined,
			AuthorizedTargets: []string{"staging.example.com:443"},
		})
		if err == nil {
			t.Fatalf("%s was authorized by confinement alone", capability)
		}
		den, ok := denialFor(decision, capability)
		if !ok || den.MissingControl != wantControl {
			t.Fatalf("%s denial = %#v, want missing %s", capability, den, wantControl)
		}
	}
}

// An environment that can only switch the network OFF cannot carry a
// capability whose whole point is reaching declared destinations. Deny-all and
// allowlist are different claims.
func TestAuthorize_DeniesEgressWithoutAnAllowlist(t *testing.T) {
	m := testManifest(t)
	m.Capabilities = []Capability{CapNetEgress}
	m.Authorization.Approval = ApprovalPerRun
	m.Modes = []Mode{{ID: "only", Capabilities: []Capability{CapNetEgress}, Approval: ApprovalPerRun}}
	decision, _ := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "only",
		Grant:              Grant{Capabilities: []Capability{CapNetEgress}},
		SubjectPermissions: []domain.Permission{domain.PermProjectManage},
		Runner: RunnerAttestation{RunnerID: "deny-all-only", Controls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits, ControlEgressDenyAll,
		}},
	})
	den, ok := denialFor(decision, CapNetEgress)
	if !ok || den.Reason != DenyMissingControl || den.MissingControl != ControlEgressAllowlist {
		t.Fatalf("denial = %#v ok=%v", den, ok)
	}
}

// An active scan with no authorized target is refused even with a perfect
// runner and a full grant. "Who said this host may be attacked" is a separate
// question from "may this skill scan at all".
func TestAuthorize_DeniesActiveScanWithoutAnAuthorizedTarget(t *testing.T) {
	m := testManifest(t)
	m.Capabilities = []Capability{CapNetActiveScan}
	m.Authorization.Approval = ApprovalPerTarget
	m.Modes = []Mode{{ID: "only", Capabilities: []Capability{CapNetActiveScan}, Approval: ApprovalPerTarget}}
	decision, _ := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "only",
		Grant:              Grant{Capabilities: []Capability{CapNetActiveScan}},
		SubjectPermissions: []domain.Permission{domain.PermSettingsManage},
		Runner:             completeRunner(),
	})
	den, ok := denialFor(decision, CapNetActiveScan)
	if !ok || den.Reason != DenyTargetNotAuthorized {
		t.Fatalf("denial = %#v ok=%v", den, ok)
	}

	allowed, err := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "only",
		Grant:              Grant{Capabilities: []Capability{CapNetActiveScan}},
		SubjectPermissions: []domain.Permission{domain.PermSettingsManage},
		Runner:             completeRunner(),
		AuthorizedTargets:  []string{"staging.example.com:443"},
	})
	if err != nil || !allowed.Allowed() {
		t.Fatalf("named target should authorize: %v %#v", err, allowed)
	}
	if allowed.RequiredApproval != ApprovalPerTarget {
		t.Fatalf("required approval = %q, want per_target", allowed.RequiredApproval)
	}
}

// A capability's approval floor is set in code, so a manifest cannot lower it
// by asking politely.
func TestAuthorize_DeniesWhenTheManifestApprovalIsTooWeak(t *testing.T) {
	m := testManifest(t)
	m.Capabilities = []Capability{CapProcessExec}
	m.Authorization.Approval = ApprovalNone
	m.Modes = []Mode{{ID: "only", Capabilities: []Capability{CapProcessExec}, Approval: ApprovalNone}}
	decision, _ := Authorize(AuthorizationRequest{
		Manifest:           m,
		ModeID:             "only",
		Grant:              Grant{Capabilities: []Capability{CapProcessExec}},
		SubjectPermissions: []domain.Permission{domain.PermProjectManage},
		Runner:             completeRunner(),
	})
	den, ok := denialFor(decision, CapProcessExec)
	if !ok || den.Reason != DenyApprovalTooWeak {
		t.Fatalf("denial = %#v ok=%v", den, ok)
	}
}

func TestAuthorize_RejectsAnUnknownMode(t *testing.T) {
	m := testManifest(t)
	_, err := Authorize(AuthorizationRequest{Manifest: m, ModeID: "nope"})
	if !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}

// Every capability in the vocabulary must have a complete spec. A capability
// that reached the table with a zero-value permission would be gated on the
// empty permission, which nobody holds and nothing grants -- or worse, on a
// permission that does not exist.
func TestCapabilityTable_IsTotalAndUsesRealPermissions(t *testing.T) {
	known := map[domain.Permission]bool{}
	for _, p := range domain.AllPermissions {
		known[p] = true
	}
	caps := AllCapabilities()
	if len(caps) == 0 {
		t.Fatal("capability vocabulary is empty")
	}
	for _, c := range caps {
		spec, ok := c.Spec()
		if !ok {
			t.Fatalf("%s has no spec", c)
		}
		if !spec.Risk.Valid() {
			t.Fatalf("%s has invalid risk %q", c, spec.Risk)
		}
		if !spec.MinApproval.Valid() {
			t.Fatalf("%s has invalid approval %q", c, spec.MinApproval)
		}
		if !known[spec.RequiredPermission] {
			t.Fatalf("%s is gated on %q, which is not an AO permission", c, spec.RequiredPermission)
		}
		if strings.TrimSpace(spec.Description) == "" {
			t.Fatalf("%s has no description for the approver to read", c)
		}
		known := map[Control]bool{}
		for _, ctrl := range AllControls() {
			known[ctrl] = true
		}
		needs := map[Control]bool{}
		for _, need := range spec.RequiresControls {
			if !known[need] {
				t.Fatalf("%s requires %q, which is not a control AO defines", c, need)
			}
			needs[need] = true
		}
		// An allowlist without confinement would be a claim nothing can back:
		// a process that is not contained can always open its own socket.
		if needs[ControlEgressAllowlist] && !needs[ControlFilesystemIsolation] {
			t.Fatalf("%s requires an egress allowlist but not confinement", c)
		}
	}
}

// Nothing that needs a control may be reachable on the runner AO ships by
// default. This is the tripwire for a future change that quietly relaxes the
// table.
func TestNoRunner_CarriesNoControlRequiringCapability(t *testing.T) {
	runner := NoRunner()
	if len(runner.Controls) != 0 || runner.Isolated() || runner.EgressControlled() {
		t.Fatalf("NoRunner attests containment: %#v", runner)
	}
	for _, c := range AllCapabilities() {
		spec, _ := c.Spec()
		if len(spec.RequiresControls) == 0 {
			continue
		}
		m := testManifest(t)
		m.Capabilities = []Capability{c}
		m.Authorization.Approval = ApprovalPerTarget
		m.Modes = []Mode{{ID: "only", Capabilities: []Capability{c}, Approval: ApprovalPerTarget}}
		_, err := Authorize(AuthorizationRequest{
			Manifest: m,
			ModeID:   "only",
			Grant:    Grant{Capabilities: []Capability{c}},
			SubjectPermissions: []domain.Permission{
				domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage,
			},
			Runner:            runner,
			AuthorizedTargets: []string{"staging.example.com:443"},
		})
		if err == nil {
			t.Fatalf("%s was authorized with no runner", c)
		}
	}
}
