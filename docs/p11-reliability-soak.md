# P11 — Reliability soak (24h → 48h → 72h)

*Estado: **harness implementado** en `feat/p11-reliability-soak` (base `9c80b9b2c`). Implementar el
harness **no** completa P11: P11 sólo se completa con tres etapas independientes (24h, 48h, 72h)
en PASS. Clasificación de plataforma: macOS arm64, un usuario, local-first, SQLite, runtime tmux,
arquitectura AO actual. Nada de esto se extiende a Windows/conpty, multiusuario, runners
distribuidos, Postgres, Hetzner ni P18.*

## 1. Objetivo

Demostrar con evidencia durable que AO sobrevive **tiempo + eventos**: reinicios del daemon,
crash controlado, recuperación de workers, cierre/reapertura de Electron, sleep/wake, reboot,
backups con AO operando, verificación de base, limpieza de runtimes y presión de memoria, sin
workers duplicados, pérdida silenciosa de datos, workflows atascados, ownership incorrecto,
mutación de estados terminales, corrupción de la base, runtimes huérfanos ni fallos silenciosos
de restore/recovery.

P11 valida el runtime, la orquestación, el almacenamiento y la recuperación de AO. **No** valida
la calidad de ningún modelo: no se declara "LLM reliability proven".

## 2. Auditoría: qué se reutiliza (código mínimo nuevo)

P11 no modifica AO. Todo el harness es un script stdlib (`scripts/p11/p11.py`) sobre superficies
que ya existen:

| Necesidad | Superficie existente reutilizada |
| --- | --- |
| Estado e identidad del daemon | `ao status --json` (P9: ambos run-files, `ready`/`stale`/`running_unverified`/…), `GET /healthz` (`pid`, `instanceId`, `installationId`, `dataDir`, `executablePath`) |
| Arranque/parada canónicos | `ao daemon` (el mismo subcomando que lanza la app empaquetada) y `ao stop` (`/shutdown` sólo a un daemon verificado; nunca señales) |
| Ownership P9 | `ao workflow recover ownership <id>` (`GET /workflows/{id}/recovery?ownership=1`), columnas `sessions.runtime_owner_token`/`runtime_instance_id`, stamps tmux `AO_SESSION_OWNER`/`AO_INSTALLATION_ID` |
| Backup / verify / restore | `ao backup create|verify|restore --json` (P10, contrato §J) |
| Integridad | `PRAGMA quick_check|integrity_check|foreign_key_check` en lectura `mode=ro` (`immutable=1` con AO parado) |
| Runtime | socket tmux `ao-<sha256(dataDir)[:12]>`, `list-sessions`, `show-environment -t '=<name>:'` |
| Sleep/wake/reboot | `kern.sleeptime`, `kern.waketime`, `kern.boottime`, `CLOCK_MONOTONIC` vs `CLOCK_UPTIME_RAW` (sin root) |
| Memoria/disco | `ps`, `vm_stat`, `sysctl vm.swapusage`, `kern.memorystatus_level`, `statvfs` |
| Recuperación de workers | E2E P9/P10 existentes y la matriz P9 en proceso (ver §4) |

### 2.1 Limitación que define el diseño

El daemon publicado **no puede ejecutar un ciclo work → review → Fix sin un LLM real**:

- el adaptador `fake` existe (`adapters/agent/fake`) pero **no está registrado** en el daemon
  (`registry.Constructors()` lo excluye y `TestHarnessedExcludesFakeHarness` lo exige);
  `domain.HarnessFake` no es un harness conocido y la validación de proyecto/spawn lo rechaza;
- no existe reviewer fake;
- una ejecución `task` omite el planner, pero su paso `work` lanza un agente real.

Registrar un harness fake sería cambiar el producto bajo prueba (y un test lo prohíbe); poner
stubs `claude`/`codex` en el `PATH` del daemon real sería frágil e inseguro sobre datos reales.
Por eso (instrucción §23) P11 tiene **dos pistas**:

- **Pista A — soak vivo** del daemon real sobre `~/.ao/data`, con el binario congelado:
  tiempo, reinicios, crash controlado, sleep/wake, reboot, backups online, base, runtime, memoria,
  inmutabilidad terminal y huérfanos. $0 de LLM: el harness no crea workflows.
- **Pista B — fixtures de ciclo de vida** congelados, ejecutados periódicamente por el monitor
  contra data dirs scratch propios (nunca `~/.ao`): daemon real + tmux real para adopción de
  workers a través de parada graceful y crash, rechazo de generación/instalación ajenas, runtime
  inalcanzable ≠ muerto, completion tardía ignorada, stale-generation writes, Fix liveness,
  inmutabilidad terminal y backup/restore a través del binario.

## 3. Invariantes (congelados en el manifest)

1. Ningún worker ni dispatch duplicado (una sesión viva por paso; un outbox `dispatched` por paso).
2. Ningún run terminal (`completed`/`failed`/`cancelled`) cambia de estado ni reabre pasos `running`.
3. Ninguna corrupción: `quick_check` en cada checkpoint, `integrity_check` + `foreign_key_check` en T0 y final.
4. Ningún runtime huérfano propiedad de AO (sellado con nuestra instalación y sin fila viva) que persista ≥2 escaneos.
5. Identidad del daemon probada: instalación, data dir, puerto y **binario congelado** sirviendo.
6. Todo backup P11 `VALID`; el restore scratch del último backup pasa.
7. Ninguna recuperación silenciosamente fallida (los fixtures pasan, sin SKIP).
8. Memoria dentro de umbrales congelados (§9).

## 4. Harness

```
scripts/p11/p11.py        harness (stdlib, /usr/bin/python3 3.9)
scripts/p11/test_p11.py   tests de la lógica de decisión
```

Artefactos congelados por etapa (fuera de `~/Downloads`, que launchd no puede leer sin TCC):

| Artefacto | Ruta | Procedencia |
| --- | --- | --- |
| Binario AO bajo prueba | `~/.ao/soak/p11/bin/ao-9c80b9b2c` | `go build -trimpath` en el worktree; `go version -m` = `vcs.revision=9c80b9b2c…`, `vcs.modified=false` |
| Fuente congelada | `~/.ao/soak/p11/src/9c80b9b2c/backend` | `git archive 9c80b9b2c backend` |
| Fixtures | `~/.ao/soak/p11/bin/tests/{tmuxe2e,daemone2e,backup-e2e,workflow}.test` | `go test -c -trimpath` sobre la fuente congelada |
| Harness por run | `runs/<run>/harness/p11.py` | copia en `init`; SHA en el manifest |

Fixtures (pista B), congelados con su SHA-256 en el manifest:

| Nombre | Binario / filtro | Gate | Qué prueba |
| --- | --- | --- | --- |
| `p9-tmux-e2e` | `tmuxe2e.test` | `AO_P9_REQUIRE_TMUX=1` | prueba de ownership contra tmux real, incarnación/instalación ajena, generación, runtimes muertos/stale, servidor inalcanzable = `unavailable` y sólo un socket ausente = `absent` |
| `p9-daemon-e2e` | `daemone2e.test` | `AO_P9_DAEMON_E2E=1` | daemon real: adopta un worker probado a través de stop graceful **y** crash; rechaza un runtime de otro launch; nunca señala un PID vivo con identidad distinta |
| `p9-lifecycle` | `workflow.test -test.run ^TestP9` | — | matriz C1–C10 (incl. recovery concurrente = un owner), completion tardía de gen-1 ignorada, report tardío rechazado, stale-generation writes, reloj saltando sobre worker vivo, espera ≤15 min por runtime ilegible, Fix liveness, readback, inmutabilidad terminal |
| `p10-daemon-e2e` | `backup-e2e.test` | `AO_P10_E2E=1` | backup online, restore rechazado con daemon vivo, PID no verificado, arranque sobre restore interrumpido, modos de identidad |

Un fixture pasa sólo si `rc=0`, ≥1 PASS, 0 FAIL, **0 SKIP** (un SKIP significa que el gate no se
activó: es UNKNOWN, nunca PASS) y no deja un servidor tmux vivo nuevo.

### 4.1 Monitor durable

- launchd `gui/<uid>/com.aoagents.p11-soak`, `RunAtLoad`, `KeepAlive{SuccessfulExit=false}`,
  `ProcessType=Background`, `AbandonProcessGroup=true`. El plist es el único archivo fuera de
  `~/.ao` (`~/Library/LaunchAgents/com.aoagents.p11-soak.plist`); `finalize` lo retira.
  No es estado de AO.
- Sobrevive a cerrar la terminal y Claude, y vuelve tras login después de un reboot.
- No necesita root, no lee secretos, no inspecciona otras apps (sólo `comm` y RSS del Electron de
  este checkout).
- Si muere, launchd lo relanza; el nuevo proceso registra `monitor_restart` con el hueco.
- El daemon se lanza en su propia sesión (`start_new_session`) con el entorno del login shell
  resuelto desde un entorno mínimo tipo launchd (como `daemonEnv()` de la app), sin heredar
  variables de la terminal o de Claude. Los valores del entorno nunca se registran.

### 4.2 Qué **no** hace

No reinicia workers, no borra sesiones tmux ni run-files, no restaura ni repara la base, no
reinicia el daemon por su cuenta ni lo mata por memoria, no hace `prune`, no toca repos del
usuario, Docker/Colima ni VS Code. Las únicas acciones son **eventos planificados y registrados**
(§6).

## 5. Evidencia

Raíz `~/.ao/soak/p11/` (0700; fuera del data dir activo, dentro de `~/.ao` por la regla del repo).

```
runs/<runId>/
  manifest.json        congelado en init (+ baseline T0); SHA en hashes.jsonl
  t0.json              reloj: wall, CLOCK_MONOTONIC, CLOCK_UPTIME_RAW, boot/sleep/wake del kernel
  events.jsonl         append-only, una línea JSON compacta por evento, fsync
  state.json           estado del monitor (último reloj, instancia, contadores)
  hashes.jsonl         SHA-256 de cada archivo de evidencia al escribirlo
  checkpoints/*.json   checkpoint completo
  db/baseline-{t0,final}.json
  backups/*-{create,verify}.json
  fixtures/*.log       salida de cada fixture
  scratch-restore/*.json
  incidents/INC-*.json
  process/daemon.log   stderr del daemon (copy-truncate a 256 MB)
  process/monitor.out
  final-report.json
```

Archivos 0600. Sin prompts, tokens, credenciales, contenido de comandos ni valores de entorno.
Los owner tokens se comparan en memoria y nunca se escriben.

Tipos de evento: `run_initialized`, `baseline`, `backup_created`, `backup_verified`,
`daemon_started`, `daemon_stopped`, `daemon_restart`, `daemon_crash_injected`, `daemon_crash`,
`stage_started`, `monitor_installed`, `monitor_started`, `monitor_restart`, `heartbeat`,
`checkpoint`, `sleep_wake_detected`, `reboot_detected`, `observed_clock_gap`, `clock_jump`,
`daemon_instance_changed`, `daemon_unavailable`, `daemon_available`, `fixture_run`,
`planned_event`, `planned_event_deferred`, `planned_event_coalesced`, `scratch_restore`,
`final_db_check`, `incident`, `incident_resolved`, `electron_close_reopen` (manual),
`stage_complete`.

## 6. Política de checkpoints y eventos

- **Heartbeat** cada 5 min (60 s en self-test): reloj, estado/identidad del daemon, RSS/CPU del
  daemon y del harness, memoria, swap, disco, tamaños de `ao.db`/`-wal`/`-shm`.
- **Checkpoint** (programado, manual, tras wake/reboot/crash): todo lo anterior + `quick_check`
  (≈5 s sobre la base real de 866 MiB), goose, conteos por estado, sesiones vivas, duplicados,
  inmutabilidad terminal, escaneo tmux con prueba de ownership, readback P9 de runs no
  terminales, huérfanos, `WARN`/`ERROR` del log del daemon desde el anterior (sólo claves
  `msg`), procesos hijos y Electron, SHA del binario congelado.
- **Backups online**: sólo en checkpoints marcados (§7); cada uno se verifica completo.
- **Reinicio graceful programado**: si hay trabajo activo (runs `pending/running/waiting` o
  sesiones vivas) se **difiere** hasta 3 h y, si nunca se libera, se registra
  `planned_event_skipped` (criterio UNKNOWN; el operador ejecuta `p11 daemon-restart`).
- Tras un sleep largo, los fixtures/backups vencidos del mismo tipo se **coalescen** en el último;
  los checkpoints nunca.

## 7. Etapas y eventos planificados (congelados)

| Etapa | Checkpoints | Backups online | Reinicios graceful (auto) | Fixtures (auto) | Manual |
| --- | --- | --- | --- | --- | --- |
| 24h | 0, 6, 12, 18, 24 | 12, 24 | T+5h | T+2, 10, 20 | ≥1 sleep ≥20 min; `daemon-crash` T+8–16h; `finalize` |
| 48h | 0, 12, 24, 36, 48 | 19.05 (justo tras reinicio = con actividad), 24, 48 | T+7, 19, 31 | T+3, 11, 21, 33, 43 | 2 noches; Electron cerrar/reabrir; `daemon-crash`; `finalize` |
| 72h | 0, 12, …, 72 | 24, 40.05 (con actividad), 48, 72 | T+10, 40 | T+4, 14, 26, 38, 50, 62 | 3 noches; reboot (si no hubo en 48h); Electron; `daemon-crash`; `finalize` |

Las tres etapas son **independientes**: un run nuevo, T0 nuevo, backup T0 nuevo. 24h dentro de
una corrida de 72h no cuenta como la etapa de 24h.

## 8. Procedimientos

### Sleep / wake
No usar `caffeinate`. Dejar dormir la Mac (tapa cerrada o reposo) ≥20 min. El monitor lo detecta
solo: `CLOCK_MONOTONIC − CLOCK_UPTIME_RAW` crece exactamente lo que el kernel estuvo dormido y
`kern.waketime` cambia. Un ciclo cuenta si ≥10 min dormido y hay un checkpoint `post-wake-N` sano.
Un hueco sin evidencia de sleep se registra `observed_clock_gap` (UNKNOWN), **nunca** como sleep.
La ausencia de heartbeat durante el sleep no se interpreta como muerte del daemon (semántica P9).

### Crash controlado del daemon
`p11 daemon-crash --confirm --restart`. Antes de `SIGKILL` exige: `ao status` = `ready`;
`/healthz` con el mismo PID e instancia; `dataDir` real; instalación = `<data>/installation_id`;
`ps -o command=` exactamente `<binario congelado> daemon`. Si algo no coincide: se niega y no
señala. Tras el kill: espera la desaparición del PID, arranca con el binario congelado y valida
estado `stale/stopped` tras el crash, instancia nueva, misma instalación, `quick_check`, conteos
terminales no decrecientes y un checkpoint `post-crash` sin fallos. Nunca se mata por nombre.

### Reinicio graceful
`ao stop` (canónico) → PID desaparecido, puerto liberado, estado `stopped/stale` → `ao daemon`
congelado → instancia nueva, misma instalación, base sana.

### Electron (48h/72h): `p11 electron-event`
Hecho verificado en código: la app de escritorio **no se adjunta** a un daemon que no lanzó ella
(`browserDaemonOwnershipDecision` devuelve `replace`); lo para por su `/shutdown` y lanza el suyo.
El procedimiento manual anterior (abrir la app con `AO_DAEMON_COMMAND` sobre el daemon vivo del soak)
**falló en la etapa 2** (§19): el daemon de reemplazo chocó con `daemon.lock` del anterior y la
etapa quedó sin daemon. Queda retirado. `AO_DAEMON_COMMAND` sólo elige el binario del reemplazo; no
evita el reemplazo.

El evento es un comando del harness, determinista y con el binario congelado como **único** comando
de daemon posible:

```bash
~/.ao/soak/p11/p11 electron-event            # --cycles 2 (cerrar/reabrir), --hold 20 s, o --interactive
```

1. Preflight (se niega sin tocar nada): checkout de la app (`electronCheckout` del manifest) en el
   `eccSha` congelado y con `frontend/` limpio, Electron instalado completo, ninguna app de ese
   checkout abierta, binario congelado con su SHA, daemon del soak `ready` con identidad probada.
2. Abre la ventana de mantenimiento y marca el evento en curso: los eventos programados (reinicio,
   checkpoint, fixtures) esperan a que termine.
3. `daemon_stop` del harness (PID desaparecido, puerto liberado).
4. Por ciclo: `npm run dev` con `AO_DATA_DIR`/`AO_RUN_FILE`/`AO_PORT` congelados y
   `AO_DAEMON_COMMAND="<binario congelado> daemon"` (sin `AO_KEEP_DAEMON`). Como no hay daemon
   vivo, la app lanza y **posee** el suyo (`owner=app`). Se exige: daemon `ready` con identidad
   probada (binario congelado, data dir, puerto, instalación), run-file `owner=app` con `appRunId`,
   el renderer pidiendo `/api/v1/auth/me`, ningún `refusing to start`; después la app sale (Ctrl+C
   al grupo, o el operador con `--interactive`), su daemon se detiene solo (EOF del supervisor) y el
   puerto queda libre. Salida de la app en `process/electron-<stamp>-cN.log`.
5. Pase lo que pase: `daemon_start` del harness, `quick_check`, checkpoint `post-electron`, fin
   del evento. Registra `electron_close_reopen` pass/fail con los checks de cada ciclo.

### Reboot (una vez en P11, preferible 48h/72h)
Reiniciar la Mac. El monitor vuelve con el login y registra `reboot_detected`. **AO no tiene
auto-arranque**: tras el login, `p11 daemon-start` y `p11 checkpoint --label post-reboot`.

## 9. Umbrales (congelados en el manifest antes de T0)

| Umbral | Valor | Uso |
| --- | --- | --- |
| hueco de heartbeat | > 2.5 intervalos | se explica (sleep) o es UNKNOWN |
| evidencia de sleep | ≥ 30 s de `MONOTONIC − UPTIME_RAW` | |
| ciclo sleep/wake válido | ≥ 600 s dormido | aceptación |
| salto de reloj | \|Δwall − Δmonotonic\| > 120 s | duración UNKNOWN |
| ventana sin explicar | > 30 min | duración UNKNOWN |
| daemon caído fuera de ventana | ≥ 3 heartbeats | incidente `high` |
| ventana de mantenimiento | 30 min (reboot: 12 h) | |
| memoria: warm-up | 60 min por instancia | |
| memoria: pendiente | > 8 MB/h (mínimos cuadrados de medianas horarias, dentro de cada instancia) | UNKNOWN (revisión) |
| memoria: crecimiento | última mediana > 2× primera | UNKNOWN (revisión) |
| memoria: evidencia mínima | 6 h post warm-up | |
| disco libre | < 10 GB | atención |
| huérfano | persiste ≥ 2 escaneos | incidente crítico |

Memoria: una subida aislada no es leak. El harness **nunca** declara PASS con crecimiento por
encima del umbral; lo deja UNKNOWN para análisis humano (baseline, post-workload, post-idle,
post-restart). Es BLOCKER si tras ciclos equivalentes el RSS crece repetidamente, no vuelve a un
rango estable y es atribuible a AO. Swap alto por otras apps es observación; la Mac de referencia
arrancó P11 con 9.1 GB de 10 GB de swap usados **antes** de AO.

## 10. Base, backups y restore

- **T0** (AO parado): `integrity_check`, `foreign_key_check`, goose, SHA-256, tamaño, mtime,
  laterales, identidad, conteos; `ao backup create` + `verify` (debe ser `VALID compatible`).
- **Checkpoints**: `quick_check` en lectura `mode=ro` (WAL reader normal; no escribe páginas).
  Sin checkpoints WAL manuales: se observa el comportamiento real del WAL.
- **Final** (`p11 finalize`, sólo con duración cumplida): checkpoint final con backup online si
  falta → `ao stop` → base con `immutable=1`: `integrity_check`, `foreign_key_check`, goose, sin
  journal de restore → re-verify del último backup → **restore scratch**.
- **Restore scratch**: `ao backup restore <último> --yes --json --root <scratch>
  --allow-secret-key-mismatch` hacia `runs/<run>/scratch-restore/work-*/data` con `AO_DATA_DIR`,
  `AO_RUN_FILE` y `AO_BACKUP_DIR` propios. El flag es obligatorio y correcto: el scratch nunca
  recibe la `secret.key` real (el harness no lee secretos) y P10 rechaza sin él
  (`secret_key_mismatch`, comprobado). Verifica integridad, FK=0, goose = manifest, SHA de `ao.db`
  = manifest, conteos idénticos a la copia del backup, `installation_id` restaurado (o ausente en
  el backup), sin laterales ni journal. Luego borra el scratch y conserva el informe.
  **Nunca** se restaura `~/.ao/data`.
- Retención: P11 no hace `prune`. Cada backup real ocupa ≈850 MB; 24h/48h/72h generan
  2/4/5 backups. Hay que podar a mano, con autorización, cuando termine P11.

Nota observada: un `ao backup create` con AO parado deja `ao.db-wal` (0 B) y `ao.db-shm` (32 KB)
junto a la base real; `ao.db` no cambia (SHA idéntico). Es el comportamiento documentado en P10
(un lector `mode=ro` no puede borrarlos) y se registra en el baseline.

## 11. Runtime

Sólo el socket de AO (`tmux -L ao-<hash>`); nunca sesiones ajenas. Clases: `proven` (owner token =
fila viva **y** `$N` = `runtime_instance_id`), `instance_mismatch`, `orphan_candidate` (sellado con
nuestra instalación sin fila viva), `foreign_installation`, `unstamped`, `unreadable` (nunca
huérfano). También `liveRowsWithoutRuntime` (dominio de la recuperación P9; se registra).
Duplicado = más de una sesión viva por paso o más de un outbox `dispatched` por paso — no por
nombre tmux. Atascado se define por semántica (estado + ownership + decisión de recuperación del
readback P9), no por antigüedad del timestamp.

## 12. Incidentes

`p11 incident --code <c> --severity critical|high|attention --summary "..."` (y `--resolve INC-… --disposition "..."`).
Auto-abiertos por el harness (deduplicados): `duplicate_worker`, `duplicate_dispatch`,
`terminal_state_mutated`, `terminal_run_step_reopened`, `db_quick_check_failed`, `db_unreadable`,
`orphan_runtime`, `installation_mismatch`, `data_dir_mismatch`, `binary_under_test_not_serving`,
`binary_under_test_changed`, `backup_not_valid`, `restore_journal_present` (críticos);
`daemon_down_unplanned`, `unplanned_daemon_restart`, `fixture_failed` (high).
Deduplicación por episodio o causa, nunca por un texto con valores variables: una caída del daemon
es **un** `daemon_down_unplanned` (clave: inicio de la caída) por muchos heartbeats que dure, y su
fin es `daemon_available`; cada cambio de instancia no planificado es su propio
`unplanned_daemon_restart` (clave: instancia nueva). Una ventana de mantenimiento que expira con el
daemon caído registra `maintenance_expired_daemon_down` (attention) una vez.

**Reinician la etapa** (crítico): corrupción de base, worker/reviewer duplicado, ownership
incorrecto, write de generación stale, run terminal mutado, daemon irrecuperable, backup corrupto
silencioso, inconsistencia restore/recovery, huérfano AO real, workflow atascado más allá de la
política, trabajo durable perdido, fail-open P9/P10, crecimiento de memoria no acotado atribuible
a AO, cambio del binario bajo prueba.

**No reinician necesariamente** (se registran): indisponibilidad esperada, red ajena, cerrar la
terminal, sleep, reinicio planificado, crash de prueba, presión de memoria de otra app, fixture
que falla intencionadamente. Un `high` abierto deja la etapa en UNKNOWN hasta su disposición.

Informe de fallo: id, timestamp, horas transcurridas, síntoma, invariante, evidencia,
reproducción, componente sospechoso, severidad, base afectada, trabajo perdido, duplicado,
reinicio requerido. Después: **STOP** de la etapa.

## 13. Reinicio del reloj

Incidente crítico → registrar → detener la etapa (`p11 finalize --abort`) → reproducir →
`fix/p11-<incidente>` desde ECC con test que falla → fix mínimo → test verde → revisión
independiente si toca ownership/recovery P9, backup/restore P10 o durabilidad → merge sólo con
autorización → **nuevo run desde hora 0** (nuevo binario congelado si cambió el código).
Si cambia el binario bajo prueba a mitad de etapa, la etapa queda invalidada. Si el monitor
pierde una ventana crítica, se clasifica UNKNOWN y se decide explícitamente.

## 14. GO / NO-GO y aceptación

`p11 status` muestra GO salvo incidente crítico abierto o criterio FAIL. `p11 report` evalúa
sólo evidencia: lo que falta es UNKNOWN y **UNKNOWN nunca se convierte en PASS**.

| Criterio | 24h | 48h | 72h |
| --- | --- | --- | --- |
| duración válida (sin salto de reloj, sin ventana sin explicar > 30 min) | 24h | 48h | 72h |
| sin incidente crítico abierto; `high` con disposición | ✓ | ✓ | ✓ |
| reinicios graceful PASS | ≥1 | ≥2 | ≥2 |
| crash controlado del daemon PASS | ≥1 | ≥1 | ≥1 |
| fixtures P9 daemon/lifecycle/tmux y P10 (cada uno) PASS, 0 FAIL | ≥1 | ≥2 | ≥3 |
| checkpoints sin fallo; duplicados, inmutabilidad, huérfanos, identidad = 0 hallazgos | ✓ | ✓ | ✓ |
| ciclos sleep/wake con checkpoint post-wake sano | ≥1 | ≥2 | ≥3 |
| backups online VALID (0 inválidos) | ≥1 | ≥3 | ≥4 |
| restore scratch del último backup | ✓ | ✓ | ✓ |
| base final (integridad, FK, goose, sin journal) | ✓ | ✓ | ✓ |
| Electron cerrar/reabrir | — | ≥1 | ≥1 |
| reboot en todo P11 | — | — | ≥1 |
| memoria | PASS | PASS | PASS |

Sólo con 24H PASS + 48H PASS + 72H PASS: **AO OPERATIONALLY PROVEN FOR ROUTINE DAILY USE = YES**
(para el alcance de plataforma de arriba). No se afirma: libre de bugs, infalible, cero falsos
positivos futuros, HA de producción, multinodo.

Los criterios de esta sección y los umbrales de §9 quedan congelados antes del T0 de la etapa 24h.
Si una métrica necesita interpretación, la decisión se documenta en la evidencia; no se cambian
umbrales para conseguir un PASS.

## 15. Operación

```bash
p11=~/.ao/soak/p11/p11                  # wrapper: ejecuta el harness congelado del run actual
$p11 status                             # RUN, STAGE, START, ELAPSED, TARGET, heartbeat, checkpoint,
                                        # DAEMON, DB, BACKUP, INCIDENTS, MEMORY, NEXT EVENT, GO/NO-GO
$p11 checkpoint --label <l>             # evidencia antes/después de un evento manual
$p11 electron-event                     # Electron cerrar/reabrir (§8); registra electron_close_reopen
$p11 incident --code <c> --severity high --summary "..."
$p11 daemon-crash --confirm --restart   # crash controlado (identidad probada)
$p11 daemon-restart | daemon-stop | daemon-start
$p11 fixtures [--name p9-daemon-e2e]
$p11 report                             # informe desde evidencia (UNKNOWN nunca es PASS)
$p11 finalize                           # sólo con duración cumplida; --abort para detener sin PASS
```

Al volver tras horas o días: **no reiniciar el soak**. `p11 status` → `p11 checkpoint` → revisar
evidencia → continuar con el mismo run.

Coste: $0 de LLM. El harness no llama modelos ni crea workflows; P7 sigue en sombra con trabajo
real y P11 no genera muestras para P7. P7.3 NOT STARTED.

## 16. Self-test del harness (no cuenta como P11)

Run `run-selftest-20260914T221113Z-8aa516`, 45 min, heartbeat 60 s, sobre un data dir scratch
**vacío** (`~/.ao/soak/p11/selftest/data`, puerto 3012). No se usó una copia de la base real para
el daemon del self-test: un daemon arrancado sobre esa copia vería rutas reales de worktrees y
proyectos. La escala real se probó con el restore scratch de un backup de la base real.

| Paso | Resultado |
| --- | --- |
| `init` / `daemon-start` / `daemon-stop` / `baseline t0` / `backup t0` / `start` | OK; baseline `integrity ok`, FK 0, goose 170; backup `VALID compatible` |
| Monitor launchd | **2 defectos del harness** encontrados y corregidos antes de T0 (evento `harness_patched`): `daemon_stop` pasaba `result` dos veces a `event()`; el plist ponía `--run` detrás del subcomando (launchd relanzaba con error de uso). Test de regresión añadido (`CommandLineTest`) |
| Hueco durante el monitor roto | registrado como `observed_clock_gap` (UNKNOWN), no como sleep |
| Checkpoint `cp-t0` | ok |
| `daemon-crash --confirm --restart` | PASS: identidad probada (comando, instalación, data dir, `/healthz`), SIGKILL, `stale`, instancia nueva, misma instalación, `quick_check`, checkpoint post-crash sano; el monitor lo clasificó cambio de instancia **planificado** |
| Fixtures bajo launchd (fx-1) | `p9-tmux-e2e` 8/8 (7 s), `p9-daemon-e2e` 3/3 (96 s), `p9-lifecycle` 63/63 (10 s), `p10-daemon-e2e` 4/4 (40 s); 0 SKIP; sin residuo tmux |
| Kill del monitor (PID verificado por launchd + argumentos) | launchd lo relanzó; `monitor_restart` con el hueco registrado |
| `scratch-restore` del backup real de 850 MB | PASS en 16.8 s: `RESTORED`, integridad, FK 0, goose = manifest, SHA = manifest, conteos = backup, sin laterales ni journal; scratch borrado |
| Guard del kill | se negó correctamente la primera vez: `/usr/bin/python3` re-ejecuta el Python de CommandLineTools y la línea de comando no coincidía |

Observación de diseño: los fixtures corren dentro del bucle del monitor (≈2.5 min), así que los
heartbeats se pausan durante ese tiempo. Con el intervalo real de 300 s eso queda por debajo del
umbral de hueco (2.5 intervalos); en el self-test (60 s) produjo una ventana UNKNOWN de 30 s.

### 16.1 Tercer defecto y cierre

- **El monitor dejó de observar a las 22:28:32.** Causa verificada con `sfltool dumpbtm`: macOS
  Background Task Management tenía el ítem `com.aoagents.p11-soak` como
  `[enabled, disallowed, notified]` (interruptor "Allow in the Background" apagado para `python3`,
  Unknown Developer) y descargó el job con SIGTERM. Además, el harness salía con 0 ante una señal y
  `KeepAlive.SuccessfulExit=false` lo trataba como fin limpio. Corrección: el monitor sale con 75
  ante una señal si la etapa no está detenida (`stopped.json`), así launchd siempre lo relanza.
  **Requisito operativo**: el ítem debe estar permitido en *System Settings › General › Login
  Items & Extensions › Allow in the Background*; el usuario lo activó a las ≈22:47 y el job siguió
  cargado hasta `finalize`. Un harness no puede ni debe esquivar ese interruptor.
- Hueco resultante: 1049 s (17.5 min), registrado `monitor_restart` + UNKNOWN; por debajo del
  umbral de 30 min, por eso la duración del self-test es PASS, pero la ventana queda en la evidencia.
- Tras relanzarse, el monitor ejecutó lo vencido: `cp-mid` con backup online `VALID` y el reinicio
  graceful programado `rs-1` (PASS: instancia nueva, misma instalación, `quick_check`, goose,
  conteos terminales). También se ejecutaron a mano `checkpoint --backup` y `daemon-restart` (PASS).
- `finalize` antes de tiempo: **se negó** (0.66 h < 0.75 h).
- `finalize` a tiempo: parada graceful PASS, base final (integridad, FK 0, goose 170, sin journal
  ni laterales) PASS, re-verify del último backup `VALID`, restore scratch PASS, ítem launchd
  retirado, monitor salió con `stage stopped` (sin relanzamiento).

Informe (`runs/run-selftest-20260914T221113Z-8aa516/final-report.json`): **VERDICT PASS, GO** —
duración 0.77 h; 2 reinicios graceful; 1 crash controlado; los 4 fixtures; 5 checkpoints sin
hallazgos (duplicados, inmutabilidad, huérfanos, identidad); 4 backups `VALID` (2 online);
24 heartbeats; 2 reinicios del monitor. Memoria `NOT_APPLICABLE` (45 min no dan evidencia
post warm-up). El self-test valida el harness; **no cuenta como P11**. Su data dir scratch se
borró; la evidencia del run se conserva.

Pendiente para el operador (no automatizado): el backup semilla del self-test
`~/.ao/backups/aob-20260914T220848.585478000Z-39edd901` (850 MB, `VALID`, base real a goose 170)
queda en la raíz de backups; P11 no hace `prune`.

## 17. Etapa 1 (24h) — resultado, cierre y auditoría de evidencia

**Run `run-24h-20260914T225838Z-9c8855` — VERDICT PASS, GO.**
T0 `2026-09-14T22:59:11Z`, parada de etapa `2026-09-15T23:06:20Z`, duración **24.12 h** sobre la
base real (`~/.ao/data`, 936 MB) con el binario congelado `ao-9c80b9b2c`
(`fd03cc8c…`), puerto 3002, run-file `~/.ao/dev/running.json`.
Informe: `runs/run-24h-20260914T225838Z-9c8855/final-report.json`.

| Criterio (§14, 24h) | Resultado | Evidencia |
| --- | --- | --- |
| duración válida | PASS | 24.12 h; `clock_jump` 0, `observed_clock_gap` 0 |
| sin incidente crítico; `high` con disposición | PASS | `incidents/` vacío; 0 eventos con resultado distinto de ok/pass/VALID |
| reinicios graceful ≥1 | PASS | `rs-1` T+5.09 h: instancia nueva `aod-…f9ff45fb59ef`, misma instalación, `quick_check`, goose, conteos terminales no decrecientes |
| crash controlado ≥1 | PASS | T+15.18 h: identidad probada (comando, instalación, data dir, `/healthz`), SIGKILL a pid 58345, `stale`, instancia nueva `aod-…450d6de503e6`, checkpoint `post-crash` sano |
| fixtures P9/P10 ≥1 cada uno | PASS | 3 rondas × 4 fixtures = 12 ejecuciones, 0 FAIL, 0 SKIP |
| checkpoints sin hallazgos | PASS | 8 checkpoints, `findings: []` en todos; duplicados, inmutabilidad terminal, huérfanos e identidad = 0 |
| sleep/wake ≥1 | PASS | 1 ciclo probado por kernel: 1861 s dormido, hueco 2170 s, `unknownSec` 0, checkpoint `post-wake-1` sano |
| backups online VALID ≥1 | PASS | 3 creados (t0 offline, `cp-t12` y `cp-t24` **online**), 4 verificaciones `VALID compatible`, 0 inválidos |
| restore scratch del último backup | PASS | 15.0 s sobre `aob-20260915T225946…-138f9405`, scratch borrado, informe conservado |
| base final | PASS | `integrity_check` ok (4.87 s), FK 0, goose **170**, sin `-wal`/`-shm`/journal de restore |
| memoria | PASS | pendiente **0.13 MB/h** sobre 20 h post warm-up; primera mediana 55.56 MB, última 51.02 MB (3 instancias) |

Observación (no es un hallazgo): la base pasó de 908 447 744 B (`7ee966e6…`) en T0 a
936 574 976 B (`745865c8…`) al final — escritura propia del daemon durante 24 h, con integridad,
FK y goose intactos. Ningún evento del run tuvo resultado distinto de `ok`/`pass`/`VALID`;
`monitor_restart` = 0 (el monitor launchd observó las 24 h sin interrupción: 274 heartbeats).

### 17.1 Auditoría de evidencia (verificada al cierre)

- **Integridad de la evidencia**: las 33 entradas de `hashes.jsonl` re-verificadas archivo a
  archivo → 33/33 SHA-256 coinciden, 0 faltantes.
- **Harness**: `runs/<run>/harness/p11.py` y la copia instalada = `69ef0331…` = blob
  `scripts/p11/p11.py` del commit `a09adfde6`. El worktree `feat/p11-reliability-soak` está limpio.
- **Binario bajo prueba**: `go version -m` sobre `~/.ao/soak/p11/bin/ao-9c80b9b2c` →
  `vcs.revision=9c80b9b2c63aa0e28acf96ac90a82b62430a8b8e`, `vcs.modified=false`, `-trimpath=true`;
  SHA-256 `fd03cc8c…` = manifest = `binarySha256` del informe final.
- **Fuente congelada**: `src/9c80b9b2c/backend` = 2580 blobs, todos idénticos a
  `git ls-tree -r 9c80b9b2c backend`; 0 archivos extra, 0 diferencias.
- **Fixtures**: los 4 binarios mantienen el SHA-256 congelado en el manifest.
- **Cierre operativo**: `stopped.json` presente, `monitor_uninstalled` rc 0, el plist
  `~/Library/LaunchAgents/com.aoagents.p11-soak.plist` ya no existe, no hay daemon vivo, ni
  servidor tmux, ni `running.json` residual.
- **Retención**: se conservan los 3 backups de la etapa más el semilla del self-test; P11 no hace
  `prune` (§10). Disco libre al cierre: 57 GB.

La etapa 1 queda cerrada. **24H PASS**; 48H y 72H siguen pendientes y son independientes (§7).

## 18. Etapa 2 (48h) — lanzamiento

**Run `run-48h-20260915T231824Z-50fdac`**, T0 `2026-09-15T23:19:27Z`, fin previsto
`2026-09-17T23:19:27Z`. Misma configuración congelada que la etapa 1, sin cambios de código de AO
ni de criterios: binario `ao-9c80b9b2c` (`fd03cc8c…`, `vcs.revision=9c80b9b2c…`,
`vcs.modified=false`), ECC `9c80b9b2c63aa0e28acf96ac90a82b62430a8b8e`, data dir `~/.ao/data`,
puerto 3002, run-file `~/.ao/dev/running.json`, socket `ao-c5b10da60941`, fuente y fixtures
congelados con sus SHA del manifest, heartbeat 300 s, restore scratch con
`--allow-secret-key-mismatch` en el puerto 3019.

Comprobaciones previas al T0 (todas OK): etapa 1 finalizada (`stopped.json`, `monitor_uninstalled`,
plist retirado) y su evidencia íntegra (33/33 hashes); sin daemon competidor, sin servidor tmux,
sin `running.json` residual; harness idéntico al blob del commit y worktree limpio; base real
`integrity_check` ok, FK 0, **goose 170**; 57 GB libres.

Baseline T0: `745865c8…`, 936 574 976 B, goose 170, FK 0, integridad completa (8.88 s).
Backup T0 (offline, `VALID compatible`): `aob-20260915T231849.806651000Z-4d21afff`.
Daemon bajo prueba: instancia `aod-41a6a369-897b-4852-8322-32fe8cfd134c`, instalación
`aoi-93bd35a0-d14c-42b8-9dd7-b76335a2f78d`, identidad sin problemas.

Eventos automáticos (§7, congelados): checkpoints T+0/12/24/36/48 más `bk-active` en T+19.05;
backups online en T+19.05, T+24 y T+48; reinicios graceful en T+7, T+19 y T+31; fixtures en
T+3, 11, 21, 33 y 43. Manuales: dos noches de sleep ≥20 min, Electron cerrar/reabrir
(T+14h..T+30h), `daemon-crash --confirm --restart` (T+26h..T+40h) y `finalize` (≥T+48h).

El plan congelado **no exige reboot en la etapa de 48h** (§14: reboot "—" en 48h, "≥1" en 72h,
contado en todo P11). No se añade ni se elimina: si ocurre un reboot en esta etapa, el monitor lo
registra y cuenta para el requisito de P11; si no, queda para la etapa de 72h.

## 19. Etapa 2 (48h) — FAIL / NO-GO y correcciones

**Veredicto del harness: `FAIL / NO-GO`** (`final-report.json` del run
`run-48h-20260915T231824Z-50fdac`), cerrada con `finalize --abort` a T+17.31h tras autorización.
`electronCloseReopen` FAIL, `duration` FAIL (17.31h de 48h), `noCriticalIncident` UNKNOWN
(INC-001..009 abiertos, sin reescribir). Resto de criterios con evidencia: fixtures P9/P10 PASS 2/2,
checkpoints 2/2 sin hallazgos, base final PASS (integridad completa, FK 0, goose 170), backup T0
re-verificado VALID, restore scratch PASS, memoria PASS.

### 19.1 Causa raíz (confirmada)
Evento Electron manual según el §8 anterior: `maintenance --reason electron` (15:19:13Z) y la app
dev con `AO_DAEMON_COMMAND` = binario congelado. A las 15:25:28.229Z la app hizo `POST /shutdown` al
daemon del soak (pid 13668, `ready` a las 15:20:50Z); 26 ms después lanzó el reemplazo, que salió con
`another AO daemon holds data dir …; refusing to start`. El daemon cierra su listener HTTP antes de
liberar `daemon.lock` (el `flock` cae con el proceso) y la app daba por terminado al anterior en
cuanto `/healthz` dejaba de responder. Sin reintento, y con `DaemonFailureBanner` debajo de
`ShellAuthGate`, la ventana quedó en "Preparando tu tablero" sin error. La ventana de mantenimiento
expiró (15:49:13Z) con el daemon caído y el harness abrió un `daemon_down_unplanned` por heartbeat
(INC-001..009, una sola caída). Evidencia: `analysis/root-cause-daemon-down-20260916.md` del run.

Reproducción con el binario congelado sobre data dirs scratch
(`frontend/src/shared/daemon-replace.e2e.test.ts`, un cliente con una petición en curso mantiene el
proceso —y el lock— vivo tras cerrar el listener): semántica anterior **10/10 rechazos**; corregida
**10/10 arranques**; con la base real restaurada (sin petición en curso) 10/10.

### 19.2 Defectos y clasificación
| Id | Tipo | Defecto | Corrección |
| --- | --- | --- | --- |
| AO-1 | bug AO | el reemplazo no esperaba la salida real del daemon anterior | `awaitPreviousDaemonExit`: PID fuera (o zombi) y `/healthz` mudo; falla cerrado ante timeout, estado ilegible u otro daemon en el puerto |
| AO-2 | bug AO | sin reintento ni detalle ante un rechazo de lock | reintento acotado (250/500/1000 ms) sólo para el rechazo literal de lock tras un reemplazo probado y sin competidor; salida del hijo keep-alive leída de `~/.ao/daemon.log` |
| AO-3 | bug AO | fallo de arranque invisible (loader infinito) | `ShellAuthGate` muestra `DaemonFailureBanner` mientras el supervisor reporta fallo |
| H-1 | bug harness | `daemon_down_unplanned` deduplicado por texto con contador; `unplanned_daemon_restart` al revés (uno por run) | clave por episodio / por instancia |
| H-2 | bug procedimiento | `electron-1` incompatible con el diseño de la app (y el script anterior lanzaba `go run`) | `p11 electron-event` (§8) |
| H-3 | deuda harness | mantenimiento expirado con daemon caído sin registro; eventos programados podían actuar durante un evento manual | `maintenance_expired_daemon_down`; `eventInProgress` difiere lo programado |
| H-4 | — | "harness sin versionar": **no era un defecto**; la fuente canónica es `scripts/p11/p11.py` en `feat/p11-reliability-soak`, idéntica (SHA `69ef0331…`) a las copias instaladas | ninguna |
| H-5 | bug harness (latente) | `pid_alive` = `kill(pid, 0)` da vivo a un zombi: un daemon lanzado por el monitor y parado por otro actor | zombi = no vivo (`ps stat`), presunto vivo si `ps` no responde |

H-5 no se disparó en la etapa 2 por el incidente, pero lo habría hecho en `rs-2` (T+19h: el monitor
para un daemon que él mismo lanzó en `rs-1` → `pidGone=false` → `daemon_restart` FAIL + crítico).
Ya aparece en la evidencia de la etapa 1: `daemon_crash_injected` registró `pidGone: false` (esperó
30 s a un zombi del monitor); el check final pasó sólo porque el monitor lo recogió después.

### 19.3 Validación (data dirs scratch, nunca `~/.ao/data`)
Base de datos: restore del backup T0 de la etapa 2 con el binario congelado en
`~/.ao/scratch/p11-electron-fix/base/data`, clonado (APFS) por ciclo. Huella de `~/.ao/data`
idéntica antes y después; perfil `~/.ao/dev/electron` respaldado y restaurado byte a byte.

- **V1 — camino del incidente en la app real** (daemon headless congelado sirviendo, la app lo
  reemplaza; clientes con una petición en curso cada 2 s para que el `/shutdown` siempre drene con
  el lock retenido): código anterior (`9c80b9b2c`) **1/5** (4 ciclos sin reemplazo en 240 s: la app
  paró el daemon y no volvió a haber daemon); corregido **10/10**: reemplazo con el binario
  congelado, `owner=persistent`, renderer conectado, sin `refusing to start`. En los 10 ciclos el
  daemon anterior drenó 4–6 s tras `/shutdown` y el reemplazo arrancó ~100 ms **después** de su
  salida.
- **V2 — `p11 electron-event --cycles 10`** (harness/2, run scratch `selftest`): **10/10 ciclos**
  (identidad probada, `owner=app`, renderer conectado, app y daemon salen, puerto libre), daemon
  del harness reiniciado, `quick_check` ok, `electron_close_reopen` **pass**. El checkpoint
  `post-electron` marca `goose_changed` (attention) sólo porque el run scratch no tenía baseline T0.

### 19.4 Revisión independiente y alcance
Revisión adversarial independiente de ambas ramas: **sin BLOCKER**. Corregidos antes de repetir la
validación: AO — sondeo de estado que podía volver a reemplazar tras un fallo (bloqueo hasta un
start/restart explícito), reintento decidido con salida de intentos anteriores (ahora sólo la del
hijo), zombi en la toma del puerto, `/shutdown` sin timeout; harness — checkpoint `post-electron`
que podía perder el lock frente a lo diferido (ahora antes de liberar el evento), restauración del
daemon con la app aún viva (ahora no se restaura y queda `electron_event_blocked`), ventana que
ignoraba `--hold`/`--interactive`, `pid_alive` con `ps` fallido, caída heredada de harness/1,
comandos de operador durante el evento (rechazados), excepción sin registro (ahora `fail`), test
del zombi que no veía `Z`.

Pendiente, anterior a este cambio y **no corregido aquí** (decisión aparte, P9): la app pide
`/shutdown` también a un daemon cuya identidad no coincide (`identity_mismatch` en
`inspectExistingDaemon`), caso que `decidePortHolderTakeover` sí rechaza.

Alcance: `p11 electron-event` para el daemon del soak antes de abrir la app, así que **no ejercita**
el camino de reemplazo que falló; ese camino (AO-1/2/3) queda validado por V1 y por
`daemon-replace.e2e.test.ts`, no por el soak. Los umbrales y `evaluate()` no cambian; la
deduplicación por instancia hace que cada reinicio no planificado abra su propio incidente `high`
(antes, uno por run): cambia la entrada del criterio, no el criterio.
