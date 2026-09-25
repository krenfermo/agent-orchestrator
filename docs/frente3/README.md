# Frente 3 — Project Memory / Project Graph

## 3A — Discovery, arquitectura y diseño (2026-09-24)

Baseline: `feat/engineering-control-center` @ `3030d85f2`, producción en
goose 174. Fase sólo de documentación. No hay cambios de runtime, DB ni
dependencias.

| # | Documento | Contenido |
|---|---|---|
| 1 | [01-discovery-report.md](01-discovery-report.md) | Baseline, inventario, estado en producción, medición y respuestas a las 16 preguntas |
| 2 | [02-context-flow-map.md](02-context-flow-map.md) | Flujo de contexto por rol, bypass del Worker, duplicaciones, docs desactualizados |
| 3 | [03-graphify-evaluation.md](03-graphify-evaluation.md) | Qué es Grae/Graphify, ficha upstream, encaje y riesgos |
| 4 | [04-architecture-proposal.md](04-architecture-proposal.md) | Build/integrate/hybrid, modelo, procedencia, incremental, retrieval, presupuesto, agentes, Skills, persistencia, lifecycle, UX |
| 5 | [05-threat-model.md](05-threat-model.md) | Amenazas, controles, gaps verificados e invariantes |
| 6 | [06-benchmark-plan.md](06-benchmark-plan.md) | Telemetría AVAILABLE/PARTIAL/MISSING, instrumentación, piloto A/B y criterio de decisión |
| 7 | [07-roadmap.md](07-roadmap.md) | 3B-3H, deuda y preguntas abiertas |
| 8 | [../adr/0012-project-memory-in-tree-core.md](../adr/0012-project-memory-in-tree-core.md) | ADR (Proposed) |

**Conclusión de 3A: GO para diseño/implementación de 3B**, con 3B
redefinida como *corrección y seguridad de lo existente*, sin encender la
memoria. El encendido depende del piloto 3D.

## 3B — Project Memory hardening (2026-09-24)

**Integrado en ECC** (merge `b7c12f0b4`; sin migración; producción en goose 174). Corrige y
endurece lo existente: el wiring del Worker, la elegibilidad basada en git,
los symlinks fail-closed, la frontera de secretos, la redacción, el
repositorio como DATO, el aislamiento entre proyectos, freshness y la
corrección incremental. Memoria y router siguen **off** por defecto.

| Documento | Contenido |
|---|---|
| [3b-implementation.md](3b-implementation.md) | Qué se implementó, con evidencia antes/después, contrato de freshness, punto de extensión Java y residuales |
| [3b-regression-evidence.md](3b-regression-evidence.md) | Tests de regresión permanentes: fallan en el baseline, pasan en 3B |

**Siguiente: 3C**, la instrumentación de la exploración real del agente (no
iniciada). Project Memory sigue **desactivada** por defecto. No se ha
demostrado ningún ahorro de tokens; eso le corresponde al piloto.
