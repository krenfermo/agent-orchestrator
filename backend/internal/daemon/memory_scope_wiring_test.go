package daemon

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	durablememory "github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type countingProjects struct{ gets int }

func (c *countingProjects) GetProject(context.Context, string) (domain.ProjectRecord, bool, error) {
	c.gets++
	return domain.ProjectRecord{ID: "p", Path: "/somewhere/else"}, true, nil
}

func (c *countingProjects) ListWorkspaceRepos(context.Context, string) ([]domain.WorkspaceRepoRecord, error) {
	return nil, nil
}

// Frente 3 / 3B: the daemon's provisioner is always scoped to the project
// registry, so a mismatched (project, repository) pair fails closed.
func TestDaemonMemoryProvisionerIsProjectScoped(t *testing.T) {
	t.Setenv(durablememory.ModeEnv, "assisted")
	svc := durablememory.NewService(sqlitetest.MustOpen(t))
	projects := &countingProjects{}
	prov := memoryProvisioner(svc, projects, nil)
	if prov == nil {
		t.Fatal("assisted mode produced no provisioner")
	}
	out := prov.Provision(context.Background(), durablememory.ProvisionRequest{
		ProjectID: "p", RepoPath: t.TempDir(), Role: durablememory.RoleWorker,
	})
	if projects.gets == 0 {
		t.Fatal("the daemon's provisioner never consulted the project registry")
	}
	if out.Attached() || out.Freshness.Kind != "" {
		t.Fatalf("a repository that is not the project's was synced or served: %+v", out.Freshness)
	}
}

// And memory stays OFF by default: no env, no provisioner.
func TestDaemonMemoryIsOffByDefault(t *testing.T) {
	t.Setenv(durablememory.ModeEnv, "")
	if prov := memoryProvisioner(durablememory.NewService(sqlitetest.MustOpen(t)), &countingProjects{}, nil); prov != nil {
		t.Fatal("memory provisioner built with AO_MEMORY_MODE unset: memory must be off by default")
	}
}
