# Checklist de release de AO

*Estado: **ninguna release ha pasado este checklist**. Existe para que la primera lo haga.*

Contexto que lo justifica: `feat/engineering-control-center` está **303 commits
por delante de `main`**, y ninguna de las 357 etiquetas del repositorio contiene
ese código. Todo P4 y P5 está integrado y sin publicar.

Cada punto se marca con **evidencia**, no con criterio. Un punto sin salida de
comando registrada no está hecho.

## 1 · Integridad de base y migraciones

- [ ] `PRAGMA integrity_check` = `ok` sobre una copia de una `ao.db` real
- [ ] `PRAGMA foreign_key_check` sin filas
- [ ] Migraciones aplican **sobre una copia de la base real**, no sobre una vacía
- [ ] `TestTableRebuildInventoryIsComplete` y `TestRegisteredTableRebuildsPreserveIncomingReferences` en verde
- [ ] `npm run sqlc` no produce diff (sin drift)
- [ ] `npm run api` no produce diff (spec y `schema.ts` al día)

> El incidente 0160 falló exactamente aquí: las migraciones pasaban sobre una
> base vacía y rompían sobre una real con filas hijas.

## 2 · Arranque y recuperación de sesiones

- [ ] Daemon arranca desde una base restaurada, sin replay de migraciones
- [ ] Sesiones existentes se readoptan tras reinicio
- [ ] Runs con obligación durable pendiente la retoman solos
- [ ] `ao workflow recover status <id>` responde para un run parado

## 3 · Ownership y credenciales

- [ ] Credenciales de agente se emiten, se presentan y se revocan
- [ ] Un worker recuperado tras reinicio tiene identidad propia y demostrable
- [ ] **El client secret OIDC no está en el entorno del daemon** (`ps eww` no lo muestra)
- [ ] Archivo de secreto en `0600`; AO rechaza permisos más laxos
- [ ] Cliente público PKCE sigue funcionando sin secreto

## 4 · Workflows Task / Autonomous / Master

- [ ] Task simple: dispatch → work → review → verify → advance
- [ ] Autonomous: replanifica y avanza sin intervención
- [ ] Master: descompone y crea runs hijo
- [ ] Un turno que no produce cambio verificable para en `needs_attention` **y dice por qué**
- [ ] Placement aislado no degrada a `direct_branch` en silencio

## 5 · Plane / GitHub y controles de escritura

- [ ] Observer SCM ingiere hechos de PR
- [ ] Plane: sólo `Backlog → TODO`; nunca `In Progress`, `Done`, cerrar ni archivar
- [ ] Trazabilidad workitem → run → commit → review → verify
- [ ] Sin push, merge ni release automáticos sin autorización explícita

## 6 · Costos, observabilidad y límites

- [ ] Uso por run, step, agente, proveedor y modelo
- [ ] Planner invocations contabilizados
- [ ] Coste facturado, tokens de contexto y trabajo útil **separados**
- [ ] Presupuestos y alertas *(pendiente — no implementado)*
- [ ] Contexto que envía AO separado del que añade el harness *(pendiente)*

## 7 · Seguridad y aislamiento

- [ ] Listener loopback en `127.0.0.1`
- [ ] Listener LAN sólo opt-in, con bearer, sin rutas de control
- [ ] **Resolver la contradicción**: `AGENTS.md` declara el loopback sin auth y con `AO_AUTH_MODE=oidc` devuelve 401
- [ ] Ningún secreto en argumentos, logs ni entorno de procesos hijo
- [ ] Evidencia de resultado rechazado del planner sin contenido del modelo
- [ ] Estado de `~/.ao` únicamente; nada en `~/Library/Application Support`

## 8 · Pruebas E2E y soak

- [ ] `go build ./...`, `go vet ./...`, `go test ./...` en verde
- [ ] `go test -race` con timeout suficiente (`internal/workflow` necesita ~1200 s)
- [ ] `npm run frontend:typecheck` y suite de frontend en verde
- [ ] `golangci-lint` — **hallazgos preexistentes se reportan en rojo, no se silencian**
- [ ] E2E contra tmux real del ciclo completo *(pendiente)*
- [ ] **Soak 24 h** *(pendiente — y bloqueado por el punto 9)*

## 9 · Rollback y conductor único

- [ ] **Respaldo restaurable verificado antes del soak** (`docs/backup-restore.md`)
- [ ] Restauración probada en un directorio de datos aislado
- [ ] Plan de rollback escrito antes de publicar
- [ ] **Un único conductor de release** — regla dura de `AGENTS.md`
- [ ] macOS: `.zip` + `latest-mac.yml` publicados (electron-updater no instala desde `.dmg`)
- [ ] Artefactos macOS verificados con `frontend/scripts/verify-mac-artifact.sh`, nunca a mano

## Bloqueadores hoy

1. Sin respaldo automatizado ni retención — `ao.db` en 823 MB
2. Sin E2E completo contra tmux real
3. Sin soak 24 h registrado
4. Contradicción loopback/auth entre `AGENTS.md` y el runtime
5. 303 commits sin integrar a `main`
