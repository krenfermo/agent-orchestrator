package skillregistry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

func ctx() context.Context { return context.Background() }

func TestFileProvider_SearchReadsTheIndexAndMovesNoBytes(t *testing.T) {
	root := buildRegistry(t,
		releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}},
		releaseSpec{pkg: pkgSpec{id: "dependency-check", version: "2.0.0"}},
	)
	p := provider(t, root)

	got, err := p.Search(ctx(), Query{Text: "security"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].SkillID != "security-audit" {
		t.Fatalf("Search(security) = %#v", got)
	}
	// The registry id is AO's, not the file's.
	if got[0].RegistryID != "fixture" {
		t.Fatalf("registryId = %q, want fixture", got[0].RegistryID)
	}
	all, err := p.Search(ctx(), Query{})
	if err != nil || len(all) != 2 {
		t.Fatalf("Search(all) = %d releases, err=%v", len(all), err)
	}
}

func TestFileProvider_SearchIsDeterministic(t *testing.T) {
	root := buildRegistry(t,
		releaseSpec{pkg: pkgSpec{id: "b-skill", version: "1.0.0"}},
		releaseSpec{pkg: pkgSpec{id: "a-skill", version: "1.0.0"}},
		releaseSpec{pkg: pkgSpec{id: "a-skill", version: "1.4.0"}},
	)
	p := provider(t, root)
	var first []string
	for i := 0; i < 5; i++ {
		got, err := p.Search(ctx(), Query{})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		var refs []string
		for _, r := range got {
			refs = append(refs, r.Ref())
		}
		if i == 0 {
			first = refs
			continue
		}
		if strings.Join(refs, ",") != strings.Join(first, ",") {
			t.Fatalf("search %d = %v, first = %v", i, refs, first)
		}
	}
	if first[0] != "a-skill@1.4.0" {
		t.Fatalf("order = %v, want newest first within a skill", first)
	}
}

func TestFileProvider_SearchFilters(t *testing.T) {
	root := buildRegistry(t,
		releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0", publisher: "acme"}},
		releaseSpec{pkg: pkgSpec{id: "old-skill", version: "0.1.0"}, mutateRelease: func(r *Release) {
			r.Deprecated, r.DeprecationNote = true, "use security-audit"
		}},
		releaseSpec{pkg: pkgSpec{id: "bad-skill", version: "0.1.0"}, mutateRelease: func(r *Release) {
			r.Revoked, r.RevocationReason = true, "malicious build"
		}},
	)
	p := provider(t, root)

	// The ordinary listing is the INSTALLABLE one.
	plain, err := p.Search(ctx(), Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(plain) != 1 || plain[0].SkillID != "security-audit" {
		t.Fatalf("default search returned %#v; deprecated and revoked must be hidden", plain)
	}
	withRevoked, err := p.Search(ctx(), Query{IncludeRevoked: true, IncludeDeprecated: true})
	if err != nil || len(withRevoked) != 3 {
		t.Fatalf("widened search = %d, err=%v", len(withRevoked), err)
	}
	byPublisher, err := p.Search(ctx(), Query{Publisher: "acme"})
	if err != nil || len(byPublisher) != 1 {
		t.Fatalf("publisher filter = %#v err=%v", byPublisher, err)
	}
	byCapability, err := p.Search(ctx(), Query{Capability: "repo.read"})
	if err != nil || len(byCapability) != 1 {
		t.Fatalf("capability filter = %#v err=%v", byCapability, err)
	}
	none, err := p.Search(ctx(), Query{Capability: "net.egress"})
	if err != nil || len(none) != 0 {
		t.Fatalf("capability filter for an unrequested capability = %#v err=%v", none, err)
	}
}

func TestFileProvider_ListVersionsShowsWithdrawnOnes(t *testing.T) {
	root := buildRegistry(t,
		releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}, mutateRelease: func(r *Release) {
			r.Revoked, r.RevocationReason = true, "malicious build"
		}},
		releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.2.0"}},
	)
	p := provider(t, root)
	got, err := p.ListVersions(ctx(), "security-audit")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	// The version list is where somebody finds out the version they wanted was
	// withdrawn. Hiding it there would answer "where did 0.1.0 go" with silence.
	if len(got) != 2 || got[0].Version != "0.2.0" || !got[1].Revoked {
		t.Fatalf("ListVersions = %#v", got)
	}
	if _, err := p.ListVersions(ctx(), "nope"); !errors.Is(err, ErrNoSuchSkill) {
		t.Fatalf("ListVersions(nope) = %v, want ErrNoSuchSkill", err)
	}
}

func TestResolveExactRelease_RefusesAnythingButOneExactVersion(t *testing.T) {
	root := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}})
	p := provider(t, root)

	for _, version := range []string{"", "  ", "latest", "^0.1.0", "0.1", "0.1.x", "*"} {
		if _, err := p.ResolveExactRelease(ctx(), "security-audit", version); !errors.Is(err, ErrNoSuchRelease) {
			t.Fatalf("ResolveExactRelease(%q) = %v, want ErrNoSuchRelease", version, err)
		}
	}
	rel, err := p.ResolveExactRelease(ctx(), "security-audit", "0.1.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	if rel.Ref() != "security-audit@0.1.0" {
		t.Fatalf("resolved %q", rel.Ref())
	}
	if _, err := p.ResolveExactRelease(ctx(), "security-audit", "9.9.9"); !errors.Is(err, ErrNoSuchRelease) {
		t.Fatalf("unknown version = %v", err)
	}
	if _, err := p.ResolveExactRelease(ctx(), "nope", "0.1.0"); !errors.Is(err, ErrNoSuchSkill) {
		t.Fatalf("unknown skill = %v", err)
	}
}

// A registry that lists one identity twice is the mutable-version problem in
// its simplest form: two rows, and whichever the reader hits first wins.
func TestFileProvider_RefusesADuplicateVersion(t *testing.T) {
	root := buildRegistry(t,
		releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}},
		releaseSpec{
			pkg:          pkgSpec{id: "security-audit", version: "0.1.0", extra: "different bytes\n"},
			artifactPath: "packages/security-audit/0.1.0-b",
		},
	)
	_, err := NewFileProvider("fixture", root)
	if !errors.Is(err, ErrRegistryUnreadable) {
		t.Fatalf("duplicate version = %v, want ErrRegistryUnreadable", err)
	}
	if !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("error = %q", err)
	}
}

// An unreadable registry must not answer a search with an empty list: "this
// registry has nothing" is a materially more comfortable fact than the truth.
func TestFileProvider_UnreadableIsNotEmpty(t *testing.T) {
	t.Run("missing index", func(t *testing.T) {
		if _, err := NewFileProvider("fixture", t.TempDir()); !errors.Is(err, ErrRegistryUnreadable) {
			t.Fatalf("missing index = %v", err)
		}
	})
	t.Run("unknown apiVersion", func(t *testing.T) {
		root := t.TempDir()
		writeIndex(t, root, map[string]any{"apiVersion": "ao.registry/v99", "releases": []any{}})
		_, err := NewFileProvider("fixture", root)
		if !errors.Is(err, ErrRegistryUnreadable) || !strings.Contains(err.Error(), "apiVersion") {
			t.Fatalf("unknown apiVersion = %v", err)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		root := t.TempDir()
		writeIndex(t, root, map[string]any{
			"apiVersion": IndexAPIVersion, "releases": []any{}, "autoInstall": true,
		})
		if _, err := NewFileProvider("fixture", root); !errors.Is(err, ErrRegistryUnreadable) {
			t.Fatalf("unknown index field = %v, want a refusal", err)
		}
	})
	t.Run("invalid release", func(t *testing.T) {
		root := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}})
		var idx map[string]any
		b, _ := os.ReadFile(filepath.Join(root, IndexFileName))
		_ = json.Unmarshal(b, &idx)
		idx["releases"].([]any)[0].(map[string]any)["requestedCapabilities"] = []any{"repo.pillage"}
		writeIndex(t, root, idx)
		if _, err := NewFileProvider("fixture", root); !errors.Is(err, ErrRegistryUnreadable) {
			t.Fatalf("invalid release = %v", err)
		}
	})
	t.Run("relative root", func(t *testing.T) {
		if _, err := NewFileProvider("fixture", "registry"); !errors.Is(err, ErrRegistryUnreadable) {
			t.Fatalf("relative root = %v", err)
		}
	})
}

// A malicious registry naming a path outside its own root must be refused
// before anything reads it.
func TestFileProvider_RefusesPathTraversal(t *testing.T) {
	for _, bad := range []string{"../outside", "packages/../../etc", "/etc/passwd"} {
		t.Run(bad, func(t *testing.T) {
			root := buildRegistry(t, releaseSpec{
				pkg:          pkgSpec{id: "security-audit", version: "0.1.0"},
				artifactPath: bad,
				skipPackage:  true,
			})
			_, err := NewFileProvider("fixture", root)
			if !errors.Is(err, ErrRegistryUnreadable) {
				t.Fatalf("artifactPath %q = %v, want ErrRegistryUnreadable", bad, err)
			}
		})
	}
}

// The string check above catches "../..". This is the case it cannot: a path
// that stays inside the root textually and leaves it through a symlink.
func TestFileProvider_RefusesASymlinkedArtifactDirectory(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "loot.txt"), []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write loot: %v", err)
	}
	root := buildRegistry(t, releaseSpec{
		pkg:          pkgSpec{id: "security-audit", version: "0.1.0"},
		artifactPath: "packages/security-audit/0.1.0",
	})
	// Replace the honest package tree with a link to somewhere else entirely.
	pkgDir := filepath.Join(root, "packages", "security-audit", "0.1.0")
	if err := os.RemoveAll(pkgDir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, pkgDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := provider(t, root)
	if _, err := p.ResolveExactRelease(ctx(), "security-audit", "0.1.0"); !errors.Is(err, ErrRegistryUnreadable) {
		t.Fatalf("symlinked artifact resolved: %v", err)
	}
	if err := p.FetchArtifact(ctx(), Release{SkillID: "security-audit", Version: "0.1.0"}, t.TempDir()); err == nil {
		t.Fatal("FetchArtifact followed a symlink out of the registry root")
	}
}

// A symlink INSIDE the package tree is the other half: the tree is where it
// says it is, and one file in it points at a credential on this host.
func TestFetchArtifact_RefusesASymlinkInsideThePackage(t *testing.T) {
	root := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}})
	pkgDir := filepath.Join(root, "packages", "security-audit", "0.1.0")
	if err := os.Symlink("/etc/passwd", filepath.Join(pkgDir, "notes.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p := provider(t, root)
	dest := filepath.Join(t.TempDir(), "quarantine")
	err := p.FetchArtifact(ctx(), Release{SkillID: "security-audit", Version: "0.1.0"}, dest)
	if err == nil {
		t.Fatal("FetchArtifact copied a symlink into the quarantine")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %q, want the refusal to name the symlink", err)
	}
}

func TestFetchArtifact_LandsBytesThatHashToTheDeclaredDigest(t *testing.T) {
	root := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}})
	p := provider(t, root)
	rel, err := p.ResolveExactRelease(ctx(), "security-audit", "0.1.0")
	if err != nil {
		t.Fatalf("ResolveExactRelease: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "quarantine")
	if err := p.FetchArtifact(ctx(), rel, dest); err != nil {
		t.Fatalf("FetchArtifact: %v", err)
	}
	got, err := skillcatalog.ComputePackageDigest(dest)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	if got != rel.ArtifactDigest {
		t.Fatalf("fetched digest %s != declared %s", got, rel.ArtifactDigest)
	}
	manifestDigest, err := FileDigest(filepath.Join(dest, skillcatalog.ManifestFileName))
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	if manifestDigest != rel.ManifestDigest {
		t.Fatalf("fetched manifest digest %s != declared %s", manifestDigest, rel.ManifestDigest)
	}
	// And the fetched tree is a package the catalog will actually accept.
	if _, err := skillcatalog.LoadPackage(dest); err != nil {
		t.Fatalf("LoadPackage on the fetched tree: %v", err)
	}
}

// The same version served with different bytes must be caught by the caller's
// hash, not by anything the registry says about itself.
func TestFetchArtifact_DifferentBytesUnderOneVersionChangeTheDigest(t *testing.T) {
	a := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0"}})
	b := buildRegistry(t, releaseSpec{pkg: pkgSpec{id: "security-audit", version: "0.1.0", extra: "backdoor\n"}})
	relA, err := provider(t, a).ResolveExactRelease(ctx(), "security-audit", "0.1.0")
	if err != nil {
		t.Fatalf("resolve a: %v", err)
	}
	relB, err := provider(t, b).ResolveExactRelease(ctx(), "security-audit", "0.1.0")
	if err != nil {
		t.Fatalf("resolve b: %v", err)
	}
	if relA.ArtifactDigest == relB.ArtifactDigest {
		t.Fatal("two different builds of one version hashed the same")
	}
}

func TestQuery_LimitsAreBounded(t *testing.T) {
	if got := (Query{}).EffectiveLimit(); got != DefaultSearchLimit {
		t.Fatalf("default limit = %d", got)
	}
	if got := (Query{Limit: -5}).EffectiveLimit(); got != DefaultSearchLimit {
		t.Fatalf("negative limit = %d", got)
	}
	if got := (Query{Limit: 100000}).EffectiveLimit(); got != MaxSearchLimit {
		t.Fatalf("huge limit = %d", got)
	}
	if got := (Query{Limit: 7}).EffectiveLimit(); got != 7 {
		t.Fatalf("explicit limit = %d", got)
	}
}
