# Frente 3 / 3D — Plan de medición y función de decisión

Fecha: 2026-09-29 · Estado: **norma 3D-PRACTICAL; pendiente revisión REAL; no ejecutado.**

Este documento contiene la única función normativa, total y ejecutable para
3D-PRACTICAL. [3d-practical.md](3d-practical.md) define el protocolo y debe
interpretarse junto con estas reglas. V1–V4 auth designs, prompts y preflight
son historial; no aportan reglas de muestreo, aislamiento o decisión.

Regla de medición: un número que AO no pudo medir nunca se presenta como
medido. Cero observado y dato no disponible son distintos.

## 1. Inventario de telemetría y límites

| Señal | Disponibilidad conocida | Nota para el piloto |
|---|---|---|
| Input tokens | AVAILABLE | `model_usage_events.input_tokens`; deltas Codex requieren validar monotonicidad |
| Uncached input tokens | AVAILABLE/PARTIAL por cliente | Métrica M1u; sin valor medido no se imputa como cero |
| Cache read/write | AVAILABLE/PARTIAL | Reportar sólo lo expuesto por provider; no inferir aislamiento |
| Llamadas | AVAILABLE | Contar todos los attempts/retries de la posición |
| Exploración/ficheros leídos | AVAILABLE tras 3C | Sólo paths/patrones y conteos; no contenido de archivos |
| Coste | PARTIAL | Diagnóstico cuando precio y tokens están disponibles |
| Duración | PARTIAL | Diagnóstico de pared, no criterio primario |
| Verify/review | AVAILABLE | Q1 y calidad descriptiva del reviewer |
| Treatment attachment | Requiere evidencia por posición/request | Digests, arm, rol y origen como especifica `3d-practical.md` |

Una señal no disponible se registra `UNAVAILABLE`; no se sustituye con cero ni
con una estimación rotulada como observación.

## 2. Pregunta, métricas y tareas

Estimand: diferencia práctica entre Project Memory `OFF` y `ASSISTED`, bajo el
fixture y la configuración congelados, en input fresco/no-cacheado, llamadas y
carga de exploración, sujeta a no degradar calidad.

| Métrica | Definición | Uso |
|---|---|---|
| M1u | Suma de `uncached_input_tokens` de todos los roles/requests/retries de una posición | Primaria |
| M2 | Número total de provider requests, incluidos retries y requests fallidas | Primaria |
| M3 | Exploración normalizada previa a primera edición (A/B/D, worker) o veredicto estructurado (C, reviewer): `max(c/C_cap, f/F_cap)` truncado a [0,1] | Primaria |
| M1 | Suma de input tokens incluidos cacheados | Diagnóstico y sensibilidad |
| Cached tokens | Suma reportada por provider | Diagnóstico/covariable |
| Q1 | Verify pasa | Calidad |
| Q4 | Tests ocultos pasan en oráculo fuera del alcance del agente | Calidad |
| Q6 | Localización Practical del seeded defect, sólo C; `NA` en A/B/D | Calidad |

`c` cuenta llamadas de exploración y `f` archivos distintos leídos/buscados
antes del hito. `C_cap`, `F_cap` y reglas del parser se congelan en el manifest.
Si falta el hito, el rol falla o falta un dato requerido, M3=1 y la posición se
clasifica según la tabla de estados/normalización. M1, duración, coste, cache,
errores del provider, turnos de fix y veredicto final del reviewer son
diagnósticos; no cambian GO.

Tareas congeladas en el manifest:

| Task | Tipo | Calidad adicional |
|---|---|---|
| A | Bugfix localizado, 1–3 archivos | Q1, Q4 |
| B | Cambio transversal endpoint/datos/tests | Q1, Q4 |
| C | Review de diff con defecto sembrado | Q1, Q4, Q6 |
| D | Control: editar archivo nombrado, sin exploración necesaria | Q1, Q4; neutralidad de eficiencia |

Q6 es `NA` (no faltante) fuera de C. Su definición primaria está en
[3d-practical.md](3d-practical.md) §5.

## 3. Diseño congelado

El manifest debe congelar antes del lote: AO commit; fixture commit; tasks y
oracles/digests; arms; provider/model/version/config; worker/reviewer flow;
caps por task/role; thresholds; version de esta función; seed; schedule;
`N=5`; Router OFF; política de contexto externo; métricas y reglas de parsing.
El `experiment_id` es el digest canónico del conjunto, como define
[3d-practical.md](3d-practical.md) §1. Un label humano es metadata.

El schedule tiene exactamente 40 posiciones (`A–D × OFF/ASSISTED × 5`). Se
randomiza e intercala dentro de bloques por tarea. Cada bloque contiene ambos
arms con orden aleatorio y seed congelada. Nunca se procesa un arm completo
antes del otro.

Por posición: conversación/session ID nueva, working copy limpia del mismo
fixture commit, AO data dir nuevo y runtime/provider local home nuevo cuando
aplique. No se comparte historial, transcript, resultado o memoria entre
posiciones. Router OFF. OFF no envía attachment Project Memory; ASSISTED envía
el attachment esperado con digest/version registrados. External context, MCP,
apps/connectors, web, global memory y otras fuentes AO se deshabilitan o se
igualan y verifican por posición. El tratamiento y la ausencia de fuentes
adicionales se trazan por request. El provider cache/shared state no aislable
se registra `RESIDUAL_CONFOUNDER`; no se atribuye a aislamiento.

## 4. Vocabulario único de estado y transición

El siguiente enum es el único vocabulario terminal por posición usado por el
diseño y este benchmark:

```text
COMPLETED
FAILED_WORKER
FAILED_REVIEWER
TIMEOUT
PROVIDER_RETRY_EXHAUSTED
PROVIDER_RATE_LIMITED
PROVIDER_POLICY_FAILURE
PROVIDER_TERMINAL_FAILURE
PROVIDER_SANCTION
MALFORMED_RESULT
BLOCKED_BY_PRIOR_POSITION
```

Transiciones de request y posición:

| Evento/request | Retry | Resultado terminal de posición |
|---|---|---|
| Respuesta normal válida | Ninguno | continúa; posición acaba COMPLETED si flujo y oráculos concluyen |
| Error transitorio retryable | Dentro del presupuesto fijo del cliente; cada request cuenta | `PROVIDER_RETRY_EXHAUSTED` al agotar |
| Rate limit | Dentro del presupuesto fijo específico; cada request cuenta | `PROVIDER_RATE_LIMITED` al agotar |
| Policy failure | Ninguno | `PROVIDER_POLICY_FAILURE` |
| Error terminal provider/stream parcial no replayable | Ninguno | `PROVIDER_TERMINAL_FAILURE` |
| Provider sanction | Ninguno | `PROVIDER_SANCTION`; posiciones siguientes bloqueadas si no pueden continuar |
| Falla de worker/reviewer | Ninguno de muestra | `FAILED_WORKER` / `FAILED_REVIEWER` |
| Deadline congelado excedido | Ninguno | `TIMEOUT` |
| Campos, trace o estado ausente/malformado/contradictorio | Ninguno | `MALFORMED_RESULT` |
| Una posición previa impide ejecutar la posición no iniciada | No aplica | `BLOCKED_BY_PRIOR_POSITION` |

Cada fila de request conserva su `request_outcome` (`SUCCESS`, `RETRYABLE`,
`RATE_LIMITED`, `POLICY_FAILURE`, `TERMINAL_PROVIDER_FAILURE` o
`PROVIDER_SANCTION`). Esos outcomes alimentan la tabla de transición; no son
estados terminales adicionales de posición. La posición final conserva un
único valor del enum anterior.

Los errores retryable/rate sólo reintentan requests dentro de la misma
posición, con presupuesto, backoff y límites congelados en el manifest. No
existe retry de posición. Toda clase de failure y bloqueo recibe imputación
completa de caps y calidad cero. Si el provider informa sanction, es terminal,
no retryable y failure. No se infiere sanction a partir de un error ambiguo:
ese caso es `PROVIDER_TERMINAL_FAILURE` o `MALFORMED_RESULT` según evidencia.

`SAMPLE_START` precede cualquier request. Antes de empezar el lote se valida
construcción OFF/ASSISTED, external context y manifest. Un fallo pre-start
impide iniciar y queda registrado `PRESTART_INVALID`; no es una posición ni
resultado puntuable. Después de SAMPLE_START no existe `INSTRUMENT_NO_GO`.
Toda posición iniciada termina COMPLETED o en failure. El schedule es fijo y
todas sus posiciones aparecen en el ledger.

## 5. Caps y normalización total

Cada tarea/rol tiene enteros congelados positivos o ambos cero si el rol no
aplica:

```text
TOKEN_CAP_ROLE[task,role] = máximo uncached input tokens del rol
CALL_CAP_ROLE[task,role]  = máximo requests del rol, incluidos retries
```

Para una posición COMPLETED, se valida individualmente, antes de agregar:

```text
M1u_role <= TOKEN_CAP_ROLE[task,role]
M2_role  <= CALL_CAP_ROLE[task,role]
M1u = Σ M1u_role
M2  = Σ M2_role
```

Un exceso de cualquier cap hace `MALFORMED_RESULT` y aplica imputación de
failure. No se permite compensar sobreconsumo de un rol con subconsumo de otro.
Requests parciales y retries cuentan para los caps. Los registros extra se
conservan como evidencia, pero no crean ni eliminan posiciones.

Para cada posición `normalize` retorna exactamente
`(M1u,M2,M3,Q1,Q4,Q6,state)`:

- COMPLETED con todos los campos requeridos, tipos, dominios, trazas y caps
  válidos conserva sus valores observados.
- Cualquier otro estado, dato null/no disponible requerido, dato malformado,
  trace incompleto, digest/arm incorrecto o cap excedido retorna:

```text
M1u = Σ TOKEN_CAP_ROLE[task, role]
M2  = Σ CALL_CAP_ROLE[task, role]
M3  = 1
Q1  = 0
Q4  = 0
Q6  = 0 si task=C; NA si task∈{A,B,D}
```

Las posiciones `BLOCKED_BY_PRIOR_POSITION` usan la misma imputación. Una
posición iniciada con estado o evidencia ilegible es `MALFORMED_RESULT`, nunca
desaparece. El ledger incluye siempre las 40 posiciones programadas.

Dominio de campos válidos: M1u/M2 enteros no negativos dentro del total de
caps, M3 finito en `[0,1]`, Q1/Q4 booleanos y Q6 booleano sólo en C o literal
`NA` en A/B/D. Cualquier null, NaN, infinito, tipo inesperado o valor fuera de
dominio invalida esa posición.

## 6. Función de decisión única

Mediana es el estadístico de cada task/arm, con promedio de centrales si N es
par. Para `X` en M1u, M2 o M3:

```text
off = median(OFF[task].X)
assisted = median(ASSISTED[task].X)

improves(task,X,limit):
  if off == 0: false
  else: assisted / off <= limit

neutral_D(X):
  if off == 0: assisted == 0
  else: 0.90 <= assisted / off <= 1.10
```

Los límites son inclusivos. El empate no cuenta como mejora en A/B/C; cero
frente a cero tampoco.

```text
decide(manifest, ledger):
  if manifest invalid or not frozen before first SAMPLE_START:
      return NO_GO(reason=PRESTART_INVALID)
  if experiment_id != canonical_digest(manifest):
      return NO_GO(reason=IDENTITY_MISMATCH)
  if schedule is not exactly 40 frozen positions:
      return NO_GO(reason=SCHEDULE_INVALID)
  if any position absent, duplicated, replaced, relotted or selectively rerun:
      return NO_GO(reason=LINEAGE_INVALID)

  S = normalize(each of the 40 scheduled positions)

  quality = for every task t in {A,B,C,D}:
      pass_count(ASSISTED,t,Q1) >= pass_count(OFF,t,Q1)
      and pass_count(ASSISTED,t,Q4) >= pass_count(OFF,t,Q4)
      and (t != C or
           pass_count(ASSISTED,C,Q6) >= pass_count(OFF,C,Q6))

  efficiency = for every task t in {A,B,C}:
      improves(t,M1u,0.85)
      and (improves(t,M2,0.80) or improves(t,M3,0.80))

  negative_control = for X in {M1u,M2,M3}: neutral_D(X)

  if quality and efficiency and negative_control:
      return GO
  return NO_GO
```

Un prestart inválido impide el lote y no puede corregirse después de observar
una posición. Tras el primer SAMPLE_START toda entrada de las 40 posiciones
debe estar terminal; si la ejecución se detiene, las restantes se marcan
`BLOCKED_BY_PRIOR_POSITION`. Se decide determinísticamente sobre la lineage
completa. No hay excepción manual, revisión de casos para cambiar puntos,
subconjunto favorable ni análisis secundario que convierta NO_GO en GO.

## 7. Registro y salida

El ledger append-only conserva experiment_id, manifest y digests, schedule,
seed, cada SAMPLE_START/terminal, requests y retries, treatment digests,
metrics, outputs/evidencias, provider errors, failures, bloqueos y decisión
final. Un error al guardar después de SAMPLE_START se refleja como failure y
no autoriza omitir una posición. La publicación incluye todas las posiciones,
diagnósticos, disponibilidad de señales y el indicador
`RESIDUAL_CONFOUNDER` con la distribución de cache observada.

El claim permitido, límites de interpretación y efecto de GO están definidos
en [3d-practical.md](3d-practical.md) §7. GO sólo permite considerar más
adelante activar opt-in, reversible y con telemetría; no autoriza rollout
global.

## 8. Telemetría histórica de 3C

La tabla inicial de disponibilidad es un inventario observado durante 3C, no
una garantía futura para todos los providers. Revalidar disponibilidad en el
preflight del manifest; si una señal primaria requerida no puede medirse, no
iniciar el lote. Las diferencias se documentan en el manifest como
`UNAVAILABLE`, sin afirmar medición.

**PRECONDITION_3D_PRACTICAL = READY_FOR_CODEX_REVIEW** (pendiente revisión REAL).
