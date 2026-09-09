# Using the skill catalog

What a person can do today, and what stays blocked until AO has an isolated
runner. For the design see `docs/adr/0003-skill-catalog-foundation.md`; for
what comes next see `roadmap.md`.

## The one rule worth stating first

**Installing a skill never enables it, and enabling it never runs it.** Those
are three separate acts with three different authorities, and nothing in AO
collapses them.

## From the desktop app

**Project Settings → Skills.**

- See which skills this project has enabled, the exact capabilities granted, who
  approved them, and the pinned version.
- Enable an installed skill: choose it, then grant capabilities one by one. Each
  one shows what it allows, its risk, and the permission needed to grant it. A
  capability you cannot grant is disabled with the permission named. A capability
  that needs containment says so — it will stay blocked until a runner exists.
- Disable a skill. That revokes the grant; re-enabling means granting again.
- **Check what a run needs.** Pick a mode and press it. It starts no process and
  changes nothing. You get: which capabilities are satisfied, which permissions
  you are missing by name, what approval is still outstanding, and whether the
  runner can carry it. The answer is *Can run*, *Needs approval*, or *Blocked*.

Installing a package is not in the UI: it takes an absolute path on the daemon
host, which is a CLI-shaped input. Use `ao skills install`.

## From the CLI

```bash
ao skills list                      # what is installed
ao skills list --project medusa     # what medusa has enabled
ao skills show security-audit --version 0.1.0
ao skills install /path/to/package  # validates and digest-verifies first
ao skills enable security-audit --project medusa --version 0.1.0 \
    --capability repo.read --capability report.write
ao skills dry-run security-audit --project medusa --mode static-code
ao skills disable security-audit --project medusa
ao skills uninstall security-audit --version 0.1.0
ao skills audit security-audit      # who installed, enabled, re-granted
```

## Who may do what

| Action | Permission |
| --- | --- |
| List installed skills, read one | `settings.read` |
| Install, uninstall | `settings.manage` |
| Read the catalog audit trail | `audit.read` (owner or administrator) |
| See a project's activations, dry-run | `project.read` on that project |
| Enable, disable on a project | `project.manage` on that project |

A grant can never exceed the approver's own permissions. A project
administrator holds `project.manage` but not `settings.manage`, so they can
grant `repo.read` and cannot grant `net.active_scan`.

On a single-user desktop with no identity layer, the loopback listener is
unauthenticated and trusted (see AGENTS.md), and the caller is treated as
holding every permission — the same passthrough every other AO route has there.

## What is blocked, and why

Every capability names the execution-environment **controls** it needs, and is
refused — naming the missing one — until they are all attested.

The container runner provides five: `filesystem_isolation`,
`process_isolation`, `no_credential_inheritance`, `resource_limits` and
`egress_deny_all`. That is enough for reading, and not enough for anything else.

| Capability | Missing control |
| --- | --- |
| `repo.write` | `writable_workspace` |
| `process.exec` | `arbitrary_process_execution` |
| `secrets.read` | `scoped_secret_delivery` |
| `net.egress`, `net.active_scan` | `egress_allowlist` — "the network is off" is not "the network is limited to these hosts" |

**Reading is not exempt.** There is no AO-enforced boundary for "an agent reads
the checkout" outside the container — the agent CLI's tool allowlist is void
under `bypassPermissions`, and a prompt, a manifest and a CLI permission are
none of them a security frontier. So the read modes require confinement too;
they simply need nothing beyond it.

With no container runtime, every mode reports *Blocked*. With one:

| Mode | Result |
| --- | --- |
| `static-code` | **Can run** — and actually executes, via `ao.static-scan/v1` |
| `secret-scan`, `authz-review` | *Can run* per the capability check; no tool contract is wired for them yet |
| `dependencies`, `api-infra-review` | *Blocked* — needs `egress_allowlist` |
| `active-pentest` | *Blocked* — needs `egress_allowlist`, and per-target approval besides |

That refusal is the control working. The dry run trusts exactly one value for
containment — the runner's own attestation — and it takes none from the caller.
There is no API field for a client to declare `isolated: true`.

## What `static-code` actually does

It stages **only the in-scope files** into a directory AO owns, mounts that
read-only, and runs AO's own pattern scanner in a container with no network.
`.git`, `node_modules` and `vendor` never cross the boundary.

The report carries coverage, not just findings: files staged, files the
container could actually see, files scanned, every file skipped with a reason,
the eight rules that ran, and the tool's own limitations. An empty findings list
means "these rules matched nothing in the files listed as scanned" — the report
never says "no vulnerabilities", because a pattern scan over a subset of files
cannot support that claim.

If the container saw fewer files than AO staged, **the run fails** rather than
producing a report. On macOS a bind mount from a path the VM does not share
arrives empty and silent, and a clean audit of a project nobody read is the
worst output this system could produce.

## Failure modes you may hit

- **`SKILL_VERSION_CONTENT_CHANGED`** — you re-installed different bytes under a
  version already installed. Publish a new version instead; an activation pins a
  version and would otherwise silently start meaning something else.
- **`SKILL_PACKAGE_ALTERED` / "does not match package contents"** — the package
  files changed on disk after install. Resolution fails closed. Reinstall from a
  clean source.
- **`SKILL_STILL_ENABLED`** — uninstall refuses while a project has that version
  enabled, and names the project. Disable it there first.
- **`SKILL_GRANT_REFUSED`** — the grant exceeds your own permissions; the error
  names the one you are missing.
