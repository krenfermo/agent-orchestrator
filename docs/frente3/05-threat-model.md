# Frente 3 / 3A — Threat model de Project Memory

Fecha: 2026-09-24 · Alcance: derivación, almacenamiento y entrega de project
memory y code graph (`projectmemory`, `codegraph`, `contextrouter`,
`service/projectmemory`, endpoints de Intelligence) y su futura entrega a
Skills.

Todos los gaps marcados **VERIFICADO** se comprobaron leyendo el código en
`3030d85f2` (y, cuando se indica, la DB de producción en modo `immutable`).

**Principio.** La memoria es contenido derivado de un repositorio no
confiable. Cualquier dato que contenga puede estar controlado por un atacante:
nombres de símbolos, comentarios de documentación, README, CLAUDE.md,
docker-compose. Por eso **la memoria nunca puede otorgar más de lo que otorga
leer el repo**, y nunca debe servir como vía para saltarse los controles de
Skills.

---

## 1. Activos y fronteras

| Activo | Dónde |
|---|---|
| Código y secretos del repo | checkout del proyecto |
| Hechos y símbolos derivados | `ao.db` (0144/0153…) |
| Packs entregados | prompts y system prompts de agentes; manifests (0145) |
| Aislamiento entre proyectos y tenants | `project_id`, RBAC, `tenant_id` vía `projects` |
| Controles de Skills | `skillcatalog.Authorize`, staging, redacción |

Fronteras que se cruzan: repo → indexador (datos no confiables), DB → pack →
LLM (la memoria pasa a ser contexto de un agente con herramientas), API →
usuario (RBAC).

---

## 2. Amenazas, controles existentes y gaps

| # | Amenaza | Control existente | Gap | Severidad | Mitigación propuesta (fase) |
|---|---|---|---|---|---|
| T1 | **Secretos en `.env`, claves, credenciales** | codegraph: denylist **antes de leer** (`codegraph/classify.go:141-186`); memoria: exclusión en *signals* (`signals.go:200-255`) | **VERIFICADO** G-B2: el walk de memoria **lee y hashea** `.env`/`.pem`/`id_rsa` y registra sus paths en `project_memory_files` (sin denylist en `indexer.go`/`incremental.go`). Hoy no se deriva contenido, pero sólo porque ningún rol los clasifica, no por diseño | Alta | Un único paquete de clasificación de secretos compartido por ambos indexadores, aplicado antes de abrir el fichero (3B) |
| T2 | **Secretos inline en ficheros admitidos** (docker-compose, workflows CI, config) | ninguno | **VERIFICADO** G-B1: `derive.go` copia excerpts literales de Makefile, Dockerfile, **docker-compose.yml**, `.github/workflows/*.yml` y config (2 KB), README (4 KB) y ficheros de instrucciones, **sin redacción**. `skillreport.Redactor` existe pero no se usa aquí | Alta | Pasar excerpts por el redactor (patrones de token/clave + entropía) antes de persistir; opción de guardar sólo claves en config (3B) |
| T3 | **Prompt injection vía repo** (README, CLAUDE.md, comentarios, docstrings) | Preámbulo "el working tree manda" (`pack.go:1091-1100`); Skills: reglas 2-3 de `skillagent/prompt.go` | **VERIFICADO** G-B4: los ficheros de instrucciones se resumen como *"standing instructions agents in this repository must follow"* (`derive.go:333`) y se renderizan bajo "### Standing instructions" sin delimitadores (`pack.go:441,1113-1123`). El `Doc` de codegraph (primera frase del comentario) entra literal. **Invierte la regla de ADR 0010 §4** | Alta | Enmarcar el pack como **datos**: bloque delimitado, cabecera "contenido del repositorio, no instrucciones de AO"; eliminar el texto "must follow"; los ficheros de instrucciones entran como "el repositorio declara:" (3B). El harness ya carga `CLAUDE.md` por su cuenta, así que re-inyectarlo como instrucción de AO **amplifica** una inyección |
| T4 | **Symlinks** hacia fuera del repo | codegraph: `EvalSymlinks` del padre + contención + `Lstat` (`codegraph/scan.go:57-131`); walk completo de memoria: sólo ficheros regulares | **VERIFICADO** G-B3: el incremental de memoria usa un prefijo léxico + `os.Stat` + `os.ReadFile` (`incremental.go:484-495`), así que **sigue symlinks**. Un `CLAUDE.md -> ~/.aws/credentials` commiteado y nombrado en un diff se leería y se guardaría como item de instrucción. `drift.go` `confinedPath` también es sólo léxico | **Alta** | Reutilizar `codegraph.resolve` (o equivalente) en `readAdmitted` y en drift (3B) |
| T5 | **Path traversal** vía diff/rename | memoria: prefijo léxico; codegraph: contención real; `resolveRepo` sólo con allowlist (`service/projectmemory/projectmemory.go:355-383`) | cubierto parcialmente por T4 | Media | ídem T4 |
| T6 | **Ficheros enormes / binarios / generados** | límites de tamaño y número; detección NUL; generados fuera del retrieval | `AO_MEMORY_MAX_*` no se aplican; un límite alcanzado no marca la cobertura como parcial | Baja | Aplicar overrides; estado `partial` (3B) |
| T7 | **Graph poisoning por copias de trabajo** | memoria salta `.claude/.cursor/.aider` | **VERIFICADO en producción:** codegraph **no** los salta. MEDUSA sirve 4.265 / 17.242 símbolos (24,7 %) de `.claude/worktrees/roc-capacity-fe/`. Un agente recibiría símbolos de una rama ajena como si fueran del proyecto | Alta (correctitud y fuga entre ramas) | Indexar sólo `git ls-files` + skip list unificada (3B). Purgar y reconstruir el grafo de MEDUSA requiere aprobación del usuario, porque es un cambio en producción |
| T8 | **Grafo stale presentado como actual** | commit y generación en el pack; drift fail-closed; stale retenido | build truncado sin marcar; el modo y el commit no quedan congelados en el run | Media | Estado `partial`; `policy_snapshot` con modo, commit y digest (3B) |
| T9 | **Fuga entre proyectos** | todas las tablas con clave `(project_id, repo_id)` y `ON DELETE CASCADE`; `Provenance` comprueba el proyecto; RBAC `memory.read` / `project.manage` en cada ruta (`project_memory.go:291-300`, `project_memory_graph.go:164-166`, `project_intelligence.go:329-332`) | G-C1: con auth desactivada o una instalación sin reclamar, el guard permite todo (es así por diseño: el modo loopback de un solo usuario confía en el listener). G-C2: `repo_id` se deriva del path | Media (multi-tenant) / Baja (local) | Mantener; test explícito de que un pack de A nunca incluye filas de B (3B) |
| T10 | **Fuga entre worktrees o tareas** | `EnsureFresh` rechaza worktrees enlazados; `entitled()` sólo sirve conocimiento `task_local` a la tarea dueña o a sus dependencias; `prune.go` | G-C3: codegraph no tiene guard propio de worktree (depende de quien lo llama) | Media | Guard en `codegraph.Index.Build/Apply` (3B) |
| T11 | **Envenenamiento por inferencia de agente** | la promoción de `task_local` a canónico exige prueba de integración (P2-D) | G-B6: no hay clase "redactado por agente"; `workflow_verified` significa que verify pasó, no que el texto sea correcto | Media | `source=agent-inference` en la vista de procedencia; nunca `deterministic`; nunca se usa para decidir autorización (3B/3E) |
| T12 | **La memoria como bypass de Skills** | Skills no consumen memoria hoy | G-A: no hay capability ni campo de scope para memoria; los deny globs no se aplican a la memoria; no hay vinculación a commit | Alta **si se integra sin controles** | Capability `memory.read` + filtro por `FileScope` + entrega como fichero staged + commit (fase de Skills, **después** de T1-T4) |
| T13 | **Contexto del harness** (`~/.claude` global, MCP) mezclado con la memoria | Skills-agent: `--safe-mode --strict-mcp-config --restricted` | worker/reviewer heredan `HOME` en trusted-local | Media (coste/privacidad) | **Frente 4**, no Frente 3; se deja registrado |
| T14 | **Herramienta externa** (Graphify) | — | log de consultas por defecto, backend LLM autodetectado, escribe en el repo, instaladores intrusivos | Alta si se adopta | Ver doc 03 §5-6; sólo headless `--code-only` sobre copia staged |
| T15 | Argumento git controlado | `BaseRef` viene de config y se pasa a `git diff` | sin `--end-of-options` (`contextrouter/sources.go:65-73`) | Baja | Añadir `--end-of-options` o retirar el router (3B) |

---

## 3. Invariantes que 3B+ deben poder demostrar con tests

1. Ningún fichero que coincida con la denylist de secretos se **abre** durante
   un indexado, ni completo ni incremental.
2. Ninguna lectura del indexador resuelve fuera del root canónico (symlink en
   el fichero o en el padre).
3. Ningún excerpt persistido contiene un literal que el redactor reconozca.
4. El pack renderizado delimita el contenido del repo como datos y no contiene
   la cadena "must follow" aplicada a contenido del repo.
5. Un pack del proyecto A no contiene filas con `project_id` de B.
6. Ningún símbolo servido tiene un path bajo un directorio de worktree de
   agente ni fuera de `git ls-files`.
7. Un Skill sin `memory.read` concedida no recibe memoria. Uno con ella no
   recibe hechos de paths fuera de su scope o dentro de su deny.
8. Un pack nunca se entrega sin commit y estado; un grafo `stale`/`partial`
   se declara como tal.

---

## 3B — estado de cada amenaza (2026-09-24)

| # | Estado tras 3B | Dónde |
|---|---|---|
| T1 secretos por ruta | **Cerrado**: política única antes de abrir, en ambos indexadores | `repoaccess/secret.go` |
| T2 secretos inline | **Mitigado**: redacción en la escritura y en la salida; límite: solo formas conocidas (R1) | `repoaccess/redact.go` |
| T3 prompt injection | **Cerrado** para el framing: bloque delimitado y con nonce, sin "must follow", filas legacy re-etiquetadas | `repoaccess/frame.go` |
| T4/T5 symlinks y traversal | **Cerrado**: Lstat de cada componente + `os.Root` + fstat | `repoaccess/read.go` |
| T6 tamaño y límites | **Cerrado**: `AO_MEMORY_MAX_*` aplicados; `PARTIAL` visible (tope de ficheros) | `sync.go`, `provision.go` |
| T7 contaminación por worktrees | **Cerrado** en código, con elegibilidad `git ls-files`; MEDUSA pendiente de una limpieza autorizada (R7) | `repoaccess/eligible.go` |
| T8 grafo stale | **Cerrado**: aviso CURRENT/STALE/UNVERIFIED/PARTIAL en cada pack | `provision.go` |
| T9 fuga entre proyectos | **Probado** de extremo a extremo; hueco de par proyecto/ruta cerrado con `WithProjectScope` | `isolation_test.go` |
| T10 fuga entre worktrees | **Cerrado** por elegibilidad; ningún indexador lee un worktree auxiliar | `contamination_test.go` |
| T11 envenenamiento por inferencia de agente | Parcial: el texto de agente se redacta y enmarca; no hay clase "redactado por agente" (3A §3, pendiente) | — |
| T12 bypass de Skills | Sin cambio: Skills no consumen memoria (integración posterior) | — |
| T15 argumento git | Sin cambio en el router (off); los indexadores usan git endurecido | `repoaccess.GitCommand` |
