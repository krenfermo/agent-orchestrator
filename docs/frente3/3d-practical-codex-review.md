# Evidencia histórica — revisión REAL de Codex de 3D-PRACTICAL

Fecha de registro: 2026-09-29
Resultado original: `PRECONDITION_3D_PRACTICAL = NEEDS_CHANGES`

La revisión estática original está preservada en
`~/.ao/scratch/frente3/reviews/3d-practical/codex/review.md`.

SHA-256 del archivo revisado:
`830e5682e89a54f5e00063e4e334d91f8f09fa61141a8b8c58fe92971aac8d94`

## Hallazgos registrados

1. El attachment ASSISTED no formaba parte inequívoca del `experiment_id`.
2. Una asimetría de failures podía satisfacer las medianas y producir GO.
3. La trace usaba una frontera pre-adapter y podía diferir de lo entregado al
   provider client.
4. “Cada request relevante” no garantizaba accounting de todos los attempts y
   roles.
5. El schedule no fijaba PRNG/derivación reproducibles ni pares por task.
6. El manifest no tenía un schema cerrado que impidiera defaults distintos.

La propuesta revisada intenta cerrar esos seis puntos en
[3d-practical.md](3d-practical.md) y
[06-benchmark-plan.md](06-benchmark-plan.md). Esta nota sólo registra el
veredicto y los hallazgos originales; **no es normativa** y no sustituye una
nueva revisión REAL de Codex. Ninguna recomendación Research-Grade se incorpora
por esta evidencia.

## Revisión REAL final y corrección acotada

La revisión final preservada en
`~/.ao/scratch/frente3/reviews/3d-practical-final/codex/review.md` tuvo SHA-256
`7876c5fc97f24e87470c99e9fd691f4d82907b897c6fc9415ec8124d6e6b86eb` y
resultado `PRECONDITION_3D_PRACTICAL = NEEDS_CHANGES`.

Registró cuatro defectos: primary Q6 no identificada, lifecycle de attempts
incompatible con append-only, cardinalidad ambigua de `retry_budgets` y falta
de un digest reproducible del entorno local relevante. La corrección normativa
está en `3d-practical.md` y `06-benchmark-plan.md`; esta nota sigue siendo sólo
evidencia histórica y no declara aprobada la nueva revisión.
