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

AO has **no isolated runner**. Every capability that needs real containment is
refused at plan time, with the reason:

| Capability | Blocked because |
| --- | --- |
| `repo.write`, `process.exec`, `secrets.read` | needs an isolated execution environment |
| `net.egress`, `net.active_scan` | needs isolation *and* controlled egress |

So for the shipped `security-audit` package: `static-code`, `secret-scan` and
`authz-review` come back *Can run*; `dependencies`, `api-infra-review` and
`active-pentest` come back *Blocked*.

That refusal is the control working. A manifest and a prompt are not a security
boundary — the dry run trusts exactly one value for containment, the runner's
own attestation, and AO's runner honestly attests nothing. Nothing in the API
accepts a self-declared `isolated: true` from a caller.

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
