package skills

import (
	"context"
	"fmt"
	"sort"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// dryrun.go — "what would this run need, and what is missing".
//
// This is the whole of what AO can honestly offer before an isolated runner
// exists, and it is useful on its own: it answers the question a person
// actually has when they look at a skill, without starting a process, opening a
// socket, or touching the workspace.
//
// Nothing in this file launches anything. It resolves the activation, checks
// the package on disk, and asks skillcatalog to authorize the run against the
// runner AO actually has. The runner AO actually has attests nothing.

// DryRunVerdict is the outcome a client renders.
type DryRunVerdict string

const (
	// DryRunExecutable means every requested capability is granted and
	// carried, and no further approval is outstanding.
	DryRunExecutable DryRunVerdict = "executable"
	// DryRunRequiresApproval means nothing is missing except a human saying
	// yes -- per run, or per named target.
	DryRunRequiresApproval DryRunVerdict = "requires_approval"
	// DryRunBlocked means at least one capability was refused. The reasons say
	// which and why.
	DryRunBlocked DryRunVerdict = "blocked"
)

// CapabilityDecision is one capability's outcome, in the order the mode
// requests them.
type CapabilityDecision struct {
	Capability skillcatalog.Capability
	// Satisfied is whether THIS capability passed every check on its own.
	//
	// It is per-capability diagnostics, not the verdict. Authorization is
	// fail-closed: if any one capability in the mode is unsatisfied the whole
	// run is refused, so a mode can be entirely blocked while most of its
	// capabilities report Satisfied. Reporting those as unsatisfied would tell
	// a user to grant something that is not the problem; Verdict is the field
	// that answers "can this run".
	Satisfied bool
	// Risk and Description come from AO's capability table, not from the
	// package: a manifest may not describe its own capability as cheaper than
	// it is.
	Risk        skillcatalog.RiskLevel
	Description string
	// RequiresControls is everything the execution environment must provide
	// for this capability.
	RequiresControls []skillcatalog.Control
	// MissingControl is the one it did not, set only when unsatisfied.
	MissingControl skillcatalog.Control
	// RequiredPermission is the AO permission this capability is gated on.
	RequiredPermission domain.Permission
	// DenialReason and Detail are set only when Granted is false.
	DenialReason skillcatalog.DenialReason
	Detail       string
}

// RunnerStatus is what the execution environment actually provides, and what
// this run would need from it.
type RunnerStatus struct {
	// RunnerID names the runner making the claim. "none" is AO today.
	RunnerID string
	// Available is whether any runner could carry this run at all.
	Available bool
	// Isolated and EgressControlled are coarse summaries of Controls, for a
	// compact UI. Controls is the real answer.
	Isolated         bool
	EgressControlled bool
	// Controls are the guarantees the environment has demonstrated.
	Controls []skillcatalog.Control
	// MissingControls are the guarantees this run needs and does not have.
	MissingControls []skillcatalog.Control
	// NeedsIsolation and NeedsEgressControl are what this run requires.
	NeedsIsolation     bool
	NeedsEgressControl bool
	// Unavailable explains an environmental refusal, when there is one.
	Unavailable string
}

// DryRun is the full answer.
type DryRun struct {
	SkillID   string
	Version   string
	SkillName string
	ModeID    string
	ModeName  string
	ModeRisk  skillcatalog.RiskLevel
	Verdict   DryRunVerdict
	Decisions []CapabilityDecision
	// MissingPermissions are the AO permissions the caller would need but does
	// not hold, deduplicated and sorted.
	MissingPermissions []domain.Permission
	// RequiredApproval is the strictest approval this run implies.
	RequiredApproval skillcatalog.ApprovalMode
	// EffectiveRisk is the highest risk among the capabilities this run needs.
	EffectiveRisk skillcatalog.RiskLevel
	Runner        RunnerStatus
	// Reasons are the structured blockers, one line each, for a client that
	// wants a summary rather than the per-capability table.
	Reasons []string
}

// containsControl reports membership without pulling in a generics helper for
// one call site.
func containsControl(list []skillcatalog.Control, want skillcatalog.Control) bool {
	for _, c := range list {
		if c == want {
			return true
		}
	}
	return false
}

// DryRunRequest asks what one mode would need on one project.
type DryRunRequest struct {
	ProjectID domain.ProjectID
	SkillID   string
	// ModeID may be empty only when the skill declares exactly one mode.
	ModeID string
	Inputs map[string]string
	// AuthorizedTargets are targets a human has already named for this run.
	// A per-target capability with none is refused.
	AuthorizedTargets []string
	// ActorPermissions are the caller's AO permissions on this project.
	ActorPermissions []domain.Permission
}

// DryRun resolves and authorizes a run WITHOUT executing it.
//
// It deliberately asks skillcatalog.Authorize against skillcatalog.NoRunner():
// the attestation is supplied by the execution environment, and AO has none.
// A caller cannot pass its own attestation in here -- a self-declared
// Isolated:true is exactly the claim this whole design refuses to accept as
// proof, so the only attestation this surface will use is the one AO can make
// truthfully.
func (s *Service) DryRun(ctx context.Context, req DryRunRequest) (DryRun, error) {
	resolved, err := s.Resolve(ctx, req.ProjectID, req.SkillID)
	if err != nil {
		return DryRun{}, err
	}
	manifest := resolved.Package.Manifest

	modeID := req.ModeID
	if modeID == "" && len(manifest.Modes) == 1 {
		modeID = manifest.Modes[0].ID
	}
	mode, ok := manifest.Mode(modeID)
	if !ok {
		return DryRun{}, apierr.Invalid("SKILL_MODE_UNKNOWN",
			fmt.Sprintf("%s has no mode %q", req.SkillID, req.ModeID), nil)
	}

	// The attestation comes from AO's own runner, never from the request.
	// DryRunRequest deliberately has no attestation field.
	runner := s.runner.Attestation()
	decision, _ := skillcatalog.Authorize(skillcatalog.AuthorizationRequest{
		Manifest:           manifest,
		ModeID:             mode.ID,
		Grant:              resolved.Activation.Grant,
		SubjectPermissions: req.ActorPermissions,
		Runner:             runner,
		AuthorizedTargets:  req.AuthorizedTargets,
	})

	out := DryRun{
		SkillID:          manifest.ID,
		Version:          resolved.Activation.Version,
		SkillName:        manifest.Name,
		ModeID:           mode.ID,
		ModeName:         mode.Name,
		ModeRisk:         mode.RiskLevel,
		RequiredApproval: skillcatalog.StricterApproval(manifest.Authorization.Approval, mode.Approval),
		EffectiveRisk:    decision.EffectiveRisk,
		Runner: RunnerStatus{
			RunnerID:         runner.RunnerID,
			Available:        runner.Isolated(),
			Isolated:         runner.Isolated(),
			EgressControlled: runner.EgressControlled(),
			Controls:         append([]skillcatalog.Control(nil), runner.Controls...),
			Unavailable:      s.runnerUnavailable,
		},
	}

	denials := map[skillcatalog.Capability]skillcatalog.Denial{}
	for _, d := range decision.Denials {
		denials[d.Capability] = d
	}
	missing := map[domain.Permission]bool{}

	for _, want := range mode.Capabilities {
		spec, known := want.Spec()
		entry := CapabilityDecision{
			Capability:         want,
			Risk:               spec.Risk,
			Description:        spec.Description,
			RequiredPermission: spec.RequiredPermission,
		}
		if known {
			entry.RequiresControls = append([]skillcatalog.Control(nil), spec.RequiresControls...)
			for _, need := range spec.RequiresControls {
				if !runner.Provides(need) && !containsControl(out.Runner.MissingControls, need) {
					out.Runner.MissingControls = append(out.Runner.MissingControls, need)
				}
				switch need {
				case skillcatalog.ControlFilesystemIsolation:
					out.Runner.NeedsIsolation = true
				case skillcatalog.ControlEgressAllowlist:
					out.Runner.NeedsEgressControl = true
				}
			}
			// The approval floor is a property of the capability, so it holds
			// even for a capability this run cannot currently carry.
			out.RequiredApproval = skillcatalog.StricterApproval(out.RequiredApproval, spec.MinApproval)
		}
		if d, denied := denials[want]; denied {
			entry.DenialReason = d.Reason
			entry.Detail = d.Detail
			entry.MissingControl = d.MissingControl
			if d.Reason == skillcatalog.DenyMissingPermission {
				missing[spec.RequiredPermission] = true
			}
			out.Reasons = append(out.Reasons, d.String())
		} else {
			entry.Satisfied = true
		}
		out.Decisions = append(out.Decisions, entry)
	}

	for p := range missing {
		out.MissingPermissions = append(out.MissingPermissions, p)
	}
	sort.Slice(out.MissingPermissions, func(i, j int) bool {
		return out.MissingPermissions[i] < out.MissingPermissions[j]
	})

	// Validate the inputs too, so a dry run catches a bad parameter rather
	// than deferring it to a run that cannot happen yet anyway.
	if err := skillcatalog.ValidateInputs(manifest, req.Inputs); err != nil {
		out.Reasons = append(out.Reasons, err.Error())
		out.Verdict = DryRunBlocked
		return out, nil
	}

	switch {
	case !decision.Allowed():
		out.Verdict = DryRunBlocked
	case out.RequiredApproval != skillcatalog.ApprovalNone &&
		out.RequiredApproval != skillcatalog.ApprovalPerActivation:
		// The activation already carries per-activation approval; anything
		// stricter is still outstanding at run time.
		out.Verdict = DryRunRequiresApproval
	default:
		out.Verdict = DryRunExecutable
	}
	return out, nil
}
