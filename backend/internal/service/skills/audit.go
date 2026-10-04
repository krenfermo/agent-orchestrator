package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// audit.go -- the full security audit (Frente 2 / 2E, ADR 0011).
//
// An audit is a PARENT run of a composite mode and one CHILD run per composed
// mode. The parent executes nothing: every child is prepared, authorized and
// executed exactly as a standalone run of its mode would be -- its own tool or
// agent, its own attestation, its own image approval, its own report and
// digest -- and the parent consolidates the children's VERIFIED reports.
//
// # What flows between children
//
// Nothing. Each child's request is built from the parent's request alone (the
// project, the skill, the caller, the caller's inputs); no child's output, and
// nothing in the repository, can change which mode runs next, with what
// inputs, or with what permissions. The order is the manifest's.
//
// # Outcomes
//
//	succeeded  every composed mode produced a verified report
//	partial    at least one did and at least one did not (0173): the report
//	           says which did not, and why; it is never 'succeeded'
//	failed     none did (SKILL_AUDIT_NO_VERIFIED_RESULT), or the daemon stopped
//	cancelled  a person cancelled the audit; the running child is cancelled too
//
// A child that was refused before it could start, or whose mode already had a
// run in flight, has no child run; the report records it.

// AuditTool is recorded as an audit parent's tool.
const AuditTool = "ao.security-audit/v1"

// auditRunnerID names what "executed" an audit parent: nothing but the
// children, whose own records name their runners.
const auditRunnerID = "composite"

// Audit error codes.
const (
	RunErrAuditPartial         = "SKILL_AUDIT_PARTIAL"
	RunErrAuditNoVerified      = "SKILL_AUDIT_NO_VERIFIED_RESULT"
	auditErrNothingRunnable    = "SKILL_AUDIT_NOTHING_RUNNABLE"
	auditErrModeBusy           = "SKILL_AUDIT_MODE_BUSY"
	auditErrChildNotCreated    = "SKILL_AUDIT_CHILD_NOT_CREATED"
	auditErrRequiresDurableRun = "SKILL_AUDIT_REQUIRES_DURABLE_RUN"
)

// auditPlan is what an accepted audit carries.
type auditPlan struct {
	composes []string
	// executors maps each composed mode to its executor, for the report.
	executors map[string]skillcatalog.Executor
}

// prepareAuditRun is prepareRun's branch for a composite mode.
func (s *Service) prepareAuditRun(ctx context.Context, req RunRequest, resolved skillcatalog.Resolved,
	mode skillcatalog.Mode, project domain.ProjectRecord, inputs map[string]string,
) (preparedRun, error) {
	manifest := resolved.Package.Manifest
	refuse := func(err error) (preparedRun, error) {
		detail := err.Error()
		var coded *apierr.Error
		if errors.As(err, &coded) {
			detail = coded.Message
		}
		s.recordRun(ctx, store.SkillAuditRunRefused, req, resolved.Activation.Version, mode.ID, "", detail)
		return preparedRun{}, err
	}
	decision, err := skillcatalog.Authorize(skillcatalog.AuthorizationRequest{
		Manifest:           manifest,
		ModeID:             mode.ID,
		Grant:              resolved.Activation.Grant,
		SubjectPermissions: req.ActorPermissions,
		Runner:             skillcatalog.NoRunner(),
		CompositeParent:    true,
	})
	if err != nil {
		return refuse(apierr.Forbidden("SKILL_RUN_REFUSED", err.Error()))
	}
	// An audit that could run nothing is not accepted: it would be a run with
	// no possible result. Each child is checked QUIETLY here (no audit rows) and
	// prepared again, for real, when it launches.
	plan := &auditPlan{composes: append([]string(nil), mode.Composes...), executors: map[string]skillcatalog.Executor{}}
	var blocked []string
	runnable := 0
	for _, id := range mode.Composes {
		sub, _ := manifest.Mode(id)
		plan.executors[id] = sub.EffectiveExecutor()
		child := childRequest(req, id)
		child.quiet = true
		if _, err := s.prepareRun(ctx, child); err != nil {
			code, msg := errorParts(err)
			blocked = append(blocked, fmt.Sprintf("%s: %s %s", id, code, msg))
			continue
		}
		runnable++
	}
	if runnable == 0 {
		return refuse(apierr.Conflict(auditErrNothingRunnable,
			"no composed mode can run on this project now: "+strings.Join(blocked, "; "), nil))
	}
	return preparedRun{
		req: req, resolved: resolved, mode: mode, project: project, inputs: inputs,
		plan: skillcatalog.Plan{
			Resolved: resolved, Mode: mode, Decision: decision, Inputs: inputs,
			Runner: skillcatalog.RunnerAttestation{RunnerID: auditRunnerID}, PlannedAt: s.now(),
		},
		tool: skillrunner.Tool(AuditTool),
		scope: skillimage.Scope{
			TenantID: project.TenantID, ProjectID: req.ProjectID, SkillID: manifest.ID,
			Version: resolved.Activation.Version, ModeID: mode.ID,
		},
		audit: plan,
	}, nil
}

// childRequest is a composed mode's request: the parent's project, skill,
// caller and inputs -- minus the mode selector, which names the child -- and
// nothing any child produced.
func childRequest(parent RunRequest, modeID string) RunRequest {
	inputs := make(map[string]string, len(parent.Inputs))
	for k, v := range parent.Inputs {
		if k != modeInputName {
			inputs[k] = v
		}
	}
	return RunRequest{
		ProjectID: parent.ProjectID, SkillID: parent.SkillID, ModeID: modeID, Inputs: inputs,
		Actor: parent.Actor, ActorPermissions: append([]domain.Permission(nil), parent.ActorPermissions...),
	}
}

// auditModeResult is one composed mode's outcome as the audit saw it.
type auditModeResult struct {
	mode    string
	runID   string
	status  string // set only when there is no child run to read it from
	code    string
	message string
}

// executeAudit runs the composed modes in order and consolidates them.
func (s *Service) executeAudit(ctx, wctx context.Context, runID string, prep preparedRun) {
	e := s.runs
	results := make([]auditModeResult, 0, len(prep.audit.composes))
	for i, modeID := range prep.audit.composes {
		if ctx.Err() != nil {
			for _, rest := range prep.audit.composes[i:] {
				results = append(results, auditModeResult{mode: rest, status: "not_started",
					code: "SKILL_AUDIT_STOPPED", message: "the audit stopped before this mode started"})
			}
			break
		}
		cprep, err := s.prepareRun(ctx, childRequest(prep.req, modeID))
		if err != nil {
			code, msg := errorParts(err)
			results = append(results, auditModeResult{mode: modeID, status: "refused_before_start", code: code, message: msg})
			continue
		}
		rec, created, err := e.store.CreateSkillRun(wctx, s.runRecord(cprep, "", runID))
		if err != nil {
			results = append(results, auditModeResult{mode: modeID, status: "refused_before_start",
				code: auditErrChildNotCreated, message: err.Error()})
			continue
		}
		if !created || rec.ParentRunID != runID {
			// A run of this mode was already in flight on this project. Two
			// containers over one checkout is what single-flight exists to
			// prevent, and adopting a run this audit did not start would put
			// somebody else's authorization inside this report.
			results = append(results, auditModeResult{mode: modeID, runID: "", status: "busy", code: auditErrModeBusy,
				message: fmt.Sprintf("run %s of %s was already in flight; this audit did not run the mode", rec.ID, modeID)})
			continue
		}
		s.runChild(ctx, runID, rec.ID, cprep)
		results = append(results, auditModeResult{mode: modeID, runID: rec.ID})
	}

	switch {
	case s.cancelWasRequested(runID):
		s.finishUnsuccessful(wctx, runID, store.SkillRunCancelled, RunErrCancelled,
			"cancelled while running; the running mode was cancelled and the rest did not start", store.SkillRunImage{})
		return
	case e.base.Err() != nil:
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrShutdown,
			"the daemon shut down while the audit was running", store.SkillRunImage{})
		return
	}

	built, err := s.buildAuditReport(wctx, prep, runID, results)
	if err != nil {
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrInvalid, truncate(err.Error(), 2000), store.SkillRunImage{})
		return
	}
	switch {
	case built.verified == 0:
		s.finishUnsuccessful(wctx, runID, store.SkillRunFailed, RunErrAuditNoVerified,
			"no composed mode produced a verified report: "+strings.Join(built.gaps, "; "), store.SkillRunImage{})
		return
	case built.complete:
		ok, err := e.store.FinishSkillRunSucceeded(wctx, runID, store.SkillRunSuccess{
			Summary: built.summary, ReportJSON: built.reportJSON, ReportSHA256: built.sha,
			Findings: built.rows, FinishedAt: s.now(),
		})
		s.afterAuditFinish(wctx, prep, runID, ok, err, built)
	default:
		ok, err := e.store.FinishSkillRunPartial(wctx, runID, store.SkillRunPartialResult{
			Summary: built.summary, ReportJSON: built.reportJSON, ReportSHA256: built.sha,
			Findings: built.rows, ErrorCode: RunErrAuditPartial,
			ErrorMessage: truncate("not every mode produced a verified report: "+strings.Join(built.gaps, "; "), 2000),
			FinishedAt:   s.now(),
		})
		s.afterAuditFinish(wctx, prep, runID, ok, err, built)
	}
}

func (s *Service) afterAuditFinish(ctx context.Context, prep preparedRun, runID string, ok bool, err error, built auditBuild) {
	if err != nil {
		s.runs.log.Error("skills: could not record audit result", "run", runID, "err", err)
		s.finishUnsuccessful(ctx, runID, store.SkillRunFailed, RunErrInterrupted,
			"the audit completed but AO could not record its result", store.SkillRunImage{})
		return
	}
	if ok {
		s.recordRun(ctx, store.SkillAuditRunExecuted, prep.req, prep.scope.Version, prep.mode.ID, "", built.summary)
	}
}

// runChild executes one child run synchronously, registered as in flight so a
// cancel of the child alone, or of its audit, reaches it as a cancel.
func (s *Service) runChild(parentCtx context.Context, parentID, childID string, prep preparedRun) {
	e := s.runs
	ctx, cancel := context.WithCancel(parentCtx)
	e.mu.Lock()
	in := &inflightRun{cancel: cancel}
	if parent, ok := e.inflight[parentID]; ok {
		parent.activeChild = childID
		in.cancelRequested = parent.cancelRequested
	}
	e.inflight[childID] = in
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		delete(e.inflight, childID)
		if parent, ok := e.inflight[parentID]; ok && parent.activeChild == childID {
			parent.activeChild = ""
		}
		e.mu.Unlock()
	}()
	s.execute(ctx, childID, prep)
}

// errorParts splits a refusal into its code and message.
func errorParts(err error) (string, string) {
	var coded *apierr.Error
	if errors.As(err, &coded) {
		return coded.Code, coded.Message
	}
	return "SKILL_RUN_FAILED", err.Error()
}

// ---------------------------------------------------------------------------
// consolidation
// ---------------------------------------------------------------------------

type auditBuild struct {
	reportJSON []byte
	sha        string
	rows       []store.SkillRunFindingRecord
	summary    string
	complete   bool
	verified   int
	gaps       []string
}

type auditSource struct {
	Mode     string `json:"mode"`
	RunID    string `json:"runId"`
	RuleID   string `json:"ruleId"`
	Severity string `json:"severity"`
}

type auditFinding struct {
	ID             string        `json:"id"`
	Severity       string        `json:"severity"`
	Confidence     string        `json:"confidence"`
	Category       string        `json:"category"`
	Title          string        `json:"title"`
	Path           string        `json:"path"`
	Line           int           `json:"line,omitempty"`
	Recommendation string        `json:"recommendation"`
	Sources        []auditSource `json:"sources"`
}

type auditCoverage struct {
	Statement     string `json:"statement"`
	FilesStaged   *int   `json:"filesStaged,omitempty"`
	FilesExamined *int   `json:"filesExamined,omitempty"`
	FilesSkipped  *int   `json:"filesSkipped,omitempty"`
	Reconciled    *bool  `json:"reconciled,omitempty"`
	Dependencies  *int   `json:"dependencies,omitempty"`
}

type auditUsage struct {
	Source       string   `json:"source"`
	DurationMS   *int64   `json:"durationMs,omitempty"`
	InputTokens  *int64   `json:"inputTokens,omitempty"`
	OutputTokens *int64   `json:"outputTokens,omitempty"`
	CostUSD      *float64 `json:"costUsd,omitempty"`
}

type auditMode struct {
	Mode         string         `json:"mode"`
	Executor     string         `json:"executor"`
	Tool         string         `json:"tool,omitempty"`
	Status       string         `json:"status"`
	Verified     bool           `json:"verified"`
	RunID        string         `json:"runId,omitempty"`
	ReportSHA256 string         `json:"reportSha256,omitempty"`
	ErrorCode    string         `json:"errorCode,omitempty"`
	ErrorMessage string         `json:"errorMessage,omitempty"`
	FindingCount *int           `json:"findingCount,omitempty"`
	Coverage     *auditCoverage `json:"coverage,omitempty"`
	Limitations  []string       `json:"limitations,omitempty"`
	Usage        *auditUsage    `json:"usage,omitempty"`
}

type auditReport struct {
	SchemaVersion string         `json:"schemaVersion"`
	ProjectID     string         `json:"projectId"`
	SkillID       string         `json:"skillId"`
	SkillVersion  string         `json:"skillVersion"`
	AuditRunID    string         `json:"auditRunId"`
	StartedAt     string         `json:"startedAt"`
	EndedAt       string         `json:"endedAt"`
	Completeness  string         `json:"completeness"`
	Summary       auditSummary   `json:"summary"`
	Modes         []auditMode    `json:"modes"`
	Findings      []auditFinding `json:"findings"`
	Limitations   []string       `json:"limitations"`
}

type auditSummary struct {
	ModesPlanned  int              `json:"modesPlanned"`
	ModesVerified int              `json:"modesVerified"`
	Findings      int              `json:"findings"`
	BySeverity    map[string]int   `json:"bySeverity"`
	ByMode        []auditModeCount `json:"byMode"`
	Statements    []string         `json:"statements"`
}

type auditModeCount struct {
	Mode     string `json:"mode"`
	Findings int    `json:"findings"`
}

var severityRank = map[string]int{"info": 0, "low": 1, "medium": 2, "high": 3, "critical": 4}
var confidenceRank = map[string]int{"possible": 0, "probable": 1, "confirmed": 2}
var severityOrder = []string{"critical", "high", "medium", "low", "info"}

// agentLimitations are what an agent mode's reading cannot claim (ADR 0010),
// stated beside its results rather than in a document nobody opens.
var agentLimitations = []string{
	"Read by a host agent over a read-only staged copy, not inside a container; its findings are a model's " +
		"reading of the code and each needs a person to confirm it.",
	"An agent's empty findings list is not evidence of absence: see what it examined.",
}

// usageNote is the AO-authored note an agent report carries (agentrun.go).
var usageNote = regexp.MustCompile(`the provider reported (\d+) input and (\d+) output token\(s\), \$([0-9.]+), (\d+) turn`)

// buildAuditReport consolidates the children's verified reports. It reads
// every child back from the store and uses a child's findings ONLY when its
// stored report still hashes to its recorded digest.
func (s *Service) buildAuditReport(ctx context.Context, prep preparedRun, runID string, results []auditModeResult) (auditBuild, error) {
	parent, _, err := s.runs.store.GetSkillRun(ctx, runID)
	if err != nil {
		return auditBuild{}, err
	}
	started := s.now()
	if parent.StartedAt != nil {
		started = *parent.StartedAt
	}
	report := auditReport{
		SchemaVersion: skillreport.AuditSchemaVersion,
		ProjectID:     string(prep.req.ProjectID),
		SkillID:       prep.resolved.Package.Manifest.ID,
		SkillVersion:  prep.scope.Version,
		AuditRunID:    runID,
		StartedAt:     started.UTC().Format(time.RFC3339),
		EndedAt:       s.now().UTC().Format(time.RFC3339),
		Findings:      []auditFinding{},
		Limitations:   []string{},
	}
	byKey := map[string]int{}
	var unkeyed []auditFinding
	var build auditBuild
	byModeCount := map[string]int{}

	for _, r := range results {
		m := auditMode{Mode: r.mode, Executor: string(prep.audit.executors[r.mode]), Status: r.status,
			RunID: r.runID, ErrorCode: r.code, ErrorMessage: truncate(r.message, 1990)}
		if m.Executor == "" || m.Executor == string(skillcatalog.ExecutorComposite) {
			m.Executor = string(skillcatalog.ExecutorTool)
		}
		if r.runID != "" {
			d, err := s.GetRun(ctx, prep.req.ProjectID, r.runID)
			if err != nil {
				return auditBuild{}, fmt.Errorf("read child run %s: %w", r.runID, err)
			}
			m.Status, m.Tool = string(d.State), d.Tool
			m.ErrorCode, m.ErrorMessage = d.ErrorCode, truncate(d.ErrorMessage, 1990)
			m.Verified = d.State == store.SkillRunSucceeded && d.Integrity == "verified"
			if m.Verified {
				m.ReportSHA256 = d.ReportSHA256
				n := len(d.Findings)
				m.FindingCount = &n
				m.Coverage, m.Limitations, m.Usage = childCoverage(d)
				for _, f := range d.Findings {
					byModeCount[r.mode]++
					af := auditFinding{
						Severity: normSeverity(f.Severity), Confidence: normConfidence(f.Confidence),
						Category: f.Category, Title: f.Title, Path: f.Path, Line: f.Line,
						Recommendation: f.Recommendation,
						Sources:        []auditSource{{Mode: r.mode, RunID: r.runID, RuleID: f.RuleID, Severity: normSeverity(f.Severity)}},
					}
					if f.Line <= 0 {
						// A file-level finding (a denied credential file, a
						// manifest) merges with nothing: two rules about one
						// file are two facts.
						af.Line = 0
						unkeyed = append(unkeyed, af)
						continue
					}
					key := f.Category + "\x00" + f.Path + "\x00" + strconv.Itoa(f.Line)
					if i, ok := byKey[key]; ok {
						mergeFinding(&report.Findings[i], af)
						continue
					}
					byKey[key] = len(report.Findings)
					report.Findings = append(report.Findings, af)
				}
			} else if d.State == store.SkillRunSucceeded {
				m.ErrorCode, m.ErrorMessage = "SKILL_AUDIT_REPORT_UNVERIFIED",
					"the child's stored report does not hash to its recorded digest; its findings are not used"
			}
		}
		if m.Verified {
			build.verified++
		} else {
			build.gaps = append(build.gaps, fmt.Sprintf("%s %s %s", r.mode, m.Status, m.ErrorCode))
			report.Limitations = append(report.Limitations, fmt.Sprintf(
				"%s did not produce a verified report (%s%s); its checks were NOT performed in this audit.",
				r.mode, m.Status, prefixed(m.ErrorCode)))
		}
		report.Modes = append(report.Modes, m)
	}
	report.Findings = append(report.Findings, unkeyed...)
	sort.SliceStable(report.Findings, func(i, j int) bool {
		a, b := report.Findings[i], report.Findings[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] > severityRank[b.Severity]
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	bySeverity := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0, "info": 0}
	for i := range report.Findings {
		report.Findings[i].ID = fmt.Sprintf("AUD-%03d", i+1)
		bySeverity[report.Findings[i].Severity]++
	}
	build.complete = build.verified == len(results)
	report.Completeness = "partial"
	if build.complete {
		report.Completeness = "complete"
	}
	report.Limitations = append(report.Limitations,
		"Coverage is only what each verified mode reports below. No mode claims the absence of a vulnerability.",
		"Findings from different modes are merged only when they are the same category at the same file and line.",
		"No dynamic testing, network scanning or penetration testing was performed.")
	summary := auditSummary{
		ModesPlanned: len(results), ModesVerified: build.verified, Findings: len(report.Findings),
		BySeverity: bySeverity, ByMode: []auditModeCount{},
	}
	for _, r := range results {
		summary.ByMode = append(summary.ByMode, auditModeCount{Mode: r.mode, Findings: byModeCount[r.mode]})
	}
	summary.Statements = auditStatements(report, build, bySeverity)
	report.Summary = summary

	// Validate, redact, validate again -- the order every stored report follows.
	schema := skillreport.AuditSchema()
	raw, err := json.Marshal(report)
	if err != nil {
		return auditBuild{}, err
	}
	if err := schema.Validate(raw); err != nil {
		return auditBuild{}, fmt.Errorf("the consolidated report does not validate: %w", err)
	}
	decoded, err := skillreport.Decode(raw)
	if err != nil {
		return auditBuild{}, err
	}
	redacted, _ := skillreport.NewRedactor(nil).Redact(decoded)
	if err := schema.ValidateValue(redacted); err != nil {
		return auditBuild{}, fmt.Errorf("the consolidated report no longer validates after redaction: %w", err)
	}
	build.reportJSON, err = json.Marshal(redacted)
	if err != nil {
		return auditBuild{}, err
	}
	build.sha = sha256Hex(build.reportJSON)

	// Rows come from the bytes being stored, like every other run's.
	var stored auditReport
	if err := json.Unmarshal(build.reportJSON, &stored); err != nil {
		return auditBuild{}, err
	}
	for i, f := range stored.Findings {
		rules := make([]string, 0, len(f.Sources))
		for _, src := range f.Sources {
			rules = append(rules, src.RuleID)
		}
		build.rows = append(build.rows, store.SkillRunFindingRecord{
			RunID: runID, Ordinal: i, RuleID: strings.Join(rules, "+"), Severity: f.Severity,
			Category: f.Category, Title: f.Title, Path: f.Path, Line: f.Line,
			Recommendation: f.Recommendation, Confidence: f.Confidence,
		})
	}
	label := "COMPLETE"
	if !build.complete {
		label = "PARTIAL"
	}
	build.summary = fmt.Sprintf("security audit %s: %d of %d modes verified, %d findings (%d critical, %d high)",
		label, build.verified, len(results), len(stored.Findings), bySeverity["critical"], bySeverity["high"])
	return build, nil
}

// auditStatements is the executive summary: sentences built from counts and
// findings, nothing else. It says what was not covered before what was found.
func auditStatements(r auditReport, b auditBuild, bySeverity map[string]int) []string {
	out := []string{}
	if b.complete {
		out = append(out, fmt.Sprintf("Complete: all %d audit modes produced a verified report.", len(r.Modes)))
	} else {
		out = append(out, truncate(fmt.Sprintf("PARTIAL: %d of %d audit modes produced a verified report. Not covered: %s.",
			b.verified, len(r.Modes), strings.Join(b.gaps, ", ")), 390))
	}
	parts := make([]string, 0, len(severityOrder))
	for _, sev := range severityOrder {
		parts = append(parts, fmt.Sprintf("%d %s", bySeverity[sev], sev))
	}
	out = append(out, fmt.Sprintf("%d consolidated findings: %s.", len(r.Findings), strings.Join(parts, ", ")))
	for i, f := range r.Findings {
		if i == 5 || severityRank[f.Severity] < severityRank["high"] {
			break
		}
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		modes := make([]string, 0, len(f.Sources))
		for _, s := range f.Sources {
			modes = append(modes, s.Mode)
		}
		out = append(out, truncate(fmt.Sprintf("[%s] %s at %s (%s).", strings.ToUpper(f.Severity), f.Title, loc,
			strings.Join(modes, ", ")), 390))
	}
	out = append(out, "An empty or short findings list is not evidence of absence: read coverage and limitations.")
	return out
}

func mergeFinding(into *auditFinding, add auditFinding) {
	into.Sources = append(into.Sources, add.Sources...)
	if severityRank[add.Severity] > severityRank[into.Severity] {
		into.Severity = add.Severity
		into.Title, into.Recommendation = add.Title, add.Recommendation
	}
	if confidenceRank[add.Confidence] > confidenceRank[into.Confidence] {
		into.Confidence = add.Confidence
	}
}

func normSeverity(s string) string {
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "info"
}

func normConfidence(s string) string {
	if _, ok := confidenceRank[s]; ok {
		return s
	}
	return "possible"
}

func prefixed(code string) string {
	if code == "" {
		return ""
	}
	return ", " + code
}

// childCoverage reads a verified child's coverage, limitations and usage from
// its own report.
func childCoverage(d SkillRunDetail) (*auditCoverage, []string, *auditUsage) {
	usage := &auditUsage{Source: "wall-clock-only"}
	if d.StartedAt != nil && d.FinishedAt != nil {
		ms := d.FinishedAt.Sub(*d.StartedAt).Milliseconds()
		usage.DurationMS = &ms
	}
	if d.Report != nil {
		c := d.Report.Coverage
		staged, scanned := c.FilesStaged, c.FilesScanned
		skipped := c.FilesSkippedPreStage + c.FilesSkippedByScanner
		reconciled := c.Reconciled
		cov := &auditCoverage{
			Statement: fmt.Sprintf("%d of %d staged file(s) scanned; %d skipped before or during the scan",
				scanned, staged, skipped),
			FilesStaged: &staged, FilesExamined: &scanned, FilesSkipped: &skipped, Reconciled: &reconciled,
		}
		lim := append([]string(nil), c.Limitations...)
		if !c.Reconciled && c.ReconciliationNote != "" {
			lim = append(lim, c.ReconciliationNote)
		}
		if d.Report.Inventory != nil {
			n := d.Report.Inventory.Total
			cov.Dependencies = &n
			cov.Statement += fmt.Sprintf("; %d declared dependencies inventoried", n)
		}
		return cov, lim, usage
	}
	if d.AgentReport != nil {
		var rep struct {
			Coverage struct {
				Examined []string          `json:"examined"`
				Skipped  []json.RawMessage `json:"skipped"`
			} `json:"coverage"`
			Notes []string `json:"notes"`
		}
		_ = json.Unmarshal(d.AgentReport, &rep)
		examined, skipped := len(rep.Coverage.Examined), len(rep.Coverage.Skipped)
		cov := &auditCoverage{
			Statement:     fmt.Sprintf("the agent reports examining %d path(s); %d excluded or skipped", examined, skipped),
			FilesExamined: &examined, FilesSkipped: &skipped,
		}
		for _, n := range rep.Notes {
			m := usageNote.FindStringSubmatch(n)
			if m == nil {
				continue
			}
			in, _ := strconv.ParseInt(m[1], 10, 64)
			out, _ := strconv.ParseInt(m[2], 10, 64)
			cost, _ := strconv.ParseFloat(m[3], 64)
			usage.Source = "provider-reported"
			usage.InputTokens, usage.OutputTokens, usage.CostUSD = &in, &out, &cost
		}
		return cov, append([]string(nil), agentLimitations...), usage
	}
	return &auditCoverage{Statement: "the child's report carries no coverage AO can read"}, nil, usage
}
