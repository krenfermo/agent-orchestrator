# Frente 3 / 3D — PRECONDITION_3D_PRACTICAL

Fecha: 2026-09-29 · Estado: **norma práctica; pendiente revisión estática REAL; no ejecutado.**

## 0. Autoridad y alcance

Este documento define el diseño experimental práctico. La única función
normativa, total y ejecutable está en [06-benchmark-plan.md](06-benchmark-plan.md)
§§4–7. En caso de diferencia, las reglas de normalización y decisión de 06
prevalecen. `3d-auth-design-v4.md` y sus prompts se conservan como historial de
una propuesta research-grade y **no son normativos para 3D-PRACTICAL**.

Pregunta única:

> ¿Project Memory ASSISTED reduce M1u, M2 y/o M3 frente a OFF sin degradar
> Q1/Q4/Q6 bajo condiciones controladas y representativas de AO?

Es una evaluación controlada de ingeniería para decidir si posteriormente se
considera activar Project Memory como opción opt-in, reversible y observada.
No pretende demostrar causalidad universal ni aislamiento perfecto del
proveedor.

No autoriza implementación, cuentas, credenciales, gates externos, ejecución
del lote ni activación de memoria. `PRECONDITION_3D = NO-GO` hasta revisión REAL
de Codex y una autorización posterior para la fase que corresponda.

## 1. Identidad del experimento y congelamiento

Antes de la primera posición se publica un manifest canónico, hasheado y
append-only. `experiment_id` es el SHA-256 de su representación canónica,
incluidos todos estos campos:

- estimand y arms;
- manifests/digests de tareas y oráculos;
- commit del fixture;
- commit AO;
- provider, modelo, versión y configuración;
- flujo worker/reviewer;
- caps por tarea y rol;
- thresholds y versión de la función de decisión;
- `N=5`, seed y schedule completo;
- Router OFF y reglas de contexto externo.

La canonización fija UTF-8, NFC, LF, orden de claves, representación de
números y digest del manifest. Labels, nombres de lote y versiones humanas son
metadata y no alteran identidad. Cualquier cambio en campo congelado crea otro
`experiment_id`; nunca puede reemplazar, ocultar ni convertir el resultado del
experimento anterior. El registro contiene todos los experiment IDs y sus
resultados; no existe “último batch bueno”.

El schedule contiene exactamente `4 × 2 × 5 = 40` posiciones: tareas A–D,
brazos OFF/ASSISTED, cinco posiciones por task/arm. Se genera con la seed
congelada, randomizado e interleaved dentro de bloques por tarea. Cada bloque
incluye ambos arms en orden aleatorio; nunca se ejecuta OFF completo seguido
por ASSISTED completo. El schedule se hashea antes de SAMPLE_START.

## 2. Condiciones por posición

La única diferencia intencional entre arms es el modo Project Memory.

- AO commit, fixture commit, tarea, modelo/config, flujo, caps, host y oráculos
  son iguales.
- Router está siempre OFF.
- OFF requiere cero bytes de attachment Project Memory.
- ASSISTED requiere el attachment esperado; se registra su digest y versión.
- External context está deshabilitado. Si un componente inevitable no puede
  apagarse, debe ser idéntico y verificable en ambos arms antes del lote; si no
  puede igualarse, el lote no comienza.
- MCP, apps/connectors, web, global memory y otras fuentes externas o de
  contexto AO se deshabilitan en ambos arms. La ausencia se comprueba por
  posición con el inventario congelado.

Cada posición usa conversación y session ID nuevas, working copy limpia del
mismo fixture commit, `AO_DATA_DIR` nuevo y runtime/provider local home nuevo
cuando aplique. No se reusa transcript, memoria, conversación, resultados ni
working copy de otra posición. El launcher comprueba los IDs/rutas antes de
start; teardown verifica que procesos locales de esa posición hayan terminado
antes de iniciar la siguiente. Una violación post-start es failure contado.

## 3. Frontera de inicio y fallos

Antes de SAMPLE_START, el validador comprueba AO/fixture/model/config/schedule,
la configuración común y la construcción representativa de los primeros
requests OFF y ASSISTED. Verifica que OFF contiene cero attachment y ASSISTED
contiene exactamente el attachment provisionado esperado, su digest y origen;
`externalContext=false`. Si falla cualquier contraste, el lote entero no
comienza y se registra `PRESTART_INVALID` con evidencia. No se inicia ninguna
posición ni se intenta reparar y continuar el mismo lote.

SAMPLE_START es el append durable inmediatamente anterior a habilitar la
posición para emitir requests. Después de él no existe `INSTRUMENT_NO_GO` ni
exclusión limpia: toda posición iniciada termina `COMPLETED` o con uno de los
failures del enum compartido en 06. Cualquier anomalía, incluidos defectos del
instrumento, tratamiento, telemetría, parser, timeout o ledger, se contabiliza
como failure conforme a la función total de 06.

Si sanction/provider failure u otro evento impide continuar, las posiciones
posteriores permanecen en schedule y lineage como
`BLOCKED_BY_PRIOR_POSITION`; reciben caps y calidad cero. No replacements,
relots, selective reruns, reducción/aumento de N ni borrado de posiciones. Un
intento posterior con cualquier manifest distinto es otro experimento completo
y enlazado, no sustitución.

## 4. Métricas, treatment trace y provider cache

M1u, M2 y M3 se recogen por posición y rol según 06. Se traza cada request
relevante, incluyendo retries. Cada registro incluye:

```text
experiment_id, sample_id, task, arm, role, call_index,
provider/model/version/config,
pre_adapter_representation_digest,
project_memory_attachment_present, attachment_digest, attachment_version,
origin (PROJECT_MEMORY when present; NONE when absent),
externalContext=false, other_AO_context_sources=absent_or_equalized,
input_tokens, cached_input_tokens, uncached_input_tokens,
retry_index, retry_cause, request_outcome, position_terminal_state
```

`position_terminal_state` usa el enum de 06 al terminar la posición; requests
anteriores llevan `null`. Cada request lleva además `request_outcome` según la
transición de 06: success, retryable, rate-limited, policy failure, terminal
provider failure o sanction.

El registro identifica la **representación enviada por AO/provider client**.
No afirma conocer los bytes que el proveedor decodificó internamente ni los
bytes vistos definitivamente por el modelo. Si el proveedor no expone cached y
uncached tokens por request, el valor se marca `UNAVAILABLE`, nunca cero ni
una estimación presentada como medida. M1u debe ser medible para poder puntuar
la función de 06; de lo contrario la posición falla o el preflight del lote no
permite iniciar, según el momento de detección.

Provider cache/shared state que no pueda aislarse se registra como
`RESIDUAL_CONFOUNDER`. Se reporta cached/uncached por request (si se expone),
task, arm, posición y orden temporal. M1u es primaria; M1 total es sensibilidad.
Cache cero observado no prueba aislamiento. No se afirma independencia de
identidades internas del proveedor.

M1 total, cache read/write, duración, coste cuando esté disponible, orden
temporal y provider errors son diagnósticos. Los errores se conservan, no se
filtran por brazo ni por resultado.

## 5. Q6 Practical

Q6 sólo aplica a task C; en A/B/D es literalmente `NA`. Antes de randomizar se
congelan `REVIEW_TARGET_SHA256`, digest por archivo, defectos obligatorios,
`causal_line` exacta y códigos válidos. El primary seeded defect debe ser rank
1 y señalar exactamente su `causal_line`; sus códigos deben coincidir con los
del oracle. `K=3` findings como máximo, fijado en el manifest. Duplicados por
archivo/línea/clase/códigos cuentan como falsos positivos y Q6=0. Todo finding
debe referirse al digest exacto del target y a líneas válidas.

Q6=1 si y sólo si el primary seeded defect es rank 1 con causal_line y códigos
exactos, todos los defectos obligatorios congelados están reportados, no se
excede K y hay cero duplicados o findings extra no preaceptados. No se usa IoU
como criterio primario. No hay adjudicación post-hoc que pueda cambiar Q6.
Hallazgos alternativos pueden registrarse aparte como observación descriptiva;
no cambian el oracle, Q6 ni la decisión.

## 6. Recursos deliberadamente fuera del diseño

3D-PRACTICAL no requiere ni ejecuta VMs por muestra, gateway/broker/ctl/helper
experimental, organizaciones o scopes experimentales, G0–G9, 60 clusters G4,
matriz G6 de hasta 2,640 ejecuciones, ni experimentos risk/sanction o de
aislamiento billing/parent/contract/device/TLS/routing. V4 queda únicamente
como historial/research-grade no normativo. No se exige aislamiento universal
del proveedor.

Infraestructura práctica: un runner secuencial con schedule y validación
pre-start, AO data dir aislado por posición, worktrees limpias, homes locales
aislados, captura de telemetría/request/treatment, oráculo Q4 fuera del alcance
del agente, ledger append-only y teardown local verificable. No se crean
cuentas ni credenciales. AO vivo/producción no se usa; el data dir no apunta a
producción.

## 7. Interpretación y claims

El único claim permitido es:

> Bajo el fixture, tareas, commit, modelos, configuración, caps, cuenta y
> ventana registrados, ASSISTED mostró/no mostró las mejoras predefinidas
> frente a OFF sin degradar la calidad definida.

El resultado no demuestra efecto universal; aislamiento de provider cache;
independencia interna de identities; aislamiento billing/risk/sanction/TLS/
routing; ni alta potencia estadística con N=5. El estado compartido del
proveedor es una limitación/covariable residual.

GO sólo autoriza considerar después una activación opt-in, reversible y con
telemetría. No autoriza rollout global.

**PRECONDITION_3D_PRACTICAL = READY_FOR_CODEX_REVIEW** (pendiente revisión REAL).
**PRECONDITION_3D = NO-GO.**
