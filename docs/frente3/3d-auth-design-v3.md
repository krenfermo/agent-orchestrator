# Frente 3 / 3D — PRECONDITION_3D_AUTH_DESIGN_V3

> **SUSTITUIDO (2026-09-28).** La revisión REAL de Codex dio
> `PRECONDITION_3D_AUTH_DESIGN_V3 = NEEDS_CHANGES`. La única especificación
> vigente es [3d-auth-design-v4.md](3d-auth-design-v4.md), con la función total
> de [06-benchmark-plan.md](06-benchmark-plan.md) §§3–5. V3 es historial no
> normativo.

Fecha: 2026-09-28. Rama: `feat/frente3-3d-prerequisites`.

## 0. Autoridad, alcance y estado

Este documento era la especificación V3 de aislamiento,
autenticación e integridad de muestra para 3D. La función estadística normativa
está en [06-benchmark-plan.md](06-benchmark-plan.md) §§3–5 y forma parte de V3.
No hay otra regla de decisión. V1 y V2 se conservan sólo como historia y no
pueden completar, interpretar ni contradecir V3.

V3 responde a la segunda revisión REAL de Codex de V2, cuyo veredicto fue
`PRECONDITION_3D_AUTH_DESIGN_V2 = NO-GO` (P1 6 / P2 4 / P3 1). No se ha
implementado ni ejecutado nada: no se creó VM, gateway, cuenta, credencial,
workspace, proyecto, gate, preregistro, mini-E2E ni muestra de 3D.

El experimento sigue siendo abierto para el agente: compara la intención de
tratar ASSISTED contra OFF. Oculta la asignación formal, el plan y otras
repeticiones, pero no finge ocultar al modelo el contexto que recibe. No hay
placebo.

**Veredicto de diseño:**

`PRECONDITION_3D_AUTH_DESIGN_V3 = READY_FOR_CODEX_REVIEW`

Esto significa “lista para ataque estático”, no “lista para implementar”. Los
gates de §10 siguen sin ejecutarse y `PRECONDITION_3D = NO-GO`.

## 1. Tratamiento real y única precondición de contraste

### 1.1 Contraste

El tratamiento no es `ContextPack.Render()` aislado. En cada frontera de
dispatch donde el producto adjunta memoria es el valor completo de:

```go
projectmemory.Provisioned.Render()
```

Para este experimento `externalContext=false` queda congelado en ambos brazos.
Por tanto, cuando hay memoria, el valor real incluye
`FreshnessNotice() + Pack.Render()`. El tratamiento esperado sigue siendo
exactamente el `Provisioned.Render()` que produzca el producto, no una
reconstrucción del pack. OFF recibe la salida real del producto con memoria
desactivada. Si AO adjuntara `External.Render()` pese a la configuración, la
precondición del primer prompt falla y el resultado es `INSTRUMENT_NO_GO`.

El estimando es el efecto total del producto, incluida la longitud y la notice
dinámica. Commit, árbol sucio, parcialidad, freshness y grafo son entradas
congeladas o medidas; no se normalizan fuera del tratamiento para fabricar una
comparación distinta.

### 1.2 Instrumentación necesaria antes de serializar

Una implementación futura deberá capturar dentro de AO, en el constructor del
prompt y **antes** de serializar para el proveedor:

- una secuencia de spans tipados `{origin, dispatch_id, role, ordinal,
  byte_start, byte_end, sha256}`;
- `origin` en un enum cerrado: `AO_BASE_PROMPT`, `USER_TASK`,
  `TREATMENT_FRESHNESS`, `TREATMENT_PACK`, `AO_EXTERNAL_CONTEXT`,
  `WORKER_QUOTE`, `REVIEWER_TEMPLATE`, `TOOL_RESULT`;
- límites exactos del attachment y digest SHA-256 del attachment completo;
- digests separados de freshness, pack y external, además de commit/index,
  freshness verdict y versión del renderer;
- binding a `sample_id`, `role`, `dispatch_id`, `native_session_id` y primer
  ordinal de petición, sin guardar la etiqueta OFF/ASSISTED en una superficie
  accesible al agente.

Los spans nacen de los objetos tipados que AO concatena. Está prohibido
inferirlos después buscando delimitadores, JSON, encabezados o texto de la
respuesta del proveedor. Contenido controlado por el agente sólo puede ocupar
un span de origen no-AO y nunca cambiar el tipo o los límites de otro span.

### 1.3 STOP de contraste

Sólo el **primer prompt del worker**, capturado antes de cualquier acción del
agente y antes de enviarlo al proveedor, es una precondición que puede terminar
el experimento:

- OFF: ningún span `TREATMENT_*` y digest del attachment vacío;
- ASSISTED: spans y digest iguales a la salida byte-a-byte de
  `Provisioned.Render()` producida en ese mismo dispatch;
- todos los spans no-tratamiento iguales a la plantilla preregistrada después
  de normalizar únicamente IDs, puertos y rutas tipados, nunca texto libre.

Una discrepancia produce `INSTRUMENT_NO_GO`. Prompts posteriores, incluidos
reviewer y fix, se registran como resultados; nunca autorizan exclusión,
reintento o sustitución. G5 debe demostrar spans resistentes a colisiones de
delimitadores, JSON citado, tool output, transcript injection y todas las
variantes de freshness.

## 2. Población fija y selección de muestra

### 2.1 Plan inmutable

Antes de ejecutar se fija una matriz de A, B, C y D × OFF/ASSISTED × `N`, con
`N ≥ 5`, orden intercalado y semilla. N no cambia después de observar ningún
resultado. Cada posición tiene un `sample_id` opaco y sus dos ámbitos de rol
preasignados. Una posición jamás se cambia de tarea, brazo, rol o ámbito.

Una muestra **empieza** cuando se habilita el listener del worker para esa
posición. Desde ese instante todo cuenta, aunque todavía no haya respuesta del
proveedor. No hay reintento de muestra, replacement, clean replacement,
relote, batch sustituto ni combinación de lotes.

### 2.2 Estados exhaustivos

Cada posición termina exactamente en uno de:

- `COMPLETED`;
- `FAILED_WORKER`, `FAILED_REVIEWER`, `TIMEOUT`, `GATEWAY_DENIAL`,
  `PROVIDER_ERROR`, `PROVIDER_SANCTION` o `AGENT_INDUCED_FAILURE`;
- `INSTRUMENT_FAILURE_AT_SAMPLE`, cuando se pierde integridad con una muestra
  ya iniciada;
- `NOT_RUN_INSTRUMENT`, únicamente para posiciones aún no iniciadas cuando el
  instrumento pierde integridad.

Todos los estados posteriores al inicio se conservan en el denominador y se
imputan como especifica 06 §3/§5. Una sanción del proveedor, timeout, 429, 5xx,
denegación del gateway, volumen permitido que agote un buffer, corrupción de
evidencia controlable por el agente o fallo inducible por el agente cuenta como
resultado de esa muestra.

### 2.3 Fallo del instrumento

La pérdida de evidencia primaria no controlable por el agente, un fallo del
supervisor/gateway, una fuga de frontera, un prompt inicial inválido o la
imposibilidad de probar el binding de rol terminan inmediatamente el
experimento como `INSTRUMENT_NO_GO`.

- La muestra activa conserva lo observado hasta el fallo y se etiqueta
  `INSTRUMENT_FAILURE_AT_SAMPLE`; no se sustituye.
- Toda asignación posterior queda `NOT_RUN_INSTRUMENT` en el plan original.
- No se calcula GO/NO-GO de eficacia ni se publica un subconjunto como lote.
- Corregir el instrumento exige una especificación/versionado y preregistro
  nuevos; no crea una continuación ni reemplazo del experimento anterior.

Antes de empezar una muestra, una indisponibilidad puramente operativa puede
**pausar** el plan sin cambiar orden ni asignación. No puede inspeccionarse
evidencia de resultados durante la pausa. Una vez habilitado el listener, ya no
es pausa: rigen los estados anteriores.

Los registros del gateway se preasignan con límites por muestra. Alcanzar el
límite por tráfico permitido cierra la muestra como `AGENT_INDUCED_FAILURE`;
no convierte el evento en fallo del instrumento. Sólo pérdida/corrupción del
registro fuera de esos límites es fallo del instrumento.

## 3. Estado del proveedor, respuestas y cuotas

### 3.1 Lo que no se presume

Un workspace/proyecto nuevo no demuestra por sí solo aislamiento de caché,
cuota, riesgo, fraude, billing, sanciones, device identity ni rate limits. Una
espera o cooldown tampoco lo demuestra. V3 no afirma que el estado de una
organización dedicada esté particionado hasta que G3/G4/G6 lo prueben.

Estado potencialmente observable por una repetición:

- headers de rate limit, remaining, reset, retry y billing;
- códigos y cuerpos de quota/policy/risk;
- latencia y disponibilidad alteradas por throttling;
- cache reads, cached tokens y estado de prefix cache;
- límites diarios/mensuales, gasto y suspensión de organización;
- identificadores de request, workspace, proyecto, región, máquina o sesión.

### 3.2 Normalización de respuestas

El gateway parsea la respuesta upstream y construye una respuesta nueva. Sólo
pasa los campos de contenido y `usage` que el cliente inventariado necesita.
Elimina todos los headers upstream salvo `content-type` y los estrictamente
necesarios que G5 enumere y pruebe como no compartidos. Siempre elimina:

- cualquier header de cuota, rate limit, billing, organization, project,
  workspace, request trace, server timing o infraestructura;
- cookies, links, redirects y URLs;
- mensajes de error que incluyan IDs, saldo, límites o scope compartido.

`retry-after` no se reenvía. El gateway genera retry local determinista desde
el presupuesto y contador de **esa muestra**, con la misma política en ambos
brazos. Un 429/5xx/sanction upstream se reduce a un error tipado estable y se
registra en evidencia privada; sigue contando como resultado. Streaming se
reencuadra con límites acumulativos, timeout y orden definidos; ningún trailer
upstream llega al cliente. G7 muta headers, trailers, streaming truncado,
compresión, cuerpos acumulativos y errores.

### 3.3 Condición de partición y fallback honesto

G6 debe inventariar para cada proveedor y modelo el scope de lectura **y de
escritura** de todos los estados anteriores, más reset, acumulación de canarios
y enforcement de policy/risk. Debe demostrar con scopes canario que saturar el
presupuesto de X no cambia headers normalizados, admisión, latencia clasificada
ni capacidad de Y, y que ningún valor organizacional llega al cliente.

Hay sólo tres salidas:

1. **Partición real demostrada.** Puede usarse una organización dedicada con
   un workspace/proyecto exclusivo por muestra y rol.
2. **Partición sólo a nivel organización.** La única alternativa
   metodológicamente válida es una organización de proveedor independiente por
   muestra y rol, también sin una identidad superior que exponga estado
   compartido al agente. Esta ruta requiere que el proveedor la permita y que
   G6 repita el ensayo entre organizaciones.
3. **Partición no demostrada o organizaciones independientes inviables.**
   `INSTRUMENT_NO_GO`; no se sustituye con cooldowns, presupuestos blandos,
   covariables ni reducción de N.

Las sanciones posteriores al inicio nunca eliminan una muestra. Si una sanción
afecta scopes futuros, el resultado actual cuenta y el experimento termina
`INSTRUMENT_NO_GO`, dejando el resto `NOT_RUN_INSTRUMENT`. No existe un lote
sustituto.

## 4. Worker, reviewer y Q6

### 4.1 Separación fuerte por rol

Por repetición existen dos conjuntos explícitos:

```text
S_k = { worker_scope_k, reviewer_scope_k }
C_k = { worker_capability_k, reviewer_capability_k }
```

Nunca hay un token común. El gateway expone listeners distintos por rol. Cada
capability está ligada server-side a `{sample_id, repetition, role,
provider_scope_id, AO binding_id, native_session_id, lifecycle_epoch}` y sólo
acepta el modelo, ruta y campos de ese rol. Cualquier discrepancia se rechaza y
cuenta como `GATEWAY_DENIAL`; tráfico cross-role jamás se atribuye al rol
declarado por el cliente.

El secreto upstream y la capability del reviewer no existen en la VM ni en un
listener activo durante el worker. El worker corre bajo un UID/sandbox propio;
recibe su capability por un descriptor heredado no volcable, no por disco,
argv, config compartida o entorno legible por otro UID. Al terminar:

1. se cierra el listener worker y se revoca su capability;
2. se verifica que no quedan procesos, descriptores o conexiones del UID
   worker;
3. sólo entonces se crea la capability reviewer, se activa su listener y se
   lanza el reviewer bajo otro UID/sandbox;
4. el listener reviewer rechaza toda petición antes de ese epoch y el worker
   listener rechaza toda petición posterior.

El reviewer puede leer el diff/transcript previsto de su misma repetición, pero
el worker no puede leer, consumir, calentar ni contaminar su scope de proveedor.
G7 debe probar replay, robo de token, listener equivocado, session spoofing,
conexión mantenida a través del cambio de epoch y procesos huérfanos.

### 4.2 Contrato exacto de Q6

La tarea C exige que el reviewer escriba exactamente:

`$AO_EVIDENCE/reviewer/q6-findings.json`

en UTF-8, JSON canónico, con este esquema cerrado:

```json
{
  "schema": "ao.q6.v1",
  "findings": [
    {
      "rank": 1,
      "file": "relative/path.ext",
      "start_line": 10,
      "end_line": 18,
      "defect_class": "AUTHORIZATION_BYPASS",
      "cause_code": "MISSING_GUARD",
      "impact_code": "UNAUTHORIZED_ACCESS",
      "explanation": "40–400 UTF-8 characters explaining cause and impact"
    }
  ]
}
```

Reglas: entre 1 y `K=3` findings; ranks únicos, contiguos y ordenados; path
exacto relativo, sin glob ni directorio; rango inclusivo máximo de 20 líneas;
explicación sin rutas adicionales ni rangos y de 40–400 caracteres. Los enums
cerrados de `ao.q6.v1` son:

- `defect_class`: `AUTHORIZATION_BYPASS`, `INPUT_VALIDATION`,
  `STATE_TRANSITION`, `DATA_INTEGRITY`, `CONCURRENCY`, `ERROR_HANDLING`,
  `RESOURCE_LIFECYCLE`, `API_CONTRACT`, `SECURITY_BOUNDARY`;
- `cause_code`: `MISSING_GUARD`, `WRONG_PREDICATE`, `WRONG_TARGET`,
  `STALE_STATE`, `UNSAFE_DEFAULT`, `MISSING_CLEANUP`, `NON_ATOMIC_UPDATE`,
  `ERROR_DROPPED`;
- `impact_code`: `UNAUTHORIZED_ACCESS`, `INCORRECT_RESULT`, `DATA_LOSS`,
  `STATE_CORRUPTION`, `RACE`, `RESOURCE_LEAK`, `CRASH`,
  `CONTRACT_VIOLATION`.

No existe `OTHER`. Más de K, schema o campo extra, whole-file, duplicados,
rangos solapados del mismo símbolo o parse error hacen Q6=0.

El oráculo oculto contiene el conjunto finito de defectos válidos, cada uno con
archivo exacto, intervalo mínimo, clase y códigos. Un finding es TP sólo si su
archivo coincide, su rango de ≤20 líneas intersecta el intervalo y los tres
códigos coinciden. Cada finding sin match único es FP; dos findings contra el
mismo defecto producen un TP y al menos un FP. Q6=1 sólo si:

- el defecto sembrado aparece como rank 1;
- no falta ningún defecto obligatorio de la tarea;
- `FP = 0` en todos los findings entregados.

Antes del preregistro, G8 audita que el diff/fixture de C sólo contiene el
conjunto aceptado dentro del alcance revisable. Si aparece otro defecto real,
se corrige el fixture o el manifest antes de congelarlo; nunca se adjudica un
finding nuevo después de ver el brazo o los resultados.

Así, shotgun y whole-file fallan aunque incluyan la línea correcta. G8 incluye
casos obligatorios: respuesta correcta; whole-file; K+1/shotgun; near miss a
una línea fuera; símbolo duplicado en otro módulo; README engañoso; duplicado
del mismo defecto; clase correcta con causa/impact incorrectos; y archivo
correcto con explicación vacía o fuera de límite.

## 5. Host, VM e imagen declarativa

### 5.1 Frontera

Se mantiene una VM desechable macOS por repetición y otra VM de oráculo para
Q4. No hay montajes, clipboard, sockets, port forwarding, shared folders ni
credenciales del operador. El canal `ctl` usa comando forzado propiedad de
root y un conjunto cerrado de verbos. El agente nunca ejecuta en el host.

La red usa configuración IPv4 estática única por clon, asignada por el
supervisor desde el plan congelado, con MAC única y **sin DHCP**, si G2 prueba
que el hipervisor lo permite. Si el invitado macOS/hipervisor exige DHCP, la
ruta DHCP es una excepción explícita: servidor dedicado al clon, lease store
desechable, client ID/MAC generados por el supervisor, campos guest ignorados y
canario de escritura/lectura entre dos clones. Si eso no puede demostrarse,
G2 falla. IPv6, link-local, multicast, DNS y servicios de bridge se bloquean.
Las respuestas SSH se permiten sólo como estado `ESTABLISHED` de conexiones
host→`ctl`; una conexión guest→host:22 nueva se rechaza.

### 5.2 Imagen sin estado de proveedor

La imagen final se construye de forma declarativa desde un manifest de paths,
tipos, propietarios, modos y hashes. Puede usarse un clon temporal con red para
descubrir archivos que los clientes necesitan, pero su HOME nunca se convierte
en base. La imagen sellada no contiene credenciales, capabilities, histories,
caches, responses, cookies, workspace/project/org IDs, URLs del gateway,
machine/device/session IDs ni estado producido por una llamada real.

G0 valida el manifest y arranca un clon sin capability: ningún cliente puede
autenticar. La configuración por rol y repetición se entrega sólo después de
clonar, por el canal de control, y se destruye con la VM.

### 5.3 Extracción hostil

La identidad que posee gateway, secretos, plan y supervisor **no** extrae ni
parsea evidencia del guest. El control lee sólo rutas fijas con APIs
descriptor-relative, `O_NOFOLLOW`, `fstat`, regular-file-only, verificación de
device/inode, límites de bytes/inodos/profundidad y timeout. Rechaza symlink,
hard-link múltiple, FIFO, socket, device, sparse fuera de cuota y carreras. No
acepta archives creados por el agente.

Los bytes se entregan a una VM helper desechable o UID sandboxed sin acceso a
red, gateway, keychain, supervisor, plan ni otras evidencias. Allí se parsean
SQLite/JSONL y se reconstruye el cambio desde base limpia y un manifest de
bytes, con hooks, filters, attributes y external diff desactivados. Sólo
digests y resultados tipados cruzan de regreso. Q4 ejecuta código únicamente
en la VM de oráculo sin red.

### 5.4 Inventario completo de estado persistente

| Estado | Dueño/scope | Escritura por muestra | Lectura posterior | Regla |
|---|---|---:|---:|---|
| imagen y manifest sellados | host control | no | sí | digest antes de cada clon |
| plan, semilla, arm map | supervisor | no | no por agente | inmutables |
| logs gateway por muestra | identidad secrets | indirecta/acotada | analista | preasignados, append-only |
| evidencia guest en cuarentena | helper sin secretos | sí | parser aislado | nunca vuelve a una muestra |
| ledger de scopes/capabilities | supervisor | no | no por agente | append-only, no reuse |
| discos/procesos/RAM VM | clon | sí | no | destrucción G1 |
| red: ARP/NDP/NAT/firewall | host/hipervisor | indirecta | posible | reglas/tabla por clon, flush+canario G2 |
| DHCP leases si inevitable | servicio por clon | sí | no | store desechable G2 |
| provider cache | scope de rol | sí | no por otro scope | G3/G4/G6 |
| cuotas/rate/spend | scope u organización | sí | potencial | partición G6 u org independiente |
| policy/risk/sanction | proveedor | sí | potencial | partición G6 u org independiente |
| provider request logs/retention | proveedor | sí | no por agente | declarado; no reutilizar scope |
| billing/operator audit | organización | sí | operador | no expuesto al agente; G6 verifica efecto |
| llavero/secrets | identidad secrets | no | gateway de rol | nunca llega a guest/helper |
| tests/oráculo oculto | host oracle | no | VM oráculo | fuera de imagen/pack |
| resultados publicados | repo/evidencia | no durante lote | después | publicación sólo al cierre |

No se afirma “nada más persiste”. G1, G2 y G6 deben ampliar este inventario si
descubren otro store; un store no clasificado impide preregistrar.

## 6. Ciclo de vida por muestra

1. El supervisor verifica plan, digests, scope nunca usado y ausencia del clon
   anterior. Una espera antes del inicio conserva la misma posición.
2. Crea red/VM desde la imagen declarativa y configura el scope worker.
3. Activa sólo el listener worker; ese acto inicia la muestra.
4. AO construye el primer prompt, captura spans/digest y ejecuta §1.3. Un fallo
   aquí termina todo como `INSTRUMENT_NO_GO`.
5. El worker corre hasta terminal/timeout. Denegaciones y fallos cuentan.
6. Se revoca worker y se verifica quiescencia. Sólo entonces se crea scope,
   capability, listener y proceso reviewer.
7. Reviewer/fix terminan o alcanzan timeout. Q6 se captura desde la ruta fija.
8. El extractor hostil copia a cuarentena; helper y VM de oráculo calculan
   métricas/Q4. Artefactos agent-writable faltantes son fallo de muestra, no
   razón para sustituirla.
9. Se cierran listeners, se revocan capabilities y se destruye la VM. El scope
   no se reutiliza.
10. No se calculan ni muestran resultados parciales durante el lote.

No hay ejecución solapada entre muestras. Los probes de proveedor no usan
scopes de muestra. Ninguna petición puede ocurrir antes de que la muestra
empiece.

## 7. Única regla de decisión

[06-benchmark-plan.md](06-benchmark-plan.md) §5 es la única función. Resume:

- matriz completa A–D × OFF/ASSISTED × N fijo;
- calidad: Q1/Q4 en todas las tareas y Q6 sólo en C, missing/fallo = 0;
- eficiencia A/B/C: mediana M1u al menos 15% menor y M2 o M3 al menos 20%
  menor;
- D: M1u, M2 y M3, las tres dentro de ±10%;
- ties y baseline cero definidos;
- failures imputados con caps, retries de cliente contados, sin retries de
  muestra;
- cualquier posición no ejecutada por integridad produce
  `INSTRUMENT_NO_GO`;
- sólo `GO`, `NO_GO` o `INSTRUMENT_NO_GO`; no hay `ITERATE` post hoc.

M1, Q2, Q3 y Q5 son descriptivos. Ninguna prosa de V1/V2 cambia esta función.

## 8. Resolución de la segunda revisión REAL de Codex

| Hallazgo | Cierre V3 | Estado |
|---|---|---|
| P1-1 sanctions/host failures permiten re-batching | §2: toda muestra iniciada cuenta; fallo de instrumento termina sin eficacia; resto `NOT_RUN_INSTRUMENT`; cero replacements/relotes | **CLOSED_BY_DESIGN** |
| P1-2 respuestas exponen cuotas/estado compartido | §3 stripping/reconstrucción; G6 prueba scope; org independiente o NO-GO | **REQUIRES_GATE (G6/G7)** |
| P1-3 worker contamina scope reviewer | §4.1 listeners, UID, capability y epoch separados; reviewer nace tras revocar/quiescer worker | **CLOSED_BY_DESIGN**, implementación probada por G7/G9 |
| P1-4 Q6 permite shotgun | §4.2 schema, K=3, rank 1, span≤20, códigos y FP=0; adversariales G8 | **CLOSED_BY_DESIGN**, oráculo probado por G8 |
| P1-5 dos reglas de decisión | 06 §§3–5 y §7; V1/V2 no normativas | **CLOSED_BY_DESIGN** |
| P1-6 tratamiento/prompt invariant incorrectos | §1 usa `Provisioned.Render()` y spans pre-serialización; sólo primer worker prompt puede STOP | **CLOSED_BY_DESIGN**, captura probada por G5 |
| P2-1 DHCP persistente | §5.1 estática; fallback DHCP por clon con store desechable y canario | **REQUIRES_GATE (G2)** |
| P2-2 imagen contiene estado vivo | §5.2 manifest declarativo y clone sin autenticación | **CLOSED_BY_DESIGN**, probado por G0/G5 |
| P2-3 extracción/parsing bajo identidad con secretos | §5.3 helper/UID sin secretos, traversal seguro, sin archive agente | **CLOSED_BY_DESIGN**, probado por G8 |
| P2-4 G4 no falsificable | §10 fija payload, n, controles y límites exactos | **REQUIRES_GATE (G4)** |
| P3-1 contradicciones internas | V3 única norma, S_k por rol, N fijo, inventario persistente y función única | **CLOSED_BY_DESIGN** |

No queda ningún P0/P1 de diseño conocido **abierto**. P1-2 permanece
deliberadamente `REQUIRES_GATE`: V3 no inventa aislamiento de proveedor. Si el
gate falla y no son viables organizaciones independientes, el instrumento es
NO-GO.

## 9. Arquitectura futura (no autorizada todavía)

```text
identidad supervisor (sin provider secrets)
  plan, lifecycle, listener epochs, ledger
identidad secrets (sin acceso a evidencia hostil)
  gateway worker_k  -> worker_scope_k
  gateway reviewer_k -> reviewer_scope_k (apagado hasta review)
VM_k no confiable
  ctl root forced-command
  worker UID/sandbox -> listener worker únicamente
  reviewer UID/sandbox -> listener reviewer únicamente, después
identidad/VM helper (sin secrets, plan ni red)
  extracción, parsing, reconstrucción de diff
VM oracle (sin red)
  tests ocultos/Q4
```

La primary listener de AO sigue loopback y sin auth; este diseño no cambia las
fronteras de red del producto ni crea un bind de producción.

## 10. Gates empíricos restantes

Ninguno se ejecuta antes de que V3 pase revisión real de Codex.

| Gate | Criterio falsificable | Salida si falla |
|---|---|---|
| G0 viabilidad/imagen | AO y clientes fijos arrancan en clon; manifest exacto; sin capability no autentican; imagen no contiene ningún patrón/ID/estado prohibido | corregir imagen o `INSTRUMENT_NO_GO` |
| G1 destrucción | canarios en disco, RAM observable, defaults, pasteboard, keychain, tmp y procesos no aparecen en clon siguiente; hipervisor no lista disco/proceso | `INSTRUMENT_NO_GO` |
| G2 red/host state | estática sin DHCP; sólo listener de rol accesible; IPv4/6/link-local/multicast/DNS/LAN/host bloqueados; tablas ARP/NAT/firewall se purgan. Si DHCP inevitable, canario two-clone no cruza y SSH sólo permite return traffic established | corregir o `INSTRUMENT_NO_GO` |
| G3 caché Anthropic | payload elegible fijo; ≥60 pares same-workspace con positive-control hits ≥54/60 y lower 95% CI ≥0.80; 0/60 hits cross-workspace y upper 95% CI <0.05; orden alternado | organización independiente y repetir G6, o NO-GO |
| G4 caché OpenAI | modelo/endpoint/params/prefix elegible fijos; 60 pares same-project, ≥54 hits y lower exact 95% CI ≥0.80; 60 pares cross-project, 0 hits y upper exact 95% CI <0.05; 60 cross-org negative controls; indicadores de cached tokens son primarios, latencia sólo secundaria. Control positivo fallido o campo ausente = inconcluso/falla | reviewer Claude sólo si G3/G6 pasan; si no, NO-GO |
| G5 clientes/tratamiento | inventario completo de requests/responses y streaming; typed spans sobreviven delimiter/quoted JSON/tool output/transcript injection; todas las freshness variants; primer prompt byte-exacto; config sin server tools | corregir antes de preregistrar |
| G6 provider state | inventario exhaustivo de headers/cuerpos/cache/quota/rate/spend/policy/risk; saturation X no cambia admisión/estado normalizado/latencia clasificada de Y; reset y acumulación de canarios; enforcement scope documentado y observado | organización independiente por muestra+rol y repetir; si no es viable/probable, NO-GO |
| G7 gateway/roles | allowlist y response rebuilding por mutación; límites acumulativos; cross-role, replay, wrong session/scope/epoch, connection carry-over y token theft siempre rechazados; log no se pierde bajo volumen máximo | corregir |
| G8 Q6/extracción/Q4 | todos los adversariales de §4.2; FIFO/device/hardlink/symlink/sparse/archive/path race/malformed SQLite+JSONL; helper no alcanza secrets/red; Q4 sólo VM oracle | corregir |
| G9 control/lifecycle | worker no alcanza reviewer capability/listener antes/durante/después; quiescencia real de UID/FD/connections; ctl no ejecuta dotfiles/hooks/binaries del agente; ningún admin conocido | corregir o NO-GO |

Las CI exactas son Clopper–Pearson unilaterales al 95%, calculadas por código
congelado. Si los números indicados no satisfacen simultáneamente el límite, el
gate no pasa. G3/G4 no usan latencia para rescatar una señal de cache ausente.

## 11. Infraestructura que tendría que crear el operador (después, no ahora)

- 60–80 GB libres y ~8 GB RAM para una VM macOS activa, más helper y oracle
  secuenciales; hipervisor con clone COW y red aislable;
- red estática por clon o servicio DHCP desechable conforme a G2; filtro host
  temporal;
- cuatro identidades separadas: supervisor, secrets/gateway, parser/helper y
  oracle; ninguna combina secrets con datos hostiles;
- organización(es) de laboratorio de Anthropic y, si G4 pasa, OpenAI; límites
  y scopes exclusivos, sin cuentas personales;
- llaveros/secret stores de laboratorio y creation workflow fuera del lote;
- fixture, hidden tests y manifest Q6 guardados fuera de imagen y pack.

Con N=5 hay **40 muestras**. Si G4 pasa: 40 workspaces Anthropic de worker y
40 proyectos OpenAI de reviewer, más aproximadamente 6 scopes Anthropic y 6
OpenAI para gates/ensayo declarativo. Si G4 falla pero G3/G6 permiten Claude:
80 workspaces Anthropic (worker+reviewer), más aproximadamente 8 de gate. No se
incluyen mini-E2E ni replacements: V3 no autoriza ninguno y no existen scopes
de reserva.

Si G6 obliga a independencia organizacional, el máximo metodológico pasa a 40
organizaciones por proveedor/rol usado (o 80 organizaciones Anthropic si ambos
roles usan Claude), cada una con un scope de muestra, más organizaciones de
control. Esa escala puede ser contractual u operativamente inviable; en tal
caso el resultado correcto es `INSTRUMENT_NO_GO`, no reducir N.

El coste monetario aún **no es calculable honestamente**: faltan modelo/price
lock, caps obtenidos sin ejecutar una muestra de eficacia, ruta G4 y resultado
G6. El preregistro futuro debe calcular el máximo como suma de caps de las 40
muestras más gates, nunca extrapolar sólo completions exitosos. V3 no crea ni
financia nada.

## 12. Cambios respecto a V2

- elimina relotes y cualquier clean replacement; instrumento roto termina el
  experimento y conserva posiciones no ejecutadas;
- reconstruye respuestas y elimina estado/cuotas compartidos; G6 decide entre
  partición real, organizaciones independientes o NO-GO;
- hace imposible que worker vea o consuma el scope reviewer mediante listener,
  UID, capability y lifecycle separados;
- reemplaza Q6 “cualquier overlap” por schema acotado, ranking y FP=0;
- reemplaza la regla vieja de 06 y la alternativa V2 por una función única;
- define el tratamiento como `Provisioned.Render()` y exige typed origin spans
  antes de serialización; sólo el primer worker prompt puede detener;
- prefiere red estática, prohíbe una imagen derivada de HOME vivo, separa
  parser de secrets e inventaría estado persistente sin afirmar inexistencia;
- convierte G4 en un test falsificable y preserva N≥5 ante cualquier gate.

## 13. Revisión estática interna de V3

Se hizo una pasada estática, sin subagente y sin ejecutar infraestructura,
buscando: `relot|replacement|retry`, reglas GO/NO-GO duplicadas, tratamiento
reducido a `ContextPack.Render`, scopes singulares, reviewer capability activa
durante worker, reducción de N, DHCP implícito, parsing bajo secrets, claims de
“nada persiste”, Q6 sin penalización y gates sin salida.

Correcciones aplicadas durante esa pasada:

- “muestra iniciada” quedó ligada a la activación del listener, no a una
  respuesta del proveedor;
- las pausas sólo existen antes del inicio y conservan la misma asignación;
- provider sanction separa resultado de muestra de pérdida posterior del
  instrumento;
- Q6 trata duplicados y findings extra como falsos positivos;
- los ratios con baseline cero, ties, missing y caps quedaron definidos en 06;
- los scopes de canario no cuentan como muestra ni pueden reutilizarse.

Resultado interno: no se conoce contradicción P0/P1 abierta. Esta revisión no
sustituye el ataque real de Codex.

## 14. Stop

El siguiente paso permitido es exclusivamente la revisión estática real de
Codex con el prompt V3. No implementar, crear infraestructura/cuentas, ejecutar
gates, preregistrar, correr mini-E2E, 3D, 3E o 3F, desplegar ni mergear.

**PRECONDITION_3D_AUTH_DESIGN_V3 = READY_FOR_CODEX_REVIEW.**

**PRECONDITION_3D = NO-GO.**
