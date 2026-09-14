# P10 — Backup / Restore / Operations

*Estado: implementado en `feat/p10-backup-restore-operations`. Sustituye la
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
3. origen fuera del data dir destino (`source_inside_destination`);
4. descubrimiento P9 sobre ambos run-files: `ready`/`not_ready`/`unhealthy` →
   `daemon_active`; `running_unverified` → `daemon_unverified`; dos daemons →
   `daemon_ambiguous`. `stale`/`foreign`/`stopped` permiten seguir. **Nunca se
   envía una señal ni se borra un run-file**;
5. `flock` de `<data>/daemon.lock` (el mismo lock P9 del daemon), retenido
   durante todo el restore: ningún daemon puede arrancar a mitad (`data_dir_locked`);
6. sonda SQLite exclusiva sobre `ao.db` (`db_in_use`);
7. `ao.db`/`installation_id`/`skills/catalog` del destino no son symlinks;
8. política de identidad y de clave (§19);
9. espacio libre: tamaño del backup (staging) + tamaño actual (rollback) + 64 MiB
   (`insufficient_space`).

**Ejecución:**

1. journal `<data>/.ao-restore-journal.json` (fase `preparing`, fsync);
2. **backup de rollback** `pre-restore` del estado actual, verificado. Si falla →
   `rollback_backup_failed`, destino intacto;
3. staging en el **mismo filesystem** del destino
   (`<data>/.ao-restore-<id>/staged`): copia en streaming con SHA-256 comparado
   con el manifest, `fsync`, permisos 0600; `quick_check` + goose del staged;
4. segunda sonda exclusiva;
5. fase `swapping`: mover a `previous/` las entradas actuales (`ao.db`,
   `ao.db-wal`, `ao.db-shm`, `ao.db-journal`, `installation_id`,
   `skills/catalog`); comprobar que no queda ningún lateral SQLite; promover las
   entradas staged con `rename`; `fsync` del directorio;
6. fase `swapped`: **verificación final** — sin laterales, SHA-256 y tamaño de
   `ao.db` iguales al manifest, `quick_check`, goose igual, identidad y archivos
   de skills correctos;
7. éxito → fase `complete`, se elimina `previous/` y el staging, se elimina el
   journal. **El backup de rollback se conserva.**

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
- El algoritmo es idempotente: para cada entrada, si su copia staged ya no está
  en `staged/` fue promovida; si su original está en `previous/`, vuelve.

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

Tres capas, todas reutilizadas de P9 / del backfill; ningún sistema de locks nuevo
para el data dir:

| Capa | Qué prueba | Fallo |
| --- | --- | --- |
| Descubrimiento P9 (probe `/healthz` con PID + instance + data dir) | que ningún daemon de esta instalación está vivo o vivo-sin-verificar | `daemon_active`, `daemon_unverified`, `daemon_ambiguous` |
| `flock` en `<data>/daemon.lock` retenido todo el restore | que ningún daemon P9 lo tiene, y que ninguno puede arrancar durante el restore | `data_dir_locked` |
| Sonda SQLite exclusiva, antes del rollback y antes del swap | que **ninguna** conexión (daemon, sqlite3, otro `ao`) tiene la base abierta | `db_in_use` |

`ao import` y `ao usage backfill-cache-ttl` pasan a usar el mismo guard offline
(ambos run-files + `daemon.lock` + sonda) — cierra la deuda P9 de import.

Riesgo residual, documentado: un proceso **no AO** que abra `ao.db` entre la
segunda sonda y el `rename` (ventana de milisegundos). AO no puede excluir a un
`sqlite3` manual que no respeta `daemon.lock`.

## H. Semántica de fallos

| Fallo | Garantía |
| --- | --- |
| backup create falla / Ctrl+C | origen intacto; staging eliminado; nada con aspecto válido |
| verify falla | ninguna escritura |
| preflight de restore falla | destino intacto (sólo se crea `daemon.lock`, el lock P9) |
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
| `preparing`, `rollback_ready`, `staged` | elimina staging y journal (no hubo swap) | **permitido** (staging viejo inofensivo) |
| `swapping`, `swapped`, `rolling_back`, `rollback_failed` | rollback idempotente | **rechazado**: estado ambiguo |
| `complete` | limpia `previous/` y journal | permitido |

El daemon comprueba el journal **después** de tomar `daemon.lock`, así que nunca
lo lee mientras un restore vivo lo escribe.

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

**secret.key.** Si el backup registró una huella y la clave del destino no
coincide (o falta), restore se niega con `secret_key_mismatch`: esas tres
columnas cifradas serían ilegibles. `--allow-secret-key-mismatch` lo acepta
explícitamente (los datos no se borran; hay que volver a introducir SMTP, token
de work items y secretos de skills). **Respalda `secret.key` aparte, con su propia
protección**; P10 no la incluye.

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

## 30. Estado operativo

`ao backup list` muestra por backup: id, kind, fecha UTC, tamaño, goose,
compatibilidad, checks de creación, estado (`ok`/`incomplete`/`invalid_manifest`);
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
| 3 | rechazado por seguridad (`daemon_active`, `daemon_unverified`, `daemon_ambiguous`, `data_dir_locked`, `db_in_use`, `installation_mismatch`, `secret_key_mismatch`, `source_inside_destination`, `insufficient_space`, `restore_interrupted`, `unsafe_path`) |
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

### Rollback manual

```bash
ao stop
ao backup restore ~/.ao/backups/aob-…-pre-restore-backup-id
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
jq '.status, .compatibility, .checks' p11-t24-verify.json

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

## Evidencia

Ver la sección "Evidencia P10" al final (tests, E2E, medidas) — se completa con
los resultados de las gates.
