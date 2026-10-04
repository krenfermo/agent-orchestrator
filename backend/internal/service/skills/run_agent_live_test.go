package skills_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillagent"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillreport"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// run_agent_live_test.go — Frente 2 / 2G, ETAPA 12: the one system path the
// deterministic suite cannot cover, an authz-review run against a REAL Claude
// agent on a synthetic project.
//
// It proves the agent half of Skills end to end: a real reviewer reads a
// read-only staged copy, emits AO's findings.v1 contract, and the service
// validates → redacts → validates → persists it with a verified SHA. The
// synthetic checkout carries a ghp_ token canary; whether or not the reviewer
// quotes it, it must never reach storage in the clear.
//
// It costs real API budget, so it is bounded (short timeout, small max budget)
// and skips — never fails — when no Claude agent is available on the host.
func TestLiveAuthzReview_RealAgentProducesAVerifiedRedactedReport(t *testing.T) {
	if testing.Short() {
		t.Skip("real-agent tests are skipped under -short")
	}

	f := newFixture(t)
	version := mustInstall(t, f)
	project := liveProject(t) // synthetic checkout with a ghp_ token in api/config.go
	const canary = "ghp_0123456789abcdefghijklmnopqrstuvwxyzA"

	id := domain.ProjectID("medusa-authz-live")
	if err := f.store.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: string(id), Path: project, RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
		ProjectID: id, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// The REAL host agent executor, bounded so a live run cannot run away.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	agent := skillagent.New(ctx, skillagent.Config{
		StagingRoot:     t.TempDir() + "/.ao-skill-staging",
		ResolveFallback: claudecode.ResolveClaudeBinary,
		Timeout:         4 * time.Minute,
		MaxBudgetUSD:    2.0,
	})
	cancel()
	if reason := agent.Unavailable(); reason != "" {
		t.Skipf("no usable Claude agent on this host: %s", reason)
	}

	svc := skills.New(f.store, f.dataDir,
		skills.WithAgentExecutor(agent, f.store),
		skills.WithDurableRuns(f.store, "ao2g-authz-live", &recordingReaper{}, nil))
	t.Cleanup(func() { svc.CloseRuns(10 * time.Second) })

	run, created, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: id, SkillID: "security-audit", ModeID: "authz-review",
		Actor: admin, ActorPermissions: adminPerms(),
	}})
	if err != nil {
		t.Fatalf("StartRun(authz-review): %v", err)
	}
	if !created {
		t.Fatal("StartRun did not create a run")
	}

	d := waitTerminalLong(t, svc, id, run.ID, 5*time.Minute)

	// A runtime that turned out to be unusable mid-run is an environment skip,
	// not a product failure.
	if d.State == store.SkillRunFailed &&
		(strings.Contains(d.ErrorCode, "RUNTIME") || strings.Contains(d.ErrorCode, "UNAVAILABLE")) {
		t.Skipf("agent runtime became unavailable during the run: %s / %s", d.ErrorCode, d.ErrorMessage)
	}
	if d.State != store.SkillRunSucceeded && d.State != store.SkillRunPartial {
		t.Fatalf("authz-review ended %q (%s: %s)", d.State, d.ErrorCode, d.ErrorMessage)
	}

	// A report was stored, it satisfies AO's findings.v1 contract, and its SHA
	// verifies over the stored bytes.
	if len(d.AgentReport) == 0 {
		t.Fatal("no agent report stored")
	}
	schema, err := skillreport.ParseSchema(skillcatalog.CanonicalFindingsSchema())
	if err != nil {
		t.Fatalf("parse findings schema: %v", err)
	}
	if err := schema.Validate(d.AgentReport); err != nil {
		t.Fatalf("stored report is not findings.v1: %v\n%s", err, d.AgentReport)
	}
	if d.Integrity != "verified" {
		t.Fatalf("integrity = %q, want verified", d.Integrity)
	}

	// The canary never reaches storage in the clear — neither the whole value
	// nor a substantial prefix of it (a leak is a leak even truncated). A bare
	// "ghp_" descriptor a reviewer writes to NAME the finding is not a secret,
	// so the property is about the value, not the prefix word.
	raw := string(d.AgentReport)
	if i := strings.Index(raw, "ghp_"); i >= 0 {
		end := i + 48
		if end > len(raw) {
			end = len(raw)
		}
		t.Logf("ghp_ context in stored report: %q", raw[i:end])
	}
	for _, n := range []int{len(canary), 24, 16} {
		if strings.Contains(raw, canary[:n]) {
			t.Fatalf("a %d-char prefix of the ghp_ canary survived into the stored report in the clear", n)
		}
	}
	for _, fr := range d.Findings {
		blob := fr.Title + " " + fr.Path + " " + fr.Recommendation
		if strings.Contains(blob, canary[:16]) {
			t.Fatalf("the ghp_ canary survived into a finding row: %+v", fr)
		}
	}

	// The audit trail records that a run executed.
	entries, err := f.store.ListSkillAuditForProject(context.Background(), id)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	var executed bool
	for _, e := range entries {
		if string(e.Action) == "run_executed" {
			executed = true
		}
	}
	if !executed {
		t.Fatalf("no run_executed entry among %d audit rows", len(entries))
	}
	t.Logf("authz-review live: state=%s findings=%d integrity=%s", d.State, len(d.Findings), d.Integrity)
}

// waitTerminalLong polls until a durable run reaches a terminal state, with a
// deadline long enough for a real agent turn.
func waitTerminalLong(t *testing.T, svc *skills.Service, project domain.ProjectID, runID string, budget time.Duration) skills.SkillRunDetail {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		d, err := svc.GetRun(context.Background(), project, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.State.Terminal() {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state within %s; last %q", runID, budget, d.State)
		}
		time.Sleep(2 * time.Second)
	}
}
