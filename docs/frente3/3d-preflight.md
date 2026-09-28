# Frente 3 / 3D — prerequisitos de validez experimental (preflight)

Fecha: 2026-09-25. Rama: `feat/frente3-3d-prerequisites`, creada desde ECC `e2e9c741d`, donde 3C ya está CLOSED.

**Alcance.** Neutralizar los cuatro bloqueantes que salieron en los E2E de 3C antes de ejecutar ningún A/B. **El experimento de 3D no se ha ejecutado.**

Regla: nada de esto cambia el comportamiento para favorecer a Memory. Cada corrección aplica igual a los dos brazos.

## 1. Carrera en el dispatch del reviewer

**Síntoma.** Apareció en 2 de 6 runs E2E, una vez en cada brazo:

1. se crea el review run;
2. entre 30 y 160 ms después se libera su claim con "the review run was created but no reviewer launch was ever recorded";
3. la review queda `failed` y el run pasa a `needs_attention`;
4. el reviewer arranca igualmente.

**Causa raíz.**

- `dispatchReviewStep` se vuelve a ejecutar en cada pasada sobre el run (wake poller, continue).
- Cuando encuentra el outbox `dispatched`, siempre tomaba el camino de recuperación tras crash: `adoptReviewOrMarkAmbiguous` → `adoptExistingReviewRun`. Ese camino existe para un dispatch que **ya no existe**.
- Pero el dueño del claim podía seguir vivo **en el mismo proceso**, aprovisionando el contexto del reviewer y arrancando su sesión.
- La recuperación sondeaba un reviewer que aún no existía, lo declaraba ausente, fallaba el review run y liberaba el claim.
- El dispatch vivo terminaba lanzando un reviewer sobre una review ya fallida. En un test determinista se ve un **segundo** reviewer.

**Corrección** (`internal/workflow/review_dispatch_inflight.go`):

- Un conjunto en memoria registra los claims (entrada de outbox + generación) cuyo dispatch se está ejecutando en este proceso. Se entra al ganar el claim y se sale al terminar el dispatch.
- En la rama `dispatched`, si el dueño de esa generación exacta está vivo aquí, la pasada **no concluye nada** y deja que el dispatch termine su propia transición.
- Es exacta, no una ventana de tiempo. No hay sleeps ni timeouts nuevos.
- Tras un reinicio el conjunto está vacío, que es justo el caso para el que existe la recuperación. La recuperación durable no cambia.

**Tests:**

- `TestReviewDispatchInProgressIsNotDeclaredAbsentByAConcurrentPass`: invoca `ContinueRun` desde dentro de la ventana de lanzamiento. **Sin el fix falla** con dos reviewers (2 inserts y 2 lanzamientos).
- `TestReviewDispatchRecoveryAfterRestartIsUnchanged`: un segundo Coordinator sobre el mismo estado durable, es decir un daemon reiniciado, sigue recuperando como antes.

## 2. Review requerida que a veces no se ejecuta

**Diagnóstico.** No es un fallo, es política:

- Una run task pide por defecto `reviewDepth=none`.
- La relief por evidencia (`EvaluateReviewEvidenceRelief`) puede bajar el suelo `light` a `none` cuando los checks pasaron y el worker no "confesó" nada en su auto-reporte.
- Ese auto-reporte es texto del agente. Por eso un brazo podía quedarse sin review y el otro no: la ruta de review dependía de la redacción del worker.

**Corrección experimental, sin cambiar el producto.** En 3D todas las runs se crean con `reviewDepth: "light"` explícito.

- `effective = DeeperOf(light, suelo)`, que da `light` o más aunque la relief se conceda.
- La review se ejecuta siempre y por la misma ruta en los dos brazos.
- Queda congelado en el snapshot (`reviewDepth.requested = light`, fuente explícita).

## 3. Colisión del ID de sesión de Claude

**Causa.** El adapter de Claude deriva el UUID de sesión de forma determinista a partir del ID de sesión de AO (`claudecode.go`). Si una run reutiliza la misma ruta de trabajo, el mismo proyecto y la misma sesión (`fx-1`) que otra anterior, Claude se niega a arrancar ("Session ID … is already in use").

**Aislamiento en 3D.** Cada repetición tiene su propio data dir, su propio clon del repo, su propio puerto y un **ID de proyecto único**. Así cambian tanto la ruta de trabajo como el UUID derivado.

Se verifica por run:

- que no aparece "already in use" en el log;
- que todos los transcripts se **crearon durante el run** (`st_birthtime`);
- que los IDs de sesión nativos no se repiten.

**Qué se comparte, constante entre brazos.** El `HOME` del provider: settings globales de `~/.claude` y la configuración de Codex. La auto-memoria de Claude Code va por ruta de trabajo, así que queda aislada por run.

**Caché de prompts del provider (servidor).** No se puede aislar. Se reparte con intercalado de brazos y se reporta como covariable (`cachedInputTokens`, `cacheWriteTokens`).

## 4. Contexto externo de GitHub

**Causa.** El contexto de GitHub viaja dentro del provisioner de memoria: solo lo recibía el brazo `assisted` (587–712 B "degraded"). El brazo `off` no recibía nada externo.

**Corrección.** `AO_MEMORY_EXTERNAL=off`. Por defecto sigue activo, así que el comportamiento no cambia. En 3D se fija en los dos brazos.

- La elección efectiva queda congelada en `policy_snapshot.contextSources.externalContext` (`off`, o `github`).
- La expone `GET /workflows/{id}/exploration` y `ao workflow exploration`.

**Variable congelada:** `externalContext = off` en ambos brazos. Ninguno recibe información externa.

## Mini-E2E de validación

Harness: `~/.ao/scratch/frente3/tools/preflight3d.py`.

- N = 4 por brazo, con el orden de brazos alternado por repetición.
- Binario compilado desde `./cmd/ao` a HEAD.
- El preflight de cada daemon imprime y valida `AO_DATA_DIR`, `AO_RUN_FILE`, el puerto y el modo.
- Resultados: sección siguiente, rellenada tras la ejecución.

### Resultados (`3d-preflight/run-20260925T132434`, binario de `bbff108f7`)

| Run | Estado | reviewDepth pedido → efectivo | Review | `contextSources` | Packs | Síntoma de la carrera | Guarda activada | Colisión | Transcripts nuevos | Cobertura |
|---|---|---|---|---|---|---|---|---|---|---|
| off-1 | completed | light → light | complete/approved (codex) | off/off/off | — | 0 | 0 | 0 | 2/2 | ✓ |
| assisted-1 | completed | light → light | complete/approved | assisted/off/off | worker, reviewer | 0 | 1 | 0 | 2/2 | ✓ |
| assisted-2 | completed | light → light | complete/approved | assisted/off/off | worker, reviewer | 0 | 1 | 0 | 2/2 | ✓ |
| off-2 | completed | light → light | complete/approved | off/off/off | — | 0 | 0 | 0 | 2/2 | ✓ |
| off-3 | completed | light → light | complete/approved | off/off/off | — | 0 | 1 | 0 | 2/2 | ✓ |
| assisted-3 | completed | light → light | complete/approved | assisted/off/off | worker, reviewer | 0 | 1 | 0 | 2/2 | ✓ |
| assisted-4 | completed | light → light | complete/approved | assisted/off/off | worker, reviewer | 0 | 1 | 0 | 2/2 | ✓ |
| off-4 | completed | light → light | complete/approved | off/off/off | — | 0 | 1 | 0 | 2/2 | ✓ |

- **Carrera.** La **ventana existe a menudo**: la guarda se activó en 6 de 8 runs, en ambos brazos. En ninguno se liberó un claim en vuelo ni se abandonó una review.
- **Ruta de review.** Idéntica en los 8 runs: se ejecutó siempre, con 0 omisiones por evidencia.
- **Sesiones.** 16 IDs de sesión nativos, 16 únicos, y 0 "already in use".
- **Contexto externo.** `off` en ambos brazos. El brazo `assisted` recibe solo el pack de memoria, sin contexto externo.
- **Producción.** Mismo mtime, mismo tamaño y goose 174 antes y después.

## Ciclo 2 (tras la revisión de Codex: NO-GO por un P1)

### P1: hueco entre el claim durable y el registro local

**Hallazgo de Codex.**

- `reviewInFlight.begin` se llamaba **después** de que `ClaimWorkflowOutboxDispatch` volviera.
- Entre el CAS (la fila ya se lee `dispatched` con la generación nueva) y el registro, una pasada concurrente veía el claim sin marca y lo trataba como abandonado.
- Además, el mapa guardaba una sola generación por entrada.

**Diseño final, derivado de las invariantes de AO.**

- La generación la acuña el registro durable `AUTHORIZED` de cada dispatch **antes** de disputar la fila (`recordReviewDispatchAuthorized` devuelve `wfc-<id>`). Por eso cada aspirante tiene una clave (entrada, generación) propia y única.
- La clave se **reserva antes del CAS** y se suelta cuando el dispatch termina, incluido el caso de CAS perdido:
  - el ganador está vivo desde antes de que la fila pueda leerse `dispatched`;
  - la reserva de un perdedor nombra una generación que la fila nunca tiene, así que no protege nada;
  - N y N+1 nunca comparten clave;
  - la comprobación de recuperación usa la generación que la fila durable nombra como dueña.
- Se conservan:
  - la semántica CAS y de generación;
  - la propiedad ownership-conditioned de liberar y fallar;
  - la recuperación tras reinicio (conjunto vacío);
  - un solo lanzamiento.
- No hay sleeps, ventanas ni reintentos.

**Tests** (`review_dispatch_inflight_test.go`, `review_dispatch_inflight_internal_test.go`):

| Test | Qué demuestra |
|---|---|
| `...DurableClaimBeforeLocalRegistrationIsLive` | Pasada concurrente con el claim ya durable y el dispatch sin volver del CAS: no hay ausencia, ni ambigüedad, ni liberación del claim, ni lanzamiento concurrente; al final, un solo reviewer. Es la reproducción de Codex con aserciones completas |
| `...InProgressIsNotDeclaredAbsentByAConcurrentPass` | Pasada concurrente dentro de la ventana de lanzamiento |
| `...RecoveryAfterRestartIsUnchanged` | Un Coordinator ajeno (reinicio) sigue recuperando |
| `...InFlightKeysAreGenerationScoped` | N y N+1 no se protegen, borran ni ocultan entre sí, y un fin repetido de N es inocuo |
| `...ReservationDoesNotCoverAReclaimedGeneration` | Con N en vuelo, la fila pasa a N+1 ajena: la pasada evalúa N+1 por sus hechos y nunca hay dos reviewers |

**Mutaciones** (`~/.ao/scratch/frente3/reviews/3d-preflight/cycle2/`):

| Mutación | Resultado |
|---|---|
| **M1**: registro después del CAS (el diseño refutado) | falla `...DurableClaimBeforeLocalRegistrationIsLive` |
| **M2**: sin la comprobación de claim vivo | fallan `...InProgress...` (2 reviewers) y `...DurableClaim...` |
| Control (con el fix) | 5/5 PASS |

### Fuga del brazo al agente (encontrada en este ciclo)

- El servidor tmux de AO hereda el entorno del daemon, así que `AO_MEMORY_MODE=assisted` llegaba al shell del worker y del reviewer.
- Ahora `AO_MEMORY_MODE`, `AO_MEMORY_EXTERNAL` y `AO_CONTEXT_ROUTER` se retienen junto con las variables de agente anidado (`sanitizeInheritedEnv`), porque solo las lee el daemon al componerse.
- El harness comprueba con `tmux show-environment -g` que no aparecen en el socket del run.

### Inventario de estado del provider

| Estado | Claude Code | Codex | Tratamiento en 3D |
|---|---|---|---|
| HOME | compartido | compartido | constante entre brazos (no se aísla: rompería la autenticación) |
| Autenticación | keychain | `~/.codex/auth.json` | nunca se lee ni se copia; solo el mtime de `auth.json` |
| Configuración global | `settings.json`, skills, plugins (no hay `CLAUDE.md` global) | `config.toml`, skills y rules | hash antes y después de cada run; debe ser idéntico |
| Transcripts e historial de sesión | `~/.claude/projects/<slug-de-cwd>` | `~/.codex/sessions` por id | aislados por ruta e id únicos por run; se verifica `birthtime` e ids únicos |
| Auto-memoria | por ruta de proyecto | feature `memories` = **false**, 0 filas | aislada por ruta (Claude); desactivada y vacía (Codex), y se verifica por run |
| Historial de entrada del TUI | `history.jsonl` | `history.jsonl` | compartido; no se inyecta al modelo; residual documentado |
| Historial de shell | el shell no interactivo de los agentes no escribe historial | igual | residual, no verificado |
| Goals | — | `goals_1.sqlite`, por conversación | residual |
| Caché de prompts del servidor | compartida | compartida | no aislable: intercalado balanceado y covariables `cached`/`cacheWrite` |
| Cuenta y rate limits | compartidos | compartidos | residual; se registran fallos de capacidad |

### Harness endurecido (`tools/preflight3d_v2.py`)

- **A. reviewDepth:** tras crear cada run se lee la política **persistida** (`run.reviewDepth`) y se aborta si no es `light` + `explicit`. El controlador ignora el error de `ApplyReviewDepthPolicy`; queda como deuda separada: la API debería rechazar la creación si la política no se aplica.
- **B. Provider:** el estado compartido se hashea antes y después de cada run.
- **C. IDs opacos:** tags `secrets.token_hex(4)`, proyecto `p<hex>` y socket `aolab-<tag>`.
  - El mapping tag → brazo y los logs del daemon viven solo en `3d-preflight/evidence-*`, fuera del árbol de trabajo `3d-lab/`.
- **D. Orden:** pares barajados con semilla aleatoria; la semilla y el plan exacto se guardan en `results.json`.
- **E. Fixture:** por cada repetición se verifican el origen, el SHA exacto y el árbol limpio; se aborta si no coinciden.
- **F. Contexto externo:** `AO_MEMORY_EXTERNAL=off` se imprime en el preflight y se verifica `externalContext=off` en el snapshot.

### Procesos huérfanos entre repeticiones (hallado durante el ciclo 2)

- AO mantiene vivas las sesiones de los agentes cuando el daemon se para. Es intencional, para poder recuperarlas.
- Tras los E2E quedaban **28 servidores tmux de scratch** con paneles de reviewer de runs anteriores. En 3D eso sería contaminación de recursos entre repeticiones.
- Se cerraron (solo sockets cuyos paneles estaban todos bajo `~/.ao/scratch/frente3/`).
- El harness ahora destruye el socket `aolab-<tag>` de cada repetición justo después de parar su daemon (`tmuxTeardown`).

## Ciclo 3 — preregistro del mini-E2E (publicado ANTES de ejecutarlo)

Archivo privado (fuera del alcance de los agentes): `~/.ao/scratch/frente3/3d-private/prereg-minie2e-20260926T003041.json`
sha256 del archivo: `4abfa69b0ffdde62fe7c1f8b9b174b595b4fe8634ebefcfa92c07fccda7e211b`

| Valor | Preregistrado |
|---|---|
| EXPECTED_FIXTURE_SHA | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` (tomado de la evidencia del ciclo 2, no del fixture a validar) |
| EXPECTED_AO_BINARY_SHA | `9e2b5739eda47de5b2b252a39a8914a2e87ed5344daef78c8514f07d029849f3` (binario de `e85a620a8`, compilado desde un clon limpio, firmado ad-hoc `-o restrict`; sin firmar: `cef3c231669c4d5a4c90e8696e6cad514e9ff9203089cba6aafcb9e9e39007cf`) |
| EXPECTED_HARNESS_SHA | `a9b08247c9227a9c90136812a8ba6bc392d9bcf13703a658d878a75831b00eac` |
| planSha (semilla y plan privados) | `d3b64ecb7e0a6433106afb5b4305fbdb0a34d61f217e47f027da54aed9caab59` |
| N por brazo | 2 |
| Snapshot Codex config.toml / rules | `96db92453db61e1a…` / `e20489d400dbb8cc…` |
| Reintentos por capacidad | máximo 2 por muestra (regla completa abajo) |

Archivos del harness y su sha256:

- `.ao/scratch/3c/run-claude.json`: `4e7ffd384dfa8db7…`
- `.ao/scratch/frente3/tools/aoexp.py`: `65ab8d6b960e59c2…`
- `.ao/scratch/frente3/tools/lab3d/agent.sb`: `786e33358c4d019a…`
- `.ao/scratch/frente3/tools/lab3d/analyze.py`: `4def361102dd1f71…`
- `.ao/scratch/frente3/tools/lab3d/gateway.py`: `153846521447af17…`
- `.ao/scratch/frente3/tools/lab3d/labcore.py`: `ca41f856ae1f0adb…`
- `.ao/scratch/frente3/tools/lab3d/prereg.py`: `9a374c0fcb8dce9d…`
- `.ao/scratch/frente3/tools/lab3d/probe.py`: `ed297de79d88be63…`
- `.ao/scratch/frente3/tools/lab3d/run.py`: `914c797ad6629933…`
- `.ao/scratch/frente3/tools/lab3d/shim.sh.tmpl`: `8111cf305467d638…`

Regla de capacidad preregistrada:

> An attempt is INVALID_PROVIDER_CAPACITY iff (1) AO durably recorded at least one provider capacity event (workflow_attempts.error_class in rate_limited|capacity_exhausted, or a review_capacity_retry checkpoint), (2) the run did not reach state=completed, and (3) every failed attempt of the run carries a capacity error class (no non-capacity failure). Such an attempt is kept as evidence, never counted, and the SAME planned sample (same task, same arm, same plan position) is re-run under a fresh opaque tag, at most 2 times. A third capacity-invalid attempt for one sample STOPS the experiment (NEEDS_ATTENTION). Every other attempt that passed its preconditions is a VALID sample whatever its outcome (completed, failed, needs_attention, timeout); a completed run that saw capacity events is VALID and reports capacityEvents as a covariate. No sample is ever selected, dropped or re-run on the basis of its outcome.

Precondiciones que ABORTAN (el intento no cuenta y el lote se detiene):

- externalContext != off
- persisted reviewDepth != light/explicit
- fixture clone SHA != EXPECTED_FIXTURE_SHA
- fixture clone dirty
- fixture source SHA != EXPECTED_FIXTURE_SHA
- AO binary sha256 != EXPECTED_AO_BINARY_SHA
- harness digest != EXPECTED_HARNESS_SHA
- provider FROZEN fingerprint != pre-registered (before or after)
- live aolab tmux server from an earlier repetition
- arm switch visible in the run's tmux global env
- non-opaque tag/project/socket
- probe: explicit arm label reachable
- probe: reach table != expected
- probe: a loopback port other than the gateway connectable
- gateway: response-leak violation
- no probe report from a launched agent

## Ciclo 3 — cegamiento experimental, fencing de generación y harness fail-closed

Motivo: el NO-GO del ciclo 2 (Codex). P1: el agente podía leer su brazo (`AO_DATA_DIR` → `ao.db` → `policy_snapshot.contextSources.memoryMode`, y la API de exploración). P2: fencing de generación, controles del harness que solo registraban, regla de capacidad.

Principio: **cegar al agente, no reducir la observabilidad del producto.** `memoryMode`, `policy_snapshot` y la exploración siguen intactos en AO; lo que cambia es lo que un agente del laboratorio puede alcanzar.

### Cambios en el producto (mínimos)

| Commit | Qué |
|---|---|
| `2c5614f88`, `9cf25d244` | El daemon lee `AO_MEMORY_MODE` / `AO_MEMORY_EXTERNAL` / `AO_CONTEXT_ROUTER` una vez, al inicio de `RunWithConfig`, y los quita de su entorno antes de lanzar ningún hijo. Motivo: macOS deja leer el entorno de cualquier proceso del mismo usuario (`KERN_PROCARGS2`), y los hijos fuera de tmux (probes de proveedor, comandos de verificación) heredaban los interruptores. La sonda del laboratorio lo detectó en los probes de arranque. |
| `5b30da51d`, `e85a620a8` | Fencing de generación del lanzamiento del reviewer (ver abajo). |

Ninguna API productiva cambió.

### Laboratorio (harness, fuera del producto): `~/.ao/scratch/frente3/tools/lab3d/`

- **Shim por repetición** (`shim.sh.tmpl`): el PATH del daemon resuelve `claude`/`codex` a un shim que da al agente un entorno por lista blanca y lo encierra, junto con todos sus hijos (hooks, shells, herramientas), en `agent.sb` (Seatbelt).
- **`agent.sb`** niega:
  - todo `~/.ao` salvo el árbol propio de la repetición, y dentro de él `ao.db*` y `daemon.lock` (se permite solo `stat` de ancestros, necesario para git);
  - los árboles de código y docs de AO;
  - en `~/.claude`: `projects` salvo el propio, `history.jsonl`, `paste-cache` (contiene el texto del diseño pegado por el operador) y `file-history`;
  - todo `~/.codex` salvo `auth.json`, `skills` y `plugins`;
  - todos los puertos loopback salvo el del gateway, los sockets de tmux (escape del sandbox), el socket supervisor del daemon, y `launchctl`/`open`/`osascript`/`tmux`.
  Todas las reglas se verificaron empíricamente. Tres hallazgos:
  - un deny de ficheros **no** impide `connect(2)` a un socket unix;
  - una regla de red filtrada por ruta también afecta a conexiones IP de loopback, por eso el orden importa;
  - `exec` se evalúa sobre la ruta resuelta (el tmux de Homebrew está en `Cellar`).
- **`CODEX_HOME` por repetición:** sesiones, historial y estado nuevos. `config.toml` y `rules` son copias del snapshot preregistrado (otras sesiones de Codex del equipo reescriben los vivos). `auth.json`, `skills` y `plugins` son symlinks: el secreto nunca se lee ni se copia.
- **Gateway** (`gateway.py`): el único endpoint de AO que alcanza un agente.
  - Lista blanca: `GET /healthz`; `POST` a `sessions/{id}/activity`, `reviews/{id}/activity`, `usage/subject-hook`, `sessions/{id}/work-report` y `sessions/{id}/reviews/{submit,cancel,trigger}`.
  - Todo lo demás recibe 403.
  - Una respuesta permitida que contenga un campo revelador se sustituye por 502 y se registra como violación.
  - El shim reescribe el `AO_RUN_FILE` que el adaptador de Codex pasa por `-c shell_environment_policy.set`, para que apunte al run file del gateway.
- **Daemon firmado ad-hoc `-o restrict`** (CS_RESTRICT): su entorno no es legible vía `procargs2`. Se verificó que Seatbelt no puede filtrar `procargs2` sin romper el runtime del agente.
- **El daemon recibe un entorno por lista blanca**, no el del operador.
- **Sonda adversarial** (`probe.py`): se ejecuta con exactamente el entorno y el sandbox del agente, justo antes de lanzarlo, en cada lanzamiento (worker y reviewer). Prueba:
  - env;
  - `procargs2` de todos los procesos del usuario;
  - alcance del filesystem;
  - inventario normalizado del árbol propio;
  - escaneo completo de puertos loopback (1–65535) y LAN;
  - rutas de control-plane a través del gateway;
  - sockets tmux, supervisor y browser;
  - `launchctl`/`open`/`osascript`/`ps`/`tmux`.
- **Diferencial entre brazos** (`analyze.py`): busca claves cuyo valor es constante dentro de cada brazo y distinto entre brazos. Eso es lo que parecería una etiqueta.

### Harness fail-closed (`run.py`)

Cualquier violación ABORTA el intento. El intento no cuenta y el lote se DETIENE (NEEDS_ATTENTION). Las precondiciones y la regla de capacidad son las del preregistro de arriba. Se comprueban antes de **cada** intento:

- binario;
- digest del harness;
- fixture (fuente y clon);
- fingerprint profundo del proveedor;
- servidores tmux vivos;
- IDs opacos.

Después de cada intento:

- `externalContext`;
- `memoryMode` entregado igual al asignado;
- sondas;
- violaciones del gateway;
- fingerprint;
- symlinks de `codex-home`.

Se vio funcionar en los smokes:

- una edición del harness en caliente abortó el intento siguiente;
- un cambio de `~/.claude/skills` y `~/.codex/rules` hecho fuera del laboratorio abortó el smoke 3. El primero es un marcador de sincronización (ahora excluido); el segundo motivó el snapshot de Codex.

### Fingerprint del proveedor

Es profundo, por contenido.

**FROZEN** (un cambio aborta):

- `~/.claude/settings.json`;
- árboles `skills` y `plugins` de Claude y de Codex;
- `mcpServers` de `~/.claude.json`;
- binario de Claude (ruta y sha256) y `--version`;
- árbol del paquete de Codex (incluye el binario nativo) y `--version`;
- `codex features list`;
- `memories` = false y 0 filas;
- snapshot de `config.toml` y `rules` de Codex.

**OBSERVED** (solo se registra):

- `stat` de `auth.json` (nunca se lee);
- tamaños de los historiales;
- digest de la copia viva de `config.toml` y `rules`.

### Fencing de generación (P2)

- `reviewClaimOwned`: la fila durable debe ser la misma entrada, estar `dispatched` y tener **exactamente** la generación de este dispatch.
- `LaunchFence` viaja dentro de la petición de lanzamiento. Se comprueba:
  - en el ready check;
  - tras el probe, en `ensureReviewerLaunched`;
  - en el launcher del daemon, inmediatamente antes de `runtime.Create` (después del aprovisionamiento de contexto).
- Un titular que ya se sabe desplazado no toca el step, el presupuesto ni el outbox del sucesor.
- Tests deterministas, sin sleeps:
  - reclaim antes y después del ready check;
  - fallo de un N obsoleto;
  - dos coordinadores solapados;
  - ventana de aprovisionamiento dentro del launcher;
  - recuperación tras reinicio, con exactamente un reviewer.
- Mutaciones: G1, G2, G3 y G5 las detectan los tests; G4 (guarda en la ruta de liberación) sobrevive porque hoy no hay camino que la ejercite.
- Residual: la ventana es `runtime.Create` en sí, y explotarla requiere dos daemons sobre el mismo data dir, cosa que impide el lock exclusivo.

### Mini-E2E (`3d-private/batch-20260926T003103`, binario `e85a620a8`)

| Muestra | Brazo | Estado | Review | `contextSources` | Manifests | Capacidad | Sondas de lanzamiento |
|---|---|---|---|---|---|---|---|
| 1 | off | completed | complete/approved (codex) | off/off/off | — | 0 | worker + reviewer: PASS |
| 2 | assisted | completed | complete/approved (codex) | assisted/off/off | worker, reviewer | 0 | PASS |
| 3 | assisted | completed | complete/approved (codex) | assisted/off/off | worker, reviewer | 0 | PASS |
| 4 | off | completed | complete/approved (codex) | off/off/off | — | 0 | PASS |

- 4/4 VALID. `reviewDepth` efectivo light en las 4. 0 colisiones de sesión, 0 síntomas de carrera.
- **Diferencial de cegamiento:** 0 diferencias consistentes con el brazo, en worker y en reviewer. Todo lo que varía lo hace también dentro de un mismo brazo: IDs y tokens aleatorios, reflogs de git, los commits del propio agente y temporales de Codex. El pack de memoria llega en el prompt: no hay fichero ni variable que lo delate.
- El gateway dejó pasar las mismas rutas en ambos brazos.
- Producción: goose 174, mismo mtime y tamaño antes y después.

### Residuales documentados

- Seatbelt comparte UID con el operador. Un agente que ejecute código nativo arbitrario para escapar del sandbox queda fuera del modelo de amenaza, aunque las vías conocidas (tmux, launchd, LaunchServices, osascript) están cerradas. El aislamiento fuerte requeriría otro usuario del SO o una VM.
- `AO_BROWSER_RUNTIME_ADDRESS` sigue alcanzable: es herramienta del agente e igual en ambos brazos.
- Restricción del laboratorio: las tareas no pueden depender de red loopback. El fixture no la usa.
- Estado de Claude que sigue legible porque Claude lo necesita: `shell-snapshots`, `sessions`, `session-env`, y la lista de proyectos de `~/.claude.json` (solo rutas, confianza y versión, sin prompts). Nada de esto contiene etiquetas de brazo.
- Caché de prompts del proveedor (servidor), cuenta y límites: compartidos y no aislables. Se reparten con el intercalado y se reportan como covariables.

### Revisión de Codex del ciclo 3: NO-GO (P0 0 / P1 3 / P2 5 / P3 1)

Revisión completa en `~/.ao/scratch/frente3/reviews/3d-preflight/cycle3/redteam/review.md`.

**Cerrado según Codex:**

- La fuga del ciclo 2 (`AO_DATA_DIR` → `ao.db` → `policy_snapshot`, API de exploración).
- El fencing de generación en la topología soportada.
- La procedencia del congelamiento.
- La igualdad de tratamiento entre brazos en los datos.
- Producción intacta.

**P1 abiertos (todos en el harness, ninguno en el producto):**

1. `~/.claude.json` sigue legible (el perfil hace `allow default` y solo niega `~/.claude/projects`). Contiene rutas de corridas antiguas de 3C/3D con los componentes literales `/assisted/` y `/off/`. No mapean el tag opaco actual a su brazo, pero son etiquetas explícitas alcanzables. La sonda no inspeccionaba ese fichero, y la documentación afirmaba lo contrario.
2. `check_probe` acepta un canal de la sonda que devolvió error (`processTable`, `ownTreeInventory`, `lan`). Con esa mutación, una muestra seguiría siendo VALID.
3. El `reviewDepth` efectivo se registra pero no se exige. Con la mutación light→full, la muestra pasa.

**P2:**

- Aserciones no fatales: el commit del rebuild, `explorationSettled`, `tmuxTeardown`, y una excepción genérica en `post_checks` que puede dejar `COMPLETE`.
- La mayor parte de `~/.claude.json` solo se observa, no se congela.
- La rama `not failed_classes` de la regla de capacidad puede ocultar un fallo posterior no relacionado.
- El fence depende del launcher concreto: falta un test de la cadena de producción.
- La mutación G4 sobrevive, y `TestP9Crash_C10` es flaky (no es topología de producción).

**P3:** la publicación del preregistro está anclada solo localmente (commit sin push).

**PRECONDITION_3D = NO-GO.** 3D no se ejecuta.

## Ciclo 4 — cierre del NO-GO del ciclo 3

Autorización: un solo ciclo, solo para cerrar los hallazgos. Los P1 eran del harness. Producto: únicamente tests nuevos (`35e3ad6e7`); ningún cambio en código productivo.

### P1-1: estado de Claude aislado por repetición

- Cada repetición tiene un **`HOME` de proveedor propio** (`<own>/home`), compartido por su daemon y sus agentes.
  - El daemon también lo usa: AO registra la confianza del workspace en el `~/.claude.json` del `HOME` del daemon y localiza los transcripts ahí.
  - Consecuencia: el `~/.claude.json` real del operador ya **no recibe ninguna entrada** del laboratorio. Se comprueba antes y después de cada intento (`real_claude_json_lab_entries`), y cualquier entrada aborta.
- `CLAUDE_CONFIG_DIR` descartado: con un directorio distinto, Claude busca otra entrada del llavero (`loggedIn: false`). Copiar la credencial está prohibido.
- Solución verificada:
  - `HOME` aislado con `Library/Keychains` como **symlink** al llavero real. `security(1)` resuelve el llavero de login por `$HOME`.
  - Claude autentica exactamente como siempre (`loggedIn: true`, llamada real OK) y la credencial nunca se lee ni se copia.
- **Contenido del `HOME` de proveedor:**
  - `settings.json` y `.gitconfig`: copias del snapshot preregistrado.
  - `~/.claude.json`: semilla mínima con solo banderas de onboarding.
  - Sin plugins ni skills del operador.
  - Sin autoinstalación del marketplace oficial (`CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1`). Un ensayo mostró que un `HOME` nuevo lo descarga, y su README contiene la palabra "assisted".
- **`agent.sb`** niega todo el estado real de proveedores: `~/.claude`, `~/.claude.json*`, `~/.cache/claude`, las cachés, Application Support y logs de Claude y OpenAI, y `~/.codex` salvo `auth`, `skills` y `plugins` en solo lectura.
- **La sonda** comprueba explícitamente cada uno de esos almacenes (`providerState`), incluido el `~/.claude.json` real y sus hermanos, y que el `HOME` del agente es el de la repetición.
- **Estado del lado del servidor** (flags de funcionalidad `cachedGrowthBookFeatures` y `cachedExperimentFeatures`, skills y plugins sincronizados de la cuenta). Los ensayos mostraron que no es determinista en un `HOME` nuevo:
  - un `userID`/`machineID` aleatorio por home daba flags distintas (3 de 724);
  - la sincronización asíncrona llegaba o no según el tiempo;
  - aun con IDs fijos, 13 flags cambiaron entre la plantilla y la corrida, porque hay flags dinámicas (p. ej. avisos de capacidad).

  Solución:
  1. El preregistro construye **una plantilla de home de Claude**: IDs fijos de laboratorio derivados de la semilla preregistrada, nunca copiados del operador; se inicializa hasta que el estado queda completo y estable en dos corridas seguidas.
  2. Se guarda y digiere; cada repetición parte de una copia idéntica.
  3. Durante las corridas no se vuelve a pedir nada al servidor (`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, verificado: el estado queda idéntico tras llamadas reales).
  4. Tras cada corrida el estado debe seguir igual a la plantilla; si no, aborta.

### P1-2: sonda fail-closed

- `check_probe` exige los 9 canales obligatorios: env, processTable, filesystemReach, providerState, ownTreeInventory, loopback, lan, gatewayControlPlane y escapes.
- Un canal falla si:
  - falta o está mal formado;
  - contiene `error`, aunque además traiga valores plausibles;
  - trae valores fuera de lo esperado.
- El shim escribe un marcador de lanzamiento **antes** de la sonda. Cada marcador exige un reporte legible, con `rc=0` y stderr vacío.
- Los ensayos hicieron abortar el lote por tres causas reales, ya corregidas:
  - `healthz` 502 antes de que el daemon escuche (ahora solo se acepta 502 en esa ruta);
  - sockets unix contados como archivos omitidos;
  - `ENOBUFS` con varias sondas escaneando a la vez (reintento acotado; si persiste, error).

### P1-3: reviewDepth efectivo

- Se exige requested = light, source = explicit **y** effective = light en el checkpoint durable `review_depth_decision`.
- Si falta el checkpoint o hay otro valor: ABORT.
- Las lecturas de la DB de la repetición pasan de `immutable=1` a `mode=ro`, que respeta el WAL. Los tests lo detectaron: con `immutable`, una lectura podía ser anterior al último commit.

### P2 cerrados

- Aserciones fatales:
  - commit del rebuild igual a EXPECTED_FIXTURE_SHA;
  - `explorationSettled`;
  - teardown de tmux (el servidor debe dejar de responder);
  - `contextRouter` = off;
  - cualquier excepción en post-checks = ABORTED;
  - el lote nunca termina `COMPLETE` si hay menos muestras VALID que las planeadas, si cambió producción o si hubo una excepción.
- **Capacidad:** se eliminó la rama que permitía que un checkpoint de capacidad ocultara un fallo posterior. Ahora hace falta que todos los intentos fallidos sean de capacidad; un checkpoint `review_capacity_retry` por sí solo nunca invalida una muestra. Además, como regla de análisis se reportan los eventos por brazo y hay un análisis de sensibilidad que cuenta los inválidos por capacidad como fallos.
- **Cadena del fence:** `TestReviewerLaunchFenceSurvivesTheProductionDecoratorChain` construye la cadena con la función de composición real (`instrumentAgentDispatch`, los tres decoradores activos) y el launcher real. Las mutaciones F1 y F2 (un decorador descarta el fence) las detecta.
- **G4:** `TestReviewReleaseBySupersededHolderSpendsNothingOnTheSuccessorsBudget` detecta la mutación.
- **Guarda P9-C10 en el laboratorio:** una repetición con confirmaciones de dispatch de worker duplicadas se aborta.

### TestP9Crash_C10: NEEDS_ATTENTION del producto (fuera del alcance de 3D)

Investigación completa: `~/.ao/scratch/frente3/reviews/3d-preflight/cycle4/p9c10-investigation.md`.

- **No** es solo una topología no soportada. `adoptLiveLaunch` no tiene un claim atómico de "una confirmación por launch".
- Con **un solo daemon**, dos `ContinueRun` concurrentes sobre un launch "unconfirmed" pueden confirmar dos veces. Medido: 5 de 400 rondas.
- Condición de disparo: el daemon murió entre crear el runtime del worker y confirmarlo, y al reiniciar el runtime sigue vivo.
- Consecuencia: filas duplicadas en el ledger. No hay doble worker.
- No se corrige en este ciclo porque sería un cambio productivo material.
- En 3D no ocurre (ningún daemon del laboratorio se cae a mitad de un lanzamiento), y la guarda del laboratorio aborta la repetición si ocurriera.

### Mutaciones del harness (`mutate_harness.py`)

`test_harness.py` tiene 32 tests. Se reintroducen 13 defectos (H1–H13), uno por vez, en una copia temporal de `run.py`, y el control pasa. Todos los mutantes mueren. Entre ellos:

- H1: se ignoran los errores de canal;
- H2: no se exige el `reviewDepth` efectivo;
- H3: un checkpoint de capacidad oculta el resultado;
- H4: `COMPLETE` con muestras faltantes;
- H5: una excepción cuenta como muestra;
- H8: no se exige comprobar el `~/.claude.json` real;
- H10: un marcador sin reporte;
- H13: se aceptan confirmaciones duplicadas.

### Ciclo 4 — preregistro del mini-E2E (publicado ANTES de ejecutarlo)

Archivo privado: `~/.ao/scratch/frente3/3d-private/prereg-c4minie2e-20260926T100425.json`, sha256 `a5a40e8fb8b5a8dc31009fa9d6df1459dd109d9f731110673e155c719876b0f8`

| Valor | Preregistrado |
|---|---|
| PRODUCT_HEAD | `35e3ad6e7` (binario `vcs.revision=35e3ad6e71e3`, `vcs.modified=false`) |
| EXPECTED_AO_BINARY_SHA | `2f72ddf8974fb0bbdb0afdfa366617a1f90870c6fae47d5952d235c89fd0354c` (firmado ad-hoc `-o restrict`) |
| EXPECTED_FIXTURE_SHA | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` |
| EXPECTED_HARNESS_SHA | `0e5e7fcb7d93bcf4fcf9a7e58132b82bf78434fe0cf33b7ee9f9be76164f6f2b` |
| Fingerprint FROZEN del proveedor (digest) | `1ca42cbcf28cc5d61a2c18705973c1e08fe796b210a5a7ab535a4909b4b5a1c5` |
| Snapshot del proveedor | `.claude/settings.json`=a1bd71fb14f3…, `.codex/config.toml`=da1f8924e536…, `.codex/rules`=26ec784c13a2…, `.gitconfig`=7c6485a8ca7a… |
| Plantilla de Claude (estado del servidor) | flags `cbb528f735e9…`, experimentos `176deca107b6…`, skills sincronizadas `140478f4406a…` |
| Proveedor/modelo | worker {'effortLevel': 'high', 'harness': 'claude-code', 'model': 'opus[1m]'}; reviewer {'harness': 'codex', 'model': 'gpt-5.6-sol', 'reasoningEffort': 'medium'} |
| reviewDepth | {'effective': 'light', 'requested': 'light', 'source': 'explicit'} |
| memoryMode / router / external | {'assisted': 'assisted (AO_MEMORY_MODE=assisted)', 'off': 'off (AO_MEMORY_MODE unset)'} / off / off (AO_MEMORY_EXTERNAL=off) |
| planSha (semilla y plan privados) | `389555efc1b4b2652821a685c9202d6659ca6ab58505dc8e6dff1b3152f20af2`, N=2 por brazo |

Políticas preregistradas (texto completo en el archivo): capacity, isolation, probe, failure.


### Ciclo 4 — mini-E2E (`3d-private/batch-20260926T100457`, binario `35e3ad6e7`, preregistro `a5a40e8f…`)

| Muestra | Brazo | Estado | Review | reviewDepth pedido / efectivo | `contextSources` | Manifests | Sondas (marcadores / reportes) | Capacidad | Colisión / carrera / confirmación duplicada | `~/.claude.json` real |
|---|---|---|---|---|---|---|---|---|---|---|
| 1 | assisted | completed | complete/approved (codex) | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | 0 | 0/0/0 | 0 entradas |
| 2 | off | completed | complete/approved (codex) | light / light | off/off/off | — | 11/11 PASS | 0 | 0/0/0 | 0 entradas |
| 3 | off | completed | complete/approved (codex) | light / light | off/off/off | — | 10/10 PASS | 0 | 0/0/0 | 0 entradas |
| 4 | assisted | completed | complete/approved (codex) | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | 0 | 0/0/0 | 0 entradas |

- Veredicto del lote: **COMPLETE**. 4/4 VALID, sin intentos inválidos ni reemplazos.
- En todas: rebuild = `b09478c0`, exploración estable, teardown OK, estado del servidor de Claude igual a la plantilla, fingerprint FROZEN sin deriva.
- **Diferencial de cegamiento:** 0 diferencias consistentes con el brazo, ni en el worker ni en el reviewer.
  - Todo lo que varía lo hace también dentro de un mismo brazo: IDs y tokens por repetición, reflogs de git, transcripts y sesiones de la propia repetición, telemetría local de Go.
  - El gateway dejó pasar las mismas rutas en ambos brazos.
- **Nota:** el reviewer puede leer el transcript del worker de **su misma repetición**, que en ASSISTED contiene el pack. Es contenido del tratamiento dentro de la misma repetición, no una etiqueta; el reviewer recibe además su propio pack.
- Producción: goose 174, mismo mtime y tamaño antes y después.

### Ciclo 4 — gates (HEAD `6de7684d1`; el código productivo es igual a `35e3ad6e7`)

Todos se corrieron con `GOTMPDIR` y `TMPDIR` **fuera de `~/.ao`** desde el principio. Logs en `~/.ao/scratch/frente3/reviews/3d-preflight/cycle4/gates/`.

| Gate | Resultado |
|---|---|
| build | PASS |
| vet `./...` | PASS |
| gofmt (archivos cambiados vs `e2e9c741d`) | 0 archivos |
| lint delta vs `e2e9c741d` (`./...`) | 0 issues |
| race: daemon, tmux, projectmemory, contextrouter | PASS |
| race: `./internal/workflow/...` completo | PASS |
| short suite completa (2 corridas) | Solo fallan dos tests preexistentes e intermitentes; el resto pasa |

Los dos tests que fallan en la short suite:

- **`TestRealTmux_Large*PromptArrivesAsOneBracketedPaste`**: test contra tmux real, sensible a la carga.
  - Aislado: 20/20 en HEAD y 20/20 en la base.
  - Con el paquete completo 5 veces: HEAD 5/5, **base 1 fallo de 5** (el test hermano).
  - Es preexistente y esta rama no lo introduce: el paquete tmux no cambió en este ciclo.
- **`TestP9Crash_C10`**: la carrera de producto documentada arriba. También falla en la base.

### Ciclo 4 — revisión de Codex: SIN VEREDICTO (límite de uso)

Codex agotó su límite de uso a mitad de la auditoría y no emitió veredicto. El próximo intento es posible el 2026-09-28 a las 10:42; el prompt está en `~/.ao/scratch/frente3/reviews/3d-preflight/cycle4/codex/`. El mismo límite afecta al reviewer Codex del laboratorio, así que tampoco se puede correr un mini-E2E nuevo hasta entonces.

**Hallazgos parciales de Codex, ya cerrados en el harness** (sin validar todavía con una corrida real):

1. `providerState` se aceptaba aunque omitiera la mayoría de los almacenes. Ahora cada almacén de `PROVIDER_STORES` debe aparecer; un test asegura que la lista de la sonda y la del runner son idénticas; la mutación H15 se detecta.
2. Un resultado LAN vacío se aceptaba. Ahora hace falta al menos una dirección escaneada; la mutación H16 se detecta.
3. Cada `CODEX_HOME` nuevo descargaba catálogos remotos (`cache/remote_plugin_catalog`, `cache/codex_apps_tools`, ~27 MB) con contenido distinto en las 4 repeticiones. La sonda solo registraba el tamaño de los archivos grandes.
   - Ahora se hashean por contenido.
   - El `config.toml` del laboratorio desactiva las funciones `apps` y `plugins` de Codex, igual en ambos brazos.
   - **Sin validar:** requiere una corrida real de Codex.

Los otros hallazgos parciales confirmaron lo afirmado:

- el código productivo no cambió desde `35e3ad6e7`;
- el agujero de `~/.claude.json` del ciclo 3 está cerrado;
- 34/34 tests y 14/14 mutantes, verificado de forma independiente;
- los tests de fencing del daemon pasan.

La parte de fencing de workflow quedó en UNKNOWN: el disco se llenó.

**Incidente de disco:** el volumen de datos llegó al 100% (483 MB libres). Se borraron únicamente cachés de compilación de Go creadas por las revisiones y gates de estos ciclos (13.5 GB); ninguna evidencia. Producción verificada después, en solo lectura: goose 174, `integrity_check` ok, sin violaciones de FK, mismo mtime y tamaño.

**Estado:** el mini-E2E `batch-20260926T100457` (4/4 VALID) se ejecutó con el harness **anterior** a estas tres correcciones, así que no vale como evidencia final. Hace falta:

1. un preregistro nuevo;
2. un mini-E2E nuevo;
3. la revisión de Codex.

**PRECONDITION_3D = NO-GO (no demostrado).**

## Cierre de PRECONDITION_3D (2026-09-28): preregistro FINAL (publicado ANTES del mini-E2E)

El preregistro `a5a40e8f…` del 26-sep **no cuenta** como evidencia final.

Archivo privado: `~/.ao/scratch/frente3/3d-private/prereg-c4final-20260928T104824.json`, sha256 `c730822cd0e82c18ffa92754c177835a0377e7a5f27cedc25791af05cc024026`

| Valor | Preregistrado |
|---|---|
| PRODUCT_HEAD | `35e3ad6e7` (binario `vcs.modified=false`) |
| EXPECTED_AO_BINARY_SHA | `2f72ddf8974fb0bbdb0afdfa366617a1f90870c6fae47d5952d235c89fd0354c` |
| EXPECTED_FIXTURE_SHA | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` |
| EXPECTED_HARNESS_SHA | `f9805de00b60df850ef9956eb03819895dfa3cacfe24fddbd6dc46f42ab5b88f` (incluye las correcciones de providerState completo, LAN, apps/plugins de Codex desactivados y hash de archivos grandes) |
| Proveedor | 2.1.283 (Claude Code); codex-cli 0.157.1; digest FROZEN `6b4e4f9c00ac985321932a6c99a125c9de9dc8198445258f49443c0577333303` |
| Snapshot | `.claude/settings.json`=a1bd71fb14f3…, `.codex/config.toml`=52ff54cd0474…, `.codex/rules`=26ec784c13a2…, `.gitconfig`=7c6485a8ca7a… |
| Plantilla de Claude | flags `1661f7e3d97e…`, experimentos `176deca107b6…` (3 corridas de inicialización) |
| Modelos | worker opus[1m] (high); reviewer gpt-5.6-sol (medium) |
| reviewDepth / memoryMode / router / external | light pedido = efectivo / off-assisted / off / off |
| planSha | `8906e404919dff83ed60ccf3c111128651b78e2f9c50608b3b0d255d53a1bb05`, N=2 por brazo (semilla y plan privados) |

Comprobaciones previas del 28-sep:

- disco: 11 GB libres;
- producción: goose 174, `integrity_check` ok, FK 0, mismo mtime y tamaño;
- sin daemons experimentales ni servidores tmux de laboratorio vivos;
- worktree limpio en `bdd6b6af4`, sin cambios en el backend desde `35e3ad6e7`;
- harness: 35 tests OK y 15 de 15 mutantes detectados;
- Codex disponible.


### Preregistro `c4final` (`c730822c…`) ANULADO: defecto del harness detectado en su lote

El mini-E2E `batch-20260928T104903` se detuvo tras la primera muestra.

**Qué pasó:**

- El `~/.codex/config.toml` del operador ganó una tabla `[features]` después del 26-sep.
- El harness **añadía** otra `[features]` a la copia del laboratorio. Eso produce un TOML inválido (`duplicate key`), y el reviewer Codex nunca arrancó: no hubo sesión ni veredicto, y la run quedó en timeout.
- La regla preregistrada contaba ese timeout como muestra VALID: un fallo del instrumento que entraba al dataset.
- Afectaba igual a los dos brazos (no era una fuga de cegamiento) y era corregible solo en el harness.

**Corrección** (solo harness, fail-closed):

1. `codex_lab_config` fusiona las funciones en la tabla `[features]` existente y valida el resultado con `tomllib`. La config efectiva se digiere en el preregistro.
2. Precondición: el propio Codex debe cargar la config de la repetición (`codex features list` con ese `CODEX_HOME`) y reportar `apps` y `plugins` desactivados. Se valida también al preregistrar.
3. Regla de instrumento: cada agente lanzado debe dejar un transcript (Claude en su home, Codex con un rollout en `CODEX_HOME`); si no, se ABORTA. Un timeout cuenta como resultado solo si todos los agentes lanzados corrieron.

Tests: 41 OK. Mutantes H1–H17 y H18 (añadir una segunda `[features]` en `labcore`) detectados. El lote anulado se conserva como evidencia y no cuenta.

### Preregistro FINAL `c4final2` (publicado ANTES del mini-E2E)

Archivo privado: `~/.ao/scratch/frente3/3d-private/prereg-c4final2-20260928T112405.json`, sha256 `0eb2ad76785d38af7ab6768f822de57a4288c05d4496c37ec949cf4ef54a3d8a`

| Valor | Preregistrado |
|---|---|
| PRODUCT_HEAD / binario | `35e3ad6e7` / `2f72ddf8974fb0bbdb0afdfa366617a1f90870c6fae47d5952d235c89fd0354c` |
| Fixture | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` |
| EXPECTED_HARNESS_SHA | `9f6205e55d97e78efb890829bf6126cdc49f6f324abc93f80e4df750d191b91a` |
| Config de Codex del laboratorio (validada por Codex) | `23e3bb40fa9dbc2b81ff09aff5e57a2683e9441ff171fc5a583cb4b0e528b4cb` |
| Proveedor | 2.1.283 (Claude Code); codex-cli 0.157.1; digest FROZEN `6b4e4f9c00ac985321932a6c99a125c9de9dc8198445258f49443c0577333303` |
| Plantilla de Claude | flags `4f80cf5b0de4…`, experimentos `176deca107b6…` |
| Modelos | worker opus[1m] (high); reviewer gpt-5.6-sol (medium) |
| planSha | `a793452038b222c9ae6ee6dadd047fc2b0b22257349bc6b9a980df8b9958af6e`, N=2 por brazo |


### Preregistro `c4final2` (`0eb2ad76…`) ANULADO: la sonda falló de forma cerrada ante DNS transitorio

- En `batch-20260928T112435` (primera repetición, ASSISTED), la run completó y el review quedó aprobado.
- El reviewer Codex **sí corrió**: rollouts en `CODEX_HOME`, veredicto por el gateway. La corrección anterior funciona, y no aparecieron catálogos de Codex.
- Pero una sonda devolvió `lan errored: gaierror(8)`: la resolución DNS/mDNS del hostname falló en ese momento. El runner abortó, como corresponde.

Corrección (solo harness): la sonda obtiene las IPv4 de las interfaces (`getifaddrs`), sin resolver el hostname. Cero direcciones sigue siendo un fallo. Verificado dentro del sandbox: 4 direcciones. Tests 41 OK y mutantes detectados. El lote se conserva como evidencia y no cuenta.

### Preregistro FINAL `c4final3` (publicado ANTES del mini-E2E)

Archivo privado: `~/.ao/scratch/frente3/3d-private/prereg-c4final3-20260928T112951.json`, sha256 `925fa7f9b6c65e557281c19675a0356dfb9d123ff0e42f08f0ced10f81076c5c`

| Valor | Preregistrado |
|---|---|
| PRODUCT_HEAD / binario | `35e3ad6e7` / `2f72ddf8974fb0bbdb0afdfa366617a1f90870c6fae47d5952d235c89fd0354c` |
| Fixture | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` |
| EXPECTED_HARNESS_SHA | `d90cee72843ca0b0ab0ab982180c22c7306bb37e218aee20ae962f026e949b94` |
| Config de Codex del laboratorio | `23e3bb40fa9dbc2b81ff09aff5e57a2683e9441ff171fc5a583cb4b0e528b4cb` |
| Proveedor | 2.1.283 (Claude Code); codex-cli 0.157.1; digest FROZEN `6b4e4f9c00ac985321932a6c99a125c9de9dc8198445258f49443c0577333303` |
| Plantilla de Claude | flags `0ea146006ac2…`, experimentos `176deca107b6…` |
| planSha | `389555efc1b4b2652821a685c9202d6659ca6ab58505dc8e6dff1b3152f20af2`, N=2 por brazo |


### Mini-E2E FINAL (`3d-private/batch-20260928T113032`, preregistro `c4final3` `925fa7f9…`)

| Muestra | Brazo | Estado | Review | reviewDepth pedido / efectivo | `contextSources` | Manifests | Sondas | Rollouts de Codex / catálogos | Capacidad | Colisión / carrera / confirmación duplicada | `~/.claude.json` real |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | assisted | completed | complete/approved | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | 2 / ninguno | 0 | 0/0/0 | 0 entradas |
| 2 | off | completed | complete/approved | light / light | off/off/off | — | 10/10 PASS | 2 / ninguno | 0 | 0/0/0 | 0 entradas |
| 3 | off | completed | complete/approved | light / light | off/off/off | — | 9/9 PASS | 2 / ninguno | 0 | 0/0/0 | 0 entradas |
| 4 | assisted | completed | complete/approved | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | 2 / ninguno | 0 | 0/0/0 | 0 entradas |

- Veredicto del lote: **COMPLETE**. 4/4 VALID, sin reemplazos.
- En todas: rebuild = `b09478c0`, exploración estable, teardown OK, estado del servidor de Claude igual a la plantilla, sin deriva del fingerprint FROZEN, todos los agentes lanzados corrieron.
- **Diferencial de cegamiento:** 0 diferencias consistentes con el brazo, ni en el worker ni en el reviewer. Lo que varía (IDs, tokens, temporales de Codex, reflogs) también varía dentro de cada brazo. El gateway dejó pasar las mismas rutas en ambos brazos.
- Producción: goose 174, mismo mtime y tamaño antes y después.

### Gates FINALES (HEAD `c1d48e6ea`; backend idéntico a `35e3ad6e7`; temporales fuera de `~/.ao`; cachés de Go borradas al terminar)

| Gate | Resultado |
|---|---|
| build / vet / gofmt / lint delta | PASS / PASS / 0 archivos / 0 issues |
| race: daemon, tmux, projectmemory, contextrouter | PASS |
| race: `./internal/workflow/...` completo | PASS |
| harness (`test_harness.py`, 41 tests) y mutaciones (H1–H18) | PASS, todos los mutantes detectados |
| short suite completa | un solo fallo: `TestGeneratedProtocolMatchesTheInstalledProvider` |

Ese fallo es **ambiental** y también ocurre en la base `e2e9c741d`:

- El test compara el protocolo generado con el binario `codex` instalado, que el host actualizó de 0.153.4 a 0.157.1: 28 métodos nuevos y 1 retirado.
- Corregirlo implica regenerar código del producto, fuera de este cierre.
- El laboratorio no usa ese adaptador: el reviewer va por el adaptador TUI de Codex.

Los dos tests intermitentes conocidos (pegado de tmux y `TestP9Crash_C10`) no fallaron en esta corrida.

### Revisión FINAL de Codex (cierre, 2026-09-28): NO-GO (P0 0 / P1 2 / P2 3 / P3 2)

Revisión completa: `~/.ao/scratch/frente3/reviews/3d-preflight/cycle4/codex/final/review.md`.

**Confirmado por Codex:**

- el cegamiento por los canales ordinarios;
- el aislamiento del estado de Claude por archivos;
- la sonda fail-closed (41 tests; mutantes detectados);
- el reviewDepth efectivo (mutación light→full rechazada);
- el fencing de generación (F1, F2 y G4 detectados);
- la reproducibilidad del preregistro, incluida la cronología local, y que anular `c4final` y `c4final2` es metodológicamente limpio;
- la comparabilidad en los datos: catálogos de Codex ausentes, plantilla idéntica, indexación idéntica;
- producción intacta.

**P1 abiertos** (corregibles solo en el harness, **no aplicados**: la autorización exige STOP ante un P1 nuevo):

1. **Escape por sockets Unix.** `agent.sb` es allow-by-default y no niega sockets Unix externos del mismo usuario:
   - el socket Docker de Colima (vivo: `~/.colima/default/docker.sock`), con `docker` en el PATH del agente y `docker run` permitido por la política copiada de Claude;
   - `~/.codex/ipc/ipc.sock`;
   - `/private/tmp/cc-socks/*.sock`.

   Un daemon Docker externo podría montar la DB de la repetición o los logs privados, que contienen `memoryMode` y `mode=assisted`: una etiqueta explícita alcanzable a través de un intermediario fuera de Seatbelt. Codex no ejecutó el montaje, para no mutar estado externo.
2. **Clasificador de capacidad.** Un intento `rate_limited` fallido más otro intento sin terminar se clasifica como `INVALID_PROVIDER_CAPACITY` aunque la run termine en timeout, `needs_attention` o un fallo ajeno. Es una vía de selección dependiente del resultado.

**P2:**

- Esquema de la sonda: se aceptan reportes sin `explicitLabelFound` ni `labelHits`, y una sonda sin archivo `.err`.
- P9-C10: deuda del producto, reproducida; el laboratorio aborta la repetición si aparece.
- Test de protocolo de Codex 0.157.1: ambiental; también falla en la base.

**P3:**

- La publicación del preregistro solo está anclada localmente.
- Documentación imprecisa del conteo de mutantes: 16 en el script más H18 aparte; H8 ya no existe.

**Correcciones propuestas** (solo harness; requieren autorización):

1. Negar por defecto la conexión a sockets Unix y permitir solo los necesarios (p. ej. mDNSResponder para DNS). Negar la ejecución de `docker`, `colima` y similares. La sonda debe probar esas familias de sockets.
2. Clasificar un intento como inválido por capacidad solo si todos sus intentos terminaron y la causa terminal de la run es de capacidad.
3. Exigir el esquema completo de la sonda y todos sus artefactos.
4. Preregistro, mini-E2E y revisión de Codex nuevos.

**PRECONDITION_3D = NO-GO. STOP + NEEDS_ATTENTION.**

## Ciclo final de cierre (autorizado 2026-09-28): correcciones del NO-GO

Todo es solo harness; el producto no cambia (código `35e3ad6e7`).

**P1: escape por sockets Unix.**

- `agent.sb` pasa a **negar por defecto** todo `connect(2)` a sockets Unix, con esta allowlist:
  - `/private/var/run/mDNSResponder`, necesario para DNS (sin él, HTTPS falla, verificado);
  - los sockets dentro del árbol propio de la repetición, donde ahora vive el `TMPDIR` del agente; el data dir de AO (socket supervisor) sigue negado.
- Así quedan fuera Docker/Colima, el IPC de Codex, `cc-socks`, VS Code, `ssh-askpass`, tmux y cualquier socket equivalente.
- Se niega ejecutar `docker`, `colima`, `lima`, `limactl` y `podman`, además de Docker.app.
- Nuevo canal obligatorio de la sonda, `unixSockets`:
  - un **canario vivo** (un listener Unix que el harness abre fuera del árbol) debe dar DENIED;
  - todos los sockets descubiertos (`/private/tmp`, `/private/var/run`, el temp dir del usuario, `~/.colima`, `.lima`, `.docker`, `.ssh`) y las rutas conocidas (sockets Docker, `~/.codex/ipc`) deben dar DENIED o no existir.

  En el ensayo: 136 sockets descubiertos, todos inaccesibles; canario DENIED; DNS y HTTPS OK.
- La sonda también intenta ejecutar `docker`, `colima` y `limactl`; debe resultar denegado.

**P1: clasificador de capacidad.** Una muestra es inválida por capacidad solo si se cumple todo lo siguiente:

- la run terminó en `failed` o `needs_attention` (un timeout nunca cuenta);
- todos los intentos terminaron y tienen clase;
- todos los intentos fallidos son de capacidad;
- el intento **determinante** (el último) falló por capacidad;
- no queda ninguna review abierta.

Cualquier otro caso es una muestra VALID con su resultado: timeout, fallo ajeno, intento incompleto, fallo sin clase o review abierta. La reproducción de Codex (un `rate_limited` más un intento sin terminar) ahora da VALID.

**P2: esquema de la sonda.**

- Campos obligatorios, con tipo: `role`, `pid`, `cwd`, `checks`, `labelHits`, `explicitLabelFound` (debe coincidir con `labelHits`) y `seconds`.
- Los 10 canales obligatorios.
- Archivos `.json`, `.rc` y `.err` obligatorios: si falta alguno, es inválido y se aborta.

**Tests y mutaciones:**

- `test_harness.py`: 49 tests OK.
- Mutantes H1–H28 más H18 de `labcore`: todos detectados. Los nuevos:
  - H19: el canario no se exige DENIED;
  - H20: se acepta un socket alcanzable;
  - H21: no se exige el esquema;
  - H22: se acepta un `.err` faltante;
  - H23 a H26: el clasificador ignora intentos sin terminar, el intento determinante, el estado timeout o las reviews abiertas;
  - H27: se permite ejecutar `docker`;
  - H28: se aceptan campos de etiqueta inconsistentes.

**P9-C10:** sin cambios. Sigue como deuda del producto y el laboratorio aborta la repetición si aparece.

**Ensayo** (`c5smoke1`, no es evidencia): COMPLETE, 2/2 VALID. Claude y Codex funcionan con los sockets Unix negados por defecto.

### Preregistro FINAL `c5final` (publicado ANTES del mini-E2E)

Archivo privado: `~/.ao/scratch/frente3/3d-private/prereg-c5final-20260928T132517.json`, sha256 `47a3cfdcdbf9446c803c2d84725c2d015e83d465726342876dc922ec729327f8`

| Valor | Preregistrado |
|---|---|
| PRODUCT_HEAD / binario | `35e3ad6e7` / `2f72ddf8974fb0bbdb0afdfa366617a1f90870c6fae47d5952d235c89fd0354c` |
| Fixture | `b09478c0afd7e1cf1ea4af1b40d30ae4a2c8d388` |
| EXPECTED_HARNESS_SHA | `f02eeb12f84f421dc7ebeaf9d84d360d589a267caae3370a1a276f5630354bba` |
| Config de Codex del laboratorio | `23e3bb40fa9dbc2b81ff09aff5e57a2683e9441ff171fc5a583cb4b0e528b4cb` |
| Proveedor | 2.1.284 (Claude Code); codex-cli 0.157.1; digest FROZEN `e25f28a0f09fd350d316ee6d02f1583a50f8b7bca60ff098a03556330aa94587` |
| Plantilla de Claude | flags `46f15044730e…`, experimentos `176deca107b6…` |
| planSha | `389555efc1b4b2652821a685c9202d6659ca6ab58505dc8e6dff1b3152f20af2`, N=2 por brazo |


### Mini-E2E FINAL (`3d-private/batch-20260928T132546`, preregistro `c5final` `47a3cfdc…`)

| Muestra | Brazo | Estado | Review | reviewDepth pedido / efectivo | `contextSources` | Manifests | Sondas | Canario Unix / sockets revisados | Rollouts de Codex / catálogos | Capacidad (eventos / sin terminar / reviews abiertas) | Colisión / carrera / duplicada | `~/.claude.json` real |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | assisted | completed | approved | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | DENIED / 137 | 2 / ninguno | 0 / 0 / 0 | 0/0/0 | 0 entradas |
| 2 | off | completed | approved | light / light | off/off/off | — | 10/10 PASS | DENIED / 137 | 2 / ninguno | 0 / 0 / 0 | 0/0/0 | 0 entradas |
| 3 | off | completed | approved | light / light | off/off/off | — | 10/10 PASS | DENIED / 137 | 2 / ninguno | 0 / 0 / 0 | 0/0/0 | 0 entradas |
| 4 | assisted | completed | approved | light / light | assisted/off/off | worker, reviewer | 10/10 PASS | DENIED / 137 | 2 / ninguno | 0 / 0 / 0 | 0/0/0 | 0 entradas |

- Veredicto del lote: **COMPLETE**. 4/4 VALID, sin reemplazos. Rebuild = `b09478c0` en todas.
- **Producción:** goose 174, mismo mtime y tamaño antes y después.
- **Diferencial de cegamiento:** worker con 0 diferencias consistentes con el brazo; reviewer con **1**.
  - Es un objeto de la caché de compilación de Go (`home/Library/Caches/go-build/ea/…-d`) cuyo contenido es la salida cacheada de `go test`: `ok example.com/ledgerlite/internal/ledger 0.228s`.
  - Las cuatro repeticiones tienen la entrada equivalente; solo cambia la duración medida (0.223–0.409 s). Las dos ASSISTED coincidieron en 0.228 s y por eso comparten el hash de contenido.
  - Es ruido de tiempo del trabajo del worker. **No es una etiqueta de brazo** ni metadato de control.
- El gateway dejó pasar las mismas rutas en ambos brazos.

### Gates FINALES (HEAD `d1de8e5a4`; backend idéntico a `35e3ad6e7`; temporales fuera de `~/.ao`; cachés de Go borradas)

| Gate | Resultado |
|---|---|
| build / vet / gofmt / lint delta | PASS / PASS / 0 archivos / 0 issues |
| race: daemon, tmux, projectmemory, contextrouter | PASS |
| race: `./internal/workflow/...` completo | PASS |
| harness: 49 tests; mutantes H1–H28 más H18 | PASS, todos detectados |
| short suite | solo dos fallos, ambos preexistentes y confirmados en la base `e2e9c741d` |

Los dos fallos de la short suite:

- `TestGeneratedProtocolMatchesTheInstalledProvider`: ambiental, por el Codex 0.157.1 instalado. No está en la ruta de 3D.
- `TestRealTmux_LargeFixPromptArrivesAsOneBracketedPaste`: intermitente con carga; en la base falló 1 de 5 corridas del paquete completo.

### Revisión de Codex del ciclo final: SIN VEREDICTO (límite de uso hasta las 15:48) + P1 nuevo confirmado → STOP

Codex agotó otra vez su límite de uso a mitad de la auditoría (`cycle5/codex/review.jsonl`).

**Mensajes parciales:**

- los dos P1 anteriores quedan cerrados: la reproducción de capacidad (`rate_limited` más un intento sin terminar) ahora da VALID, y la mutación light→full se detecta;
- 49/49 tests;
- lote 4/4 con estado de Claude congelado idéntico.

Pero Codex abrió un **hallazgo nuevo, que verifiqué en los transcripts de la repetición 1**:

- La sesión de Claude del laboratorio carga los **conectores de claude.ai de la cuenta** como herramientas disponibles: Claude Docs (create, read, update, delete, batch), Gmail (get_message, create_draft, labels…) y otros.
- No se usaron en este lote (el worker solo usó Bash), pero son **accesibles**.
- Son estado externo compartido, fuera del sandbox (van por el servicio de claude.ai, no por sockets locales), con escritura. Suponen un canal entre repeticiones y una vía posible a información del experimento.
- **P1** (contaminación/cegamiento). No estaba en el alcance autorizado de este ciclo.

**Corrección propuesta** (solo harness; requiere autorización):

1. Desactivar los servidores MCP de claude.ai en el `HOME` de proveedor del laboratorio (variable de entorno o ajuste en la copia de `settings.json`), igual en ambos brazos.
2. Añadir a la sonda y al runner una comprobación: cero herramientas `mcp__claude_ai_*` en la sesión inicial de cada agente; abortar si aparece alguna.
3. Revisar también los conectores o apps del lado de Codex.
4. Preregistro, mini-E2E y revisión de Codex nuevos.

**PRECONDITION_3D = NO-GO. STOP.**
