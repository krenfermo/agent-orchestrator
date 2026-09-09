# Mode: dependency review

Read dependency manifests and lockfiles and check them against published
advisories. This mode holds `repo.read`, `deps.read`, `net.egress` and
`report.write`.

`net.egress` means this mode **cannot run without an isolated runner that
confines outbound traffic to the declared allowlist**. If the catalog refuses
the run, that is the control working; report the refusal rather than falling
back to an unconfined fetch.

## Scope

Direct and transitive dependencies declared in the checkout's lockfiles. Only
the hosts in `scope.network.allow` may be contacted.

## What to look for

- Known-vulnerable versions, with the advisory id and the affected range.
- Whether the vulnerable code path is actually reachable from this project.
  An unreachable advisory is `low` with the reason recorded, not `critical`.
- Dependencies with no lockfile pin, floating ranges, or a `latest` tag.
- Direct git or URL dependencies that bypass the registry.
- Abandoned packages, and packages whose name is a near-miss of a popular one.
- Build-time dependencies with install scripts.

## Discipline

Report the advisory, the installed version, the fixed version, and the upgrade
that resolves it. If an upgrade is blocked by another pin, say which one.

Do not report the full transitive tree as findings. Summarise counts in `notes`
and file findings only for what needs action.
