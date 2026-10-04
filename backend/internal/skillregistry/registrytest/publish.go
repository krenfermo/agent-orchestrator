package registrytest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillregistry"
)

// publish.go -- putting a real package into the fixture.
//
// The digests a published release declares are COMPUTED from the bytes the
// fixture will actually serve, never written by hand. A supply-chain test that
// asserted against a hard-coded hash would pass whether or not the verification
// path works, which is the one thing these tests exist to prove. A test that
// wants a WRONG digest gets it by mutating the release afterwards, which is
// what a lying registry does.

// ManifestTemplate is the skill.yaml a published package carries. The
// placeholders are substituted by Publish.
const ManifestTemplate = `apiVersion: ao.skill/v1
id: PKG_ID
name: PKG_NAME
version: PKG_VERSION
description: A skill published by the private-registry fixture.
origin:
  type: local
  ref: /tmp/example
riskLevel: medium
capabilities: [repo.read, report.write]
scope:
  files:
    read: ["**"]
  repos:
    mode: worktree
  network:
    mode: none
outputs:
  format: json
  schemaRef: schemas/out.json
authorization:
  requiredPermissions: [project.read]
  approval: per_activation
compatibility:
  aoMinVersion: 0.11.0
integrity:
  algorithm: sha256
  digest: "DIGEST_PLACEHOLDER"
provenance:
  publisher: PKG_PUBLISHER
policy:
  update: manual
  deactivateRevokesGrants: true
modes:
  - id: quick
    name: Quick
    description: A quick pass.
    riskLevel: low
    capabilities: [repo.read, report.write]
    approval: per_activation
    guide: modes/quick.md
`

// Spec is one release to publish.
type Spec struct {
	SkillID   string
	Name      string
	Version   string
	Publisher string
	// Extra adds one more file to the package. It is how two builds of the
	// same version end up with different bytes under one identity.
	Extra string
	// MutateManifest rewrites the manifest before its digest is computed. It
	// is how a package comes to disagree with the release that describes it.
	MutateManifest func(string) string
	// MutateRelease rewrites the release AFTER its digests were computed from
	// the bytes the fixture will serve. It is how a registry publishes a
	// declaration that does not match what it hands over.
	MutateRelease func(*skillregistry.Release)
}

// Publish adds one release, with digests computed from the bytes served.
func (s *Server) Publish(t *testing.T, spec Spec) skillregistry.Release {
	t.Helper()
	if spec.Name == "" {
		spec.Name = "Example Audit"
	}
	if spec.Publisher == "" {
		spec.Publisher = "tests"
	}

	files := map[string]string{
		"schemas/out.json": "{\"type\":\"object\"}\n",
		"modes/quick.md":   "# quick\n",
	}
	if spec.Extra != "" {
		files["extra.txt"] = spec.Extra
	}
	manifest := strings.NewReplacer(
		"PKG_ID", spec.SkillID,
		"PKG_NAME", spec.Name,
		"PKG_VERSION", spec.Version,
		"PKG_PUBLISHER", spec.Publisher,
	).Replace(ManifestTemplate)
	if spec.MutateManifest != nil {
		manifest = spec.MutateManifest(manifest)
	}

	// The package digest excludes the manifest, which is what lets it be
	// computed and then written INTO the manifest. Both digests are taken from
	// a real tree laid down on disk, exactly as the daemon will compute them
	// from the tree it unpacks.
	dir := t.TempDir()
	files[skillcatalog.ManifestFileName] = manifest
	PackageDir(t, dir, files)
	artifactDigest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	manifest = strings.Replace(manifest, "DIGEST_PLACEHOLDER", artifactDigest, 1)
	files[skillcatalog.ManifestFileName] = manifest
	if err := os.WriteFile(filepath.Join(dir, skillcatalog.ManifestFileName), []byte(manifest), 0o600); err != nil {
		t.Fatalf("rewrite manifest: %v", err)
	}
	manifestDigest := Sha256(manifest)

	rel := skillregistry.Release{
		SkillID:               spec.SkillID,
		Name:                  spec.Name,
		Version:               spec.Version,
		Publisher:             spec.Publisher,
		Description:           "A skill published by the private-registry fixture.",
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
	}
	if spec.MutateRelease != nil {
		spec.MutateRelease(&rel)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases[spec.SkillID+"@"+spec.Version] = &Entry{Release: rel, Files: files}
	return rel
}
