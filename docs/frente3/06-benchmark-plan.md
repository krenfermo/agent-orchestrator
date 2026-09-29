# Frente 3 / 3A — Plan de medición y piloto

Fecha: 2026-09-28 · Estado: **norma V4; no ejecutado.**

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

## 3. Métricas del piloto (norma V4)

Esta sección y las §§4–5 contienen la **única regla normativa** del piloto.
`3d-auth-design.md`, V2 y V3 son historia y no pueden aportar
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
  [3d-auth-design-v4.md](3d-auth-design-v4.md) §6. Q6 sólo es aplicable a C;
  `NA` en A, B y D no es un dato faltante.

La normalización total de estados, nulls y valores inválidos está en §5.2. No
se reintenta una muestra. Los retries HTTP internos del cliente cuentan como
llamadas y consumo de esa misma muestra.

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
plan y preregistro posteriores, nunca como reinterpretación del lote V4.

**Controles:**

- mismo modelo y harness fijados (`claude-opus-5` / `sonnet` explícito);
- mismo `HOME` aislado para todos los brazos (runtime-home `strict`), para que
  la varianza de `~/.claude` (217 K vs 45 K) no domine;
- mismo commit;
- ámbitos de proveedor nuevos, exclusivos por repetición y rol, sólo cuando
  los gates de [3d-auth-design-v4.md](3d-auth-design-v4.md) demuestren que el
  ámbito es una partición real; los cooldowns no son un control de aislamiento;
- **N se fija una sola vez, con N ≥ 5 por tarea y brazo**, antes del lote. Para
  OFF/ASSISTED y el mínimo N=5 son 4 × 2 × 5 = **40 muestras**. `preferred` no
  forma parte de este lote. Ningún resultado, fallo o sanción permite reducir
  o aumentar N.

**Condiciones de host:** según la memoria operativa del proyecto, no se
ejecuta junto a un AO vivo cargado y se usa un data dir aislado
(`AO_DATA_DIR`). El piloto **no toca la DB de producción**.

---

## 5. Única función de decisión total y ejecutable

### 5.1 Dominio, caps y registros

El plan fija `N ≥ 5`, tareas `{A,B,C,D}`, brazos `{OFF,ASSISTED}`, orden, roles
posibles y, para cada tarea `t` y rol `r`, dos enteros positivos:

```text
TOKEN_CAP_ROLE[t,r] = máximo input no cacheado permitido al rol
CALL_CAP_ROLE[t,r]  = máximo de requests al provider, incluidos retries
TOKEN_CAP[t]        = Σ_r TOKEN_CAP_ROLE[t,r]
CALL_CAP[t]         = Σ_r CALL_CAP_ROLE[t,r]
```

Un rol no utilizado tiene ambos caps `0`. Los caps, el máximo de retries por
clase y los roles se congelan en el preregistro raíz; no se estiman a partir de
la muestra observada. M1u y M2 válidos son totales de todos los roles y deben
ser enteros en `[0,TOKEN_CAP[t]]` y `[0,CALL_CAP[t]]`. M3 debe ser finito en
`[0,1]`. Q1 y Q4 deben ser `0|1`; Q6 debe ser `0|1` sólo en C y el literal
`NA` en A/B/D. Todo otro tipo, `null`, NaN, infinito o valor fuera de dominio es
`MALFORMED_SAMPLE`.

Estados terminales permitidos para posiciones de eficacia:

```text
COMPLETED
FAILED_WORKER | FAILED_REVIEWER | TIMEOUT | GATEWAY_DENIAL
PROVIDER_RETRY_EXHAUSTED | PROVIDER_RATE_LIMITED | PROVIDER_POLICY_FAILURE
PROVIDER_TERMINAL_FAILURE | PROVIDER_SANCTION | AGENT_INDUCED_FAILURE
BLOCKED_BY_PRIOR_SAMPLE | MALFORMED_SAMPLE
```

Todos requieren `SAMPLE_START` salvo `BLOCKED_BY_PRIOR_SAMPLE`, que el
supervisor asigna a una posición todavía no iniciada cuando una muestra previa
impide causalmente ejecutarla. Esa excepción cuenta como fallo y nunca como
unrun.

`INSTRUMENT_NO_GO` y `NOT_RUN_INSTRUMENT` pertenecen al experimento/lineage,
no son estados de eficacia de una muestra iniciada. La clasificación causal
que puede producirlos está cerrada en [3d-auth-design-v4.md](3d-auth-design-v4.md)
§2.

### 5.2 Normalización total por muestra

`normalize(sample)` siempre retorna un tuple completo
`(M1u,M2,M3,Q1,Q4,Q6)`:

1. La identidad de la posición siempre procede del plan/ledger host-only, no
   del record producido por la muestra. Si, después de `SAMPLE_START`, el
   record trae task/arm/sample_id ajenos, duplicados, corruptos o ausentes, la
   posición host-only correspondiente se clasifica `MALFORMED_SAMPLE`; records
   extra se conservan como evidencia pero nunca crean/eliminan posiciones.
2. Si el estado es `COMPLETED` y **todos** los campos satisfacen §5.1, conserva
   sus valores.
3. Si el estado es cualquier fallo permitido, o es `COMPLETED` con al menos un
   campo null/malformed/out-of-domain, reclasifica `MALFORMED_SAMPLE` cuando
   corresponda y retorna:

```text
M1u = TOKEN_CAP[task]
M2  = CALL_CAP[task]
M3  = 1
Q1  = 0
Q4  = 0
Q6  = 0 si task=C; NA si task∈{A,B,D}
```

No se conservan parcialmente campos “buenos” de un registro inválido. Un
timeout, provider error, sanction, fallo de parser/gateway alcanzable por la
muestra y agotamiento de retries siguen la regla 3. Cada retry del cliente
incrementa M2 y sus tokens incrementan M1u; si el sample termina en éxito y no
excede caps, sus totales observados son válidos. Exceder un cap es
`AGENT_INDUCED_FAILURE` y usa la imputación, aunque el cliente declare éxito.

### 5.3 Validación total del experimento

`validate(plan, records, lineage)` retorna exactamente uno:

- `INVALID_EXPERIMENT_INPUT`: exclusivamente schema/plan/cap/lineage inválido
  detectado **antes del primer SAMPLE_START**, o ledger WORM roto por una causa
  demostrablemente no alcanzable por ninguna muestra;
- `INSTRUMENT_NO_GO`: existe un evento independiente válido según V4 §2 y por
  ello una o más posiciones son `NOT_RUN_INSTRUMENT`;
- `SCOREABLE`: existen exactamente `4 × 2 × N` posiciones, cada una iniciada o
  `BLOCKED_BY_PRIOR_SAMPLE`, y todas normalizan por §5.2.

Después del primer `SAMPLE_START`, task/arm/ID corrupto, record duplicado,
ausente o ilegible, parser failure y ledger-write failure alcanzable por una
muestra se normalizan como fallo de esa posición, no como
`INVALID_EXPERIMENT_INPUT`. Si la ejecución cesa sin un evento independiente
válido, cada posición restante pasa determinísticamente a
`BLOCKED_BY_PRIOR_SAMPLE`. Así siempre hay `4×2×N` posiciones y no existe un
operator abort limpio. `INVALID_EXPERIMENT_INPUT` es un NO-GO del instrumento
pre-start; una posición bloqueada nunca se marca unrun.

### 5.4 Función de eficacia

Mediana es el estadístico usual, promediando los dos centrales si son pares.
Para una métrica X y tarea t:

```text
off = median(OFF_t.X)
assisted = median(ASSISTED_t.X)

improves(t,X,limit):
  if off == 0: false
  else: assisted/off <= limit

neutral_D(X):
  if off == 0: assisted == 0
  else: 0.90 <= assisted/off <= 1.10
```

Los límites son inclusivos. Un empate ordinario en A/B/C no mejora; cero/cero
en A/B/C tampoco. Entonces:

```text
decide(plan, records, lineage):
  v = validate(plan, records, lineage)
  if v == INVALID_EXPERIMENT_INPUT: return INSTRUMENT_NO_GO
  if v == INSTRUMENT_NO_GO: return INSTRUMENT_NO_GO

  S = normalize(each preregistered position)

  quality = for every t in {A,B,C,D}:
      pass_count(ASSISTED,t,Q1) >= pass_count(OFF,t,Q1)
      and pass_count(ASSISTED,t,Q4) >= pass_count(OFF,t,Q4)
      and (t != C or
           pass_count(ASSISTED,C,Q6) >= pass_count(OFF,C,Q6))

  efficiency = for every t in {A,B,C}:
      improves(t,M1u,0.85)
      and (improves(t,M2,0.80) or improves(t,M3,0.80))

  negative_control = for every X in {M1u,M2,M3}: neutral_D(X)

  if quality and efficiency and negative_control: return GO
  return NO_GO
```

### 5.5 Resultado de lineage

La readiness nunca recibe sólo “el último batch”. Recibe la lineage append-only
de V4 §3. Un root permite como máximo `MAX_SUCCESSORS=1`, fijado antes del
primer `SAMPLE_START`. El successor sólo puede corregir el instrumento; no
puede cambiar tareas, brazos, N, umbrales, caps, modelos ni estimando.

```text
lineage_decide(lineage):
  if family registry/ledger invalid, missing, rewritten or has >1 successor:
      return NO_GO(reason=LINEAGE_INVALID)
  compute and retain decide(...) for every preregistration
  if any predecessor ended for an event not independently admissible by V4 §2:
      return NO_GO(reason=INVALID_PREDECESSOR)
  if terminal preregistration result == GO:
      return GO(with mandatory disclosed_lineage=true)
  return NO_GO(reason=TERMINAL_NOT_GO)
```

Todos los resultados intermedios, abortos y posiciones se publican. No existe
`ITERATE`, selección por subconjunto, replacement, relot ni regla secundaria.
M1, Q2, Q3, Q5, costes y análisis de sensibilidad son sólo diagnósticos.

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
