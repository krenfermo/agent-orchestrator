package skillregistry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// fixture_test.go -- a real registry on disk, built from real package trees
// whose digests are computed from the bytes actually written.
//
// Nothing here fakes a digest. A supply-chain test that asserted on a
// hard-coded hash would pass whether or not the verification path works, which
// is the one thing these tests exist to prove.

const fixtureManifest = `apiVersion: ao.skill/v1
id: PKG_ID
name: Example Audit
version: PKG_VERSION
description: An example skill used by the registry tests.
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

// pkgSpec is one package tree the fixture should lay down.
type pkgSpec struct {
	id        string
	version   string
	publisher string
	// extra adds one more file, which is how two builds of the same version
	// end up with different bytes under one identity.
	extra string
	// mutate rewrites the manifest after the substitutions above.
	mutate func(string) string
}

// writePkg lays down one package tree and returns its computed artifact
// digest and manifest digest.
func writePkg(t *testing.T, dir string, spec pkgSpec) (artifactDigest, manifestDigest string) {
	t.Helper()
	for _, sub := range []string{"schemas", "modes"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(rel)), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("schemas/out.json", "{\"type\":\"object\"}\n")
	write("modes/quick.md", "# quick\n")
	if spec.extra != "" {
		write("extra.txt", spec.extra)
	}

	publisher := spec.publisher
	if publisher == "" {
		publisher = "tests"
	}
	body := strings.ReplaceAll(fixtureManifest, "PKG_ID", spec.id)
	body = strings.ReplaceAll(body, "PKG_VERSION", spec.version)
	body = strings.ReplaceAll(body, "PKG_PUBLISHER", publisher)
	if spec.mutate != nil {
		body = spec.mutate(body)
	}
	write(skillcatalog.ManifestFileName, body)

	// The package digest excludes the manifest, which is exactly what lets it
	// be computed and then written into the manifest.
	digest, err := skillcatalog.ComputePackageDigest(dir)
	if err != nil {
		t.Fatalf("ComputePackageDigest: %v", err)
	}
	body = strings.Replace(body, "DIGEST_PLACEHOLDER", digest, 1)
	write(skillcatalog.ManifestFileName, body)

	manifestDigest, err = FileDigest(filepath.Join(dir, skillcatalog.ManifestFileName))
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	return digest, manifestDigest
}

// releaseSpec is one index entry the fixture should publish.
type releaseSpec struct {
	pkg pkgSpec
	// mutateRelease rewrites the release AFTER its digests were computed from
	// the bytes on disk. It is how a test publishes a WRONG digest without
	// faking the package.
	mutateRelease func(*Release)
	// artifactPath overrides where the index says the bytes are.
	artifactPath string
	// skipPackage publishes an index entry with no package tree behind it.
	skipPackage bool
}

// buildRegistry writes a complete local registry and returns its root.
func buildRegistry(t *testing.T, specs ...releaseSpec) string {
	t.Helper()
	root := t.TempDir()
	entries := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		artifactPath := spec.artifactPath
		if artifactPath == "" {
			artifactPath = "packages/" + spec.pkg.id + "/" + spec.pkg.version
		}
		var artifactDigest, manifestDigest string
		if !spec.skipPackage {
			pkgDir := filepath.Join(root, filepath.FromSlash(artifactPath))
			if err := os.MkdirAll(pkgDir, 0o700); err != nil {
				t.Fatalf("mkdir package: %v", err)
			}
			artifactDigest, manifestDigest = writePkg(t, pkgDir, spec.pkg)
		} else {
			artifactDigest = strings.Repeat("a", 64)
			manifestDigest = strings.Repeat("b", 64)
		}
		publisher := spec.pkg.publisher
		if publisher == "" {
			publisher = "tests"
		}
		rel := Release{
			SkillID:               spec.pkg.id,
			Name:                  "Example Audit",
			Version:               spec.pkg.version,
			Publisher:             publisher,
			Description:           "An example skill used by the registry tests.",
			RiskLevel:             "medium",
			ManifestDigest:        manifestDigest,
			ArtifactDigest:        artifactDigest,
			RequestedCapabilities: []string{"repo.read", "report.write"},
			ExecutionModes: []ReleaseMode{{
				ID: "quick", Name: "Quick", Description: "A quick pass.",
				RiskLevel: "low", Capabilities: []string{"repo.read", "report.write"},
			}},
			Compatibility: Compatibility{AOMinVersion: "0.11.0"},
			PublishedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		if spec.mutateRelease != nil {
			spec.mutateRelease(&rel)
		}
		entry := map[string]any{}
		relJSON, err := json.Marshal(rel)
		if err != nil {
			t.Fatalf("marshal release: %v", err)
		}
		if err := json.Unmarshal(relJSON, &entry); err != nil {
			t.Fatalf("unmarshal release: %v", err)
		}
		// registryId is AO's, set by the provider. Leaving it out of the file
		// is what the real layout looks like.
		delete(entry, "registryId")
		entry["artifactPath"] = artifactPath
		entries = append(entries, entry)
	}
	writeIndex(t, root, map[string]any{
		"apiVersion": IndexAPIVersion,
		"releases":   entries,
	})
	return root
}

func writeIndex(t *testing.T, root string, index map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, IndexFileName), append(b, '\n'), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

func provider(t *testing.T, root string) *FileProvider {
	t.Helper()
	p, err := NewFileProvider("fixture", root)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	return p
}
