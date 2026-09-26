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
