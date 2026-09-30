package projectmemory_test

import (
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
)

func TestConfigFromEnvMemoryRoles(t *testing.T) {
	t.Setenv(projectmemory.RolesEnv, "worker")
	cfg, err := projectmemory.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Roles[projectmemory.RoleWorker] || cfg.Roles[projectmemory.RoleReviewer] {
		t.Fatalf("roles=%v", cfg.Roles)
	}
	t.Setenv(projectmemory.RolesEnv, "worker,nonsense")
	if _, err := projectmemory.ConfigFromEnv(); err == nil {
		t.Fatal("unknown role accepted")
	}
}

// A role left out of AO_MEMORY_ROLES is provisioned exactly as ModeOff.
func TestProvisionWithholdsMemoryFromUntargetedRoles(t *testing.T) {
	f := newFixture(t)
	svc := projectmemory.NewService(f.store)
	cfg := projectmemory.DefaultConfig()
	cfg.Mode = projectmemory.ModeAssisted
	cfg.Roles = map[projectmemory.PackRole]bool{projectmemory.RoleWorker: true}
	out := projectmemory.NewProvisioner(svc, cfg).Provision(f.ctx, projectmemory.ProvisionRequest{
		ProjectID: testProject, RepoPath: t.TempDir(), Role: projectmemory.RoleReviewer,
	})
	if out.Attached() || !strings.Contains(out.Metrics.FallbackReason, "not enabled for role reviewer") {
		t.Fatalf("reviewer received memory: attached=%v reason=%q", out.Attached(), out.Metrics.FallbackReason)
	}
}
