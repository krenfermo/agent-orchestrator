# Frente 3 / 3C: observabilidad de la exploración de agentes

Fecha: 2026-09-24
Branch: `feat/frente3-3c-exploration-observability`, creada desde ECC `8b8b1665c`.
Estado: implementado y validado con Claude y Codex reales sobre un fixture. Revisión independiente de Codex: ciclo 1 **NO-GO** (1 P0, 3 P1), corregido; ver §11. **Sin merge.**

Regla heredada de 3A/3B: **nunca se presenta como medido un número que AO no midió.**
Cada cifra lleva su `basis`:

- `observed`: lo reporta el transcript del provider, o AO lo cuenta exactamente a partir de hechos que el transcript reporta.
- `derived`: interviene una inferencia, y `method` la nombra.
- `unavailable`: el valor es `null`, nunca `0`.

Project Memory y Context Router siguen **OFF**.

---

## 1. Arquitectura

```
transcript del provider (JSONL, ya seguido por el usage pipeline)
   │  observe/usage/ingestor.go ── lee un chunk (cursor durable)
   ▼
observe/usage/parser.go ──┬─ eventos de uso (sin cambios)  → model_usage_events
                          └─ observe/usage/exploration.go   → domain.AgentToolFacts
                                 (tool calls, resultados, prompts, inyecciones)
   │  Store.ApplyUsageChunk(..., events, tools...)  ← MISMA transacción que eventos + cursor
   ▼
agent_tool_observations (0175)  +  vista agent_tool_observation_attribution
   │  misma resolución de ventana rol/ciclo que usage_event_attribution
   ▼
service/usage/exploration.go (ExplorationReader: métricas por agente + calidad)
   ▼
GET /api/v1/workflows/{id}/exploration   ·   ao workflow exploration <id> [--json]
```

**Por qué extender y no crear un sistema paralelo.** El usage pipeline ya aporta todo lo necesario:

- sigue los transcripts de Claude y Codex;
- los vincula con el subject (sesión, pane de reviewer o resolver, invocación del planner);
- es exactly-once gracias a las claves derivadas del artefacto;
- sobrevive a reinicios con su cursor durable;
- atribuye cada evento a su ventana de run, rol y ciclo.

3C añade una segunda salida a ese mismo parser. Esa salida se escribe en la misma transacción que los eventos y el cursor. Con esto:

- un crash no puede dejar el cursor por delante de observaciones no escritas;
- una relectura desde el offset 0 no duplica nada.

**Correlación.** La cadena es:

`agent_tool_observations.binding_id` → `usage_bindings` (subject, harness) → `usage_attribution_windows` (proyecto, run, step, rol, ciclo, attempt).

- Una observación sin `observed_at` cae en la ventana más temprana del subject y se reporta como `approximate`.
- Tool call → llamada al modelo: en Claude, cada tool call lleva el `source_event_key` del mensaje facturado que lo emitió.

**Raíz del proyecto.** La normalización de paths usa la raíz que **AO registró** para el subject, nunca la del transcript. La resuelve `GetUsageSubjectWorkspaceRoot`:

| Subject | Raíz |
|---|---|
| sesión | su worktree |
| pane de review | el worktree de la sesión revisada |
| resolver | el worktree de la sesión que pregunta |
| planner | el checkout del proyecto |

El `cwd` del transcript solo se acepta como base de paths relativos si está dentro de esa raíz.

## 2. Qué existía y qué se añadió

**Existía y se reutiliza:**

- el usage pipeline completo (watcher, ingestor y parser);
- `model_usage_events`, con tokens, `turn_class` (0169) y TTL de caché (0170);
- las ventanas de atribución (0149 y 0150);
- `TaskUsefulWorkMetrics` y las tablas de workflow para la calidad;
- la frontera 3B (`repoaccess.CheckPath`, `IsSecretPath`, `Redact`).

**Existía, pero no servía para esto:**

- Los hooks de Claude envían solo `tool_name` y `tool_use_id`, nunca el input.
- Codex no tiene hooks de herramientas.
- El recorder `AO_PROJECT_MEMORY_BASELINE` es opt-in y escribe a fichero. Además, `ObserveProviderUsage` no tiene ningún llamador.

**Añadido:**

| Pieza | Fichero |
|---|---|
| Vocabulario cerrado: origin, op, path scope y basis | `domain/agent_exploration.go` |
| Extractor de transcripts | `observe/usage/exploration.go`, con el hook en `parser.go` |
| Migración 0175 (tabla, 2 índices y vista) | `storage/sqlite/migrations/0175_agent_tool_observations.sql` |
| Queries y store | `queries/agent_exploration.sql`, `store/agent_exploration_store.go` |
| Read model y señales de calidad | `service/usage/exploration.go` |
| API | `httpd/controllers/workflow_exploration.go`, specgen, `openapi.yaml`, `schema.ts` |
| CLI | `cli/workflow_exploration.go` (`ao workflow exploration <id> [--json]`) |

**UI.** No se añadió. La vista no encaja de forma natural en Intelligence, que trata de memoria, grafo y búsqueda. El uso de un run vive en la página de detalle del run. Queda como P3.

## 3. Qué se persiste (y qué nunca)

Una fila por observación:

- `observation_key`, exactly-once;
- `event_key`, el vínculo con la llamada al modelo;
- `ordinal`, el offset del registro;
- `observed_at`;
- `origin`: `agent_exploration`, `ao_context`, `harness_context` o `unknown`;
- `op`: `read`, `search`, `list`, `command_explore`, `command_edit`, `command`, `edit`, `delegate`, `web`, `wait`, `plan`, `other`, `prompt` o `injected`;
- `tool_name`, de un **vocabulario cerrado**: el nombre de una herramienta conocida (`Read`, `Bash`, `exec`...), `mcp` para cualquier herramienta MCP, un tipo de attachment conocido de Claude Code, o vacío (= other). Un nombre libre del transcript nunca se persiste tal cual (P0 del ciclo 1);
- `path_scope`: `project`, `secret`, `excluded`, `outside_harness`, `outside`, `unresolved` o `none`;
- `path`, **solo** si el scope es `project`;
- `result_bytes`, `result_items` y `result_error`: `NULL` hasta que se observan, nunca 0 por defecto.

**Nunca se persisten:**

- el contenido de ficheros;
- comandos;
- patrones de búsqueda o queries;
- argumentos;
- prompts;
- resultados y diffs;
- paths fuera del proyecto;
- paths de secretos o de directorios excluidos.

**Enforcement en dos capas:**

1. **Parser.** Los tipos decodifican solo los campos necesarios. El comando solo se lee para nombrar el programa principal, y el resultado solo para medir su longitud.
2. **Store.** `AgentToolObservation.Valid()` rechaza la escritura completa, sin mover el cursor, en estos casos:
   - un path en un scope que no es `project`;
   - un path absoluto, que escapa de la raíz, no normalizado, con backslash, letra de unidad o caracteres de control;
   - un valor fuera del vocabulario;
   - un `tool_name` o un path que el redactor 3B reescribiría (con forma de credencial).

**Frontera de paths (3B)**, en orden:

1. Normalización léxica contra la raíz registrada por AO. Incluye los alias `/private/var` ↔ `/var`, `/private/tmp` ↔ `/tmp` en **ambas** direcciones. Un path con forma de otra plataforma (`C:\...`, backslash) o con caracteres de control queda `unresolved`.
   Si el path todavía existe, se resuelven sus symlinks (lstat de cada componente, sin abrir el fichero): un symlink que sale del proyecto se clasifica `outside`.
2. `repoaccess.CheckPath`: secretos y directorios excluidos (`.git`, `node_modules`, `.claude/worktrees`…).
3. `repoaccess.Redact` sobre el propio path: un path con forma de credencial se trata como `secret`.
4. `~/…` se clasifica como `outside`.
5. Sin raíz conocida el resultado es `unresolved`, nunca "outside".
6. El fichero no se abre nunca, porque el worktree puede haber desaparecido.

**Cobertura (P1 del ciclo 1).** `agent_tool_coverage` registra, por fuente de transcript:

- el rango de bytes que ha parseado el extractor 3C;
- las versiones del extractor (`ExplorationExtractorVersion`);
- cuándo cubrió por primera vez (`first_covered_at`).

Se escribe en la misma transacción que las observaciones.

El read model reporta cifras de herramientas de un agente solo si cumple las tres condiciones:

1. **ningún** evento de uso de sus fuentes se ingirió sin el extractor (`recorded_at < first_covered_at`, o fuente sin cobertura);
2. el extractor alcanzó el cursor;
3. una sola versión del extractor clasificó todo.

No se exige "desde el byte 0": el colector reanuda legítimamente un transcript ya conocido en una fila de fuente nueva, a mitad de fichero.

Si falla cualquiera de las tres (transcript ingerido antes de 0175, binario anterior, versiones mezcladas), esas cifras son `unavailable` con el motivo. Los tokens y las llamadas, que vienen del ledger de uso, se mantienen.

**Retención.** Se borra en cascada junto con el binding de uso, igual que `model_usage_events`.

## 4. Capability matrix

| Señal | Claude Code (TUI, transcript JSONL) | Codex (TUI 0.15x, rollout) |
|---|---|---|
| Llamadas al modelo | OBSERVED (mensajes facturados) | OBSERVED (deltas de `token_count`) |
| Input / output tokens | OBSERVED | OBSERVED |
| Cached tokens (lectura) | OBSERVED | OBSERVED |
| Escritura de caché | OBSERVED (con TTL) | OBSERVED (sin TTL) |
| Tool calls | OBSERVED (`tool_use`) | OBSERVED (`custom_tool_call`/`function_call`; en code-mode una llamada `exec` puede ejecutar varios comandos) |
| Lecturas de ficheros con path | OBSERVED si usa `Read`/`NotebookRead` | OBSERVED vía `item_completed.CommandExecution.parsed_cmd` `read` (el parser **propio** de Codex). `unknown` en comandos compuestos. UNAVAILABLE en rollouts sin items |
| Búsquedas | OBSERVED (`Grep`) | OBSERVED (`parsed_cmd` `search`; la query nunca se decodifica) |
| Listados | OBSERVED (`Glob`/`LS`) | OBSERVED (`parsed_cmd` `list_files`) |
| Exploración por shell | DERIVED (`command_explore`: programa principal `cat`/`rg`/`sed -n`/`git log`…; sin path) | no se parsea (code-mode es JavaScript) |
| Comandos no atribuibles a ficheros | OBSERVED (todas las llamadas a Bash) | OBSERVED (`parsed_cmd` `unknown`) |
| Ediciones estructuradas y ficheros editados | OBSERVED (`Edit`/`Write`/`MultiEdit`) | OBSERVED (`item_completed.FileChange`; `patch_apply_end` en versiones antiguas) |
| Ediciones por shell | DERIVED (`sed -i`, `>`, `tee`, `patch`, `git apply`) | — |
| Bytes devueltos al modelo por lectura | OBSERVED (longitud de `tool_result`, con el formato del harness) | UNAVAILABLE (una salida por `exec`) |
| Ítems del resultado | OBSERVED (`numLines`, `numFiles`) | UNAVAILABLE |
| Bytes del prompt de AO | DERIVED (se asume que el prompt es de AO) | DERIVED (`item_completed.UserMessage`; en versiones antiguas `user_message`) |
| Contexto inyectado por el harness | DERIVED (attachments, meta y resumen de compactación; tamaño registrado, no el input del tokenizer) | DERIVED y **cota inferior** (base_instructions y mensajes developer; los AGENTS.md y el entorno inyectados como items de rol user no son separables) |
| Tokens de harness en la 1.ª llamada | DERIVED (input de la 1.ª llamada − prompt de AO/4) | DERIVED (mismo método) |
| Ratios de exploración, AO y harness (bytes) | DERIVED | UNAVAILABLE |
| Duración | Run: OBSERVED (reloj de AO). Por agente: DERIVED (span de timestamps del transcript) | Igual |
| Clase de turno por llamada (I2) | OBSERVED (`turn_class`, por nombre de herramienta) | DERIVED: cada tool call se asigna a la primera llamada con timestamp ≥ al suyo (el rollout escribe los ítems de una respuesta antes de su `token_count`) |
| Planner (`claude --print --tools ""`) | Sin herramientas por construcción. Tokens por uso directo; sin transcript | n/a |

## 5. Métricas (read model) y su basis

**Por agente** (sujeto × rol × ciclo):

| Métrica | Basis |
|---|---|
| `modelCalls`, `inputTokens`, `outputTokens`, `cachedInputTokens`, `cacheWriteTokens`, `firstCallInputTokens` | O |
| `harnessTokensFirstCall` | D |
| `toolCalls`, `commands`, `edits` | O |
| `exploreCommands`, `shellEdits` | D |
| `fileReads`, `uniqueFilesRead`, `repeatedReads`, `searches`, `listings`, `explorationOps` | O, o U según la matriz |
| `explorationOpsAll` (**M3**) | Claude: D (Read/Grep/Glob/LS + comandos de inspección por programa). Codex: O (ítems `parsed_cmd` read/search/list; la llamada shell no se suma otra vez) |
| `unattributedCommands` | O |
| `uniqueFilesEdited` | O |
| `opsBeforeFirstEdit` | O si la 1.ª edición es estructurada; D si fue por shell; **U si no hubo edición** (secuencia censurada) o si las observaciones vienen de más de un transcript |
| `callsBeforeFirstEdit` | D (ordena dos flujos por timestamp); **U si no hubo edición** |
| `turnMix` (I2) | O (Claude) / D (Codex) |
| `sources`, `toolCoverage` | — |
| `repoBytesObserved`, `explorationResultBytes` | O (Claude) / U (Codex) |
| `aoContextBytes`, `harnessContextBytes` | D |
| ratios | D o U |
| `activeSpanMs` | D |
| `pathScopes` (conteos), `topFiles` (solo paths del proyecto) | — |

Cuando `unattributedCommands > 0`, las métricas de ficheros llevan `lowerBound: true` y su `method` dice **"LOWER BOUND"**. Lo mismo `uniqueFilesEdited` cuando hubo escrituras por shell sin path. Una cota inferior nunca se compara como exacta.

**Nivel de run (I3, I5).**

- `contextSources`: modo de memoria y estado del router **efectivos** del daemon que creó el run, congelados en `policy_snapshot.contextSources` en la misma escritura que crea el run. Sobrevive a la congelación de la política de ejecución. `recorded: false` = run anterior a 3C (no "off").
- `memoryPacks`: resumen de los `project_memory_context_manifests` del run (rol, digest del pack, commit indexado, generación, ítems, bytes, tokens estimados). Es la unión "lo que AO entregó ↔ lo que el agente consumió" que I5 pedía, hecha en la DB por run en vez de en el recorder de fichero opt-in (`ObserveProviderUsage` sigue sin llamador: el span de dispatch termina antes de que el uso se ingiera, así que no hay nada que unir en él).

**Totales del run.** Las métricas de secuencia (primera edición, primera llamada) son `unavailable` si hay más de un agente. Un run con varios harnesses reporta solo lo que todos pueden observar.

**Calidad del run.** Solo lectura. **No** usa `Coordinator.GetRun`, que tiene efectos colaterales.

| Señal | Basis |
|---|---|
| `finalState`, `completed` | — |
| `durationMs` | O |
| `attempts`, `failedAttempts`, `retries` | O |
| `providerFailovers` | O |
| `verifyRuns` | O |
| `verifyPassed` | del último `verify_result` |
| `checksPassed`, `checksFailed` | O |
| `reviewRuns` | O |
| `finalReviewVerdict` | de la última review |
| `fixCycles` | O (ciclos distintos de `fix_dispatched`) |

## 6. Pruebas reales (fixture controlado, memory/router OFF)

**Entorno:**

- Daemon aislado: `ao server --data-dir ~/.ao/scratch/3c/data --port 3071`, en trusted-local y con un admin de scratch.
- Variables de memoria y router **unset**.
- Producción no se tocó: no había AO vivo ni `~/.ao/running.json`.

**Fixture `ledgerlite`:**

- 11 ficheros trackeados;
- bug sembrado en `money.Split`;
- `.env` gitignored con un centinela;
- `CLAUDE.md` y `AGENTS.md`.

Se clonó en dos proyectos: `fx-claude` y `fx-codex`.

**Ejecuciones** (tarea: *"`go test ./...` falla; arregla la causa raíz sin tocar tests"*):

| # | Qué | Resultado |
|---|---|---|
| 1 | Run `wf-44608f00` (worker Claude Opus 5.5, reviewer **Codex**) | completed, verify ✓, review **approved**, 0 ciclos de fix, 248 s |
| 2 | Run `wf-1ae539fa` (proyecto fx-codex; el router eligió worker **Claude**, porque la política enruta a Codex solo en tareas "trivial") | completed, verify ✓, 159 s |
| 3 | Sesión `fx-codex-2` (worker Codex, spawn directo) | fix correcto; ingerida con el binario anterior |
| 4 | Sesión `fx-codex-3` (worker Codex) | prompt de AO 180 B, igual al que reportó `ao spawn`; `FileChange` → `internal/money/money.go` |
| 5 | Run `wf-be6a0536` (binario final; worker Claude, reviewer Codex) | completed, verify ✓, review **approved**, 239 s |
| 6 | Sesión `fx-codex-4` (binario final, worker Codex) | 5 `exec`, 4 comandos `unknown` para Codex, `FileChange` → `internal/money/money.go`, 6 llamadas, input 91 372 (74 752 en caché), output 657; los tests pasan |

Un run adicional (`wf-de828853`) se canceló antes de lanzar ningún agente, por un error de configuración de la política.

**Ejemplo de salida real** (run 5, worker; `~` = derived, `n/a` = unavailable):

```
Agent Exploration -- worker  [session fx-claude-2, claude-code]
  Models                     claude-opus-5-5
  Files read                 0       Unique files         0       Repeated               0
  Searches                   0       Listings             0       Explore cmds           ~2
  Tool calls                 4       Commands             4       Edits                  0 (shell ~1)
  Files edited               0       Ops before 1st edit  ~2      Calls before 1st edit  ~3
  Model calls                5       Input tokens         212820  Cached                 199250
  Output tokens              1389    1st-call input       40040   Active span ms         ~21791
  Harness tokens (1st call)  ~39223  Unattributed cmds    4
  Repo bytes                 0       AO bytes             ~3266  Harness bytes  ~177057
  Exploration share          ~0.8%   AO share             ~1.8%  Harness share  ~97.4%
Quality
  Final state     completed  Duration ms  239008
  Verify passed   true       Checks       1 passed / 0 failed  Verify runs  1
  Review verdict  approved   Review runs  1                    Fix cycles   0
```

**Verificación contra el transcript crudo.** Los conteos coinciden con el transcript. El worker de Claude hizo todo con Bash:

1. `git ls-files && go test`
2. `cat money.go`
3. el fix con `sed -i`
4. `ao work report`

No usó ni Read, ni Grep, ni Edit.

**Hallazgos de la línea base sin memoria:**

- **~98 % del input de la 1.ª llamada es contexto del harness.** Estimación: ~39 200 de 40 040 tokens en Claude, y ~17 100 de 18 800 en el reviewer Codex. El prompt de AO son ~800 tokens.
- En este repo pequeño la exploración es mínima (2 comandos). El coste de la sesión lo dominan las relecturas cacheadas: 199 K de 213 K tokens de input son lecturas de caché.
- Ninguno de los dos providers usó herramientas estructuradas de lectura en este fixture. Las métricas de ficheros son una **cota inferior** y así se declaran. El número que sí es exacto es el de comandos no atribuibles.

**Privacidad, verificada sobre la DB de scratch.** Hay 147 observaciones reales en 8 bindings:

- solo 2 filas llevan path, y es `internal/money/money.go`;
- 0 coincidencias del centinela de `.env` y del token;
- 0 paths absolutos;
- 0 textos de comando o de diff.

## 7. Migración

`0175_agent_tool_observations`:

- es aditiva: dos tablas (`agent_tool_observations`, `agent_tool_coverage`), 2 índices y una vista;
- no hace backfill;
- tiene Down (probado en up→down);
- no reconstruye ninguna tabla;
- añade FKs entrantes a `usage_bindings` y a `usage_sources` con `ON DELETE CASCADE` (desde ambas tablas). El inventario de `migrate_rebuild_fk_safety_test` se deriva del esquema, así que un rebuild futuro de esas tablas la verá.

No hay colisión de número con ninguna rama local ni remota. El `parser_state` no cambió de formato, por lo que hacer rollback del binario es seguro.

**INCIDENTE (2026-09-25 07:04 UTC), ya REVERTIDO.**

- Qué pasó:
  - Se compiló `go build .` desde `backend/`. Eso produce el **wrapper de compatibilidad del daemon**, no el CLI.
  - Se lanzó con `--version`. El wrapper ignoraba sus argumentos y arrancó un daemon sobre el data dir por defecto `~/.ao/data`.
  - Ese daemon migró producción de goose 174 a 175 y estuvo vivo unos 5 minutos.
- Recuperación, autorizada por Joaquín (opción B), el 2026-09-25 a las 15:08 UTC:
  - backup consistente previo: `~/.ao/data/pre-0175-rollback-20260925T150644Z.db`, SHA256 `86df0917…5607`;
  - ensayo sobre un clon;
  - Down de 0175 con goose y las migraciones exactas de `4a86d5c49`.
- Resultado:
  - producción vuelve a goose **174**, con `integrity_check` ok y 0 violaciones de FK;
  - no queda ningún objeto `agent_tool_*`;
  - todas las demás tablas inventariadas son idénticas por hash antes y después.
- Siguen, sin tocar, las escrituras secundarias del arranque accidental: finalización de usage, resync de memoria y grafo de 4 proyectos (MEDUSA no), y `renewed_at` de 2 branch locks de MEDUSA. Evidencia en `~/.ao/scratch/frente3/incident-recovery/`.
- El guardrail que evita esta clase de incidente está en §12.

## 8. Producción (solo lectura, apertura `immutable=1`)

> El incidente del 2026-09-25 (§7) llevó producción a goose 175. Se revirtió a **174** el mismo día; tras el rollback, `integrity_check` es ok y hay 0 violaciones de FK.

- goose **174**
- `integrity_check` **ok**
- `foreign_key_check` **0**
- `agent_tool_observations` ausente

No se migró ni se hizo rebuild de MEDUSA. No hubo limpieza. No se activó ni la memoria ni el router.

## 9. Riesgos y deuda (clasificados)

**P0:** ninguno.

**P1:** ninguno.

**P2 (tras el ciclo 1):**

1. **Clasificación inmutable.** Una observación se clasifica una sola vez. Mitigado: la cobertura registra la versión del extractor y un agente con versiones mezcladas queda `unavailable`. Para 3D se fija el binario.
2. **Bytes de harness sobrestimados en Claude.** Proceden de los attachments registrados en el transcript, no de lo que se envía. Por eso el ratio es DERIVED, y la estimación en tokens de la 1.ª llamada es la cifra preferible.
3. **Parser de Codex.** `parsed_cmd` marca `unknown` los comandos compuestos; la exploración de Codex queda casi siempre como cota inferior (`lowerBound: true`).
4. **Root del resolver.** Se toma del worktree de la sesión que pregunta. Si el resolver corre en otro worktree de placement, sus paths caen en `outside`.
5. **Codex nunca como worker en un run** con la política por defecto. 3D fija el harness del worker por la prioridad de la política de ejecución (congelada en el snapshot).
6. **Symlinks de paths ya borrados.** Si el path ya no existe al ingerir, vale la respuesta léxica.

Resueltos en el ciclo 1: `tool_name` libre (P0); I2/I3/I5; cobertura; M3 y censura de la 1.ª edición; `uniqueFilesEdited` como cota; paths de otra plataforma y alias; secuencias sobre varios transcripts.

**P3:**

- Vista en la UI, en el detalle del run.
- Endpoint por sesión.
- Retención propia, distinta de la del binding.

## 10. Diseño propuesto para 3D (pre-registro; no ejecutado)

Revisado tras la crítica metodológica del ciclo 1 (§11). Todo lo de esta sección se fija **antes** de ver ningún resultado.

**Brazos:**

- CONTROL: memoria `off`, router `off`.
- TREATMENT: memoria `assisted`, router `off`.
- `preferred` y el router quedan fuera de 3D.

Cada run declara su brazo en `contextSources` (congelado en el snapshot), y el de TREATMENT prueba en `memoryPacks` qué pack recibió cada dispatch (digest y commit indexado).

**Aislamiento por run** (contaminación entre repeticiones):

- **Data dir nuevo por run**, clonado de un snapshot dorado común a los dos brazos: proyecto registrado, índice de memoria ya construido en el commit del fixture y política de ejecución fijada. Motivo: la memoria **aprende de cada tarea terminada incluso con el modo en `off`** (`TaskMemory` graba resultados; solo el consumo depende del modo). Sin data dir nuevo, la repetición *n* de TREATMENT recibiría lo aprendido en las anteriores, incluida la solución.
- **Worktree nuevo** por run, desde el mismo commit del fixture.
- Mismo binario de AO (SHA registrado) y misma `ExplorationExtractorVersion`, visible en `toolCoverage.extractorVersions`.
- Harness por rol fijado en la política de ejecución del snapshot dorado: worker Claude, reviewer Codex, `fallback = wait_for_preferred`. Un run cuyo worker o reviewer no use el harness previsto se excluye y se reporta.
- Modelo fijado.
- HOME del provider aislado (`AO_PROVIDER_RUNTIME_ISOLATION=strict`).
- Ningún otro daemon ni AO vivo durante el experimento.

**Orden y caché:**

- Bloques emparejados por (tarea, repetición). Dentro de cada par, el orden CONTROL/TREATMENT se sortea con una semilla registrada.
- La caché de prompt del provider no se puede aislar entre runs, porque el prefijo del harness es idéntico en los dos brazos. El emparejamiento y el sorteo reparten la calidez de la caché entre brazos.
- Se reportan por run `cachedInputTokens` y `cacheWriteTokens` como covariables. M1 incluye la caché, así que no depende de ella.

**Fixture:** repo propio de 195 ficheros (Go + TS, 13 migraciones SQL, tests, `CLAUDE.md`/`AGENTS.md` cortos). Tiene dos trampas: un `Normalize` duplicado en dos módulos, y un `ARCHITECTURE.md` que sitúa mal los descuentos. Los tests ocultos están fuera del repo y los ejecuta un oráculo **después** del run, sobre la rama final. El agente nunca los ve, ni siquiera a través del verify de AO.

**Tareas:**

| Tarea | Tipo |
|---|---|
| A | bugfix localizado (off-by-one del bloqueo de login; el objetivo no nombra ficheros) |
| B | cambio transversal (migración + store + 2 rutas + cliente TS + test + docs) |
| C | revisión del commit HEAD contra `docs/API.md`, corrigiendo el defecto sembrado |
| D (control) | cambio de una constante en un fichero nombrado |

**Métricas por run y rol** (worker y reviewer por separado, nunca agrupadas). Salen del JSON de `GET /workflows/{id}/exploration`, que se archiva sin modificar:

- **M1:** `inputTokens`.
- **M1u:** `uncachedInputTokens`.
- **M2:** `modelCalls`.
- **M3:** `explorationOpsAll`.
  - Es un método por harness, y los dos brazos usan el mismo harness por rol.
  - `unattributedCommands` se reporta aparte como sensibilidad. Nunca se suma a M3.
- Secundarias: `opsBeforeFirstEdit` y `callsBeforeFirstEdit` (solo si ambos brazos editaron; si no, están censuradas), `harnessTokensFirstCall`, `outputTokens`, `uniqueFilesRead`/`repeatedReads` (solo si no son cota inferior), `turnMix` y la duración.
- Un run cuya `toolCoverage` no sea completa no aporta cifras de herramientas.

**Calidad:**

- Q1: `verifyPassed`;
- Q2: `finalReviewVerdict`;
- Q3: `fixCycles`;
- Q4: oráculo de tests ocultos;
- `retries` y `providerFailovers`.

**Estadística pre-registrada.** N = 5 pares por tarea; es un piloto sin potencia para significancia formal. Con 5 pares un test de signos bilateral no baja de p = 0,0625. Por tarea se reportan:

- la mediana y el rango por brazo;
- la diferencia relativa de medianas;
- cuántos de los 5 pares van en la dirección del efecto.

Un efecto cuenta como **consistente** solo si supera el umbral en la mediana **y** va en la misma dirección en ≥ 4 de 5 pares.

**Exclusiones pre-registradas:**

- Se repite (y se reporta) un run que falla por infraestructura: el harness no arranca, caída del provider o crash del daemon.
- Un fallo de calidad del agente **nunca** se excluye.

**Decisión** (doc 06 §5):

| Resultado | Condición |
|---|---|
| **GO** | En A, B y C: M1u −15 % **y** (M2 o M3) −20 %, consistentes. Q1/Q4 sin degradación (pass-rate ≥ CONTROL) y ciclos de fix ≤ CONTROL + 0,5 de mediana. D dentro de ±10 %. Las trampas no inducen errores solo con memoria |
| **ITERATE** | Mejora parcial o inconsistente, mejora en un rol con regresión en otro, o varianza que impide concluir |
| **NO-GO** | M1u sin mejora o peor en ≥ 2 de 3 tareas, o cualquier degradación de Q1/Q4, o una trampa que induce errores con memoria |

Una métrica `unavailable` en un brazo **no entra en la comparación**. Una `derived` se compara solo con otra del mismo `method`, y una cota inferior nunca se compara como exacta.

**Prerequisitos que ya cubre 3C:** `contextSources` (I3), `memoryPacks` (I5), cobertura, M1u y M3.

**Pendiente para 3D:**

- construir el snapshot dorado;
- aplicar 0175 **solo** en la DB de scratch.

## 11. Revisión independiente (Codex CLI)

Codex CLI 0.153.4 hizo de revisor, en modo de solo lectura y con el encargo explícito de **refutar** el GO. El prompt, la respuesta, el JSONL y la salida de los gates están en `~/.ao/scratch/frente3/reviews/3c/`.

**Ciclo 1: NO-GO.**

| Sev. | Hallazgo | Resolución |
|---|---|---|
| P0 | `tool_name` libre persistido: un nombre de herramienta MCP o de un plugin podía llevar un secreto | Vocabulario cerrado en el parser (herramientas conocidas, `mcp`, attachments conocidos, o vacío) y, en el store, rechazo de todo chunk con un `tool_name` o un path con forma de credencial |
| P1 | Faltaban I2, I3 e I5 del roadmap | I3: `contextSources` en el snapshot. I5: `memoryPacks` por run. I2: `turnMix` (observed en Claude, derived en Codex) |
| P1 | Telemetría ausente reportada como 0 observado | Tabla `agent_tool_coverage` y `toolCoverage`: sin cobertura completa desde el byte 0, las cifras de herramientas son `unavailable` |
| P1 | M3 sumaba comandos no clasificados; la 1.ª edición inexistente se contaba | M3 = `explorationOpsAll`, sin comandos genéricos. Sin edición, las secuencias son `unavailable` (censura) |
| P2 | `uniqueFilesEdited = 0` pese a una edición por shell | `lowerBound: true` y "LOWER BOUND" en el método |
| P2 | Paths de Windows, alias en una sola dirección, symlinks | `unresolved` para lo foráneo; alias en ambas direcciones; symlink de salida = `outside` |
| P2 | Secuencias mezclando varios transcripts | `sources`; la secuencia sobre > 1 transcript es `unavailable`; el primer prompt se elige por tiempo |

Codex marcó como **UNVERIFIED** build, vet, lint, short suite y el flake de tmux: su sandbox de solo lectura no deja compilar. En el ciclo 2 recibe los logs completos de los gates.

## 12. Guardrail de producción (P0 tras el incidente)

**Clase de incidente.** Una invocación inválida, o un binario experimental, cae en un daemon que usa el data dir por defecto (producción).

**Corrección mínima, fail-closed (`backend/main.go`):**

- el wrapper de desarrollo **rechaza cualquier argumento**, con exit 2;
- exige `AO_DATA_DIR` y `AO_RUN_FILE` **explícitos**;
- nunca cae a `~/.ao/data` ni a `~/.ao/running.json`;
- la validación ocurre antes de abrir, crear o migrar nada.

**Tests:**

- `TestWrapperRefusesBeforeStarting`: el arranque nunca se invoca con `--version`, con subcomandos, con cualquier argumento o sin ubicaciones explícitas.
- `TestWrapperProcessNeverTouchesDefaultDataDir`: ejecuta el `main()` real en un proceso hijo con un `HOME` temporal y comprueba exit 2 y que no se crea `~/.ao`. Con una mutación que desactiva el guard, el test falla: el daemon arranca confinado al `HOME` temporal.
- `TestProbesAndMisuseNeverStartADaemon` (CLI `cmd/ao`): `--version`, `version`, `--help`, la invocación sin argumentos, un flag desconocido y un comando desconocido no arrancan daemon ni crean estado.

**Riesgo residual, declarado.** Un binario experimental de `cmd/ao` ejecutado con un subcomando **explícito** de daemon (`ao daemon`, `ao server`, `ao start`) y sin `AO_DATA_DIR` sigue usando `~/.ao/data`. No es una invocación inválida: es la ruta legítima del app empaquetado, que también se compila con un `go build ./cmd/ao` sin sello de release. Hoy no hay forma de distinguir un binario "experimental" de uno "de release". Cerrarlo exige decidir una de dos cosas:

- sellar las builds de release;
- o exigir autorización explícita para migrar el data dir por defecto.

Las dos son decisiones de arquitectura y release, y quedan para Joaquín. Mitigación operativa en las herramientas de experimentos de Frente 3:

- `aoexp.py` compila siempre `./cmd/ao`;
- rechaza rutas fuera de `~/.ao/scratch`;
- fija e imprime `AO_DATA_DIR`, `AO_RUN_FILE` y el puerto antes de arrancar.
