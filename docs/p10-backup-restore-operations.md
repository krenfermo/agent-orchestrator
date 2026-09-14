# P10 — Backup / Restore / Operations

*Estado: implementado en `feat/p10-backup-restore-operations` (base `847c47808`). Clasificación: **BACKUP / RESTORE OPERATIONALLY PROVEN**; la fiabilidad operativa de larga duración todavía **no** está probada (P11). Sustituye la
sección "Pendiente" de [`backup-restore.md`](backup-restore.md), que describía el
procedimiento manual con `VACUUM INTO` y la prueba de que funciona; P10 lo
convierte en comandos con verificación, restauración, rollback y retención.*

> **Verificar prueba integridad, no autenticidad.** Un backup `VALID` es uno
> cuyos bytes coinciden con su manifest y cuya base pasa las comprobaciones de
> SQLite. No prueba quién lo produjo: P10 no firma backups. Restaura sólo
> backups cuyo origen conoces.

## 0. Auditoría previa (qué había)

| Hecho | Evidencia |
| --- | --- |
| Driver SQLite puro Go `modernc.org/sqlite v1.51.0` | `backend/go.mod` |
| Pragmas del daemon: `journal_mode(WAL)`, `busy_timeout(5000)`, `foreign_keys(ON)`, `synchronous(NORMAL)`; un writer + 8 lectores | `storage/sqlite/db.go` |
| `sqlite.Open` **ejecuta migraciones goose** (`goose.Up` + reparaciones) | `storage/sqlite/db.go` `migrate()` |
| El daemon toma `flock` exclusivo en `<data>/daemon.lock` y `<runfile>.lock` **antes** de abrir la base | `daemon/daemon.go` (P9) |
| Identidad de instalación en `<data>/installation_id` (`aoi-<uuid>`, 0600, creada una vez con `O_EXCL`/link) | `daemonmeta/identity.go` |
| Descubrimiento de daemon con prueba de identidad (`ready`/`unhealthy`/`running_unverified`/`foreign`/`stale`) sobre ambos run-files | `cli/daemon_discovery.go` |
| Guard offline en dos capas (run-files + sonda SQLite `locking_mode=exclusive`) | `cli/usage_backfill.go` `assertNoLiveDaemon` |
| `ao import` sólo miraba `cfg.RunFilePath` (deuda P9) | `cli/import.go` |
| No existía comando de backup, verify, restore ni retención; sólo un test de `VACUUM INTO` | `storage/sqlite/backup_restore_test.go` |
| Copias históricas en `~/.ao/data/ao.db.backup-*` y `~/.ao/backups/pre-*` hechas con `cp` | listado del operador (no se tocan) |

Sondas empíricas con el driver real (test temporal, borrado):

- `VACUUM INTO` sobre una base WAL con marcos sin checkpoint produce un archivo
  **independiente en modo rollback** (bytes 18-19 = 1,1), sin `-wal`/`-shm`, que
  contiene esos marcos.
- Abrir con `mode=ro&immutable=1` no crea archivos laterales.
- La sonda exclusiva (`busy_timeout(0)` + `locking_mode(exclusive)` + escritura
  en transacción revertida) falla con `SQLITE_BUSY` si otra conexión — incluso un
  lector `mode=ro` ocioso — tiene la base abierta, y pasa si nadie la tiene.
- Un lector `mode=ro` sobre una base WAL cerrada **deja `-wal`/`-shm` vacíos**
  al lado (no puede borrarlos). No modifica páginas (el SHA de `ao.db` no cambia).

## 1. Inventario de activos

| ACTIVO | CRÍTICO | BACKUP | RESTORE | DERIVADO | SECRETO | RAZÓN |
| --- | --- | --- | --- | --- | --- | --- |
| `ao.db` (+ WAL) | sí | **sí**, snapshot `VACUUM INTO` | sí | no | contiene 3 columnas cifradas | Estado durable autoritativo, incluido `goose_db_version` |
| `ao.db-wal`, `ao.db-shm`, `ao.db-journal` | — | **no** | se **retiran** del destino | transitorio | no | El snapshot ya incorpora el WAL; un WAL/SHM/journal viejo junto a una base restaurada la corrompe |
| `installation_id` | sí | **sí** | sí, según política §19 | no | no (ya se publica en `running.json` y logs) | Identidad P9; los runtimes tmux se sellan con ella |
| `secret.key` | sí | **no** | no se toca | no | **sí** (AES-256-GCM) | Secreto. Cifra `app_settings.smtp_password_encrypted`, el token de work items y `skill_secrets.sealed_value`. El manifest lleva sólo una **huella** unidireccional; restore se niega si la clave del destino no coincide (§19b) |
| `skills/catalog/**` | sí | **sí** (132 KB en la máquina de referencia) | sí | no | no | `skill_installs.package_dir` apunta a estos archivos por ruta absoluta y digest; no se regeneran |
| `skills/using-ao/` | no | no | no se toca | sí | no | Se reescribe en cada arranque |
| `skills/quarantine/` | no | no | no se toca | transitorio | no | Staging de descargas |
| `codegraph/` | no | no | no se toca | sí (caché `graph.json`) | no | El grafo autoritativo vive en SQLite (migración 0153) |
| `project-memory/` | no | no | no se toca | sí | no | La memoria durable vive en SQLite (migración 0144) |
| `worktrees/` | sí para el usuario | **no** | no se toca | no | puede | Árboles git de trabajo (380 MB en la referencia). El código vive en sus repos; la regla "no borrar worktrees sucios" prohíbe reemplazarlos. La base guarda sus rutas |
| `users/<uuid>/` | — | **no** | no se toca | no | **sí** (logins de proveedores, 2.1 GB) | Credenciales de proveedor |
| `cli-credentials.json` | — | **no** | no se toca | no | **sí** | Token CLI; se vuelve a hacer `ao auth login` |
| `agent-credentials/` | — | **no** | no se toca | no | **sí** | Credenciales por lanzamiento, revocadas por el reconciliador |
| `prompts/`, `incidents/`, `reviewer-runtime/`, `tmp/`, `hook-bin/`, `scratch/` | no | no | no se toca | transitorio/derivado | pueden contener prompts | Se recrean al lanzar; prompts/transcripts no son modelo durable de AO |
| `daemon.lock`, `running.json`, `*.lock` | no | **no** | no se toca | transitorio | no | Autoridad de runtime vivo, nunca autoridad de restore (§81) |
| `hooks.log`, `telemetry_cli_daily.json`, `~/.ao/daemon.log` | no | no | no se toca | log | no | |
| `~/.ao/electron/`, `~/.ao/app-state.json` | no | no | no se toca | sí | cookies | Caché del supervisor |
| Keychain | — | no | no | — | sí | Fuera del data dir |

Consecuencia: **un backup AO = `ao.db` (snapshot) + `installation_id` + `skills/catalog/**` + `manifest.json`**.
Restore sólo reemplaza esas entradas (y retira los laterales de SQLite). Todo lo
demás del data dir queda exactamente como estaba.

## A. Unidad de backup y formato

Un directorio auditable, sin empaquetar:

```
<backup-root>/
  aob-20260914T101530.123456789Z-1a2b3c4d/   <- backup finalizado (nombre = backupId)
    manifest.json                            0600
    ao.db                                    0600
    installation_id                          0600  (si el origen tenía)
    skills/catalog/...                       0700/0600 (bits de grupo/otros eliminados)
  .staging-aob-...                           <- en curso o huérfano; nunca "válido"
  .deleting-aob-...                          <- prune interrumpido
  .locks/aob-....lock                        <- flock del backup en uso
  operations.jsonl                           <- historial de operaciones (metadatos)
```

- **backupId**: `aob-<UTC con nanosegundos>-<8 hex aleatorios>`. Ordenable
  lexicográficamente y sin colisión entre creaciones concurrentes.
- **Raíz**: `--root`, si no `AO_BACKUP_DIR`, si no `<padre del data dir>/backups`
  (= `~/.ao/backups`). Se rechaza una raíz igual o dentro del data dir, o que sea
  un archivo. Las entradas que no siguen el patrón `aob-*` (las copias históricas
  `pre-0170-…`) **se ignoran y nunca se modifican**.

## B. Modelo de consistencia

- Método `vacuum_into`, consistencia `single_read_transaction`: `VACUUM INTO`
  corre dentro de una transacción de lectura y ve **una** instantánea; los
  marcos WAL confirmados forman parte de ella. El resultado es un archivo
  independiente en modo rollback — no depende de copiar `-wal`.
- La conexión de origen es `mode=ro`: jamás escribe páginas de la base.
- **Backup online permitido**: con el daemon vivo, un lector WAL concurrente es
  el uso normal de SQLite; el writer no se bloquea. Con el daemon parado, igual.
  *Restore, en cambio, exige siempre AO parado.*
- Tras el snapshot: `PRAGMA integrity_check` (completo), `PRAGMA
  foreign_key_check`, lectura de `MAX(version_id)` de `goose_db_version` sobre
  la copia abierta `immutable=1`.
- Dos backups simultáneos están **permitidos**: cada uno tiene id, staging y
  lock propios.

### Creación atómica

1. `flock` en `.locks/<id>.lock`; 2. crear `.staging-<id>/` (0700);
3. `VACUUM INTO` → `staging/ao.db`; 4. copiar `installation_id` y
`skills/catalog/**` (sin seguir symlinks) calculando SHA-256 en streaming;
5. `fsync` de cada archivo; 6. checks SQLite sobre la copia; 7. escribir
`manifest.json` (temp + fsync + rename); 8. `fsync` del staging;
9. `rename(.staging-<id>, <id>)` + `fsync` de la raíz. Un fallo o Ctrl+C antes de
9 elimina el staging; un crash lo deja como `.staging-*`, que `list` muestra
como `incomplete` y `prune` elimina sólo si su lock está libre.

## C. Verify

`ao backup verify <path>` no escribe nada. Devuelve `VALID` / `INVALID` /
`UNSUPPORTED` con códigos de razón, y una compatibilidad
`compatible` / `upgrade_required` / `newer_than_binary`.

Comprueba: directorio no-symlink y no staging; `manifest.json` regular, ≤ 4 MiB,
JSON estricto (campos desconocidos rechazados); formato `ao.backup/v1` (otra
versión `ao.backup/vN` → `UNSUPPORTED`); rutas de assets (relativas, limpias,
sin `..`, sin absolutas, sin duplicados ni colisiones de mayúsculas, rol
coherente con la ruta); todo archivo del directorio listado (un `ao.db-wal`
extraño → `stray_sqlite_sidecar`); ningún symlink; tamaño y SHA-256 por asset;
base abierta `immutable=1`: `integrity_check` (o `quick_check` con `--quick`),
`foreign_key_check`, versión goose igual a la del manifest.

## D. Restore

`ao backup restore <path>` (alias `ao restore`). Más estricto que backup.

**Preflight — nada escribe en el destino:**

1. no hay journal de restore previo (`restore_interrupted`);
2. verify completo del origen: `VALID` y no `newer_than_binary`;
3. origen fuera del data dir destino (`source_inside_destination`), y ni el data
   dir ni la raíz del pre-restore dentro del backup origen
   (`destination_inside_source`, comprobado antes de crear nada);
4. descubrimiento P9 sobre ambos run-files: `ready`/`not_ready`/`unhealthy` →
   `daemon_active`; `running_unverified` → `daemon_unverified`; dos daemons →
   `daemon_ambiguous`. `stale`/`foreign`/`stopped` permiten seguir. **Nunca se
   envía una señal ni se borra un run-file**;
5. `flock` de `<data>/daemon.lock` (el mismo lock P9 del daemon), retenido
   durante todo el restore: ningún daemon puede arrancar a mitad (`data_dir_locked`);
6. sonda SQLite exclusiva sobre `ao.db` (`db_in_use`) **y** la lista de
   descriptores abiertos del sistema operativo (`lsof` en macOS/BSD, `/proc` en
   Linux) sobre `ao.db` y sus laterales: cualquier otro proceso que los tenga
   abiertos → `db_in_use` nombrando el PID; no poder listarlos también rechaza
   (§G). Una base que SQLite no puede abrir (`SQLITE_CORRUPT`/`SQLITE_NOTADB`) →
   `destination_damaged`, salvo `--preserve-broken-state` (§E2); su exclusión la
   prueba entonces sólo la lista del SO;
7. `ao.db`/`installation_id`/`skills/catalog` del destino no son symlinks;
8. política de identidad y de clave (§19);
9. sin base en el destino no hay backup pre-restore posible: rechazo
   `no_rollback_possible` si el restore reemplazaría un `skills/catalog` no vacío
   o una identidad distinta, que sólo existen ahí;
10. espacio libre: tamaño del backup (staging) + tamaño actual (rollback) + 64 MiB
   (`insufficient_space`).

**Ejecución:**

1. journal `<data>/.ao-restore-journal.json` (fase `preparing`, fsync);
2. **backup de rollback** `pre-restore` del estado actual, verificado. Si falla →
   `rollback_backup_failed`, destino intacto; si falla porque la base actual está
   dañada (integridad, FK, ilegible) → `destination_damaged`, y con
   `--preserve-broken-state` se toma en su lugar una **copia forense** byte a byte
   (§E2);
3. staging en el **mismo filesystem** del destino
   (`<data>/.ao-restore-<id>/staged`): copia en streaming con SHA-256 comparado
   con el manifest, `fsync`, permisos 0600; `quick_check` + goose del staged;
4. segunda sonda exclusiva y segunda comprobación de descriptores abiertos;
5. fase `swapping`: mover a `previous/` las entradas actuales (`ao.db`,
   `ao.db-wal`, `ao.db-shm`, `ao.db-journal`, `installation_id`,
   `skills/catalog`); comprobar que no queda ningún lateral SQLite; promover las
   entradas staged con `rename`; `fsync` del directorio;
6. fase `swapped`: **verificación final** — sin laterales, SHA-256 y tamaño de
   `ao.db` iguales al manifest, `quick_check`, goose igual, identidad y archivos
   de skills correctos, y **ningún proceso conserva un descriptor del `ao.db`
   reemplazado** (ahora en `previous/`); si alguno lo tiene → rollback;
7. éxito → fase `complete` (durable), se elimina el journal y **después**
   `previous/` y el staging. **El backup de rollback se conserva.** RESTORED
   (exit 0) sólo se informa cuando el journal dice `complete` en disco.

No se ejecutan migraciones durante el restore. La base queda exactamente como
se respaldó; el siguiente arranque del daemon decide migrar (§21-22).

## E. Rollback

- Si la verificación final falla (o cualquier paso posterior a `swapping`):
  rollback automático — las entradas promovidas van a `failed/`, las de
  `previous/` vuelven a su sitio (renames atómicos), se comprueba que `ao.db` es
  **el mismo inode** que antes del swap y que pasa `quick_check`.
  Resultado `RESTORE_FAILED_ROLLED_BACK` (exit 6).
- Si el rollback también falla: `RESTORE_FAILED_ROLLBACK_FAILED` (exit 7), el
  journal queda en `rollback_failed`, y el mensaje nombra el backup de rollback y
  `ao backup recover`.
- El algoritmo es idempotente y converge desde cualquier punto (incluido un
  rollback interrumpido): una entrada promovida cuyo original sigue en
  `previous/`, o que no existía antes del swap (`preExisting` del journal), es la
  restaurada y va a `failed/`; si su original ya volvió, se conserva. Un lateral
  SQLite que no existía antes del swap va a `failed/`. Todo lo que sigue en
  `previous/` vuelve a su nombre. Nunca se mueve un archivo a través de un
  symlink del work dir.
- Antes de mover nada, el rollback comprueba que ningún proceso tiene abierta la
  base **restaurada**: moverla dejaría los `-wal`/`-shm` de ese proceso, por
  nombre, pegados a la base anterior. Si alguno la tiene →
  `RESTORE_FAILED_ROLLBACK_FAILED` con el journal pidiendo `recover`, que vuelve a
  comprobarlo.
- **El journal manda** (revisión independiente): si no se puede registrar
  `complete`, el restore **hace rollback** (exit 6) — nunca sale 0 con un journal
  que aún pueda leerse como inacabado. En abandon, rollback y recover el journal
  se elimina **antes** que el work dir; si no se puede eliminar se reescribe a la
  fase asentada; si tampoco, el work dir se conserva y el informe marca
  `recoverRequired` (la CLI dice "ejecuta `ao backup recover` antes de arrancar").
- Un journal `complete` contradicho por un rollback (existe `failed/`, que el
  rollback crea antes de mover nada) se trata como no asentado: arranque
  rechazado y `recover` hace rollback en vez de borrar `previous/`.

## E2. Restaurar sobre una base dañada (`--preserve-broken-state`)

Se restaura, a menudo, **porque** la base actual está rota — y una base rota no
puede producir un backup pre-restore `VALID`.

- Por defecto: `destination_damaged` (exit 3), nada cambia, el mensaje nombra la
  salida. Cuenta como dañada: `SQLITE_CORRUPT`/`SQLITE_NOTADB` al abrirla,
  `integrity_check` fallido, violaciones FK. **No** cuenta: espacio, E/S,
  cancelación — ahí el flag no cambia nada (no es un `--force`).
- Con `--preserve-broken-state`: antes de tocar el destino se copia byte a byte
  `ao.db` y sus laterales tal cual, `installation_id` y `skills/catalog` a
  `<root>/aof-<id>/`, con `forensic.json` (tamaños y SHA-256, sin rutas
  absolutas). Cada archivo se copia con fsync y después copia **y** origen se
  vuelven a hashear y comparar; sólo entonces se promueve la copia y sigue el
  restore normal (staging, swap, verificación, rollback atómico por renames).
- La copia forense es evidencia, **no** un backup: `verify` la declara INVALID,
  `list` no la muestra, `prune` nunca la borra (un `.staging-aof-*` huérfano
  tampoco se poda: bórralo a mano). El informe la marca
  `rollbackKind=forensic_copy`.
- Un crash a mitad del swap sobre una base dañada: `recover` devuelve el
  original dañado byte a byte (no necesita abrirlo).

## F. Manifest `ao.backup/v1`

```json
{
  "format": "ao.backup/v1",
  "backupId": "aob-20260914T101530.123456789Z-1a2b3c4d",
  "kind": "manual | pre-restore | pre-migration",
  "createdAt": "2026-09-14T10:15:30.123456789Z",
  "tool": { "name": "ao", "version": "…", "commit": "…" },
  "source": {
    "dataDirFingerprint": "hex(16 bytes)",
    "installationId": "aoi-…",
    "secretKeyFingerprint": "hex(16 bytes) | vacío",
    "journalMode": "wal"
  },
  "schema": { "gooseVersion": 170, "binaryHead": 170 },
  "method": "vacuum_into",
  "consistency": "single_read_transaction",
  "checks": { "integrityCheck": "ok", "foreignKeyViolations": 0 },
  "assets": [
    { "path": "ao.db", "role": "database", "size": 0, "sha256": "…", "mode": "0600" }
  ],
  "notes": ""
}
```

Sin rutas absolutas (el data dir se registra como huella), sin entorno, sin
tokens, sin prompts. El manifest no contiene su propio hash. Las huellas son
`SHA-256("ao.backup/v1 <etiqueta>\0" ‖ valor)` truncado a 16 bytes: permiten
comparar igualdad, no recuperar el valor (una clave de 256 bits no es atacable
por fuerza bruta).

## G. Guard de daemon activo y exclusión de base

Cuatro capas; ningún sistema de locks nuevo para el data dir:

| Capa | Qué prueba | Fallo |
| --- | --- | --- |
| Descubrimiento P9 (probe `/healthz` con PID + instance + data dir) | que ningún daemon de esta instalación está vivo o vivo-sin-verificar | `daemon_active`, `daemon_unverified`, `daemon_ambiguous` |
| `flock` en `<data>/daemon.lock` retenido todo el restore | que ningún daemon P9 lo tiene, y que ninguno puede arrancar durante el restore | `data_dir_locked` |
| Sonda SQLite exclusiva, antes del pre-restore y antes del swap | que ninguna conexión con **lock** tiene la base abierta | `db_in_use` |
| Descriptores abiertos del SO (`lsof`/`/proc`): antes del pre-restore, antes del swap, **después de los renames** sobre `previous/`, antes de cualquier rollback | que **ningún proceso** tiene un descriptor de la base o sus laterales, tenga lock o no | `db_in_use` (antes) / rollback (después) |

**Por qué la cuarta capa (revisión independiente, BLOCKER).** Una conexión
SQLite no tiene ningún lock hasta su primera lectura: un `sqlite3 ao.db` abierto
y ocioso pasaba las dos sondas. Ese proceso conserva un descriptor del inode
viejo pero calcula `-wal`/`-shm` por **nombre**; tras el swap su primera
escritura creaba un WAL que se adjuntaba a la base **restaurada** y la corrompía
(`database disk image is malformed`) con el restore ya informado como RESTORED.
Reproducido con un `sqlite3` real, abierto antes del restore y abierto dentro de
la ventana. Nadie puede obtener un descriptor del inode viejo una vez renombrado
a `previous/`, así que la comprobación posterior a los renames cierra la ventana:
quien abra la ruta después abre la base restaurada, que es una conexión normal.
Si algo tiene el inode viejo, rollback — que le devuelve el archivo que abrió.
En Windows SQLite abre sin `FILE_SHARE_DELETE`: el rename de una base abierta
falla y el swap hace rollback solo.

`ao import` y `ao usage backfill-cache-ttl` pasan a usar el mismo guard offline
(ambos run-files + `daemon.lock` + sonda cuando hay base + la puerta de arranque:
se niegan ante un restore interrumpido en fase crítica) — cierra la deuda P9 de import.

Residual acotado: un proceso que abra la base **restaurada** entre el swap y un
rollback posterior lo bloquea (rollback fallido explícito, `recover` tras
cerrarlo); un proceso que escriba en la base vieja durante la ventana y la
cierre antes de la comprobación escribe en el inode descartado (pérdida de esa
escritura ajena, nunca corrupción de la restaurada: sus laterales huérfanos
hacen fallar la verificación final).

## H. Semántica de fallos

| Fallo | Garantía |
| --- | --- |
| backup create falla / Ctrl+C | origen intacto; staging eliminado; nada con aspecto válido |
| verify falla | ninguna escritura |
| preflight de restore falla | ninguna entrada gestionada reemplazada; puede crearse `daemon.lock`, y la sonda SQLite puede checkpointear marcos WAL confirmados (el mismo estado lógico) y dejar un `-shm` vacío |
| rollback backup falla | destino intacto; journal eliminado |
| staging falla (copia, hash, ENOSPC, EXDEV) | destino intacto; staging eliminado |
| Ctrl+C antes de `swapping` | aborta limpio, destino intacto |
| Ctrl+C durante `swapping`/`swapped` | se ignora: la sección crítica termina o hace rollback |
| verificación final falla | rollback automático (exit 6) o fallo explícito (exit 7) |
| crash del proceso en cualquier fase | el journal dice la fase; `ao backup recover` resuelve |
| prune falla | cada borrado es `rename` a `.deleting-*` y luego `RemoveAll`: los backups restantes quedan intactos |

**EXDEV.** Nunca se hace `rename` entre filesystems: el backup se escribe en
`<root>/.staging-*` y se promueve dentro de `<root>`; el restore **copia** el
backup al staging del destino y promueve dentro del data dir. Si un `rename`
devuelve `EXDEV` igualmente (punto de montaje dentro del data dir), se falla
cerrado con `cross_device`, sin fingir atomicidad.

**fsync.** Archivo + directorio padre en: cada asset del backup, `manifest.json`,
la promoción del backup, el journal en cada cambio de fase, el staging del
restore, el swap y el rollback. No en temporales sin valor durable.

### Crash durante restore y arranque del daemon

| Fase del journal | `ao backup recover` | Arranque del daemon |
| --- | --- | --- |
| `preparing`, `rollback_ready`, `staged` | elimina journal y staging (no hubo swap) | **permitido** (staging viejo inofensivo) |
| `rolled_back` | elimina journal y work dir | permitido |
| `swapping`, `swapped`, `rolling_back`, `rollback_failed` | rollback idempotente | **rechazado**: estado ambiguo |
| `complete` sin `failed/` | elimina journal y `previous/` | permitido |
| `complete` **con** `failed/` (un rollback lo contradijo) | rollback idempotente | **rechazado** |
| journal ilegible, con claves duplicadas, symlink o no regular | se niega a adivinar: inspección manual | **rechazado** |

El daemon comprueba el journal **después** de tomar `daemon.lock`, así que nunca
lo lee mientras un restore vivo lo escribe. `recover` **no abre la base**: abrirla
para sondear borraba el `-shm` vacío que el rollback había devuelto y hacía
fallar para siempre la comprobación (revisión independiente); usa la lista de
descriptores del SO, que no muta nada. Probado con crash real tras **cada**
rename del swap, antes de `complete` y tras un rollback terminado, con `recover`
×3 (el 2.º y 3.º no mueven nada).

## 19. Política de identidad y de clave

**installation_id.** Restore es recuperación de desastre **de la misma**
instalación:

| backup | destino | resultado |
| --- | --- | --- |
| `B` | sin identidad | se instala `B` |
| `B` | `B` | igual |
| `B` | `D ≠ B` | **rechazo** `installation_mismatch`, salvo `--identity=backup` (adopta `B`) o `--identity=destination` (conserva `D`: restore como copia) |
| sin identidad | cualquiera | se conserva la del destino |

Restaurar `B` no permite adoptar runtimes creados **después** del backup: P9
exige que el token de propietario del runtime coincida con el lanzamiento que la
fila de la sesión registra, y la base restaurada no contiene esos lanzamientos.
Los runtimes vivos del futuro quedan sin fila → nunca adoptados (test
`TestRestoredStateNeverOwnsARuntimeLaunchedAfterTheBackup`).

**secret.key.** Filas durables selladas con ella (AES-256-GCM, `secretbox`):
`app_settings.smtp_password_encrypted`, el `api_token_encrypted` de work items y
`skill_secrets.sealed_value` (secretos de skills **y** credenciales de registros
privados). `provider_profiles.secret_ciphertext` existe pero ningún proveedor lo
escribe. Un backup P10 **no es autocontenido** respecto a esas filas (opción C):
la clave queda fuera a propósito, y la semántica es fail-closed (opción B):

- el manifest lleva sólo la huella `SHA-256("ao.backup/v1 secret-key\0"‖clave)`
  truncada a 128 bits: estable, no reversible, nunca la clave ni su base64;
- si el backup tiene huella y el destino tiene otra clave **o ninguna**, restore
  se niega con `secret_key_mismatch`, nada cambia;
- `--allow-secret-key-mismatch` restaura con un aviso explícito: los datos no se
  borran pero esas filas quedan ilegibles hasta reintroducirlas (probado con el
  `secretbox` real: ilegibles con la clave nueva, legibles al devolver la
  original);
- `ao backup create` recuerda que la clave no va en el backup. **Respalda
  `secret.key` aparte, con su propia protección.**

Nunca hay un restore RESTORED silencioso con filas cifradas irrecuperables.

## 21-22. Compatibilidad de esquema

Sea `N` la versión goose del backup y `M` la cabeza de migraciones embebida en el
binario:

- `N == M` → `compatible`, restore permitido.
- `N < M` → `upgrade_required`, restore permitido; el **arranque normal** migrará.
  Se avisa explícitamente. Recomendado: `ao backup create` tras restaurar y
  antes de arrancar.
- `N > M` → `newer_than_binary`, verify exit 5, **restore rechazado**. No hay
  downgrade.

## 28. Retención

`ao backup prune [--keep N] [--max-age D] [--apply]` — **dry-run por defecto**.

- Candidatos: sólo backups `aob-*` finalizados, `kind=manual`, con manifest legible.
- Se borra un candidato sólo si está fuera de los `N` más recientes (defecto 10)
  **y**, si se dio `--max-age`, es más antiguo.
- **Nunca**: `pre-restore`, `pre-migration`, el backup válido más reciente, uno
  con lock tomado (en uso por un restore), entradas que no son `aob-*`.
- Staging huérfano (lock libre) y `.deleting-*` se limpian con `--apply`.
- Quien tiene un lock (`.locks/<id>.lock`) nunca borra su archivo: borrarlo con
  otro proceso esperando dejaría a dos "dueños" del mismo backup. El restore
  bloquea el backup origen en **su** raíz, sea cual sea `--root`. `prune --apply`
  barre los lock files de backups que ya no existen.

## 30. Estado operativo

`ao backup list` muestra por backup: id, kind, fecha UTC, tamaño, goose,
compatibilidad, checks de creación, estado (`ok`/`in_progress`/`incomplete`/`deleting`/`invalid_manifest`/`unsupported`);
y al final, de `operations.jsonl`: último backup, último restore y su backup de
rollback.

## 31. Hook de backup previo a migración (no activado)

No se activa en P10: añadiría minutos de `VACUUM INTO` (≈900 MB) a un arranque
que el supervisor Electron espera en segundos y podría encadenar backups en
bucles de arranque. Integración futura: en `daemon.RunWithConfig`, tras
`daemonlock` y antes de `sqlite.Open`, leer la versión goose con
`immutable=1`; si `< MigrationHead()`, llamar a `backup.Create` con
`Kind=pre-migration` (ya protegido por prune) y fallar cerrado si no verifica.

## Códigos de salida

| Código | Significado |
| --- | --- |
| 0 | éxito / `VALID` compatible o `upgrade_required` |
| 1 | fallo interno o de E/S |
| 2 | uso incorrecto |
| 3 | rechazado por seguridad (`daemon_active`, `daemon_unverified`, `daemon_ambiguous`, `data_dir_locked`, `db_in_use`, `backup_in_use`, `no_rollback_possible`, `installation_mismatch`, `secret_key_mismatch`, `source_inside_destination`, `destination_inside_source`, `destination_damaged`, `insufficient_space`, `restore_interrupted`, `unsafe_path`, `invalid_backup_root`) |
| 4 | backup `INVALID` |
| 5 | backup `UNSUPPORTED` o `newer_than_binary` |
| 6 | `RESTORE_FAILED_ROLLED_BACK` |
| 7 | `RESTORE_FAILED_ROLLBACK_FAILED` |

`--json` en todos los subcomandos devuelve el informe con `status`/`result` y
`reasons[].code`.

## I. Excluido intencionadamente

Secretos (`secret.key`, `cli-credentials.json`, `agent-credentials/`,
`users/`, Keychain), worktrees, logs, `running.json`, locks, sockets tmux,
cachés derivadas, prompts/transcripts fuera de la base, `node_modules`.
No hay firma criptográfica. No hay historial en la base (sin migración). No hay
soporte Windows probado: `rename` sobre archivos abiertos y `flock` difieren; se
compila, no se promete.

## Runbook

### Confirmar que AO está parado

```bash
ao status --json          # state debe ser "stopped" (o "stale")
lsof -nP -iTCP:3002 -sTCP:LISTEN   # vacío
```

Si `ao status` dice `running_unverified` o `unhealthy`: **no** mates nada a
ciegas. Comprueba el PID con `ps -p <pid> -o command=`; si no es AO, el run-file
es viejo — muévelo a mano sólo después de confirmarlo.

### Backup normal

```bash
ao backup create                 # online, con o sin daemon
ao backup create --note "antes de X" --json
```

### Verificar un backup / identificar uno seguro

```bash
ao backup list
ao backup verify ~/.ao/backups/aob-…           # exit 0 = VALID y restaurable
```

Seguro = `VALID`, compatibilidad `compatible` (o `upgrade_required` si aceptas
migrar al arrancar), `kind` y fecha esperados.

### Comprobar goose

```bash
ao backup verify <path> --json | jq '.gooseVersion, .binaryHead, .compatibility'
sqlite3 "file:$HOME/.ao/data/ao.db?mode=ro&immutable=1" \
  "SELECT MAX(version_id) FROM goose_db_version WHERE is_applied=1"   # sólo con AO parado
```

### Restaurar tras un fallo

```bash
ao stop
ao backup restore ~/.ao/backups/aob-…
ao start
```

La salida nombra el backup de rollback `pre-restore` creado.

### Restaurar cuando la base actual está corrupta

```bash
ao stop
ao backup restore ~/.ao/backups/aob-…      # exit 3 destination_damaged
ao backup restore ~/.ao/backups/aob-… --preserve-broken-state
```

La salida nombra `~/.ao/backups/aof-…` (`forensic_copy`). Para volver al estado
dañado (p. ej. para forense): con AO parado, copia de vuelta al data dir los
archivos que lista `forensic.json` (`ao.db`, laterales, `installation_id`,
`skills/catalog`) retirando antes los actuales. No es un backup: `ao backup
restore` no la acepta.

### Si restore dice `recoverRequired` / "run `ao backup recover`"

No arranques AO; ejecuta `ao backup recover` (idempotente: repetirlo no mueve
nada). Si `db_in_use`: cierra el proceso que nombra y repite.

### Rollback manual

```bash
ao stop
ao backup restore <ruta del backup pre-restore que imprimió el restore>
```

### Si AO no arranca

1. Lee el error del daemon. Si menciona un restore interrumpido:
   `ao backup recover`.
2. `ao backup verify` del último backup; restáuralo.
3. Si el error es de migración: restaura el `pre-restore`/último backup y usa
   un binario cuya cabeza goose sea ≥ la del backup.

### Qué NO borrar

- `~/.ao/data/secret.key`, `installation_id`, `ao.db`.
- Cualquier `pre-restore` hasta haber confirmado que el estado restaurado es el correcto.
- `<data>/.ao-restore-journal.json` y `<data>/.ao-restore-*/` — usa `ao backup recover`.
- Las copias históricas `pre-*` en `~/.ao/backups/` (P10 no las gestiona).

## J. Contrato operativo para P11 (soak)

```bash
# Antes del soak (AO parado o vivo):
ao backup create --note "p11-t0" --json > p11-t0.json
ao backup verify "$(jq -r .path p11-t0.json)"

# En 24h / 48h / 72h, con AO vivo (snapshot online):
ao backup create --note "p11-t24" --json > p11-t24.json
ao backup verify --json "$(jq -r .path p11-t24.json)" > p11-t24-verify.json

# Salud de la base entre snapshots (sin tocar la base viva):
jq '.status, .compatibility, .checkMode, .integrity, .reasons' p11-t24-verify.json

# Comparar estado entre snapshots (sobre las copias, nunca la base viva):
for t in t0 t24; do
  sqlite3 "file:$(jq -r .path p11-$t.json)/ao.db?mode=ro&immutable=1" \
    "SELECT 'workflow_runs', COUNT(*) FROM workflow_runs UNION ALL
     SELECT 'sessions', COUNT(*) FROM sessions UNION ALL
     SELECT 'change_log', COUNT(*) FROM change_log"
done

# Tras un crash intencionado:
ao stop                      # o confirmar que el daemon murió
ao backup recover            # sólo si hay un restore interrumpido
ao backup restore "$(jq -r .path p11-t24.json)"
ao start
```

## Evidencia P10

Todas las pruebas construyen su propia instalación scratch; ninguna abre, copia ni
restaura `~/.ao`. El único acceso a la base real fue la medición opt-in de solo
lectura (`mode=ro&immutable=1`), con comprobación antes/después.

### Mapa de pruebas

| Propiedad (DoD) | Prueba |
| --- | --- |
| A/B snapshot consistente, WAL sin checkpoint capturado sin copiar `-wal` (§51) | `TestCreateCapturesUncheckpointedWALWithoutCopyingIt` |
| Escritor concurrente: prefijo consistente, integridad y FK (§35) | `TestCreateUnderConcurrentWritesIsConsistent` |
| Creates concurrentes sin colisión (§34) | `TestConcurrentCreatesNeverCollide` |
| Fallo/Ctrl+C en create: nada válido, origen intacto (§38, §65) | `TestCreateFailureOrCancelKeepsNothingAndTouchesNoSource` |
| Raíz de backup peligrosa (§72) | `TestCreateRefusesADangerousRoot` |
| Symlinks en origen y destino (§40) | `TestCreateRefusesSymlinksInTheSource`, `TestRestoreRefusesUnsafePaths` |
| Permisos 0700/0600 (§39) | `TestCreatePermissions`, `TestRestoreRoundTripReturnsStateA` |
| Privacidad (§42) | `TestBackupCarriesNoSecrets` |
| Hash en streaming (§59) | `TestVerifyHashesByStreaming` (base de 39 MB, pico de heap +1–4 MiB) |
| C manifest versionado, rutas (§41, §45) | `TestParseManifest*`, `TestManifestAssetRules`, `TestValidateAssetPath`, `TestBackupIDsAreSortableAndDistinct`, `TestJournalRejectsPathsOutsideTheManagedEntries` |
| D corrupción (§25): 18 casos fallan cerrados y el restore no toca el destino | `TestCorruptBackupsFailClosedAndNeverRestore`, `TestVerifyWritesNothing` |
| E/F daemon activo, lock, conexión abierta | `TestRestoreRefusesWhenAODaemonIsNotProvenStopped`, `TestRestoreRefusesWhenTheDataDirOrDatabaseIsHeld` |
| G/H/I/J round trip, pre-restore verificado, WAL/SHM viejos (§23, §24, §52) | `TestRestoreRoundTripReturnsStateA`, `TestRestoreLeavesNoStaleWALOrSHM`, `TestSwapAndRollbackAreExactAndIdempotent` |
| K auto-rollback y rollback fallido recuperable (§27, §55) | `TestRestoreAutoRollbackAfterTheSwap`, `TestRestoreRollbackFailureIsExplicitAndRecoverable` |
| Inyección de fallos antes del swap (§26) | `TestRestoreFailuresBeforeTheSwapLeaveTheDestinationUntouched` |
| EXDEV (§70) | `TestRestoreAutoRollbackAfterTheSwap/promotion_crosses_a_device` |
| Cancelación (§38) | `TestRestoreCancellation` |
| Crash en create y restore, journal, arranque (§77–§79) | `TestRollbackFromEveryPartialSwap` (cada rename), `TestCrashDuringRestoreIsAlwaysResolvable` (6 puntos, proceso hijo real con `os.Exit`), `TestCrashDuringCreateNeverLooksValid` |
| O esquema anterior sin migrar / posterior rechazado (§56, §57) | `TestRestoreOlderSchemaIsNotMigrated`, `TestRestoreNewerSchemaFailsClosed`, `TestMigrationHeadIsTheVersionAFreshDatabaseReaches` |
| Identidad y clave (§19) | `TestRestoreInstallationIdentityPolicy`, `TestDecideIdentity`, `TestRestoreSecretKeyPolicy` |
| Runtime posterior al backup nunca adoptado (§20) | `TestRestoredStateNeverOwnsARuntimeLaunchedAfterTheBackup` |
| P retención (§28, §58, §65) | `TestPruneDeletesOnlyCandidates`, `TestPruneNewestAndMaxAge`, `TestPruneFailureLeavesOtherBackupsIntact` |
| CLI: daemon vivo (ambas convenciones), no verificado, unhealthy, lock, stale, confirmación, códigos de salida, guard de import | `TestBackupCLI_*`, `TestRestoreCLI_*`, `TestHoldDataDirOfflineRefusesAHeldDataDir`, `TestBackupExitCodes` |

### E2E con el daemon real (`AO_P10_E2E=1`)

| Prueba | Resultado |
| --- | --- |
| `TestP10_OnlineBackupRestoreRefusedWhileRunningThenRestoredAfterStop` — backup online válido; `ao restore` con daemon vivo → exit 3 `daemon_active`, daemon intacto; tras `ao stop` → RESTORED, fila posterior al backup desaparece, sin `-wal`/`-shm`; el daemon arranca sobre el estado restaurado | PASS (3.9 s) |
| `TestP10_RestoreRefusesAnUnverifiedLivePID` — PID vivo con probe de otra identidad → exit 3 `daemon_unverified`, proceso sin señal, run-file intacto; tras resolverlo → restore OK | PASS (1.0 s) |
| `TestP10_DaemonRefusesToBootOverAnInterruptedRestore` — journal en `swapping` → `ao daemon` se niega nombrando `ao backup recover`; recover → `RECOVERED_ROLLED_BACK`; el daemon arranca | PASS (1.1 s) |

### Gates

| Gate | Resultado |
| --- | --- |
| `gofmt -l` | limpio |
| `go build ./...`; `GOOS=linux`/`GOOS=windows go build ./internal/backup/` | OK |
| `go vet` backup, backup/e2e, cli, daemon | OK |
| `go test` backup, cli, daemon, daemonlock, runfile, daemonmeta, telemetrymeta | OK |
| `go test ./internal/storage/sqlite/` (suite completa) | OK (38.5 s) |
| `-race` backup (186 s, incluye crashes en proceso hijo), daemonlock, telemetrymeta, cli (backup/restore/import/guard/discovery/stop, 48 s) | OK, sin data races |
| Race amplio de workflow | no ejecutado: P10 no toca workflow/lifecycle/recovery |
| golangci-lint v2.12.2 | 28 incidencias = base `847c47808`, diff vacío (delta 0) |
| Frontend / OpenAPI | sin cambios de API: no aplica |

### Mediciones (§36, §59)

Base sintética de 449 MiB (`AO_P10_BENCH=1`, macOS arm64, SSD interno):

| Operación | Tiempo | Pico de heap sobre base | Pico de disco extra |
| --- | --- | --- | --- |
| `create` | 2.4 s | +4.2 MiB | +448 MiB (1× snapshot) |
| `verify` completo | 0.86 s | +4.0 MiB | 0 |
| `verify --quick` | 0.47 s | +4.0 MiB | 0 |
| `restore` (incluye pre-restore + staging + verificación) | 6.1 s | +6.1 MiB | +896 MiB (≈2×) |

maxrss del proceso: 51 MiB. El swap son renames: `previous/` no ocupa espacio extra.

Base real de 866 MiB, **solo lectura** (`AO_P10_REAL_DB`, `mode=ro&immutable=1`,
copia en el scratchpad y borrada después): `VACUUM INTO` 5.1 s → 811 MiB;
`integrity_check` + `foreign_key_check` + goose sobre la copia 10.4 s: `ok`, 0
violaciones, goose 170; SHA-256 del origen 0.9 s. Estimación para la base real:
`create` ≈ +811 MiB de disco, `restore` ≈ +1.7 GiB de pico; libres 66 GiB.

### Base real antes / después

| | Antes | Después |
| --- | --- | --- |
| Ruta | `~/.ao/data/ao.db` | igual |
| Tamaño | 908 447 744 | 908 447 744 |
| mtime | 2026-09-13 19:37:21 | 2026-09-13 19:37:21 |
| SHA-256 | `7ee966e6a7dfa5a1dc8b9860cd6cd478dbb3c0a4637a5f52ff01263b59a4e03f` | idéntico |
| goose | 170 | 170 |
| `-wal`/`-shm`/`-journal` | ausentes | ausentes |

AO real al final: parado (puerto 3002 libre, sin run-files, sin daemon, sin
Electron, sin servidor tmux). `~/.ao/backups` histórico sin cambios.

### Revisión adversarial interna (§82)

Revisión independiente de sólo lectura sobre `847c47808..HEAD` (un agente
aparte, sin builds ni acceso a `~/.ao`), con los puntos de §82. Sin ningún camino
de pérdida silenciosa de la base original; confirmados como seguros: mismo
camino/anidamiento/symlinks, manifest y journal manipulados, ventana del daemon
(daemon.lock retenido hasta el final, rollback incluido), WAL/laterales,
convergencia ante crash en cada rename, EXDEV/ENOSPC, staging y prune,
cancelación, permisos y secretos, clasificación de resultados y puerta de arranque.

Hallazgos y disposición — todos corregidos, cada uno con su prueba:

| # | Severidad | Hallazgo | Corrección | Prueba |
| --- | --- | --- | --- | --- |
| F1 | media | Si fallaba escribir `complete` en el journal se informaba RESTORED con el journal en `swapped`, y el `recover` recomendado deshacía el restore | *(Sustituido por R2 de la revisión independiente: ahora rollback, nunca exit 0)* | `TestRestoreCompletionThatCannotBeJournaledIsNeverReportedRestored` |
| F2 | media | Sin `ao.db` no hay backup pre-restore, pero el `skills/catalog` (y la identidad con `--identity=backup`) se borraban al terminar | Rechazo `no_rollback_possible` | `TestRestoreRefusesToReplaceWhatADatabaselessDataDirAloneHolds` |
| F3 | media | `ao import` y el backfill escribían sobre un restore interrumpido a mitad del swap (escritura perdida o recuperación rota) | El guard offline aplica la puerta de arranque | `TestHoldDataDirOfflineRefusesAnInterruptedRestore` |
| F4 | baja | Mensajes "el data dir no cambió" imprecisos (la sonda puede checkpointear; el lector deja laterales vacíos) | Redacción exacta en código y documento | revisión de texto |
| F5 | baja | Borrar el lock file al soltarlo permitía dos dueños; el origen fuera de `--root` no se bloqueaba | Los lock files no los borra quien los tiene; el origen se bloquea en su raíz; prune barre huérfanos | `TestRestoreLocksTheSourceInItsOwnRoot`, `TestLockFilesStayWithHoldersAndPruneSweepsOrphans` |
| F6 | baja | Recover seguía symlinks dentro del work dir | Rechazo antes de mover nada | `TestRecoverRefusesToMoveFilesThroughASymlinkedWorkDir` |

### Revisión de integración independiente (P10 review)

Revisión adversarial independiente de `674fa2c28` antes del merge en ECC, sin
confiar en el informe anterior: lectura de todo el código, experimentos con
procesos reales (`sqlite3`, crashes con `os.Exit`, journal inmutable con
`chflags`), inyección de fallos en cada escritura del journal y en cada rename.

| # | Severidad | Hallazgo (reproducido) | Corrección | Prueba |
| --- | --- | --- | --- | --- |
| R1 | **BLOCKER** | Un `sqlite3` abierto y ocioso (sin lock) pasaba las dos sondas; tras el swap su escritura creaba un WAL que se adjuntaba a la base **restaurada** y la corrompía (`database disk image is malformed`) con el restore informado RESTORED. Igual si se abría dentro de la ventana | Cuarta capa: descriptores abiertos del SO antes del pre-restore, antes del swap y **después de los renames** sobre `previous/` (cierra la ventana; si alguien tiene el inode viejo, rollback); no poder listarlos rechaza | `TestRestoreRefusesAProcessThatOpenedTheDatabaseWithoutReading`, `TestRestoreRollsBackWhenAProcessOpenedTheDatabaseInsideTheWindow`, `TestRestoreFailsClosedWhenOpenFilesCannotBeListed`, `TestOpenHoldersFollowsTheDescriptorAcrossARename` |
| R2 | HIGH | Journal inmutable tras el swap: el rollback borraba su work dir, decía "nada más que hacer", dejaba `swapped` bloqueando cada arranque y un `recover` que sólo podía fallar. Y `complete` no escribible salía 0 | Journal primero y work dir después en abandon/rollback/recover; reescritura a fase asentada; si no, work dir conservado y `recoverRequired`; `complete` no escribible → rollback; `complete` contradicho por `failed/` = no asentado | `TestRestoreJournalFailureMatrix` (8 fases × único/persistente × escrito-o-no), `TestRestoreWhoseJournalCannotBeClearedStaysRecoverable`, `TestACompletionRecordContradictedByARollbackIsNotTrusted` |
| R3 | HIGH | `recover` sondeaba la base abriéndola: borraba el `-shm` vacío preexistente que el rollback había devuelto y `checkRolledBack` fallaba para siempre (`ao.db-shm present=false`) tras cualquier rollback terminado (p. ej. crash durante el rollback) | `recover` usa descriptores del SO (no muta); `-wal`/`-shm` no son estado (el stamp de `ao.db` prueba que no se perdió ningún frame); rollback/recover se niegan si alguien tiene abierta la base restaurada | `TestCrashDuringRestoreIsAlwaysResolvable/after-rollback`, `TestRollbackRefusesWhileTheRestoredDatabaseIsOpen` |
| R4 | BLOCKER operativo | Base destino corrupta (la propia sonda fallaba) o con violaciones FK (sin pre-restore VALID): restore imposible para siempre, justo cuando más se necesita | `destination_damaged` por defecto; `--preserve-broken-state` con copia forense verificada byte a byte (§E2); no cubre espacio/E/S/cancelación | `TestRestoreRefusesADamagedDestinationByDefault`, `TestRestoreWithPreserveBrokenStateKeepsAForensicCopy`, `TestPreserveBrokenStateDoesNotOverrideAnOrdinaryPreRestoreFailure`, `TestRecoverPutsADamagedOriginalBackByteForByte`, `TestRestoreCLI_DamagedDestinationNeedsPreserveBrokenState` |
| R5 | MEDIUM | Data dir (o raíz del pre-restore) dentro del backup origen: aceptado; el restore escribía dentro del backup, que dejaba de verificar | `destination_inside_source` antes de crear nada | `TestRestoreRefusesToWriteIntoItsOwnSource` |
| R6 | LOW | Claves JSON duplicadas: `encoding/json` se queda con la última (un manifest "manual" aquí, "pre-restore" o v2 para otro lector) | Manifest y journal ambiguos → INVALID / no confiable | `TestManifestAndJournalRejectDuplicateKeys` |

Confirmado sin cambios: `VACUUM INTO` abre una única transacción de lectura sobre
`main` durante toda la copia (`sqlite3RunVacuum` → `BtreeBeginTrans(pMain, 0)`
cuando hay `INTO`, en el fuente transpilado de modernc v1.51.0); crash real tras
**cada** rename del swap y antes de `complete` con `recover` ×3 idempotente
(`TestCrashAfterEveryRenameIsRecoveredIdempotently`); journals hostiles
(malformado, campos desconocidos, symlink, directorio, traversal, work dir
ausente) fallan cerrados; online bajo escrituras reales ×5
(`TestOnlineBackupsUnderWritesAreAlwaysSnapshots`); create+prune concurrentes
(`TestCreateAndPruneConcurrently`); privacidad en todos los informes
(`TestNoSecretReachesAnyReport`); identidad × P9
(`TestExplicitIdentityModesNeverOwnTheOtherInstallationsRuntimes`,
`TestP10_RestoreIdentityModesThroughTheBinary`); `secret.key` con el `secretbox`
real (`TestRestoredSealedRowsAreNeverSilentlyUnreadable`,
`TestSecretKeyFingerprintIsOneWayAndStable`).

Gates tras las correcciones (macOS arm64, AO real parado):

| Gate | Resultado |
| --- | --- |
| `gofmt -l`, `go build ./...`, `go vet ./internal/...`, `GOOS=linux go vet` / `GOOS=windows go build` de backup | OK |
| `go test` backup (68.5 s), cli, daemon, daemonlock, runfile, daemonmeta, telemetrymeta, workerownership, secretbox | OK |
| `go test ./internal/storage/sqlite/` | OK (39.7 s) |
| E2E daemon real `AO_P10_E2E=1` (4 pruebas, incluida identidad por el binario) | OK (10 s) |
| `-race` backup (465 s, crashes en proceso hijo y `sqlite3` reales), cli backup/restore/import/guard/backfill/discovery/stop (50 s), daemonlock | OK, sin data races |
| golangci-lint v2.12.2 `--new-from-rev=847c47808` | 0 issues |
| Race amplio de workflow / frontend | no aplica: sin cambios de workflow/lifecycle ni de API HTTP |

### Deudas restantes

- **Hook de backup previo a migración** (§31): documentado, no activado.
- **Exclusión**: depende de `lsof` en macOS/BSD (sistema) o `/proc` en Linux; sin
  ellos restore rechaza. Un proceso de **otro usuario** (root) con la base abierta
  no se ve en Linux. Un proceso que abra la base restaurada durante un rollback lo
  bloquea hasta cerrarlo (explícito, recuperable).
- **Copia forense**: un `.staging-aof-*` huérfano por crash no lo poda `prune`
  (borrado manual); no hay comando para devolverla (runbook manual).
- **Coherencia skills ↔ base**: el backup refleja el catálogo tal cual; filas de
  `skill_installs` cuyo paquete falte en el origen no se detectan al crear (en uso,
  `LoadPackage` verifica el digest y falla cerrado).
- **Colisión de rutas por normalización Unicode** (NFC/NFD en APFS): no se
  detecta en el manifest; en restore el `O_EXCL` del staging falla cerrado.
- **FreeBSD** no compila `fsutil_unix.go` (`Statfs_t.Bavail` con signo); no es
  plataforma objetivo.
- **Windows**: compila, no está probado (`flock`, `rename` sobre abiertos, fsync de
  directorios).
- **`secret.key`** fuera del backup por diseño: el operador debe respaldarla aparte.
- **Worktrees, logins de proveedor y credenciales** fuera del backup por diseño.
- **Firma/autenticidad** de backups: no existe; verify prueba integridad.
- **Recover nunca rueda hacia delante**: un restore interrumpido tras el swap se
  deshace y se repite.
- **Stamp `size+mtime` del journal** en recover: si una sonda de `recover`
  llegara a checkpointear un WAL del estado original, la comprobación falla
  cerrada (`RECOVER_FAILED`) en lugar de aceptar un estado dudoso.
