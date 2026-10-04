package skills_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillagent"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// agentrun_test.go pins the 2C contract -- an agent mode on the durable run --
// against the REAL SQLite store, with a fake agent executor that returns
// whatever output a test needs, including hostile output.

const plantedSecret = "Hunter2-prod-771188"

func hostAgentAttestation() skillcatalog.RunnerAttestation {
	return skillcatalog.RunnerAttestation{RunnerID: skillagent.RunnerID, Controls: []skillcatalog.Control{
		skillcatalog.ControlStagedReadOnlyCopy, skillcatalog.ControlAgentToolConfinement,
		skillcatalog.ControlScrubbedEnvironment, skillcatalog.ControlTamperDetection,
	}}
}

type fakeAgent struct {
	mu          sync.Mutex
	att         skillcatalog.RunnerAttestation
	unavailable string
	output      string
	err         error
	block       bool
	started     chan string
	release     chan struct{}
	got         []skillagent.Request
	reaped      []string
}

func newFakeAgent(output string) *fakeAgent {
	return &fakeAgent{
		att: hostAgentAttestation(), output: output,
		started: make(chan string, 8), release: make(chan struct{}),
	}
}

func (a *fakeAgent) Attestation() skillcatalog.RunnerAttestation { return a.att }
func (a *fakeAgent) Unavailable() string                         { return a.unavailable }
func (a *fakeAgent) Model() string                               { return "sonnet" }

func (a *fakeAgent) Run(ctx context.Context, req skillagent.Request) (skillagent.Result, error) {
	a.mu.Lock()
	a.got = append(a.got, req)
	a.mu.Unlock()
	if a.block {
		a.started <- req.RunID
		select {
		case <-a.release:
		case <-ctx.Done():
			return skillagent.Result{}, fmt.Errorf("%w: interrupted: %w", skillagent.ErrProvider, ctx.Err())
		}
	}
	res := skillagent.Result{
		Output:    []byte(a.output),
		StartedAt: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
		EndedAt:   time.Date(2026, 9, 23, 10, 3, 0, 0, time.UTC),
		Staging: skillrunner.Staging{
			Inputs: []skillrunner.StagedInput{
				{RelPath: "api/orders.go"}, {RelPath: "api/auth.go"}, {RelPath: "README.md"},
			},
			Skipped:      []skillrunner.SkippedFile{{Path: ".env", Reason: "denied-by-manifest"}},
			SkippedCount: 1,
		},
		Literals: []string{plantedSecret},
		Evidence: skillagent.Evidence{Model: "sonnet", PermissionDenials: 1, DeniedTools: []string{"Read"}},
	}
	return res, a.err
}

func (a *fakeAgent) Reap(runID string) (bool, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reaped = append(a.reaped, runID)
	return true, "", nil
}

// agentReport builds a findings.v1 document; edit tweaks it before encoding.
func agentReport(edit func(doc map[string]any)) string {
	doc := map[string]any{
		"schemaVersion": "security-audit/findings/v1",
		"run": map[string]any{
			"projectId": "whatever-the-agent-says", "mode": "authz-review", "skillVersion": "9.9.9",
			"startedAt": "2026-09-23T10:00:00Z", "endedAt": "2026-09-23T10:01:00Z",
		},
		"coverage": map[string]any{
			"examined": []any{"api/orders.go", "api"},
			"skipped":  []any{},
		},
		"findings": []any{map[string]any{
			"id": "AUTHZ-1", "title": "GetOrder reads by id before checking the tenant", "severity": "high",
			"confidence": "probable", "category": "idor",
			"evidence": map[string]any{
				"summary":   "orders.go:12 selects by id only",
				"locations": []any{map[string]any{"path": "./api/orders.go", "line": 12}},
			},
			"reproduction":   map[string]any{"reproducible": false, "steps": []any{"GET /orders/2 as tenant 1"}},
			"recommendation": "Add the tenant predicate to the query.",
		}},
		"notes": []any{},
	}
	if edit != nil {
		edit(doc)
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

type agentRig struct {
	f       fixture
	svc     *skills.Service
	agent   *fakeAgent
	project domain.ProjectID
	version string
}

func newAgentRig(t *testing.T, agent *fakeAgent, owner string, caps ...skillcatalog.Capability) agentRig {
	t.Helper()
	f := newFixture(t)
	version := mustInstall(t, f)
	project := f.seedProject(t, "medusa")
	if len(caps) == 0 {
		caps = []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite}
	}
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: project, SkillID: "security-audit", Version: version, Capabilities: caps,
		Actor: admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	var opt skills.Option
	if agent != nil {
		opt = skills.WithAgentExecutor(agent, f.store)
	} else {
		opt = skills.WithAgentExecutor(nil, f.store)
	}
	svc := skills.New(f.store, f.dataDir, opt,
		skills.WithDurableRuns(f.store, owner, &recordingReaper{}, nil))
	t.Cleanup(func() { svc.CloseRuns(5 * time.Second) })
	return agentRig{f: f, svc: svc, agent: agent, project: project, version: version}
}

func (r agentRig) req() skills.StartRunRequest {
	return skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: r.project, SkillID: "security-audit", ModeID: "authz-review",
		Actor: admin, ActorPermissions: adminPerms(),
	}}
}

func (r agentRig) runToEnd(t *testing.T) skills.SkillRunDetail {
	t.Helper()
	run, created, err := r.svc.StartRun(context.Background(), r.req())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if !created {
		t.Fatal("StartRun did not create a run")
	}
	return r.wait(t, run.ID)
}

func (r agentRig) wait(t *testing.T, runID string) skills.SkillRunDetail {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, err := r.svc.GetRun(context.Background(), r.project, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.State.Terminal() {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %q", runID, d.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentRun_SucceedsAndStoresAValidatedCompletedReport(t *testing.T) {
	r := newAgentRig(t, newFakeAgent(agentReport(nil)), "aod-agent-1")
	d := r.runToEnd(t)
	if d.State != store.SkillRunSucceeded {
		t.Fatalf("state = %s (%s: %s)", d.State, d.ErrorCode, d.ErrorMessage)
	}
	if d.Tool != skillagent.Tool || d.RunnerID != skillagent.RunnerID {
		t.Fatalf("tool=%q runner=%q", d.Tool, d.RunnerID)
	}
	if d.Integrity != "verified" || d.AgentReport == nil || d.Report != nil {
		t.Fatalf("integrity=%q agentReport=%v staticReport=%v", d.Integrity, d.AgentReport != nil, d.Report != nil)
	}
	var rep map[string]any
	if err := json.Unmarshal(d.AgentReport, &rep); err != nil {
		t.Fatal(err)
	}
	run := rep["run"].(map[string]any)
	// What only AO knows is AO's: the agent's claims were overwritten.
	if run["projectId"] != "medusa" || run["skillVersion"] != r.version || run["mode"] != "authz-review" ||
		run["startedAt"] != "2026-09-23T10:00:00Z" || run["endedAt"] != "2026-09-23T10:03:00Z" {
		t.Fatalf("run block not AO's: %v", run)
	}
	skipped := rep["coverage"].(map[string]any)["skipped"].([]any)
	if len(skipped) != 1 || !strings.Contains(skipped[0].(map[string]any)["reason"].(string), "denied-by-manifest") {
		t.Fatalf("AO's staging exclusions missing from coverage: %v", skipped)
	}
	notes := fmt.Sprint(rep["notes"])
	if !strings.Contains(notes, skillagent.RunnerID) || !strings.Contains(notes, "1 tool call(s) refused") {
		t.Fatalf("notes = %s", notes)
	}
	if len(d.Findings) != 1 || d.Findings[0].RuleID != "AUTHZ-1" || d.Findings[0].Path != "api/orders.go" ||
		d.Findings[0].Line != 12 || d.Findings[0].Severity != "high" {
		t.Fatalf("findings = %+v", d.Findings)
	}
	if !strings.Contains(d.Summary, "examined 2 of 3 staged files, 1 findings") {
		t.Fatalf("summary = %q", d.Summary)
	}

	// The agent got the VERIFIED package bytes and the manifest's scope, and
	// nothing the caller said.
	got := r.agent.got[0]
	skillMD, _ := os.ReadFile(filepath.Join(securitySrc, "SKILL.md"))
	guide, _ := os.ReadFile(filepath.Join(securitySrc, "modes", "authz-review.md"))
	if got.Instructions != string(skillMD) || got.ModeGuide != string(guide) {
		t.Fatal("the agent did not get the package's own SKILL.md and mode guide")
	}
	if string(got.Schema) != string(skillcatalog.CanonicalFindingsSchema()) {
		t.Fatal("the agent did not get the canonical schema")
	}
	if strings.Join(got.DenyGlobs, ",") != ".env,.env.*,**/*.pem,**/*.key,**/id_rsa*" {
		t.Fatalf("deny globs = %v", got.DenyGlobs)
	}
	if got.ProjectPath != filepath.Join("/tmp", "medusa") || got.RunID != d.ID {
		t.Fatalf("request = %+v", got)
	}
}

// A secret the agent copied into its report -- verbatim, as a prefix, in the
// finding table's columns -- never reaches the database.
func TestAgentRun_SecretsInTheReportAreRedactedBeforeStorage(t *testing.T) {
	out := agentReport(func(doc map[string]any) {
		f := doc["findings"].([]any)[0].(map[string]any)
		f["title"] = "Hardcoded DB password " + plantedSecret
		f["recommendation"] = "Rotate AKIAIOSFODNN7EXAMPLE and " + plantedSecret[:10]
		f["evidence"].(map[string]any)["locations"].([]any)[0].(map[string]any)["excerpt"] = `db_password = "` + plantedSecret + `"`
		doc["notes"] = []any{"the value is " + plantedSecret}
	})
	r := newAgentRig(t, newFakeAgent(out), "aod-agent-2")
	d := r.runToEnd(t)
	if d.State != store.SkillRunSucceeded {
		t.Fatalf("state = %s (%s)", d.State, d.ErrorMessage)
	}
	if !strings.Contains(d.Summary, "redacted") {
		t.Fatalf("summary does not report the redaction: %q", d.Summary)
	}
	assertNotInDB(t, r.f.dataDir, plantedSecret[:8], "AKIAIOSFODNN7EXAMPLE")
}

// assertNotInDB reads the database file and its WAL as raw bytes.
func assertNotInDB(t *testing.T, dataDir string, needles ...string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dataDir, "*.db*"))
	if len(matches) == 0 {
		t.Fatal("no database file found")
	}
	for _, p := range matches {
		b, err := os.ReadFile(p) //nolint:gosec // test.
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range needles {
			if strings.Contains(string(b), n) {
				t.Fatalf("%q found in %s", n, filepath.Base(p))
			}
		}
	}
}

func TestAgentRun_InvalidOutputFailsAndStoresNothing(t *testing.T) {
	cases := map[string]string{
		"free text":        "The project looks secure.",
		"schema violation": agentReport(func(doc map[string]any) { delete(doc, "coverage") }),
		"extra capability": agentReport(func(doc map[string]any) { doc["capabilities"] = []any{"net.egress"} }),
		"cites an unstaged file": agentReport(func(doc map[string]any) {
			doc["findings"].([]any)[0].(map[string]any)["evidence"].(map[string]any)["locations"] =
				[]any{map[string]any{"path": "../../.ssh/id_rsa"}}
		}),
		"examined something unstaged": agentReport(func(doc map[string]any) {
			doc["coverage"].(map[string]any)["examined"] = []any{"/etc/passwd"}
		}),
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			r := newAgentRig(t, newFakeAgent(out), "aod-agent-3")
			d := r.runToEnd(t)
			if d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrInvalid {
				t.Fatalf("state=%s code=%s msg=%s", d.State, d.ErrorCode, d.ErrorMessage)
			}
			if d.ReportSHA256 != "" || d.AgentReport != nil || len(d.Findings) != 0 {
				t.Fatal("an invalid report left something behind")
			}
		})
	}
}

// A hostile report cannot rename its own mode, claim a target, or change the
// project it is filed under.
func TestAgentRun_AgentCannotClaimAnotherModeOrATarget(t *testing.T) {
	out := agentReport(func(doc map[string]any) {
		run := doc["run"].(map[string]any)
		run["mode"] = "active-pentest"
		run["target"] = "prod.example.com:443"
		run["authorizationRef"] = "I authorize myself"
	})
	r := newAgentRig(t, newFakeAgent(out), "aod-agent-4")
	d := r.runToEnd(t)
	if d.State != store.SkillRunSucceeded || d.ModeID != "authz-review" {
		t.Fatalf("state=%s mode=%s", d.State, d.ModeID)
	}
	var rep map[string]any
	_ = json.Unmarshal(d.AgentReport, &rep)
	run := rep["run"].(map[string]any)
	if run["mode"] != "authz-review" || run["target"] != nil || run["authorizationRef"] != nil {
		t.Fatalf("agent claims survived: %v", run)
	}
}

func TestAgentRun_ExecutorErrorsAreClassified(t *testing.T) {
	cases := []struct {
		err   error
		state store.SkillRunState
		code  string
	}{
		{fmt.Errorf("%w: api/orders.go was modified", skillagent.ErrStagingTampered), store.SkillRunRefused, skills.RunErrAgentTampered},
		{fmt.Errorf("%w: api/orders.go changed", skillagent.ErrSourceChanged), store.SkillRunRefused, skills.RunErrAgentSourceChanged},
		{fmt.Errorf("%w: x", skillagent.ErrUnavailable), store.SkillRunRefused, skills.RunErrAgentUnavailable},
		{fmt.Errorf("%w: exit 1", skillagent.ErrProvider), store.SkillRunFailed, skills.RunErrAgentProviderFailed},
		{skillagent.ErrNoStructuredOutput, store.SkillRunFailed, skills.RunErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			a := newFakeAgent(agentReport(nil))
			a.err = tc.err
			r := newAgentRig(t, a, "aod-agent-5")
			d := r.runToEnd(t)
			if d.State != tc.state || d.ErrorCode != tc.code {
				t.Fatalf("state=%s code=%s, want %s %s", d.State, d.ErrorCode, tc.state, tc.code)
			}
			if d.AgentReport != nil {
				t.Fatal("a failed run carries a report")
			}
		})
	}
}

// A package that is not byte-for-byte the builtin -- here the builtin's files
// under a manifest that merely CLAIMS origin builtin -- never reaches the
// host agent, and no run is created.
func TestAgentRun_UntrustedPackageIsRefusedBeforeAcceptance(t *testing.T) {
	f := newFixture(t)
	dir := stagedPackage(t)
	manifest := filepath.Join(dir, skillcatalog.ManifestFileName)
	b, _ := os.ReadFile(manifest) //nolint:gosec // test.
	// A fork at a version the builtin never ships, whatever the builtin is at.
	body := regexp.MustCompile(`(?m)^version: .*$`).ReplaceAllString(string(b), "version: 90.0.0-fork")
	body = strings.Replace(body, "name: Security Audit", "name: Security Audit (fork)", 1)
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Install(context.Background(), skills.InstallRequest{SourceDir: dir, Actor: admin}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	project := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: project, SkillID: "security-audit", Version: "90.0.0-fork",
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatal(err)
	}
	agent := newFakeAgent(agentReport(nil))
	svc := skills.New(f.store, f.dataDir, skills.WithAgentExecutor(agent, f.store),
		skills.WithDurableRuns(f.store, "aod-agent-6", nil, nil))
	defer svc.CloseRuns(time.Second)
	_, _, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: project, SkillID: "security-audit", ModeID: "authz-review",
		Actor: admin, ActorPermissions: adminPerms(),
	}})
	if code := apiCode(t, err); code != "SKILL_AGENT_UNTRUSTED" {
		t.Fatalf("code = %s", code)
	}
	runs, _ := svc.ListRuns(context.Background(), project, 10)
	if len(runs) != 0 || len(agent.got) != 0 {
		t.Fatalf("runs=%d agentCalls=%d", len(runs), len(agent.got))
	}
	// And the dry run says so rather than calling it executable.
	dry, err := svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: project, SkillID: "security-audit", ModeID: "authz-review", ActorPermissions: adminPerms(),
	})
	if err != nil || dry.Verdict != skills.DryRunBlocked || !strings.Contains(strings.Join(dry.Reasons, " "), "SKILL_AGENT_UNTRUSTED") {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
}

func TestAgentRun_InsufficientCapabilityIsRefusedBeforeAcceptance(t *testing.T) {
	r := newAgentRig(t, newFakeAgent(agentReport(nil)), "aod-agent-7", skillcatalog.CapReportWrite)
	_, _, err := r.svc.StartRun(context.Background(), r.req())
	if code := apiCode(t, err); code != "SKILL_RUN_REFUSED" || !strings.Contains(err.Error(), "repo.read") {
		t.Fatalf("code=%s err=%v", code, err)
	}
}

func TestAgentRun_NoExecutorOrNoAttestationIsRefused(t *testing.T) {
	r := newAgentRig(t, nil, "aod-agent-8")
	_, _, err := r.svc.StartRun(context.Background(), r.req())
	if code := apiCode(t, err); code != "SKILL_AGENT_UNAVAILABLE" {
		t.Fatalf("code = %s", code)
	}
	a := newFakeAgent(agentReport(nil))
	a.att = skillcatalog.RunnerAttestation{RunnerID: skillagent.RunnerID}
	a.unavailable = "claude does not support --restricted"
	r2 := newAgentRig(t, a, "aod-agent-9")
	_, _, err = r2.svc.StartRun(context.Background(), r2.req())
	if code := apiCode(t, err); code != "SKILL_AGENT_UNAVAILABLE" || !strings.Contains(err.Error(), "--restricted") {
		t.Fatalf("code=%s err=%v", code, err)
	}
}

// A container attestation, however complete, never runs an agent mode, and a
// host agent never runs a tool mode: the executors are not interchangeable.
func TestAgentRun_ExecutorsAreNotInterchangeable(t *testing.T) {
	// Container fully attested, no agent: authz-review is refused.
	exec := &recordingExecutor{attestation: fullyAttested(), report: sampleReport()}
	f, _, auth, scope := newRunFixture(t, exec, true)
	svc := skills.New(f.store, f.dataDir, skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithDurableRuns(f.store, "aod-agent-10", nil, nil))
	defer svc.CloseRuns(time.Second)
	req := skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: "security-audit", ModeID: "authz-review",
		Actor: admin, ActorPermissions: adminPerms(),
	}}
	if _, _, err := svc.StartRun(context.Background(), req); apiCode(t, err) != "SKILL_AGENT_UNAVAILABLE" {
		t.Fatalf("err = %v", err)
	}

	// Host agent only: static-code (a tool mode) is refused -- the host
	// agent's controls do not stand in for the container's.
	agent := newFakeAgent(agentReport(nil))
	svc2 := skills.New(f.store, f.dataDir, skills.WithAgentExecutor(agent, f.store),
		skills.WithDurableRuns(f.store, "aod-agent-11", nil, nil))
	defer svc2.CloseRuns(time.Second)
	req.ModeID = "static-code"
	if _, _, err := svc2.StartRun(context.Background(), req); err == nil {
		t.Fatal("a host agent ran a tool mode")
	}
	if len(agent.got) != 0 {
		t.Fatal("the agent was invoked for a tool mode")
	}
}

// static-code keeps running through the container runner, unchanged, next to
// a wired agent executor.
func TestAgentRun_ToolModeIsUnchanged(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested(), report: sampleReport()}
	f, _, auth, scope := newRunFixture(t, exec, true)
	agent := newFakeAgent(agentReport(nil))
	svc := skills.New(f.store, f.dataDir, skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithAgentExecutor(agent, nil), skills.WithDurableRuns(f.store, "aod-agent-12", nil, nil))
	defer svc.CloseRuns(time.Second)
	run, _, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: runRequest(scope)})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, _ := svc.GetRun(context.Background(), scope.ProjectID, run.ID)
		if d.State.Terminal() {
			if d.State != store.SkillRunSucceeded || d.Tool != string(skillrunner.ToolStaticScan) || d.Report == nil {
				t.Fatalf("static-code run = %s %s", d.State, d.Tool)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("static-code run did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(agent.got) != 0 {
		t.Fatal("the agent was invoked for static-code")
	}
}

func TestAgentRun_CancelAndShutdown(t *testing.T) {
	a := newFakeAgent(agentReport(nil))
	a.block = true
	r := newAgentRig(t, a, "aod-agent-13")
	run, _, err := r.svc.StartRun(context.Background(), r.req())
	if err != nil {
		t.Fatal(err)
	}
	<-a.started
	// A double click while it runs returns the same run.
	again, created, err := r.svc.StartRun(context.Background(), r.req())
	if err != nil || created || again.ID != run.ID {
		t.Fatalf("second start: created=%v id=%s err=%v", created, again.ID, err)
	}
	if _, err := r.svc.CancelRun(context.Background(), r.project, run.ID); err != nil {
		t.Fatal(err)
	}
	d := r.wait(t, run.ID)
	if d.State != store.SkillRunCancelled {
		t.Fatalf("state = %s", d.State)
	}

	a2 := newFakeAgent(agentReport(nil))
	a2.block = true
	r2 := newAgentRig(t, a2, "aod-agent-14")
	run2, _, err := r2.svc.StartRun(context.Background(), r2.req())
	if err != nil {
		t.Fatal(err)
	}
	<-a2.started
	r2.svc.CloseRuns(5 * time.Second)
	d2, _ := r2.svc.GetRun(context.Background(), r2.project, run2.ID)
	if d2.State != store.SkillRunFailed || d2.ErrorCode != skills.RunErrShutdown {
		t.Fatalf("state=%s code=%s", d2.State, d2.ErrorCode)
	}
}

// A daemon that died mid-run: the next instance reaps the agent through the
// agent executor (not the container reaper) and ends the run interrupted.
func TestAgentRun_ReconcileReapsTheAgentOfADeadOwner(t *testing.T) {
	a := newFakeAgent(agentReport(nil))
	r := newAgentRig(t, a, "aod-new-owner")
	ctx := context.Background()
	rec, _, err := r.f.store.CreateSkillRun(ctx, store.SkillRunRecord{
		ID: "skr-0123456789abcdef01234567", ProjectID: r.project, SkillID: "security-audit",
		SkillVersion: r.version, ModeID: "authz-review", Tool: skillagent.Tool,
		OwnerInstance: "aod-dead-owner", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.store.MarkSkillRunRunning(ctx, rec.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	n, err := r.svc.ReconcileRuns(ctx)
	if err != nil || n != 1 {
		t.Fatalf("reconciled=%d err=%v", n, err)
	}
	if len(a.reaped) != 1 || a.reaped[0] != rec.ID {
		t.Fatalf("reaped = %v", a.reaped)
	}
	d, _ := r.svc.GetRun(ctx, r.project, rec.ID)
	if d.State != store.SkillRunFailed || d.ErrorCode != skills.RunErrInterrupted {
		t.Fatalf("state=%s code=%s", d.State, d.ErrorCode)
	}
}

func TestAgentRun_DryRunAnswersAgainstTheHostAgent(t *testing.T) {
	r := newAgentRig(t, newFakeAgent(agentReport(nil)), "aod-agent-15")
	dry, err := r.svc.DryRun(context.Background(), skills.DryRunRequest{
		ProjectID: r.project, SkillID: "security-audit", ModeID: "authz-review", ActorPermissions: adminPerms(),
		Inputs: map[string]string{"mode": "authz-review"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Verdict != skills.DryRunExecutable || dry.Runner.RunnerID != skillagent.RunnerID || !dry.Runner.Available {
		t.Fatalf("dry run = %+v", dry)
	}
	for _, dec := range dry.Decisions {
		if dec.Capability == skillcatalog.CapRepoRead &&
			(len(dec.RequiresControls) != 4 || dec.RequiresControls[0] != skillcatalog.ControlStagedReadOnlyCopy) {
			t.Fatalf("repo.read requires %v", dec.RequiresControls)
		}
	}
}
