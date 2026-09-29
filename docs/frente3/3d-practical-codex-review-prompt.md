# Prompt de revisión adversarial REAL: PRECONDITION_3D_PRACTICAL

Revisa estáticamente si la especificación Practical responde de manera válida
a la pregunta interna de ingeniería de AO:

> ¿Project Memory ASSISTED reduce M1u, M2 y/o M3 frente a OFF sin degradar
> Q1/Q4/Q6 bajo condiciones controladas y representativas de AO?

## Alcance autoritativo

Lee únicamente:

- `docs/frente3/3d-practical.md` — protocolo práctico;
- `docs/frente3/06-benchmark-plan.md` — única función total normativa.

`3d-auth-design-v4.md`, su prompt y `3d-preflight.md` son historial no
normativo. No importes reglas de ellos.

## Ataque requerido

Busca contraejemplos concretos que impidan responder la pregunta comparativa:

- mismatch OFF/ASSISTED antes de start y anomalías post-start;
- identidad content-addressed, manifest incompleto o schedule alterable;
- enum/transition table contradictorio o decisión parcial;
- reemplazos, relots, reruns selectivos u omisión de posiciones;
- diferencia no intencional entre arms o contaminación local entre posiciones;
- contexto externo, Router, MCP/apps/web/global memory no equalizados;
- treatment trace incompleta o atribución incorrecta de representación;
- provider cache confundido con aislamiento o con efecto de tratamiento;
- caps por role que no se validan, accounting de retries/partial calls;
- failures, sanction y bloqueos que puedan mejorar artificialmente un arm;
- Q1/Q4/Q6 que permitan degradación escondida o corrección post-hoc;
- decisión que requiera juicio manual después de ver resultados;
- claims por encima de fixture, cuenta, proveedor, modelo y ventana observados.

Verifica explícitamente que sean imposibles estos seis ataques de la revisión
REAL previa:

A. Sustituir el attachment ASSISTED y conservar el mismo `experiment_id`.
B. Introducir un solo failure y aun así obtener GO.
C. El adapter elimina/cambia el attachment después de que la trace lo midió.
D. Un helper hace un provider call sin contabilizarlo.
E. El schedule correlaciona arm con orden temporal/warm-up.
F. El mismo manifest permite comportamientos distintos por defaults
   implícitos del provider, SDK o cliente.

Para cada ataque, indica el campo/regla exactos que lo bloquean. En particular,
comprueba que `decide()` compara el schedule completo regenerado desde PRNG,
versión y seed congelados; que las 40 posiciones deben estar `COMPLETED` para
GO; y que toda invocación SDK tiene una correspondencia 1:1 por
`(sample_id, attempt_id, call_index)` entre attempt, trace row y
usage/accounting row.

No ejecutes nada ni edites archivos. No propongas gates o experimentos externos.

No exijas aislamiento universal del proveedor, G4/G6 Research-Grade, VMs,
organizaciones independientes, ni experimentos risk/sanction, billing, parent,
contract, device, TLS o routing. Puedes identificar una omisión en esas áreas
sólo si presentas una cadena causal concreta que invalide la comparación
OFF-versus-ASSISTED especificada. No basta con que reduzca certeza sobre el
aislamiento interno del proveedor; esa incertidumbre es una limitación
aceptada como `RESIDUAL_CONFOUNDER`.

## Formato de respuesta

Resume primero el alcance y evidencia inspeccionada. Luego lista hallazgos
ordenados por severidad; cada hallazgo debe incluir ubicación, escenario
reproducible, efecto sobre la pregunta de ingeniería y corrección mínima.
Separa defectos estáticos de limitaciones aceptadas. No reabras V4 ni agregues
G0–G9, VMs, gateway/broker experimental, organizaciones experimentales o
aislamiento Research-Grade del provider, salvo que demuestres un camino causal
concreto que invalide una de las comparaciones especificadas. Si no hay hallazgos,
explícalo brevemente. No conviertas limitaciones ya declaradas en defectos sin
demostrar cómo sesgan o invalidan la comparación.

Termina con exactamente una línea:

```text
PRECONDITION_3D_PRACTICAL = READY_FOR_CODEX_REVIEW | NEEDS_CHANGES | NO-GO
```

`READY_FOR_CODEX_REVIEW` sólo significa que esta revisión estática no encontró
defectos que invaliden la comparación especificada; no autoriza implementar,
ejecutar el lote, activar Project Memory ni hacer rollout.
