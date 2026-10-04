# 12. Project memory: the in-tree subsystem is the core; external graph tools are optional read-only sources

Date: 2026-09-24
Status: **Proposed** (Frente 3 / 3A). Needs user approval before it becomes
Accepted. Q1 below stays an open question after acceptance.

## Context

Frente 3 was framed as "integrate Grae/Graphify to give AO a project memory".
The 3A discovery ([docs/frente3/01-discovery-report.md](../frente3/01-discovery-report.md))
found that AO already ships the core:

- a durable, incremental, generation-fenced code graph in AO's SQLite (0153);
- durable project memory with authority, provenance, evidence class and drift
  (0144/0146/0157);
- role-scoped budgeted context packs behind one provider-neutral seam
  (`projectmemory.Provisioner` → `ContextPack`);
- an operator UI and CLI;
- a token ledger.

Graphify is not referenced anywhere in the repository except as prose.
"Grae" matches no identifiable project.

Evidence that bears on the decision:

- Graphify-Labs/graphify has four properties that conflict with rules this
  repository already enforces:
  - it writes its state inside the repository and `~/.cache`, against the
    `~/.ao` hard rule;
  - it needs a Python runtime with ~30 native grammars;
  - its schema churns (near-daily releases);
  - it lacks the generation/authority/provenance metadata that
    `project-memory-authority.md` §19 requires of any adapter.
- None of the Frente 3 bottlenecks found in 3A is in the graph. They are:
  - worker dispatch bypasses the memory decorators;
  - agent-side consumption is unobservable;
  - indexing hygiene and security gaps exist;
  - Java is not covered.
- Java is the one capability gap. It is verified in two user projects
  (`sige`, `ws_sigeseguros_crm`).

## Decision

1. **The in-tree `projectmemory` + `codegraph.Index` subsystem is the core and
   the only source of truth AO serves from.** It is itself a cache over the
   repository: the repository at a commit is authoritative.
2. **No external graph tool becomes a core dependency or a source of truth.**
   An external tool may only ever be a **read-only source**:
   - it is imported as `derived`/`inferred` facts behind the existing
     `MemoryGraph`/`TeeGraph` port;
   - it runs headless on a staged copy, with output under `~/.ao`;
   - it is reported under its real backend name.
3. **No new database.** Memory and graph stay in AO's SQLite.
4. **Frente 3 proceeds by correcting and wiring what exists, then measuring,
   before enabling anything**, as set out in docs/frente3/07-roadmap.md.

## Open question (not decided by this ADR)

**Q1. Java coverage.** The choice is between a native Go extractor (the same
technique as `extract_ts.go`) and a read-only Graphify adapter. It is decided
by the coverage mini-benchmark in `docs/frente3/06-benchmark-plan.md` §6, and
recorded here as an amendment.

## Consequences

- No Python or tree-sitter runtime enters the product now.
- Graphify stays reachable through an existing port if Q1 or future evidence
  needs it, with no lock-in.
- The value of project memory is still **unproven** for agent consumption.
  This ADR does not claim token savings. That is 3D's job, and a NO-GO there
  closes Frente 3 without enabling memory.
