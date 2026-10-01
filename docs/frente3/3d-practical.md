# Frente 3 / 3D — PRECONDITION_3D_PRACTICAL

Fecha: 2026-09-29 · Estado: **norma práctica corregida; pendiente nueva revisión estática REAL; no ejecutado.**

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
incluidos **todos** los campos del schema cerrado de
[06-benchmark-plan.md](06-benchmark-plan.md) §3. El envelope del ledger lleva
`{experiment_id, manifest}`; el manifest no contiene `experiment_id` y no hay
hash recursivo.

La canonización fija UTF-8, NFC, LF, orden de claves, representación de
números y digest del manifest. Labels, nombres de lote y versiones humanas son
metadata y no alteran identidad. Cualquier cambio en campo congelado crea otro
`experiment_id`; nunca puede reemplazar, ocultar ni convertir el resultado del
experimento anterior. El registro contiene todos los experiment IDs y sus
resultados; no existe “último batch bueno”.

El schedule contiene exactamente `4 × 2 × 5 = 40` posiciones: tareas A–D,
brazos OFF/ASSISTED, cinco posiciones por task/arm. Se genera con la seed,
PRNG, derivación y algoritmo de pares definidos en 06 §3. Cada tarea contiene
cinco pares; cada par tiene exactamente una posición OFF y una ASSISTED, con
orden derivado de la seed. El schedule completo se hashea antes de SAMPLE_START
y `decide()` regenera y compara el schedule entero.

## 2. Condiciones por posición

La única diferencia intencional entre arms es el modo Project Memory.

- AO commit, fixture commit, tarea, modelo/config, flujo, caps, configuración
  local relevante y oráculos son iguales. Esa igualdad se representa mediante
  el `EXECUTION_ENVIRONMENT_DIGEST` cerrado de 06 §3.6; OFF y ASSISTED usan
  exactamente el mismo expected digest.
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

El manifest fija un `CLOSED_ROLE_SET` y, para cada combinación alcanzable
`task × role × call_class`, contiene exactamente una entrada por arm. Los roles
se limitan al enum `planner`, `worker`, `reviewer`, `repair`, `summarizer`,
`helper`; las call classes al enum `initial`, `continuation`, `tool_result`,
`review`, `repair`, `summary`, `helper`, `retry`. El flow define cuáles son
alcanzables. Una celda ausente, extra o ambigua hace `PRESTART_INVALID`.

OFF exige `attachment_present=false`, sin attachment bytes ni spans de origen
Project Memory. Cada celda ASSISTED congela explícitamente si el attachment
está presente. Cuando está presente, la celda incluye SHA-256 y bytes exactos,
attachment version, provenance schema/version, construction/render version,
indexed commit, source manifest digest, freshness inputs y los demás parámetros
necesarios para reconstruir los mismos bytes. Cuando está ausente, el objeto es
exactamente `{attachment_present:false}`. Los bytes se guardan como artefacto
inmutable identificado por su digest y cubierto por el canonical manifest. Cada
request ASSISTED debe coincidir con su celda exacta; un OFF con cualquier
byte/span del attachment o un mismatch post-start es `MALFORMED_RESULT`. Toda
request role/call class fuera del mapping falla igual. El preflight valida todas
las celdas/artefactos congelados antes del primer SAMPLE_START.

## 3. Frontera de inicio y fallos

Antes de SAMPLE_START, el validador comprueba AO/fixture/model/config/schedule,
la configuración común, todos los blobs del treatment mapping y las
representaciones finales construidas para las celdas iniciales OFF/ASSISTED.
También recalcula el `EXECUTION_ENVIRONMENT_DIGEST` sin secretos y lo compara
con el expected digest común. Cada posición anexa su observed digest antes de
su SAMPLE_START y lo revalida cuando pueda cambiar una entrada allowlisted.
Verifica que OFF contiene cero attachment y ASSISTED coincide exactamente con
su celda provisionada, digest y origen; `externalContext=false`. Si falla
cualquier contraste, el lote entero no comienza y se registra
`PRESTART_INVALID` con evidencia. No se inicia ninguna
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

Todo provider attempt posterior a SAMPLE_START se traza, sin excepción, para
cualquier rol, incluidos retries, partial streams, worker, reviewer, repair,
fix, summarizer, helper y otros roles enumerados en el `CLOSED_ROLE_SET`. M1u y
M2 incluyen absolutamente todos esos attempts materializados válidamente. El
ledger físico es append-only: cada attempt se representa con exactamente un
evento `ATTEMPT_DISPATCHED` previo al envío y exactamente un evento
`ATTEMPT_FINALIZED` posterior, ambos con la misma identidad
`(sample_id, attempt_id, call_index)`. `call_index` es único y contiguo por
posición, asignado en orden de dispatch; partial stream es un attempt. Hueco,
duplicado, finalization ausente, identity mismatch, accounting ausente o
role/call class desconocido produce `MALFORMED_RESULT`.

Todos los roles reciben el mismo AO-owned `ObservedProviderClient`; sólo esa
frontera instrumentada puede invocar el provider SDK. Helpers no pueden
construir clientes alternos ni llamar directamente al SDK. Cada invocación
asigna el call_index y anexa `ATTEMPT_DISPATCHED` antes del dispatch. Después
anexa `ATTEMPT_FINALIZED`; nunca actualiza ni reemplaza el evento previo. La
materialización normativa une ambos tipos sólo cuando hay exactamente uno de
cada tipo con identidad coincidente. Cualquier otra cardinalidad o combinación
hace `MALFORMED_RESULT`.

La frontera de trace es el objeto final ya adaptado inmediatamente antes de
entregarlo al transport/provider SDK. Toda adaptación ocurre antes de esta
frontera. Se serializa el objeto final según el schema provider/request
congelado y se registra `SHA256(canonical_request_bytes)`; la traza extrae
attachment y contexto desde esos mismos bytes. Esos bytes/campos forman un
snapshot inmutable y ese mismo objeto se entrega al transport/SDK sin mutación
intermedia. `ATTEMPT_DISPATCHED` incluye:

```text
experiment_id, sample_id, task, arm, role, attempt_id, call_index, call_class,
provider/model/version/config,
final_post_adapter_representation_sha256,
project_memory_attachment_present, attachment_digest, attachment_version,
origin (PROJECT_MEMORY when present; NONE when absent),
externalContext=false, context_source_inventory_sha256, context_source_states,
retry_chain_id, retry_index, dispatch_timestamp
```

`ATTEMPT_FINALIZED` repite exactamente `experiment_id`, `sample_id`, `task`,
`arm`, `role`, `attempt_id`, `call_index` y `call_class`, y añade
`request_outcome`, `input_tokens`, `cached_input_tokens`,
`uncached_input_tokens`, retry metadata, terminal/provider metadata y
`finalized_timestamp`. `attempt_id` identifica una invocación de forma única.
`call_index` es un entero desde 1 sin huecos por posición, asignado justo antes
de dispatch. Un valor de accounting requerido ausente se registra `MISSING` y
fuerza `MALFORMED_RESULT`; no se interpreta como cero.

Attachment present/absent, digest/version, origin, external-context y otras
fuentes AO se leen desde ese mismo snapshot final. El registro identifica la
**representación final entregada por AO al transport/provider SDK**; no afirma
conocer los bytes que el proveedor decodificó internamente ni los bytes vistos
definitivamente por el modelo. Si el provider no devuelve usage, la
finalization indica explícitamente `MISSING` y la posición será
`MALFORMED_RESULT`, nunca se omite ni se registra como cero. M1u debe ser
medible; la disponibilidad del mecanismo se valida antes de SAMPLE_START.

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
congelan `REVIEW_TARGET_SHA256`, digest por archivo, defectos obligatorios con
`defect_id` único, `causal_line` exacta y códigos válidos. El campo obligatorio
`primary_defect_id` referencia exactamente uno de esos defectos. Campo missing,
`defect_id` duplicado o referencia inexistente hace `PRESTART_INVALID`. El finding rank 1
debe corresponder exactamente a `primary_defect_id`, incluida su línea, target
y códigos; no puede elegirse el primary después de ejecutar el lote. `K=3`
findings como máximo, fijado en el manifest. Duplicados por
archivo/línea/clase/códigos cuentan como falsos positivos y Q6=0. Todo finding
debe referirse al digest exacto del target y a líneas válidas.

Q6=1 si y sólo si el primary seeded defect es rank 1 en su `causal_line` exacta
con códigos exactos, todos los defectos obligatorios congelados aparecen una
vez en sus causal lines y con códigos exactos, el total de findings no excede
K y no hay duplicados ni findings extra. No se usa IoU. No hay adjudicación
post-hoc: cualquier finding extra es falso positivo para Q6. Los hallazgos
alternativos se conservan en la salida para descripción; nunca modifican el
oracle ni el score después de observar resultados.

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

**PRECONDITION_3D_PRACTICAL = READY_FOR_CODEX_REVIEW** (correcciones pendientes de nueva revisión REAL).
**PRECONDITION_3D = NO-GO.**
