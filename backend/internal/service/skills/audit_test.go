package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillagent"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillimage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// audit_test.go pins the 2E contract (ADR 0011) against the REAL SQLite store:
// an audit is a parent run of one child run per composed mode, each on its own
// boundary; the consolidated report uses only verified children; a partial
// audit is 'partial', never 'succeeded'.

// perToolExecutor is the container runner stand-in: one report per tool, an
// optional per-tool error, and an optional tool that blocks until released.
type perToolExecutor struct {
	mu        sync.Mutex
	reports   map[skillrunner.Tool]skillrunner.StaticScanReport
	errs      map[skillrunner.Tool]error
	blockTool skillrunner.Tool
	started   chan string
	release   chan struct{}
	seen      []skillrunner.StaticScanRequest
}

func (e *perToolExecutor) Attestation() skillcatalog.RunnerAttestation { return fullyAttested() }
func (e *perToolExecutor) Execute(context.Context, skillcatalog.Plan) (skillcatalog.Result, error) {
	return skillcatalog.Result{}, skillcatalog.ErrNoRunner
}

func (e *perToolExecutor) RunStaticScan(ctx context.Context, authority skillrunner.ImageAuthority,
	req skillrunner.StaticScanRequest,
) (skillrunner.StaticScanReport, error) {
	e.mu.Lock()
	e.seen = append(e.seen, req)
	e.mu.Unlock()
	if req.Tool == e.blockTool && e.blockTool != "" {
		e.started <- req.RunID
		select {
		case <-e.release:
		case <-ctx.Done():
			return skillrunner.StaticScanReport{}, ctx.Err()
		}
	}
	approval, err := authority.ApprovedImage(ctx, req.Scope, string(req.Tool))
	if err != nil {
		return skillrunner.StaticScanReport{}, err
	}
	if err := e.errs[req.Tool]; err != nil {
		return skillrunner.StaticScanReport{}, err
	}
	r := e.reports[req.Tool]
	r.Tool, r.ImageDigest, r.ApprovalID, r.ApprovedBy = string(req.Tool), approval.Digest, approval.ID, approval.ApprovedBy
	return r, nil
}

func toolReport(findings ...skillrunner.ScanFinding) skillrunner.StaticScanReport {
	return skillrunner.StaticScanReport{
		SchemaVersion: "ao.static-scan/v1",
		Coverage: skillrunner.ScanCoverage{FilesDiscovered: 4, FilesStaged: 4, FilesVisible: 4, FilesScanned: 3,
			FilesSkippedByScanner: 1, Reconciled: true, Limitations: []string{"pattern scanner"}},
		Findings: findings,
	}
}

type auditRig struct {
	f       fixture
	svc     *skills.Service
	exec    *perToolExecutor
	agent   *fakeAgent
	auth    *skills.ImageAuthority
	project domain.ProjectID
	version string
}

// newAuditRig enables security-audit with every capability the audit needs,
// and approves images for the modes named in approve.
func newAuditRig(t *testing.T, owner string, agent *fakeAgent, approve ...string) auditRig {
	t.Helper()
	f := newFixture(t)
	version := mustInstall(t, f)
	project := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: project, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapDepsRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	exec := &perToolExecutor{
		reports: map[skillrunner.Tool]skillrunner.StaticScanReport{
			skillrunner.ToolSecretScan: toolReport(skillrunner.ScanFinding{RuleID: "SEC-011", Severity: "high",
				Category: "secret", Title: "Credential-shaped literal", Path: "app/db.py", Line: 3,
				Recommendation: "rotate", Confidence: "possible"}),
			skillrunner.ToolDependencyScan: toolReport(skillrunner.ScanFinding{RuleID: "DEP-003", Severity: "medium",
				Category: "dependency", Title: "No lockfile", Path: "tools/package.json", Line: 2,
				Recommendation: "commit a lockfile", Confidence: "confirmed"}),
			skillrunner.ToolStaticScan: toolReport(skillrunner.ScanFinding{RuleID: "AOSS-006", Severity: "critical",
				Category: "secret", Title: "Credential-shaped literal assigned in source", Path: "app/db.py", Line: 3,
				Recommendation: "rotate first", Confidence: "possible"}),
		},
		errs:    map[skillrunner.Tool]error{},
		started: make(chan string, 8), release: make(chan struct{}),
	}
	auth := skills.NewImageAuthority(f.store, f.store).WithImageInspector(acceptAll())
	tools := map[string]skillrunner.Tool{
		"secret-scan": skillrunner.ToolSecretScan, "dependencies": skillrunner.ToolDependencyScan,
		"static-code": skillrunner.ToolStaticScan,
	}
	for _, mode := range approve {
		scope := skillimage.Scope{TenantID: domain.DefaultTenantID, ProjectID: project,
			SkillID: "security-audit", Version: version, ModeID: mode}
		req := approveRequest(scope, digestOf('c'))
		req.Tool = string(tools[mode])
		if _, err := auth.Approve(context.Background(), req); err != nil {
			t.Fatalf("Approve %s: %v", mode, err)
		}
	}
	opts := []skills.Option{
		skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithDurableRuns(f.store, owner, &recordingReaper{}, nil),
	}
	if agent != nil {
		opts = append(opts, skills.WithAgentExecutor(agent, nil))
	}
	svc := skills.New(f.store, f.dataDir, opts...)
	t.Cleanup(func() { svc.CloseRuns(5 * time.Second) })
	return auditRig{f: f, svc: svc, exec: exec, agent: agent, auth: auth, project: project, version: version}
}

func allTools() []string { return []string{"secret-scan", "dependencies", "static-code"} }

func (r auditRig) start(t *testing.T, key string) skills.SkillRun {
	t.Helper()
	run, _, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: r.project, SkillID: "security-audit", ModeID: "full-audit",
		Actor: admin, ActorPermissions: adminPerms(),
	}, IdempotencyKey: key})
	if err != nil {
		t.Fatalf("StartRun full-audit: %v", err)
	}
	return run
}

func (r auditRig) wait(t *testing.T, id string) skills.SkillRunDetail {
	t.Helper()
	return waitRun(t, r.svc, r.project, id)
}

type auditDoc struct {
	SchemaVersion string `json:"schemaVersion"`
	AuditRunID    string `json:"auditRunId"`
	Completeness  string `json:"completeness"`
	Summary       struct {
		ModesPlanned  int            `json:"modesPlanned"`
		ModesVerified int            `json:"modesVerified"`
		BySeverity    map[string]int `json:"bySeverity"`
		Statements    []string       `json:"statements"`
	} `json:"summary"`
	Modes []struct {
		Mode, Status, RunID, ErrorCode, ReportSHA256, Executor string
		Verified                                               bool
	} `json:"modes"`
	Findings []struct {
		ID, Severity, Confidence, Category, Path string
		Line                                     int
		Sources                                  []struct{ Mode, RunID, RuleID, Severity string }
	} `json:"findings"`
	Limitations []string `json:"limitations"`
}

func decodeAudit(t *testing.T, d skills.SkillRunDetail) auditDoc {
	t.Helper()
	if d.AgentReport == nil || d.Integrity != "verified" {
		t.Fatalf("no verified audit report (integrity %s)", d.Integrity)
	}
	if err := skillreport.AuditSchema().Validate(d.AgentReport); err != nil {
		t.Fatalf("the stored audit report does not validate: %v", err)
	}
	var doc auditDoc
	if err := json.Unmarshal(d.AgentReport, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestAudit_CompleteRunsEveryModeAsItsOwnRunAndConsolidates(t *testing.T) {
	r := newAuditRig(t, "aod-audit-1", newFakeAgent(agentReport(nil)), allTools()...)
	run := r.start(t, "")
	if run.Tool != skills.AuditTool {
		t.Fatalf("tool = %s", run.Tool)
	}
	d := r.wait(t, run.ID)
	if d.State != store.SkillRunSucceeded {
		t.Fatalf("state = %s %s %s", d.State, d.ErrorCode, d.ErrorMessage)
	}
	doc := decodeAudit(t, d)
	if doc.Completeness != "complete" || doc.Summary.ModesVerified != 4 || doc.AuditRunID != run.ID {
		t.Fatalf("doc = %+v", doc.Summary)
	}
	// Each composed mode ran as its own child run, in manifest order, with its
	// own tool -- and each child's own report is still there, verified.
	if len(d.Children) != 4 {
		t.Fatalf("children = %d", len(d.Children))
	}
	wantOrder := []string{"secret-scan", "dependencies", "static-code", "authz-review"}
	for i, c := range d.Children {
		if c.ModeID != wantOrder[i] || c.ParentRunID != run.ID || c.State != store.SkillRunSucceeded {
			t.Fatalf("child %d = %s parent=%s state=%s", i, c.ModeID, c.ParentRunID, c.State)
		}
		cd, err := r.svc.GetRun(context.Background(), r.project, c.ID)
		if err != nil || cd.Integrity != "verified" {
			t.Fatalf("child %s report integrity %s err %v", c.ModeID, cd.Integrity, err)
		}
		if doc.Modes[i].ReportSHA256 != cd.ReportSHA256 || doc.Modes[i].RunID != c.ID {
			t.Fatalf("mode %s does not cite its child's verified report", c.ModeID)
		}
	}
	if doc.Modes[3].Executor != "agent" || doc.Modes[0].Executor != "tool" {
		t.Fatalf("executors = %s, %s", doc.Modes[0].Executor, doc.Modes[3].Executor)
	}
	// The container attestation and the agent attestation stay on their own runs.
	for _, c := range d.Children {
		want := "container/docker"
		if c.ModeID == "authz-review" {
			want = "host-agent/claude-code"
		}
		if c.RunnerID != want {
			t.Fatalf("child %s ran on %s", c.ModeID, c.RunnerID)
		}
	}
	if d.RunnerID != "composite" || len(d.Controls) != 0 {
		t.Fatalf("the parent claimed an execution environment: %s %v", d.RunnerID, d.Controls)
	}
	// Same category at the same file and line: merged, stricter severity, both sources.
	merged := false
	for _, f := range doc.Findings {
		if f.Path == "app/db.py" && f.Line == 3 {
			merged = len(f.Sources) == 2 && f.Severity == "critical"
		}
	}
	if !merged || len(doc.Findings) != 3 {
		t.Fatalf("findings = %+v", doc.Findings)
	}
	if doc.Findings[0].Severity != "critical" || doc.Summary.BySeverity["critical"] != 1 {
		t.Fatal("findings are not ordered by severity")
	}
	if len(d.Findings) != 3 || d.Findings[0].RuleID != "SEC-011+AOSS-006" {
		t.Fatalf("parent finding rows = %+v", d.Findings)
	}
	if !strings.HasPrefix(doc.Summary.Statements[0], "Complete:") {
		t.Fatalf("statements = %v", doc.Summary.Statements)
	}
	// Nothing flowed between children: every tool request carries only the
	// caller's inputs, and the agent got the package's own instructions.
	for _, req := range r.exec.seen {
		if req.Scope.ModeID == "full-audit" {
			t.Fatal("a tool ran under the audit's scope")
		}
	}
}

func TestAudit_AMissingProviderMakesItPartialNeverSucceeded(t *testing.T) {
	agent := newFakeAgent(agentReport(nil))
	agent.att = skillcatalog.RunnerAttestation{RunnerID: "host-agent/claude-code"}
	agent.unavailable = "claude is not installed where the daemon can find it"
	r := newAuditRig(t, "aod-audit-2", agent, allTools()...)
	d := r.wait(t, r.start(t, "").ID)
	if d.State != store.SkillRunPartial || d.ErrorCode != "SKILL_AUDIT_PARTIAL" {
		t.Fatalf("state=%s code=%s", d.State, d.ErrorCode)
	}
	doc := decodeAudit(t, d)
	if doc.Completeness != "partial" || doc.Summary.ModesVerified != 3 {
		t.Fatalf("summary = %+v", doc.Summary)
	}
	last := doc.Modes[3]
	if last.Mode != "authz-review" || last.Status != "refused_before_start" || last.ErrorCode != "SKILL_AGENT_UNAVAILABLE" || last.Verified {
		t.Fatalf("authz-review = %+v", last)
	}
	if !strings.HasPrefix(doc.Summary.Statements[0], "PARTIAL:") ||
		!strings.Contains(strings.Join(doc.Limitations, " "), "authz-review did not produce a verified report") {
		t.Fatalf("the partial audit does not say what it did not cover: %v / %v", doc.Summary.Statements, doc.Limitations)
	}
	if len(d.Children) != 3 {
		t.Fatalf("a refused mode created a child run: %d", len(d.Children))
	}
}

func TestAudit_AnUnapprovedImageOrInvalidAgentOutputIsPartial(t *testing.T) {
	// dependencies has no image approval: its child runs and ends refused.
	// The agent's output is not a report: its child ends failed.
	r := newAuditRig(t, "aod-audit-3", newFakeAgent("The project is fine."), "secret-scan", "static-code")
	d := r.wait(t, r.start(t, "").ID)
	if d.State != store.SkillRunPartial {
		t.Fatalf("state = %s", d.State)
	}
	doc := decodeAudit(t, d)
	got := map[string]string{}
	for _, m := range doc.Modes {
		got[m.Mode] = m.Status + " " + m.ErrorCode
	}
	if got["dependencies"] != "refused SKILL_IMAGE_NOT_APPROVED" || got["authz-review"] != "failed SKILL_RUN_OUTPUT_INVALID" {
		t.Fatalf("modes = %v", got)
	}
	if doc.Summary.ModesVerified != 2 {
		t.Fatalf("verified = %d", doc.Summary.ModesVerified)
	}
}

func TestAudit_NoVerifiedResultIsFailedWithoutAReport(t *testing.T) {
	agent := newFakeAgent(agentReport(nil))
	agent.err = errors.New("provider exploded")
	r := newAuditRig(t, "aod-audit-4", agent) // no image approved at all
	d := r.wait(t, r.start(t, "").ID)
	if d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrAuditNoVerified || d.AgentReport != nil {
		t.Fatalf("state=%s code=%s report=%v", d.State, d.ErrorCode, d.AgentReport != nil)
	}
	if len(d.Children) != 4 {
		t.Fatalf("children = %d", len(d.Children))
	}
}

func TestAudit_RefusedBeforeAcceptance(t *testing.T) {
	// Nothing could run (no runner, no agent): no audit run.
	f := newFixture(t)
	version := mustInstall(t, f)
	project := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: project, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapDepsRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatal(err)
	}
	svc := skills.New(f.store, f.dataDir, skills.WithAgentExecutor(nil, f.store),
		skills.WithDurableRuns(f.store, "aod-audit-5", nil, nil))
	defer svc.CloseRuns(time.Second)
	req := skills.StartRunRequest{RunRequest: skills.RunRequest{ProjectID: project, SkillID: "security-audit",
		ModeID: "full-audit", Actor: admin, ActorPermissions: adminPerms()}}
	if _, _, err := svc.StartRun(context.Background(), req); apiCode(t, err) != "SKILL_AUDIT_NOTHING_RUNNABLE" {
		t.Fatalf("err = %v", err)
	}
	// A grant without deps.read cannot run the audit: capabilities are never
	// granted on the audit's behalf.
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: project, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StartRun(context.Background(), req); apiCode(t, err) != "SKILL_RUN_REFUSED" ||
		!strings.Contains(err.Error(), "deps.read") {
		t.Fatalf("err = %v", err)
	}
	runs, _ := svc.ListRuns(context.Background(), project, 10)
	if len(runs) != 0 {
		t.Fatalf("refusals created %d run(s)", len(runs))
	}
}

func TestAudit_CancelStopsTheRunningChildAndTheRest(t *testing.T) {
	r := newAuditRig(t, "aod-audit-6", newFakeAgent(agentReport(nil)), allTools()...)
	r.exec.blockTool = skillrunner.ToolDependencyScan
	run := r.start(t, "")
	child := <-r.exec.started
	if _, err := r.svc.CancelRun(context.Background(), r.project, run.ID); err != nil {
		t.Fatal(err)
	}
	d := r.wait(t, run.ID)
	if d.State != store.SkillRunCancelled || d.AgentReport != nil {
		t.Fatalf("audit = %s", d.State)
	}
	cd := r.wait(t, child)
	if cd.State != store.SkillRunCancelled {
		t.Fatalf("the running child ended %s %s, want cancelled", cd.State, cd.ErrorCode)
	}
	if len(d.Children) != 2 {
		t.Fatalf("modes after the cancelled one started: %d children", len(d.Children))
	}
}

func TestAudit_ShutdownEndsTheAuditAndItsChild(t *testing.T) {
	r := newAuditRig(t, "aod-audit-7", newFakeAgent(agentReport(nil)), allTools()...)
	r.exec.blockTool = skillrunner.ToolStaticScan
	run := r.start(t, "")
	child := <-r.exec.started
	r.svc.CloseRuns(5 * time.Second)
	d, _ := r.svc.GetRun(context.Background(), r.project, run.ID)
	cd, _ := r.svc.GetRun(context.Background(), r.project, child)
	if d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrShutdown ||
		cd.State != store.SkillRunFailed || cd.ErrorCode != skills.RunErrShutdown {
		t.Fatalf("audit %s/%s child %s/%s", d.State, d.ErrorCode, cd.State, cd.ErrorCode)
	}
}

func TestAudit_AModeAlreadyInFlightIsNotAdopted(t *testing.T) {
	r := newAuditRig(t, "aod-audit-8", newFakeAgent(agentReport(nil)), allTools()...)
	r.exec.blockTool = skillrunner.ToolStaticScan
	standalone, _, err := r.svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: r.project, SkillID: "security-audit", ModeID: "static-code", Actor: admin, ActorPermissions: adminPerms(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-r.exec.started
	d := r.wait(t, r.start(t, "").ID)
	close(r.exec.release)
	if d.State != store.SkillRunPartial {
		t.Fatalf("state = %s", d.State)
	}
	doc := decodeAudit(t, d)
	for _, m := range doc.Modes {
		if m.Mode == "static-code" && (m.Status != "busy" || m.RunID != "") {
			t.Fatalf("static-code = %+v", m)
		}
	}
	if sd := r.wait(t, standalone.ID); sd.ParentRunID != "" {
		t.Fatal("the standalone run was adopted by the audit")
	}
}

func TestAudit_IdempotentAndReconciled(t *testing.T) {
	r := newAuditRig(t, "aod-new", newFakeAgent(agentReport(nil)), allTools()...)
	a := r.start(t, "audit-key-1")
	b := r.start(t, "audit-key-1")
	if a.ID != b.ID {
		t.Fatal("the same idempotency key started two audits")
	}
	r.wait(t, a.ID)

	ctx := context.Background()
	parent, _, err := r.f.store.CreateSkillRun(ctx, store.SkillRunRecord{
		ID: "skr-aaaaaaaaaaaaaaaaaaaaaaaa", ProjectID: r.project, SkillID: "security-audit",
		SkillVersion: r.version, ModeID: "full-audit", Tool: skills.AuditTool,
		OwnerInstance: "aod-dead", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	childRec, _, err := r.f.store.CreateSkillRun(ctx, store.SkillRunRecord{
		ID: "skr-bbbbbbbbbbbbbbbbbbbbbbbb", ProjectID: r.project, SkillID: "security-audit",
		SkillVersion: r.version, ModeID: "secret-scan", Tool: string(skillrunner.ToolSecretScan),
		OwnerInstance: "aod-dead", CreatedAt: time.Now().UTC(), ParentRunID: parent.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent.ID, childRec.ID} {
		if _, err := r.f.store.MarkSkillRunRunning(ctx, id, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	n, err := r.svc.ReconcileRuns(ctx)
	if err != nil || n != 2 {
		t.Fatalf("reconciled %d, err %v", n, err)
	}
	for _, id := range []string{parent.ID, childRec.ID} {
		d, _ := r.svc.GetRun(ctx, r.project, id)
		if d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrInterrupted {
			t.Fatalf("%s = %s %s", id, d.State, d.ErrorCode)
		}
	}
}

func TestAudit_SecretsDoNotReachTheConsolidatedReport(t *testing.T) {
	out := agentReport(func(doc map[string]any) {
		f := doc["findings"].([]any)[0].(map[string]any)
		f["title"] = "Password " + plantedSecret + " is hardcoded"
	})
	r := newAuditRig(t, "aod-audit-9", newFakeAgent(out), allTools()...)
	d := r.wait(t, r.start(t, "").ID)
	if d.State != store.SkillRunSucceeded {
		t.Fatalf("state = %s", d.State)
	}
	if strings.Contains(string(d.AgentReport), plantedSecret) {
		t.Fatal("the consolidated report carries the secret")
	}
	assertNotInDB(t, r.f.dataDir, plantedSecret[:8])
}

func TestAudit_DryRunNamesTheModesThatWouldNotRun(t *testing.T) {
	agent := newFakeAgent(agentReport(nil))
	agent.att = skillcatalog.RunnerAttestation{RunnerID: "host-agent/claude-code"}
	agent.unavailable = "no claude"
	r := newAuditRig(t, "aod-audit-10", agent, allTools()...)
	dry, err := r.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: r.project, SkillID: "security-audit", ModeID: "full-audit",
		Inputs: map[string]string{"mode": "full-audit"}, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatal(err)
	}
	reasons := strings.Join(dry.Reasons, " | ")
	if dry.Verdict != skills.DryRunExecutable || !strings.Contains(reasons, "authz-review would not run (the audit would be PARTIAL)") {
		t.Fatalf("verdict=%s reasons=%s", dry.Verdict, reasons)
	}
	if len(dry.Runner.MissingControls) != 0 {
		t.Fatalf("the audit parent claims missing controls: %v", dry.Runner.MissingControls)
	}
}

// Cancelling ONE child cancels that mode only: the audit goes on with the
// rest and ends partial, naming the cancelled mode.
func TestAudit_CancellingOneChildLeavesTheAuditRunning(t *testing.T) {
	r := newAuditRig(t, "aod-audit-11", newFakeAgent(agentReport(nil)), allTools()...)
	r.exec.blockTool = skillrunner.ToolDependencyScan
	run := r.start(t, "")
	child := <-r.exec.started
	if _, err := r.svc.CancelRun(context.Background(), r.project, child); err != nil {
		t.Fatal(err)
	}
	d := r.wait(t, run.ID)
	if d.State != store.SkillRunPartial {
		t.Fatalf("audit = %s %s", d.State, d.ErrorCode)
	}
	doc := decodeAudit(t, d)
	got := map[string]string{}
	for _, m := range doc.Modes {
		got[m.Mode] = m.Status
	}
	if got["dependencies"] != "cancelled" || got["static-code"] != "succeeded" || got["authz-review"] != "succeeded" {
		t.Fatalf("modes = %v", got)
	}
}

// A child that fails with a very long message still yields a valid
// consolidated report: the audit bounds what it quotes to its own schema.
func TestAudit_ALongChildErrorStillValidates(t *testing.T) {
	agent := newFakeAgent(agentReport(nil))
	agent.err = fmt.Errorf("%w: %s", skillagent.ErrProvider, strings.Repeat("x", 6000))
	r := newAuditRig(t, "aod-audit-12", agent, allTools()...)
	d := r.wait(t, r.start(t, "").ID)
	if d.State != store.SkillRunPartial {
		t.Fatalf("state = %s %s %s", d.State, d.ErrorCode, d.ErrorMessage)
	}
	decodeAudit(t, d)
}
