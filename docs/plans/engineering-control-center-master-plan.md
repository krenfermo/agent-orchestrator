# Engineering Control Center — plan maestro

Estado: propuesta de arquitectura, sin implementación  
Fecha de auditoría: 2026-08-13  
Base auditada: `8732c455970587bcd46fc1353bc5d1314f54eb37` en `feat/engineering-control-center`  
Remotos: `origin` = fork `krenfermo/agent-orchestrator`; `upstream` = `Untrivial-ai/agent-orchestrator`

## 1. Objetivo y límites

Este documento define cómo evolucionar Agent Orchestrator (AO) hacia una consola personal de ingeniería para un único administrador, conservando dos capacidades complementarias:

1. **Engineering Intelligence / Observatorio:** evidencia técnica de múltiples repositorios y desarrolladores, inicialmente desde GitHub y posteriormente desde Plane.
2. **AI Development Orchestrator:** ejecución durable del ciclo Planner → Worker → Reviewer → corrección → verificación → siguiente tarea, inicialmente con Codex como worker y Claude Code como reviewer.

Límites explícitos:

- No hay multitenancy, organizaciones internas, roles, permisos por usuario ni aislamiento entre tenants.
- No se capturan teclado, mouse, pantalla, presencia ni actividad ajena a evidencia técnica.
- Las fuentes permitidas son GitHub, Plane y los hechos del propio AO.
- La auditoría es local y se basa en código y documentación del repositorio; no presupone comportamiento no conectado en runtime.
- Este plan no implementa ninguna fase.

Documentos leídos completos: `AGENTS.md`, `README.md`, `DESIGN.md`, `docs/architecture.md`, `docs/backend-code-structure.md`, `docs/development.md`, `docs/STATUS.md` y `docs/stack.md`. También se inspeccionaron los paquetes backend, renderer Electron, migraciones, queries, stores, DTOs, rutas, registros de adaptadores y wiring del daemon.

## 2. Conclusión ejecutiva

La base es altamente reutilizable para la ejecución y supervisión de agentes, y parcialmente reutilizable para Engineering Intelligence.

- **Muy reutilizable:** daemon local single-user, puertos/adaptadores, proyectos, sesiones, worktrees, runtimes, terminal, Chat durable, PR/CI/reviews ligados a sesiones, lifecycle, SQLite, CDC/SSE, notificaciones, usage, auto-review y cambio durable Claude Code↔Codex.
- **Reutilizable con extensión:** proveedor GitHub, autenticación, normalización SCM, polling con ETag, rate-limit cooldown, lectura de checks/reviews, frontend tipado y shell Electron.
- **No resuelto:** observación histórica y global de repositorios, ramas y commits no pertenecientes a sesiones AO; identidad de desarrolladores; archivos por commit/PR como dataset consultable; correlación con Plane; resúmenes ejecutivos fundamentados; workflow durable que gobierne automáticamente todo el ciclo y políticas automáticas de failover/cooldown/retry.

La arquitectura recomendada es **(b) módulos nuevos dentro del daemon actual**, en un bounded context de intelligence con tablas, puertos, observer y endpoints propios. Los datos globales no deben introducirse en `sessions`, `pr`, `pr_checks` ni `pr_comment`, porque esas tablas modelan propiedad y reacción operacional de una sesión AO. Un servicio complementario no está justificado para el producto local single-user en esta etapa.

## 3. Inventario técnico actual

### 3.1 Plataforma y límites

| Área | Implementación actual | Relevancia |
|---|---|---|
| Backend | Go 1.25.7, daemon de larga vida | Lugar natural para observers, políticas y persistencia |
| API | `net/http` + chi, contratos code-first | Extensible mediante servicio + controlador + OpenAPI |
| Desktop | Electron + React 19, TanStack Query/Router, Tailwind/shadcn | Cliente delgado reutilizable para dashboard |
| Persistencia | SQLite WAL, `database/sql`, `modernc.org/sqlite` | Adecuada para una consola personal y polling local |
| SQL | Queries manuales + sqlc | Mantener para tablas de intelligence |
| Migraciones | goose SQL, append-only | Agregar migraciones; nunca editar las existentes |
| Eventos | triggers SQLite → `change_log` → poller → broadcaster → SSE | Reutilizable como invalidación de vistas |
| Git | CLI real mediante `os/exec` | Reutilizable para worktrees y evidencia local AO |
| Terminal | tmux en Darwin/Linux; conpty en Windows; WebSocket `/mux` | Ya soporta agentes de larga duración |
| Estado local | Todo bajo `~/.ao` o `AO_DATA_DIR` | Debe conservarse |
| Red | listener primario loopback; LAN opt-in autenticado para mobile | No ampliar exposición para intelligence |

### 3.2 Arquitectura backend relevante

La arquitectura real mantiene el flujo `OBSERVE → UPDATE durable facts → DERIVE/ACT`:

- `internal/domain`: vocabulario y hechos durables.
- `internal/ports`: contratos estrechos hacia sistemas externos.
- `internal/service/*`: casos de uso y read models para controladores.
- `internal/session_manager`: mutaciones multi-paso de sesiones, provisión y recuperación.
- `internal/lifecycle`: write path canónico para hechos de lifecycle y reacciones SCM.
- `internal/observe/*`: loops de observación.
- `internal/storage/sqlite`: migraciones, queries, stores y transacciones.
- `internal/cdc`: lectura ordenada de `change_log` y fan-out.
- `internal/httpd/controllers`: traducción HTTP; no negocio.
- `internal/adapters/*`: hojas que implementan puertos.
- `internal/daemon`: composition root y supervisión de loops.

Reglas que también deben gobernar el fork:

- Persistir hechos; derivar estados de UI y conclusiones.
- Un error de probe no demuestra ausencia o muerte.
- Los observers no deben inventar eventos terminales ante fallos de red.
- Los adaptadores no deben importar servicios/core.
- CLI y frontend siguen siendo clientes del daemon.
- CDC nace en triggers, no en emisiones manuales paralelas.
- No introducir infraestructura distribuida para un proceso local single-user.

### 3.3 Capacidades de ejecución y agentes

- Registro de **26 adaptadores worker/TUI** en `internal/adapters/agent/registry`.
- Drivers Chat nativos para Codex, Claude Code, OpenCode, Droid y Kimchi.
- Codex Chat usa app-server nativo; Claude Code usa `claude-agent-acp` sobre el binario/autenticación del usuario.
- Una sesión conserva exactamente un controller activo (`tui` o `chat`) y soporta handoff durable entre interfaces para Codex y Claude Code.
- Conversaciones Chat persistentes: turns, mensajes, actividades, approvals, input estructurado, uso, compaction, rollback, branches, provider events y fencing por generación.
- Runtimes recuperables, reaper conservador, mensajes mode-aware, worktree por sesión y shell terminals separados.
- Orchestrator de proyecto con prompt de coordinación, comandos `ao spawn`, `ao send`, inspección de sesiones y delegación.
- `POST /orchestrators/delegate` crea un worker; el orchestrator actual sigue siendo un agente coordinador, no un scheduler durable de workflows.

### 3.4 Adaptadores solicitados

| Capacidad | Implementación | Estado y límites |
|---|---|---|
| GitHub SCM | `internal/adapters/scm/github` | Activo; PR, CI, reviews, mergeability, merge y resolve-comments |
| GitHub tracker | `internal/adapters/tracker/github` | Activo para `Get/List`; intake opt-in de issues GitHub está conectado |
| GitLab | `internal/adapters/scm/gitlab`, `tracker/gitlab`, `multi` | Existe aunque no es requisito inicial; demuestra neutralidad de puertos |
| Codex worker | `internal/adapters/agent/codex` | Launch/restore/hooks/auth/activity/continuation/transcript |
| Claude Code worker | `internal/adapters/agent/claudecode` | Launch/restore/hooks/auth/activity/continuation/transcript |
| Codex Chat | `internal/adapters/chatdriver/codexappserver` | Usage y rate limits tipados; conversación durable |
| Claude Chat | `internal/adapters/chatdriver/claudeacp` | ACP; no equivale a telemetría completa de rate limits Codex |
| Reviewers | `internal/adapters/reviewer/*`, `internal/review` | 25 adaptadores; runs por PR/SHA/harness; pane reusable/restorable |
| Claude reviewer | `adapters/reviewer/claudecode` | Herramientas read-only allowlisted; deny de writes/push/commit |
| Codex reviewer | `adapters/reviewer/codex` | Sandbox `read-only`; submit mediante AO/gh autorizado |
| Worktrees | `adapters/workspace/gitworktree` | Create/restore/destroy/preserve; multi-repo workspace; no force-delete dirty |
| Runtime | `adapters/runtime/tmux`, `conpty`, `runtimeselect` | Long-running, attach, liveness y generaciones |
| Terminal | `internal/terminal`, `/mux` | PTY por cliente y shell terminals |
| PR actions | `service/pr`, `adapters/scm/*/merge_action.go` | Merge con expected-head CAS y resolución de comentarios |
| Persistence | `storage/sqlite` | SQLite/sqlc/goose; hechos operacionales y conversación |
| Events/CDC | `cdc`, `change_log`, SSE `/api/v1/events` | Invalidación durable; catálogo de event types estrecho |
| Usage | `observe/usage`, `service/usage`, tablas `usage_*` | Tokens Codex/Claude y fuentes JSONL; no es scheduler de cuotas |
| Auto-review | `internal/autoreview` | Sweep y trigger por nuevo head; no completa automáticamente el ciclo de corrección |
| Agent switching | `session_manager/agent_switching.go` | Saga durable Claude Code↔Codex, idempotencia, handoff, fencing y recovery |

### 3.5 Observers conectados

- **SCM:** cada 30 s; ETag de lista de PRs y checks, batch GraphQL, refresh de reviews más lento, límite de staleness, diff semántico, cooldown ante rate limit.
- **Runtime reaper:** liveness de runtime; no interpreta probe fallido como muerte.
- **Activity:** reconcilia señales/hook/terminal.
- **Usage:** descubre e ingiere JSONL nativo de Codex y Claude, con cursor, retry y anomalías.
- **Tracker intake:** cada minuto, opt-in por proyecto; lista issues GitHub elegibles y crea una sesión por issue.
- **Auto-review:** decide si un nuevo PR head necesita un pass del reviewer.

Existe drift documental: `docs/STATUS.md` todavía describe el tracker lane como no conectado, pero `daemon.go`, `tracker_intake_wiring.go` y `observe/trackerintake` demuestran intake GitHub en runtime. El plan debe confiar en código + tests y corregir documentación en un checkpoint futuro, sin usar esa corrección como excusa para mezclar cambios.

## 4. Qué información de GitHub obtiene AO hoy

### 4.1 Datos remotos persistidos actualmente

Para PRs atribuidos a sesiones AO:

- Repositorio/provider/host y URL canónica.
- Número, título, autor, branch source/target.
- Head SHA, base SHA y merge commit SHA.
- Estado draft/open/merged/closed y timestamps de create/update/merge/close.
- Additions, deletions y número de changed files.
- Mergeability normalizada y valores raw del proveedor.
- CI aggregate y checks individuales: nombre, SHA, estado, conclusión, URL, details y tail acotado de logs fallidos.
- Review decision.
- Reviews submitted: id, autor, estado, URL, body, bot, target SHA y timestamp.
- Review threads/comments: path, line, resolved, bot, autores, bodies y URLs, con ventanas acotadas.
- Hashes semánticos y timestamps de observación para evitar escrituras/nudges duplicados.

El observer descubre PRs por repositorio, pero **sólo los atribuye a una sesión viva** cuyo branch coincide exactamente o es un descendiente stacked del branch de sesión; también filtra por identidad autenticada cuando puede resolverla. Por tanto, no es un observatorio del repositorio completo.

### 4.2 Datos locales AO disponibles sin GitHub adicional

- Branch, path y head SHA del worktree.
- Hasta 500 cambios dirty/staged/untracked y hasta 20 commits recientes en snapshots de handoff.
- Browser de archivos/diff contra base, limitado a 5,000 archivos y diffs acotados.
- Changed files por turn de Chat cuando el proveedor los reporta.
- Activity state y último timestamp por sesión.
- Uso de tokens/modelos Codex/Claude y rate limits actuales reportados por Codex Chat.

Estos datos son útiles para trabajo ejecutado dentro de AO, pero no constituyen historia global del equipo.

### 4.3 Información adicional necesaria

| Requisito | Estado actual | Gap exacto |
|---|---|---|
| Repositorios conectados | `projects` requiere checkout/proyecto AO | Catálogo remoto independiente y estado de sync |
| Ramas activas | Branch de cada sesión y source branch de PR | Lista global, head, default/protected, first/last seen, deleted/stale |
| Commits | Head/base/merge SHA; 20 commits locales sólo para handoff | Commit stream durable, autores, timestamps, mensajes y relaciones con branches/PRs |
| Archivos modificados | Conteo PR, paths en review, diff local AO | Archivos por commit/PR durable con status y stats |
| Actividad reciente | Sesiones/PR update | Timeline normalizado cross-repo por actor y tipo de evidencia |
| CI | Checks sólo en PRs AO | Workflow runs/check suites/jobs globales y asociación con SHA/branch/PR |
| Tests | Inferibles por nombre de check/log | Modelo explícito cuando GitHub expone reportes; si no, evidencia “check de tests” heurística y marcada |
| Reviews | Sólo PRs AO, ventana acotada | Reviews globales, requests, estados y atención por repo/persona |
| Merges | Sólo PRs AO | Merges globales y relación con commit/autor |
| Última actividad | Por sesión/PR | `last_evidence_at` derivado por repo, persona, branch, PR y tarea |
| Desarrolladores | Autor login en PR/reviews | Identidad estable y aliases; no métricas invasivas |
| Plane | Inexistente | Adaptador, sync, issues y links evidence↔task |
| Resúmenes IA | Conversación de agentes, sin dataset ejecutivo | Ventanas de evidencia, citas/links, cache y provenance |

No se debe presentar “tests disponibles” como hecho cuando sólo existe un check con nombre ambiguo. El modelo debe conservar `evidence_type`, `source`, URL y nivel de confianza.

## 5. Modelo SQLite actual relevante

### 5.1 Proyectos

`projects` contiene `id`, path local, origin URL, display name, timestamps, `kind` y config JSON tipada. `workspace_repos` agrega child repositories, origin, default branch y git status; `session_worktrees` liga sesiones con worktrees por repo.

Reutilización: identidad operativa de repos clonados y configuración por proyecto. Límite: un repo remoto observado no necesariamente debe ser un proyecto ejecutable/local.

### 5.2 Sesiones y actividad

`sessions` contiene identidad, project/issue/kind/harness/reviewer, mode, `activity_state`, `activity_last_at`, `first_signal_at`, `is_terminated`, branch/workspace/runtime/native conversation/generation, prompts/últimos updates, preview, políticas auto-review/auto-inject/merge y timestamps.

El status mostrado nunca se persiste: se deriva de activity + termination + PR facts. Esta regla debe mantenerse. Para intelligence, “bloqueado” también debe ser una inferencia explicable, no una etiqueta mutable sin evidencia.

### 5.3 Pull requests, CI y reviews

- `pr`: una PR pertenece obligatoriamente a una sesión; incluye lifecycle, author, branches, SHA, stats, raw provider states, hashes y observed timestamps.
- `pr_checks`: check por PR/name/commit con status, conclusion, URL/details/log tail.
- `pr_reviews`: reviews submitted por PR.
- `pr_review_threads`: threads y resolución.
- `pr_comment`: comentarios de review normalizados.
- `review` y `review_run`: reviewer panes y passes por session/PR/SHA/harness, verdict, body, delivery y trigger manual/auto.

Reutilización: ciclo operacional de una sesión. Límite decisivo: `pr.session_id NOT NULL`; no sirve como almacén global sin violar el dominio.

### 5.4 Conversaciones, agent switching y checkpoints existentes

- `conversations`, `conversation_turns`, `conversation_messages`, `conversation_activities`, `conversation_provider_events` y `conversation_branches` guardan el estado Chat.
- `session_interface_transitions` + outbox guardan handoff TUI↔Chat.
- `agent_native_sessions` conserva conversaciones nativas de provider.
- `agent_switches` es una saga durable con fases, generaciones, target start mode, acknowledgement, error code y artefactos de handoff.
- `session_cleanup_facts` conserva cleanup por generación.

Esto es una base fuerte para checkpoints técnicos, pero todavía no existe un `workflow_run` que represente Planner → Worker → Reviewer → Fix → Verify como state machine de producto.

### 5.5 Usage y eventos

- `usage_bindings`, `usage_sources`, `model_usage_events` y auxiliares conservan fuentes/cursors/anomalías y tokens.
- `change_log` es append-only y recibe eventos por triggers.
- El enum/check de `change_log.event_type` es estrecho. Agregar nuevos tipos requiere una migración cuidadosa o reutilizar una invalidación existente sólo cuando semánticamente sea correcto.

No se deben emitir manualmente eventos desde stores. Las nuevas tablas observables deben tener triggers y payloads compactos; la API vuelve a leer el read model completo.

## 6. Capacidades actuales que ya resuelven requisitos

| Requisito objetivo | Cobertura actual | Evaluación |
|---|---|---|
| Múltiples repos privados | Múltiples projects/workspace repos; token env/`gh auth token` | Parcial; centrado en checkouts AO |
| Branch por tarea | Branch/worktree aislado por sesión | Completo para trabajo AO |
| PR/CI/reviews/merge | Observer + storage + UI + actions + nudges | Completo para PRs atribuidos a sesiones |
| Archivos modificados | Workspace file API/diff y turn diffs | Completo para sesión activa, no histórico global |
| Última actividad | Activity facts + timestamps + PR observed times | Completo para sesiones, parcial global |
| Reviewer Claude | Adapter, engine, persisted runs, restore | Ya disponible |
| Worker Codex | TUI/Chat, hooks, restore, usage | Ya disponible |
| Corrección por feedback | Lifecycle inyecta CI/review/merge conflicts | Disponible como nudge, no loop stateful completo |
| Auto review | Policy + sweep + idempotencia por head SHA | Disponible |
| Failover Codex→Claude | Manual durable agent switch bidireccional | Mecanismo disponible; falta política automática |
| Usage/rate limits | Usage durable para Codex/Claude; rate-limit snapshot Codex Chat | Parcial y no unificado como health de agente |
| Retry/cooldown | SCM cooldown; usage source retry; tracker backoff | Patrones disponibles, no política común de ejecución |
| Checkpoints/restart | Chat, interface transitions y agent-switch sagas | Excelente base; falta workflow checkpoint global |
| Operación larga | daemon, runtimes, observers y recovery | Base disponible; falta scheduler de objetivos |

Estimación arquitectónica, no métrica de líneas: se puede reutilizar aproximadamente **70–80% del control plane de orquestación** y **35–45% del pipeline de Engineering Intelligence**. La UI shell, API, storage primitives y GitHub client son reutilizables; el dataset histórico global y sus read models son nuevos.

## 7. Gaps de producto y arquitectura

### 7.1 Engineering Intelligence

- El sujeto de observación actual es la sesión AO, no el repositorio ni el desarrollador.
- No hay historial durable de commits/branches globales.
- No hay modelo de identidad/aliases para personas.
- No hay dataset temporal uniforme de evidencia.
- No hay sync cursors durables por stream/repo; los ETags SCM viven en memoria.
- No hay conexión remota de repos independiente de checkout local.
- No hay summaries fundamentados ni almacenamiento de provenance.
- No hay Plane ni tabla de enlaces tarea↔evidencia.
- La UI está organizada alrededor de sesiones/Kanban, no alrededor de repos/personas/tiempo.

### 7.2 Orquestación autónoma

- El Planner es hoy el orchestrator agent guiado por prompt, no una state machine durable.
- No existe entidad de objetivo, run, step, dependency, attempt o checkpoint de workflow.
- Auto-review no crea por sí mismo una tarea de corrección ni verifica su resultado.
- Agent switching sólo acepta un cambio explícito y sólo workers TUI Claude/Codex; no es failover automático por health.
- No hay health/cooldown común por harness/account/model.
- No existe clasificador durable de errores `rate_limited`, `auth`, `transient`, `tool`, `test_failed`, `review_changes_requested`.
- No hay budget de intentos, backoff configurable o circuit breaker por agente.
- No hay política de reanudación completa después de reinicio para un workflow multi-step, aunque sí para varias sagas internas.

## 8. Evaluación de ubicación de Engineering Intelligence

### Opción A — dentro del daemon y mezclado con módulos actuales

Ventaja: mínima infraestructura. Desventajas: contaminaría `session`, `pr` y lifecycle con hechos que no pertenecen a sesiones; rompería ownership y haría conflictivas las actualizaciones de upstream.

**Decisión: no recomendada.**

### Opción B — módulos nuevos del daemon

Crear un bounded context independiente que use el mismo proceso, SQLite, broadcaster, API y Electron:

```text
backend/internal/
  domain/                 # sólo tipos compartidos mínimos nuevos
  ports/                  # RepositoryIntelligenceSource, PlaneSource
  service/intelligence/   # read models y casos de uso
  observe/intelligence/   # polling/sync y backoff
  storage/sqlite/         # nuevas migraciones/queries/stores
  adapters/intelligence/github/
  adapters/intelligence/plane/   # futuro
  httpd/controllers/      # controlador delgado
```

Ventajas: reutiliza operación local; transacciones y CDC; credenciales ya resueltas; una sola lifecycle de daemon; frontend tipado. El aislamiento se obtiene por paquetes, puertos, tablas y endpoints, no por otro proceso.

**Decisión: recomendada.**

### Opción C — servicio complementario

Sería justificable si aparecen ingestión de webhooks públicos, varios administradores, procesamiento masivo, acceso remoto permanente o necesidad de escalar independientemente. Hoy implicaría duplicar secretos, config, SQLite/IPC, supervisión, logging, empaquetado, health, updater y autenticación.

**Decisión: diferir.** Definir puertos provider-neutral permitirá extraer el módulo más adelante si datos reales lo exigen.

## 9. Arquitectura objetivo

```text
GitHub API ──> GitHub Intelligence Adapter ──> Intelligence Sync Observer
                                                     │
Plane API ───> Plane Adapter (futuro) ────────────────┤
                                                     ▼
                                           SQLite intelligence_* facts
                                                     │
                        AO session/PR facts ──> Evidence projection/linker
                                                     │
                                  triggers → change_log → SSE invalidation
                                                     │
                                                     ▼
                          Intelligence Service / Summary Service / Workflow Engine
                                      │                    │
                                      ▼                    ▼
                              REST read models       Session Manager / Review Engine
                                      │
                                      ▼
                              Electron Observatory
```

Principios:

1. **Separar source facts, links e inferencias.** Un commit es un hecho; “parece bloqueado” es una inferencia con reasons y evidence ids.
2. **Idempotencia por identidad externa.** Cada entidad remota tiene provider + host + repo + external id/SHA.
3. **Sync incremental durable.** Cursors/ETags/watermarks sobreviven reinicios.
4. **Read-only por defecto.** GitHub/Plane writes son capacidades explícitas posteriores; observatorio no cambia repos ni tareas.
5. **No guardar tokens.** Reusar `AO_GITHUB_TOKEN`/`gh` y, para Plane, keychain/env o broker del desktop; la DB sólo guarda estado de conexión no secreto.
6. **Attribution prudente.** Login/autores y aliases explícitos; no deducir identidad humana sólo por nombre parecido.
7. **Resúmenes con evidencia.** Todo claim incluye ids/URLs y ventana temporal; ausencia de evidencia no significa ausencia de trabajo.
8. **Privacidad por diseño.** Sólo metadata técnica necesaria, retención configurable y sin telemetría personal.

## 10. Estrategia para mantener compatibilidad con upstream

### 10.1 Superficies que no se deben modificar conceptualmente

- Semántica de `sessions`, activity, status derivado y termination.
- Ownership actual `pr → session` y feedback routing.
- `session_manager` como dueño de recursos de sesión.
- Lifecycle como write path de hechos operacionales de sesión.
- Adaptadores existentes de Codex/Claude/runtime/worktree/reviewer salvo extensiones de interfaces demostradas por más de un consumidor.
- Listener loopback y reglas del listener LAN.
- CLI thin-client y frontend thin-renderer.
- Migraciones ya mergeadas y archivos `storage/sqlite/gen/*` editados a mano.
- Cadena code-first OpenAPI y generación `schema.ts`.
- Directorio de estado `~/.ao`.
- CDC por triggers.

### 10.2 Estrategia de cambios

- Mantener `upstream` como remoto canónico y rebase/merge frecuente en checkpoints pequeños.
- Preferir archivos nuevos bajo `service/intelligence`, `observe/intelligence` y `adapters/intelligence`.
- Usar tablas `intelligence_*`/`workflow_*`; no agregar columnas de intelligence a `sessions` o `pr` salvo que el concepto sea compartido de verdad.
- Aceptar como touch points inevitables y pequeños: `daemon` wiring, router/controller registration, DTO/spec registry, navegación frontend y archivos generados.
- Un checkpoint/PR por vertical; no mezclar refactors upstream con features del fork.
- Añadir migration nueva (la siguiente libre al momento de implementar; hoy sería posterior a `0093`) y regenerar sqlc/OpenAPI.
- Mantener un puente explícito `intelligence_pull_request ↔ AO session/pr`, sin hacer de ninguna tabla una copia oculta de la otra.
- No renombrar paquetes upstream ni mover archivos para “ordenar” el fork.
- Ejecutar gates del área y luego `go test ./...`, `npm run frontend:typecheck`; para contrato, `npm run api`; para SQL, `npm run sqlc`.

## 11. Modelo de datos propuesto

Los nombres son propuestos; cada grupo debe aterrizar sólo en el checkpoint que lo necesita.

### 11.1 Catálogo y sync

**`intelligence_repositories`**

- `id`, `project_id NULL`, `provider`, `host`, `owner`, `name`, `external_id`
- `default_branch`, `html_url`, `enabled`, `registered_at`, `last_success_at`, `last_error_code`
- unique `(provider, host, owner, name)`

`project_id` enlaza un checkout AO cuando existe; no es obligatorio.

**`intelligence_sync_cursors`**

- `repository_id`, `stream` (`repository`, `branches`, `commits`, `pull_requests`, `reviews`, `checks`)
- `cursor`, `etag`, `watermark_at`, `state`, `failure_count`, `next_retry_at`, `last_error_code`, `updated_at`
- PK `(repository_id, stream)`

### 11.2 Personas

**`intelligence_developers`**: id local, display name opcional, active flag.  
**`intelligence_developer_identities`**: developer id, provider, host, external id/login, avatar URL, first/last seen. Unique por identidad provider. No guardar email privado en V1.

La asociación automática sólo se hace por external id/login exacto; unir aliases distintos requiere confirmación administrativa.

### 11.3 Git

**`intelligence_branches`**: repo, name, head SHA, default/protected, first/last seen, last commit at, deleted at.  
**`intelligence_commits`**: repo, SHA, author identity, committer identity, message/subject, authored/committed/pushed timestamps cuando existan, parent count, stats, URL.  
**`intelligence_branch_commits`**: repo, branch, SHA, first/last seen; permite ramas que comparten commits.  
**`intelligence_commit_files`**: repo, SHA, path, previous path, status, additions/deletions; cargar con presupuesto y on-demand para evitar coste excesivo.

### 11.4 PR, reviews y CI globales

**`intelligence_pull_requests`**: identidad externa, repo, número, author, source/target, head/base/merge SHA, lifecycle, draft, stats, mergeability, timestamps, URL, last observed.  
**`intelligence_pr_reviews`**, **`intelligence_review_threads`**, **`intelligence_review_comments`**: hechos provider-neutral.  
**`intelligence_check_runs`**: repo, external id, SHA, PR opcional, name, workflow/job, status/conclusion, started/completed, URL, test classification y confidence.  
**`intelligence_ao_links`**: entity type/id ↔ project/session/pr URL; mantiene el puente con la operación AO sin cambiar ownership existente.

Los bodies y log tails son datos potencialmente sensibles y voluminosos. V1 debe conservar sólo lo necesario para atención/resumen, con límites equivalentes o más estrictos que SCM actual.

### 11.5 Evidencia e inferencias

**`intelligence_evidence_events`** append-only:

- `id`, `dedupe_key`, repo, developer opcional
- `kind` (`commit`, `branch_update`, `pr_opened`, `pr_updated`, `review_submitted`, `ci_completed`, `merge`, `ao_activity`, `plane_update`)
- `subject_type/id`, `occurred_at`, `observed_at`, `source_url`
- `summary_facts_json` pequeño y versionado

**`intelligence_assessments`**:

- scope, window start/end, kind (`blocked_candidate`, `attention_needed`, `stale_evidence`)
- severity/confidence, reasons JSON, evidence ids, computed at, expires at

No guardar una etiqueta “blocked” como verdad primaria. Se recalcula de hechos y muestra razones.

**`intelligence_summaries`**:

- scope (`global`, repo, developer), window, evidence watermark/hash
- model/provider, prompt version, generated at, summary JSON/text, citations JSON
- status/error, superseded at

Un resumen se reutiliza sólo si el hash de evidencia y prompt version coinciden.

### 11.6 Plane futuro

**`plane_connections`**, **`plane_projects`**, **`plane_issues`**, **`plane_sync_cursors`** y **`plane_evidence_links`**. Los links pueden apuntar a repo, branch, commit, PR, AO session o evidence event. No agregar `plane_issue_id` directamente a todas las tablas Git.

### 11.7 Workflow autónomo futuro

**`workflow_runs`**: objective, project/repo, state, policy snapshot, created/updated/completed.  
**`workflow_steps`**: kind (`plan`, `work`, `review`, `fix`, `verify`, `advance`), dependency, state, assigned harness, session/review ids, expected artifacts.  
**`workflow_attempts`**: step, attempt number, harness/model, start/end, outcome/error class, retry after.  
**`workflow_checkpoints`**: run/step, durable phase, payload version, evidence watermark, created at.  
**`agent_health`**: harness/account/model scope, state, reason, cooldown until, consecutive failures, last success/failure.  
**`workflow_outbox`**: comandos idempotentes pendientes hacia session/review engines.

El workflow no debe copiar transcripts. Referencia sessions, turns, PRs, reviews y evidence ids.

## 12. Integración GitHub propuesta

### 12.1 Reutilizar

- Token precedence `AO_GITHUB_TOKEN` → `gh auth token`.
- Client REST/GraphQL, sanitización de errores y rate-limit types.
- `SCMRepo` parsing y provider-neutral DTO style.
- ETag guards, pagination bounds, semantic hashes y cooldown.
- Normalización de PR/check/review/mergeability.
- `httptest` y clients inyectables; cero red real en unit tests.

### 12.2 Agregar

Un puerto separado, por ejemplo `RepositoryIntelligenceSource`, porque el actual `observe/scm.Provider` está diseñado alrededor de PRs de sesiones:

- `GetRepository`
- `ListBranches`
- `ListCommits(updatedAfter/cursor)`
- `GetCommitFiles`
- `ListPullRequests(updatedAfter/cursor, state=all)`
- `ListReviews/Checks` o batch fetch equivalente
- `AuthenticatedIdentity` y `RateLimitState`

El adaptador puede compartir el client interno GitHub, pero no ensanchar el contrato SCM con métodos que lifecycle de sesiones no necesita.

### 12.3 Estrategia de sync

- Poll incremental con jitter y budgets por repo/stream.
- Watermark durable antes de avanzar cursor sólo después de persistencia transaccional exitosa.
- Full reconciliation periódica acotada para deletes/force-push/closed PRs.
- Backoff por provider y repositorio; respetar `Retry-After`/reset.
- Refresh prioritario de PRs abiertas y branches activas; menor frecuencia para historia cerrada.
- Webhooks se difieren: requerirían endpoint público y cambian el threat model local.
- Import inicial acotado, por ejemplo 30 días configurable, con progreso visible y cancelable.

### 12.4 Seguridad y privacidad

- Scopes read-only mínimos para observatorio.
- Mutaciones PR existentes siguen en su capability/endpoint separado.
- No persistir tokens ni headers.
- Redactar URLs con credenciales/query sensibles en logs.
- Guardar sólo evidencia técnica necesaria y ofrecer retención/purge por repo en fase posterior.

## 13. Futura integración Plane

Plane debe entrar como adapter/observer nuevo, no como sustituto de `Tracker` hasta demostrar contratos comunes. El `Tracker` actual cubre Get/List y bootstrap de workers, pero Plane necesita proyectos, states, assignees, updates y links durables.

Secuencia:

1. Read-only connection/preflight y catálogo de proyectos.
2. Sync incremental de issues y estados.
3. Mapping explícito Plane user ↔ developer identity.
4. Links manuales y automáticos por URL/branch/PR references.
5. Read model “tarea con evidencia reciente”.
6. Sólo después, writes opt-in (comentarios/transiciones) con auditoría y CAS.

La ausencia de commits no prueba falta de trabajo. La UI debe decir “sin evidencia técnica observada en la ventana” y mostrar qué fuentes/ventanas se consultaron.

## 14. Estrategia Codex/Claude

### 14.1 Estado inicial recomendado

- Worker default: Codex.
- Reviewer default: Claude Code mediante `ProjectConfig.Reviewers` o reviewer por sesión.
- Planner: orchestrator de proyecto; en la primera etapa continúa siendo agent-driven.
- Auto-review: reutilizar coordinator + review engine por target SHA.
- Feedback: reutilizar lifecycle auto-inject de CI/review y `ao send`.

### 14.2 Qué no duplicar

- No crear un runner específico Codex fuera de `ports.Agent`/Chat driver.
- No crear un segundo reviewer Claude fuera de `ports.Reviewer`.
- No copiar worktrees para review: el reviewer ya inspecciona el checkout con restricciones read-only.
- No crear un nuevo transcript store: referenciar conversaciones/sessions existentes.

### 14.3 Loop objetivo

El workflow engine debe coordinar capacidades existentes:

1. `plan`: produce steps con acceptance criteria.
2. `work`: crea o reutiliza worker Codex y espera artefacto verificable.
3. `review`: dispara Claude reviewer contra head SHA exacto.
4. `fix`: si changes requested, envía findings al mismo worker o ejecuta failover.
5. `verify`: espera checks/tests definidos y un head estable.
6. `advance`: completa step y desbloquea dependencias.

Cada transición exige hechos, no texto del agente: SHA, review run, check conclusion, test command result estructurado o confirmación explícita del administrador.

## 15. Estrategia de failover

El mecanismo base ya existe: `POST /sessions/{id}/switch-agent` realiza un cambio durable Claude Code↔Codex conservando AO session, terminal identity, worktree, branch, PR ownership y browser. Incluye saga, idempotency key, source/target generation, handoff bounded, ack y recovery.

Lo que se agrega es una **política alrededor del mecanismo**, no otro mecanismo:

- Clasificador provider-neutral de fallo.
- Health por harness/account/model.
- Cooldown durable con `cooldown_until`.
- Budget de retries por step y por agente.
- Orden de fallback configurable; inicial `codex → claude-code`.
- Guardas: nunca failover automático durante approval/blocked, operación Git destructiva, review submit ambiguo o target start ambiguo.
- Un failover sólo se declara exitoso cuando el target generation acepta el continuation y el workflow checkpoint avanza.
- Si Codex reporta rate limit en Chat, usar snapshot/reset. En TUI o Claude, clasificar sólo señales tipadas/conservadoras; no parsear mensajes arbitrarios como verdad sin adapter capability.

Política inicial sugerida:

- Un retry corto en el mismo harness para error transitorio idempotente.
- Rate limit conocido: cooldown hasta reset + jitter; intentar Claude si la tarea lo permite.
- Auth/binary missing: no retry automático; attention requerida.
- Test/review failure: no failover por defecto, es trabajo normal de corrección.
- Dos fallos de provider start/delivery en el mismo step: abrir circuit breaker y pedir atención.

## 16. Checkpoints y reanudación

### 16.1 Qué ya existe

- Sessions y worktrees durables.
- Native conversation ids y generations.
- Chat turns/messages/activities.
- Interface transition saga + outbox.
- Agent switch saga + handoff file/hash + recovery.
- Review runs por SHA.
- Usage cursors y retry state.
- Cleanup facts por generation.

### 16.2 Qué falta

Un checkpoint de workflow debe incluir sólo referencias y decisiones necesarias:

- run/step/attempt ids y policy version.
- project/repo/session/worktree/branch.
- expected base/head SHA y PR URL.
- review run id/verdict/target SHA.
- checks requeridos y último resultado.
- último comando idempotente emitido y acknowledgement.
- retry count, cooldown y next action.
- evidence watermark.

### 16.3 Recovery al arrancar

1. Leer runs no terminales.
2. Reconciliar primero sagas inferiores existentes (interface/agent switch/review/session).
3. Comparar checkpoint con hechos actuales.
4. Si el efecto ya ocurrió, avanzar sin repetirlo.
5. Si es seguro e idempotente, reemitir desde outbox.
6. Si el estado es ambiguo o requiere decisión humana, marcar attention con reason; no asumir éxito ni repetir una mutación.

## 17. Dashboard propuesto

Mantener el Kanban actual para ejecución y agregar una entrada **Observatory** con cuatro vistas:

### Overview

- Cambios de hoy, repos activos, PRs que requieren atención, CI fallando y actividad AO.
- Resumen ejecutivo con links a evidencia.
- Indicador de freshness por fuente y última sincronización.

### Repositories

- Actividad, branches activas, commits, PRs, checks, reviews, merges y última evidencia.
- Estado de conexión/sync y errores no sensibles.

### Developers

- Qué parece estar desarrollando cada persona basado en commits/PR/reviews.
- Evidencia cronológica y repos afectados.
- Sin rankings, productividad scores, horas online ni comparación de volumen.

### Attention

- PRs con CI/review/merge blockers.
- Branches/PRs sin avance técnico en una ventana configurable.
- Tareas Plane sin evidencia reciente, cuando Plane exista.
- Cada item muestra `why`, freshness, confidence y evidencia enlazada.

El dashboard debe distinguir siempre:

- **Observed fact**: hecho remoto/local.
- **Derived signal**: regla determinista.
- **AI summary**: interpretación generada con citations.

## 18. Roadmap incremental por checkpoints

Cada checkpoint es entregable y revertible de forma independiente.

### Checkpoint 0 — Vista baseline sobre hechos AO existentes

- **Objetivo:** validar el bounded context y entregar una primera vista Observatory sin nuevas fuentes ni esquema.
- **Archivos/módulos afectados:** nuevos `service/intelligence`, controlador/DTO/spec; wiring mínimo en daemon/router; nueva route/component/hook frontend; tests. Archivos generados OpenAPI/schema cambian por contrato.
- **Reutilizamos:** project/session services/stores, PR summaries, review/usage reads, status derivado, TanStack Query, shell y SSE como invalidación.
- **Agregamos:** `GET /api/v1/intelligence/overview` con facts AO-only y una página Observatory claramente etiquetada.
- **Migraciones:** ninguna.
- **Endpoints:** sólo `GET /api/v1/intelligence/overview` con filtro opcional `projectId`.
- **Frontend:** overview con repos/proyectos AO, sesiones activas, branches AO, PR attention, CI/reviews, last activity y freshness; estado vacío y disclaimer de cobertura.
- **Pruebas:** unit de ensamblado; controller/contract; route/component; no-network test; API drift.
- **Aceptación exacta:** con fixtures de dos proyectos y sesiones/PRs, el endpoint devuelve conteos y attention correctos; la UI los muestra; indica “sólo actividad gestionada por AO”; no hay migración ni llamada nueva a GitHub; gates backend/frontend pasan.
- **Riesgos:** duplicar lógica de status. Mitigación: consumir read models actuales, no reimplementar precedencia.

### Checkpoint 1 — Catálogo de repositorios y salud de conexión

- **Objetivo:** separar “repo observado” de “project ejecutable” y probar credenciales read-only.
- **Afectados:** domain/ports mínimos, `service/intelligence`, GitHub intelligence adapter, storage/queries/store, controller, settings/Observatory.
- **Reutilizamos:** parser SCMRepo, GitHub client/token source, project origin URLs.
- **Agregamos:** `intelligence_repositories` y sync status; auto-link opcional a project existente.
- **Migraciones:** nueva tabla repositorios + triggers de invalidación.
- **Endpoints:** list/add/enable-disable/remove repos y `POST .../probe`; remove sólo borra intelligence data, nunca checkout AO.
- **Frontend:** conexión, scopes/identity/freshness y selección de repos.
- **Pruebas:** token absent/auth fail/rate limit, duplicate repo, project link, no secret persistence, controller errors.
- **Aceptación:** registrar varios repos privados por owner/name, reiniciar daemon y conservar catálogo; probe usa credenciales existentes; ninguna mutación GitHub.
- **Riesgos:** scopes insuficientes y GitHub Enterprise. Mitigar con capability response y host explícito; limitar V1 a github.com si GHE no se certifica.

### Checkpoint 2 — Ramas y commits incrementales

- **Objetivo:** historial global mínimo por repo y developer identity exacta.
- **Afectados:** intelligence observer/adapter/store/domain; daemon supervisor; API timeline.
- **Reutilizamos:** poll loop, cooldown, pagination bounds, semantic hashing.
- **Agregamos:** branches, commits, branch-commit links, cursors y developer identities.
- **Migraciones:** tablas `intelligence_branches`, `intelligence_commits`, `intelligence_branch_commits`, developers/identities, cursors.
- **Endpoints:** repo branches, commits y activity timeline paginada.
- **Frontend:** repo detail con active/stale branches, commits y last evidence.
- **Pruebas:** pagination/cursor, force-push/deleted branch reconciliation, restart resume, rate limit, idempotencia.
- **Aceptación:** una segunda sync sin cambios crea cero duplicados; reinicio continúa watermark; nueva commit aparece una vez y actualiza last activity.
- **Riesgos:** coste API y branch explosion. Mitigar con ventana inicial, límites y frecuencia adaptativa.

### Checkpoint 3 — PR/reviews/CI globales y archivos bajo demanda

- **Objetivo:** observar todos los PRs relevantes, no sólo los de sesiones AO.
- **Afectados:** intelligence GitHub adapter/observer/store/service y bridge AO.
- **Reutilizamos:** normalización y queries GraphQL actuales, check/review logic y URLs.
- **Agregamos:** tablas intelligence PR/review/check/file y `intelligence_ao_links`.
- **Migraciones:** tablas e índices por repo/state/updated/actor/SHA.
- **Endpoints:** PR list/detail, checks, reviews, changed files; atención derivada.
- **Frontend:** repo PR board/list y detail con CI/reviews/files.
- **Pruebas:** open→merged/closed, review pagination parcial, >100 checks, file budget, cross-fork, AO link.
- **Aceptación:** PR externo a AO aparece; PR AO se enlaza sin duplicar ownership; atención se deriva igual de hechos equivalentes.
- **Riesgos:** dos snapshots divergentes. Mitigar con adapter normalization compartida y bridge explícito; no dual-write manual.

### Checkpoint 4 — Timeline de evidencia y developer view

- **Objetivo:** consulta uniforme por tiempo, repo y persona sin scoring invasivo.
- **Afectados:** evidence projector/service/store/API/frontend.
- **Reutilizamos:** timestamps/identidades de commits, PRs, reviews, checks y AO activity.
- **Agregamos:** evidence events deduplicados y aliases manuales.
- **Migraciones:** `intelligence_evidence_events`; índices por occurred_at/repo/developer; alias metadata.
- **Endpoints:** paginated activity y developer detail; merge/split identity admin actions.
- **Frontend:** Developers + timeline, filtros y provenance.
- **Pruebas:** deterministic projection, exact aliasing, pagination, timezone/day boundary, no volume score.
- **Aceptación:** “qué cambió hoy” se responde determinísticamente desde eventos y cada fila enlaza a fuente.
- **Riesgos:** atribución errónea. Mitigar con exact matching y confirmación para merges.

### Checkpoint 5 — Señales de atención y freshness

- **Objetivo:** detectar PRs/tareas/repositories que requieren atención mediante reglas explicables.
- **Afectados:** assessment rules/service/API/dashboard.
- **Reutilizamos:** status/merge readiness y notification patterns.
- **Agregamos:** assessments con reasons/confidence/expiry; configuración de ventanas.
- **Migraciones:** assessments y settings de intelligence.
- **Endpoints:** attention list y rule settings.
- **Frontend:** Attention view con why/evidence/freshness y dismiss/snooze local.
- **Pruebas:** truth tables de reglas, stale sync no produce falsas conclusiones, dismissal expiry.
- **Aceptación:** CI failed, changes requested y stale evidence generan razones exactas; sync atrasado muestra “datos incompletos”, no “bloqueado”.
- **Riesgos:** falsos positivos. Mitigar con lenguaje de candidato, confidence y freshness gates.

### Checkpoint 6 — Resúmenes ejecutivos con IA

- **Objetivo:** generar “qué desarrolla cada persona”, “qué cambió hoy” y “qué necesita atención” con evidencia.
- **Afectados:** summary service, provider-neutral summarizer port/adapter, store/API/UI.
- **Reutilizamos:** Chat driver/agent infrastructure sólo si ofrece invocación apropiada; evidence/assessment dataset.
- **Agregamos:** summary jobs/cache, prompt versions, citations y redaction/budget.
- **Migraciones:** `intelligence_summaries` y job state si es durable.
- **Endpoints:** latest/generate summaries y status.
- **Frontend:** summary cards con timestamp, scope y enlaces de evidencia.
- **Pruebas:** prompt snapshot, citation validation, empty/incomplete evidence, idempotent evidence hash, provider failure.
- **Aceptación:** ningún claim sin evidence id/URL; misma evidencia reutiliza cache; cambio de watermark invalida resumen.
- **Riesgos:** alucinación y fuga de código privado. Mitigar con facts mínimos, citations obligatorias, provider/config explícito y sin bodies completos por defecto.

### Checkpoint 7 — Plane read-only y enlaces

- **Objetivo:** importar tareas Plane y relacionarlas con evidencia.
- **Afectados:** Plane port/adapter/observer/store/service/API/frontend.
- **Reutilizamos:** sync/cursor/backoff/evidence patterns y tracker vocabulary donde aplique.
- **Agregamos:** connection, projects/issues, states, identities y links.
- **Migraciones:** tablas Plane y evidence links.
- **Endpoints:** connection/project/issues/link/unlink y task evidence view.
- **Frontend:** Plane settings, task list y evidence panel.
- **Pruebas:** pagination, auth/rate limit, state changes, manual/automatic linking, no writes.
- **Aceptación:** una tarea muestra evidencia técnica reciente o “sin evidencia observada” con ventana/freshness; Plane no se modifica.
- **Riesgos:** API cambiante y mapeo de usuarios. Mitigar con adapter aislado y external ids.

### Checkpoint 8 — Workflow durable mínimo

- **Objetivo:** representar Planner → Worker → Reviewer → Fix → Verify como state machine, inicialmente con avance administrado.
- **Afectados:** `workflow` domain/service/coordinator/store/API/UI; integración por puertos con session/review.
- **Reutilizamos:** spawn/send, review runs, PR/check facts, CDC, notifications.
- **Agregamos:** runs/steps/attempts/checkpoints/outbox y reconciler de boot.
- **Migraciones:** tablas workflow y triggers.
- **Endpoints:** create/get/list/cancel/advance/retry run.
- **Frontend:** run timeline, current checkpoint, evidence y controles.
- **Pruebas:** state transition tables, crash at each boundary, duplicate commands, cancellation, ambiguous external state.
- **Aceptación:** un run sobrevive reinicio en cada fase sin duplicar worker/review; sólo avanza con acceptance facts.
- **Riesgos:** segunda lifecycle paralela. Mitigar: workflow coordina servicios existentes; nunca escribe sessions/reviews directamente.

### Checkpoint 9 — Corrección y verificación automáticas

- **Objetivo:** cerrar automáticamente review→fix→re-review/CI dentro de budgets.
- **Afectados:** workflow policies/coordinator, evidence adapters, UI controls.
- **Reutilizamos:** auto-review, auto-inject, session messenger y check observer.
- **Agregamos:** expected artifact rules, stable-head guard, max correction loops y timeouts.
- **Migraciones:** policy snapshot/attempt outcomes si no estaban en checkpoint 8.
- **Endpoints:** policy update y pause/resume.
- **Frontend:** autonomía, budget, pause y audit log.
- **Pruebas:** changes requested, new SHA, CI pending/fail/pass, review stale for old SHA, loop budget exhausted.
- **Aceptación:** changes requested genera una sola corrección; un nuevo head invalida review vieja; green+approved completa el step.
- **Riesgos:** loops infinitos o merge indebido. Mitigar con budgets, no auto-merge inicial y pause global.

### Checkpoint 10 — Health, cooldown y failover Codex→Claude

- **Objetivo:** failover automático conservador ante usage/rate/provider failure.
- **Afectados:** agent health service/store, error classifiers/capabilities, workflow policy, agent switch integration.
- **Reutilizamos:** Codex rate limits, usage, auth probes y durable agent-switch saga.
- **Agregamos:** health/circuit breaker, cooldown y ordered fallback.
- **Migraciones:** `agent_health` y attempt failure classification.
- **Endpoints:** health/status, clear cooldown, policy.
- **Frontend:** agent availability/cooldown/reason y override manual.
- **Pruebas:** known reset, unknown rate error, auth missing, blocked session, restart during switch, duplicate retry.
- **Aceptación:** rate-limited Codex entra en cooldown y un worker eligible cambia una sola vez a Claude con context/branch/PR intactos; estados ambiguos piden atención.
- **Riesgos:** falso rate limit, pérdida contextual y coste. Mitigar con typed signals, saga existente y budgets.

### Checkpoint 11 — Operación autónoma prolongada

- **Objetivo:** reanudación unattended dentro de límites explícitos.
- **Afectados:** workflow scheduler/reconciler, global pause, retention, notifications, diagnostics.
- **Reutilizamos:** daemon supervisor, observers, recovery, notifications y app state.
- **Agregamos:** leases locales, heartbeat del coordinator, maintenance windows, budgets temporales/coste y audit report.
- **Migraciones:** scheduler lease/run audit si son necesarios.
- **Endpoints:** pause-all/resume, diagnostics y autonomous window.
- **Frontend:** control de autonomía, próxima acción, límites y recovery report.
- **Pruebas:** daemon kill/restart, laptop sleep/clock jump, network offline, rate limit prolongado, disk full, corrupted checkpoint handling.
- **Aceptación:** tras reinicio se reconcilian runs sin duplicar efectos; límites detienen de forma segura; toda acción automática es auditable.
- **Riesgos:** acciones inesperadas. Mitigar con default paused para mutaciones, scopes, budgets, no auto-merge hasta decisión posterior y kill switch visible.

## 19. Definición exacta de Fase 0

Fase 0 corresponde únicamente al Checkpoint 0. Su propósito es validar integración y UX usando hechos existentes, no adelantar el nuevo sync.

### Incluye

- Nuevo bounded context `service/intelligence` de sólo lectura.
- Un read model agregado por proyecto con:
  - repositorio/proyecto AO;
  - sesiones worker activas y actividad/último update;
  - branch AO;
  - PRs atribuidos;
  - CI aggregate y nombres de checks fallidos disponibles en summary;
  - review/mergeability/changed-file counts disponibles;
  - usage compacto si el store lo reporta;
  - razones deterministas de atención reutilizando semántica actual.
- `GET /api/v1/intelligence/overview?projectId=`.
- Página desktop `Observatory` con filtros All/project, summary counts, attention list y freshness.
- Texto permanente: “Esta fase muestra sólo evidencia de trabajo gestionado u observado por AO; aún no es una vista completa de GitHub.”
- Tests de servicio, HTTP, contrato y UI.

### No incluye

- Ninguna migración.
- Nuevas llamadas a GitHub o scopes.
- Repos remotos sin project AO.
- Developer identity, commits globales, Plane o IA.
- Scheduler/workflow nuevo.
- Failover automático.
- Cambios de semántica a sessions/status/PR/lifecycle.

### Archivos esperados al implementarla

Principalmente archivos nuevos, más touch points inevitables:

- `backend/internal/service/intelligence/*` (nuevo)
- `backend/internal/httpd/controllers/intelligence.go` y test (nuevo)
- `backend/internal/httpd/controllers/dto.go`
- `backend/internal/httpd/apispec/specgen/build.go`
- router/API deps y `backend/internal/daemon/daemon.go` con wiring mínimo
- OpenAPI y `frontend/src/api/schema.ts` regenerados
- route/hook/component/tests nuevos bajo `frontend/src/renderer`
- navegación mínima en shell/sidebar

### Criterio de salida de Fase 0

Fase 0 está terminada sólo cuando:

1. El overview deriva todo desde APIs/stores existentes y no lee SQLite directo desde controlador.
2. Cero migrations y cero network calls nuevas.
3. Los counts/attention del fixture coinciden exactamente con session/PR facts.
4. UI presenta cobertura y freshness sin afirmar visibilidad global.
5. SSE invalida/refetch o el polling actual actualiza la vista sin payload masivo.
6. Pasan tests de servicio/controlador/UI, spec drift, `go test ./...` y `npm run frontend:typecheck`.
7. El diff no contiene refactors ajenos.

## 20. Riesgos técnicos principales

| Riesgo | Impacto | Mitigación |
|---|---|---|
| Conflictos con upstream | Alto | Módulos/tablas nuevas, touch points mínimos, checkpoints pequeños |
| Duplicar PR facts | Alto | Separar global intelligence de session-owned PR; bridge explícito |
| Rate limits GitHub | Alto | Sync incremental durable, ETag, budgets, jitter/cooldown, freshness visible |
| Falsa atribución | Alto | External ids exactos, aliases manuales, confidence/provenance |
| Inferir “bloqueado” por silencio | Alto | Freshness gate, lenguaje candidato y reasons; ausencia ≠ inactividad |
| Volumen SQLite | Medio | Ventanas de import, índices, archivos on-demand, retención configurable |
| CDC ruidoso | Medio | Triggers compactos como invalidación; coalescing; no payloads completos |
| Secretos/contenido privado | Alto | No tokens en DB, redacción, scopes mínimos, summaries con facts acotados |
| Workflow duplicado tras crash | Alto | Outbox/idempotency/checkpoints/reconciliation por efectos |
| Failover incorrecto | Alto | Typed classification, circuit breaker, saga existente, blocked/ambiguous guard |
| Reviewer ejecuta código no confiable | Alto | Mantener sandbox/allowlist actual; no ampliar herramientas sin amenaza revisada |
| Tests inferidos de checks | Medio | Tipo/confidence explícitos; no afirmar test result sin evidencia |
| Drift docs/código | Medio | Characterization tests y actualización documental por checkpoint |
| Dependencia de CLI instalada | Medio | Capability/auth probes y errores accionables; AO no bundlea provider CLIs |

## 21. Preguntas abiertas

1. ¿Los repos de intelligence siempre tendrán checkout local o deben conectarse sólo por owner/name? Este plan soporta ambos y recomienda no exigir checkout.
2. ¿GitHub Enterprise está en alcance inicial o sólo github.com?
3. ¿Qué ventana de import inicial se desea: 7, 30 o 90 días?
4. ¿Qué retención se requiere para commit files, review bodies y failed log tails?
5. ¿Qué identidad manda cuando Git author y GitHub login no se pueden vincular de forma segura?
6. ¿Los bots deben ocultarse por defecto, agruparse o mostrarse como actores separados?
7. ¿Qué checks cuentan como “tests” y se aceptará una clasificación configurable por repo?
8. ¿Qué significa “poca evidencia reciente” por tipo de tarea y qué ventana usa cada repositorio?
9. ¿Qué proveedor/modelo generará resúmenes y qué política de envío de metadata privada se acepta?
10. ¿Plane será cloud o self-hosted, y qué versión/API se debe certificar?
11. ¿Los links Plane↔Git deben ser manuales, por convención de branch/PR, o ambos?
12. ¿El workflow autónomo puede abrir/pushear PRs sin aprobación? El plan asume que sí puede continuar trabajo ya autorizado, pero no mergear automáticamente al inicio.
13. ¿Cuántos ciclos fix/review y cuánto coste/tiempo máximo por objetivo?
14. ¿Failover debe aplicar sólo a workers TUI al inicio, coherente con agent switching actual, o también a Chat tras ampliar la capability?
15. ¿Claude puede ser worker de fallback con el mismo nivel de permisos que Codex o requiere policy distinta?
16. ¿Se necesita export/purge de toda la evidencia por repo desde la primera versión?

## 22. Decisiones recomendadas para comenzar

1. Aprobar opción **(b), módulos nuevos del daemon**.
2. Ejecutar Fase 0 sin migraciones para validar read model/UX y límites.
3. Mantener `projects` como registro ejecutable y agregar después `intelligence_repositories` como catálogo observable.
4. No reutilizar `pr` como almacén global; enlazarlo.
5. Empezar GitHub con polling read-only, no webhooks.
6. Tratar el cambio durable Codex↔Claude como primitive de failover y construir la policy después del workflow durable.
7. No introducir IA hasta que timeline, provenance y freshness sean verificables.
8. No introducir Plane writes ni auto-merge en los primeros checkpoints.

