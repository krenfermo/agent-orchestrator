package skillcatalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Capability is one thing a skill asks to be able to do. It is a closed
// vocabulary: an unknown capability is rejected, never ignored, because
// ignoring it would silently downgrade a package that believed it had asked
// for something.
type Capability string

const (
	// CapRepoRead reads the project's source in a checkout.
	CapRepoRead Capability = "repo.read"
	// CapRepoWrite modifies files in the project checkout.
	CapRepoWrite Capability = "repo.write"
	// CapDepsRead reads dependency manifests and lockfiles, and consults
	// locally available advisory data. It does not imply network access; a
	// skill that wants to reach an advisory API also needs net.egress.
	CapDepsRead Capability = "deps.read"
	// CapSecretsRead reads named secrets from AO's secret store.
	CapSecretsRead Capability = "secrets.read"
	// CapNetEgress opens outbound network connections.
	CapNetEgress Capability = "net.egress"
	// CapNetActiveScan sends traffic intended to probe or exercise a target's
	// weaknesses, rather than merely fetch from it.
	CapNetActiveScan Capability = "net.active_scan"
	// CapProcessExec runs arbitrary subprocesses.
	CapProcessExec Capability = "process.exec"
	// CapReportWrite writes a structured report into AO's evidence store.
	CapReportWrite Capability = "report.write"
)

// CapabilitySpec is the fixed policy for one capability. It lives in code
// rather than in the manifest on purpose: a package must not be able to
// describe its own capability as cheaper than it is.
type CapabilitySpec struct {
	// Risk is the inherent risk of the capability.
	Risk RiskLevel
	// MinApproval is the weakest approval mode AO will accept for it. A
	// manifest may demand more; it can never accept less.
	MinApproval ApprovalMode
	// RequiresControls are the execution-environment guarantees that must ALL
	// be attested before this capability may be carried. An empty list means
	// the capability needs nothing from the runner -- reading a file AO
	// already handed over, or writing a report AO stores itself.
	//
	// These are the controls the capability actually needs, not a blanket
	// "isolated": net.egress needs an allowlist that repo.write does not, and
	// process.exec needs an image contract that neither of them does.
	RequiresControls []Control
	// RequiredPermission is the AO RBAC permission a human must hold on the
	// project before this capability may be granted there.
	RequiredPermission domain.Permission
	// Description is shown to whoever approves the grant.
	Description string
}

// capabilitySpecs is the authoritative capability table.
var capabilitySpecs = map[Capability]CapabilitySpec{
	// The three that need nothing from a runner: AO hands over the checkout
	// and stores the report itself, so there is no boundary to attest.
	CapRepoRead: {
		Risk:               RiskLow,
		MinApproval:        ApprovalNone,
		RequiredPermission: domain.PermProjectRead,
		Description:        "Read the project's source in a checkout.",
	},
	CapDepsRead: {
		Risk:               RiskLow,
		MinApproval:        ApprovalNone,
		RequiredPermission: domain.PermProjectRead,
		Description:        "Read dependency manifests and lockfiles.",
	},
	CapReportWrite: {
		Risk:               RiskLow,
		MinApproval:        ApprovalNone,
		RequiredPermission: domain.PermProjectRead,
		Description:        "Write a structured report into AO's evidence store.",
	},

	// Everything below needs a boundary. Each names the controls it actually
	// depends on, so a refusal can say which one is missing.
	CapRepoWrite: {
		Risk:        RiskMedium,
		MinApproval: ApprovalPerRun,
		RequiresControls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits,
			// Confinement is not enough: AO must also be able to return the
			// changes to the host under review.
			ControlWritableWorkspace,
		},
		RequiredPermission: domain.PermProjectManage,
		Description:        "Modify files in the project checkout.",
	},
	CapProcessExec: {
		Risk:        RiskHigh,
		MinApproval: ApprovalPerRun,
		RequiresControls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits,
			// A sandbox says where a command may run. It does not say which
			// command, shipped by whom, built how -- that is the skill-image
			// contract, and it is a separate control.
			ControlArbitraryProcessExecution,
		},
		RequiredPermission: domain.PermProjectManage,
		Description:        "Run arbitrary subprocesses.",
	},
	CapSecretsRead: {
		Risk:        RiskHigh,
		MinApproval: ApprovalPerRun,
		RequiresControls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits,
			ControlScopedSecretDelivery,
		},
		RequiredPermission: domain.PermSettingsManage,
		Description:        "Read named secrets from AO's secret store.",
	},
	CapNetEgress: {
		Risk:        RiskHigh,
		MinApproval: ApprovalPerRun,
		RequiresControls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits,
			// Deny-all is deliberately NOT listed: an environment that can
			// only switch the network off cannot carry a capability whose
			// whole point is reaching declared destinations.
			ControlEgressAllowlist,
		},
		RequiredPermission: domain.PermProjectManage,
		Description:        "Open outbound network connections, limited to the declared allowlist.",
	},
	CapNetActiveScan: {
		Risk:        RiskCritical,
		MinApproval: ApprovalPerTarget,
		RequiresControls: []Control{
			ControlFilesystemIsolation, ControlProcessIsolation,
			ControlNoCredentialInheritance, ControlResourceLimits,
			ControlEgressAllowlist, ControlArbitraryProcessExecution,
		},
		RequiredPermission: domain.PermSettingsManage,
		Description:        "Send probing traffic to an explicitly authorized target.",
	},
}

// Spec returns the fixed policy for a capability.
func (c Capability) Spec() (CapabilitySpec, bool) {
	spec, ok := capabilitySpecs[c]
	return spec, ok
}

// Valid reports whether the capability is in AO's vocabulary.
func (c Capability) Valid() bool {
	_, ok := capabilitySpecs[c]
	return ok
}

// AllCapabilities returns the vocabulary in a stable order, for UI and tests.
func AllCapabilities() []Capability {
	out := make([]Capability, 0, len(capabilitySpecs))
	for c := range capabilitySpecs {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func validateCapabilityList(field string, caps []Capability) error {
	if len(caps) == 0 {
		return invalidf("%s must declare at least one capability", field)
	}
	seen := map[Capability]bool{}
	for _, c := range caps {
		if !c.Valid() {
			return invalidf("%s contains unknown capability %q", field, c)
		}
		if seen[c] {
			return invalidf("%s lists %q twice", field, c)
		}
		seen[c] = true
	}
	return nil
}

// Control is one named guarantee an execution environment either provides or
// does not. Containment used to be two booleans here (isolated, egress
// controlled) and that was too coarse: "the process is confined" and "this
// skill may write to your repository" are different claims, and one boolean
// cannot carry both. Naming each control lets a runner attest exactly the
// subset it has actually demonstrated, and lets a refusal say which control is
// missing rather than "not isolated".
type Control string

const (
	// ControlFilesystemIsolation is "the run sees only the inputs AO mounted".
	// Nothing else on the host filesystem is reachable.
	ControlFilesystemIsolation Control = "filesystem_isolation"
	// ControlProcessIsolation is "the run's processes are contained and die
	// with it" -- its own PID namespace, no orphan surviving teardown.
	ControlProcessIsolation Control = "process_isolation"
	// ControlNoCredentialInheritance is "the run receives none of AO's
	// environment": no provider token, no agent credential, no daemon env.
	ControlNoCredentialInheritance Control = "no_credential_inheritance" //nolint:gosec // G101: a control NAME, and the one that says no credential is passed.
	// ControlResourceLimits is "CPU, memory, process count, wall clock and
	// output size are all bounded and enforced by the kernel".
	ControlResourceLimits Control = "resource_limits"
	// ControlEgressDenyAll is "the run cannot open any outbound connection".
	ControlEgressDenyAll Control = "egress_deny_all"
	// ControlEgressAllowlist is "outbound traffic is limited to the declared
	// destinations". It is a STRICTER and DIFFERENT claim from deny-all: an
	// environment that provides deny-all provides no allowlist, because there
	// is nothing to allow. Conflating them is the mistake ADR 0004 exists to
	// avoid.
	ControlEgressAllowlist Control = "egress_allowlist"
	// ControlWritableWorkspace is "the run may modify a workspace and AO can
	// return those changes to the host under review".
	ControlWritableWorkspace Control = "writable_workspace"
	// ControlScopedSecretDelivery is "AO can hand the run a named secret
	// scoped to it, without an environment variable and without inheritance".
	ControlScopedSecretDelivery Control = "scoped_secret_delivery" //nolint:gosec // G101: a control name, not a secret.
	// ControlArbitraryProcessExecution is "a skill may run commands it
	// authored". It needs the skill-image contract -- what a skill may ship,
	// how it is built, how its command is pinned -- not merely a sandbox.
	ControlArbitraryProcessExecution Control = "arbitrary_process_execution"
)

// AllControls is every control in a stable order, for the runtime matrix and
// for tests that assert the capability table only names controls that exist.
func AllControls() []Control {
	return []Control{
		ControlFilesystemIsolation,
		ControlProcessIsolation,
		ControlNoCredentialInheritance,
		ControlResourceLimits,
		ControlEgressDenyAll,
		ControlEgressAllowlist,
		ControlWritableWorkspace,
		ControlScopedSecretDelivery,
		ControlArbitraryProcessExecution,
	}
}

// RunnerAttestation is what an execution runner has DEMONSTRATED it provides.
// It is produced by the runner from its own probes, never supplied by a caller
// and never read off a manifest: a self-declared guarantee is the claim this
// whole design refuses to accept as proof.
type RunnerAttestation struct {
	// RunnerID names the runner making the claim, for the audit record.
	RunnerID string
	// Controls are the guarantees this environment provides. A control absent
	// here is a control AO does not have, whatever the runtime may in
	// principle be capable of.
	Controls []Control
}

// Provides reports whether this environment attests one control.
func (a RunnerAttestation) Provides(c Control) bool {
	for _, got := range a.Controls {
		if got == c {
			return true
		}
	}
	return false
}

// Isolated reports whether the environment confines the filesystem, the
// process tree and AO's credentials -- the three that together mean "this is
// not running in the daemon or a worker worktree".
func (a RunnerAttestation) Isolated() bool {
	return a.Provides(ControlFilesystemIsolation) &&
		a.Provides(ControlProcessIsolation) &&
		a.Provides(ControlNoCredentialInheritance)
}

// EgressControlled reports whether outbound traffic can be limited to declared
// destinations. Deny-all alone is deliberately NOT enough.
func (a RunnerAttestation) EgressControlled() bool {
	return a.Provides(ControlEgressAllowlist)
}

// NoRunner is the attestation AO makes when no runner is wired: it provides
// nothing, so every capability requiring any control is refused.
func NoRunner() RunnerAttestation { return RunnerAttestation{RunnerID: "none"} }

// DenialReason classifies why a capability was refused.
type DenialReason string

const (
	// DenyNotGranted is "the project never granted this capability".
	DenyNotGranted DenialReason = "not_granted"
	// DenyMissingPermission is "the requesting principal lacks the AO
	// permission this capability is gated on".
	DenyMissingPermission DenialReason = "missing_permission"
	// DenyMissingControl is "the execution environment does not attest a
	// control this capability requires". The denial names the control, so a
	// refusal says what has to be built rather than "not isolated".
	DenyMissingControl DenialReason = "missing_control"
	// DenyApprovalTooWeak is "the manifest's approval mode is weaker than the
	// capability's floor".
	DenyApprovalTooWeak DenialReason = "approval_too_weak"
	// DenyUnknownCapability is "this is not in AO's vocabulary".
	DenyUnknownCapability DenialReason = "unknown_capability"
	// DenyTargetNotAuthorized is "a per-target capability was requested with
	// no explicitly authorized target".
	DenyTargetNotAuthorized DenialReason = "target_not_authorized"
)

// Denial is one refused capability.
type Denial struct {
	Capability Capability
	Reason     DenialReason
	Detail     string
	// MissingControl is set when Reason is DenyMissingControl: the specific
	// guarantee the execution environment did not attest.
	MissingControl Control
}

func (d Denial) String() string {
	return fmt.Sprintf("%s: %s (%s)", d.Capability, d.Detail, d.Reason)
}

// firstMissingControl returns the first required control the environment does
// not attest. Controls are checked in the order the capability declares them,
// so the reported blocker is stable rather than map-order dependent.
func firstMissingControl(spec CapabilitySpec, runner RunnerAttestation) (Control, bool) {
	for _, need := range spec.RequiresControls {
		if !runner.Provides(need) {
			return need, false
		}
	}
	return "", true
}

func runnerName(runner RunnerAttestation) string {
	if strings.TrimSpace(runner.RunnerID) == "" {
		return "none"
	}
	return runner.RunnerID
}

// ErrCapabilityDenied is returned whenever any requested capability is
// refused. The decision carries the full list; the error exists so a caller
// that only checks err cannot accidentally proceed.
var ErrCapabilityDenied = errors.New("skillcatalog: capability denied")

// Decision is the outcome of resolving one run's capability request.
type Decision struct {
	// Granted are the capabilities that survived every check, in the order
	// they were requested.
	Granted []Capability
	// Denials are every refusal, one per capability.
	Denials []Denial
	// RequiredApproval is the strictest approval mode implied by the granted
	// set and the manifest. A caller must obtain this before running.
	RequiredApproval ApprovalMode
	// EffectiveRisk is the highest risk among the granted capabilities.
	EffectiveRisk RiskLevel
}

// Allowed reports whether nothing was denied.
func (d Decision) Allowed() bool { return len(d.Denials) == 0 }

// Err returns ErrCapabilityDenied with every denial spelled out, or nil.
func (d Decision) Err() error {
	if d.Allowed() {
		return nil
	}
	parts := make([]string, 0, len(d.Denials))
	for _, den := range d.Denials {
		parts = append(parts, den.String())
	}
	return fmt.Errorf("%w: %s", ErrCapabilityDenied, strings.Join(parts, "; "))
}

// AuthorizationRequest is everything needed to decide one run.
type AuthorizationRequest struct {
	// Manifest is the installed skill's manifest.
	Manifest Manifest
	// ModeID selects which mode is being run. Empty means the whole skill's
	// capability set, which is only correct for a single-mode skill.
	ModeID string
	// Grant is what the project actually granted at activation time.
	Grant Grant
	// SubjectPermissions are the AO permissions the requesting principal holds
	// on this project. Fail-closed: an empty set grants nothing.
	SubjectPermissions []domain.Permission
	// Runner is what the execution environment attests. The zero value
	// attests nothing.
	Runner RunnerAttestation
	// AuthorizedTargets are targets a human explicitly approved for this run.
	// A per-target capability with no matching target is refused.
	AuthorizedTargets []string
}

// Authorize resolves a run's capability request fail-closed: a capability is
// granted only when every check passes, and any single denial fails the whole
// decision. It never partially proceeds.
func Authorize(req AuthorizationRequest) (Decision, error) {
	requested := req.Manifest.Capabilities
	approval := req.Manifest.Authorization.Approval
	if req.ModeID != "" {
		mode, ok := req.Manifest.Mode(req.ModeID)
		if !ok {
			return Decision{}, invalidf("skill %q has no mode %q", req.Manifest.ID, req.ModeID)
		}
		requested = mode.Capabilities
		approval = StricterApproval(approval, mode.Approval)
	}

	granted := map[Capability]bool{}
	for _, c := range req.Grant.Capabilities {
		granted[c] = true
	}
	held := map[domain.Permission]bool{}
	for _, p := range req.SubjectPermissions {
		held[p] = true
	}

	decision := Decision{RequiredApproval: approval}
	for _, c := range requested {
		spec, ok := c.Spec()
		if !ok {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyUnknownCapability,
				Detail: "not in AO's capability vocabulary",
			})
			continue
		}
		if !granted[c] {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyNotGranted,
				Detail: "the project did not grant this capability when the skill was enabled",
			})
			continue
		}
		if !held[spec.RequiredPermission] {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyMissingPermission,
				Detail: fmt.Sprintf("requires the %s permission on this project", spec.RequiredPermission),
			})
			continue
		}
		if approval.rank() < spec.MinApproval.rank() {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyApprovalTooWeak,
				Detail: fmt.Sprintf("requires approval %s, manifest asks for %s", spec.MinApproval, approval),
			})
			continue
		}
		if missing, ok := firstMissingControl(spec, req.Runner); !ok {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyMissingControl, MissingControl: missing,
				Detail: fmt.Sprintf("the execution environment (%s) does not provide %s",
					runnerName(req.Runner), missing),
			})
			continue
		}
		if spec.MinApproval == ApprovalPerTarget && len(req.AuthorizedTargets) == 0 {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyTargetNotAuthorized,
				Detail: "no explicitly authorized target was supplied for this run",
			})
			continue
		}
		decision.Granted = append(decision.Granted, c)
		if spec.Risk.rank() > decision.EffectiveRisk.rank() {
			decision.EffectiveRisk = spec.Risk
		}
		decision.RequiredApproval = StricterApproval(decision.RequiredApproval, spec.MinApproval)
	}

	if !decision.Allowed() {
		// Nothing is granted when anything is denied: a half-authorized run is
		// a run whose declared scope no longer describes it.
		decision.Granted = nil
		decision.EffectiveRisk = ""
		return decision, decision.Err()
	}
	return decision, nil
}
