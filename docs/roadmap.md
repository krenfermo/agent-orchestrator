# AO — hoja de ruta consolidada

**Versión 1.1 · 2026-09-09 · base `feat/engineering-control-center` @ `43883a5c2`**

> **v1.1** — Integrados los cinco lotes temáticos en ECC (`44589cbc5`, y los
> merges de los lotes 2–4), más la limpieza de lint `43883a5c2`. Registradas las
> cuatro decisiones del usuario. **Nada empujado a `origin`; `main` sigue sin tocar.**

Este documento **no sustituye** a los planes que ya existen en el repositorio y
no reclama ser el original. Consolida y da estado a lo que ya está escrito:

| Fuente existente | Qué aporta |
| --- | --- |
| [`plans/engineering-control-center-master-plan.md`](plans/engineering-control-center-master-plan.md) | El plan maestro (887 líneas): inventario, arquitectura objetivo, Checkpoints 0–11, Fase 0, riesgos y preguntas abiertas. **Es la fuente de la numeración por checkpoints.** |
| [`STATUS.md`](STATUS.md) | El libro mayor de lo que está en `main` y lo que está en vuelo. |
| `p0b`, `p0c`, `p0d`, `p1`, `p1b`, `p1c`, `p1d`, `p2-project-memory-audit` | Los documentos de fase, cada uno con su propio `Status:`. |
| [`adr/0001-lan-listener-for-mobile.md`](adr/0001-lan-listener-for-mobile.md), [`adr/0002-secure-interactive-reviewer-gateway.md`](adr/0002-secure-interactive-reviewer-gateway.md) | Las dos decisiones arquitectónicas registradas. |

Lo que este documento añade es lo que ninguna de esas fuentes tiene: **un estado
por elemento respaldado por commits, pruebas o evidencia operativa**, agrupado en
los nueve frentes, con dependencias, criterios de aceptación, riesgos y el
siguiente subalcance.

## Decisiones tomadas (2026-09-09)

1. **Integración por tramos temáticos con gates de validación.** Hecho para este
   lote: cuatro tramos, cada uno con build, vet y pruebas propias.
2. **Respaldo restaurable y procedimiento de recuperación ANTES del soak 24 h.**
   Procedimiento diseñado y probado — `docs/backup-restore.md`, rama
   `feat/backup-restore`. El soak queda bloqueado hasta que exista respaldo
   automatizado.
3. **Frente 9 (arquitectura empresarial) queda en diseño** hasta estabilizar.
4. **Congelar funcionalidad nueva en ECC** tras integrar estas correcciones.
   Skills sigue aislado en `feat/skills-catalog-foundation` y **no** forma parte
   de este lote.

## Cómo leer los estados

Cinco estados, deliberadamente distintos. Un elemento no avanza de estado por
tener diseño ni por tener tests que simulan su entorno.

| Estado | Significado exacto |
| --- | --- |
| **Implementado** | El código existe en una rama. No dice nada sobre dónde está esa rama. |
| **Integrado** | Está mergeado en la rama de integración (`feat/engineering-control-center`). **No está en `main`.** |
| **Activado** | Está encendido en tiempo de ejecución por defecto. Muchas cosas integradas están detrás de un flag apagado. |
| **Probado** | Tiene pruebas que ejercitan el mecanismo real, no un doble. Se anota qué prueba y con qué alcance. |
| **Pendiente** | No hay código, o sólo hay diseño. |

### La distinción que más importa hoy

`feat/engineering-control-center` está **303 commits por delante de `main`**, y
`main` está a 0 commits de ECC. Todo P4 (SSO, RBAC, multi-tenancy,
notificaciones, Plane, GitHub/Project intelligence, Project Memory), todo P5
hasta el momento y el code graph están **integrados en ECC y ausentes de
`main`**. Ninguna release los tiene.

**Consecuencia:** «integrado» en este documento significa *integrado en ECC*.
La deuda de integración hacia `main` es, en volumen, el mayor riesgo abierto del
proyecto y no tiene fase asignada. Ver [Frente 0](#frente-0--deuda-de-integración).

---

## Frente 0 — Deuda de integración

No estaba en la lista de nueve frentes y va primero porque condiciona a todos.

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| ECC → `main` | **Pendiente** | `git rev-list --count main..feat/engineering-control-center` = 303 |
| Ramas mergeadas en ECC y no en `main` | **Integrado (ECC)** | 28 ramas de feature mergeadas en ECC, incluidas `p4a-sso-oidc`, `p4b-users-teams-rbac`, `p4c-multitenancy` |
| Lote de correcciones 2026-09-09 | **Integrado (ECC)** | `fix/oidc-secret-out-of-process-env`, `fix/migration-rebuild-fk-gate`, `feat/planner-rejected-result-evidence`, `fix/workflow-attention-i18n`, `docs/consolidated-roadmap` — cuatro merges `--no-ff` + `43883a5c2` |
| Ramas listas y **no** integradas | **Implementado** | `feat/backup-restore` (respaldo restaurable), `feat/skills-catalog-foundation` (aislada por decisión), `fix/new-session-naming` |

**Criterio de aceptación:** una release desde `main` que arranque, migre una
`ao.db` real y pase el gate de `npm run lint` + `go test -race`.

**Riesgo:** cuanto más crece ECC, menos revisable es su merge. El incidente 0160
ocurrió dentro de ECC; un merge de 303 commits a `main` concentra ese tipo de
riesgo en un solo evento.

**Siguiente subalcance:** decidir política — merge por tramos temáticos con gate
de arranque real por tramo, o congelar ECC y estabilizar. **Es una decisión del
usuario, no técnica.**

---

## Frente 1 — Confiabilidad

Planner, worker lifecycle, placement, recovery, tmux E2E, sqlc y migraciones.

| Elemento | Estado | Evidencia | Pendiente |
| --- | --- | --- | --- |
| Placement aislado y bloqueos | **Integrado + probado** | `p1d-placement-locks-and-failover.md` (*implemented, closed*) | — |
| No degradar a `direct_branch` en silencio | **Integrado** | Clase `invalid_placement`, migración 0160 | Verificar en un proyecto configurado `isolated_worktree` |
| Worker lifecycle, generación/attempt, CAS, ownership | **Integrado** | `fix/placement-worker-lifecycle`, `fix/agent-identity-review-recovery` | — |
| Credenciales de worker y revocación | **Integrado** | `fix/agent-credential-revocation` | — |
| Recuperación de sesiones y tareas | **Integrado** | `autonomous-recovery.md`, `p1b-recovery-and-repair.md` | Riesgo residual de *natural-key adoption* y multi-repo, sin cerrar |
| Detector F2 de resultado de planner | **Integrado + activado + probado en producción** | `consistency.go`; disparó correctamente el 2026-09-09 en MEDUSA y se recuperó solo en 1 reintento | — |
| Evidencia de resultado rechazado | **Integrado (ECC)** | `04751b186`, merge lote 3 | Activo en el próximo arranque |
| Migraciones: gate de rebuild con FKs entrantes | **Integrado + probado** | `027a21f99`, merge lote 2; suite sqlite en verde | — |
| Verify estructurado, evidencia pre-review | **Integrado** | `pre-review-evidence.md`, `feat/proportional-execution-review` | — |
| **Tests E2E contra tmux real** | **Parcial** | `p0c-runtime-evidence.md` cubre tmux real | Cobertura E2E del ciclo completo workflow→worker→review→verify sobre tmux real: **pendiente** |
| **Soak 24 h** | **Pendiente** | `p0d-reliability-validation.md` define el criterio | No hay evidencia de una corrida completa reciente |

**Riesgo abierto verificado:** el 2026-09-09 un run de MEDUSA se bloqueó en
`ambiguous_worker_state` porque el entregable pedido vivía en una ruta cubierta
por `.gitignore`. AO acertó (no había cambio verificable en git) pero el
diagnóstico costó una intervención humana completa. **AO no distingue «el worker
no hizo nada» de «el worker produjo algo que git no puede ver».**

**Siguiente subalcance P0/P1 recomendado:** detectar en el dispatch que una ruta
declarada en los criterios de aceptación está git-ignorada, y decirlo antes de
gastar el turno. Coste bajo, evita exactamente el bloqueo observado.

---

## Frente 2 — Skills y seguridad a demanda

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Catálogo de Skills | **Implementado, no integrado** | rama `feat/skills-catalog-foundation` (trabajo de otro frente, no auditado aquí) |
| Auditoría de seguridad por proyecto a demanda | **Pendiente** | — |
| Manifiesto de capacidades y permisos mínimos | **Pendiente** | — |
| Pentest activo con autorización explícita | **Pendiente** | — |

**Dependencia dura:** el gateway de capacidades de reviewers interactivos
([ADR 0002](adr/0002-secure-interactive-reviewer-gateway.md)) ya establece que
*un prompt no es una frontera de seguridad* y que los adaptadores experimentales
son host-trusted. Cualquier Skill con permisos debe colgar de ese contrato, no
inventar otro.

**Criterio de aceptación:** seleccionar MEDUSA y lanzar una auditoría completa
sin que ningún otro proyecto sea auditado, con reporte reproducible
(hallazgo, severidad, evidencia, falsos positivos).

**Riesgo:** convertir cada tarea normal en un pentest. El diseño debe ser
opt-in por proyecto y por ejecución.

---

## Frente 3 — Grae/Graphify, Project Memory y contexto incremental

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Project Memory (P2-A) | **Integrado, activado `off` por defecto** | `project-memory.md`; `AO_MEMORY_MODE=off\|assisted\|preferred` |
| Memoria en el ciclo de ejecución (P2-B) | **Integrado + medido** | 87.209 → 40.378 bytes (−53,7 %) en `preferred` sobre este repo |
| Shared task knowledge (P2-C) | **Integrado + medido** | 32 hechos compartidos en el mismo módulo, 0 en otro módulo |
| Code graph | **Integrado** | `backend/internal/codegraph`, migración 0153 |
| Context router | **Integrado, `off` por defecto** | `contextrouter`, flag `AO_CONTEXT_ROUTER` |
| **Grae/Graphify como proveedor real** | **Pendiente** | Rama `feat/grae-graphify-memory` mergeada en ECC, pero **tener un contrato preparado no es tener un proveedor integrado** |

**Nota de honestidad exigida:** Grae/Graphify **no** debe marcarse integrado. Lo
que existe es el contrato/adaptador; falta evaluarlo como proveedor real con
medición frente a la lectura completa del repositorio.

**Criterio de aceptación:** un primer análisis produce inventario y grafo; los
commits siguientes actualizan incrementalmente; el router entrega sólo lo
necesario; y hay medición de tokens, cache hits y ahorro frente al baseline.

**Riesgo:** introducir un segundo sistema de memoria en paralelo. La regla
vigente es que la memoria es *caché, nunca fuente de verdad*.

---

## Frente 4 — Tokens, costos, presupuestos, routing y capacidad

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Contabilidad de uso (P3-E) | **Integrado** | `usage-accounting.md`; migraciones 0149/0150; separa coste facturado, tokens de contexto y trabajo útil |
| Usage por planner invocation | **Integrado + activado** | `usage_bindings.subject_kind='planner_invocation'` |
| Capacity Scheduler | **Parcial** | `p1c-capacity-and-runtime-gc.md` (*implemented*) cubre capacidad y GC de runtime |
| Presupuestos, alertas y límites | **Pendiente** | — |
| Routing por complejidad y failover | **Integrado** | `p1d`, checkpoint `routing_decision` con `reasonCodes` |

**Pregunta abierta con evidencia real, sin responder:** el 2026-09-09 dos
invocaciones del planner con el mismo objetivo y 3 minutos de diferencia
consumieron **217.533** y **44.857** tokens de input (4,85×). El payload propio
de AO era 81.565 bytes (~20 K tokens); el resto vino del entorno del harness
(`HOME` del usuario: CLAUDE.md global, skills, definiciones MCP). **No hay
instrumentación que separe «contexto que AO envió» de «contexto que el harness
añadió».**

**Siguiente subalcance:** medir y registrar esa diferencia por invocación. Es la
métrica que responde «por qué una tarea trivial consume cientos de miles de
tokens de caché», que hoy no se puede responder.

---

## Frente 5 — Plane/GitHub, CI/CD y herramientas empresariales

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Observer SCM (GitHub) | **Integrado + activado** | `internal/observe/scm`, hechos PR en lifecycle |
| Plane | **Integrado** | `plane-integration.md` (P4-E), `fix/plane-project-id-field` |
| Trazabilidad workitem→run→commit→review→verify | **Parcial** | `workflow_mutation_provenance`, `work_items` (migración 0158) |
| Tracker lane (issues) | **Pendiente en runtime** | `STATUS.md`: el adaptador existe pero **no hace nada en runtime** (#112) |
| Conectores BD/IDE/CI-CD | **Pendiente** | — |
| Oracle/DBA con privilegios mínimos | **Pendiente** | — |

**Criterio de aceptación para Oracle/DBA:** lectura de metadatos y código
fuente con privilegios mínimos, generación de scripts, y **validación humana
obligatoria antes de cualquier ejecución en producción.**

---

## Frente 6 — Runtime GC, backups/DR, soak y recuperación

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Runtime GC | **Integrado** | `p1c-capacity-and-runtime-gc.md` |
| Watchdog/autostart, caffeinate | **Pendiente** | — |
| Backups / DR | **Implementado + probado, no integrado ni automatizado** | `feat/backup-restore` @ `5bc7bfc5d`: `VACUUM INTO` bajo escritura concurrente, restauración verificada (integridad, FK, versión de esquema, apertura como store). **Sigue sin automatizar y sin retención** |
| Soak 24/48/72 h | **Pendiente** | Criterio en `p0d`, sin corrida registrada |

**Riesgo verificado:** `~/.ao/data/ao.db` pesa **823 MB** con un WAL de 4,7 MB.
No hay política de retención ni de compactación documentada. Un rebuild de tabla
sobre esa base es exactamente el escenario del incidente 0160.

**Siguiente subalcance:** política de retención + backup verificado (uno que se
restaure y arranque), antes que el soak. Un soak sin backup restaurable es un
riesgo, no una prueba.

---

## Frente 7 — Board, UX, notificaciones, RBAC y gobierno

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| SSO/OIDC (P4-A) | **Integrado + activado** | `sso-oidc.md`; el daemon corre `AO_AUTH_MODE=oidc` |
| RBAC (P4-B) | **Integrado** | `rbac.md` |
| Multi-tenancy (P4-C) | **Integrado** | migración 0156, `tenant_id` |
| Notificaciones (P4-D) | **Integrado + activado** | `STATUS.md` |
| Board / Control Center | **Integrado** | `presentation.go`, `board.go` |
| Decisiones humanas y auditoría | **Integrado** | `workflow_questions`, `attention.go` |
| **i18n de mensajes de workflow** | **Integrado (ECC)** | `7519689a5`, merge lote 4a; 240/240 ficheros de frontend en verde |
| **Higiene del secreto OIDC** | **Integrado (ECC), pendiente de activar** | `0d289a124`, merge lote 1. Activarlo exige mover el secreto al archivo `0600` y reiniciar el daemon |

**Defecto conocido, sin corregir:** `AGENTS.md` declara como regla dura que el
listener loopback permanece **sin autenticación**. Con `AO_AUTH_MODE=oidc` el
loopback devuelve `401`. La regla y el comportamiento están en contradicción y
una de las dos debe cambiar; hoy rompe el CLI para cualquier operador sin sesión.

**Deuda de i18n:** el fix cubre 6 códigos de razón de worker. Quedan ~50
constantes `Reason*` cuyos mensajes siguen llegando en inglés.

---

## Frente 8 — Runners remotos, Windows/ConPTY, Hetzner, web/iPhone

| Elemento | Estado | Evidencia |
| --- | --- | --- |
| Listener LAN autenticado (móvil) | **Integrado + activado opt-in** | [ADR 0001](adr/0001-lan-listener-for-mobile.md) |
| App móvil (Expo) | **Integrado** | `STATUS.md`, sección Mobile |
| Windows / ConPTY | **Implementado, NO apto para producción** | Falta ownership, InstanceID y recuperación equivalentes al runtime soportado |
| Runners remotos / Hetzner | **Pendiente** | — |
| Acceso web/iPhone sin exponer el daemon | **Pendiente** | — |

**Regla que no debe romperse:** el listener primario sigue en `127.0.0.1`; el
LAN es opt-in, con bearer, sin rutas de control, y por decisión explícita es
texto plano de red doméstica. Cualquier acceso remoto real necesita una decisión
arquitectónica nueva (ADR), no una ampliación de este listener.

---

## Frente 9 — Arquitectura empresarial y transformación digital

Inventario de sistemas, procesos, datos, integraciones, ADRs, portafolio y
evaluación buy/build/AI.

**Estado: pendiente en su totalidad.** No hay código ni diseño en el
repositorio. El plan maestro cubre *Engineering Intelligence* (§7.1, §9) que es
su antecesor natural, pero no el alcance empresarial.

**Dependencia:** requiere Frente 3 (grafo e inventario) y Frente 5 (conectores)
antes de ser algo más que un documento.

---

## Riesgos transversales

1. **Deuda de integración de 303 commits** hacia `main`. El mayor riesgo abierto.
2. **Base de datos de 823 MB** sin retención ni backup verificado.
3. **Contradicción loopback/auth** entre `AGENTS.md` y el runtime.
4. **Pérdida de resultado del proveedor** en el planner: mitigada por F2, no
   evitable por AO. Ocurrió una vez de forma verificada.
5. **Exposición de secretos por entorno de proceso**: corregida para OIDC en
   `0d289a124`; no auditada para el resto de variables `AO_*`.

## Preguntas abiertas para el usuario

1. ¿Política de integración de ECC → `main`: por tramos o congelar y estabilizar?
2. ¿Se prioriza el soak 24 h o backup/retención primero? (Recomendación: backup.)
3. ¿Frente 9 entra en alcance o se aparca hasta tener Frentes 3 y 5?
