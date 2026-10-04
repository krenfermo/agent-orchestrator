# E2E del ciclo completo sobre tmux real

*Estado: **implementado** en `test/frente1-tmux-full-cycle-e2e` (base
`fe59cd1fe`). Cierra el elemento «Tests E2E contra tmux real» de
[`roadmap.md`](roadmap.md) §Frente 1.*

```
AO_WORKFLOW_TMUX_E2E=1 go test ./e2e/workflowcycle/ -v -count=1 -timeout 15m
```

Desde `backend/`. Opt-in porque compila `ao` y arranca un daemon. Con la
variable puesta, la falta de `tmux` o `git` es un **fallo**, no un skip.

## 1. Qué es real y qué no

| Pieza | En el test |
| --- | --- |
| Daemon | `ao daemon` compilado desde el árbol, arranque completo (`cmd/ao`) |
| API | HTTP real (`/api/v1/projects`, `/workflows`, `/start`, `/continue`, `/sessions`, `/runtime/gc`) |
| Base de datos | SQLite real con todas las migraciones |
| Runtime | tmux real, servidor privado (`AO_TMUX_SOCKET=ao-cycle-e2e-<pid>-<n>`) |
| Git | repositorio real; worktree aislado real bajo el data-dir |
| Verify | el `verify` de producción ejecuta el comando y los `Verification.Files` |
| GC de runtimes | el `runtimegc.Sweeper` de producción, vía `POST /runtime/gc` |
| **Agentes** | **shim** (`testdata/agent-shim.sh`) instalado como `claude` y `codex` |

Los agentes son lo único sustituido, y sin tocar código de AO. El harness
`fake` de AO no sirve: no está registrado en el daemon y el enrutado de
workflow sólo elige `claude-code`/`codex` (a propósito: el `fake` nunca debe ser
seleccionable en producción). El shim actúa como agente **sólo** por las
superficies que usa un agente real: los hooks nativos (`ao hooks …`), el
veredicto (`ao review submit …`) y el propio workspace. Con la política por
defecto, el worker es `claude-code` y el reviewer es `codex`: el test ejercita
la configuración real de producción.

P11 descartó los shims en PATH por inseguros **contra datos reales**. Aquí no
hay datos reales:

- `HOME`, `TMPDIR`, `CODEX_HOME`, data-dir (DB y worktrees), run-file, puerto,
  socket tmux y repositorio son temporales (`/tmp/aoce-*`, fuera de `~/.ao`).
- El `PATH` del daemon es sólo `<bin privado>:/usr/bin:/bin:/usr/sbin:/sbin`.
  Ningún directorio con un `claude` o un `codex` reales está en él.
- `ANTHROPIC_API_KEY` ficticia: el sondeo de credenciales de Claude se resuelve
  por entorno y nunca toca el keychain. No se llama a ningún modelo.
- El entorno del daemon **no lleva variables de locale**, como el de un daemon
  lanzado por launchd o por la app de escritorio (ver §4).
- La limpieza (`t.Cleanup`) mata el daemon hijo y el servidor tmux privado,
  borra el socket y el directorio scratch, y falla si queda alguna sesión. Está
  probada también forzando un fallo con una sesión tmux viva.

## 2. Contrato

### Escenario 1 — `TestWorkflowCycleCompletesThroughRealTmux`

Run `task`, `reviewDepth: light`, `placement: isolated_worktree`, verify con un
comando (`grep`) y dos `Verification.Files` (uno con `exactContent`).

| # | Aserción |
| --- | --- |
| C1 | El run se crea y arranca por la API HTTP |
| C2 | Exactamente un intento de worker. La sesión tmux lleva `AO_SESSION_OWNER=ao-session:<sesión>:<launch>`, y `AO_INSTALLATION_ID`/`AO_DAEMON_INSTANCE_ID` iguales a los que publica `/readyz` |
| C3 | El agente corrió **dentro** del servidor tmux privado (`$TMUX`, pane y sesión en la traza) y es el shim del bin scratch |
| C4 | El worker corrió en un worktree aislado bajo el data-dir y produjo el cambio |
| C5–C6 | `work` → `review` → `verify` en ese orden; exactamente un reviewer, veredicto `approved`, enviado por `ao review submit` para el `reviewRunId` del step |
| C7 | Verify: un intento, `succeeded` |
| C8 | `completed`, sin pasar por `needs_attention`/`failed`. Entregable **commiteado** en la rama `ao/*` (padre = base, `src/cycle.txt` con el contenido exacto, worktree limpio). El checkout del proyecto queda intacto |
| C9 | Sin worker ni reviewer duplicado (dos sesiones tmux en total durante el run). Ninguna sesión sobrevive a la terminación más el barrido de GC |
| C10 | La sesión tmux del worker desaparece al completar, y `GET /sessions/<id>` la da por terminada |

### Escenario 2 — `TestIgnoredContractualDeliverableNeverSpawnsThroughRealTmux`

El BLOCKER de la auditoría de `feat/gitignored-deliverable-preflight`:
`src/cycle.txt` observable y `out/report.pdf` requerido por
`Verification.Files` bajo la regla `out/`.

- El run aparca en `needs_attention` y el intento de `work` lleva
  `deliverable_not_observable`.
- Tras un `continue` del operador, el rechazo se mantiene.
- En ningún momento el run pasa por `completed`.
- Ningún agente corre y **nunca existe una sesión tmux**. Ninguna rama `ao/*`
  avanza.

## 3. Evidencia de una ejecución

tmux 3.7b, macOS. Escenario 1 ≈ 5 s, escenario 2 ≈ 11 s (incluye 10 s
esperando tras el `continue`).

```
waiting    plan:completed work:completed review:pending ... verify:pending
completed  plan:completed work:completed review:completed ... verify:completed
```

La traza del worker registra `argv0=<scratch>/bin/claude`,
`tmux=/private/tmp/tmux-501/ao-cycle-e2e-…`, `tmux_session=cycle-e2e-1`,
`pwd=<scratch>/data/worktrees/cycle-e2e/cycle-e2e-1`. La del reviewer registra
`argv0=<scratch>/bin/codex`, `review_submit=ok`, en el mismo worktree.

## 4. Defectos de producción que encontró

Ninguno de los dos era visible para los tests unitarios. Los dos se corrigen en
esta rama, cada uno en su propio commit y con su regresión.

1. **Inventario tmux ciego fuera de un locale UTF-8** (`e65a9b406`).
   `ListSessions` pedía `#{session_id}\t#{session_name}`. Sin variables de
   locale, o con `C`, tmux devuelve el tab como `_`, ninguna línea se parte y el
   inventario sale vacío sin error. Consecuencia: el GC de runtimes nunca veía el
   pane de un reviewer de workflow ya terminado, ni en el barrido periódico ni en
   el de arranque. Lo reproduce `real_tmux_inventory_locale_test.go`, con tmux
   real bajo `LC_ALL=C` y un control negativo.
2. **El `CHECK` de `workflow_attempts.error_class` se había desviado**
   (`9aaa1ed2b`, migración 0171). Siete clases que AO escribe —las cuatro
   `provider_*` del preflight de proveedor, `deliverable_not_observable`,
   `superseded` e `integration_failed`— violaban el `CHECK` en SQLite real. El
   rechazo pre-spawn hacía fallar la operación entera: `StartRun` devolvía 500 y
   el run quedaba `running` en lugar de aparcar. Nunca se lanzaba nada, pero la
   parada legible no llegaba. Hay un test anti-deriva que extrae del código Go
   todas las clases declaradas y exige que el esquema migrado acepte cada una.

## 5. Límites y deuda

- **El pane del reviewer sobrevive a la terminación hasta el siguiente barrido
  de GC** (≤ 15 min, `runtimeGCInterval`). `completeRun` recoge al instante los
  runtimes de los *steps* (el worker), pero el reviewer de workflow no guarda su
  sesión en el step y queda en manos del GC. Con la corrección 1 el GC lo
  recoge. El test lo registra y dispara el barrido en lugar de esperar 15 min.
  Recogerlo en `reclaimTerminalRuntimesForRun` es un cambio de ciclo de vida
  aparte.
- Los agentes son shims. Se prueba el sistema AO completo, no el comportamiento
  de Claude Code ni de Codex.
- Una sola ruta feliz y un rechazo. Ciclos de fix, `changes_requested`,
  failover de proveedor y reinicio del daemon a mitad de ciclo no están
  cubiertos aquí.
- `go test -timeout` que expira aborta el proceso sin ejecutar `t.Cleanup`. Por
  eso cada espera del test tiene su propio plazo (≤ 3 min), muy por debajo del
  timeout recomendado.
