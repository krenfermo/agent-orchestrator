package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillagent"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// agentrun.go -- a skill mode whose executor is an AGENT (Frente 2 / 2C,
// ADR 0010).
//
// It rides the same durable run as the container path: the same prepare-then-
// accept contract, the same states, the same CAS transitions, ownership,
// idempotency, restart reconciliation and report digest. What differs is who
// executes and what AO must do before it may store what came back.
//
// # Before acceptance (no run exists if any of these refuse)
//
//  1. The package is TRUSTED: byte for byte the builtin this binary embeds, or
//     an install AO verified a signature for, not revoked, whose manifest still
//     hashes to what was verified. A manifest saying origin: builtin is a claim,
//     not evidence.
//  2. Its output schema IS the canonical findings contract, and parses inside
//     the validator's strict subset.
//  3. AO's host agent executor attests the host-agent controls, and
//     skillcatalog.Authorize grants every capability of the mode against that
//     attestation -- which it does only for repo.read and report.write.
//
// # After the agent exits (the run ends refused or failed if any of these do)
//
//  1. The staged copy and the source are unchanged (the executor checks).
//  2. The output validates, strictly, against the canonical schema.
//  3. AO overwrites what only AO knows (project, mode, version, times), checks
//     every path the report cites is one AO staged, and adds its own coverage
//     (what it excluded before staging) and notes (how the run executed).
//  4. Every string is redacted, including every literal credential AO found in
//     the staged files itself.
//  5. The result validates again, and only then is stored, with its SHA-256.

// AgentExecutor is the host agent execution environment. *skillagent.Executor
// implements it.
type AgentExecutor interface {
	Attestation() skillcatalog.RunnerAttestation
	Unavailable() string
	Model() string
	Run(ctx context.Context, req skillagent.Request) (skillagent.Result, error)
	Reap(runID string) (killed bool, removed string, err error)
}

// WithAgentExecutor wires AO's host agent executor and the project reader a
// run's scope is derived from. Without it every agent mode is refused before
// acceptance. projects may be nil when WithSkillExecutor already supplied one.
func WithAgentExecutor(e AgentExecutor, projects ProjectReader) Option {
	return func(s *Service) {
		s.agent = e
		if projects != nil {
			s.projects = projects
		}
	}
}

var _ AgentExecutor = (*skillagent.Executor)(nil)

// agentPlan is what an accepted agent run carries: the verified instruction
// bytes and the parsed schema.
type agentPlan struct {
	trust        string
	instructions string
	guide        string
	schemaRaw    []byte
	schema       *skillreport.Schema
	deny         []string
}

// Trust labels recorded on the run.
const (
	agentTrustBuiltin = "builtin"
	agentTrustTrusted = "trusted"
)

// prepareAgentRun is prepareRun's branch for a mode whose executor is agent.
func (s *Service) prepareAgentRun(ctx context.Context, req RunRequest, resolved skillcatalog.Resolved,
	mode skillcatalog.Mode, project domain.ProjectRecord, inputs map[string]string,
) (preparedRun, error) {
	manifest := resolved.Package.Manifest
	refuse := func(err error) (preparedRun, error) {
		var coded *apierr.Error
		detail := err.Error()
		if errors.As(err, &coded) {
			detail = coded.Message
		}
		s.recordRun(ctx, store.SkillAuditRunRefused, req, resolved.Activation.Version, mode.ID, "", detail)
		return preparedRun{}, err
	}
	if s.agent == nil {
		return refuse(apierr.Conflict("SKILL_AGENT_UNAVAILABLE",
			"this installation has no host agent executor, so no agent mode can run", nil))
	}

	plan, err := s.loadAgentPlan(ctx, resolved, mode)
	if err != nil {
		return refuse(err)
	}

	attestation := s.agent.Attestation()
	decision, err := skillcatalog.Authorize(skillcatalog.AuthorizationRequest{
		Manifest:           manifest,
		ModeID:             mode.ID,
		Grant:              resolved.Activation.Grant,
		SubjectPermissions: req.ActorPermissions,
		Runner:             attestation,
		PackageTrusted:     true,
	})
	if err != nil {
		if u := s.agent.Unavailable(); u != "" {
			return refuse(apierr.Conflict("SKILL_AGENT_UNAVAILABLE", "no host agent: "+u, nil))
		}
		return refuse(apierr.Forbidden("SKILL_RUN_REFUSED", err.Error()))
	}
	stagingPaths, err := stagingPathsFor(manifest)
	if err != nil {
		return refuse(apierr.Conflict("SKILL_SCOPE_NOT_STAGEABLE", err.Error(), nil))
	}
	return preparedRun{
		req: req, resolved: resolved, mode: mode, project: project, inputs: inputs,
		plan: skillcatalog.Plan{
			Resolved: resolved, Mode: mode, Decision: decision, Inputs: inputs,
			Runner: attestation, PlannedAt: s.now(),
		},
		tool: skillrunner.Tool(skillagent.Tool),
		scope: skillimage.Scope{
			TenantID: project.TenantID, ProjectID: req.ProjectID, SkillID: manifest.ID,
			Version: resolved.Activation.Version, ModeID: mode.ID,
		},
		stagingPaths: stagingPaths,
		agent:        plan,
	}, nil
}

// loadAgentPlan verifies the package may be followed by a host agent and reads
// the exact bytes the agent will be given.
func (s *Service) loadAgentPlan(ctx context.Context, resolved skillcatalog.Resolved, mode skillcatalog.Mode) (*agentPlan, error) {
	pkg := resolved.Package
	manifest := pkg.Manifest
	read := func(rel string) ([]byte, error) {
		if err := validRel(rel); err != nil {
			return nil, err
		}
		return os.ReadFile(filepath.Join(pkg.Dir, filepath.FromSlash(rel))) //nolint:gosec // package-relative path validated above.
	}
	// The bytes are read FIRST and the trust check runs after, so the bytes
	// the agent receives are bytes the check has just vouched for.
	instructions, err := read("SKILL.md")
	if err != nil {
		return nil, apierr.Conflict("SKILL_AGENT_PACKAGE_INCOMPLETE",
			fmt.Sprintf("%s has no SKILL.md for the agent to follow", manifest.ID), nil)
	}
	guide, err := read(mode.Guide)
	if err != nil {
		return nil, apierr.Conflict("SKILL_AGENT_PACKAGE_INCOMPLETE",
			fmt.Sprintf("%s has no guide %s for mode %s", manifest.ID, mode.Guide, mode.ID), nil)
	}
	schemaRaw, err := read(manifest.Outputs.SchemaRef)
	if err != nil {
		return nil, apierr.Conflict("SKILL_AGENT_PACKAGE_INCOMPLETE",
			fmt.Sprintf("%s has no output schema %s", manifest.ID, manifest.Outputs.SchemaRef), nil)
	}

	trust, err := s.agentTrust(ctx, resolved)
	if err != nil {
		return nil, err
	}
	if trust == "" {
		return nil, apierr.Forbidden("SKILL_AGENT_UNTRUSTED", fmt.Sprintf(
			"%s@%s is neither the builtin package this AO build embeds nor a signature-trusted install; "+
				"an agent mode runs on the host and follows the package's own instructions, so only "+
				"builtin or trusted packages may run one (ADR 0010)", manifest.ID, resolved.Activation.Version))
	}
	if !bytes.Equal(schemaRaw, skillcatalog.CanonicalFindingsSchema()) {
		return nil, apierr.Conflict("SKILL_AGENT_SCHEMA_NOT_CANONICAL", fmt.Sprintf(
			"%s declares output schema %s, which is not AO's findings.v1 contract; an agent's report is "+
				"validated and stored against that contract only", manifest.ID, manifest.Outputs.SchemaRef), nil)
	}
	schema, err := skillreport.ParseSchema(schemaRaw)
	if err != nil {
		return nil, apierr.Conflict("SKILL_AGENT_SCHEMA_NOT_CANONICAL", err.Error(), nil)
	}
	return &agentPlan{
		trust: trust, instructions: string(instructions), guide: string(guide),
		schemaRaw: schemaRaw, schema: schema, deny: append([]string(nil), manifest.Scope.Files.Deny...),
	}, nil
}

// agentTrust answers "may a host agent follow this package's instructions".
// It returns "builtin", "trusted" or "" (no).
func (s *Service) agentTrust(ctx context.Context, resolved skillcatalog.Resolved) (string, error) {
	pkg := resolved.Package
	builtin, err := skillcatalog.MatchesBuiltin(pkg.Manifest.ID, pkg.Dir)
	if err != nil {
		return "", apierr.Conflict("SKILL_AGENT_TRUST_UNVERIFIABLE", err.Error(), nil)
	}
	if builtin {
		return agentTrustBuiltin, nil
	}
	if s.origins == nil {
		return "", nil
	}
	origin, ok, err := s.origins.GetSkillInstallOrigin(ctx, pkg.Manifest.ID, resolved.Activation.Version)
	if err != nil {
		return "", err
	}
	if !ok || origin.TrustState != skillregistry.TrustTrusted || origin.RevokedAt != nil {
		return "", nil
	}
	// The signature covered the manifest AO installed. The package digest
	// cannot cover skill.yaml, so it is re-hashed here: a trusted install whose
	// manifest was edited afterwards is no longer the thing that was signed.
	got, err := skillregistry.FileDigest(filepath.Join(pkg.Dir, skillcatalog.ManifestFileName))
	if err != nil || got != origin.ManifestDigest {
		return "", nil //nolint:nilerr // an unreadable or altered manifest is simply not trusted.
	}
	return agentTrustTrusted, nil
}

func validRel(rel string) error {
	clean := path.Clean(rel)
	if rel == "" || path.IsAbs(rel) || clean != rel || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("package path %q is not a clean relative path", rel)
	}
	return nil
}

// executeAgentRun is execute's branch for an accepted agent run. ctx is the
// executor context (cancelled by a cancel or a shutdown); wctx survives both.
func (s *Service) executeAgentRun(ctx, wctx context.Context, runID string, prep preparedRun) {
	e := s.runs
	res, err := s.agent.Run(ctx, skillagent.Request{
		RunID:        runID,
		ProjectID:    string(prep.req.ProjectID),
		ProjectPath:  prep.project.Path,
		SkillID:      prep.resolved.Package.Manifest.ID,
		SkillVersion: prep.scope.Version,
		ModeID:       prep.mode.ID,
		ScopePaths:   prep.stagingPaths,
		DenyGlobs:    prep.agent.deny,
		Instructions: prep.agent.instructions,
		ModeGuide:    prep.agent.guide,
		Schema:       prep.agent.schemaRaw,
	})
	if err != nil {
		switch {
		case s.cancelWasRequested(runID):
			s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
				"cancelled while running; the agent was stopped and its staged copy removed", store.SkillRunImage{})
		case e.base.Err() != nil:
			s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrShutdown,
				"the daemon shut down while the agent was running; it was stopped and its staged copy removed",
				store.SkillRunImage{})
		default:
			state, code := classifyAgentError(err)
			msg, _ := skillreport.NewRedactor(res.Literals).String(err.Error())
			msg = truncate(msg, 2000)
			if state == store.SkillRunRefused {
				s.recordRun(wctx, store.SkillAuditRunRefused, prep.req, prep.scope.Version, prep.mode.ID, "", msg)
			}
			s.finishUnsuccessful(wctx, runID, state, code, msg, store.SkillRunImage{})
		}
		return
	}
	if s.cancelWasRequested(runID) {
		s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
			"cancelled while running; the agent finished but its report was discarded", store.SkillRunImage{})
		return
	}
	accepted, err := acceptAgentReport(prep, res)
	if err != nil {
		// The message names what was wrong. Where it quotes the output (a
		// path, a property name) that text is untrusted and unredacted, so it
		// goes through the same redactor before it is recorded.
		msg, _ := skillreport.NewRedactor(res.Literals).String(err.Error())
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInvalid, truncate(msg, 2000), store.SkillRunImage{})
		return
	}
	sum := sha256.Sum256(accepted.reportJSON)
	ok, err := e.store.FinishSkillRunSucceeded(wctx, runID, store.SkillRunSuccess{
		Summary:      accepted.summary,
		ReportJSON:   accepted.reportJSON,
		ReportSHA256: hex.EncodeToString(sum[:]),
		Findings:     accepted.findingRows(runID),
		FinishedAt:   s.now(),
	})
	if err != nil {
		e.log.Error("skills: could not record agent run result", "run", runID, "err", err)
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInterrupted,
			"the agent completed but AO could not record its result", store.SkillRunImage{})
		return
	}
	if ok {
		s.recordRun(wctx, store.SkillAuditRunExecuted, prep.req, prep.scope.Version, prep.mode.ID, "",
			fmt.Sprintf("%s (%s package): %s", skillagent.RunnerID, prep.agent.trust, accepted.summary))
	}
}

// Agent run error codes.
const (
	RunErrAgentUnavailable    = "SKILL_AGENT_UNAVAILABLE"
	RunErrAgentTampered       = "SKILL_AGENT_STAGING_TAMPERED"
	RunErrAgentSourceChanged  = "SKILL_AGENT_SOURCE_CHANGED"
	RunErrAgentProviderFailed = "SKILL_AGENT_PROVIDER_FAILED"
)

// classifyAgentError maps an executor error onto refused or failed. The
// boundary declining to trust the run (a tampered copy, a changed source, no
// usable executor or staging) is a refusal; a provider that produced nothing
// usable is a failure.
func classifyAgentError(err error) (store.SkillRunState, string) {
	switch {
	case errors.Is(err, skillagent.ErrUnavailable):
		return store.SkillRunRefused, RunErrAgentUnavailable
	case errors.Is(err, skillagent.ErrStagingTampered):
		return store.SkillRunRefused, RunErrAgentTampered
	case errors.Is(err, skillagent.ErrSourceChanged):
		return store.SkillRunRefused, RunErrAgentSourceChanged
	case errors.Is(err, skillrunner.ErrStagingUnusable):
		return store.SkillRunRefused, "SKILL_STAGING_UNUSABLE"
	case errors.Is(err, skillagent.ErrNoStructuredOutput):
		return store.SkillRunFailed, RunErrInvalid
	case errors.Is(err, skillagent.ErrProvider):
		return store.SkillRunFailed, RunErrAgentProviderFailed
	}
	return store.SkillRunFailed, "SKILL_RUN_FAILED"
}

// acceptedReport is an agent report AO has validated, completed and redacted.
type acceptedReport struct {
	reportJSON []byte
	summary    string
	findings   []agentFinding
}

type agentFinding struct {
	id, severity, category, title, path, recommendation, confidence string
	line                                                            int
}

func (a acceptedReport) findingRows(runID string) []store.SkillRunFindingRecord {
	rows := make([]store.SkillRunFindingRecord, 0, len(a.findings))
	for i, f := range a.findings {
		rows = append(rows, store.SkillRunFindingRecord{
			RunID: runID, Ordinal: i, RuleID: f.id, Severity: f.severity, Category: f.category,
			Title: f.title, Path: f.path, Line: f.line, Recommendation: f.recommendation, Confidence: f.confidence,
		})
	}
	return rows
}

// acceptAgentReport is the whole gate between an agent's output and storage.
// The order is the contract: validate, complete, redact, validate again.
func acceptAgentReport(prep preparedRun, res skillagent.Result) (acceptedReport, error) {
	schema := prep.agent.schema
	if err := schema.Validate(res.Output); err != nil {
		return acceptedReport{}, fmt.Errorf("the agent's report was rejected: %w", err)
	}
	decoded, err := skillreport.Decode(res.Output)
	if err != nil {
		return acceptedReport{}, fmt.Errorf("the agent's report was rejected: %w", err)
	}
	doc := obj(decoded)

	staged := map[string]bool{}
	for _, in := range res.Staging.Inputs {
		staged[in.RelPath] = true
	}

	// What only AO knows is AO's to write. The agent was told these values;
	// it is not trusted to have kept them.
	run := obj(doc["run"])
	run["projectId"] = string(prep.req.ProjectID)
	run["mode"] = prep.mode.ID
	run["skillVersion"] = prep.scope.Version
	run["startedAt"] = res.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	run["endedAt"] = res.EndedAt.Format("2006-01-02T15:04:05Z07:00")
	// A target and an authorization are active-pentest facts a person
	// supplies; an agent over a staged copy has neither, and a commit it did
	// not read from a repository it was not given is invented.
	delete(run, "target")
	delete(run, "authorizationRef")
	delete(run, "commit")

	coverage := obj(doc["coverage"])
	examined := arr(coverage["examined"])
	normalizedExamined := make([]any, 0, len(examined))
	examinedFiles := map[string]bool{}
	for _, raw := range examined {
		p := normalizeCited(str(raw))
		if p == "." {
			for f := range staged {
				examinedFiles[f] = true
			}
			normalizedExamined = append(normalizedExamined, p)
			continue
		}
		if !staged[p] && !isStagedDir(p, staged) {
			return acceptedReport{}, fmt.Errorf("the agent's report was rejected: coverage.examined names %q, "+
				"which AO did not stage", truncate(p, 120))
		}
		for f := range staged {
			if f == p || strings.HasPrefix(f, p+"/") {
				examinedFiles[f] = true
			}
		}
		normalizedExamined = append(normalizedExamined, p)
	}
	coverage["examined"] = normalizedExamined
	skipped := arr(coverage["skipped"])
	for _, sk := range res.Staging.Skipped {
		skipped = append(skipped, map[string]any{"path": sk.Path, "reason": "excluded by AO before staging: " + sk.Reason})
	}
	coverage["skipped"] = skipped

	for i, fr := range arr(doc["findings"]) {
		for _, lr := range arr(obj(obj(fr)["evidence"])["locations"]) {
			loc := obj(lr)
			p := normalizeCited(str(loc["path"]))
			if !staged[p] {
				return acceptedReport{}, fmt.Errorf("the agent's report was rejected: finding %d cites %q, "+
					"which AO did not stage", i, truncate(p, 120))
			}
			loc["path"] = p
		}
	}

	// Redaction, over every string -- including the ones AO just wrote,
	// which are harmless, and the ones the agent wrote, which are the point.
	redactor := skillreport.NewRedactor(res.Literals)
	redactedDoc, redactions := redactor.Redact(doc)
	doc = obj(redactedDoc)

	notes := arr(doc["notes"])
	ev := res.Evidence
	notes = append(notes, fmt.Sprintf("AO: executed by %s (model %s) over a read-only staged copy of %d file(s); "+
		"%d excluded before staging; %d tool call(s) refused by the CLI's confinement%s.",
		skillagent.RunnerID, ev.Model, len(res.Staging.Inputs), res.Staging.SkippedCount,
		ev.PermissionDenials, deniedTools(ev.DeniedTools)))
	if ev.InputTokens > 0 || ev.OutputTokens > 0 {
		// The spend, as the CLI reported it. It is recorded, not governed: 2C
		// sets no budget (AO_SKILL_AGENT_MAX_BUDGET_USD is the hook).
		notes = append(notes, fmt.Sprintf("AO: the provider reported %d input and %d output token(s), $%.4f, %d turn(s).",
			ev.InputTokens, ev.OutputTokens, ev.CostUSD, ev.NumTurns))
	}
	if redactions > 0 {
		notes = append(notes, fmt.Sprintf("AO: redacted %d secret-shaped value(s) before storing this report.", redactions))
	}
	doc["notes"] = notes

	if err := schema.ValidateValue(doc); err != nil {
		return acceptedReport{}, fmt.Errorf("the report no longer validates after AO completed and redacted it: %w", err)
	}
	reportJSON, err := json.Marshal(doc)
	if err != nil {
		return acceptedReport{}, fmt.Errorf("encode report: %w", err)
	}
	// Findings rows are decoded from the exact bytes being stored, so the
	// table can hold nothing the report does not -- redaction included.
	var stored struct {
		Findings []struct {
			ID             string `json:"id"`
			Title          string `json:"title"`
			Severity       string `json:"severity"`
			Confidence     string `json:"confidence"`
			Category       string `json:"category"`
			Recommendation string `json:"recommendation"`
			Evidence       struct {
				Locations []struct {
					Path string `json:"path"`
					Line int    `json:"line"`
				} `json:"locations"`
			} `json:"evidence"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(reportJSON, &stored); err != nil {
		return acceptedReport{}, fmt.Errorf("decode report: %w", err)
	}
	findings := make([]agentFinding, 0, len(stored.Findings))
	for _, f := range stored.Findings {
		row := agentFinding{
			id: f.ID, severity: f.Severity, category: f.Category, title: f.Title,
			recommendation: f.Recommendation, confidence: f.Confidence,
		}
		if len(f.Evidence.Locations) > 0 {
			row.path, row.line = f.Evidence.Locations[0].Path, f.Evidence.Locations[0].Line
		}
		findings = append(findings, row)
	}
	summary := fmt.Sprintf("%s (%s) examined %d of %d staged files, %d findings",
		skillagent.RunnerID, ev.Model, len(examinedFiles), len(res.Staging.Inputs), len(findings))
	if redactions > 0 {
		summary += fmt.Sprintf(", %d value(s) redacted", redactions)
	}
	if ev.PermissionDenials > 0 {
		summary += fmt.Sprintf(", %d tool call(s) refused", ev.PermissionDenials)
	}
	return acceptedReport{reportJSON: reportJSON, summary: summary, findings: findings}, nil
}

// obj, arr and str read a decoded document. After validation the shapes are
// known; the checked assertions keep a shape the schema did not promise from
// becoming a panic.
func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func arr(v any) []any {
	a, _ := v.([]any)
	return a
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func normalizeCited(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

func isStagedDir(p string, staged map[string]bool) bool {
	for f := range staged {
		if strings.HasPrefix(f, p+"/") {
			return true
		}
	}
	return false
}

func deniedTools(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return " (" + strings.Join(tools, ", ") + ")"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
