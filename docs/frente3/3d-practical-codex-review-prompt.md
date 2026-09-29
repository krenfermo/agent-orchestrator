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
Separa defectos estáticos de limitaciones aceptadas. Si no hay hallazgos,
explícalo brevemente. No conviertas limitaciones ya declaradas en defectos sin
demostrar cómo sesgan o invalidan la comparación.

Termina con exactamente una línea:

```text
PRECONDITION_3D_PRACTICAL = READY_FOR_IMPLEMENTATION | NEEDS_CHANGES | NO-GO
```

`READY_FOR_IMPLEMENTATION` sólo significa que el diseño puede pasar a una
decisión separada de implementación; no autoriza ejecutar el lote, activar
Project Memory ni hacer rollout.
