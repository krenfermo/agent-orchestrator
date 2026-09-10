package githubtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry/registrytest"
)

// publish.go -- putting a real, tagged, digest-consistent release into the
// fixture forge.
//
// The digests a descriptor declares are COMPUTED from the tree the fixture
// will actually serve in its archive, never written by hand. A supply-chain
// test that asserted against a hard-coded hash would pass whether or not the
// verification path works, which is the one thing these tests exist to prove.
// A test that wants a WRONG digest gets it by mutating the descriptor
// afterwards -- which is what a lying publisher does.
//
// The tree laid down on disk here is unpacked EXACTLY as the daemon will
// unpack the archive: one synthetic root stripped, .ao dropped when the
// package root is the repository root. If those two rules were re-implemented
// here the fixture would be asserting that two copies of the same idea agree,
// which is worth nothing.

// Spec is one release to publish into a repository.
type Spec struct {
	SkillID   string
	Name      string
	Version   string
	Publisher string
	// Tag is the git tag. Empty means "v" + Version, which is what a
	// repository usually does.
	Tag string
	// Commit is the SHA to publish at. Empty means one derived from the tag,
	// so a test that does not care never has to invent forty hex characters.
	Commit string
	// Path is the package root inside the repository. Empty means the
	// repository root, in which case .ao is excluded from the package.
	Path string
	// Extra adds one more file to the package tree, which is how two builds of
	// the same version end up with different bytes.
	Extra string
	// Prerelease and Draft mark the forge's own release flags.
	Prerelease bool
	Draft      bool
	// Annotated makes the tag an annotated tag object, so AO has to peel it.
	Annotated bool
	// MutateManifest rewrites skill.yaml before its digest is computed. It is
	// how a package comes to disagree with the descriptor that describes it.
	MutateManifest func(string) string
	// MutateRelease rewrites the descriptor AFTER its digests were computed
	// from the bytes the fixture will serve. It is how a publisher declares
	// one thing and ships another.
	MutateRelease func(*skillregistry.Release)
}

// Published is what a Publish produced, so a test can assert against the exact
// values the daemon will see.
type Published struct {
	Release skillregistry.Release
	Tag     string
	Commit  string
	// Files is the whole repository tree at that commit, descriptor included.
	Files map[string]string
	// Descriptor is the .ao/release.json body served at that commit.
	Descriptor string
}

// Publish adds one tagged release to a repository, with digests computed from
// the bytes the fixture will serve.
func (r *Repo) Publish(t *testing.T, spec Spec) Published {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "External Audit"
	}
	if spec.Publisher == "" {
		spec.Publisher = "tests"
	}
	tag := spec.Tag
	if tag == "" {
		tag = "v" + spec.Version
	}
	commit := spec.Commit
	if commit == "" {
		commit = CommitSHA(r.Owner + "/" + r.Name + "@" + tag)
	}
	pkgPath := strings.Trim(strings.TrimSpace(spec.Path), "/")

	manifest := strings.NewReplacer(
		"PKG_ID", spec.SkillID,
		"PKG_NAME", spec.Name,
		"PKG_VERSION", spec.Version,
		"PKG_PUBLISHER", spec.Publisher,
	).Replace(registrytest.ManifestTemplate)
	if spec.MutateManifest != nil {
		manifest = spec.MutateManifest(manifest)
	}

	// The package tree, as it will exist AFTER the daemon unpacks the archive.
	pkg := map[string]string{
		"schemas/out.json": "{\"type\":\"object\"}\n",
		"modes/quick.md":   "# quick\n",
	}
	if spec.Extra != "" {
		pkg["extra.txt"] = spec.Extra
	}
	dir := t.TempDir()
	pkg[skillcatalog.ManifestFileName] = manifest
	registrytest.PackageDir(t, dir, pkg)
	artifactDigest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	manifest = strings.Replace(manifest, "DIGEST_PLACEHOLDER", artifactDigest, 1)
	pkg[skillcatalog.ManifestFileName] = manifest
	if err := os.WriteFile(
		filepath.Join(dir, skillcatalog.ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	manifestDigest := registrytest.Sha256(manifest)

	rel := skillregistry.Release{
		SkillID:               spec.SkillID,
		Name:                  spec.Name,
		Version:               spec.Version,
		Publisher:             spec.Publisher,
		Description:           "A skill published by the forge fixture.",
		RiskLevel:             "medium",
		ManifestDigest:        manifestDigest,
		ArtifactDigest:        artifactDigest,
		RequestedCapabilities: []string{"repo.read", "report.write"},
		ExecutionModes: []skillregistry.ReleaseMode{{
			ID: "quick", Name: "Quick", Description: "A quick pass.",
			RiskLevel: "low", Capabilities: []string{"repo.read", "report.write"},
		}},
		Compatibility: skillregistry.Compatibility{AOMinVersion: "0.11.0"},
		PublishedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		// The publisher declares ONLY the path. Everything else in the source
		// is stamped by AO from its own resolution, and a fixture that filled
		// it in here would be testing that AO copies a payload rather than
		// that it refuses to.
		Source: skillregistry.GitSource{Path: pkgPath},
	}
	if spec.MutateRelease != nil {
		spec.MutateRelease(&rel)
	}

	// The repository tree at this commit: the package under its path, plus the
	// descriptor at .ao/release.json.
	files := map[string]string{}
	prefix := ""
	if pkgPath != "" {
		prefix = pkgPath + "/"
	}
	for name, body := range pkg {
		files[prefix+name] = body
	}
	descriptor := descriptorJSON(t, rel)
	files[skillregistry.GitHubDescriptorPath] = descriptor

	r.Commit(commit, files)
	if spec.Draft || spec.Prerelease {
		r.tags[tag] = commit
		r.PublishRaw(Release{
			Tag: tag, Name: tag, Draft: spec.Draft, Prerelease: spec.Prerelease,
			PublishedAt: rel.PublishedAt,
		})
	} else {
		r.Tag(tag, commit)
	}
	if spec.Annotated {
		r.Annotate(tag)
	}

	// What the daemon will see, with the source AO stamps.
	resolved := rel
	resolved.Source = skillregistry.GitSource{
		Provider:   skillregistry.SourceGitHub,
		Owner:      r.Owner,
		Repository: r.Name,
		Tag:        tag,
		Commit:     commit,
		Path:       pkgPath,
		Visibility: visibilityOf(r),
	}
	return Published{
		Release: resolved, Tag: tag, Commit: commit,
		Files: files, Descriptor: descriptor,
	}
}

// Republish writes the same skill at a DIFFERENT commit and moves the tag onto
// it.
//
// This is the force-push, in one call: the tag name is unchanged, the release
// listing is unchanged, and the bytes behind the name are somebody else's.
func (r *Repo) Republish(t *testing.T, spec Spec, newCommit string) Published {
	t.Helper()
	spec.Commit = newCommit
	out := r.Publish(t, spec)
	tag := spec.Tag
	if tag == "" {
		tag = "v" + spec.Version
	}
	r.MoveTag(tag, newCommit)
	return out
}

func visibilityOf(r *Repo) string {
	if r.Private {
		return "private"
	}
	return "public"
}

// descriptorJSON renders the .ao/release.json body.
func descriptorJSON(t *testing.T, rel skillregistry.Release) string {
	t.Helper()
	body, err := json.MarshalIndent(struct {
		APIVersion string                `json:"apiVersion"`
		Release    skillregistry.Release `json:"release"`
	}{APIVersion: skillregistry.ProtocolVersion, Release: rel}, "", "  ")
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	return string(body) + "\n"
}

// CommitSHA derives a deterministic, full-length commit SHA from a label, so a
// test can name commits readably without inventing forty hex characters.
func CommitSHA(label string) string { return registrytest.Sha256(label)[:40] }

// SignDescriptor re-renders a repository's descriptor with a signature
// attached, at the same commit.
//
// The signature is made over the release AS AO WILL RECONSTRUCT IT -- with the
// registry id stamped in -- which is what the daemon's verifier rebuilds. A
// test that wants a signature that does NOT match mutates the release
// afterwards, which is what a tampering publisher does.
func (r *Repo) SignDescriptor(
	t *testing.T, commit string, rel skillregistry.Release, registryID string,
	signer *registrytest.Signer, signedAt time.Time,
) skillregistry.Release {
	t.Helper()
	signed := rel
	signed.RegistryID = registryID
	sig := signer.SignRelease(t, signed, signedAt)
	signed.Signature = sig
	// The descriptor on disk carries no registry id: AO stamps that itself.
	onDisk := rel
	onDisk.Signature = sig
	tree := r.commits[commit]
	if tree == nil {
		t.Fatalf("SignDescriptor: no commit %s in %s/%s", commit, r.Owner, r.Name)
	}
	tree[skillregistry.GitHubDescriptorPath] = descriptorJSON(t, onDisk)
	return signed
}
