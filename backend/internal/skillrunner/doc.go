// Package skillrunner is AO's execution boundary for skills: a Linux container
// with no network, no root, no capabilities, an immutable root filesystem,
// kernel-enforced resource limits, and none of the daemon's environment.
//
// It is a PROTOTYPE of the boundary, not a production execution path. Nothing
// in AO calls it by default; wiring it in is a deliberate later step, and the
// controls it attests are a strict subset of what the blocked capabilities
// need. See docs/adr/0004-skill-runner-isolation.md.
//
// # The one rule
//
// If the runtime is not available, this package REFUSES. There is no host
// fallback and no degraded mode. A capability that needed containment and did
// not get it is refused, never downgraded — the same rule ADR 0003 applied to
// NoRunner(), restated where it is finally enforceable.
//
// # Attestation is earned, not declared
//
// Probe() asks the runtime what it actually supports and returns the set of
// controls that answer supports. A caller cannot supply an attestation, and no
// field of a run request influences one. A runtime that fails a probe attests
// less; it never attests more.
//
// # Evidence comes from the boundary
//
// Every run returns BoundaryEvidence that AO collected: the effective uid, the
// cgroup limits the kernel reports, whether outbound network was reachable,
// the image digest that actually ran, and the number of input files the
// container could see. That last one is a control rather than a diagnostic.
// On macOS a bind mount from a path the VM does not share mounts EMPTY, with
// no error and exit 0 — so a security audit would read nothing, find nothing,
// and report a clean result for a project it never opened. A run whose inputs
// did not arrive is failed here, so that report is unreachable.
package skillrunner
