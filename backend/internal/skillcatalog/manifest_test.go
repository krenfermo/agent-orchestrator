package skillcatalog

import (
	"errors"
	"strings"
	"testing"
)

// validManifestYAML is the baseline every mutation test starts from. It is
// deliberately a full, realistic manifest: a test that mutates a stripped-down
// fixture proves nothing about the manifests AO will actually load.
const validManifestYAML = `
apiVersion: ao.skill/v1
id: example-audit
name: Example Audit
version: 1.2.3
description: An example skill used by the catalog tests.
origin:
  type: local
  ref: /tmp/example
riskLevel: medium
capabilities: [repo.read, report.write]
tools:
  allowed: [Read, Grep]
  denied: [Write]
scope:
  files:
    read: ["**"]
  repos:
    mode: worktree
  network:
    mode: none
inputs:
  - name: mode
    type: enum
    required: true
    enum: [quick, deep]
    description: How thorough to be.
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
  digest: "1111111111111111111111111111111111111111111111111111111111111111"
provenance:
  publisher: tests
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

func decode(t *testing.T, yamlText string) (Manifest, error) {
	t.Helper()
	return DecodeManifest(strings.NewReader(yamlText))
}

func TestDecodeManifest_AcceptsAValidManifest(t *testing.T) {
	m, err := decode(t, validManifestYAML)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if m.ID != "example-audit" || m.Version != "1.2.3" {
		t.Fatalf("decoded %q@%q", m.ID, m.Version)
	}
	if got, ok := m.Mode("quick"); !ok || got.Approval != ApprovalPerActivation {
		t.Fatalf("mode quick = %#v ok=%v", got, ok)
	}
}

// An unknown key must be an error, not a dropped field. `capabilties: []`
// accepted as "declares nothing" is the worst possible reading of a typo in a
// security-relevant key.
func TestDecodeManifest_RejectsUnknownFields(t *testing.T) {
	_, err := decode(t, validManifestYAML+"\nsandboxed: true\n")
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
	if !strings.Contains(err.Error(), "sandboxed") {
		t.Fatalf("error should name the unknown field, got %v", err)
	}
}

func TestDecodeManifest_RejectsASecondDocument(t *testing.T) {
	_, err := decode(t, validManifestYAML+"\n---\nid: sneaky\n")
	if !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("err = %v, want ErrInvalidManifest", err)
	}
}

// Each case replaces one snippet of the valid manifest and must be rejected.
// They are the contract's real teeth: every one is a way an accepted manifest
// would have granted something nobody decided.
func TestManifestValidation_RejectsInvalidManifests(t *testing.T) {
	cases := []struct {
		name    string
		old     string
		new     string
		wantSub string
	}{
		{"unsupported api version", "apiVersion: ao.skill/v1", "apiVersion: ao.skill/v2", "apiVersion"},
		{"uppercase id", "id: example-audit", "id: ExampleAudit", "kebab-case"},
		{"reserved id", "id: example-audit", "id: using-ao", "reserved"},
		{"non semver version", "version: 1.2.3", "version: v1.2", "MAJOR.MINOR.PATCH"},
		{"missing description", "description: An example skill used by the catalog tests.", "description: \"\"", "description is required"},
		{"builtin origin with ref", "  type: local\n  ref: /tmp/example", "  type: builtin\n  ref: /tmp/example", "origin.ref must be empty"},
		{"git origin without ref", "  type: local\n  ref: /tmp/example", "  type: git", "origin.ref is required"},
		{"unknown risk level", "riskLevel: medium", "riskLevel: spicy", "riskLevel"},
		{"unknown capability", "capabilities: [repo.read, report.write]", "capabilities: [repo.read, kernel.load]", "unknown capability"},
		{"duplicate capability", "capabilities: [repo.read, report.write]", "capabilities: [repo.read, repo.read]", "twice"},
		{"empty capabilities", "capabilities: [repo.read, report.write]", "capabilities: []", "at least one capability"},
		{"absolute file scope", `    read: ["**"]`, `    read: ["/etc/**"]`, "repo-relative"},
		{"traversing file scope", `    read: ["**"]`, `    read: ["../other/**"]`, "traverse upward"},
		{"allow entries without allowlist mode", "    mode: none", "    mode: none\n    allow: [\"example.com:443\"]", "must be empty"},
		{"egress capability without network scope", "capabilities: [repo.read, report.write]", "capabilities: [repo.read, report.write, net.egress]", "allowlist"},
		{"active scan without egress", "capabilities: [repo.read, report.write]", "capabilities: [repo.read, report.write, net.active_scan]", "requires"},
		{"secret value in scope", "  repos:", "  secrets:\n    requested: [\"hunter2\"]\n  repos:", "UPPER_SNAKE_CASE"},
		{"unknown input type", "    type: enum", "    type: regex", "not a supported type"},
		{"enum without values", "    enum: [quick, deep]", "    enum: []", "lists no values"},
		{"non json output", "  format: json", "  format: sarif", "outputs.format"},
		{"unknown permission", "  requiredPermissions: [project.read]", "  requiredPermissions: [skills.god]", "not an AO permission"},
		{"no permissions", "  requiredPermissions: [project.read]", "  requiredPermissions: []", "at least one AO permission"},
		{"bad digest", `  digest: "1111111111111111111111111111111111111111111111111111111111111111"`, `  digest: "abc"`, "64 lowercase hex"},
		{"unsupported digest algorithm", "  algorithm: sha256", "  algorithm: md5", "integrity.algorithm"},
		{"unverifiable signature", "  publisher: tests", "  publisher: tests\n  signature: MEUCIQ", "cannot verify signatures"},
		{"auto enable", "  update: manual", "  update: manual\n  autoEnable: true", "autoEnable must be false"},
		{"grants survive deactivation", "  deactivateRevokesGrants: true", "  deactivateRevokesGrants: false", "deactivateRevokesGrants must be true"},
		{"mode requests undeclared capability", "    capabilities: [repo.read, report.write]\n    approval: per_activation", "    capabilities: [repo.read, net.egress]\n    approval: per_activation", "does not declare"},
		{"mode weakens approval", "    approval: per_activation\n    guide: modes/quick.md", "    approval: none\n    guide: modes/quick.md", "weaker than the skill approval"},
		{"mode exceeds skill risk", "    riskLevel: low", "    riskLevel: critical", "exceeds the skill riskLevel"},
		{"mode guide escapes the package", "    guide: modes/quick.md", "    guide: ../../etc/passwd", "package-relative"},
		{"aoMax below aoMin", "  aoMinVersion: 0.11.0", "  aoMinVersion: 0.11.0\n  aoMaxVersion: 0.9.0", "below aoMinVersion"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(validManifestYAML, tc.old) {
				t.Fatalf("fixture no longer contains %q; update the test", tc.old)
			}
			mutated := strings.Replace(validManifestYAML, tc.old, tc.new, 1)
			_, err := decode(t, mutated)
			if err == nil {
				t.Fatalf("manifest was accepted but should have been rejected")
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %v does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// A manifest with no modes has no way to be run, and "run the whole skill"
// would mean running its full capability union. Rejecting is checked directly
// rather than through YAML, because the strict decoder catches a removed key
// before Validate ever sees it.
func TestManifestValidation_RejectsAManifestWithNoModes(t *testing.T) {
	m, err := decode(t, validManifestYAML)
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	m.Modes = nil
	err = m.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one mode") {
		t.Fatalf("err = %v, want a no-modes rejection", err)
	}
}

func TestParseVersion_OrdersVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.1.0", "1.0.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"0.9.0", "1.0.0", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
	}
	for _, tc := range cases {
		a, err := ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.a, err)
		}
		b, err := ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.b, err)
		}
		got := a.Compare(b)
		if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
			t.Fatalf("%s vs %s = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseVersion_RejectsLooseVersions(t *testing.T) {
	for _, bad := range []string{"", "1", "1.2", "v1.2.3", "1.2.3.4", "01.2.3", "1.2.3+build"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Fatalf("ParseVersion(%q) was accepted", bad)
		}
	}
}
