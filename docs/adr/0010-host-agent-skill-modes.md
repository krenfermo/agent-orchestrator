# 10. Skill modes executed by an agent: host read-only for builtin/trusted packages

Date: 2026-09-23
Status: Accepted (Frente 2 / 2C). Amends the phase-4 note of ADR 0004 for
exactly one capability (`repo.read`) and exactly one kind of package
(builtin or signature-trusted). Everything else in ADR 0004 stands.

## Context

Until 2C a skill mode was carried out by an AO-authored **tool** in the
container runner (ADR 0004): `static-code` greps a staged copy with a command
AO wrote. That proves the boundary, but it does not do what a skill is for: a
skill is instructions (`SKILL.md`, `modes/<mode>.md`) an **agent** reads and
applies. 2C adds that — Claude Code reading the skill's own guide and a copy of
the project, and returning structured findings — without building a second
agent system and without weakening the container boundary.

ADR 0004's phase-4 note concluded that reading needs confinement too, because
"the only thing that would have confined an agent reading a repository outside
a container is the agent CLI's own tool allowlist, and AGENTS.md records that as
void under bypassPermissions". Running the agent inside the container is the
target architecture, but it needs the provider CLI, its credential and model
egress inside the runner — egress the runner does not have (`net.egress` stays
refused). The approved decision for 2C is the **hybrid model**:

- `tool` modes keep running in the Docker runner, unchanged;
- `agent` modes may run **on the host**, only for **builtin or trusted**
  packages;
- untrusted/third-party packages never run an agent on the host.

## Decision

### 1. The contract: `executor: tool | agent` per mode

`ao.skill/v1` gains an optional per-mode `executor`. Absent means `tool`, which
is what every manifest meant before the field existed. An `agent` mode must
request `report.write` (its only product is a report). The builtin
`security-audit` moves to **0.2.0** with `authz-review` as its one agent mode.
The version bump is required: the package digest excludes `skill.yaml`, so an
installation that already had 0.1.0 would otherwise keep the old manifest.

### 2. A host agent attests its own controls, never a container's

`skillagent.Executor` attests four named controls, each demonstrated before it
is claimed:

| Control | What AO does |
| --- | --- |
| `staged_read_only_copy` | The in-scope files are copied (manifest deny list and agent-config files excluded, symlinks refused), made read-only (files 0400, dirs 0500), and the copy — never the checkout — is the working directory. |
| `agent_tool_confinement` | Claude Code is launched with `--tools Read,Grep,Glob`, `--restricted`, `--safe-mode`, `--strict-mcp-config`, `--disable-slash-commands`, `--permission-mode dontAsk`, `--permission-prompts none`, `--no-session-persistence`. Never a bypass flag. The resolved binary's `--help` must list every flag, or the executor attests nothing. |
| `scrubbed_environment` | An allowlisted env: HOME, a fixed PATH, locale, TMPDIR and the provider's own credential variables. No `AO_*`, no forge token, no SSH agent, no cloud credential. |
| `tamper_detection` | After the agent exits, the copy must hold exactly the staged files with exactly the staged bytes, and every source file must still hash to what was staged; otherwise the run is refused and its output discarded. |

`skillcatalog.CapabilitySpec` gains `HostAgentControls`, a **separate
alternative** to `RequiresControls`. Only `repo.read` has one. A capability is
satisfied by one complete set, never a mixture. `report.write` needs no
control, as before. Everything else — `deps.read`, `repo.write`,
`process.exec`, `net.egress`, `net.active_scan`, `secrets.read` — has no host
path and is refused on a host agent with a container control named. The
container path is exactly as strict as it was: no container runner attests a
host-agent control.

### 3. Trust is evidence, not a claim

A host agent follows the package's instructions outside a container, so whose
instructions they are is part of the authorization:

- **builtin** means the installed package is **byte for byte** the one this
  binary embeds — every file, `skill.yaml` included, nothing extra
  (`skillcatalog.MatchesBuiltin`). `origin.type: builtin` in a manifest is a
  claim any package can make and is not consulted;
- **trusted** means an install-origin row with `TrustState == trusted` (a
  signature AO verified against a configured root, ADR 0008), not revoked, whose
  `skill.yaml` still hashes to the manifest digest that was verified.

`skillcatalog.Authorize` takes a `PackageTrusted` flag and, for a host-agent
environment, refuses every control-requiring capability of an untrusted package
(`package_not_trusted`). The zero value is untrusted. The service refuses an
untrusted package before acceptance (`403 SKILL_AGENT_UNTRUSTED`, no run).

### 4. Instructions and data travel in different channels

The system prompt holds only AO's rules, then the verified `SKILL.md` and mode
guide. Project content is never quoted into a prompt; the task message lists
the staged file names, and the agent reaches content only through its read
tools. AO's rules tell the agent that repository content is data and that an
embedded instruction is reported as a `Prompt injection:` finding. None of this
is the boundary — the tools that cannot write, execute or leave the copy are,
and the output gate below is.

### 5. Output is untrusted until AO has checked it

Claude Code returns the report through `--json-schema` (a copy of the canonical
schema without the top-level `$schema`/`$id`, which its validator cannot
resolve). AO then, in order:

1. validates the bytes against the **package's own** `findings.v1.json` —
   which must be byte-identical to AO's canonical contract — with a strict
   validator that interprets the schema file itself, rejects duplicate keys,
   trailing data and unknown properties, and refuses any schema keyword it does
   not implement (`internal/skillreport`);
2. overwrites what only AO knows (project, mode, skill version, times), drops
   any `target`/`authorizationRef`/`commit` the agent claimed, requires every
   cited path to be a file AO staged, and appends its own coverage (what it
   excluded before staging) and notes (how the run executed);
3. **redacts** every string: well-known credential shapes, credential
   assignments, URL passwords, and every literal credential AO harvested from
   the staged files itself — plus its 8+ character prefixes, hex digests and
   base64;
4. validates again, and only then stores the report with its SHA-256, in the
   same transaction as its findings rows (2B).

An error message that quotes the output is redacted before it is recorded.

### 6. Codex is not a provider here

Codex's `read-only` sandbox forbids writes but permits reads across the disk, so
a Codex agent could read the daemon user's home — the exact exposure the staged
copy exists to prevent. It is refused rather than wired with a weaker claim.

## Evidence (measured on this machine, Claude Code 2.1.281)

- Without `--restricted`, a `Read` of an absolute path outside the working
  directory **succeeded** and returned a canary. With `--restricted`, `Read`,
  `Grep` and `Glob` of the same path were denied and recorded in
  `permission_denials`. `--restricted` is therefore load-bearing, and its absence
  makes the executor attest nothing.
- A real `authz-review` run over a project with planted instructions in a source
  comment and in `CLAUDE.md` returned a schema-valid report with the real IDOR
  and authentication findings **and** one `Prompt injection:` finding per plant;
  the project stayed byte-identical and no requested file was written.

## What this does NOT claim

- **It is not a container.** The agent is a process of the daemon's user. A
  compromise of the Claude Code CLI, or a CLI bug in `--restricted`, is not
  contained by anything AO adds; tamper detection would catch a write, not a
  read.
- **Network.** The agent reaches its model provider; that is how it works. It
  has no web tool, and no other egress is offered, but none is enforced at the
  network layer either.
- **Reads.** Confinement of reads to the copy is the CLI's `--restricted`
  behaviour, verified at boot by flag presence and by the live tests, not by an
  OS boundary.
- **Revocation of a trust root after install** is not re-evaluated here beyond
  the origin row's own revocation.

These are the residual risks accepted for builtin/trusted packages only, and
the reason untrusted packages cannot use this path. The target architecture —
the agent inside the runner — removes the first three once the runner has a
controlled egress to the model provider.

## Consequences

- `authz-review` runs as a durable 2B run with `tool = ao.skill-agent/v1` and
  `runner_id = host-agent/claude-code`. No migration: `skill_runs` already
  stores free-text tool/runner fields and the report as bytes.
- A dead daemon's agent is reaped by its pid file (only if its working
  directory is still that run's copy), and the copy removed.
- The dry run answers an agent mode against the host agent and the package's
  verified trust, the same two values a real run is authorized against.
