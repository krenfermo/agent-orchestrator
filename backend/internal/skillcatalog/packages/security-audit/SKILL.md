---
name: security-audit
description: On-demand security audit of one selected AO project, in one explicitly chosen mode.
---

# Security Audit

This skill audits **one project, on demand, in one mode, with an explicit
scope**. It never runs because a change landed; a person selects the project,
the mode and the scope every time.

## Before anything else

1. Confirm which project is being audited and that the caller intends this one.
2. Confirm the mode. One run is one mode — do not drift from `static-code` into
   probing a running service.
3. Confirm the scope. If `scopePaths` is set, everything outside it is out of
   scope and must be listed in `coverage.skipped`.
4. For `active-pentest` only: confirm the target and the `authorizationRef`.
   Without both, stop and report that the run is unauthorized. See
   [modes/active-pentest.md](modes/active-pentest.md).

## Mode guides

Read only the guide for the selected mode:

- [modes/static-code.md](modes/static-code.md)
- [modes/dependencies.md](modes/dependencies.md)
- [modes/secret-scan.md](modes/secret-scan.md)
- [modes/authz-review.md](modes/authz-review.md)
- [modes/api-infra-review.md](modes/api-infra-review.md)
- [modes/active-pentest.md](modes/active-pentest.md)

## Output

Every mode produces one JSON document validating against
[schemas/findings.v1.json](schemas/findings.v1.json). Nothing else is a report.

`coverage` is not optional. An empty `findings` list from a run that examined
nothing looks identical to a clean audit, and the difference is the whole value
of the report.

## Reporting rules

- **Never put a secret in the report.** If you find a credential, cite
  `path` and `line` and describe what kind of secret it is. Do not copy the
  value, a prefix of it, or a hash of it into `evidence`, `notes`, or a commit
  message. The same applies to personal data and customer content.
- **Severity is about impact, not about how alarming the code looks.** Rate what
  an attacker gains, given the preconditions you actually verified.
- **Confidence is separate from severity.** A `possible` critical is not a
  `confirmed` critical; say which one you have.
- **Every finding carries reproduction.** If you could not reproduce it, set
  `reproducible: false` and give the preconditions that would be needed. A
  finding with no path to reproduction is a hypothesis, and should say so.
- **Assess false positives explicitly.** For anything above `low`, fill in
  `falsePositiveAssessment`: what else could explain the code, and why that
  explanation does not hold here.
- **Recommend a fix, not a lecture.** One concrete change per finding.

## What this skill is not

A manifest and a prompt are not a security boundary. The tool allow/deny list
and the file scope in `skill.yaml` describe intent; only an isolated runner
enforces them. Until AO has one, the modes that need real containment
(`dependencies`, `active-pentest`) are refused by the catalog rather than run
unconfined. That refusal is the correct behavior — do not work around it.
