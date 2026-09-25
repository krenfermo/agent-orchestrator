# Frente 3 / 3A — Plan de medición y piloto

Fecha: 2026-09-24 · Estado: **diseño; no ejecutado.**

Regla heredada de `docs/project-memory-baseline.md`: **un número que AO no pudo
medir nunca se presenta como medido.** Un `0` medido y un `null` no disponible
son hallazgos distintos.

---

## 1. Telemetría disponible (ETAPA 3)

| Métrica | Estado | Dónde | Proveedor | Limitación relevante para el A/B |
|---|---|---|---|---|
| Input tokens por llamada | **AVAILABLE** | `model_usage_events.input_tokens` / `uncached_input_tokens` | Claude y Codex | Codex: deltas de acumulados; los no monótonos se descartan como anomalía |
| Output tokens | **AVAILABLE** | `output_tokens`, `reasoning_tokens` | ambos | el resumen de compactación no se registra |
| Cache read | **AVAILABLE** | `cache_read_tokens` | ambos | — |
| Cache write | **PARTIAL** | `cache_write_tokens` | sólo Claude | — |
| TTL 5m/1h | **PARTIAL** | 0170 | sólo Claude | no está en los DTOs |
| Nº de llamadas | **AVAILABLE** | conteo de eventos por binding, rol, step y ciclo | ambos | las compactaciones no cuentan |
| Trayectoria del contexto (inicial, pico, crecimiento por llamada) | **AVAILABLE** | `service/usage/dynamics.go` | ambos | es tamaño, no composición |
| Coste USD | **PARTIAL** | `pricing.go` (calculado) | sólo Claude | Codex `unknown` |
| Payload enviado por AO | **PARTIAL** | evidencia `AO_PROJECT_MEMORY_BASELINE` | — | opt-in; **no funciona en worker** (bypass); 0 registros en producción |
| Pack (items, bytes, tokens estimados) | **AVAILABLE** | `project_memory_context_manifests` | — | sólo cuando hay modo activo |
| Harness vs AO | **MISSING** | — | — | abierto (Frente 4) |
| **Ficheros leídos por el agente** | **MISSING** | — | — | el parser sólo decodifica `type` + `name` de `tool_use` (`observe/usage/parser.go:424`) |
| Tool calls | **PARTIAL** | `turn_class` (0169) | sólo Claude | una clase por mensaje; 111 filas en producción, 0 `read` |
| Duración | **PARTIAL** | attempts y runs; `elapsedSeconds` | ambos | sin latencia por llamada |
| Reintentos / failover | **PARTIAL** | `workflow_attempts`, `provider_attempts` | ambos | hay que derivarlo |
| Verify, veredicto de review, ciclos de fix | **AVAILABLE** | attempts, `review_run.verdict`, ventanas `cycle` | ambos | pass/fail sin conteo de tests |
| Modo de memoria del run | **MISSING** | — | — | no está en `policy_snapshot` |
| Tokens de Skills | **MISSING** | — | — | `skill_runs` sin binding |

---

## 2. Qué hay que instrumentar antes del piloto (3C)

Mínimo imprescindible. Sin esto, el piloto no puede demostrar reducción de la
exploración:

| # | Instrumento | Diseño | Privacidad |
|---|---|---|---|
| I1 | **Lecturas del agente** | Extender el parser para extraer de cada `tool_use` de Read/Grep/Glob/Bash (`cat`, `rg`, `sed -n`…) **sólo el path o patrón objetivo**, normalizado a relativo al worktree. Por llamada se registran los ficheros distintos y las relecturas. Claude primero; Codex (`function_call` en rollout) después | Sólo paths, nunca contenido. Paths fuera del worktree → `<outside>` |
| I2 | **Clase de turno completa** | Usar `turn_class` también para Codex y contar tool calls por mensaje (no sólo la clase) | — |
| I3 | **Congelar modo y pack en el run** | `policy_snapshot` con `memory_mode`, `pack_digest` e `indexed_commit` por dispatch | — |
| I4 | **Split harness/AO de la 1.ª llamada** | `input_tokens(1.ª llamada) − contextSentTokens(AO, est.)`, etiquetado `estimated` | — |
| I5 | Llamador de `Span.ObserveProviderUsage` | unir tokens reales a la evidencia | — |
| I6 | **Índice de la primera edición** | nº de llamada en que aparece la primera `Edit`/`Write` en el worktree. Es un proxy directo de "tiempo explorando antes de actuar" | — |

---

## 3. Métricas del piloto

**Primarias (las decisivas para el GO):**

- **M1 — Input acumulado de la sesión** (Σ `input_tokens`) por rol y por run.
  Incluye caché, porque es la señal de "cuánto contexto se procesó". Se
  reporta aparte **M1u**, el input sin caché, porque es la señal de coste.
- **M2 — Nº de llamadas al modelo** por rol.
- **M3 — Llamadas de exploración** antes de la 1.ª edición (I6) y ficheros
  distintos leídos (I1).

**Secundarias:** duración de pared, coste USD (sólo Claude), pico de contexto,
bytes del pack y relecturas.

**Calidad (gates, no métricas de ahorro):**

- **Q1** — verify pasa;
- **Q2** — veredicto final del reviewer `approved`;
- **Q3** — nº de ciclos de fix;
- **Q4** — oráculo de tareas: tests ocultos que el agente no ve, ejecutados
  por AO al final;
- **Q5** — revisión humana ciega de una muestra de diffs.

---

## 4. Diseño del piloto

**Repositorio fixture controlado.** Es necesario porque el ahorro depende del
repo. El fixture será un repo propio, versionado bajo el data dir de AO. **No**
se usará el repo de AO, porque sus `.claude/worktrees` y su tamaño contaminan la
medida. Criterios que debe cumplir:

- ~150-400 ficheros en al menos 2 lenguajes soportados (Go + TS);
- rutas HTTP, tablas SQL y tests;
- un CLAUDE.md corto;
- trampas deliberadas: un símbolo con el mismo nombre en dos módulos y un
  README que describe mal un módulo, para medir si la memoria induce errores.

**Tareas** (definidas antes de ver resultados, con tests ocultos):

| Tarea | Tipo | Rol que más debería beneficiarse |
|---|---|---|
| A | Bugfix localizado ("corrige X en el login"), 1-3 ficheros | Worker |
| B | Cambio transversal (endpoint + tabla + test) | Worker + Planner |
| C | Review de un diff dado con un defecto sembrado | Reviewer |
| D (control) | Tarea que no requiere explorar (editar un fichero nombrado) | ninguno. **Se espera diferencia ≈ 0**, y sirve para detectar sesgo |

**Brazos:**

- **OFF:** `AO_MEMORY_MODE=off`.
- **ASSISTED:** `AO_MEMORY_MODE=assisted`.
- **PREFERRED:** `AO_MEMORY_MODE=preferred`, sólo si ASSISTED no degrada la
  calidad.

**Controles:**

- mismo modelo y harness fijados (`claude-opus-5` / `sonnet` explícito);
- mismo `HOME` aislado para todos los brazos (runtime-home `strict`), para que
  la varianza de `~/.claude` (217 K vs 45 K) no domine;
- mismo commit;
- caché de provider caliente o fría **igual** en todos los brazos: orden
  aleatorio intercalado y ≥ 6 minutos entre corridas (TTL 5 m), o reportar
  aparte;
- **N ≥ 5 repeticiones** por tarea y brazo. Se reportan medianas y rango, no
  medias sueltas. Con 3 tareas × 3 brazos × 5 repeticiones salen 45 runs más
  el control. El coste se estima antes con `turnbench` y un run piloto.

**Condiciones de host:** según la memoria operativa del proyecto, no se
ejecuta junto a un AO vivo cargado y se usa un data dir aislado
(`AO_DATA_DIR`). El piloto **no toca la DB de producción**.

---

## 5. Criterio de decisión

| Resultado | Condición |
|---|---|
| **GO** (se recomienda `assisted` o `preferred` por proyecto) | En A, B y C: mediana de M1u **−15 %** o mejor **y** M2 o M3 **−20 %** o mejor frente a OFF. Q1-Q4 **sin degradación**: pass-rate ≥ OFF y ciclos de fix ≤ OFF + 0,5 de mediana. D ≈ 0 (±10 %) |
| **ITERATE** | Mejora en M3 sin mejora en M1u; o mejora en un rol y regresión en otro; o calidad dentro de ±1 fallo con varianza alta. Se ajustan presupuestos y framing y se repite |
| **NO-GO** | M1u sin mejora o peor en ≥ 2 de 3 tareas; o cualquier degradación de Q1/Q4; o las trampas del fixture inducen errores con memoria que no ocurren sin ella |

Los umbrales (−15 %, −20 %) son **propuesta a validar con el usuario antes de
correr**. Fijarlos antes evita elegir el criterio a la vista de los
resultados.

---

## 6. Criterio de lenguaje (Java / Graphify)

Pregunta separada, que se resuelve con un mini-benchmark de **cobertura**, sin
tokens:

- Sobre `ws_sigeseguros_crm` (read-only, copia staged) comparar un prototipo
  de extractor Java en Go (lexer + llaves) frente a lo que produciría un parser
  AST. Métricas: símbolos declarados recuperados, endpoints (anotaciones
  Spring) y relaciones de import.
- Si el extractor propio alcanza ≥ 90 % de las declaraciones y endpoints, se
  construye propio. Si no, y además hay demanda real de trabajo de agentes en
  esos repos, se evalúa el adaptador Graphify read-only (doc 03 §6).
