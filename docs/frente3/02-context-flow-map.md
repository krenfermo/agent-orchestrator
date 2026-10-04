# Frente 3 / 3A — Mapa del flujo de contexto actual

Fecha: 2026-09-24 · Baseline: `feat/engineering-control-center` @ `3030d85f2` ·
Solo lectura. Rutas relativas a `backend/internal/` salvo que empiecen por `docs/`.

Este documento responde a la pregunta de la ETAPA 2: **¿cómo recibe contexto
cada rol hoy, y cuánto del repositorio tiene que redescubrir por su cuenta?**
Todo lo afirmado aquí está verificado contra el código, no contra la
documentación, que en varios puntos está desactualizada (ver §5).

---

## 0. El hallazgo que cambia el diseño

**Los Workers (y por tanto los Repair Agents, que se lanzan como workers) no
reciben memory pack, ni salida del context router, ni code graph en producción,
sea cual sea el valor de los flags.** Verificado:

- `daemon/workflow_wiring.go:317` inyecta
  `WorkerLauncher: &workflowWorkerLauncher{spawner: sessionMgr, …}` con el
  `sessionMgr` **sin decorar**.
- Los tres decoradores —`wfdispatch.Instrument` (`:496`), `wfrouter.Instrument`
  (`:510`), `wfmemory.Instrument` (`:523`)— reescriben `deps.Spawner`,
  `deps.ReviewerLauncher` y `deps.Planner`; ninguno toca `deps.WorkerLauncher`
  (`projectmemory/wfmemory/wfmemory.go:63-70`).
- `workflow/dispatch_state_machine.go:204-211` `workerLauncherOrDefault()`
  devuelve el launcher inyectado y sólo cae al `Spawner` (decorado) si no hay
  ninguno. En producción siempre lo hay.
- `daemon/workflow_worker_launcher.go:114` llama `l.spawner.Spawn` sobre el
  spawner crudo.

Cronología: el pack de memoria para workers se conectó en P2-B (`0c22fd957`,
2026-08-31); el launcher propio de workers llegó en P5-A 2C (`8d37a2928`,
2026-09-08) y, sin intención, sacó al worker del camino decorado. Las
mediciones de worker de P2-B (`docs/project-memory-optimization.md` §8) son
anteriores a esa regresión. `daemon/contextrouter_wiring_test.go` sólo prueba la
construcción del router, no qué superficies quedan envueltas, así que ningún
test lo detectó.

**Consecuencia:** el rol que más explora el repositorio —el Worker— es
precisamente el que nunca recibe la memoria. Cualquier plan de Frente 3 que
asuma "la memoria ya llega a los agentes, sólo hay que encenderla" parte de una
premisa falsa.

---

## 1. Tabla por rol

| Rol | Constructor de contexto (AO) | Lo que AO entrega | Agente / flags | Exploración propia | Memory/Router/Graph hoy |
|---|---|---|---|---|---|
| **Planner** (padres Autonomous y Master; Task no tiene planner) | `workflow/master_coordinator.go:235` → `adapters/planner/command/context.go:57-76` | 6 ficheros fijos (`AGENTS.md`, `README.md`, `go.mod`, `package.json`, `docs/architecture.md`, `docs/STATUS.md`, ≤48 KiB c/u, SHA-256) + 3 sondas git. ~87 KB en este repo | `claude --print --tools "" --permission-mode plan --no-session-persistence --model sonnet` (`planner.go:198`); sólo Claude | **Ninguna** (sin herramientas). El harness sí carga `CLAUDE.md` global y de proyecto, skills y MCP | Sí, con `AO_CONTEXT_ROUTER` y/o `AO_MEMORY_MODE`; el grafo llega vía el resumen de arquitectura del pack |
| **Worker** (Task y cada hijo de Autonomous/Master, que siempre son estrategia `task`) | `workflow/plan.go:129-192` `BuildWorkStepPromptWithSpec` + `session_manager/manager.go:3334-3379` system prompt | Objetivo, criterios, spec, guardrails, receta `ao work report`, sección de turn-economy ("no inspecciones el repo primero"), recap de dependencias; system prompt ~8 KB con `AgentRules` | Claude Code o Codex interactivo en el worktree (`routing_dispatch.go:163`); permisos por config de proyecto | **Total**: localiza el código, convenciones más allá de `CLAUDE.md`, historia | **No** (bypass, §0) |
| **Fix loop** | `workflow/cascade.go:316` `BuildFixPrompt` (`fix_prompt.go:29`) | Hallazgos del reviewer, objetivo, criterios, guardrails. Sólo si hay decisión `LifecycleCompact`: pack de sesión prepended (`cascade.go:385-405`) | Mensaje a la **misma sesión** del worker (`fix_dispatch.go:69-73`) | Relee diff y ficheros; su coste es crecimiento de contexto, no descubrimiento | No (por diseño) |
| **Reviewer de workflow** | `workflow/review_dispatch.go:1597-1712`, `review_prompt.go:102` | Alcance, SHAs base/head, criterios; en modo *light* además paths cambiados (≤50), evidencia pre-review ejecutada por AO y el work report | `daemon/workflow_reviewer_launcher.go:347-447`; Claude con allowlist Read/Grep/Glob + `git diff/log/show`, `ao review submit` (`adapters/reviewer/claudecode/claudecode.go:47-92`); también Codex y otros | Rederiva diff, historia y código circundante **en cada ciclo**; cada reviewer arranca en frío | Sí con flags (el pack de memoria rellena `SystemPrompt`; si no, el router); sin flags el `SystemPrompt` está vacío |
| **Reviewer de sesión/PR** (`review`, `autoreview`, `reviewgateway`) | `review/prompt.go:17-60` | Un **segundo texto de rol de reviewer**, independiente del anterior | Varios adaptadores; los TUI no confiables corren sin el checkout (`reviewgateway/gateway.go:47-49`) | Rederiva todo | No |
| **Repair Agents** (recovery / incident) | `workflow/repair_agent.go:714-735`, `repair_context.go` | Hallazgos (≤16 KB), salida de verify (≤8 KB), criterios, artefacto | Nuevo run de workflow → camino de worker | Relocaliza el código del fallo | **No** (mismo bypass que el worker). Además, el Spawner decorado usa siempre `RoleWorker` (`wfmemory.go:222`): el presupuesto `RoleRepair` sólo se usa en vistas previas |
| **Diagnostic Agent** | `workflow/incident_advisor.go:434` + `BuildIncidentContextPack` | Todo el contexto, con presupuesto en bytes | Scratch dir; Read/Grep/Glob **denegados** (`daemon/incident_agent_launcher.go:65-80`) | Ninguna posible | No (tiene su propio empaquetador) |
| **Skills — modo tool** (`skillrunner`) | `service/skills/run.go` + `skillrunner/staging.go` | Copia staged filtrada por scope/deny | Contenedor `--network none`, raíz read-only, sin LLM | Ninguna (determinista) | No |
| **Skills — modo agent** (`skillagent`, sólo `authz-review`) | `skillagent/prompt.go:28-50` | Reglas AO + `SKILL.md` + guía del modo; la tarea es la **lista** de ficheros staged; el contenido del repo nunca se cita en el prompt | `claude --tools Read,Grep,Glob --restricted --safe-mode --strict-mcp-config --no-session-persistence --model sonnet` (`skillagent/run.go:180-205`) | Explora libremente la copia staged, read-only, hermética frente al harness | No |

Notas:

- `api-infra-review` no declara `executor` y no tiene fila de scantools: sigue
  sin implementarse como modo agent (`docs/skills/skill-runs.md:274-276`).
- **Autonomous y Master** no son roles de agente propios: son un Planner más N
  hijos de estrategia `task` (`domain/execution_strategy.go:391-404`, estampado
  en `workflow.go:1340`). Todo lo dicho del Worker aplica a cada hijo.

---

## 2. Matriz flag × superficie (estado real)

| Superficie | `AO_CONTEXT_ROUTER=1` | `AO_MEMORY_MODE=assisted/preferred` | Code graph | `AO_PROJECT_MEMORY_BASELINE=1` |
|---|---|---|---|---|
| Planner | sí (Documents) | sí (pack; dedupe en `preferred`) | vía pack (arquitectura) | sí, con lecturas de ficheros |
| Worker / Repair | **no (bypass)** | **no (bypass)** | no | **no (bypass)** |
| Reviewer de workflow | sí | sí (tiene prioridad sobre el router) | vía pack | sí, sólo payload |
| Fix | no | no | no | sí, sólo el mensaje añadido |
| Reviewer sesión/PR, Diagnostic, Skills | no | no | no | no |

Por defecto los dos primeros flags están **off** y no hay entrada en el paquete
`config`: cada flag es un `os.Getenv` en su paquete. Lo que **sí** corre siempre
es la derivación: el `Reconciler` (`service/projectmemory/reconciler.go`) indexa
code graph y memoria cada 60 s para todo proyecto no archivado, y la UI de
Intelligence se puebla. **AO construye la memoria en segundo plano y ningún
agente la recibe.**

Interacción router + memoria: con ambos flags activos, `workflow_wiring.go:663`
pone `memoryRepo = nil` y `contextrouter/default.go:51-57` cae al **almacén JSON
legacy**, no a "sin memoria" como dice el comentario de `:659-662`.

---

## 3. De dónde sale el contexto que no controla AO

El contexto que más pesa no lo ensambla AO:

| Fuente | Quién la carga | ¿La controla AO? | Evidencia |
|---|---|---|---|
| Prefijo del harness (system prompt, schemas de tools, instrucciones MCP) | harness | no | 26.009 de 54.402 tokens de la 1.ª llamada de `wf-1c2cb9bd` (`docs/task-context-and-cost-budget.md:53-59`) |
| `CLAUDE.md` de proyecto | harness (cwd = worktree) | no | ~7.048 tokens, reenviado en cada una de 193 llamadas ≈ 1,35 M tokens (`task-context-and-cost-budget.md:216-217`) |
| `~/.claude` global (CLAUDE.md, skills, MCP, agentes) | harness con el `HOME` real | no, en instalación trusted-local | `providerruntime/resolver.go:139-153`; `runtimehome.SubprocessEnv` sólo aísla en modo `strict` y su propósito es aislar credenciales, no el contexto |
| Payload de AO | AO | sí | ~4.148 tokens = **7,6 %** de la 1.ª llamada (`docs/task-performance-cost-and-liveness.md:206-212`) |

La varianza 217.533 vs 44.857 tokens entre dos invocaciones del planner con el
mismo objetivo (`docs/roadmap.md`, Frente 4) es estructuralmente posible en
todos los caminos salvo el de Skills-agent, **el único que ya usa**
`--safe-mode --strict-mcp-config --restricted`. El doc
`task-performance-cost-and-liveness.md` §E.3 dice que AO "no tiene flag para
acotar" tools/MCP; el propio repo lo desmiente (`skillagent/run.go:180-205`,
`planner.go:198`).

---

## 4. Duplicaciones

1. **`CLAUDE.md` de proyecto** lo recarga el harness de cada rol (planner, cada
   worker, cada ciclo de reviewer, repair, reviewers de sesión). AO no lo
   suministra ni lo deduplica; el planner además envía `AGENTS.md` inline.
2. **`~/.claude` global** se reenvía a todo rol que no sea Skills.
3. **Diff e historia git** los rederivan el worker, el reviewer (en cada ciclo),
   el fix y el repair, aunque AO ya observa los paths cambiados y ejecuta la
   evidencia pre-review (sólo la entrega en reviews *light*).
4. **Dos textos de rol de reviewer** escritos por separado
   (`review/prompt.go:57` y `workflow/review_prompt.go`), más una tercera fuente
   cuando hay system prompt de memoria o router.
5. **Tres ensambladores con presupuesto**: `contextrouter`,
   `projectmemory.PackBuilder` (+ presupuestos de grafo) e
   `IncidentContextPack`.
6. **Dos grafos**: el JSON `NativeIndexer` del router, que **ningún camino de
   producción escribe** (el router siempre informa "project has not been
   indexed", `contextrouter/router.go:454-456`), y el SQLite `codegraph.Index`
   que alimenta la memoria.
7. `AgentRulesFile` se relee en cada spawn sin comprobar hash
   (`session_manager/prompt.go:133-142`).
8. Varios packs por rol se **calculan pero no llegan** al worker: las vistas de
   `SessionContextPack`, el pack de memoria de worker y la selección del router
   para worker.

---

## 5. ¿Cuánto redescubre cada agente?

Cualitativo, porque **AO no mide las lecturas del agente en ningún rol**
(`RepeatedReads` es `Unavailable` para worker, reviewer y repair,
`observe/projectmemory/recorder.go:450-477`). Poner un número aquí sería
inventarlo.

| Rol | Pre-ensamblado por AO | Redescubre por su cuenta |
|---|---|---|
| Planner | lo máximo: 6 docs + estado git (+ pack con flags) | nada (sin herramientas) |
| Worker / Repair | tarea, reglas, recap. Sin mapa, sin memoria, sin grafo, sin diff | **todo**. `wf-1c2cb9bd`: 193 llamadas, contexto de 54 K a 324 K tokens |
| Reviewer de workflow | alcance, SHAs y (light) evidencia de AO | diff, historia y código circundante, **por ciclo**. `wf-1c2cb9bd`: 43 llamadas y 2,1 M tokens de input entre 3 reviewers (`docs/p7-turn-economy.md:73-75`) |
| Fix | hallazgos | vuelve sobre el código dentro de un contexto ya grande |
| Diagnostic | el pack completo | nada (no puede leer el repo) |
| Skill agent | el alcance staged | sólo el alcance staged |
| Skill tool | — | nada |

**Conclusión de la etapa:** la exploración repetida se concentra en Worker y
Reviewer, que son justo los roles que hoy no reciben memoria (el Worker nunca;
el Reviewer sólo con flags que están off). El Planner ya recibe mucho contexto y
es donde se midió el ahorro (−53,7 %), pero es una invocación por run frente a
decenas o cientos de llamadas de worker y reviewer.

---

## 6. Docs desactualizados detectados (no se corrigen en 3A)

| Doc | Afirmación | Código actual |
|---|---|---|
| `docs/project-memory.md` §10, `project-memory-optimization.md` §2, `p2-project-memory-audit.md` §5/§8 | El worker/repair recibe el pack | Bypass desde P5-A 2C (§0) |
| `docs/context-router.md:112-128` | "Two surfaces are routed" | También el reviewer (`wfrouter/reviewer.go`) |
| `docs/p2-project-memory-audit.md` §4.2 | codegraph sin escritor en producción | Cierto sólo para el JSON `NativeIndexer`; el SQLite `Index` lo construye el Reconciler |
| `docs/p2-project-memory-audit.md` §2.6(a) | El repair recibe sólo el objetivo | Ahora lleva hallazgos, verify, criterios y artefacto |
| `docs/code-graph.md:299-302` | Repair recibe presupuesto propio | El camino de dispatch nunca usa `RoleRepair` |
| `docs/code-graph.md:155-159` | El worktree de una tarea pasa por `AnalyzeChanged` | Sin llamador en producción |
| `docs/code-graph.md:374-376` | No hay UI de grafo ni panel de memoria | P4-G Intelligence tiene pestañas Graph, Memory y Context |
| `docs/project-memory.md` §12 | No hay comando que imprima un pack | `GET /intelligence/context` y la pestaña Context |
| `docs/task-performance-cost-and-liveness.md` §E.3 | AO no tiene flag para acotar tools/MCP | `skillagent` y el planner ya lo hacen |
| `projectmemory/mode.go:83-86` | `AO_MEMORY_MAX_FILES`/`_FILE_BYTES` funcionan | Se parsean pero `NewService` usa siempre `DefaultIndexLimits()` (`service.go:118`) |
| `daemon/workflow_wiring.go:546` | taskMemory es nil con memoria off | Desde P4-H no depende del modo |
