# Respaldo y restauración de AO

*Estado: procedimiento diseñado y probado (`backend/internal/storage/sqlite/backup_restore_test.go`). **No hay automatización todavía**: no existe comando `ao backup`, ni cron, ni retención.*

## Lo que había antes de este documento

Nada. No hay `VACUUM INTO` en el árbol, no hay comando de respaldo y no hay
retención para la base. Lo que existe en una máquina real son copias hechas a
mano — `ao.db.backup-before-0119-20260821-110009`,
`data.backup-recovery-hardening-20260825-100114` — tomadas con `cp` contra un
daemon vivo.

**Un `cp` de una base WAL bajo un escritor vivo no es un respaldo.** La copia
puede caer entre la escritura de una página y su marco en el WAL, y restaurar
como una base corrupta o silenciosamente atrasada. No hay aviso hasta que hace
falta.

## El procedimiento

`VACUUM INTO` es la respuesta de SQLite y no necesita cooperación del daemon ni
ventana de parada:

- corre dentro de una transacción de lectura, así que ve **una** instantánea
  consistente pase lo que pase mientras dura;
- escribe una base nueva y compactada, no una copia de páginas;
- **no modifica el origen**.

```bash
# Con el daemon corriendo. No requiere pararlo.
sqlite3 "file:$HOME/.ao/data/ao.db?mode=ro" \
  "VACUUM INTO '$HOME/ao-backups/ao-$(date -u +%Y%m%dT%H%M%SZ).db'"
```

### Qué más hay que guardar

La base no basta para reconstruir una instalación:

| Elemento | Dónde | Nota |
| --- | --- | --- |
| Base SQLite | `~/.ao/data/ao.db` | Vía `VACUUM INTO`, nunca `cp` |
| Configuración OIDC | variables `AO_OIDC_*` | **Guarda los NOMBRES, nunca los valores.** El secreto vive en su archivo `0600` y se respalda aparte, con su propia protección |
| Referencias de worktrees | `git worktree list` por proyecto | Son rutas y ramas; el contenido vive en los repos, no en AO |
| Credenciales de agente | `~/.ao/data/agent-credentials/` | Se revocan y se vuelven a emitir; **no** se restauran |

### Qué NO se restaura

- **`running.json`** — es un handshake PID/puerto de un daemon que no está
  corriendo después de una restauración. Restaurar uno es como un CLI acaba
  hablándole a un puerto que no es de nadie. Hay un test que fija esto.
- **`~/.ao/electron/`** — caché de Chromium, cookies, almacenamiento local. Se
  regenera.
- **Credenciales vivas** — de agente, de CLI y de sesión. Después de una
  restauración se vuelve a autenticar.

### Restaurar

```bash
# Con el daemon PARADO y sobre un directorio de datos NUEVO, nunca encima del real.
mkdir -p ~/.ao-restore/data
cp ao-20260909T013000Z.db ~/.ao-restore/data/ao.db
chmod 600 ~/.ao-restore/data/ao.db
AO_DATA_DIR=~/.ao-restore/data ao start
```

Se verifica en este orden, y una restauración no cuenta como buena hasta que
los cuatro pasan:

1. `PRAGMA integrity_check` → `ok`
2. `PRAGMA foreign_key_check` → sin filas
3. `SELECT MAX(version_id) FROM goose_db_version` → **igual** que el origen. Si
   subió, la restauración volvió a correr migraciones y eso no es la misma base.
4. El daemon abre el store sin replay.

## Qué está probado

`backup_restore_test.go`, sobre una base que el propio test construye — **nunca
sobre `~/.ao/data/ao.db` del operador**:

| Prueba | Propiedad |
| --- | --- |
| `TestBackupUnderConcurrentWritesRestoresConsistent` | Instantánea tomada **mientras** un escritor inserta continuamente: restaura con `integrity_check` ok, sin violaciones de FK, sin filas huérfanas, y el origen queda intacto |
| `TestRestoredBackupOpensAsALiveStore` | El archivo restaurado abre como store de AO, conserva la **misma versión de esquema** (no re-migra) y conserva los datos |
| `TestBackupExcludesTransientRunState` | `running.json` no vive en el directorio de datos, así que el procedimiento debe excluirlo explícitamente |

## Límites conocidos, sin adornos

- **No está automatizado.** Hoy es un comando que alguien tiene que ejecutar.
- **No hay retención ni compactación.** `ao.db` pesa **823 MB** en la máquina de
  referencia. `VACUUM INTO` compacta la copia, pero nada compacta el origen ni
  poda historia (`change_log`, `conversation_*`, `model_usage_events`).
- **Un respaldo sin restauración probada no es un respaldo.** El punto 3 de la
  verificación existe porque una base que re-migra al abrirse no es la que se
  respaldó.
- **No cubre los repositorios de trabajo.** AO guarda referencias a worktrees,
  no su contenido. El código vive en sus propios repos y en su propio remoto.

## Pendiente

1. `ao backup` / `ao restore` como comandos, con las cuatro verificaciones dentro.
2. Política de retención y poda de las tablas de historia.
3. Un respaldo programado, con verificación automática de restauración.
