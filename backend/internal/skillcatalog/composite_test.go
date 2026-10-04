package skillcatalog

import (
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// composite_test.go pins the 2E composite mode (ADR 0011): it composes, it
// cannot widen, and its parent authorization checks grants but no controls.

func withComposite(extra string) string {
	return strings.Replace(validManifestYAML, "    guide: modes/quick.md\n",
		"    guide: modes/quick.md\n"+extra, 1)
}

const secondMode = `  - id: deep
    name: Deep
    description: A deeper pass.
    riskLevel: medium
    capabilities: [repo.read, report.write]
    approval: per_activation
    guide: modes/deep.md
`

func compositeMode(caps, composes, risk, approval string) string {
	return "  - id: all\n    name: All\n    description: Everything.\n    riskLevel: " + risk +
		"\n    capabilities: " + caps + "\n    approval: " + approval +
		"\n    guide: modes/all.md\n    executor: composite\n    composes: " + composes + "\n"
}

func TestComposite_Validation(t *testing.T) {
	ok := withComposite(secondMode + compositeMode("[repo.read, report.write]", "[quick, deep]", "medium", "per_activation"))
	if m, err := decode(t, ok); err != nil || m.Modes[2].EffectiveExecutor() != ExecutorComposite {
		t.Fatalf("a valid composite was refused: %v", err)
	}
	for name, yaml := range map[string]string{
		"wider than the union":    withComposite(secondMode + compositeMode("[repo.read, report.write, deps.read]", "[quick, deep]", "medium", "per_activation")),
		"narrower than the union": withComposite(secondMode + compositeMode("[report.write]", "[quick, deep]", "medium", "per_activation")),
		"unknown mode":            withComposite(compositeMode("[repo.read, report.write]", "[quick, ghost]", "medium", "per_activation")),
		"duplicate":               withComposite(compositeMode("[repo.read, report.write]", "[quick, quick]", "medium", "per_activation")),
		"empty":                   withComposite(compositeMode("[repo.read, report.write]", "[]", "medium", "per_activation")),
		"risk below a composed":   withComposite(secondMode + compositeMode("[repo.read, report.write]", "[quick, deep]", "low", "per_activation")),
		"nested composite": withComposite(compositeMode("[repo.read, report.write]", "[quick]", "medium", "per_activation") +
			strings.Replace(compositeMode("[repo.read, report.write]", "[all]", "medium", "per_activation"), "id: all", "id: outer", 1)),
		"composes on a tool mode": strings.Replace(validManifestYAML, "    guide: modes/quick.md\n", "    guide: modes/quick.md\n    composes: [quick]\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(t, yaml); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
}

func TestComposite_ParentAuthorizationChecksGrantsNotControls(t *testing.T) {
	m, err := decode(t, withComposite(secondMode+compositeMode("[repo.read, report.write]", "[quick, deep]", "medium", "per_activation")))
	if err != nil {
		t.Fatal(err)
	}
	perms := []domain.Permission{domain.PermProjectRead}
	grant := Grant{Capabilities: []Capability{CapRepoRead, CapReportWrite}}
	// No runner at all: the parent is still authorized -- it executes nothing.
	if _, err := Authorize(AuthorizationRequest{Manifest: m, ModeID: "all", Grant: grant,
		SubjectPermissions: perms, Runner: NoRunner(), CompositeParent: true}); err != nil {
		t.Fatalf("parent: %v", err)
	}
	// ...but not without the grant.
	d, err := Authorize(AuthorizationRequest{Manifest: m, ModeID: "all",
		Grant: Grant{Capabilities: []Capability{CapReportWrite}}, SubjectPermissions: perms,
		Runner: NoRunner(), CompositeParent: true})
	if !errors.Is(err, ErrCapabilityDenied) || d.Denials[0].Reason != DenyNotGranted {
		t.Fatalf("an ungranted composite was authorized: %+v %v", d, err)
	}
	// A composite cannot be authorized as if it executed, and a normal mode
	// cannot borrow the parent's control-free path.
	if _, err := Authorize(AuthorizationRequest{Manifest: m, ModeID: "all", Grant: grant,
		SubjectPermissions: perms, Runner: completeRunner()}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("a composite authorized as an executing mode: %v", err)
	}
	if _, err := Authorize(AuthorizationRequest{Manifest: m, ModeID: "quick", Grant: grant,
		SubjectPermissions: perms, Runner: NoRunner(), CompositeParent: true}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("a tool mode took the control-free path: %v", err)
	}
}

func TestSecurityAudit_FullAuditComposesTheFourModes(t *testing.T) {
	pkg, err := LoadPackage(securityAuditDir)
	if err != nil {
		t.Fatal(err)
	}
	full, ok := pkg.Manifest.Mode("full-audit")
	if !ok || full.EffectiveExecutor() != ExecutorComposite {
		t.Fatal("full-audit is missing or not composite")
	}
	if strings.Join(full.Composes, ",") != "secret-scan,dependencies,static-code,authz-review" {
		t.Fatalf("composes = %v", full.Composes)
	}
}
