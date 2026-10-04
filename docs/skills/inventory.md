# Skills phase 1 — inventory of what AO already has

What exists in the repository today for skills, tools, per-project
configuration, permissions, secrets, evidence and isolation. Written before the
catalog was designed, so the design could extend what is here instead of
building a second version of it.

Read with `docs/adr/0003-skill-catalog-foundation.md`, which states what was
decided as a result.

## 1. Skills, tools, MCP and adapters

### `backend/internal/skillassets` — one skill, deliberately unmanaged

`skillassets` embeds a single skill (`using-ao`) with `//go:embed` and installs
it at `<dataDir>/skills/using-ao`:

- `Install(dataDir)` is called once from `daemon.go:155` at boot, and
  **clobbers** the directory every time. The package doc is explicit that this
  is intentional: "the daemon binary already is the version", so there is no
  version marker and no hash.
- `Materialize(destDir)` writes the same tree anywhere; the opencode adapter
  uses it to place the skill under `.opencode/skills/`
  (`adapters/agent/opencode/hooks.go:217`).
- `session_manager/manager.go:3366` appends an absolute pointer to
  `SKILL.md` and `commands/*.md` into **every** agent system prompt.

**What it gives us:** the precedent that skills live under `~/.ao/skills/`, a
working prompt-injection point, and a per-adapter materialization path.

**What it does not give us:** a registry, versions, per-project choice,
capabilities, permissions, integrity, provenance, install/uninstall, or any
notion of a skill being *off*. It is one skill, always on, everywhere.

### Tool allow/deny — the one real enforcement point

`ports.LaunchConfig.AllowedTools` / `DisallowedTools`
(`internal/ports/agent.go:371`) scope an agent to a tool allowlist, and the doc
comment states the load-bearing caveat verbatim: **`bypassPermissions` ignores
both lists**, so a restricted launch must stay off bypass. The reviewer
adapters already rely on this (`adapters/reviewer/claudecode`, `copilot`,
`devin`) to get a genuinely read-only reviewer.

This is the closest thing AO has to an enforced tool boundary, and it lives at
the agent CLI, not in AO. It is per-launch, not per-skill.

### `backend/internal/reviewgateway` — prior art for a capability boundary

ADR 0002 built a provider-neutral boundary for interactive reviewer TUIs: a
private neutral working directory, isolated config/state/cache/temp roots, an
empty git-hooks dir, and a content-addressed task manifest under
`AO_DATA_DIR/reviewer-runtime/<reviewer-id>`. The ADR's own status line is
"Accepted (gateway); **platform isolation required before adapter rollout**".

It is narrow — hardcoded to reviewers, PR URLs and review-run ids — but it is
the right shape, and its status is the honest precedent for this phase: AO has
already decided once that a boundary without platform isolation is not a
boundary.

### MCP

No general MCP server registry. MCP appears only inside individual chat drivers
(`adapters/chatdriver/acp`, `codexappserver`) as part of those protocols. There
is nothing to extend for skill-supplied tools.

## 2. Per-project configuration

`domain.ProjectConfig` (`internal/domain/projectconfig.go`) is the typed
per-project config, persisted as **one JSON blob** in `projects.config`
(`storage/sqlite/queries/projects.sql`). Every field is typed and validated;
there is no free-form map, and the doc comment says only fields with a live
consumer are modeled.

Consequence for this phase: adding a field there needs **no migration**, but it
does touch a file the stabilization workstream may be editing, and it would put
skill state in a struct whose stated rule is "only fields with a live consumer".

## 3. Permissions, RBAC, capabilities and secrets

### RBAC is real and complete

`domain.Permission` (`internal/domain/authz.go`) is a closed vocabulary of 30
permissions with `AllPermissions` as a stable ordered list, three scopes
(global / tenant / project), role tables in `service/authz/roles.go`, and
`Service.Authorize(ctx, principal, perm, resource)`. The file's own comment
states the rule this phase adopted: *"There is no permission here for a feature
AO does not ship — an unenforced permission is a false promise."*

**This is the system not to duplicate.** Skill activation reuses
`domain.Permission` directly.

### "Capabilities" already means something else

`ports.ChatCapabilities` → `capabilityNames()` in `httpd/controllers/dto.go`
describes *what a session's provider can do*, so a client can gate UI. It is
descriptive, not authorizing. Skill capabilities are a different concept and
this phase keeps them in their own package rather than overloading that name in
`domain`.

### Secrets

- `internal/secretbox` — AES-GCM seal/open for the few credentials AO must read
  back (SMTP). Key at rest under the data dir.
- `internal/agentcred` — mints a credential into a `0600` file and passes the
  *filename* via `AO_AGENT_CREDENTIAL_FILE`, explicitly to keep the secret out
  of the process environment.
- `ProjectConfig.Env` — plain env vars forwarded into worker runtimes.

There is no named secret store a skill could request from, and no per-skill
secret scoping.

## 4. Execution records, evidence and reports

- `domain.Review` / `ReviewRun` — review lifecycle, verdicts, trigger sources.
- `domain.PreReviewEvidence` (`pre_review_evidence.go`) — versioned
  (`pre-review-evidence/v1`), typed check records with statuses, severity
  ordering and relief rules. The best existing model for "a structured artifact
  a run produced that later decisions depend on".
- `change_log` + DB triggers — CDC for the frontend.

There is no generic "a job produced this structured report" store. Security
findings would need either a new table or reuse of the evidence shape.

## 5. Process isolation and network/file access

- `domain.ProviderRuntimeIsolation` (auto / host / strict) decides **where a
  provider CLI keeps its credentials** — an isolated per-user runtime home vs
  the desktop user's own. Its doc comment is a good warning about isolation
  side effects: a substituted `HOME` on macOS also substitutes the keychain
  domain.
- `internal/reviewgateway` prepares private directories per reviewer.
- `internal/worktree` / `internal/workspace` give a session its own checkout.

**There is no sandbox.** No container runtime, no seccomp/namespace work, no
egress control anywhere in the backend (`grep` for egress/NetworkPolicy returns
nothing). A worker session runs as the daemon's user with the daemon user's
network. AO's own network hard rule is about *inbound* listeners (loopback +
the opt-in LAN listener), not outbound.

## 6. Conclusions that shaped the design

1. **Extend, don't duplicate: RBAC.** `domain.Permission` is the permission
   vocabulary; skill activation gates on it.
2. **Don't extend `skillassets`.** Its clobber-on-boot, no-version contract is
   correct for what it does and wrong for a catalog. The catalog takes a
   sibling directory and leaves it alone.
3. **The minimum extension point is a new leaf package.** Nothing in the
   inventory is a partial skill catalog, so there is nothing to grow — but a
   catalog needs no lifecycle, migration or HTTP change to be real and testable.
4. **No isolation exists, so no capability that needs it may be granted.** The
   inventory's most important finding is the absence of a sandbox. ADR 0002
   already set the precedent for saying so out loud rather than shipping a
   boundary that is not one.
