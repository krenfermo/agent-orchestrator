# Frente 3 / 3A — Discovery report

Fecha: 2026-09-24 · Rama de trabajo: `docs/frente3-3a-project-memory` (worktree
`../ao-frente3-3a`, creada desde `3030d85f2`) · Fase: **sólo discovery y diseño**.
No se ha instalado nada, no hay cambios de runtime ni de DB, y producción no se
ha tocado. La DB de producción sólo se ha leído con `?immutable=1`.

Documentos de esta fase:

| # | Documento |
|---|---|
| 1 | Este informe (baseline, inventario, medición, respuestas) |
| 2 | [02-context-flow-map.md](02-context-flow-map.md): flujo de contexto por rol |
| 3 | [03-graphify-evaluation.md](03-graphify-evaluation.md): Grae/Graphify |
| 4 | [04-architecture-proposal.md](04-architecture-proposal.md): arquitectura, modelo, retrieval, lifecycle, UX |
| 5 | [05-threat-model.md](05-threat-model.md) |
| 6 | [06-benchmark-plan.md](06-benchmark-plan.md) |
| 7 | [07-roadmap.md](07-roadmap.md): 3B+ y deuda |
| 8 | [../adr/0012-project-memory-in-tree-core.md](../adr/0012-project-memory-in-tree-core.md): ADR (Proposed) |

---

## 1. Baseline verificado

| Comprobación | Esperado | Observado |
|---|---|---|
| Rama | `feat/engineering-control-center` | igual |
| HEAD local | `3030d85f2` | `3030d85f2fc6ccd3e99234d3c4237510bc730933` |
| `origin/…` local | = HEAD | igual |
| Remoto (`git ls-remote`) | = HEAD | igual |
| Árbol | limpio | limpio |
| Producción goose | 174 | `max(version_id)=174` en `~/.ao/data/ao.db` (immutable) |
| Frente 2 | cerrado | `docs/roadmap.md`, 2G integrado (`5a6e782cc`) |

---

## 2. La premisa del Frente 3 hay que corregirla

El brief supone que AO todavía no tiene Project Memory ni Project Graph. **No es
así.** Entre 2026-08-24 y 2026-09-11 se construyeron y mergearon en ECC:

- P2-A: memoria durable.
- P2-B: packs en el ciclo de ejecución.
- P2-C: conocimiento compartido entre tareas.
- P2-D: autoridad y procedencia.
- Code graph en SQLite (migración 0153).
- P4-G: Project Intelligence (UI).
- P4-H: derivación automática.
- Context router.
- Harnesses de medición: `aobaseline`, `ctxregress`, `turnbench`.

La rama `feat/grae-graphify-memory` (`b7f40eec7`) es ancestro de ECC. El roadmap
ya lo reconoce (`docs/roadmap.md`, Frente 3):

> tener un contrato preparado no es tener un proveedor integrado

Por tanto, en 3A no se diseña un sistema desde cero. Se audita el que existe,
se busca por qué no produce el beneficio que se pretende y se decide qué falta
de verdad.

---

## 3. Inventario (ETAPA 1)

Formato: CAPACIDAD → IMPLEMENTACIÓN → CONSUMIDOR → PERSISTENCIA → LIMITACIONES →
REUTILIZABLE. Rutas relativas a `backend/internal/`.

| Capacidad | Implementación | Consumidor | Persistencia | Limitaciones | Reutilizable |
|---|---|---|---|---|---|
| **Code graph durable** (símbolos, aristas, arquitectura) | `codegraph.Index` (`index.go`); extractores Go `go/ast`, TS/JS lexer+llaves, Python lexer+indentación, SQL scan (`extract*.go`) | Reconciler (siempre ON), pack de memoria, API/UI Intelligence | SQLite, migración 0153: `code_graph_index/files/symbols/edges` | Sin Java/Kotlin/PHP/JSP/…; las aristas apuntan a nombres sin resolver; retrieval por substring (sin FTS); **skip list sin `.claude/.cursor/.aider`**; no respeta `.gitignore` | **Sí**, tras corregir la skip list |
| Code graph JSON por checkout | `codegraph.NativeIndexer`, puerto `CodeGraphProvider` | Sólo `contextrouter` y `ctxregress` | `~/.ao/data/codegraph/*.json` | **Nadie lo escribe en producción**: el router siempre ve "not indexed" | **No** (duplicado legacy) |
| **Project memory durable** (items/relaciones) | `projectmemory` (`indexer.go`, `derive.go`, `incremental.go`, `service.go`) | Reconciler (derivación siempre ON), `wfmemory` (gated por modo), CLI/API/UI | SQLite 0144/0145/0146/0157 | Deriva de docs/manifests/config, no de código fuente; `symbol_summary` nunca se produce; límites `AO_MEMORY_MAX_*` ignorados | **Sí** |
| Generaciones/CAS, restart-safety | `index.go` (`ClaimCodeGraphBuild`, `served_generation`), memoria con generación y cursor | Indexadores | columnas `generation`/`served_generation`/`phase` | Build stale se retoma a los 30 min | **Sí** |
| Incremental | `codegraph/diff.go` (`git diff --name-status -M`), `projectmemory/incremental.go` | Syncer / `EnsureFresh` | — | Sin change set demostrable → full rebuild. **El incremental de memoria sigue symlinks** | **Sí** (con fix) |
| Drift/staleness | `projectmemory/drift.go`, `stale.go`; estados de memoria y grafo (`service/projectmemory/memory_state.go`, `intelligence_state.go`) | status, pack (retiene lo stale) | columnas `state` | La contención de drift es sólo léxica | **Sí** |
| Autoridad/procedencia | `domain/project_memory_authority.go`, `evidence_class` (0157), `confidence` | pack (`Servable()`), router (`OriginCanonical`) | columnas | No hay clase "inferido por agente" | **Sí**, es la base de ETAPA 7 |
| Conocimiento entre tareas (P2-C) | `knowledge.go`, `authority.go`, `wfmemory/taskmemory.go` | pack, CLI `ao memory knowledge` | items `task_local`/`canonical` | Promoción por prueba de integración | **Sí** |
| Packs por rol y presupuestos | `pack.go`, `budget.go`, `graphmemory.go`, `dedupe.go`, `cache.go` | `wfmemory` (planner, reviewer; worker bypass) | manifests (0145) | Enmarca `CLAUDE.md` como "standing instructions … must follow"; repair usa el pack de worker | **Sí** (con fixes) |
| Context router | `contextrouter`, `wfrouter` | planner, reviewer (y spawner, que ya no usa el worker) | — | Fuente de grafo muerta; con memoria ON cae al JSON legacy | **Parcial** (presupuestos y progresión); redundante con el pack |
| Provisioner/sync con coalescencia | `projectmemory/provision.go`, `sync.go` | `wfmemory` | — | Coalescencia por proceso; rechaza worktrees enlazados | **Sí** |
| Puerto de grafo externo | `projectmemory/graph.go` `MemoryGraph`, `LocalGraph`, `TeeGraph`, `UnavailableGraph` | `LocalGraph` en `service.go:116`; `TeeGraph` sólo en tests | tabla de relaciones | Sin adaptador externo | **Parcial**: es el punto de enganche si se decide un adaptador |
| Intelligence UI/API | `httpd/controllers/project_intelligence.go`, `project_memory*.go`; `frontend/.../intelligence/*` | operador | — | Pestañas Overview, Architecture, Graph, Memory, Search, GitHub y Context | **Sí**, ya cubre casi toda la ETAPA 16 |
| CLI | `cli/memory*.go` (`report`, `status`, `inspect`, `graph status/sync/query`, `knowledge`, `context`, `validate`, `provenance`, `prune`) | operador | vía HTTP | — | **Sí** |
| Ledger de uso de tokens | `observe/usage` (watcher→parser→ingestor), `service/usage` | UI token ledger, dynamics | `model_usage_events`, `usage_bindings`, `usage_attribution_windows` (0052/0149/0150/0169/0170) | Skills sin binding; sólo tipo y nombre de la tool, sin argumentos | **Sí**, es la base del benchmark |
| Evidencia de payload AO | `observe/projectmemory` (+ `wfdispatch`) | opt-in `AO_PROJECT_MEMORY_BASELINE` | JSON en `~/.ao/data/project-memory/baseline/` | **No existe en producción**; `ObserveProviderUsage` sin llamadores | **Sí** |
| Harnesses A/B | `cmd/aobaseline`, `observe/ctxregress` + `cmd/aoctxregress`, `observe/turnbench`, `ao memory report` | CI / operador | — | Ninguno llama a un proveedor con memoria on/off; `ctxregress` usa un agente stub | **Parcial**: base de 3D |
| Diagnostic context pack | `workflow/incident_advisor.go` | Diagnostic Agent | — | Tercer empaquetador | No (fuera de alcance) |
| Workspaces/worktrees | `workspace`, `worktree`, `workspacewatch` | sesiones | — | No detectan lenguaje ni dependencias; `workspacewatch` sería una posible fuente de triggers | Parcial |
| Embeddings / vector / FTS / tree-sitter | — | — | — | **Ausentes** (ni en `go.mod` ni en SQL) | n/a |
| Referencias Grae/Graphify | Sólo prosa y fixtures de test | — | — | Ver doc 03 | n/a |

### Estado real en producción (DB leída con `immutable=1`)

| Dato | Valor |
|---|---|
| `project_memory_items` / `_relations` | 2.608 / 2.626 |
| `code_graph_index` | 13 repos; símbolos 173.775; aristas 513.685 |
| `project_memory_context_manifests` | 31 (worker 12, reviewer 19). Último de worker: 2026-09-08 00:11 UTC. Los de reviewer siguen hasta el 09-09 18:24. **Consistente con** la regresión de `8d37a2928` (2026-09-08 17:18 UTC); no lo prueba, porque no se sabe cuándo se desplegó el binario |
| Evidencia baseline (`~/.ao/data/project-memory/baseline`) | **no existe** |
| `model_usage_events` | 3.879 (2026-09-04 → 09-11) |
| `turn_class` poblado | 111 filas; 0 `read` |

**Contaminación del grafo verificada:** MEDUSA tiene 477 ficheros y **4.265 de
17.242 símbolos (24,7 %)** servidos desde `.claude/worktrees/roc-capacity-fe/`,
una copia de trabajo que se indexó como si fuera código del proyecto. El grafo
de AO sólo tiene 1 fichero `.claude/` (generación 1, anterior a la mayoría de los
worktrees). No se ha ejecutado un rebuild para comprobar cuánto entraría hoy.

**Cobertura de lenguajes real** (`git ls-files`, lectura):

| Proyecto | Extensiones dominantes | ¿Lo soporta el grafo? |
|---|---|---|
| MEDUSA | tsx 200, py 182, ts 128 | sí |
| plataforma_territorial | py 307, tsx 249, ts 193 | sí |
| SIGE | java 600, jsp 293, js 736 | **parcial** (sólo JS) |
| ws_sigeseguros_crm | java 421 | **no**: 0 ficheros en el grafo |

---

## 4. Medición disponible (ETAPA 3)

Detalle en [06-benchmark-plan.md](06-benchmark-plan.md). Resumen:

| Métrica | Estado | Fuente |
|---|---|---|
| Input tokens por llamada | **AVAILABLE** | `model_usage_events.input_tokens` (Claude y Codex) |
| Output tokens (+reasoning) | **AVAILABLE** | ídem. El output de la compactación no se registra |
| Cache read | **AVAILABLE** | ídem |
| Cache write | **PARTIAL** | sólo Claude |
| TTL 5m/1h de caché | **PARTIAL** | sólo Claude (0170); no expuesto en DTOs |
| Tamaño y trayectoria del contexto | **AVAILABLE** | `service/usage/dynamics.go` |
| Payload enviado por AO | **PARTIAL** | evidencia opt-in; 0 registros en producción |
| Contexto harness vs AO | **MISSING** | abierto desde Frente 4 |
| Ficheros leídos por el agente | **MISSING** (worker/reviewer/fix); **PARTIAL** (planner) | el parser decodifica sólo tipo y nombre de la tool |
| Nº de tool calls | **PARTIAL** | `turn_class` (0169): una clase por mensaje, sólo Claude, casi vacío |
| Nº de llamadas al modelo | **AVAILABLE** | conteo de eventos |
| Duración | **PARTIAL** | attempts y runs; sin latencia por llamada; 55 % de eventos sin `observed_at` |
| Coste USD | **PARTIAL** | calculado para Claude; Codex `unknown` |
| Reintentos | **PARTIAL** | derivable de attempts y failover |
| Compactaciones | **PARTIAL** | estado del parser; 0 en producción |
| Tests/verify, veredicto de review, ciclos de fix | **AVAILABLE** | attempts, `review_run`, ventanas de atribución |
| Modo de memoria del run | **MISSING** | no se congela en `policy_snapshot` |
| Tokens de Skills | **MISSING** | `skill_runs` sin binding de uso |

---

## 5. Respuestas a las 16 preguntas

**1. ¿Qué tiene AO hoy que podamos reutilizar?**
Casi todo lo que el brief pide construir:
- grafo de código durable, incremental y con generaciones/CAS;
- memoria con autoridad, procedencia y drift;
- packs por rol con presupuestos, dedupe y fallback que nunca rompe un dispatch;
- conocimiento entre tareas;
- UI y CLI de inspección;
- ledger de tokens por llamada;
- harnesses de medición.

Ver §3.

**2. ¿Qué problema concreto falta resolver?**
No falta un indexador. Faltan cuatro cosas:
- **(a)** que la memoria llegue a los roles que exploran: el Worker nunca la
  recibe, por un bypass de cableado, y el Reviewer sólo con flags apagados;
- **(b)** poder observar el lado del consumo (qué lee el agente, cuántas
  llamadas exploratorias hace) para demostrar el ahorro;
- **(c)** corregir defectos de higiene y seguridad del indexado (worktrees
  indexados, symlinks, secretos, framing de instrucciones) antes de encender
  nada;
- **(d)** cubrir Java, que usan dos de los proyectos del usuario.

**3. ¿Grae/Graphify es adecuado?**
- **Como núcleo, no.** Graphify escribe dentro del repo, es Python con unas 30
  gramáticas nativas, saca releases cada 1-3 días y no tiene la
  generación/autoridad/procedencia que AO exige.
- **Como fuente opcional de solo lectura** para lenguajes que AO no parsea, es
  una opción real, pero sin evidencia aún frente a escribir un extractor Java
  propio.
- **"Grae" no corresponde a ningún proyecto identificable.** Ver doc 03.

**4. ¿Integrar, construir o híbrido?**
Construir sobre lo existente, que es la opción B. Si el piloto demuestra que
hace falta, se añade un adaptador externo read-only detrás del puerto existente
(la puerta al híbrido C queda abierta, no decidida). Ver doc 04 §1 y el ADR 0012.

**5. ¿Fuente de verdad?**
El repositorio: el working tree en un commit concreto. La memoria y el grafo son
caché derivada, regla ya vigente (`docs/project-memory.md` §1). La única
excepción son los hechos introducidos por el usuario o por tareas verificadas,
que no se pueden derivar del repo y por eso llevan procedencia propia.

**6. ¿Cómo se mantiene incrementalmente?**
Como ya se hace:
- `git diff --name-status -M` desde el commit indexado;
- hash de contenido por fichero;
- `Apply` en la generación servida;
- full rebuild cuando no hay un change set demostrable.

Faltan dos cosas:
- limitar la entrada a los ficheros rastreados por git;
- un trigger más cercano al dispatch que el Reconciler de 60 s. `EnsureFresh` ya
  existe por dispatch.

Ver doc 04 §4.

**7. ¿Cómo evitamos stale context?**
Con los mecanismos que ya existen:
- cada pack declara el commit y la generación;
- drift fail-closed por identidad del repo;
- los items stale o no demostrables se retienen;
- `EnsureFresh` se ejecuta antes de cada dispatch;
- el preámbulo dice "el working tree manda".

Lo que falta:
- congelar en el run el modo y el commit del pack;
- avisar en la UI cuando el grafo servido difiere del HEAD.

**8. ¿Cómo seleccionamos contexto por tarea?**
Con el retrieval híbrido existente:
- anclas por paths nombrados, términos con stemming y coordinación;
- expansión por el grafo a llamadores, tests, tablas y rutas;
- presupuesto por rol.

El Reviewer añade diff más vecindario afectado, conectando `AnalyzeChanged`, hoy
sin llamador. Ver doc 04 §5.

**9. ¿Cómo reducirá tokens?**
La hipótesis a probar, no un hecho:
- el coste lo domina N (llamadas) × contexto creciente;
- el payload de AO es el 7,6 % de la primera llamada;
- un pack pequeño y relevante puede reducir las llamadas exploratorias iniciales
  del worker y del reviewer, que es donde crece N.

Los bytes que se dejan de enviar son secundarios. `assisted` **añade** bytes.

**10. ¿Cómo lo demostraremos?**
Con un A/B en vivo sobre un repo fixture controlado:
- memoria off vs `assisted`/`preferred`;
- mismas tareas y N repeticiones;
- midiendo en el ledger las llamadas, el input acumulado y las llamadas de
  lectura/exploración (requiere instrumentar las rutas de lectura de las tools);
- con gates de calidad: verify, veredicto de review y ciclos de fix.

Ver doc 06.

**11. ¿Cómo se integra con Claude/Codex/Skills sin duplicar lógica?**
Hay un único seam: `projectmemory.Provisioner` renderiza un `ContextPack`
neutral al proveedor, que ya existe. Lo que hay que hacer:
- colgar el Worker del mismo decorador que el resto;
- retirar la fuente de grafo del router;
- que Skills consuma el mismo pack como **fichero staged**, filtrado por el
  scope del manifest.

Ver doc 04 §7.

**12. ¿Cómo protegemos secretos y evitamos cross-project leakage?**
- Una denylist de secretos única, aplicada antes de leer, en ambos indexadores.
- Symlinks seguros en el incremental.
- Redacción de excerpts.
- Sólo ficheros rastreados.
- Framing "datos, no instrucciones".
- Una nueva capability de Skills (`memory.read`) que pase por `Authorize`.
- Aislamiento por `project_id`, que ya existe con RBAC `memory.read`.

Ver doc 05.

**13. ¿Qué persistencia necesitamos?**
La SQLite existente: tablas 0144/0145/0146/0153/0157. **No hace falta ninguna DB
nueva.** Para 3C sólo haría falta, como mucho, una tabla nueva de lecturas de
agente o columnas en el ledger.

**14. ¿Qué debe ser reconstruible?**
Todo lo derivado del repo: grafo, items `repo_derivation` y arquitectura, con
rebuild ya disponible. **No son reconstruibles** los hechos `task_outcome`,
`workflow_knowledge` y `user_provided`. Esos se tratan como estado de AO
cubierto por backup/restore (P10), no como caché.

**15. ¿Primera implementación mínima de 3B?**
Corrección y seguridad de lo que ya existe, sin funcionalidades nuevas:
- conectar el Worker (y el Repair con su rol) al decorador;
- skip list unificada y sólo ficheros rastreados;
- symlink-safe en el incremental;
- denylist de secretos compartida;
- redacción de excerpts;
- framing de datos;
- congelar modo y commit de memoria en el run;
- aplicar `AO_MEMORY_MAX_*`.

Todo sigue **off por defecto**. Ver doc 07.

**16. ¿Qué NO debemos construir todavía?**
- Una graph DB (Neo4j, FalkorDB).
- Embeddings o vector store.
- Un adaptador de Graphify.
- Un visor de grafos más rico.
- Memory ON por defecto.
- Resúmenes generados por LLM.
- Entidades de infraestructura (Service, Container, Queue, Environment…) sin
  demanda medida.
- La integración de Skills, antes de los fixes de seguridad.
- Cualquier cosa que dependa del ahorro antes de que el piloto lo demuestre.

---

## 6. Riesgos principales

1. **Encender la memoria antes de corregir los defectos de §3:** se sirven
   símbolos de worktrees, excerpts sin redactar y `CLAUDE.md` como instrucción.
2. **Que el ahorro no exista:** el 92 % del contexto de la primera llamada es
   del harness y el pack puede no cambiar N. El piloto debe permitir un NO-GO
   honesto.
3. **Reportar bytes no enviados como tokens ahorrados**, algo que los propios
   docs prohíben.
4. **La regresión del Worker demuestra que falta un test de cableado** que
   compruebe qué superficies quedan decoradas. Sin él, podría volver a
   ocurrir.
5. **Solaparse con el Frente 4:** el aislamiento del `HOME` y el recorte de
   tools/MCP ahorran probablemente más que la memoria, y no son de este
   frente.
