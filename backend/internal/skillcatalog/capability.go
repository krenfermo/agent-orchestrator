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
	// RequiresIsolation means this capability must not be exercised in the
	// daemon process or in an ordinary worker worktree. Only a runner that
	// attests isolation may carry it.
	RequiresIsolation bool
	// RequiresEgressControl means the runner must be able to actually
	// constrain outbound network to the declared allowlist.
	RequiresEgressControl bool
	// RequiredPermission is the AO RBAC permission a human must hold on the
	// project before this capability may be granted there.
	RequiredPermission domain.Permission
	// Description is shown to whoever approves the grant.
	Description string
}

// capabilitySpecs is the authoritative capability table.
var capabilitySpecs = map[Capability]CapabilitySpec{
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
	CapRepoWrite: {
		Risk:               RiskMedium,
		MinApproval:        ApprovalPerRun,
		RequiresIsolation:  true,
		RequiredPermission: domain.PermProjectManage,
		Description:        "Modify files in the project checkout.",
	},
	CapProcessExec: {
		Risk:               RiskHigh,
		MinApproval:        ApprovalPerRun,
		RequiresIsolation:  true,
		RequiredPermission: domain.PermProjectManage,
		Description:        "Run arbitrary subprocesses.",
	},
	CapSecretsRead: {
		Risk:               RiskHigh,
		MinApproval:        ApprovalPerRun,
		RequiresIsolation:  true,
		RequiredPermission: domain.PermSettingsManage,
		Description:        "Read named secrets from AO's secret store.",
	},
	CapNetEgress: {
		Risk:                  RiskHigh,
		MinApproval:           ApprovalPerRun,
		RequiresIsolation:     true,
		RequiresEgressControl: true,
		RequiredPermission:    domain.PermProjectManage,
		Description:           "Open outbound network connections, limited to the declared allowlist.",
	},
	CapNetActiveScan: {
		Risk:                  RiskCritical,
		MinApproval:           ApprovalPerTarget,
		RequiresIsolation:     true,
		RequiresEgressControl: true,
		RequiredPermission:    domain.PermSettingsManage,
		Description:           "Send probing traffic to an explicitly authorized target.",
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

// RunnerAttestation is what an execution runner claims it actually provides.
// It is supplied by the runner, never by the skill, and an absent runner
// attests nothing — which is why every isolation-requiring capability is
// refused until a real runner exists.
type RunnerAttestation struct {
	// RunnerID names the runner making the claim, for the audit record.
	RunnerID string
	// Isolated means the skill runs in a container or equivalent boundary
	// with its own filesystem view, not in the daemon process or a worker
	// worktree.
	Isolated bool
	// EgressControlled means outbound network is denied by default and only
	// the declared allowlist can be reached.
	EgressControlled bool
}

// NoRunner is the attestation AO has today: nothing is isolated and nothing
// constrains egress, because no skill runner is wired.
func NoRunner() RunnerAttestation { return RunnerAttestation{RunnerID: "none"} }

// DenialReason classifies why a capability was refused.
type DenialReason string

const (
	// DenyNotGranted is "the project never granted this capability".
	DenyNotGranted DenialReason = "not_granted"
	// DenyMissingPermission is "the requesting principal lacks the AO
	// permission this capability is gated on".
	DenyMissingPermission DenialReason = "missing_permission"
	// DenyNeedsIsolation is "no runner attests the isolation this capability
	// requires".
	DenyNeedsIsolation DenialReason = "needs_isolated_runner"
	// DenyNeedsEgressControl is "no runner attests it can constrain egress".
	DenyNeedsEgressControl DenialReason = "needs_egress_control"
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
}

func (d Denial) String() string {
	return fmt.Sprintf("%s: %s (%s)", d.Capability, d.Detail, d.Reason)
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
		if spec.RequiresIsolation && !req.Runner.Isolated {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyNeedsIsolation,
				Detail: "no runner attests an isolated execution environment",
			})
			continue
		}
		if spec.RequiresEgressControl && !req.Runner.EgressControlled {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyNeedsEgressControl,
				Detail: "no runner attests it can confine outbound network to the allowlist",
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
