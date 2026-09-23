package skills_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
)

func TestEnsureBuiltins_InstallsWithoutEnablingAndIsIdempotent(t *testing.T) {
	f := newFixture(t)
	project := f.seedProject(t, "medusa")
	ctx := context.Background()

	first := f.svc.EnsureBuiltins(ctx)
	if len(first) != 1 || first[0].SkillID != "security-audit" || first[0].Action != "installed" {
		t.Fatalf("first boot: %+v", first)
	}
	rec, ok, err := f.store.GetSkillInstall(ctx, "security-audit", first[0].Version)
	if err != nil || !ok || rec.InstalledBy != skills.BuiltinActor {
		t.Fatalf("builtin not installed as %s: ok=%v err=%v rec=%+v", skills.BuiltinActor, ok, err, rec.InstalledBy)
	}
	// Available is not enabled: no project got anything.
	acts, err := f.store.ListSkillActivationsForProject(ctx, project)
	if err != nil || len(acts) != 0 {
		t.Fatalf("installing a builtin created %d activation(s) (err %v)", len(acts), err)
	}
	trail, _ := f.store.ListSkillAuditForSkill(ctx, "security-audit")

	second := f.svc.EnsureBuiltins(ctx)
	if len(second) != 1 || second[0].Action != "present" {
		t.Fatalf("second boot: %+v", second)
	}
	after, _ := f.store.ListSkillAuditForSkill(ctx, "security-audit")
	if len(after) != len(trail) {
		t.Fatalf("an idempotent boot wrote %d audit row(s)", len(after)-len(trail))
	}
}

func TestEnsureBuiltins_NeverOverwritesADifferentInstalledCopy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// The same version is already recorded with different bytes (a package
	// edited and re-pinned by hand). The manifest pins its own digest, so a
	// tampered copy cannot come through Install; the row is written directly.
	rec, err := f.svc.Install(ctx, skills.InstallRequest{SourceDir: stagedPackage(t), Actor: admin})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	rec.Digest = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := f.store.UpsertSkillInstall(ctx, rec); err != nil {
		t.Fatal(err)
	}
	before, _, _ := f.store.GetSkillInstall(ctx, "security-audit", "0.1.0")
	out := f.svc.EnsureBuiltins(ctx)
	if len(out) != 1 || out[0].Action != "conflict" {
		t.Fatalf("EnsureBuiltins over a different copy: %+v", out)
	}
	after, _, _ := f.store.GetSkillInstall(ctx, "security-audit", "0.1.0")
	if after.Digest != before.Digest {
		t.Fatal("the builtin overwrote an installed copy")
	}
}
