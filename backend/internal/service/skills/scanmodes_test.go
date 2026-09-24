package skills_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillrunner"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// scanmodes_test.go pins the 2D wiring: secret-scan and dependencies run
// through the SAME durable run and the SAME runner call as static-code, each
// with its own tool, the manifest's deny list, and its own image approval.

func TestScanModes_ReachTheRunnerWithTheirToolAndTheDenyList(t *testing.T) {
	for mode, tool := range map[string]skillrunner.Tool{
		"secret-scan":  skillrunner.ToolSecretScan,
		"dependencies": skillrunner.ToolDependencyScan,
	} {
		t.Run(mode, func(t *testing.T) {
			exec := &recordingExecutor{attestation: fullyAttested(), report: sampleReport()}
			f, _, auth, scope := newRunFixture(t, exec, false)
			if _, err := f.svc.Enable(context.Background(), skills.EnableRequest{
				ProjectID: scope.ProjectID, SkillID: "security-audit", Version: scope.Version,
				Capabilities: []skillcatalog.Capability{
					skillcatalog.CapRepoRead, skillcatalog.CapReportWrite, skillcatalog.CapDepsRead,
				},
				Actor: admin, ActorPermissions: adminPerms(),
			}); err != nil {
				t.Fatal(err)
			}
			scope.ModeID = mode
			req := approveRequest(scope, digestOf('b'))
			req.Tool = string(tool)
			if _, err := auth.Approve(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			svc := skills.New(f.store, f.dataDir, skills.WithSkillExecutor(exec, auth, f.store, "", ""),
				skills.WithDurableRuns(f.store, "aod-2d", nil, nil))
			defer svc.CloseRuns(time.Second)
			run, _, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
				ProjectID: scope.ProjectID, SkillID: "security-audit", ModeID: mode,
				Actor: admin, ActorPermissions: adminPerms(),
			}})
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}
			d := waitRun(t, svc, scope.ProjectID, run.ID)
			if d.State != store.SkillRunSucceeded || d.Tool != string(tool) {
				t.Fatalf("state=%s tool=%s (%s %s)", d.State, d.Tool, d.ErrorCode, d.ErrorMessage)
			}
			got := exec.seen()
			if len(got) != 1 || got[0].Tool != tool {
				t.Fatalf("runner got %+v", got)
			}
			if strings.Join(got[0].DenyGlobs, ",") != ".env,.env.*,**/*.pem,**/*.key,**/id_rsa*" {
				t.Fatalf("deny list = %v", got[0].DenyGlobs)
			}
			if got[0].Params != skillrunner.DefaultParamsFor(tool) {
				t.Fatalf("params = %+v, want the tool's own defaults", got[0].Params)
			}
		})
	}
}

// An approval is per mode AND per tool: the static-code image approval does
// not let the secret scan run.
func TestScanModes_AStaticCodeApprovalDoesNotAuthorizeTheSecretScan(t *testing.T) {
	exec := &recordingExecutor{attestation: fullyAttested(), report: sampleReport()}
	f, _, auth, scope := newRunFixture(t, exec, true) // approves static-code only
	svc := skills.New(f.store, f.dataDir, skills.WithSkillExecutor(exec, auth, f.store, "", ""),
		skills.WithDurableRuns(f.store, "aod-2d-2", nil, nil))
	defer svc.CloseRuns(time.Second)
	run, _, err := svc.StartRun(context.Background(), skills.StartRunRequest{RunRequest: skills.RunRequest{
		ProjectID: scope.ProjectID, SkillID: "security-audit", ModeID: "secret-scan",
		Actor: admin, ActorPermissions: adminPerms(),
	}})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	d := waitRun(t, svc, scope.ProjectID, run.ID)
	if d.State != store.SkillRunRefused || d.ErrorCode != "SKILL_IMAGE_NOT_APPROVED" {
		t.Fatalf("state=%s code=%s", d.State, d.ErrorCode)
	}
}

func waitRun(t *testing.T, svc *skills.Service, project domain.ProjectID, id string) skills.SkillRunDetail {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, err := svc.GetRun(context.Background(), project, id)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if d.State.Terminal() {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %s", id, d.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
