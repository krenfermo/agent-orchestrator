# Frente 3 / 3A — Plan de medición y piloto

Fecha: 2026-09-28 · Estado: **norma V3; no ejecutado.**

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

## 3. Métricas del piloto (norma V3)

Esta sección y las §§4–5 contienen la **única regla normativa** del piloto.
`3d-auth-design.md` y `3d-auth-design-v2.md` son historia y no pueden aportar
umbrales, excepciones ni reglas alternativas. Los umbrales se congelan sin
cambios en el preregistro; observar resultados nunca autoriza cambiar `N`, una
imputación ni una frontera.

**Métricas de consumo:**

- **M1 — Input acumulado de la sesión** (Σ `input_tokens`) por rol y por run.
  Incluye caché y se reporta como diagnóstico; no decide el GO.
- **M1u — Input acumulado sin caché.** Es la señal decisiva de coste.
- **M2 — Nº de llamadas al modelo** por rol.
- **M3 — carga normalizada de exploración.** Para A, B y D se mide en el worker
  antes de la primera edición; para C, en el reviewer antes del primer
  veredicto estructurado. Si `c` es el número de llamadas de exploración y `f`
  el de ficheros distintos, `M3 = max(c/C_cap, f/F_cap)`, acotado a `[0,1]`.
  `C_cap` y `F_cap` son límites del harness fijados en el preregistro. Si no hay
  primera edición/veredicto, el rol falla o falta cualquiera de los dos datos,
  `M3 = 1`. Así un fallo barato no puede parecer eficiente.

**Secundarias:** duración de pared, coste USD (sólo Claude), pico de contexto,
bytes del pack y relecturas.

**Calidad (gates, no métricas de ahorro):**

- **Q1** — verify pasa;
- **Q2** — veredicto final del reviewer `approved` (**descriptivo; no decide**);
- **Q3** — nº de ciclos de fix (**descriptivo; no decide**);
- **Q4** — oráculo de tareas: tests ocultos que el agente no ve, ejecutados por
  el supervisor en la VM de oráculo al final;
- **Q5** — revisión humana ciega de una muestra de diffs (descriptiva; no
  decide este experimento);
- **Q6** — localización estructurada de la tarea C, definida en
  [3d-auth-design-v3.md](3d-auth-design-v3.md) §4. Q6 sólo es aplicable a C;
  `NA` en A, B y D no es un dato faltante.

Para una muestra que termina en timeout, denegación, sanción, crash u otro
fallo después de iniciarse, Q1/Q4 y Q6 cuando aplique valen `0`. En M1u y M2 se
imputan los topes preregistrados de tokens y llamadas, no el consumo parcial;
M3 vale `1`. No se reintenta la muestra. Los reintentos HTTP internos del
cliente sí cuentan como llamadas y consumo de esa misma muestra.

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

`PREFERRED` queda fuera de este experimento. Sólo podría evaluarse en otro
plan y preregistro posteriores, nunca como reinterpretación del lote V3.

**Controles:**

- mismo modelo y harness fijados (`claude-opus-5` / `sonnet` explícito);
- mismo `HOME` aislado para todos los brazos (runtime-home `strict`), para que
  la varianza de `~/.claude` (217 K vs 45 K) no domine;
- mismo commit;
- ámbitos de proveedor nuevos, exclusivos por repetición y rol, sólo cuando
  los gates de [3d-auth-design-v3.md](3d-auth-design-v3.md) demuestren que el
  ámbito es una partición real; los cooldowns no son un control de aislamiento;
- **N se fija una sola vez, con N ≥ 5 por tarea y brazo**, antes del lote. Para
  OFF/ASSISTED y el mínimo N=5 son 4 × 2 × 5 = **40 muestras**. `preferred` no
  forma parte de este lote. Ningún resultado, fallo o sanción permite reducir
  o aumentar N.

**Condiciones de host:** según la memoria operativa del proyecto, no se
ejecuta junto a un AO vivo cargado y se usa un data dir aislado
(`AO_DATA_DIR`). El piloto **no toca la DB de producción**.

---

## 5. Única función de decisión ejecutable

### 5.1 Entradas congeladas

La función recibe el plan preregistrado (`N ≥ 5`), las 40 o más posiciones en
su orden sorteado, el estado del instrumento, y para cada posición: tarea,
brazo, estado terminal, M1u, M2, M3, Q1, Q4 y Q6/`NA`. M1u y M2 son el total de
todos los roles de la muestra; M3 usa el rol focal definido en §3. Q2, Q3, M1
y Q5 se reportan, pero no cambian el veredicto. La ausencia de una muestra
planificada no se ignora: sólo puede ser `NOT_RUN_INSTRUMENT` tras terminar el
experimento por fallo del instrumento.

Mediana significa el estadístico usual (promedio de los dos valores centrales
si el número es par). Para una métrica `X`, `ratio(t, X) =
median(ASSISTED_t.X) / median(OFF_t.X)`. Si la mediana OFF es cero, el ratio
cumple sólo cuando la mediana ASSISTED también es cero; en A/B/C un par cero
no cuenta como mejora. Los límites son inclusivos: exactamente `0.85`, `0.80`,
`0.90` o `1.10` cumple. Cualquier otro empate en A/B/C no es mejora.

### 5.2 Algoritmo

```text
decide(plan, samples, instrument):
  if instrument.failed_after_start
     or any planned sample == NOT_RUN_INSTRUMENT:
       return INSTRUMENT_NO_GO

  if samples do not equal exactly the preregistered task × arm × N positions:
       return INSTRUMENT_NO_GO

  normalize every post-start failure as:
       Q1=0; Q4=0; Q6=0 when task C;
       M1u=TOKEN_CAP; M2=CALL_CAP; M3=1

  quality = for every task t:
       pass_count(ASSISTED, Q1, t) >= pass_count(OFF, Q1, t)
       and pass_count(ASSISTED, Q4, t) >= pass_count(OFF, Q4, t)
       and, only for C,
           pass_count(ASSISTED, Q6, C) >= pass_count(OFF, Q6, C)

  efficiency = for each t in {A,B,C}:
       ratio(t, M1u) <= 0.85
       and (ratio(t, M2) <= 0.80 or ratio(t, M3) <= 0.80)

  negative_control = for each X in {M1u,M2,M3}:
       if median(OFF_D.X) == 0:
           median(ASSISTED_D.X) == 0
       else 0.90 <= ratio(D, X) <= 1.10

  if quality and efficiency and negative_control: return GO
  return NO_GO
```

No existe `ITERATE`, análisis por subconjunto, sustitución, relote ni regla
secundaria capaz de cambiar el resultado. Los intervalos, rangos, M1, Q2, Q3,
Q5, costes y análisis de sensibilidad se publican como diagnóstico. Un fallo
del instrumento produce `INSTRUMENT_NO_GO`, no un resultado de eficacia, y no
autoriza otro lote bajo el mismo preregistro.

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
