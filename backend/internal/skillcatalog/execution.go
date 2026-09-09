package skillcatalog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrNoRunner is returned by every execution path in this phase. AO has no
// isolated skill runner, and a runner that ran a high-risk skill in the
// daemon's own process while calling itself a sandbox would be worse than no
// runner at all.
var ErrNoRunner = errors.New("skillcatalog: no skill runner is available")

// RunRequest is one request to run a skill mode on a project.
type RunRequest struct {
	ProjectID domain.ProjectID
	SkillID   string
	ModeID    string
	// Inputs are the caller-supplied values for the manifest's declared
	// inputs.
	Inputs map[string]string
	// AuthorizedTargets are the concrete targets a human approved for this
	// run. Required by any per-target capability.
	AuthorizedTargets []string
	// RequestedBy identifies the principal asking for the run.
	RequestedBy string
	// SubjectPermissions are that principal's AO permissions on the project.
	SubjectPermissions []domain.Permission
}

// Plan is a validated, authorized description of a run that has NOT happened.
// It exists so the authorization decision can be built, tested and shown to a
// user long before anything can execute it.
type Plan struct {
	Resolved Resolved
	Mode     Mode
	Decision Decision
	Inputs   map[string]string
	// Runner is the attestation the plan was authorized against.
	Runner RunnerAttestation
	// PlannedAt is when the plan was built. A plan is not durable: the grant
	// or the package can change under it, so a runner must re-plan rather
	// than replay an old one.
	PlannedAt time.Time
}

// Runner is the interface an isolated skill executor will implement. It is
// declared now so the catalog's contract is fixed before anything can run, and
// so a caller cannot accidentally depend on an execution API that does not
// exist yet.
//
// Two rules are load-bearing for any implementation:
//
//  1. Attestation must describe what the runner ACTUALLY enforces. Returning
//     Isolated: true without a real boundary defeats every check in this
//     package, because Authorize trusts this one value.
//  2. Execute must re-run Authorize against its own attestation. A Plan built
//     elsewhere is an input to be re-checked, not a permission slip.
type Runner interface {
	// Attestation is what this runner genuinely provides.
	Attestation() RunnerAttestation
	// Execute runs the planned skill mode and returns its structured report.
	Execute(ctx context.Context, plan Plan) (Result, error)
}

// Result is a completed run's structured output. The report body is carried as
// raw bytes validated against the manifest's declared output schema, so the
// catalog does not need to know any particular skill's finding shape.
type Result struct {
	SkillID   string
	Version   string
	ModeID    string
	StartedAt time.Time
	EndedAt   time.Time
	// Report is the JSON document the run produced.
	Report []byte
	// ExitReason records how the run ended, for the audit trail.
	ExitReason string
}

// PlanRun resolves, validates and authorizes a run without executing it.
//
// This is the whole of what this phase implements. It is genuinely useful on
// its own: a UI can call it to show exactly which capabilities a run would
// need, which are missing, and why — before any runner exists.
func PlanRun(r *Registry, req RunRequest, runner RunnerAttestation) (Plan, error) {
	resolved, err := r.Resolve(req.ProjectID, req.SkillID)
	if err != nil {
		return Plan{}, err
	}
	modeID := req.ModeID
	if modeID == "" && len(resolved.Package.Manifest.Modes) == 1 {
		modeID = resolved.Package.Manifest.Modes[0].ID
	}
	mode, ok := resolved.Package.Manifest.Mode(modeID)
	if !ok {
		return Plan{}, invalidf("skill %q has no mode %q", req.SkillID, req.ModeID)
	}
	inputs, err := resolveInputs(resolved.Package.Manifest, req.Inputs)
	if err != nil {
		return Plan{}, err
	}
	decision, err := Authorize(AuthorizationRequest{
		Manifest:           resolved.Package.Manifest,
		ModeID:             mode.ID,
		Grant:              resolved.Activation.Grant,
		SubjectPermissions: req.SubjectPermissions,
		Runner:             runner,
		AuthorizedTargets:  req.AuthorizedTargets,
	})
	if err != nil {
		return Plan{Resolved: resolved, Mode: mode, Decision: decision}, err
	}
	return Plan{
		Resolved:  resolved,
		Mode:      mode,
		Decision:  decision,
		Inputs:    inputs,
		Runner:    runner,
		PlannedAt: time.Now().UTC(),
	}, nil
}

// resolveInputs validates the caller's values against the manifest's declared
// inputs: required values present, no unknown keys, enums in range.
func resolveInputs(m Manifest, got map[string]string) (map[string]string, error) {
	declared := map[string]InputParam{}
	for _, in := range m.Inputs {
		declared[in.Name] = in
	}
	for name := range got {
		if _, ok := declared[name]; !ok {
			return nil, invalidf("input %q is not declared by skill %s", name, m.ID)
		}
	}
	out := map[string]string{}
	for name, in := range declared {
		value, present := got[name]
		value = strings.TrimSpace(value)
		if !present || value == "" {
			if in.Required {
				return nil, invalidf("input %q is required by skill %s", name, m.ID)
			}
			continue
		}
		if in.Type == "enum" {
			allowed := false
			for _, e := range in.Enum {
				if e == value {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, invalidf("input %q must be one of %s", name, strings.Join(in.Enum, ", "))
			}
		}
		out[name] = value
	}
	return out, nil
}

// UnavailableRunner is the only Runner this phase ships. It attests nothing
// and refuses everything, which is exactly what AO can honestly offer today.
type UnavailableRunner struct{}

// Attestation reports no isolation and no egress control.
func (UnavailableRunner) Attestation() RunnerAttestation { return NoRunner() }

// Execute always fails.
func (UnavailableRunner) Execute(context.Context, Plan) (Result, error) {
	return Result{}, fmt.Errorf("%w: skill execution requires an isolated runner, which is not implemented", ErrNoRunner)
}

var _ Runner = UnavailableRunner{}
