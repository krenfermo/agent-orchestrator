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
	// HostAgentControls is the ONE alternative to RequiresControls, and only
	// read capabilities have it (ADR 0010). It is the set of guarantees AO's
	// host agent executor must attest for a builtin or trusted skill's agent
	// mode to carry this capability without a container.
	//
	// It is a separate list, never a merge into RequiresControls, so the
	// container path is exactly as strict as it was: no container runner
	// attests a host-agent control, and no host agent attests a container
	// one. Nil means the capability has no host-agent path at all -- every
	// capability that writes, executes, reaches the network or reads a
	// secret -- and a host agent is refused it with a container control named.
	HostAgentControls []Control
	// RequiredPermission is the AO RBAC permission a human must hold on the
	// project before this capability may be granted there.
	RequiredPermission domain.Permission
	// Description is shown to whoever approves the grant.
	Description string
}

// capabilitySpecs is the authoritative capability table.
var capabilitySpecs = map[Capability]CapabilitySpec{
	// Reading is NOT free of controls.
	//
	// Phase 3 left repo.read and deps.read requiring nothing, on the reasoning
	// that AO already had the checkout so there was no boundary to attest.
	// That reasoning does not survive the question "restricted by what?".
	//
	// A skill that reads a repository is an agent plus tools reading
	// attacker-influenced content, and the only thing that would have confined
	// it outside a container is the agent CLI's own tool allowlist -- which
	// AGENTS.md records as void under bypassPermissions, and which is a launch
	// flag rather than a boundary. A prompt, a manifest and a CLI permission
	// are none of them a security frontier, so "just reading" gets the same
	// confinement as everything else: it runs inside the container or it does
	// not run.
	//
	// The distinction that survives is which controls each needs. Reading
	// needs the five the container prototype demonstrates; the five blocked
	// capabilities each need one more that does not exist yet.
	//
	// ADR 0010 adds exactly one exception, for repo.read alone: a builtin or
	// trusted skill's AGENT mode may read on the host when AO's agent executor
	// attests the four host-agent controls. The package trust is checked by the
	// service before this table is consulted; the table only says which
	// guarantees stand in for confinement, and deps.read deliberately has none.
	CapRepoRead: {
		Risk:               RiskLow,
		MinApproval:        ApprovalNone,
		RequiresControls:   confinementControls(),
		HostAgentControls:  hostAgentControls(),
		RequiredPermission: domain.PermProjectRead,
		Description:        "Read the project's source in a confined checkout.",
	},
	CapDepsRead: {
		Risk:               RiskLow,
		MinApproval:        ApprovalNone,
		RequiresControls:   confinementControls(),
		RequiredPermission: domain.PermProjectRead,
		Description:        "Read dependency manifests and lockfiles.",
	},
	// report.write is the one that genuinely needs nothing: it is AO storing a
	// document AO already holds, on AO's side of the boundary. It is listed
	// with no controls so a mode that only reports -- and reads nothing --
	// stays possible, not as a loophole for a mode that reads.
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

// confinementControls are the five a container boundary provides and that any
// skill work -- including reading -- must have. They are a function rather than
// a package var so no caller can append to the shared backing array.
func confinementControls() []Control {
	return []Control{
		ControlFilesystemIsolation,
		ControlProcessIsolation,
		ControlNoCredentialInheritance,
		ControlResourceLimits,
		ControlEgressDenyAll,
	}
}

// hostAgentControls are the four a host agent executor must demonstrate before
// it may read a project for a builtin or trusted skill (ADR 0010). They are
// NOT confinement and are named so nobody can mistake them for it: the agent
// is a same-user process on the host, and what bounds it is a copy it reads, a
// CLI configuration that removes every tool but reading and confines those to
// the copy, an environment with nothing of AO's in it, and a check afterwards
// that nothing it could reach was changed.
func hostAgentControls() []Control {
	return []Control{
		ControlStagedReadOnlyCopy,
		ControlAgentToolConfinement,
		ControlScrubbedEnvironment,
		ControlTamperDetection,
	}
}

// Spec returns the fixed policy for a capability.
func (c Capability) Spec() (CapabilitySpec, bool) {
	spec, ok := capabilitySpecs[c]
	return spec, ok
}

// ControlsFor is the control set this capability is judged by in the given
// environment: the host-agent alternative for a host agent that has one, the
// container set otherwise. It is what a dry run shows as "requires".
func (s CapabilitySpec) ControlsFor(runner RunnerAttestation) []Control {
	if runner.IsHostAgent() && len(s.HostAgentControls) > 0 {
		return s.HostAgentControls
	}
	return s.RequiresControls
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

	// The four host-agent controls (ADR 0010). Only AO's host agent executor
	// attests them, and only repo.read accepts them in place of confinement.

	// ControlStagedReadOnlyCopy is "the agent's working directory is an
	// AO-staged copy of the in-scope files, made read-only, never the checkout".
	ControlStagedReadOnlyCopy Control = "staged_read_only_copy"
	// ControlAgentToolConfinement is "the agent CLI was launched with only its
	// read tools, confined to the working directory, with no shell, no network
	// tool, no MCP server, no hook, no plugin and no bypass mode" -- verified
	// against the CLI AO actually resolved, not assumed from its name.
	ControlAgentToolConfinement Control = "agent_tool_confinement"
	// ControlScrubbedEnvironment is "the agent receives an allowlisted
	// environment: none of AO's variables, credentials or tokens".
	ControlScrubbedEnvironment Control = "scrubbed_environment"
	// ControlTamperDetection is "after the agent exits AO proves the staged copy
	// and the source files it came from are byte-identical to what was staged,
	// and trusts no output otherwise".
	ControlTamperDetection Control = "tamper_detection"
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
		ControlStagedReadOnlyCopy,
		ControlAgentToolConfinement,
		ControlScrubbedEnvironment,
		ControlTamperDetection,
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
	// DenyPackageNotTrusted is "the environment is a host agent and the package
	// is neither the builtin this binary embeds nor signature-trusted". A host
	// agent follows the package's instructions outside a container, so whose
	// instructions they are is part of the authorization (ADR 0010).
	DenyPackageNotTrusted DenialReason = "package_not_trusted"
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
//
// A capability with a host-agent alternative is satisfied by EITHER complete
// set, never by a mixture. When neither is complete the blocker is named from
// the set the environment is evidently trying to provide: a host agent is told
// which host-agent control it lacks, a container which container control.
func firstMissingControl(spec CapabilitySpec, runner RunnerAttestation) (Control, bool) {
	primary, ok := firstMissingIn(spec.RequiresControls, runner)
	if ok {
		return "", true
	}
	if len(spec.HostAgentControls) == 0 {
		return primary, false
	}
	alt, ok := firstMissingIn(spec.HostAgentControls, runner)
	if ok {
		return "", true
	}
	if runner.IsHostAgent() {
		return alt, false
	}
	return primary, false
}

func firstMissingIn(need []Control, runner RunnerAttestation) (Control, bool) {
	for _, c := range need {
		if !runner.Provides(c) {
			return c, false
		}
	}
	return "", true
}

// IsHostAgent reports whether this environment is attesting host-agent
// controls rather than container ones. It is derived from the controls, not
// declared, so there is no field a caller could set to claim it.
func (a RunnerAttestation) IsHostAgent() bool {
	for _, c := range hostAgentControls() {
		if a.Provides(c) {
			return true
		}
	}
	return false
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
	// PackageTrusted says the service verified the package is the builtin
	// this binary embeds (byte for byte) or an install AO verified a signature
	// for. It is consulted ONLY for a host-agent environment: a container's
	// controls do not depend on whose package it runs, and a host agent's do.
	// The zero value is untrusted, so a caller that forgets it is refused.
	PackageTrusted bool
	// CompositeParent authorizes the PARENT of a composite mode (ADR 0011):
	// grant, permission and approval are checked for the union of the composed
	// modes' capabilities, and controls are NOT, because the parent executes
	// nothing. Every composed mode is authorized again, fully, against its own
	// executor's attestation when it launches. Refused for any other mode.
	CompositeParent bool
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
		if req.CompositeParent != (mode.Executor == ExecutorComposite) {
			return Decision{}, invalidf("mode %q: a composite mode is authorized only as a composite parent, "+
				"and only a composite mode can be", mode.ID)
		}
	} else if req.CompositeParent {
		return Decision{}, invalidf("a composite parent names its mode")
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
	hostAgent := req.Runner.IsHostAgent()
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
		if req.CompositeParent {
			// Controls are the composed runs' business, checked at their launch.
			decision.Granted = append(decision.Granted, c)
			if spec.Risk.rank() > decision.EffectiveRisk.rank() {
				decision.EffectiveRisk = spec.Risk
			}
			decision.RequiredApproval = StricterApproval(decision.RequiredApproval, spec.MinApproval)
			continue
		}
		if hostAgent && len(spec.RequiresControls) > 0 && !req.PackageTrusted {
			decision.Denials = append(decision.Denials, Denial{
				Capability: c, Reason: DenyPackageNotTrusted,
				Detail: fmt.Sprintf("the execution environment (%s) is a host agent, which runs only "+
					"builtin or signature-trusted packages", runnerName(req.Runner)),
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
