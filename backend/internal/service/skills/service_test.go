package skills_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/skills"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillassets"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const (
	admin       = "admin@example.com"
	securitySrc = "../../skillcatalog/packages/security-audit"
)

func adminPerms() []domain.Permission {
	return []domain.Permission{
		domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage,
	}
}

func readerPerms() []domain.Permission {
	return []domain.Permission{domain.PermProjectRead}
}

// fixture is one service over a real migrated SQLite store and a real data dir.
type fixture struct {
	svc     *skills.Service
	store   *sqlite.Store
	dataDir string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dataDir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dataDir)
	return fixture{svc: skills.New(st, dataDir), store: st, dataDir: dataDir}
}

func (f fixture) seedProject(t *testing.T, id string) domain.ProjectID {
	t.Helper()
	if err := f.store.UpsertProject(context.Background(), domain.ProjectRecord{
		ID: id, Path: filepath.Join("/tmp", id), RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}
	return domain.ProjectID(id)
}

// stagedPackage copies the shipped security-audit package into a temp dir so a
// test can tamper with it without touching the repository copy.
func stagedPackage(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pkg")
	if err := skillcatalog.CopyPackage(securitySrc, dir); err != nil {
		t.Fatalf("stage package: %v", err)
	}
	return dir
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *apierr.Error", err)
	}
	return apiErr.Code
}

func mustInstall(t *testing.T, f fixture) string {
	t.Helper()
	rec, err := f.svc.Install(context.Background(), skills.InstallRequest{
		SourceDir: stagedPackage(t), Actor: admin,
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return rec.Manifest.Version
}

func TestInstall_PersistsAndEnablesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	project := f.seedProject(t, "medusa")

	rec, err := f.svc.Install(ctx, skills.InstallRequest{SourceDir: stagedPackage(t), Actor: admin})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rec.Manifest.ID != "security-audit" || rec.Digest == "" {
		t.Fatalf("record = %#v", rec)
	}
	if _, err := os.Stat(filepath.Join(rec.PackageDir, "skill.yaml")); err != nil {
		t.Fatalf("package files were not copied: %v", err)
	}

	installed, err := f.svc.ListInstalled(ctx)
	if err != nil || len(installed) != 1 {
		t.Fatalf("ListInstalled = %#v, %v", installed, err)
	}
	// Installing grants nothing anywhere: the whole point of the split.
	acts, err := f.svc.ListForProject(ctx, project)
	if err != nil || len(acts) != 0 {
		t.Fatalf("install created activations: %#v, %v", acts, err)
	}
	if _, err := f.svc.Resolve(ctx, project, "security-audit"); err == nil {
		t.Fatal("a freshly installed skill resolved for a project that never enabled it")
	} else if code := apiCode(t, err); code != "SKILL_NOT_ENABLED" {
		t.Fatalf("code = %q", code)
	}
}

func TestInstall_RejectsBadSources(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cases := []struct {
		name     string
		source   func(t *testing.T) string
		wantCode string
	}{
		{"empty", func(*testing.T) string { return "  " }, "SKILL_SOURCE_REQUIRED"},
		{"relative", func(*testing.T) string { return "packages/security-audit" }, "SKILL_SOURCE_NOT_ABSOLUTE"},
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope") }, "SKILL_PACKAGE_NOT_FOUND"},
		{"inside the catalog", func(*testing.T) string {
			return filepath.Join(skillcatalog.Dir(f.dataDir), "packages", "x")
		}, "SKILL_SOURCE_INSIDE_CATALOG"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Install(ctx, skills.InstallRequest{SourceDir: tc.source(t), Actor: admin})
			if err == nil {
				t.Fatal("install was accepted")
			}
			if code := apiCode(t, err); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// A package whose bytes do not match its declared digest must not install, and
// the rejection must be recorded: "somebody tried to install something that
// failed verification" is the trail entry an operator most wants to find.
func TestInstall_RejectsATamperedPackageAndAuditsTheRejection(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	src := stagedPackage(t)
	if err := os.WriteFile(filepath.Join(src, "modes", "static-code.md"), []byte("# tampered\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	_, err := f.svc.Install(ctx, skills.InstallRequest{SourceDir: src, Actor: admin})
	if err == nil {
		t.Fatal("a tampered package installed")
	}
	if code := apiCode(t, err); code != "SKILL_MANIFEST_INVALID" {
		t.Fatalf("code = %q", code)
	}
	installed, err := f.svc.ListInstalled(ctx)
	if err != nil || len(installed) != 0 {
		t.Fatalf("a tampered package was recorded: %#v, %v", installed, err)
	}
	entries, err := f.svc.AuditForSkill(ctx, "")
	if err != nil {
		t.Fatalf("AuditForSkill: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != "install_rejected" {
		t.Fatalf("rejection was not audited: %#v", entries)
	}
	if !strings.Contains(entries[0].Detail, "does not match package contents") {
		t.Fatalf("audit detail lost the reason: %q", entries[0].Detail)
	}
}

// Re-installing the same bytes is idempotent. Re-installing DIFFERENT bytes
// under a version somebody already approved is refused: an activation pins a
// version, and it would silently start meaning something else.
func TestInstall_RefusesToRepublishAVersionWithDifferentContents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mustInstall(t, f)

	if _, err := f.svc.Install(ctx, skills.InstallRequest{SourceDir: stagedPackage(t), Actor: admin}); err != nil {
		t.Fatalf("re-installing identical bytes should be idempotent: %v", err)
	}

	altered := stagedPackage(t)
	guide := filepath.Join(altered, "modes", "static-code.md")
	body, err := os.ReadFile(guide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(guide, append(body, []byte("\nextra\n")...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Refresh the digest so the package is internally valid but different.
	refreshDigest(t, altered)

	_, err = f.svc.Install(ctx, skills.InstallRequest{SourceDir: altered, Actor: admin})
	if err == nil {
		t.Fatal("a changed package republished over an installed version")
	}
	if code := apiCode(t, err); code != "SKILL_VERSION_CONTENT_CHANGED" {
		t.Fatalf("code = %q", code)
	}
}

var digestLine = regexp.MustCompile(`(?m)^  digest: ".*"$`)

func refreshDigest(t *testing.T, dir string) {
	t.Helper()
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	manifest := filepath.Join(dir, skillcatalog.ManifestFileName)
	body, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	fixed := digestLine.ReplaceAllString(string(body), `  digest: "`+digest+`"`)
	if err := os.WriteFile(manifest, []byte(fixed), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func TestEnable_PinsAVersionRecordsTheApproverAndIsolatesProjects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	poseidon := f.seedProject(t, "poseidon")
	f.seedProject(t, "crm")

	act, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{
			skillcatalog.CapRepoRead, skillcatalog.CapReportWrite, skillcatalog.CapNetEgress,
		},
		Actor: admin, ActorPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("Enable medusa: %v", err)
	}
	if !act.Enabled || act.Version != version || act.Grant.ApprovedBy != admin || act.Grant.ApprovedAt.IsZero() {
		t.Fatalf("activation = %#v", act)
	}

	if _, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: poseidon, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable poseidon: %v", err)
	}

	// One install, three projects, three different answers.
	medusaResolved, err := f.svc.Resolve(ctx, medusa, "security-audit")
	if err != nil {
		t.Fatalf("Resolve medusa: %v", err)
	}
	if len(medusaResolved.Activation.Grant.Capabilities) != 3 {
		t.Fatalf("medusa grant = %#v", medusaResolved.Activation.Grant)
	}
	poseidonResolved, err := f.svc.Resolve(ctx, poseidon, "security-audit")
	if err != nil {
		t.Fatalf("Resolve poseidon: %v", err)
	}
	if len(poseidonResolved.Activation.Grant.Capabilities) != 2 {
		t.Fatalf("poseidon grant = %#v", poseidonResolved.Activation.Grant)
	}
	if _, err := f.svc.Resolve(ctx, "crm", "security-audit"); err == nil {
		t.Fatal("crm resolved a skill nobody enabled there")
	}
}

// A grant can never be broader than the person making it. This is the check
// the route middleware cannot make: it knows the caller may manage the
// project, not which capabilities that authorises.
func TestEnable_RefusesAGrantWiderThanTheApprover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")

	_, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		// net.active_scan is gated on settings.manage, which a reader lacks.
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapNetActiveScan},
		Actor:        "reader@example.com", ActorPermissions: readerPerms(),
	})
	if err == nil {
		t.Fatal("a reader granted an active-scan capability")
	}
	if code := apiCode(t, err); code != "SKILL_GRANT_REFUSED" {
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(err.Error(), "settings.manage") {
		t.Fatalf("error should name the missing permission: %v", err)
	}
	acts, err := f.svc.ListForProject(ctx, medusa)
	if err != nil || len(acts) != 0 {
		t.Fatalf("a refused enable left an activation: %#v, %v", acts, err)
	}
}

func TestEnable_Rejections(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	base := skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead},
		Actor:        admin, ActorPermissions: adminPerms(),
	}
	cases := []struct {
		name     string
		mutate   func(skills.EnableRequest) skills.EnableRequest
		wantCode string
	}{
		{"no project", func(e skills.EnableRequest) skills.EnableRequest { e.ProjectID = ""; return e }, "PROJECT_REQUIRED"},
		{"no version", func(e skills.EnableRequest) skills.EnableRequest { e.Version = ""; return e }, "SKILL_VERSION_REQUIRED"},
		{"no approver", func(e skills.EnableRequest) skills.EnableRequest { e.Actor = ""; return e }, "SKILL_APPROVER_REQUIRED"},
		{"version not installed", func(e skills.EnableRequest) skills.EnableRequest { e.Version = "9.9.9"; return e }, "SKILL_NOT_INSTALLED"},
		{"unknown skill", func(e skills.EnableRequest) skills.EnableRequest { e.SkillID = "ghost"; return e }, "SKILL_NOT_INSTALLED"},
		{"undeclared capability", func(e skills.EnableRequest) skills.EnableRequest {
			e.Capabilities = []skillcatalog.Capability{skillcatalog.CapProcessExec}
			return e
		}, "SKILL_GRANT_REFUSED"},
		{"unknown capability", func(e skills.EnableRequest) skills.EnableRequest {
			e.Capabilities = []skillcatalog.Capability{"kernel.load"}
			return e
		}, "SKILL_MANIFEST_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Enable(ctx, tc.mutate(base))
			if err == nil {
				t.Fatal("enable was accepted")
			}
			if code := apiCode(t, err); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// Editing a package underneath AO must break resolution, not be resolved from
// a stale row. The digest recorded at install is the anchor.
func TestResolve_FailsClosedOnAnAlteredPackage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	rec, err := f.svc.GetInstalled(ctx, "security-audit", version)
	if err != nil {
		t.Fatalf("GetInstalled: %v", err)
	}
	guide := filepath.Join(rec.PackageDir, "modes", "active-pentest.md")
	if err := os.WriteFile(guide, []byte("# anything goes\n"), 0o600); err != nil {
		t.Fatalf("tamper installed copy: %v", err)
	}

	if _, err := f.svc.Resolve(ctx, medusa, "security-audit"); err == nil {
		t.Fatal("an altered package resolved")
	} else if code := apiCode(t, err); code != "SKILL_MANIFEST_INVALID" {
		t.Fatalf("code = %q", code)
	}
}

func TestDisable_RevokesTheGrantAndKeepsTheHistory(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := f.svc.Disable(ctx, medusa, "security-audit", admin); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if _, err := f.svc.Resolve(ctx, medusa, "security-audit"); err == nil {
		t.Fatal("a disabled skill resolved")
	} else if code := apiCode(t, err); code != "SKILL_DISABLED" {
		t.Fatalf("code = %q", code)
	}
	acts, err := f.svc.ListForProject(ctx, medusa)
	if err != nil || len(acts) != 1 {
		t.Fatalf("activations = %#v, %v", acts, err)
	}
	if acts[0].Enabled || len(acts[0].Grant.Capabilities) != 0 {
		t.Fatalf("disable did not revoke the grant: %#v", acts[0])
	}
	if err := f.svc.Disable(ctx, medusa, "ghost", admin); err == nil {
		t.Fatal("disabling an unknown skill succeeded")
	}
}

func TestUninstall_RefusesWhileEnabledThenSucceeds(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")
	if _, err := f.svc.Enable(ctx, skills.EnableRequest{
		ProjectID: medusa, SkillID: "security-audit", Version: version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	rec, err := f.svc.GetInstalled(ctx, "security-audit", version)
	if err != nil {
		t.Fatalf("GetInstalled: %v", err)
	}

	err = f.svc.Uninstall(ctx, "security-audit", version, admin)
	if err == nil {
		t.Fatal("uninstalled a version a project still had enabled")
	}
	if code := apiCode(t, err); code != "SKILL_STILL_ENABLED" {
		t.Fatalf("code = %q", code)
	}
	if !strings.Contains(err.Error(), "medusa") {
		t.Fatalf("error should name the project: %v", err)
	}

	if err := f.svc.Disable(ctx, medusa, "security-audit", admin); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := f.svc.Uninstall(ctx, "security-audit", version, admin); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(rec.PackageDir); !os.IsNotExist(err) {
		t.Fatalf("package files survived uninstall: %v", err)
	}
	// The stale activation row must go too, or a re-enable would point at
	// files that no longer exist.
	acts, err := f.svc.ListForProject(ctx, medusa)
	if err != nil || len(acts) != 0 {
		t.Fatalf("activations survived uninstall: %#v, %v", acts, err)
	}
	if err := f.svc.Uninstall(ctx, "security-audit", version, admin); err == nil {
		t.Fatal("second uninstall succeeded")
	}
}

// The catalog must survive a restart with its grants intact -- the whole
// reason phase 2 moved off a JSON file.
func TestCatalog_SurvivesAReopen(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()

	first := sqlitetest.MustOpenAt(t, dataDir)
	svc := skills.New(first, dataDir)
	if err := first.UpsertProject(ctx, domain.ProjectRecord{
		ID: "medusa", Path: "/tmp/medusa", RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	rec, err := svc.Install(ctx, skills.InstallRequest{SourceDir: stagedPackage(t), Actor: admin})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := svc.Enable(ctx, skills.EnableRequest{
		ProjectID: "medusa", SkillID: "security-audit", Version: rec.Manifest.Version,
		Capabilities: []skillcatalog.Capability{skillcatalog.CapRepoRead, skillcatalog.CapReportWrite},
		Actor:        admin, ActorPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A genuine reopen of the same file on disk. sqlitetest.MustOpenAt clones a
	// fresh migrated template and refuses to replace an existing database, so
	// the second open has to go through the real sqlite.Open the daemon uses.
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc2 := skills.New(reopened, dataDir)
	resolved, err := svc2.Resolve(ctx, "medusa", "security-audit")
	if err != nil {
		t.Fatalf("Resolve after reopen: %v", err)
	}
	if resolved.Activation.Grant.ApprovedBy != admin || len(resolved.Activation.Grant.Capabilities) != 2 {
		t.Fatalf("grant did not survive: %#v", resolved.Activation.Grant)
	}
	entries, err := svc2.AuditForProject(ctx, "medusa")
	if err != nil || len(entries) != 1 || entries[0].Action != "enable" {
		t.Fatalf("audit did not survive: %#v, %v", entries, err)
	}
}

// The daemon clobbers <dataDir>/skills/using-ao on every boot. An installed
// package must not be inside that path, or a restart would delete it. This is
// the regression test for that specific collision.
func TestDaemonBootDoesNotOverwriteInstalledPackages(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	rec, err := f.svc.GetInstalled(ctx, "security-audit", version)
	if err != nil {
		t.Fatalf("GetInstalled: %v", err)
	}
	before, err := skillcatalog.ComputePackageDigest(rec.PackageDir)
	if err != nil {
		t.Fatalf("digest before: %v", err)
	}

	// Exactly what daemon boot does.
	if err := skillassets.Install(f.dataDir); err != nil {
		t.Fatalf("skillassets.Install: %v", err)
	}

	after, err := skillcatalog.ComputePackageDigest(rec.PackageDir)
	if err != nil {
		t.Fatalf("the boot hook deleted an installed package: %v", err)
	}
	if before != after {
		t.Fatalf("the boot hook rewrote an installed package: %s -> %s", before, after)
	}
	if strings.HasPrefix(rec.PackageDir, skillassets.Dir(f.dataDir)) {
		t.Fatalf("the catalog installs inside the clobbered built-in skill dir: %s", rec.PackageDir)
	}
	if _, err := f.svc.Resolve(ctx, f.seedProject(t, "medusa"), "security-audit"); err == nil {
		t.Fatal("resolve should still report not-enabled, not succeed")
	}
}

// The audit trail is the answer to "who installed this, who enabled it, and
// who widened its grant".
func TestAudit_RecordsInstallEnableGrantChangeAndDisable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	medusa := f.seedProject(t, "medusa")

	enable := func(actor string, caps []skillcatalog.Capability) {
		t.Helper()
		if _, err := f.svc.Enable(ctx, skills.EnableRequest{
			ProjectID: medusa, SkillID: "security-audit", Version: version,
			Capabilities: caps, Actor: actor, ActorPermissions: adminPerms(),
		}); err != nil {
			t.Fatalf("Enable: %v", err)
		}
	}
	enable(admin, []skillcatalog.Capability{skillcatalog.CapRepoRead})
	enable("second@example.com", []skillcatalog.Capability{
		skillcatalog.CapRepoRead, skillcatalog.CapReportWrite, skillcatalog.CapNetEgress,
	})
	if err := f.svc.Disable(ctx, medusa, "security-audit", admin); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	entries, err := f.svc.AuditForSkill(ctx, "security-audit")
	if err != nil {
		t.Fatalf("AuditForSkill: %v", err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, string(e.Action))
	}
	// Newest first.
	want := []string{"disable", "grant_changed", "enable", "install"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	for _, e := range entries {
		if e.Action == "grant_changed" {
			if e.Actor != "second@example.com" || len(e.Capabilities) != 3 {
				t.Fatalf("grant_changed entry = %#v", e)
			}
		}
	}
	// The install entry belongs to no project; the activation entries do.
	for _, e := range entries {
		switch e.Action {
		case "install":
			if e.ProjectID != nil {
				t.Fatalf("install was attributed to a project: %#v", e)
			}
		case "enable", "disable", "grant_changed":
			if e.ProjectID == nil || *e.ProjectID != medusa {
				t.Fatalf("%s was not attributed to medusa: %#v", e.Action, e)
			}
		}
	}
}

// The stored manifest must round-trip, so a list view can be served without
// reading the package from disk.
func TestInstall_StoresTheManifestVerbatim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	version := mustInstall(t, f)
	rec, err := f.svc.GetInstalled(ctx, "security-audit", version)
	if err != nil {
		t.Fatalf("GetInstalled: %v", err)
	}
	onDisk, err := skillcatalog.LoadPackage(rec.PackageDir)
	if err != nil {
		t.Fatalf("LoadPackage: %v", err)
	}
	stored, err := json.Marshal(rec.Manifest)
	if err != nil {
		t.Fatalf("marshal stored: %v", err)
	}
	fresh, err := json.Marshal(onDisk.Manifest)
	if err != nil {
		t.Fatalf("marshal fresh: %v", err)
	}
	if string(stored) != string(fresh) {
		t.Fatalf("stored manifest drifted from the package on disk")
	}
}
