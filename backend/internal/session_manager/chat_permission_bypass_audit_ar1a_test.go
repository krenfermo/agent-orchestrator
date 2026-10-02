package sessionmanager

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AR-1a (D-SEC-4, Codex AR1A-05): Chat spawn and Chat resume reach a provider
// with the project's permission policy just like the terminal path, so an
// explicit bypass there is audited too.
func TestChatSpawnAndResumeAuditAnExplicitBypass(t *testing.T) {
	launcher := &recordingLauncher{}
	mgr, store, _ := newChatManager(launcher)
	var buf bytes.Buffer
	mgr.logger = slog.New(slog.NewTextHandler(&buf, nil))
	project := store.projects[string(chatTestProject)]
	project.Config.AgentConfig.Permissions = ports.PermissionModeBypassPermissions
	project.Config.Worker.AgentConfig.Permissions = ports.PermissionModeBypassPermissions
	store.projects[string(chatTestProject)] = project

	if _, _, _, err := mgr.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: chatTestProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		RequestedMode: domain.SessionModeChat,
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(buf.String(), "operation=chat-spawn") {
		t.Fatalf("chat spawn with an explicit bypass left no audit line: %s", buf.String())
	}

	buf.Reset()
	seedChatResumeSession(store, domain.ActivityExited)
	if _, err := mgr.ResumeAgentWithMode(context.Background(), "mer-1"); err != nil {
		t.Fatalf("ResumeAgentWithMode: %v", err)
	}
	if !strings.Contains(buf.String(), "audit=agent_permission_bypass") || !strings.Contains(buf.String(), "chat-resume") {
		t.Fatalf("chat resume with an explicit bypass left no audit line: %s", buf.String())
	}
}

// Agent switching builds the target's launch from the project's policy too, so
// an explicit bypass there is audited on the switch path (Codex AR1A-05).
func TestAgentSwitchAuditsAnExplicitBypass(t *testing.T) {
	runtime := &fakeRestartRuntime{fakeRuntime: &fakeRuntime{}}
	manager, store, _ := newSwitchTestManager(t, runtime)
	var buf bytes.Buffer
	manager.logger = slog.New(slog.NewTextHandler(&buf, nil))
	target := manager.agents.(switchTestAgents)[domain.HarnessCodex].(*switchTestAgent)
	rec := store.sessions["proj-1"]
	project := store.projects[string(rec.ProjectID)]
	project.Config.Worker.AgentConfig.Permissions = ports.PermissionModeBypassPermissions
	caps, err := validateContinuationAgent(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.prepareTargetActivation(context.Background(), store, rec, project, target, caps, domain.AgentSwitch{TargetHarness: domain.HarnessCodex}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "audit=agent_permission_bypass") || !strings.Contains(buf.String(), "operation=agent-switch") || !strings.Contains(buf.String(), "harness=codex") {
		t.Fatalf("agent switch with an explicit bypass left no audit line: %s", buf.String())
	}
}
