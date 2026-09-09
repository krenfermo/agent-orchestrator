package skills

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// run.go — the one execution path, and everything a caller may NOT contribute
// to it.
//
// # What the caller supplies
//
// A project, a skill, a mode, and who they are. That is the whole list.
//
// # What the caller cannot supply, and why each one matters
//
//   - An ATTESTATION. It comes from AO's own runner. A self-declared
//     Isolated:true is the exact claim this design refuses as proof (ADR 0004),
//     and accepting one would defeat every capability check at once.
//   - A COMMAND. AO authors the argv, in Go, from a closed tool vocabulary. A
//     manifest contributes two validated integers and nothing else.
//   - An IMAGE. It comes from an administrator's approval for this exact scope.
//   - ISOLATION FLAGS. Every container setting is ADR 0004's fixed minimum;
//     there is no request field that can relax one.
//   - A SCOPE. It is derived from the resolved activation -- the project's
//     tenant, the version the project is pinned to, the mode being run -- so a
//     caller cannot name a scope with a wider approval attached to it.
//   - The FILES to read. The scan reads what the manifest declared, not what
//     the request asks for.
//
// # What is deliberately NOT enabled here
//
// One mode: static-code, the one with a live boundary test. process.exec,
// repo.write, net.egress, net.active_scan and secrets.read are refused by the
// capability table exactly as before -- this file adds no surface that could
// reach them, and a mode requiring any of them fails authorization before a
// container is considered.

// ErrNotExecutable is returned when a resolved mode is not one AO can run. It
// is distinct from an authorization failure: the mode may be perfectly
// authorized and still have no implementation behind it.
var ErrNotExecutable = errors.New("skills: no execution path exists for this mode")

// executableModes maps a manifest mode onto the AO tool that implements it.
//
// It is a closed map, and it holds exactly one entry. Adding a second is a code
// change and a release -- which is the point, because it is the moment somebody
// decides a new kind of thing may run as AO.
var executableModes = map[string]skillrunner.Tool{
	"static-code": skillrunner.ToolStaticScan,
}

// SkillExecutor is the execution environment this service drives. It is an
// interface so the service can be tested without a container runtime, and so
// the surface the service depends on stays small and visible.
//
// It embeds skillcatalog.Runner because the attestation a dry run reports and
// the one a real run is authorized against have to be the same value. Two
// sources would let a dry run describe an environment that is not the one that
// executes, which is the most misleading thing this surface could do.
type SkillExecutor interface {
	skillcatalog.Runner
	RunStaticScan(ctx context.Context, authority skillrunner.ImageAuthority,
		req skillrunner.StaticScanRequest) (skillrunner.StaticScanReport, error)
}

// ProjectReader resolves the project a run belongs to. The tenant comes from
// here rather than from the request: a caller who could name their own tenant
// could name one with a wider approval attached to it.
type ProjectReader interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
}

// RunRequest is everything a caller may say about a run.
type RunRequest struct {
	ProjectID domain.ProjectID
	SkillID   string
	// ModeID may be empty only when the skill declares exactly one mode.
	ModeID string
	// Inputs are the manifest's DECLARED inputs. They are validated against
	// the manifest -- required present, no unknown keys, enums in range -- and
	// for the static scan they never reach a command line: AO authors the argv
	// in Go from two validated integers, and takes nothing from here. They are
	// recorded in the plan, which is where a reader looks to see what was asked
	// for.
	//
	// This is the "typed, validated parameters" ADR 0005 described, and it is
	// deliberately not a way to contribute a command. A manifest cannot build
	// an argv and neither can a caller.
	Inputs map[string]string
	// Actor and ActorPermissions are the caller and what they hold on this
	// project. The permissions are checked by skillcatalog.Authorize against
	// what the mode's capabilities require; nothing here re-implements that.
	Actor            string
	ActorPermissions []domain.Permission
}

// RunResult is one completed execution.
type RunResult struct {
	SkillID string
	Version string
	ModeID  string
	Tool    string
	// Report is the tool's structured output, including the coverage that says
	// what was and was not read.
	Report skillrunner.StaticScanReport
	// Plan is the authorization this run happened under.
	Plan skillcatalog.Plan
}

// RunSkill executes one authorized mode of one activated skill.
//
// The order is load-bearing and every step is a refusal:
//
//  1. Resolve the activation. Not installed, not enabled, pinned to a missing
//     version, or a package whose bytes no longer match its digest all stop here.
//  2. Resolve the project, for its tenant. A project AO cannot read is not one
//     it can build a scope for.
//  3. Authorize, against AO'S OWN attestation. A mode needing a control the
//     runtime does not attest is refused with the control named.
//  4. Check the mode is one AO implements at all.
//  5. Execute, which resolves the approved image, re-checks the approval
//     immediately before launch, and refuses on anything unexpected.
//
// Steps 1-4 happen before anything is staged, so a refused run leaves no copy
// of somebody's source on disk.
func (s *Service) RunSkill(ctx context.Context, req RunRequest) (RunResult, error) {
	if s.executor == nil {
		return RunResult{}, apierr.Conflict("SKILL_RUNNER_UNAVAILABLE",
			s.runnerRefusal(), nil)
	}

	resolved, err := s.Resolve(ctx, req.ProjectID, req.SkillID)
	if err != nil {
		return RunResult{}, err
	}
	manifest := resolved.Package.Manifest

	modeID := req.ModeID
	if modeID == "" && len(manifest.Modes) == 1 {
		modeID = manifest.Modes[0].ID
	}
	mode, ok := manifest.Mode(modeID)
	if !ok {
		return RunResult{}, apierr.Invalid("SKILL_MODE_UNKNOWN",
			fmt.Sprintf("%s has no mode %q", req.SkillID, modeID), nil)
	}

	project, ok, err := s.project(ctx, req.ProjectID)
	if err != nil {
		return RunResult{}, err
	}
	if !ok {
		return RunResult{}, apierr.NotFound("PROJECT_NOT_FOUND",
			fmt.Sprintf("no project %q", req.ProjectID))
	}
	if strings.TrimSpace(project.Path) == "" {
		return RunResult{}, apierr.Invalid("PROJECT_PATH_MISSING",
			fmt.Sprintf("project %q has no checkout on this host to scan", req.ProjectID), nil)
	}

	// A manifest that declares a mode-selecting input gets the mode AO
	// authorized, not one the caller names separately. Two answers to "which
	// mode is this" is how a run gets authorized as one thing and instructed as
	// another.
	supplied := make(map[string]string, len(req.Inputs)+1)
	for k, v := range req.Inputs {
		supplied[k] = v
	}
	if declared, ok := declaredInput(manifest, modeInputName); ok && declared.Required {
		if named, given := supplied[modeInputName]; given && named != mode.ID {
			return RunResult{}, apierr.Invalid("SKILL_MODE_INPUT_MISMATCH",
				fmt.Sprintf("the run is authorized for mode %q and the %q input says %q",
					mode.ID, modeInputName, named), nil)
		}
		supplied[modeInputName] = mode.ID
	}
	inputs, err := skillcatalog.ResolveInputs(manifest, supplied)
	if err != nil {
		return RunResult{}, apierr.Invalid("SKILL_INPUTS_INVALID", err.Error(), nil)
	}

	// The attestation is AO's own. RunRequest has no field for one, and this is
	// the only place it is read.
	attestation := s.executor.Attestation()
	decision, err := skillcatalog.Authorize(skillcatalog.AuthorizationRequest{
		Manifest:           manifest,
		ModeID:             mode.ID,
		Grant:              resolved.Activation.Grant,
		SubjectPermissions: req.ActorPermissions,
		Runner:             attestation,
	})
	if err != nil {
		s.recordRun(ctx, store.SkillAuditRunRefused, req, resolved.Activation.Version, mode.ID, "", err.Error())
		return RunResult{}, apierr.Forbidden("SKILL_RUN_REFUSED", err.Error())
	}
	plan := skillcatalog.Plan{
		Resolved: resolved, Mode: mode, Decision: decision, Inputs: inputs,
		Runner: attestation, PlannedAt: s.now(),
	}

	tool, ok := executableModes[mode.ID]
	if !ok {
		// Authorized and unimplemented are different answers, and collapsing
		// them would tell an operator their grant was wrong when it was fine.
		detail := fmt.Sprintf("mode %q is authorized but AO ships no execution path for it "+
			"(implemented: %s)", mode.ID, implementedModes())
		s.recordRun(ctx, store.SkillAuditRunRefused, req, resolved.Activation.Version, mode.ID, "", detail)
		return RunResult{}, apierr.Conflict("SKILL_MODE_NOT_EXECUTABLE", detail, nil)
	}

	// The scope is DERIVED, never supplied. Every field comes from something
	// AO resolved: the project's tenant, the version the project is pinned to,
	// the mode that was authorized.
	scope := skillimage.Scope{
		TenantID:  project.TenantID,
		ProjectID: req.ProjectID,
		SkillID:   manifest.ID,
		Version:   resolved.Activation.Version,
		ModeID:    mode.ID,
	}

	stagingPaths, err := stagingPathsFor(manifest)
	if err != nil {
		s.recordRun(ctx, store.SkillAuditRunRefused, req, scope.Version, mode.ID, "", err.Error())
		return RunResult{}, apierr.Conflict("SKILL_SCOPE_NOT_STAGEABLE", err.Error(), nil)
	}

	report, err := s.executor.RunStaticScan(ctx, s.images, skillrunner.StaticScanRequest{
		Scope:       scope,
		ProjectID:   string(req.ProjectID),
		ProjectPath: project.Path,
		// The files come from the MANIFEST's declared read scope, not from the
		// request. A caller who could name paths could name the ones the
		// manifest was reviewed for not naming.
		ScopePaths:          stagingPaths,
		StagingRootOverride: s.stagingRoot,
		Params:              skillrunner.DefaultToolParams(),
		Limits:              skillrunner.DefaultLimits(),
	})
	if err != nil {
		s.recordRun(ctx, store.SkillAuditRunRefused, req, scope.Version, mode.ID, "", err.Error())
		return RunResult{}, executionError(err)
	}

	s.recordRun(ctx, store.SkillAuditRunExecuted, req, scope.Version, mode.ID, report.ImageDigest,
		fmt.Sprintf("%s scanned %d of %d staged files, %d findings, approval %s by %s",
			tool, report.Coverage.FilesScanned, report.Coverage.FilesStaged,
			len(report.Findings), report.ApprovalID, report.ApprovedBy))

	return RunResult{
		SkillID: manifest.ID, Version: scope.Version, ModeID: mode.ID,
		Tool: string(tool), Report: report, Plan: plan,
	}, nil
}

// executionError maps a runner refusal onto the API envelope without losing
// which kind of refusal it was. A caller that cannot tell "no image is
// approved" from "the container runtime is down" cannot act on either.
func executionError(err error) error {
	switch {
	case errors.Is(err, skillrunner.ErrImageNotApproved), errors.Is(err, skillimage.ErrNotApproved):
		return apierr.Forbidden("SKILL_IMAGE_NOT_APPROVED", err.Error())
	case errors.Is(err, skillrunner.ErrRuntimeUnavailable):
		return apierr.Conflict("SKILL_RUNTIME_UNAVAILABLE", err.Error(), nil)
	case errors.Is(err, skillrunner.ErrStagingUnusable):
		return apierr.Conflict("SKILL_STAGING_UNUSABLE", err.Error(), nil)
	case errors.Is(err, skillrunner.ErrToolNotApproved):
		return apierr.Conflict("SKILL_TOOL_NOT_APPROVED", err.Error(), nil)
	default:
		// An apierr from a lower layer keeps its own code and status.
		var coded *apierr.Error
		if errors.As(err, &coded) {
			return err
		}
		return apierr.Conflict("SKILL_RUN_FAILED", err.Error(), nil)
	}
}

func implementedModes() string {
	out := make([]string, 0, len(executableModes))
	for mode := range executableModes {
		out = append(out, mode)
	}
	return strings.Join(out, ", ")
}

func (s *Service) project(ctx context.Context, id domain.ProjectID) (domain.ProjectRecord, bool, error) {
	if s.projects == nil {
		return domain.ProjectRecord{}, false, apierr.Conflict("PROJECT_READER_UNAVAILABLE",
			"this service cannot resolve projects, so it cannot build the scope a run needs", nil)
	}
	return s.projects.GetProject(ctx, string(id))
}

func (s *Service) runnerRefusal() string {
	if s.runnerUnavailable != "" {
		return "no skill runner: " + s.runnerUnavailable
	}
	return "this installation has no skill runner configured, so nothing can execute"
}

func (s *Service) recordRun(
	ctx context.Context, action store.SkillAuditAction, req RunRequest,
	version, modeID, digest, detail string,
) {
	projectID := req.ProjectID
	_ = s.store.AppendSkillAudit(ctx, store.SkillAuditEntry{
		ID:         s.newID(),
		OccurredAt: s.now(),
		Actor:      req.Actor,
		Action:     action,
		SkillID:    req.SkillID,
		Version:    version,
		ProjectID:  &projectID,
		Digest:     digest,
		Detail:     modeID + ": " + detail,
	})
}

// stagingPathsFor translates a manifest's declared read scope into the literal
// paths staging can copy.
//
// The manifest speaks in globs; staging copies directories. The two are not the
// same language, and the gap has exactly one safe direction:
//
//   - "**" (and "." and "./") mean the whole checkout, which staging expresses
//     as no scope paths at all. Widening nothing.
//   - A literal path is passed through unchanged.
//   - Any OTHER glob is REFUSED. Falling back to "the whole checkout" for a
//     pattern AO cannot express would widen the scope silently, which is the
//     one outcome worse than refusing: the manifest would have been reviewed
//     for "src/**" and the run would read everything.
//
// A refusal here is a package AO cannot run yet, not a package that is wrong.
func stagingPathsFor(m skillcatalog.Manifest) ([]string, error) {
	out := make([]string, 0, len(m.Scope.Files.Read))
	for _, raw := range m.Scope.Files.Read {
		path := strings.TrimSpace(raw)
		switch path {
		case "", "**", ".", "./", "**/*":
			// The whole checkout. Returning nil rather than accumulating means
			// one "**" beside a literal path still means everything, which is
			// what the manifest said.
			return nil, nil
		}
		if strings.ContainsAny(path, "*?[") {
			return nil, fmt.Errorf("the manifest limits reading to %q, which AO's staging cannot "+
				"express as a set of files; it refuses rather than widen the scope to the whole checkout", raw)
		}
		out = append(out, path)
	}
	return out, nil
}

// declaredInput finds one of a manifest's declared inputs by name.
func declaredInput(m skillcatalog.Manifest, name string) (skillcatalog.InputParam, bool) {
	for _, in := range m.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return skillcatalog.InputParam{}, false
}

// modeInputName is the conventional name for a manifest input that selects the
// mode. When a package declares it, AO fills it in from the mode it authorized
// rather than trusting the caller to agree with itself.
const modeInputName = "mode"

// runDeadline is not enforced here: the runner owns the wall clock, because a
// timeout the caller could raise is not a limit. It is named so the value is
// greppable next to the code that would be tempted to add one.
const runDeadline = time.Duration(0)

// Compile-time proof that AO's real runner satisfies the execution port. It is
// here rather than in skillrunner so the dependency points one way: the service
// knows about the runner, and the runner knows nothing about the service.
var _ SkillExecutor = (*skillrunner.Runner)(nil)

// And that the trust root satisfies what the runner asks of an authority.
var _ skillrunner.ImageAuthority = (*ImageAuthority)(nil)
