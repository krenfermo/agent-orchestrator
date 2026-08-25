# agent-orchestrator rewrite docs

The agent-orchestrator is being rebuilt as a long-running Go backend daemon
(`backend/`) plus an Electron + TypeScript frontend (`frontend/`). The backend
supervises coding-agent sessions and exposes daemon control, project/session
state, terminal streaming, and CDC/event infrastructure.

Start with [architecture.md](architecture.md) for the current backend model and
[cli/README.md](cli/README.md) for the CLI surface.

## Reference docs

| Doc                                                    | What it covers                                                                                                        |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| [architecture.md](architecture.md)                     | Current backend model, package layout, status derivation, persistence/CDC, and load-bearing rules.                    |
| [backend-code-structure.md](backend-code-structure.md) | Package ownership rules for the Go backend: domain, services, ports, adapters, storage, HTTP, CLI, and daemon wiring. |
| [cli/README.md](cli/README.md)                         | CLI commands and daemon control surface.                                                                              |
| [cloud-development.md](cloud-development.md)           | Optional private checkout workflow, current Cloud foundation, remaining implementation, and recommended build order. |
| [cloud-refactor.md](cloud-refactor.md)                 | Public contracts, generated Cloud schema types, typed client, reusable product UI, and private implementation boundaries. |
| [code-graph.md](code-graph.md)                         | Provider-agnostic code-graph adapter boundary, the native AST/symbol indexer, hash-gated incremental updates, and per-project index isolation under `~/.ao`. |
| [development.md](development.md)                       | Prerequisites, build steps, running tests, and troubleshooting for local development.                                 |
| [STATUS.md](STATUS.md)                                 | What is shipped on `main` today and what is still in flight.                                                          |
| [workflow-lifecycle-mapping.md](workflow-lifecycle-mapping.md) | Canonical mapping from persisted workflow-engine tables to the UI lifecycle vocabulary; contract for the backend projection and workflow UI. |
| [stack.md](stack.md)                                   | Accepted library/runtime choices, pending stack decisions, and dependencies explicitly avoided for V1.                |
| [telemetry.md](telemetry.md)                           | Telemetry collection, privacy safeguards, and configuration.                                                           |
| [posthog-cost-controls.md](posthog-cost-controls.md)   | PostHog event-name migration, ingestion drop rules, and dashboard queries for reducing telemetry spend.              |
| [project-memory-baseline.md](project-memory-baseline.md) | Phase 0 project-memory baseline: the per-dispatch context/token evidence schema, its measured/estimated/unavailable labeling rule, and the harness that produces it. |
| [project-memory-store.md](project-memory-store.md) | Durable project memory: the item schema and its provenance fields, where items live under `~/.ao`, the content-hash idempotent upsert, staleness against HEAD/file hashes, and ingesting the baseline evidence. |

## Mental model

Persist durable facts, derive display status:

- session table: `activity_state`, `is_terminated`, identity, metadata
- PR tables: PR/CI/review facts
- derived read model: `service.Session` computes display status from session + PR facts
