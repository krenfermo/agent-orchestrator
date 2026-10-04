package skillcatalog

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// writePackage lays down a minimal valid package with the given id/version and
// a digest computed from what it actually wrote, so tests exercise the real
// integrity path rather than a bypass.
func writePackage(t *testing.T, id, version string, mutate func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schemas"), 0o700); err != nil {
		t.Fatalf("mkdir schemas: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "modes"), 0o700); err != nil {
		t.Fatalf("mkdir modes: %v", err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("schemas/out.json", "{\"type\":\"object\"}\n")
	write("modes/quick.md", "# quick\n")

	body := strings.Replace(validManifestYAML, "id: example-audit", "id: "+id, 1)
	body = strings.Replace(body, "version: 1.2.3", "version: "+version, 1)
	if mutate != nil {
		body = mutate(body)
	}
	// Digest first, then substitute: the manifest is excluded from its own
	// digest precisely so this ordering works.
	write(ManifestFileName, body)
	digest, err := ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	body = strings.Replace(body,
		`"1111111111111111111111111111111111111111111111111111111111111111"`,
		`"`+digest+`"`, 1)
	write(ManifestFileName, body)
	return dir
}

func newRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(Dir(t.TempDir()))
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	return r
}

const admin = "admin@example.com"

func adminPerms() []domain.Permission {
	return []domain.Permission{
		domain.PermProjectRead, domain.PermProjectManage, domain.PermSettingsManage,
	}
}

func TestInstall_RecordsThePackageAndEnablesNothing(t *testing.T) {
	r := newRegistry(t)
	src := writePackage(t, "example-audit", "1.2.3", nil)

	entry, err := r.Install(src, admin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if entry.ID != "example-audit" || entry.Version != "1.2.3" {
		t.Fatalf("entry = %#v", entry)
	}
	if got := r.ListInstalled(); len(got) != 1 {
		t.Fatalf("installed = %#v", got)
	}
	// The whole point of the split: installing grants nothing anywhere.
	if got := r.ListActivations(); len(got) != 0 {
		t.Fatalf("install created activations: %#v", got)
	}
	if _, err := r.Resolve("proj-1", "example-audit"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("Resolve on a fresh install = %v, want ErrNotEnabled", err)
	}
	if _, err := os.Stat(filepath.Join(r.PackageDir("example-audit", "1.2.3"), ManifestFileName)); err != nil {
		t.Fatalf("package files were not copied: %v", err)
	}
}

func TestInstall_RejectsADuplicateVersionButAllowsANewOne(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("duplicate install = %v, want ErrAlreadyInstalled", err)
	}
	if _, err := r.Install(writePackage(t, "example-audit", "1.3.0", nil), admin); err != nil {
		t.Fatalf("second version: %v", err)
	}
	if got, ok := r.LatestVersion("example-audit"); !ok || got != "1.3.0" {
		t.Fatalf("LatestVersion = %q ok=%v", got, ok)
	}
}

// A package whose bytes do not match its declared digest is not installable.
// Without this, "integrity" would be a field nobody checks.
func TestInstall_RejectsATamperedPackage(t *testing.T) {
	r := newRegistry(t)
	src := writePackage(t, "example-audit", "1.2.3", nil)
	if err := os.WriteFile(filepath.Join(src, "modes", "quick.md"), []byte("# tampered\n"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, err := r.Install(src, admin)
	if !errors.Is(err, ErrInvalidManifest) || !strings.Contains(err.Error(), "does not match package contents") {
		t.Fatalf("err = %v, want an integrity failure", err)
	}
	if got := r.ListInstalled(); len(got) != 0 {
		t.Fatalf("a tampered package was recorded: %#v", got)
	}
}

// The manifest may only point at files that exist; a dangling reference would
// otherwise surface as a confusing runtime error long after install.
func TestInstall_RejectsAMissingReferencedFile(t *testing.T) {
	r := newRegistry(t)
	src := writePackage(t, "example-audit", "1.2.3", nil)
	if err := os.Remove(filepath.Join(src, "modes", "quick.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	// Recompute so the failure is the missing file, not the digest.
	digest, err := ComputePackageDigest(src)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(src, ManifestFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fixed := digestLine.ReplaceAllString(string(body), `  digest: "`+digest+`"`)
	if err := os.WriteFile(filepath.Join(src, ManifestFileName), []byte(fixed), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := r.Install(src, admin); err == nil || !strings.Contains(err.Error(), "is missing") {
		t.Fatalf("err = %v, want a missing-file rejection", err)
	}
}

func TestEnable_PinsAVersionAndRecordsTheGrant(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	act, err := r.Enable(EnableRequest{
		ProjectID:          "medusa",
		SkillID:            "example-audit",
		Version:            "1.2.3",
		GrantCapabilities:  []Capability{CapRepoRead, CapReportWrite},
		ApprovedBy:         admin,
		SubjectPermissions: adminPerms(),
	})
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !act.Enabled || act.Version != "1.2.3" || act.Grant.ApprovedBy != admin {
		t.Fatalf("activation = %#v", act)
	}
	resolved, err := r.Resolve("medusa", "example-audit")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Package.Manifest.ID != "example-audit" {
		t.Fatalf("resolved = %#v", resolved.Package.Manifest.ID)
	}
	// Another project sees nothing: activation is per project, not global.
	if _, err := r.Resolve("poseidon", "example-audit"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("Resolve on an unrelated project = %v, want ErrNotEnabled", err)
	}
}

func TestEnable_RefusesGrantsTheApproverCannotMake(t *testing.T) {
	r := newRegistry(t)
	src := writePackage(t, "example-audit", "1.2.3", func(body string) string {
		body = strings.Replace(body, "capabilities: [repo.read, report.write]",
			"capabilities: [repo.read, report.write, process.exec]", 1)
		return body
	})
	if _, err := r.Install(src, admin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// A member holding only project.read cannot grant process.exec, which is
	// gated on project.manage.
	_, err := r.Enable(EnableRequest{
		ProjectID:          "medusa",
		SkillID:            "example-audit",
		Version:            "1.2.3",
		GrantCapabilities:  []Capability{CapRepoRead, CapProcessExec},
		ApprovedBy:         "member@example.com",
		SubjectPermissions: []domain.Permission{domain.PermProjectRead},
	})
	if err == nil || !strings.Contains(err.Error(), "project.manage") {
		t.Fatalf("err = %v, want a missing-permission rejection", err)
	}
	if got := r.ListActivations(); len(got) != 0 {
		t.Fatalf("a refused enable left an activation: %#v", got)
	}
}

func TestEnable_Rejections(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	base := EnableRequest{
		ProjectID:          "medusa",
		SkillID:            "example-audit",
		Version:            "1.2.3",
		GrantCapabilities:  []Capability{CapRepoRead},
		ApprovedBy:         admin,
		SubjectPermissions: adminPerms(),
	}
	cases := []struct {
		name    string
		mutate  func(EnableRequest) EnableRequest
		wantSub string
	}{
		{"no project", func(e EnableRequest) EnableRequest { e.ProjectID = ""; return e }, "projectId is required"},
		{"no version", func(e EnableRequest) EnableRequest { e.Version = ""; return e }, "always pinned"},
		{"version not installed", func(e EnableRequest) EnableRequest { e.Version = "9.9.9"; return e }, "not installed"},
		{"unknown skill", func(e EnableRequest) EnableRequest { e.SkillID = "ghost"; return e }, "not installed"},
		{"no approver", func(e EnableRequest) EnableRequest { e.ApprovedBy = ""; return e }, "approvedBy is required"},
		{"undeclared capability", func(e EnableRequest) EnableRequest {
			e.GrantCapabilities = []Capability{CapNetEgress}
			return e
		}, "does not request it"},
		{"unknown capability", func(e EnableRequest) EnableRequest {
			e.GrantCapabilities = []Capability{"kernel.load"}
			return e
		}, "unknown capability"},
		{"duplicate grant", func(e EnableRequest) EnableRequest {
			e.GrantCapabilities = []Capability{CapRepoRead, CapRepoRead}
			return e
		}, "granted twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Enable(tc.mutate(base)); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want %q", err, tc.wantSub)
			}
		})
	}
}

// Re-enabling replaces the activation rather than accumulating a second one,
// so a project can never hold two different grants for the same skill.
func TestEnable_ReplacesAnExistingActivation(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Install(writePackage(t, "example-audit", "1.3.0", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	enable := func(version string, caps []Capability) {
		t.Helper()
		if _, err := r.Enable(EnableRequest{
			ProjectID: "medusa", SkillID: "example-audit", Version: version,
			GrantCapabilities: caps, ApprovedBy: admin, SubjectPermissions: adminPerms(),
		}); err != nil {
			t.Fatalf("Enable %s: %v", version, err)
		}
	}
	enable("1.2.3", []Capability{CapRepoRead, CapReportWrite})
	enable("1.3.0", []Capability{CapRepoRead})

	if got := r.ListActivations(); len(got) != 1 {
		t.Fatalf("activations = %#v", got)
	}
	resolved, err := r.Resolve("medusa", "example-audit")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Activation.Version != "1.3.0" {
		t.Fatalf("pinned version = %q", resolved.Activation.Version)
	}
	if len(resolved.Activation.Grant.Capabilities) != 1 {
		t.Fatalf("grant was not replaced: %#v", resolved.Activation.Grant)
	}
}

// Disabling drops the grant. Re-enabling therefore requires granting again,
// rather than silently reviving what was approved months earlier.
func TestDisable_RevokesTheGrant(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "example-audit", Version: "1.2.3",
		GrantCapabilities: []Capability{CapRepoRead, CapReportWrite},
		ApprovedBy:        admin, SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := r.Disable("medusa", "example-audit"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, err := r.Resolve("medusa", "example-audit"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("Resolve after disable = %v, want ErrNotEnabled", err)
	}
	acts := r.ListActivations()
	if len(acts) != 1 || acts[0].Enabled || len(acts[0].Grant.Capabilities) != 0 {
		t.Fatalf("disabled activation = %#v", acts)
	}
	if err := r.Disable("medusa", "ghost"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("Disable of an unknown skill = %v", err)
	}
}

func TestUninstall_RefusesWhileEnabledAndSucceedsAfterDisable(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "example-audit", Version: "1.2.3",
		GrantCapabilities: []Capability{CapRepoRead},
		ApprovedBy:        admin, SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := r.Uninstall("example-audit", "1.2.3"); !errors.Is(err, ErrInUse) {
		t.Fatalf("Uninstall while enabled = %v, want ErrInUse", err)
	}
	if err := r.Disable("medusa", "example-audit"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := r.Uninstall("example-audit", "1.2.3"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(r.PackageDir("example-audit", "1.2.3")); !os.IsNotExist(err) {
		t.Fatalf("package files survived uninstall: %v", err)
	}
	// The stale activation must be gone too, or a later re-enable would point
	// at files that no longer exist.
	if got := r.ListActivations(); len(got) != 0 {
		t.Fatalf("activations survived uninstall: %#v", got)
	}
	if err := r.Uninstall("example-audit", "1.2.3"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("second uninstall = %v", err)
	}
}

func TestRegistry_SurvivesAReopen(t *testing.T) {
	dir := Dir(t.TempDir())
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("OpenRegistry: %v", err)
	}
	if _, err := r.Install(writePackage(t, "example-audit", "1.2.3", nil), admin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := r.Enable(EnableRequest{
		ProjectID: "medusa", SkillID: "example-audit", Version: "1.2.3",
		GrantCapabilities: []Capability{CapRepoRead},
		ApprovedBy:        admin, SubjectPermissions: adminPerms(),
	}); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	resolved, err := reopened.Resolve("medusa", "example-audit")
	if err != nil {
		t.Fatalf("Resolve after reopen: %v", err)
	}
	if resolved.Activation.Grant.ApprovedBy != admin {
		t.Fatalf("grant did not survive: %#v", resolved.Activation.Grant)
	}
}

// The catalog must not live inside skillassets' directory: the daemon clobbers
// <dataDir>/skills/using-ao on every boot, and an install there would vanish.
func TestDir_DoesNotCollideWithTheBuiltInSkill(t *testing.T) {
	dataDir := "/tmp/ao-data"
	catalog := Dir(dataDir)
	builtin := filepath.Join(dataDir, "skills", "using-ao")
	if catalog == builtin || strings.HasPrefix(catalog, builtin+string(filepath.Separator)) {
		t.Fatalf("catalog %q lives inside the clobbered built-in skill dir %q", catalog, builtin)
	}
}

// digestLine matches the manifest's digest entry so a test can rewrite it
// after deliberately changing the package's contents.
var digestLine = regexp.MustCompile(`(?m)^  digest: ".*"$`)
