// Package skillcatalog is AO's versioned catalog of installable Skills: the
// manifest contract, the installed-package registry, per-project activation,
// and the fail-closed capability resolution that must pass before a Skill may
// ever run.
//
// # Relationship to skillassets
//
// internal/skillassets already ships ONE skill (using-ao): an embedded tree the
// daemon clobbers into <dataDir>/skills/using-ao at every boot. That is
// deliberately unmanaged — the daemon binary is its version, there is no
// registry, no per-project choice, and no permission model, because a CLI
// reference guide needs none. This package does not replace it and does not
// touch its directory; skillassets stays the built-in, always-on skill and
// skillcatalog owns <dataDir>/skills/catalog for everything installed.
//
// # What this package is not
//
// It is not a sandbox and it does not execute anything. A manifest is a
// declaration, not a security boundary: a prompt cannot stop a process, and
// this package never claims otherwise. Capabilities that need real containment
// (network egress, active scanning, arbitrary process execution) are refused
// unless a Runner attests that it actually provides it — and no such runner is
// wired yet, so those capabilities are refused today. See
// docs/adr/0003-skill-catalog-foundation.md.
//
// # Deliberate isolation
//
// This is a leaf package. It imports internal/domain for the existing
// Permission and ProjectID vocabulary (so activation reuses AO's RBAC rather
// than inventing a parallel one) and nothing else from AO. It adds no
// migration, no HTTP route, no CLI command and no lifecycle hook; wiring is a
// later, separate phase.
package skillcatalog
