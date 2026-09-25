# Frente 3 / 3B — Evidencia de regresión: falla en el baseline, pasa en 3B

Fecha: 2026-09-24. Esta es la evidencia de que los tests críticos de 3B
**reproducen** los defectos en el código anterior y **pasan** con 3B.

**Cómo se obtuvo.** Cada test se copió, sin cambios, a un worktree scratch
desmontable en el baseline `3030d85f2` y se ejecutó allí. El worktree se
eliminó después. En el test de canarios solo se quitaron las aserciones de
framing, porque usan `repoaccess`, que no existe en el baseline. Esta
evidencia es reproducible con el mismo procedimiento; no hace falta volver a
ejecutarla en cada integración.

Todos son **tests de regresión permanentes** y corren en `go test -short`.

## Tests permanentes y resultado en el baseline

| Test (paquete) | Defecto que prueba | Baseline `3030d85f2` | 3B |
|---|---|---|---|
| `TestPreFixCompositionLeftTheWorkerWithoutMemory` (daemon) | Worker sin memoria por el launcher sin decorar | reproduce la composición anterior dentro del propio test | PASS |
| `TestMemoryReachesWorkerWheneverItReachesPlannerOrReviewer` (daemon) | ídem | (el helper no existe en el baseline) | PASS |
| `TestEveryAgentDispatchSurfaceIsClassified` (daemon) | un launcher nuevo evita el context provider | n/a (guarda) | PASS |
| `TestDecoratorsAreComposedOnlyByInstrumentAgentDispatch` (daemon) | composición fuera del punto único | n/a (guarda) | PASS |
| `TestLinkedAgentWorktreeIsNeverIndexedIntoTheProject` (codegraph) | patrón MEDUSA: `.claude/worktrees/...` | **FAIL**: símbolos del worktree indexados, incluido un duplicado de `appEntry` | PASS |
| `TestNestedCheckoutIsNeverIndexedWithoutGit` (codegraph) | checkout anidado sin git | **FAIL**: `LeakedFromWorktree` indexado | PASS |
| `TestUntrackedFilesAreNotIndexedInAGitProject` (codegraph) | ficheros sin trackear | **FAIL**: `scratch.go#UntrackedScratch` indexado | PASS |
| `TestIncrementalUpdateNeverFollowsASymlinkedInstructionFile` (projectmemory) | symlink seguido en el incremental | **FAIL**: canario de un fichero de credenciales externo guardado como item `instruction` (`CLAUDE.md`, `AGENTS.md`) | PASS |
| `TestFullIndexNeverRecordsOrReadsSecretFiles` (projectmemory) | secreto abierto antes de decidir | **FAIL**: `.aws/credentials` abierto y registrado en el ledger | PASS |
| `TestMemoryIndexesOnlyTheProjectsTrackedFiles` (projectmemory) | ficheros no elegibles en memoria | **FAIL**: `NOTES.md` sin trackear en el ledger | PASS |
| `TestDriftInvalidatesAFactWhoseSourceBecameASymlink` (projectmemory) | drift sigue symlinks | **FAIL**: hecho aún válido con la fuente convertida en symlink | PASS |
| `TestSecretCanariesNeverLeaveTheBoundary` (projectmemory) | secretos en DB, WAL, items, packs, contexto del worker y logs | **FAIL**: 6 de 8 canarios en `ao.db-wal`, en items (`task_result`, `architecture`, `file_summary`, `build_test`, `instruction`), en los 4 packs de rol y en el `IssueContext` del worker; además *"a pack presents repository text as an instruction"* | PASS (0 de 8) |
| `TestPackKeepsRepositoryInjectionsInsideTheDataBlock`, `TestLegacyInstructionSummaryIsNotRepeatedInAOsVoice` (projectmemory) | contenido del repo presentado como instrucción | el baseline renderiza "Standing instructions … must follow" (constatado en el test de canarios) | PASS |
| `TestFrameUntrusted*`, `TestRedactCoversRepositoryConfigShapes`, `TestReadConfinedRefusesEverySymlinkShape`, `TestSecretPathPolicy`, `TestListEligible*` (repoaccess) | contratos unitarios de la frontera | n/a (paquete nuevo) | PASS |
| `TestProjectsSharingNamesNeverSeeEachOthersMemoryOrGraph`, `TestChangesInOneProjectNeverTouchTheOther`, `TestUnnamedOrUnknownProjectFailsClosed`, `TestArchivedProjectDoesNotLeakIntoTheOther` (projectmemory) | aislamiento entre proyectos | el aislamiento ya existía (no es regresión) | PASS |
| `TestScopedProvisionRefusesAMismatchedOrArchivedProject` (projectmemory) | `ProjectID(B) + RepoPath(A)` | una sonda previa al fix, sobre código cuyo `Provision` era idéntico al del baseline, mostró **A servido e indexado bajo B** | PASS |
| `TestDaemonMemoryProvisionerIsProjectScoped`, `TestDaemonMemoryIsOffByDefault` (daemon) | el daemon siempre aplica el alcance; memoria off por defecto | n/a | PASS |
| `TestFreshnessFollowsCommitsAndBranchSwitches`, `TestMemoryFromAnEarlierCommitIsLabelledStale`, `TestDirtyWorktreeIsDeclared`, `TestPartialIndexIsDeclared`, `TestCrashedRefreshIsNeverServedAsCurrent`, `TestParserFailureIsVisibleNotSilent`, `TestNoCompletedPassIsWithheld` (projectmemory) | freshness | el baseline no tiene aviso: tras un sync que agota el tiempo solo decía "derived at commit X" | PASS |
| `TestIncrementalUpdateEqualsAFullBuildOfTheSameTree` (codegraph), `TestMemoryIncrementalUpdateEqualsAFullIndex` (projectmemory) | nodos o aristas fantasma, duplicados, dependencias obsoletas | el incremental ya era correcto (no es regresión) | PASS |
| `TestRepositoryContentIsReadOnlyThroughRepoaccess` (projectmemory), `TestExtractIsOnlyCalledThroughRedaction` (codegraph) | guardas de código fuente | n/a | PASS |

## Sondas de integración post-merge (scratch, no permanentes)

Se ejecutaron sobre el ECC ya mergeado (`b7c12f0b4`) y después se eliminaron:

| Sonda | Resultado |
|---|---|
| DTOs de item y de conocimiento de la API con una fila **legacy sin redactar** (compose password, forge token, contraseña en URL en metadatos) | PASS: la salida muestra `POSTGRES_PASSWORD: [REDACTED]` y `API_TOKEN=[REDACTED]` |
| Contexto enrutado del worker con canarios AWS y Slack, un `<<<END …>>>` falsificado y "ignore previous instructions" | PASS: redactado, exactamente un BEGIN y un END, la inyección dentro del bloque |
| Errores de `ReadConfined` (secreto, symlink, traversal, inexistente) | PASS: ningún error contiene contenido del fichero |

## Flake de tmux (no relacionado con 3B)

`TestRealTmux_LargeFixPromptArrivesAsOneBracketedPaste` y
`TestRealTmux_LargeWorkerPromptArrivesAsOneBracketedPaste` fallan en la suite
completa: *"bracketed-paste markers missing (0 bytes)"*.

- Pasan 3 de 3 veces aislados, tanto en ECC tras el merge como en el baseline
  `3030d85f2`.
- En el baseline, con dos ejecuciones del paquete tmux en paralelo, fallan
  también otros tests de tmux real (`TestRuntimeIntegration`,
  `TestSessionFactsIntegration_EmptyPanePIDDoesNotFailTheObservation`).
- 3B no modificó ningún fichero de `internal/adapters/runtime/tmux`.

Clasificación: **flake ambiental existente**, por contención entre sesiones
reales de tmux. No se ha modificado código de tmux.
