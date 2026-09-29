# Frente 3 / 3D — PRECONDITION_3D_AUTH_DESIGN_V4

Fecha: 2026-09-28. Rama: `feat/frente3-3d-prerequisites`.

## 0. Autoridad, alcance y conservación de V3

Este documento es la **única especificación normativa** de aislamiento,
autenticación, causalidad y lineage para 3D. La única función estadística está
en [06-benchmark-plan.md](06-benchmark-plan.md) §§3–5 y forma parte de V4. V1,
V2 y V3 son historia no normativa.

V4 es una corrección estricta de V3, no otro rediseño. Conserva:

- estimando abierto de intención de tratar ASSISTED contra OFF, sin placebo;
- matriz A–D × OFF/ASSISTED × N fijo, con N≥5;
- cero replacement o relot dentro de una preregistración;
- VM desechable, imagen declarativa, scopes exclusivos y gateway normalizador;
- broker reviewer creado sólo después de terminar y revocar worker;
- Q6 estructurado con K=3, ranking y FP=0;
- tratamiento real basado en `Provisioned.Render()`;
- resultado separado `INSTRUMENT_NO_GO` para fallos genuinos del instrumento.

V4 cierra literalmente los doce hallazgos de la revisión REAL de Codex de V3.
No se ha implementado ni ejecutado VM, gateway, cuenta, credencial, gate,
preregistro, mini-E2E ni muestra.

`PRECONDITION_3D_AUTH_DESIGN_V4 = READY_FOR_CODEX_REVIEW`

Esto no autoriza implementación. `PRECONDITION_3D = NO-GO`.

## 1. Plan y SAMPLE_START

El root preregistration congela tareas, brazos, N, seed, orden, modelos, caps,
scopes previstos, clasificadores, thresholds, código de decisión, horarios de
pausa y `MAX_SUCCESSORS=1`. Cada posición tiene un `sample_id` opaco. Nada se
cambia al observar resultados.

`SAMPLE_START(k)` es el evento durable y append-only inmediatamente anterior a
habilitar cualquier broker/listener o proceso capaz de emitir una petición del
worker de la posición k. No existe provider request de muestra antes de ese
evento. Desde `SAMPLE_START` se aplica la clasificación causal de §2 y la
posición jamás desaparece. La muestra permanece `active_sample` hasta que
extracción, parsing, Q4/Q6, accounting y append terminal concluyen; terminar el
proceso del agente no la vuelve pre-start.

## 2. Clasificación causal ejecutable y pausas

### 2.1 Fuentes controlables y grafo de alcance

El preregistro congela un grafo dirigido de dependencias. Sus fuentes
`SAMPLE_CONTROLLED` son, sin excepción:

- bytes, paths, nombres, Unicode, output, tool results y archivos del agente;
- timing, orden, concurrencia, ausencia/presencia de frames y cierre de stream;
- conexiones, descriptores, frames, source ports y conexiones mantenidas;
- volumen, tamaño lógico/físico, número de objetos y resource consumption;
- CPU, memoria, disco, logs, file descriptors, buffers y timeouts consumibles;
- requests al provider, prompts, parámetros, retries y contenido generado;
- cualquier comportamiento del worker/reviewer o dato derivado de la muestra.

Un evento `E` es `REACHABLE_BY_SAMPLE` si existe **algún** camino permitido en
el grafo desde una fuente anterior hasta el componente/estado que falló. No se
exige demostrar intención ni causalidad efectiva. Si falta evidencia para
eliminar todos los caminos, el default es `REACHABLE_BY_SAMPLE=true`.

### 2.2 Algoritmo normativo

```text
classify(E, active_sample, frozen_graph, independent_evidence):
  if active_sample == none:
      return classify_pre_start(E)

  if exists_path(SAMPLE_CONTROLLED, E.failed_state, frozen_graph):
      return SAMPLE_FAILURE(active_sample)

  if not independent_evidence.matches_exact_allowlist(E):
      return SAMPLE_FAILURE(active_sample)

  return INSTRUMENT_NO_GO(E)

classify_pre_start(E):
  if no SAMPLE_START exists in the family:
      if E matches a §2.4 predicate: return PAUSE
      return INSTRUMENT_NO_GO(E, phase=PRESTART)

  if exists_path(any_prior_sample, E.failed_state) or cause is unknown:
      return BLOCK_ALL_AFFECTED_REMAINING_POSITIONS
  if E matches the already-published CALENDAR_HOLD: return PAUSE
  if independent_evidence.matches_exact_allowlist(E):
      return INSTRUMENT_NO_GO(E)
  return BLOCK_ALL_REMAINING_POSITIONS
```

Por tanto, después de `SAMPLE_START`, crash o bug de gateway, supervisor,
adapter, parser, extractor, logger, broker o control plane cuenta como resultado
de la muestra si ese componente procesó o pudo ser afectado por bytes, timing,
conexiones, volumen, recursos o requests de la muestra. Revelar un bug no lo
convierte en fallo independiente. La muestra se normaliza conservadoramente
con los caps de 06 §5.2.

### 2.3 Allowlist exclusiva de INSTRUMENT_NO_GO

Sólo pasan el último branch del algoritmo:

1. `FIRST_PROMPT_CONTRAST_MISMATCH`: detectado antes de lanzar al agente, sobre
   bytes producidos por AO/adapter y sin haber procesado input del agente;
2. `IMMUTABLE_CONTROL_CORRUPTION`: digest del plan, spec, imagen o binario de
   control host-only cambia y la ruta es inaccesible a la VM/muestra;
3. `EXTERNAL_FACILITY_FAILURE`: corte eléctrico/hardware físico atestado por
   monitor independiente, simultáneo en un control físicamente separado, y
   sin camino desde consumo de la VM;
4. `GLOBAL_PROVIDER_INCIDENT`: empezó antes de la primera request de la muestra,
   está atestado por provider y por scopes de control independientes, y no es
   quota/risk/sanction/billing/admission de ninguna identidad relacionada.

La evidencia debe existir contemporáneamente, estar hasheada en el ledger y
probar ausencia de camino. Operador abort, excepción sin causa, log perdido,
host resource exhaustion, provider error ambiguo y causa desconocida **no**
están en la allowlist: son fallo de muestra.

### 2.4 Únicos pause predicates

El pause engine no puede leer/decriptar métricas, transcripts, error bodies ni
resultados. Antes de cada `SAMPLE_START` sólo acepta:

- `CALENDAR_HOLD`: intervalo horario exacto publicado en el root preregistration;
- `BEFORE_FIRST_SAMPLE_GLOBAL_OUTAGE`: sólo si ninguna posición de la lineage
  ha empezado y cumple la evidencia de `GLOBAL_PROVIDER_INCIDENT`;
- `BEFORE_FIRST_SAMPLE_OPERATOR_WINDOW`: hora mínima de inicio congelada en el
  root, sin consultar provider ni resultados.

No hay pause por cooldown, quota, rate, latencia, gasto observado, RAM/disco
consumido durante la lineage, “operational unavailability” ni conveniencia.
Entre muestras sólo puede aplicar `CALENDAR_HOLD` ya publicado.
La comprobación de sanction/bloqueo atribuible a una muestra previa ocurre
antes de evaluar ese hold: un calendario coincidente no convierte el bloqueo
en cooldown ni difiere su imputación.

Si una sanción, throttling o contaminación causada o plausiblemente causada
por una muestra previa impide posiciones siguientes, no hay pausa/retry limpio:
cada posición afectada se registra `BLOCKED_BY_PRIOR_SAMPLE` y recibe la
imputación de fallo de 06 §5.2. Si el alcance no puede determinarse, se presume
que afecta todas las posiciones restantes que usan el provider.

## 3. Lineage inmutable entre preregistraciones

`experiment_family_id = SHA256(canonical_estimand || task_suite_version ||
arms)`; no lo elige el operador. Un registry WORM
global impide crear otro root desconectado con el mismo family ID/estimando;
cualquier intento se enlaza y hace `NO_GO(reason=LINEAGE_INVALID)`. Readiness consulta el
registry completo, no una ruta proporcionada por el operador.

Dentro de esa familia existe un ledger WORM/append-only, replicado fuera del
host del laboratorio y publicado por digest antes del primer inicio. Cada entrada encadena
`prev_entry_sha256` e incluye:

- `experiment_family_id`, `lineage_id`, `root_preregistration_id`, timestamp y sequence;
- documento/spec SHA, código de decisión SHA y versión de schemas;
- preregistration completa, seed, orden, tasks/arms/N/caps/models;
- cada `SAMPLE_START`, sample_id y posición;
- posición ejecutada, fallida, bloqueada o `NOT_RUN_INSTRUMENT`;
- cada `INSTRUMENT_NO_GO`, allowlist code y evidencia causal hasheada;
- todos los failed gate attempts y provider-state ledger references;
- diff de cualquier corrección posterior y su razón;
- successor preregistration, parent digest y compatibilidad de estimando;
- resultado de cada preregistration y resultado final de lineage.

No se borra, reescribe, reemplaza ni oculta un experimento abortado. El
successor único sólo corrige instrumento; conserva seed derivada de
`SHA256(root_seed || predecessor_digest || "successor-1")`, tareas, brazos, N,
umbrales, caps, modelos y estimando. Cambiar cualquiera abre otra pregunta y
no puede convertir esta lineage en GO. Sólo un predecessor terminado por un
evento independiente de la allowlist §2.3 puede tener successor; `GO`,
`NO_GO`, sample failure u operator abort son terminales.

La readiness se calcula con `lineage_decide` de 06 §5.5 y siempre publica la
cadena completa. “Último batch bueno” no es un artefacto válido.

## 4. Estado del provider y G6 factorial

### 4.1 Matriz de identidades

G6 usa scopes/organizaciones destructivos que jamás podrán ser muestras:

| Celda | X/Y |
|---|---|
| A | mismo scope |
| B | scopes distintos, misma organización |
| C | organizaciones distintas, mismo parent/admin/billing/contract/egress identity |
| D | organizaciones independientes también en parent, admin, billing instrument, contract/tier y provider-visible egress identity |

“Organización independiente” exige D, no sólo nombres de org distintos. Device
identity, API-key issuer, verified domain, regional routing y TLS identity se
inventarían. Si el provider no permite o no documenta D, el gate es
`INCONCLUSIVE`, nunca PASS.

### 4.2 Capas intervenidas por separado

Para cada celda se usa una intervención separada y provider-approved para:

1. request counters;
2. input/output token counters;
3. spend y budget;
4. rate enforcement;
5. cache;
6. abuse/risk;
7. sanctions/suspension;
8. billing/policy state;
9. admission;
10. normalized error class;
11. time-to-first-byte, chunk cadence y total latency.

No se usa violación real de políticas. Abuse/risk/sanction sólo puede probarse
en sandbox/hook autorizado por provider; si no existe, esa capa queda
`INCONCLUSIVE` y no se afirma aislamiento.

Cada capa requiere positive control en A —o en C para un estado explícitamente
parent-level— que demuestre mediante telemetría administrativa que el contador,
cache, limitador o policy state objetivo fue realmente ejercitado. Saturar un
workspace no es positive control de organization spend/risk/sanction.

### 4.3 Unidades y límites

Cada layer/cell usa 60 clusters independientes. Un cluster crea scopes nuevos,
un estímulo X y un probe Y, ordenados aleatoriamente dentro de un bloque de
tiempo; ningún scope se reutiliza entre clusters. En C/D tampoco se reutiliza
el org/parent/billing/egress pair que define la unidad; si el provider sólo
ofrece tenants virtuales, debe demostrar que son unidades estadísticas
independientes. Para señales binarias:

- positive control: ≥54/60 efectos y límite inferior Clopper–Pearson unilateral
  95% ≥0.80;
- aislamiento: 0/60 efectos y límite superior unilateral 95% <0.05;
- cualquier efecto cross-boundary = FAIL; positive control insuficiente =
  INCONCLUSIVE.

Para counters/spend: el positive control debe cruzar el threshold exacto
configurado y Y debe tener delta `0` a la resolución documentada. Para latencia
se usan diferencias cluster-paired de log(TTFT) y log(total): PASS sólo si
ambos IC TOST 95% quedan enteros en `log(0.95)..log(1.05)` y no cambia cadence,
truncation, admission ni error class; el positive control debe mostrar un IC
entero fuera de `log(0.80)..log(1.20)` en la dirección inducida. Si no, es
INCONCLUSIVE.

G6 reporta PASS/FAIL/INCONCLUSIVE por capa y celda. Para usar B como frontera,
todas las capas relevantes deben tener positive control y PASS en B. Para usar
D, todas deben tener control y PASS en D, y las identidades deben cumplir
§4.1. Una sola capa FAIL/INCONCLUSIVE impide esa frontera.

### 4.4 Gate contamination

Toda organización, parent, billing identity, egress, scope o credential que
participe en G3/G4/G5/G6/G7, incluso en un intento fallido, se marca
`GATE_ONLY_RETIRED` en el provider-state ledger. Nunca se usa para efficacy,
imagen, ensayo, mini-E2E ni successor. El ledger registra estímulos, counters,
errores, sanctions y correcciones fallidas. No se acepta “reset” salvo que cada
una de las once capas tenga un reset provider-enforced probado; V4 no depende
de esa excepción.

## 5. Máquina de estados de respuesta normalizada

### 5.1 Estados y observación del cliente

Cada request entra en `REQUEST(n)` y termina en exactamente uno:

| Estado | Cliente | Retry/backoff | Terminal | Accounting |
|---|---|---|---|---|
| `SUCCESS` | 200, content frames y usage permitidos; sin headers/trailers de identidad compartida | ninguno | success | usage upstream validado |
| `RETRYABLE` | HTTP 503 + JSON `AO_PROVIDER_RETRYABLE` sin IDs/details + `Retry-After` sintético | cliente, hasta `R_retryable[role]` | al agotar: `PROVIDER_RETRY_EXHAUSTED` | cada intento/coste cuenta |
| `RATE_LIMITED` | HTTP 429 + JSON `AO_PROVIDER_RATE_LIMITED` sin limit/reset + `Retry-After` sintético | cliente, hasta `R_rate[role]` | al agotar: `PROVIDER_RATE_LIMITED` | cada intento/coste cuenta |
| `PROVIDER_POLICY_FAILURE` | HTTP 403 + JSON estable `AO_PROVIDER_POLICY` | nunca | failure | request y usage disponible en ledger |
| `TERMINAL_PROVIDER_FAILURE` | HTTP 502 + JSON estable `AO_PROVIDER_TERMINAL` | nunca | failure | request y usage disponible; si falta, unknown privado |

Los `R_*`, base backoff y máximo se congelan por rol. Backoff es
`min(B_role*2^(n-1),BMAX_role)` más jitter determinista derivado de
`HMAC(lineage_key,sample_id||role||n)`, nunca de headers/timing upstream.
El `Retry-After` upstream nunca llega al cliente: el gateway emite sólo el
valor sintético calculado arriba. Quota, trace, org/project/workspace, billing,
server timing, cookies, redirects y trailers nunca llegan al cliente. El retry
es del cliente inventariado y cada nueva request se autentica/contabiliza de
nuevo; el gateway no hace retries ocultos.

### 5.2 Streaming, compaction y terminalidad

Cada frame se parsea, limita acumulativamente, valida contra role/epoch/session
y reserializa. Content y usage conservan orden y semántica inventariada. Si un
stream falla antes de emitir content, transiciona a `RETRYABLE`; después de
emitir cualquier byte model-facing, nunca replaya y termina
`TERMINAL_PROVIDER_FAILURE` para evitar duplicación. Un usage final ausente no
se sintetiza para el cliente; la muestra queda fallo y 06 imputa caps.

El cliente recibe los campos usage exactos que G5 demuestre necesarios para
context accounting y compaction. El gateway mantiene accounting autoritativo
por intento; M1u/M2 incluyen retries y streams parciales. Ningún error parcial
puede aparecer como success.

### 5.3 Compatibility gates

G5/G7 ejecutan, sólo en recursos gate-only, cada estado y transición contra un
direct-provider positive control. Comparan secuencia de calls, retries,
backoffs, frames, compactions, terminal state, M1u/M2 y client exit code. PASS
requiere igualdad exacta salvo IDs/clock y el stripping declarado; cualquier
cambio en decisión de retry, compaction, terminalidad o accounting es FAIL.
Si el cliente necesita un header o semántica que revelaría estado compartido,
el gate falla: no se relaja normalización ni se llama compatible.

## 6. Separación por rol y Q6

### 6.1 Broker no exportable

El worker nunca posee bytes bearer del reviewer ni del provider. Recibe un FD
a un broker local no exportable. En cada request **y cada frame de streaming**
el broker/gateway valida server-side:

```text
OS audit token + peer UID/PID + sample_id + role + repetition
+ lifecycle_epoch + launch_nonce + server-issued session binding
+ provider_scope_id
```

`native_session_id` aportado por el cliente es sólo dato; no es raíz de
confianza. El supervisor crea `launch_nonce` fuera del guest-agent y registra
el PID/audit token esperado antes del exec. Duplicar o pasar el FD a otro peer
falla peer credentials. El secret broker guarda el bearer upstream y nunca lo
exporta al FD.

Al terminar worker se incrementa epoch, se revoca el broker, se cancelan
upstream requests y streams, se cierran todos los FDs/connections y G9 prueba
quiescencia. Mientras exista una conexión, descriptor o proceso worker vivo,
el reviewer broker no puede crearse. Requests/frames con epoch viejo se
rechazan aunque la conexión se aceptara antes.

Worker y reviewer usan scopes separados. G6 debe demostrar además que quota,
cache, risk, sanction, billing, admission, error class y latency del worker no
afectan al reviewer. P1-3 requiere simultáneamente G6+G7+G9.

### 6.2 Versión y coordenadas exactas de Q6

Se puntúa únicamente `ao.q6.v2` contra `REVIEW_TARGET_SHA256`, un manifest
congelado del árbol **post-diff entregado al reviewer**, antes de cualquier
cambio del reviewer. Cada archivo tiene byte digest y debe ser UTF-8 NFC con
LF; line 1 empieza en byte 0 y cada LF inicia la línea siguiente. CRLF,
decoding ambiguo, path case-fold collision o cambio de digest hace el fixture
inválido antes de randomizar. Las coordenadas jamás se trasladan a preimage,
diff hunk ni worktree posterior.

El archivo único es `$AO_EVIDENCE/reviewer/q6-findings.json`:

```json
{
  "schema": "ao.q6.v2",
  "review_target_sha256": "hex",
  "findings": [{
    "rank": 1,
    "file": "relative/path.ext",
    "start_line": 10,
    "end_line": 14,
    "defect_class": "AUTHORIZATION_BYPASS",
    "cause_code": "MISSING_GUARD",
    "impact_code": "UNAUTHORIZED_ACCESS"
  }]
}
```

No hay `explanation`: V4 elige explícitamente la opción **B** y la elimina del
quality construct para evitar semántica subjetiva posterior. Los enums cerrados
de `ao.q6.v2`, sin `OTHER`, son:

- `defect_class`: `AUTHORIZATION_BYPASS`, `INPUT_VALIDATION`,
  `STATE_TRANSITION`, `DATA_INTEGRITY`, `CONCURRENCY`, `ERROR_HANDLING`,
  `RESOURCE_LIFECYCLE`, `API_CONTRACT`, `SECURITY_BOUNDARY`;
- `cause_code`: `MISSING_GUARD`, `WRONG_PREDICATE`, `WRONG_TARGET`,
  `STALE_STATE`, `UNSAFE_DEFAULT`, `MISSING_CLEANUP`, `NON_ATOMIC_UPDATE`,
  `ERROR_DROPPED`;
- `impact_code`: `UNAUTHORIZED_ACCESS`, `INCORRECT_RESULT`, `DATA_LOSS`,
  `STATE_CORRUPTION`, `RACE`, `RESOURCE_LEAK`, `CRASH`,
  `CONTRACT_VIOLATION`.

`K=3`; el oracle manifest debe tener `1 ≤ mandatory_defects ≤ 3`. Cada defecto
define file digest, oracle interval `[os,oe]`, una `causal_line` dentro del
intervalo y códigos exactos. Un finding es TP único sólo si:

```text
same exact file and REVIEW_TARGET digest
1 <= start_line <= causal_line <= end_line <= file_line_count
reported span <= 20
intersection([start,end],[os,oe]) / union([start,end],[os,oe]) >= 0.50
all class/cause/impact codes equal
```

Q6=1 sólo si el primary seeded defect es rank 1, todos los mandatory defects
aparecen una vez, findings≤K y FP=0. Whole-file, 20-line grazing, K=3 tiling,
enum guessing, near miss, duplicate symbol, README engañoso, duplicados y line
drift fallan.

### 6.3 Alternative legitimate defects

Antes de randomizar, dos adjudicadores independientes revisan el exact target
sin oracle; un tercero resuelve desacuerdo. Congelan accepted defects y tests
reproductores en el manifest, manteniendo mandatory≤K. Tras el lote, todo
finding sin match se anonimiza eliminando arm/order/metrics y se aplica un
protocolo ya congelado: dos adjudicadores deben reproducirlo con el comando y
criterio de severidad preregistrados; desacuerdo lo deja FP. Un alternative
aceptado no sustituye el primary mandatory ni cambia rank 1, pero no cuenta FP.
Las decisiones y hashes se publican para ambos brazos a la vez; no hay edición
libre del oracle ni juicio basado sólo en prosa.

G8 prueba todos los ataques anteriores, incluida una alternativa legítima que
el protocolo acepta y otra plausible que rechaza.

## 7. Provenance hasta la representación model-facing

`externalContext=false` está congelado para ambos brazos. Con memoria, el
attachment esperado es el `Provisioned.Render()` real, incluida la composición
`FreshnessNotice() + Pack.Render()`; no se reconstruye desde el pack.

AO genera typed origin spans antes de composición, pero la provenance **no
termina ahí**. El provider adapter debe propagar IDs/spans a través de escaping,
wrapping, message arrays y serialization. Para el primer worker prompt se
registran y hashean, fuera del alcance del agente:

- `Provisioned.Render()` exacto y sus spans freshness/pack;
- representación request exacta post-adapter, sin authorization;
- body wire exacto después de escaping/rewrapping;
- bytes de cada mensaje decodificados con un decoder independiente;
- mapping de boundaries/origin desde AO hasta los decoded model-facing bytes;
- adapter/version/model/request schema y todos los digests.

El check es doble y no se sustituye uno por otro:

```text
decoded bytes with origin TREATMENT == byte_exact(Provisioned.Render())
count(origin AO_EXTERNAL_CONTEXT) == 0
```

OFF exige cero treatment spans/bytes; ASSISTED exige exactamente una attachment
completa, ni truncada, duplicada ni relocada fuera de la posición congelada.
Provider wrapping permitido fuera del attachment no puede alterar sus decoded
bytes. La comparación usa boundaries del adapter, no búsqueda de delimitadores
o texto controlable por agente.

Sólo este check del **primer worker prompt**, antes de ejecutar al agente, puede
producir `FIRST_PROMPT_CONTRAST_MISMATCH`/`INSTRUMENT_NO_GO`. Cualquier anomalía
posterior es resultado de muestra conforme a §2.

G5 ataca adapter wrapping, escaping, truncation, duplication, relocation,
delimiter collisions, quoted JSON, tool output, external context y todas las
freshness variants. Si un cliente/adapter opaco no puede aportar y verificar el
mapping model-facing, G5 falla; no se reconstruye provenance parseando texto.

## 8. Host, VM, estado persistente y extracción hostil

### 8.1 Inventario ampliado

Se conserva una VM macOS desechable por muestra, clonada de una imagen
declarativa read-only sin credentials, provider state, histories, caches,
workspace/org IDs, gateway URLs ni device/session IDs. No hay shared folders,
clipboard, socket forwarding ni código del agente ejecutado en host. Una VM
oracle sin red ejecuta Q4. La red estática es preferida; el fallback DHCP usa
servidor y lease store desechables por clon.

El inventario completo, autocontenido en V4, es:

| Store | Reset/aislamiento requerido |
|---|---|
| imagen/manifest/binarios sellados | read-only, digest antes de cada clon; G0 |
| plan, arm map, seed, lineage/ledger | host-only WORM, inaccesible a muestra |
| VM disk/RAM/process/tmp/defaults/keychain | clon por muestra, destrucción G1 |
| provider scope/cache/requests/retention | scope nunca reutilizado; G3/G4/G6 |
| provider counters/spend/risk/sanction/policy | frontera que pase matriz G6 |
| secrets/keychain/bearers | secret broker identity; nunca guest/helper |
| evidence guest/raw gateway logs | cuarentena por muestra; nunca reingresa |
| hidden tests/Q6 manifest | oracle identity, fuera de imagen/pack |
| firewall conntrack/state tables | namespace/anchor por muestra; purge + canario G2 |
| gateway DNS/resolver cache | proceso/resolver fresh por rol/muestra; sin reuse |
| upstream HTTP/TLS connection pools | gateway process fresh; keepalive/tickets no cruzan roles/muestras |
| TLS sessions/tickets | disabled o key/cache nuevo por rol/muestra |
| SSH ControlMaster, known_hosts, agent, client cache | ControlMaster=no; known_hosts efímero pinned; sin agent; identidad helper |
| hypervisor registry/log/cache | directorio por clon, purge y canario G1/G2 |
| clone backing/page/block caches | base read-only; clone ID único; canario cross-clone |
| ARP/NDP/NAT/source-port allocation | namespace y source range único; flush |
| provider-visible egress IP/ASN/region | factor C/D de G6; no se presume independencia |
| provider routing/device/client fingerprints | inventario G5/G6 y scope boundary |
| gateway logs/buffers | preallocated por muestra; proceso fresh; nunca reuse |
| host clock/randomness/process IDs | registrados; no usados como secreto de arm |
| resultados publicados/gate ledger | append-only, sólo visibles tras cierre |

G2 prueba cada store host/hypervisor; G6 prueba el egress visible. Cada
rol/muestra usa gateway process y upstream transport frescos cuando el provider
es parte de la medición. No se afirma que “nada más persiste”: cualquier store
nuevo descubierto se añade al ledger y hace fallar el gate hasta clasificarlo.

### 8.2 Extracción sin identidad confiable

El supervisor, plan owner y secrets/gateway identity nunca reciben ni parsean
evidence bytes o SSH stdout del guest. Un transport stub bajo identidad sin
plan, secrets, gateway, other evidence ni credenciales reutilizables abre la
única conexión pinned a `ctl`; trata stdout/stderr como frames binarios
length-delimited, con límites, sin terminal, shell, escape processing ni logs
interpretados. Entrega un pipe unidireccional al extractor.

El proceso que **recibe/extracta** corre bajo otra identidad/VM helper sin
plan, secrets, gateway y **sin network capability**. Sólo lee el pipe y un
directorio vacío. Abre descriptor-relative con `O_NOFOLLOW`, pinnea los FDs
desde validación hasta copia, compara inode/device antes/después y nunca vuelve
a resolver el path.

Política de nombres/metadata:

- path bytes deben ser UTF-8 NFC, case-fold unique y sin alternate names;
- sólo regular files, link count 1; rechaza symlink, hardlink, FIFO, socket,
  device, sparse fuera de cuota y archive del agente;
- rechaza xattrs, resource forks, ACL no canónica y file flags;
- límites de files/bytes/depth/path/line length; logical lines demasiado largas
  fallan la muestra, no el instrumento;
- TOCTOU, descriptor pinning y replacement se atacan explícitamente;
- Git hooks, filters, attributes, external diff y config del guest no se usan.

El helper reconstruye desde base limpia y byte manifest. Q4 sólo corre en VM
oracle sin red. Cualquier fallo provocado por objetos/bytes hostiles es
`AGENT_INDUCED_FAILURE` por §2, incluso si revela bug del extractor.

G8/G9 tienen una assertion por cada promesa: helper no puede abrir plan,
secrets, gateway, network u otra evidence; supervisor no recibe hostile SSH
output; stub no interpreta output; Unicode/case-fold, xattr/fork, ACL, flags,
long lines, descriptor pinning, TOCTOU y hostile SSH frames son rechazados.

## 9. Lifecycle por muestra

1. Append ledger pre-start; sólo pause predicates §2.4 pueden esperar.
2. Crear red, clone, fresh gateway/transport y worker scope nunca usado.
3. Append `SAMPLE_START`; activar broker worker.
4. Construir/serializar primer prompt y ejecutar provenance §7. Sólo una
   mismatch independiente puede terminar instrumento.
5. Ejecutar worker. Todo fallo alcanzable se puntúa como muestra.
6. Revocar epoch/FD/streams/connections; demostrar quiescencia.
7. Crear reviewer scope/broker/epoch. Si queda worker vivo, la posición falla;
   no se reintenta y reviewer no inicia.
8. Ejecutar reviewer/fix y capturar Q6 v2.
9. Stub/helper extraen; oracle calcula Q4. Hostile failure cuenta muestra.
10. Destruir VM/gateway/transports/state, append hashes y continuar en orden.

No se muestran resultados parciales. Provider sanction previa sigue §2.4. Los
gate resources nunca entran en este lifecycle.

## 10. Gates empíricos restantes

Ninguno se ejecuta antes de revisión REAL de V4.

| Gate | Criterio V4 | FAIL/INCONCLUSIVE |
|---|---|---|
| G0 imagen/viabilidad | manifest declarativo; clone sin capability no autentica; clients/AO fijos arrancan; ledger schema y classifier graph validan | corregir o NO-GO |
| G1 destrucción | todos los stores guest/hypervisor locales, incluidos backing caches/registry/logs, no cruzan clone | NO-GO |
| G2 red/host | static/DHCP disposable; conntrack, ARP/NDP/NAT/source ports, resolver, SSH state, hypervisor state y fresh transport por sample; canarios write/read | corregir o NO-GO |
| G3 cache Anthropic | 60 independent fresh-prefix/scope clusters, same-scope positive ≥54/60 lower95≥.80 y cross-scope 0/60 upper95<.05 | FAIL/INCONCLUSIVE → sólo frontera D si G6 pasa; si no NO-GO |
| G4 cache OpenAI | §10.1 exacto | INCONCLUSIVE → Claude; FAIL → Claude; Claude requiere G3/G6 |
| G5 client/provenance | state machine vs direct provider; treatment mapping post-adapter/model-facing; external absent; all attacks §7 | corregir; opacidad del adapter = FAIL |
| G6 provider partition | matriz/layers/positive controls/bounds §4, incluido worker→reviewer y egress identity | cualquier layer INCONCLUSIVE impide la frontera |
| G7 gateway/broker | state machine, cumulative bounds, per-request/frame peer+role+epoch+session+scope, revocation/replay/carry-over; direct-provider parity | corregir |
| G8 Q6/extraction/oracle | §6 adversarials y cada assertion §8.2, incluida alternative defect | corregir |
| G9 lifecycle/control/lineage | no live worker connection al reviewer epoch; server-issued session binding; hostile SSH isolation; ledger append-only/successor rules | corregir o NO-GO |

### 10.1 G4 estadísticamente falsificable

La unidad independiente es un cluster con: prefix elegible aleatorio nuevo,
scope pair nuevo, control same-project, control cross-project same-org y control
cross-organization D. El project pair, org pair y parent/billing/egress pair
del cluster no se reutilizan en otro cluster. Cada prefix contiene nonce de
alta entropía y estructura cache-eligible idéntica; nunca se reutiliza. Se
requieren 60 clusters.

Clusters se randomizan dentro de ≥6 bloques temporales/routing epochs, máximo
10 clusters por epoch; orden de same/cross probes aleatorio. El indicador
primario es cached-token/cache-read documentado, no latencia. Análisis es a
nivel cluster (un hit/no-hit por condición), sin tratar requests correlacionadas
como réplicas.

- positive same-project: ≥54/60 hit y lower CP unilateral95 ≥0.80;
- cross-project: 0/60 hit y upper CP unilateral95 <0.05;
- cross-organization D: 0/60 hit y upper CP unilateral95 <0.05.

`PASS` exige los tres, 60 scope pairs completos y diversidad de epochs.
`FAIL` es cualquier cross hit con positive control válido. `INCONCLUSIVE` es
positive control insuficiente, indicador ausente, prefix inelegible, menos de
60 clusters/scopes o epochs insuficientes. FAIL e INCONCLUSIVE eligen reviewer
Claude, nunca PASS. Todos los recursos G4 se retiran por §4.4.

## 11. Mapeo de la revisión REAL de V3

### 11.1 Doce findings concretos

| # | Finding | Resolución | Estado |
|---|---|---|---|
| F1 | agent-reachable instrument failure | algoritmo reachability/default sample failure y pause allowlist §2 | **CLOSED_BY_DESIGN** |
| F2 | clean reset entre preregistrations | WORM lineage, successor único y readiness total §3/06 §5.5 | **CLOSED_BY_DESIGN** |
| F3 | G6 no distingue scopes/org/parent | factorial A–D, 11 layers, positive controls y bounds §4 | **REQUIRES_GATE (G6)** |
| F4 | response reconstruction observable/incompatible | autómata completo y direct-provider parity §5 | **REQUIRES_GATE (G5/G7)** |
| F5 | role separation/provider coupling/FD | broker no exportable, peer creds cada frame, server binding, G6 coupling §6.1 | **REQUIRES_GATE (G6/G7/G9)** |
| F6 | Q6 construct invalid | target/line exactos, causal line, IoU≥.50, no explanation, adjudication congelada §6.2–6.3 | **CLOSED_BY_DESIGN**, casos probados G8 |
| F7 | decision function parcial | caps algebraicos y totalización de null/malformed/todos los estados 06 §5 | **CLOSED_BY_DESIGN** |
| F8 | provenance termina pre-adapter | spans hasta wire/decoded model bytes y external check separado §7 | **REQUIRES_GATE (G5)** |
| F9 | G4 pseudoreplicado/incompleto | cluster independiente, fresh prefix/pairs, epochs y tres bounds §10.1 | **REQUIRES_GATE (G4)** |
| F10 | gates contaminan sample orgs | gate-only retired incluso failed attempts; provider ledger §4.4 | **CLOSED_BY_DESIGN** |
| F11 | host persistent state incompleto | inventory ampliado, fresh transports, G2/G6 §8.1 | **REQUIRES_GATE (G2/G6)** |
| F12 | hostile extraction/control incompletos | stub+helper sin trust, metadata/TOCTOU/SSH inventory y assertions §8.2 | **REQUIRES_GATE (G8/G9)** |

### 11.2 Disposition P1/P2/P3 heredada que Codex reabrió

| Hallazgo | V4 |
|---|---|
| P1-1 selection/rebatching | **CLOSED_BY_DESIGN** (§2–§3) |
| P1-2 provider shared state | **REQUIRES_GATE (G6)** |
| P1-3 worker→reviewer | **REQUIRES_GATE (G6/G7/G9)** |
| P1-4 Q6 oracle | **CLOSED_BY_DESIGN**, G8 valida fixture |
| P1-5 decision rule | **CLOSED_BY_DESIGN** (06 §5) |
| P1-6 treatment invariant | **REQUIRES_GATE (G5)** |
| P2-1 host/DHCP | **REQUIRES_GATE (G2/G6)** |
| P2-2 declarative image | **REQUIRES_GATE (G0/G5)** |
| P2-3 hostile extraction | **REQUIRES_GATE (G8/G9)** |
| P2-4 G4 | **REQUIRES_GATE (G4)** |
| P3-1 contradictions | **CLOSED_BY_DESIGN**; static audit §14 |

No hay P0/P1 `OPEN`. Ningún `REQUIRES_GATE` se presenta como evidencia pasada.

## 12. Infraestructura y viabilidad práctica

La arquitectura base no cambia: ~60–80 GB, una VM macOS activa, helper/oracle
secuenciales, identidades supervisor/secrets/transport/extractor/oracle, filtro
y scopes exclusivos. Aumenta:

- gateway/upstream transport fresh por rol/muestra;
- egress/parent/billing realmente independientes para probar D;
- organizaciones gate-only que jamás podrán alojar samples;
- WORM lineage externo;
- G4: 60 clusters con scope pairs nuevos y ≥6 routing epochs;
- G6: 4 celdas × 11 layers × 60 clusters, con scopes/orgs desechables y
  positive controls provider-approved.

Las 40 muestras mínimas siguen requiriendo 40 scopes worker + 40 reviewer, más
sus organizaciones según la frontera que pase. No hay scopes de replacement.
Los gates ahora dominan ampliamente el número de recursos. G4 por sí solo
requiere 60 project pairs y 60 org/parent pairs no reutilizados. G6 requiere
hasta `4×11×60 = 2,640` cluster executions; C/D necesitan pares de tenants
independientes por cluster. El número de objetos reales puede bajar sólo si el
provider ofrece tenants sandbox que G6 demuestre equivalentes a esas unidades;
no es honesto dar un total de cuentas billables sin esa capacidad.

**Viabilidad práctica:** condicional y hoy baja. El diseño sigue siendo
ejecutable en principio, pero G6 abuse/risk/sanction y la independencia D
probablemente requieren cooperación formal del provider. Sin sandbox/positive
controls o identidades D verdaderamente independientes, el resultado es
INCONCLUSIVE/NO-GO y 3D no debe correrse. El fallback Claude evita que G4
inconcluso bloquee por sí solo, pero Claude también necesita G3/G6.

El coste monetario sigue no calculable sin caps/model price lock y disponibilidad
de sandboxes. Debe presupuestarse el máximo de samples **más todos los clusters
gate-only**, incluidos failed attempts; nunca sólo completions exitosos.

## 13. Cambios V3 → V4

- causalidad conservadora algorítmica; bugs alcanzables cuentan muestra;
- pause allowlist cerrada y prior-sample sanctions imputadas, no pausadas;
- lineage WORM y readiness de cadena completa con successor máximo uno;
- G6 factorial A–D por once capas con positive controls/bounds;
- autómata único success/retry/rate/policy/terminal y parity directa;
- capability como broker OS-authenticated, nunca bearer exportable;
- Q6 v2 fija target/lines/causal line/IoU y elimina explanation;
- 06 totaliza null, malformed, caps por role y cada estado;
- provenance cruza adapter hasta wire y decoded model-facing bytes;
- G4 usa clusters/prefix/scope pairs/epochs y cross-org bound exacto;
- gate organizations se retiran para siempre;
- inventario host y extracción añaden todos los stores/ataques pedidos.

## 14. Revisión estática interna

Se revisan conjuntamente este documento y 06 §§3–5 buscando:

`replacement`, `relot`, `retry`, reducción de N, `null`, pass-through,
provider adapter, `INSTRUMENT_NO_GO`, pause, reviewer credential, `S_k` y
“nothing persists”. V1–V3 y preflight están marcados históricos. Resultado:

- replacement/relot sólo aparecen como prohibiciones;
- retry sólo es transición interna contada, nunca retry de muestra;
- N/caps/thresholds son inmutables;
- null/malformed totalizan a fallo;
- no hay pass-through de response ni bearer reviewer;
- adapter provenance llega al modelo;
- instrument/pause tienen allowlists cerradas;
- no se usa `S_k` singular ni se afirma que nada más persiste.

No se conoce P0/P1 `OPEN`. Esta pasada no sustituye la revisión REAL de Codex.

## 15. Stop

El único siguiente paso permitido es revisión estática real con el prompt V4.
No implementar, crear infraestructura/cuentas, ejecutar gates, preregistrar,
correr mini-E2E, 3D/3E/3F, desplegar, releasear ni mergear.

**PRECONDITION_3D_AUTH_DESIGN_V4 = READY_FOR_CODEX_REVIEW.**

**PRECONDITION_3D = NO-GO.**
