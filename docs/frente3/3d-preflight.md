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
