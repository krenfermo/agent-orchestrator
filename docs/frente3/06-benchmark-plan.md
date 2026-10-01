# Frente 3 / 3D — Plan de medición y función de decisión

Fecha: 2026-09-29 · Estado: **norma 3D-PRACTICAL corregida; pendiente nueva revisión REAL; no ejecutado.**

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

### 3.1 Schema normativo cerrado del manifest

El manifest es un objeto JSON `ao.3d-practical.manifest.v2`. El siguiente
inventario es exhaustivo: todos los campos son obligatorios y no nullable salvo
que se indique expresamente. En este objeto y en todos los objetos anidados se
aplica `additionalProperties=false`; unknown field, campo requerido ausente,
tipo/formato/dominio incorrecto, array con cardinalidad/orden inválido o null
inesperado da `PRESTART_INVALID`. No se rellenan defaults del provider, SDK,
cliente, AO, parser ni runner. Todo comportamiento efectivo debe tener valor
explícito en el manifest. El schema normativo es este inventario exhaustivo
bajo `ao.3d-practical.manifest.v2`; `manifest_schema_sha256` es SHA-256 del
texto canonizado de §§3.1–3.7 (UTF-8 NFC, LF), excluyendo únicamente el valor
serializado de ese campo. El validador recalcula y compara ese digest, y aplica
estas reglas exactamente sin defaults adicionales.

Tipos base: `string` UTF-8 NFC; `sha256` es 64 lowercase hex; `git_commit` es
40 lowercase hex; `uint` entero JSON no negativo; `positive_uint` entero JSON
mayor que cero; `bool` JSON booleano; enums son exactos y case-sensitive.
Arrays no permiten duplicados salvo `schedule` y listas cuya repetición está
definida abajo.

| Campo raíz | Tipo y restricciones |
|---|---|
| `schema_version` | const `ao.3d-practical.manifest.v2` |
| `manifest_schema_sha256` | digest del texto normativo §§3.1–3.7 en UTF-8 NFC/LF |
| `estimand` | const `project_memory_assisted_vs_off_v1` |
| `ao_commit`, `fixture_commit` | `git_commit` completos |
| `tasks` | array exacto en orden A,B,C,D; cada item `{task_id: enum A-D, task_manifest_sha256: sha256, fixture_subtree_sha256: sha256, oracle_ref: sha256, treatment_target_roles: nonempty unique array<CLOSED_ROLE_SET>}`; cada target role debe tener al menos una celda ASSISTED con attachment presente |
| `arms` | tuple exacto `[`"OFF"`, `"ASSISTED"`]` |
| `treatment_mapping` | array cerrado que contiene una y sólo una celda por combinación alcanzable `task × role × call_class`; schema §3.2 |
| `provider` | objeto cerrado según §3.3 |
| `provider_access_boundary` | const `AO_OBSERVED_CLIENT_ONLY_V1`; roles sólo reciben el cliente instrumentado; SDK handle/credencial no es accesible desde role/helper code fuera de esa frontera |
| `invocation_config_mapping` | array cerrado, exactamente una fila por celda alcanzable `task × role × call_class`; item `{task_id,role,call_class,model_id,model_version,effective_config_schema,effective_config_sha256,effective_config}` |
| `CLOSED_ROLE_SET` | array no vacío, sin duplicados, ordenado según enum fijo: `planner`, `worker`, `reviewer`, `repair`, `summarizer`, `helper` |
| `workflow` | objeto cerrado: `worker_flow_version: string`, `reviewer_flow_version: string`, `task_roles: array` exacto A-D; item `{task_id, role_flow: nonempty array<{role:CLOSED_ROLE_SET,flow_position:positive_uint,reachable_call_classes:nonempty array<enum §3.2>}>}`; flow_position contiguo desde 1 |
| `TOKEN_CAP_ROLE`, `CALL_CAP_ROLE` | arrays completos en orden task A-D y `CLOSED_ROLE_SET`; item `{task_id, role, cap: uint}`; roles no usados deben cap=0 en ambas tablas |
| `retry_policy` | objeto cerrado según §3.4; por role y request outcome, sin herencia ni defaults |
| `execution_environment` | objeto cerrado según §3.6; contiene el único expected `EXECUTION_ENVIRONMENT_DIGEST` común a OFF y ASSISTED y sus inputs reproducibles, sin secretos |
| `deadlines` | objeto cerrado `{position_seconds: positive_uint, provider_attempt_seconds: positive_uint, role_seconds: array<{role,seconds}>}` completo por cada role |
| `instrument` | `{schema_version:string,attempt_event_schema_version:string,execution_environment_digest_version:string,provider_request_schema_version:string,exploration_parser_version:string,verify_version:string,q4_runner_version:string,q6_scorer_version:string}`; versiones/digests exactos |
| `M3_caps` | array completo task-role; item `{task_id,role,C_cap:uint,F_cap:uint}`; exactamente un rol medido por task (worker A/B/D, reviewer C) tiene ambos positivos, los demás ambos cero |
| `thresholds` | objeto cerrado según §6 con los valores const allí definidos |
| `decision_rule_version` | const `ao.3d-practical.decision.v2` |
| `N` | const integer 5 |
| `randomization` | objeto cerrado según §3.5, incluye PRNG, raíz/derivación de seed y schedule completo |
| `Router` | const `OFF` |
| `external_context` | objeto cerrado según §3.6 |
| `context_source_inventory` | array completo, cerrado según §3.6 |
| `Q1_oracle`, `Q4_oracle`, `Q6_oracle` | objetos cerrados según §3.7, metadata requerida para cada tarea |
| `position_isolation` | consts/explicit values para `new_conversation=true`, `new_session_id=true`, `clean_working_copy=true`, `new_AO_DATA_DIR=true`, `new_runtime_provider_home_when_applicable=true`, `no_prior_transcript_memory_or_results=true`, `local_process_teardown=verified_before_next_position` |

El envelope almacenado junto al manifest es `{experiment_id:sha256,
manifest_sha256:sha256,manifest:object,metadata:{human_label:string|null,
human_version:string|null}}`; no permite fields adicionales. `experiment_id` y
`manifest_sha256` son ambos SHA-256 de `canonical_bytes(manifest)`; no se
incluyen en el objeto hasheado. Labels quedan fuera del canonical manifest y
de toda decisión. Canonización: JSON UTF-8 NFC, LF, claves ordenadas
lexicográficamente por bytes UTF-8, sin whitespace irrelevante, números
enteros decimales sin signo `+` ni ceros iniciales, strings JSON escapadas por
RFC 8259; floats, NaN e Infinity no están permitidos. Arrays conservan el
orden normado indicado.

Los cuatro campos añadidos por esta corrección —Q6 primary, lifecycle de
attempt, retry budgets unívocos y execution environment— forman parte del
manifest v2 canónico. Reutilizar el `experiment_id` de cualquier manifest
anterior, u omitir uno de esos cambios conservando su ID, falla la igualdad con
`canonical_bytes(manifest)` y no puede iniciar ni sustituir un lote previo.

### 3.2 Treatment mapping cerrado

`call_class` es exactamente uno de `initial`, `continuation`, `tool_result`,
`review`, `repair`, `summary`, `helper`, `retry`. Cada entry es un objeto
cerrado `{task_id, role, call_class, OFF, ASSISTED}`. La matriz debe coincidir
exactamente con las celdas alcanzables del workflow; roles/clases desconocidos,
faltantes o extra invalidan el manifest.

`OFF` es exactamente `{attachment_present:false}`; attachment, digest, versión
y provenance spans son campos prohibidos por el schema de esa variante.
`ASSISTED` contiene la propiedad obligatoria `attachment_present:bool`. Si es
false, el objeto sólo puede contener esa propiedad. Si es true, contiene
`attachment_sha256`, `attachment_artifact_ref`, `attachment_version`,
`provenance_schema_version`, `construction_version`, `render_version`,
`indexed_commit`, `source_manifest_sha256`, `freshness_inputs_sha256` y
`origin=PROJECT_MEMORY`. `attachment_artifact_ref` referencia bytes completos
inmutables cuyo digest coincide con `attachment_sha256`; el blob es parte del
contenido cubierto por el digest canónico (directamente embebido o mediante un
manifest de blobs content-addressed incluido en el canonical bytes set).
Freshness inputs y source manifest permiten reconstruir la construcción; los
bytes exactos permiten verificarla. Toda request ASSISTED se coteja con la
celda task/role/call_class y artifact congelados. OFF requiere ausencia literal
en el snapshot final. Mismatch después de SAMPLE_START es `MALFORMED_RESULT`.

### 3.3 Provider y configuración efectiva

`provider` es `{provider_id, account_ref_sha256, client_id, client_version,
provider_api_version}`. Para cada invocation config mapping, `effective_config_schema`
identifica una definición
versionada y cerrada de todos los parámetros que el provider/SDK/client puede
tomar por default, incluyendo sampling, output/context limits, tools, streaming,
reasoning, compaction, timeout y request headers con relevancia semántica.
Cada `effective_config` debe aportar cada parámetro definido por esa versión,
incluidos los no aplicables con su valor explícito `null` sólo si el schema lo
declara nullable; su digest debe coincidir con la canonicalización de ese
objeto. El mapping debe tener una fila por celda alcanzable, sin default ni
fallback entre filas. Parámetro desconocido/no representado o default implícito
produce `PRESTART_INVALID`. `account_ref_sha256` identifica de forma estable la
misma cuenta existente en ambos arms; no contiene credenciales.

### 3.4 Retry y backoff

`retry_policy` es el objeto cerrado `{algorithm_version,retry_budgets}`. El
array `retry_budgets` contiene exactamente una fila por cada role del
`CLOSED_ROLE_SET`, sin filas adicionales, y `role` es unique. Cada fila es el
objeto cerrado `{role,retryable_max_retries:uint,
rate_limited_max_retries:uint,backoff_policy}`. `backoff_policy` es el objeto
cerrado `{algorithm:"exponential_capped_v1",base_delay_ms:positive_uint,
max_delay_ms:positive_uint,jitter_algorithm:"none_v1"}` y calcula
`min(base_delay_ms × 2^(attempt_index-1), max_delay_ms)`. No hay herencia,
fallback ni default. Role ausente/desconocido o duplicado produce
`PRESTART_INVALID`.
El presupuesto cuenta retries además del primer attempt. No hay gateway retries
ni retries de posición; la política de retry transparente del SDK vale cero y
se serializa explícitamente en `effective_config`. Un outcome provider distinto
de retryable/rate no se retrya. Cada retry tiene `retry_chain_id` del logical
call original y `retry_index` contiguo por outcome dentro de esa chain; el
primer retry tiene índice 1 y éste es el exponente usado por backoff.

### 3.5 PRNG, seed y schedule pareado

`randomization` es `{prng_algorithm:"ChaCha20-IETF",prng_version:"RFC8439-v1",
seed_hex:64-lowercase-hex,root_seed_generation:"OS_CSPRNG_32_bytes_before_manifest_v1",
task_stream_derivation:"sha256-task-domain-v1",
schedule_algorithm:"task-paired-off-assisted-v1",schedule_version:1,
orientation_balance:"2_3_each_task",
schedule:[40 entries]}`. Para cada task en orden A,B,C,D y pair index 1–5,
derivar key-task = `SHA256(UTF8("AO-3D-PRACTICAL-TASK-v1") || seed_bytes ||
UTF8(task_id))`. Inicializar ChaCha20-IETF con key-task, nonce de 12 cero
bytes y counter=0; interpretar el keystream como palabras uint32 little-endian
consecutivas consumidas una sola vez en orden. Consumir primero la palabra 0;
su bit 0 elige cuántos pares usan OFF primero: 2 si es 0, 3 si es 1.
Fisher-Yates permuta `[1,2,3,4,5]` desde i=4 hasta i=1 usando palabras
siguientes y rejection sampling (`limit=floor(2^32/(i+1))*(i+1)`; rechazar
x>=limit; j=x mod (i+1)); una palabra rechazada también se consume. Los
primeros k índices de la permutación son OFF-first; los restantes son
ASSISTED-first. Así cada task tiene ambos órdenes y una diferencia de a lo
sumo uno. La schedule enumera task A-D, pair 1-5, orden
derivado, con `position_index` 1–40 contiguo. Cada entry es
`{position_index:uint, task_id, pair_index:uint 1..5, pair_order:[arm,arm],
arm, sample_id:sha256}`; sample IDs son
`SHA256(UTF8("AO-3D-PRACTICAL-SAMPLE-v1") || seed_bytes || UTF8(task_id) ||
uint8(pair_index) || UTF8(arm))`. Cada par tiene dos entries consecutivos y
exactamente OFF/ASSISTED. La semilla raíz son 32 bytes obtenidos una sola vez
por OS CSPRNG antes de congelar el manifest; nunca se prueban seeds alternativas
contra resultados de schedule. `decide()` regenera los 40 entries de la seed y
compara igualdad estructural exacta con la schedule canónica. Existencia de 40
filas sin igualdad exacta es `SCHEDULE_INVALID`.

### 3.6 Entorno de ejecución, Router y contexto

`execution_environment` es el objeto cerrado
`{digest_schema_version,expected_execution_environment_digest,inputs}`.
`inputs` es el objeto cerrado y canónico
`{os_platform_arch,ao_binary_sha256,ao_commit,runtime_versions,
provider_client_cli_versions,task_tool_versions,
effective_environment_config_allowlist,runner_instrument_versions,
additional_local_configuration}`. `os_platform_arch` es el objeto cerrado
`{os,platform,arch}`. Cada lista de versiones contiene objetos cerrados
`{component,version,binary_sha256}` ordenados por component. Las dos listas de
configuración contienen objetos cerrados `{name,effective_value_or_sha256}`
ordenados por name; `additional_local_configuration` enumera cualquier otra
configuración local capaz de modificar prompts, exploración, ejecución o
resultados. Los valores son efectivos y explícitos, pero excluyen secretos,
credenciales y tokens; cuando corresponda se usa sólo un digest estable no
reversible o un identificador no secreto. Una entrada relevante no allowlisted
produce `PRESTART_INVALID`. El expected digest es
`SHA256(canonical_bytes({digest_schema_version,inputs}))`.
Esto congela sólo configuración local relevante para la comparación; no es un
fingerprint completo del host ni un mecanismo de aislamiento.

Existe un único expected digest raíz, por lo que OFF y ASSISTED deben compartir
exactamente el mismo valor. El preflight lo reproduce antes del lote. Además,
cada posición anexa un evento `EXECUTION_ENVIRONMENT_OBSERVED` con
`{experiment_id,sample_id,position_index,observed_digest,timestamp}` antes de
`SAMPLE_START`, y vuelve a observarlo después de cualquier frontera donde una
entrada allowlisted pueda cambiar y antes del terminal. Divergencia detectada
antes del primer `SAMPLE_START` impide el lote con `PRESTART_INVALID`;
una vez que existe el primer `SAMPLE_START` del lote, cualquier divergencia
(incluso la observada antes del start de una posición posterior), observación
ausente/duplicada o cambio detectado produce `MALFORMED_RESULT` para la
posición afectada.

Router, external context y contexto se definen así:

`external_context` es `{policy, equalized_sources}`; policy es `DISABLED` o
`EQUALIZED`. En DISABLED, `equalized_sources=[]`; en EQUALIZED, es una lista no
vacía ordenada de `{source_id,representation_sha256}`. Inventory tiene
exactamente los sources
`project_memory`, `router`, `mcp`, `apps_connectors`, `web`, `global_memory`,
`AO_external_evidence`, `provider_tools` y `other`. Cada item cerrado es
`{source_id,state,verification_version}`; `other` añade `inventory_sha256`.
State es `OFF` para router; Project Memory se rige sólo por treatment mapping;
los demás sources son `DISABLED` o `EQUALIZED`. Si un source está `EQUALIZED`,
su objeto requiere `representation_sha256` y debe aparecer con el mismo digest
en `external_context.equalized_sources`; si no, ese campo está prohibido.
`other` debe digerir las fuentes adicionales inspeccionadas. Una fuente AO no
catalogada antes del lote da `PRESTART_INVALID`. La igualdad se comprueba en
cada posición y por ambos arms. `externalContext` AO permanece literal `false`
en todos los requests; EQUALIZED sólo describe fuentes de contexto adicionales
que no son Project Memory.

### 3.7 Metadata de oráculos

`Q1_oracle` contiene `{version, verify_command_sha256,
accepted_exit_codes:nonempty_unique_array<uint>}`;
`Q4_oracle` contiene `{version, runner_image_or_binary_sha256, command_sha256,
timeout_seconds, task_oracles:[{task_id,hidden_test_manifest_sha256}]}` con
una fila por task A-D. `Q6_oracle`
contiene `{version,review_target_sha256,file_manifest_sha256,
primary_defect_id,mandatory_defects:[{defect_id,target_sha256,file,file_sha256,
causal_line:uint,defect_class,cause_code,impact_code}],K:3}`. `defect_id` es
string no vacío y unique dentro de `mandatory_defects`; `primary_defect_id` es
obligatorio y debe referenciar exactamente uno de esos IDs. Campo ausente,
`defect_id` duplicado o referencia inexistente produce `PRESTART_INVALID`.
`target_sha256` debe ser igual a `review_target_sha256`; `file` es path relativo UTF-8 NFC sin `..`,
`file_sha256` coincide con el target y `causal_line` cae dentro del archivo.
`defect_class` es `AUTHORIZATION_BYPASS`, `INPUT_VALIDATION`,
`STATE_TRANSITION`, `DATA_INTEGRITY`, `CONCURRENCY`, `ERROR_HANDLING`,
`RESOURCE_LIFECYCLE`, `API_CONTRACT` o `SECURITY_BOUNDARY`; `cause_code` es
`MISSING_GUARD`, `WRONG_PREDICATE`, `WRONG_TARGET`, `STALE_STATE`,
`UNSAFE_DEFAULT`, `MISSING_CLEANUP`, `NON_ATOMIC_UPDATE` o `ERROR_DROPPED`;
`impact_code` es `UNAUTHORIZED_ACCESS`, `INCORRECT_RESULT`, `DATA_LOSS`,
`STATE_CORRUPTION`, `RACE`, `RESOURCE_LEAK`, `CRASH` o
`CONTRACT_VIOLATION`. Los enums/códigos Q6 están cerrados por la
versión `ao.q6.practical.v2`, con al menos un mandatory defect y no más de K.
Definición de líneas, archivos y códigos exactos queda dentro del digest del
manifest Q6 y no puede modificarse post-start. No hay adjudicación posterior;
cualquier finding extra no duplicado se reporta como alternativo descriptivo,
cuenta como falso positivo para Q6 y no puede cambiarse luego. El finding rank
1 debe corresponder exactamente al mandatory defect identificado por
`primary_defect_id`, incluidos file/digest, causal_line, clase y códigos. El
orden del array no designa primary y ningún resultado puede cambiar esa
referencia. En A/B/D Q6 es
NA pero se congela el mismo Q6 schema/version para evitar defaults divergentes.

Schedule y provider config siempre se interpretan conforme a esta versión. No
se permite que el proveedor, SDK o cliente seleccione un default no serializado.

`validate_manifest(manifest)` es true si y sólo si: schema version es el const
del schema y schema digest coincide exactamente con el digest recalculado
según §3.1; existen todos y sólo los campos raíz/anidados
de esta sección con tipos, cardinalidades, formatos y enums válidos; todos los
cross-references, digests, blob bytes, treatment/config/workflow/cap/retry/
deadline/environment/context/oracle matrices son completos y coinciden;
`retry_budgets` tiene exactamente una fila unique por cada closed role;
`primary_defect_id` referencia exactamente un mandatory defect; thresholds son los
const de §6; `N=5`; el schedule es idéntico a la regeneración de §3.5; y no hay
defaults implícitos. Todo otro input es false y recibe `PRESTART_INVALID` si se
detecta antes del primer SAMPLE_START. La función no intenta completar o
reparar campos inválidos.

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

`ObservedProviderClient` es la única frontera autorizada para invocar el SDK;
el boundary de dependencias/credenciales hace que role/helper code no tenga
SDK handle ni credencial con los que eludirlo, y el preflight rechaza una
composición distinta de `AO_OBSERVED_CLIENT_ONLY_V1`. No hay llamadas directas
de helpers/roles ni retries automáticos internos del SDK. El ledger físico
nunca actualiza ni reemplaza eventos. Antes de enviar, cada dispatch anexa
exactamente un `ATTEMPT_DISPATCHED` con `sample_id`, `attempt_id`, `call_index`,
role/call_class, digest de la representación final, metadata de
treatment/context y timestamp. Tras obtener respuesta o error, anexa
exactamente un `ATTEMPT_FINALIZED` con la misma identity completa más
`request_outcome`, `input_tokens`, `cached_input_tokens`,
`uncached_input_tokens`, retry metadata, terminal/provider metadata y
timestamp.

La vista/materialización normativa produce un attempt si y sólo si encuentra
exactamente un `ATTEMPT_DISPATCHED` y exactamente un `ATTEMPT_FINALIZED` para
`(sample_id,attempt_id,call_index)`, y todos los campos de identity repetidos
coinciden. `attempt_id` es único por posición y `call_index` enumera
contiguamente desde 1 en orden de dispatch para toda la posición, no por rol.
Finalization ausente o duplicada, dispatch duplicado, identity mismatch o
accounting ausente/`MISSING` fuerza `MALFORMED_RESULT`; nunca se rellena con
cero ni se muta el evento de dispatch. Un índice no contiguo, rol/clase fuera
del CLOSED_ROLE_SET/mapping o attempt observado sin ambos eventos también hace
la posición `MALFORMED_RESULT`. Un mismatch detectado sólo después de
SAMPLE_START nunca es `PRESTART_INVALID`.

Los errores retryable/rate sólo reintentan requests dentro de la misma
posición, con presupuesto, backoff y límites congelados en el manifest. No
existe retry de posición. Toda clase de failure y bloqueo recibe imputación
completa de caps y calidad cero. Si el provider informa sanction, es terminal,
no retryable y failure. No se infiere sanction a partir de un error ambiguo:
ese caso es `PROVIDER_TERMINAL_FAILURE` o `MALFORMED_RESULT` según evidencia.

`SAMPLE_START` precede cualquier request. Antes de empezar el lote se valida
construcción OFF/ASSISTED, external context, el expected
`EXECUTION_ENVIRONMENT_DIGEST` y el manifest. Cada posición registra su
observed digest antes de SAMPLE_START. Un fallo pre-start
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

M2 suma exactamente uno por attempt materializado válidamente, incluidos retries,
requests terminales y streams parciales; no cuenta turnos conversacionales.
M1u suma `uncached_input_tokens` de cada attempt materializado válidamente y M1
suma `input_tokens` de esos mismos attempts. Ninguna métrica se calcula desde
eventos sueltos o materializaciones inválidas. Cada role se agrega por separado antes de validar y luego se suma
al nivel de posición.

Un exceso de cualquier cap hace `MALFORMED_RESULT` y aplica imputación de
failure. No se permite compensar sobreconsumo de un rol con subconsumo de otro.
Requests parciales y retries cuentan para los caps. Los registros extra se
conservan como evidencia, pero no crean ni eliminan posiciones.

Para cada posición `normalize` retorna exactamente
`(M1u,M2,M3,Q1,Q4,Q6,state)`:

- COMPLETED con todos los campos requeridos, tipos, dominios, environment
  observations, attempts materializados y caps válidos conserva sus valores
  observados.
- Un dato null/no disponible requerido, dato malformado, lifecycle de attempt
  incompleto/duplicado, environment digest divergente, digest/arm incorrecto o
  cap excedido fija `state=MALFORMED_RESULT` para una posición iniciada.
  Cualquier estado distinto de COMPLETED conserva su failure enum y retorna:

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

`thresholds` es un objeto cerrado con estos valores const: `m1u_max_ratio=0.85`,
`m2_or_m3_max_ratio=0.80`, `D_neutral_lower=0.90`,
`D_neutral_upper=1.10`, `quality_rule="pass_count_non_decrease_per_task"`.
Cualquier otro valor o field es `PRESTART_INVALID`; no se ajusta después de
observar datos.

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
  if preflight environment digest != manifest expected digest:
      return NO_GO(reason=PRESTART_INVALID)
  if schedule != regenerate_schedule(manifest.randomization):
      return NO_GO(reason=SCHEDULE_INVALID)
  S = normalize(each of the 40 scheduled positions)
  lineage_valid = exactly one terminal record per scheduled position,
                  no duplicate/extra/replaced/relotted/selectively rerun positions
  all_completed = every normalized/materialized scheduled position has state
                  COMPLETED and all required evidence is valid

  quality = for every task t in {A,B,C,D}:
      pass_count(ASSISTED,t,Q1) >= pass_count(OFF,t,Q1)
      and pass_count(ASSISTED,t,Q4) >= pass_count(OFF,t,Q4)
      and (t != C or
           pass_count(ASSISTED,C,Q6) >= pass_count(OFF,C,Q6))

  efficiency = for every task t in {A,B,C}:
      improves(t,M1u,0.85)
      and (improves(t,M2,0.80) or improves(t,M3,0.80))

  negative_control = for X in {M1u,M2,M3}: neutral_D(X)

  if lineage_valid and all_completed and quality and efficiency and negative_control:
      return GO
  return NO_GO
```

La normalización recorre siempre las 40 posiciones congeladas, no sólo filas
recibidas. Si falta el registro terminal de una posición, crea en la vista
diagnóstica un `MALFORMED_RESULT` imputado a caps/calidad cero y marca
`lineage_valid=false`. Una posición no iniciada se marca
`BLOCKED_BY_PRIOR_POSITION` sólo si existe el evento de parada previo; de lo
contrario es `MALFORMED_RESULT`. Un registro duplicado/extra se conserva como
evidencia, marca inválida la lineage y nunca añade/quita posiciones.

La condición `all_completed` es arm-blind y determinista. Si cualquiera de las
40 posiciones termina en `FAILED_*`, `TIMEOUT`, cualquier `PROVIDER_*`,
`MALFORMED_RESULT` o `BLOCKED_BY_PRIOR_POSITION`, el resultado necesariamente
es `NO_GO`, aunque las métricas normalizadas satisfagan el resto de la función.
Se calculan y publican métricas imputadas para diagnóstico, pero nunca pueden
producir GO ni favorecer el arm con más fallos.

Un prestart inválido impide el lote y no puede corregirse después de observar
una posición. Tras el primer SAMPLE_START toda entrada de las 40 posiciones
debe estar terminal; si la ejecución se detiene, las restantes se marcan
`BLOCKED_BY_PRIOR_POSITION`. Se decide determinísticamente sobre la lineage
completa. No hay excepción manual, revisión de casos para cambiar puntos,
subconjunto favorable ni análisis secundario que convierta NO_GO en GO.

Validación normativa de los ataques finales:

| Ataque | Bloqueo determinista |
|---|---|
| A. Intercambiar cuál mandatory defect es primary | `primary_defect_id` está en el manifest canónico; rank 1 distinto da Q6=0 y cambiar la referencia cambia `experiment_id` |
| B. Dispatch sin finalization | la materialización 1:1 falla y la posición es `MALFORMED_RESULT` |
| C. Finalization duplicada | la cardinalidad exacta falla y la posición es `MALFORMED_RESULT` |
| D. Dos retry budgets para un role | unicidad y cobertura exacta de `CLOSED_ROLE_SET` fallan con `PRESTART_INVALID` |
| E. OFF/ASSISTED con distinta versión de tool | diverge el expected/observed `EXECUTION_ENVIRONMENT_DIGEST`: `PRESTART_INVALID` antes del lote o `MALFORMED_RESULT` después del primer SAMPLE_START |
| F. Conservar `experiment_id` tras A–E | schema v2 y todos esos campos están en `canonical_bytes(manifest)`; la igualdad de identidad falla |

## 7. Registro y salida

El ledger append-only conserva experiment_id, manifest y digests, schedule,
seed, cada environment observation, SAMPLE_START/terminal,
`ATTEMPT_DISPATCHED`, `ATTEMPT_FINALIZED`, requests y retries, treatment digests,
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

**PRECONDITION_3D_PRACTICAL = READY_FOR_CODEX_REVIEW** (correcciones pendientes de nueva revisión REAL).
