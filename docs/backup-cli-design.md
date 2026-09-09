# `ao backup` / `ao restore` — diseño

*Estado: **diseño**. Nada de esto está implementado. El procedimiento manual que sí está probado vive en [`backup-restore.md`](backup-restore.md).*

## Inventario: qué ocupa la base realmente

Medido sobre `~/.ao/data/ao.db` (789 MB) con `dbstat`, sólo tamaños, sin leer
contenido:

| Objeto | MB | Nota |
| --- | --- | --- |
| `code_graph_edges` + sus 4 índices | 404 | **Derivado**: se reconstruye desde el repositorio |
| `code_graph_symbols` + sus 3 índices | 158 | **Derivado** |
| `change_log` + `idx_change_log_project` | 193 | CDC, 1.254.211 filas |
| `project_memory_files` | 3 | |
| Todo lo demás | ~31 | |

**El code graph es ~562 MB, el 71 % de la base.** `change_log` es el 24 %. Todo
lo operativo — 116 runs, 112 sesiones, 3.383 checkpoints, 3.395 eventos de uso,
124 review runs — cabe holgadamente en el 5 % restante.

> **Corrección al roadmap v1.1.** Allí propuse podar `conversation_*`. Están
> **vacías** (0 filas las tres). La propuesta era incorrecta y esta medición la
> sustituye.

### Consecuencia para el diseño

Son dos problemas distintos y no se resuelven igual:

- **El code graph es caché derivada.** No es un problema de retención sino de
  ubicación: se reconstruye desde el repositorio, así que un respaldo no
  necesita llevarlo. Excluirlo reduce el respaldo en ~71 % sin perder nada
  irrecuperable.
- **`change_log` es CDC con ventana natural.** Lo consumen el poller y el
  broadcaster; las filas ya entregadas y más antiguas que el replay de
  `Last-Event-ID` no las lee nadie. Es el único candidato real a retención, y
  aun así **no se poda nada hasta que exista política aprobada.**

## `ao backup`

```
ao backup [--out DIR] [--include-derived] [--json]
```

- Snapshot online con `VACUUM INTO` — transacción de lectura, una instantánea
  consistente, no modifica el origen, no requiere parar el daemon.
- **Por defecto excluye el code graph** (`--include-derived` lo incluye). Se
  registra en el manifiesto que va excluido y que se reconstruye solo.
- Verifica **antes de dar el respaldo por bueno**: `integrity_check`,
  `foreign_key_check`, versión de esquema y apertura como store de AO. Un
  respaldo que no pasa las cuatro se marca como fallido y no se cuenta.
- Escribe un **manifiesto** junto al archivo:

  ```json
  {
    "createdAt": "...", "aoVersion": "...", "schemaVersion": 160,
    "sizeBytes": 0, "durationMs": 0, "sha256": "...",
    "includesDerived": false,
    "verification": {"integrity": "ok", "foreignKeys": "ok", "opensAsStore": true},
    "config": {"variables": ["AO_DATA_DIR", "AO_OIDC_CLIENT_SECRET_FILE"]},
    "worktrees": [{"project": "...", "path": "...", "branch": "..."}]
  }
  ```

  **`config.variables` lleva NOMBRES, nunca valores.** Ningún secreto entra en
  el manifiesto, en el archivo ni en los logs. Las credenciales de agente no se
  respaldan: se revocan y se vuelven a emitir.

## `ao restore`

```
ao restore <archivo> --data-dir DIR --confirm
```

- **Sólo offline.** Se niega si hay un daemon vivo (comprueba el run-file y el
  PID) en vez de competir con él.
- **Se niega a escribir sobre un directorio de datos existente** salvo
  `--force`, y `--force` exige que el destino tenga respaldo propio previo.
- Valida el archivo **antes** de tocar el destino: sha256 contra el manifiesto,
  `integrity_check`, `foreign_key_check`, versión de esquema.
- **Rollback**: el directorio destino anterior se mueve a
  `<dir>.pre-restore-<timestamp>` y no se borra nunca. Si el arranque posterior
  falla, se restituye moviendo de vuelta.
- Nunca restaura `running.json`, `~/.ao/electron/` ni credenciales vivas.

## Retención

```
ao backup --retain <n>     # conserva n respaldos
```

- **Configurable, sin borrado automático.** Por defecto no borra nada: informa
  de cuántos respaldos hay y cuánto ocupan, y deja el borrado al operador.
- El borrado automático sólo se habilita cuando exista una política aprobada, y
  aun entonces nunca elimina el último respaldo verificado bueno.

## Métricas

Tamaño, duración, resultado de las cuatro verificaciones, y si incluyó lo
derivado. Números y estados; sin rutas de proyecto, sin nombres de rama, sin
contenido.

## Retención de `change_log` — propuesta, NO aprobada

**No se implementa ni se ejecuta nada sobre datos reales todavía.**

Dependencias que hay que resolver antes de proponer un número:

1. Qué ventana necesita el replay de `Last-Event-ID` del stream SSE.
2. Si algún consumidor relee `change_log` fuera de esa ventana.
3. Si hay obligación de auditoría sobre esas filas — **pregunta abierta al
   usuario**: AO no tiene hoy política de retención legal ni operativa escrita.
4. Si podar requiere `VACUUM` del origen para devolver espacio (lo requiere), y
   que eso necesita una ventana de parada.

Sólo con (1)–(4) respondidas tiene sentido proponer una ventana concreta.

## Orden de implementación

1. `ao backup` con las cuatro verificaciones y el manifiesto.
2. `ao restore` offline con confirmación y rollback.
3. `--retain` informativo, sin borrar.
4. Métricas.
5. Retención de `change_log` **sólo tras aprobación explícita**.
