# Deliverable observability — the pre-dispatch `.gitignore` check

*Estado: **implementado** en `feat/gitignored-deliverable-preflight` (base
`6ac0af793`). Subalcance P0/P1 identificado en
[`roadmap.md`](roadmap.md) §Frente 1 como «siguiente subalcance recomendado».*

## 1. El incidente

El 2026-09-09 un run de MEDUSA se bloqueó en `ambiguous_worker_state` porque el
entregable pedido vivía en una ruta cubierta por `.gitignore`. AO acertó —no
había cambio verificable en git— pero el diagnóstico costó una intervención
humana completa, porque **AO no distingue «el worker no hizo nada» de «el worker
produjo algo que git no puede ver»**. Las dos situaciones presentan lo mismo: un
worker inactivo sobre un árbol sin cambios.

Son indistinguibles *después* y perfectamente distinguibles *antes*: el plan
dice qué rutas debe producir la tarea y el repositorio dice cuáles ignora.

## 2. Por qué la parada es inevitable, no probable

Una vez que el entregable requerido está ignorado, dos cosas ya están decididas:

- **El clasificador de completitud sólo mira git** (`HeadSHA` contra el
  `BaseSHA` del checkpoint, más estado sucio/untracked). Una ruta ignorada no
  mueve ninguno, así que una tarea *mutating* que produjo exactamente lo pedido
  se lee como una tarea que no produjo nada.
- **El trabajo tampoco sobreviviría.** `StashUncommitted` y
  `MaterializeIntegrationCommit` construyen su commit con `git add -A` sin `-f`
  (`backend/internal/adapters/workspace/gitworktree/workspace.go`), que honra
  `.gitignore`: el entregable se descarta en silencio del commit de preservación
  y del de integración.

No es una heurística sobre lo que *podría* salir mal. Es AO negándose a empezar
un trabajo que ya ha probado que no podría ni observar ni conservar.

## 3. La regla

El chequeo corre en `attemptWorkHarness`, justo después del preflight de
proveedor y antes de cualquier spawn (`backend/internal/workflow/dispatch.go`).
Para una tarea *mutating* que declara al menos un entregable requerido, rechaza
en cualquiera de **dos** condiciones:

> 1. **Contractual.** Alguna ruta de `Verification.Files` con `Exists: true`
>    está ignorada (en todas sus lecturas, ver abajo).
> 2. **Todos.** Todos los entregables requeridos (contractuales o de prosa)
>    están ignorados — la forma exacta de MEDUSA.

La diferencia entre ambas es la diferencia entre lo que el plan **afirmó** y lo
que AO **leyó** en prosa:

- Un `Verification.Files` con `Exists: true` es contractual. Si está ignorado,
  el commit de integración (`git add -A`, sin `-f`) lo descarta mientras
  `verify`, que lee el sistema de ficheros del worktree, lo da por bueno: AO
  declararía éxito habiendo perdido un entregable requerido. Que existan otros
  entregables observables no lo cambia; una tarea que exige A+B no es segura
  porque sólo A sea preservable. *(Corrige el BLOCKER de la auditoría
  pre-merge: la versión inicial sólo rechazaba cuando **todo** estaba
  ignorado.)*
- Una ruta encontrada **sólo** en la prosa de un criterio no adquiere esa
  fuerza. «El build escribe `dist/app.js` y los tests pasan» menciona una salida
  de build ignorada sin exigir que se commitee. Una ruta así sólo contribuye a
  un rechazo cuando nada más de lo que la tarea exige es observable.

Cuando rechaza por la condición contractual, el detalle nombra sólo las rutas
contractuales ignoradas; cuando rechaza por «todos», las nombra todas.

**Lecturas de una ruta contractual.** `verifyFile` resuelve una ruta relativa
en el espacio de nombres donde corren los comandos de su spec
(`verifyPathContextFor`) y, si no existe ahí, cae a la lectura desde la raíz del
repositorio (`verify.go`). El preflight pregunta a git por ambas y considera la
ruta oculta sólo si **todas** sus lecturas están ignoradas: rechazar por la
lectura que `verify` quizá no use sería fundar un rechazo en una conjetura.

Cada cláusula restante elimina una forma de equivocarse:

| Cláusula | Qué evita |
| --- | --- |
| **mutating** | Una tarea declarada `read_only` no debe producir nada; su éxito *es* el árbol intacto (`read_only_completion.go`). `unspecified` se trata como mutating, como en todo AO. |
| **al menos uno** | La mayoría de tareas no nombran ninguna ruta. Rechazarlas apagaría el producto. |
| **prosa sin fuerza contractual** | Rechazar por cualquier mención textual ignorada pararía tareas sanas que citan de pasada una salida de build, que es una frase común y no un defecto. |

### Qué cuenta como entregable requerido

1. **`Verification.Files` con `Exists: true`** — el plan nombrando
   estructuralmente un fichero que debe existir después. Sin inferencia.
2. **Rutas nombradas por los criterios de aceptación** — los criterios *son* los
   requisitos en vigor (`effective_task_specification.go`), así que una ruta que
   uno de ellos nombra es una ruta requerida. La única inferencia es la
   extracción, que reutiliza el extractor conservador del paquete
   (`extractPaths`, «todo lo que no tiene claro lo descarta») más una única
   ampliación: extensiones de artefacto (`.pdf`, `.csv`, `.xlsx`, `.log`…) que
   el clasificador de scope deliberadamente no admite. Esa ampliación es local
   al chequeo: `codeExtensions` gobierna la estimación de write-scope y la
   detección de conflictos de todo plan, y ensancharla serializaría trabajo que
   no colisiona.

Las URL se eliminan del texto antes de tokenizar: el tokenizador compartido
parte por el separador de esquema, así que `https://example.com/report.pdf`
llegaría como `example.com/report.pdf` —con barra y extensión de artefacto— y
sería, por todas las reglas, una ruta del repositorio. No lo es.

## 4. La respuesta de git es la respuesta de AO

El adaptador (`backend/internal/adapters/workspace/gitignoreprobe`) ejecuta una
sola orden de fontanería, read-only:

```
git -C <repo> check-ignore -v -z --stdin
```

Tres decisiones son toda la corrección del chequeo:

- **No se pasa `--no-index`.** Sin él git omite por completo las rutas
  **trackeadas**, que es la respuesta correcta: git ve los cambios de un fichero
  trackeado por mucho que un patrón lo cubriera. Con `--no-index` AO rechazaría
  dispatches sobre trabajo perfectamente visible.
- **Un patrón que empieza por `!` es una negación.** `check-ignore -v` reporta la
  regla negadora como coincidencia igual que cualquier otra; leerla como
  «ignorado» invertiría todas las excepciones de todos los `.gitignore` del
  repositorio.
- **Exit 1 significa «ninguna está ignorada»**, no error. Sólo ≥2 lo es.

Rutas inexistentes se responden por su nombre, que es la situación en la que el
chequeo siempre corre: el entregable aún no se ha producido.

## 5. Lo que nunca hace

Tres propiedades son portantes, y son deliberadamente las mismas tres sobre las
que descansa `provider_preflight.go`:

- **Sin probe cableado no hay rechazo.** Es exactamente el comportamiento previo.
- **Un probe que falla tampoco rechaza.** «AO no pudo preguntarle a git» es
  *unknown*, y fundar un rechazo en un unknown sería peor que el incidente.
- **Nunca edita el `.gitignore` de nadie.** Hacer `git add -f` por cuenta de una
  persona, o reescribir sus reglas para que el dispatch siga, es la misma clase
  de error que responder el prompt de confianza de un proveedor. Reporta; decide
  la persona.

## 6. La parada

Clase de error `deliverable_not_observable`, razón de atención homónima, nunca
reintentable (esperar no edita un `.gitignore`) y nunca sujeta a failover de
proveedor. Es clase propia y no un sabor de `ambiguous_worker_state` porque es
el tipo de afirmación opuesto: aquélla es AO admitiendo que no puede probar qué
pasó, *a posteriori*; ésta es AO probando *por adelantado* exactamente qué
habría fallado. El detalle nombra cada ruta, la regla que la esconde en formato
`fichero:línea:patrón`, el requisito que la pidió, y los dos remedios.

## 7. Límites conocidos

- **Sólo prosa que el extractor reconoce.** Un criterio que nombra un entregable
  en una forma que el extractor conservador no admite (p. ej. `out/report`, sin
  extensión) no se detecta. El fallo es hacia el comportamiento previo.
- **El scope durable de la tarea (`workflow_tasks.scope_json`) no se consulta.**
  Es una estimación (`WorkflowTaskScopeEstimated`) y fundar un rechazo en una
  estimación es precisamente lo que este repositorio evita en otros sitios.
- **`deliverable_not_observable` no está en el `enum` del DTO de attempt**
  (`httpd/controllers/workflow.go`), igual que `provider_auth_interactive` e
  `invalid_placement` tampoco lo están. Añadirlo exige regenerar el contrato
  OpenAPI y `schema.ts`; queda como deuda declarada, no como olvido.
- **La re-resolución de módulo Go en tiempo de verify no se replica.** Si un
  comando Go de la spec declara un directorio que no está dentro de ningún
  módulo, `verify` lo mueve a la raíz de módulo descubierta al ejecutarse; el
  preflight sólo conoce el directorio declarado. En ese caso raro puede
  preguntar por una lectura distinta de la que `verify` acabará usando.
- **Una ruta sólo de prosa ignorada junto a otras observables** no se reporta.
  Es deliberado (ver §3); si debe preservarse, el plan tiene que declararla en
  `Verification.Files`.
- **Un `.gitignore` que cambia después del dispatch** no se re-evalúa. El
  chequeo es de pre-dispatch por diseño.
