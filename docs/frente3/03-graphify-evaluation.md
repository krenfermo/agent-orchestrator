# Frente 3 / 3A — Evaluación de Grae / Graphify

Fecha de la investigación: 2026-09-24. Lectura del repositorio y de fuentes
públicas. **No se ha instalado nada.** Graphify publica releases cada 1-3 días,
así que las cifras de upstream caducan pronto.

Etiquetas usadas: **HECHO** (verificado, con fuente), **INFERENCIA**,
**PROPUESTA AO**.

---

## 1. A qué nos referimos dentro de AO

- **HECHO.** En el repositorio los dos nombres sólo aparecen en prosa,
  comentarios y fixtures de test:
  - `codegraph/codegraph.go:5,67`: `"graphify"` como valor de ejemplo de `Name()`.
  - `projectmemory/graph.go:17-44`.
  - `domain/project_memory.go:38,792`.
  - `httpd/controllers/workflow_usage_ledger_dto.go:400,477`.
  - Un test del frontend que exige que la UI **no** diga "Graphify"
    (`workflow-token-ledger.test.tsx:164`).
- **HECHO.** No hay cliente, dependencia, clave de configuración, endpoint ni CLI
  de ninguno de los dos (`docs/p2-project-memory-audit.md` §4.1, reconfirmado).
- **HECHO.** El primer uso de "Graphify" es `58e2535bf` (2026-08-24): *"so
  Graphify or any future graph tool can plug in"*. "Grae" aparece en la
  auditoría de P2 (2026-08-29). Ningún commit ni documento da una URL, versión o
  paquete.
- **HECHO.** Los docs fijan reglas para cualquier adaptador:
  - `project-memory-authority.md` §19: un adaptador debe transportar identidad,
    autoridad, generación, validez, supersesión y evidencia. Si no puede, debe
    ser de solo lectura. *"Graphify must never become a source of truth."*
  - `usage-accounting.md:234-237`: el backend local se reporta como `local`,
    nunca como "Graphify".
- **INFERENCIA.** Los nombres llegaron como parte de la especificación del
  usuario y siempre se han usado en pareja. El repo nunca define "Grae".

---

## 2. Identificación upstream

### Graphify

| Proyecto | ★ (2026-09-24) | Qué es | ¿Encaja? |
|---|---|---|---|
| **Graphify-Labs/graphify** (antes `safishamsi/graphify`; PyPI `graphifyy`) | ~121 K | Skill `/graphify` + librería/CLI Python: repo → grafo de conocimiento para Claude Code, Codex, etc. | **Sí** |
| elbruno/graphify-dotnet, rhanka/graphify (→ engram), forks | < 100 | Ports o derivados | copias |
| kbastani/graphify, raufer/graphify, warioddly/graphify | < 500 | NLP sobre Neo4j, parser de texto, gráficos | no |

**INFERENCIA (confianza alta):** AO se refiere a Graphify-Labs/graphify. Es la
única herramienta de "grafo de código para agentes" con ese nombre, encaja con
el vocabulario del puerto de AO y coincide en fechas: se creó el 2026-04-03 y AO
lo nombra el 2026-08-24.

### Grae

**HECHO: no se encontró ningún proyecto real de grafo de código ni de memoria
para agentes llamado Grae.**
- Búsquedas web y de GitHub (`grae in:name`, 1.061 resultados) sólo devuelven
  proyectos no relacionados: A-GRAE (RL), GRAE (autoencoders), tracking 3D.
- `grae` no existe en PyPI (404) ni en npm.
- `grae.dev` y `grae.ai` son dominios aparcados.

**INFERENCIA (confianza baja sobre cuál):** probablemente sea un error de
transcripción. Candidatos conceptuales: GraphRAG (microsoft), GRAG, Graphiti
(getzep). **No hay evidencia para elegir uno.** Queda como pregunta abierta para
el usuario (doc 07 §5).

---

## 3. Ficha de Graphify-Labs/graphify

Fuentes: `github.com/Graphify-Labs/graphify` (README, ARCHITECTURE.md,
pyproject.toml y SECURITY.md de la rama `v8`, releases vía `gh api`).

| Aspecto | HECHO |
|---|---|
| Qué es | Skill + librería/CLI Python. Pipeline `detect → extract → build (networkx) → cluster (Leiden) → analyze → report → export` |
| Parser | **tree-sitter AST, determinista y sin LLM** para código. El README habla de ~37 gramáticas: Python, TS/JS, Go, Rust, **Java**, C/C++, C#, Kotlin, PHP, Swift… Algunos lenguajes van por regex o como extra |
| Semántica | Docs, PDF, imágenes y vídeo pasan por una "semantic pass" con LLM; audio y vídeo se transcriben localmente con faster-whisper |
| Modelo de datos | Nodo `{id, label, source_file, source_location}`. Arista `{source, target, relation (calls/imports/uses/inherits…), confidence: EXTRACTED/INFERRED/AMBIGUOUS}`. Nodos de "rationale" extraídos de comentarios `# WHY:`/`# NOTE:` |
| Almacenamiento | `graphify-out/` **dentro del proyecto**: `graph.json`, `GRAPH_REPORT.md`, `graph.html`, `cache/`. Exportaciones a Neo4j, FalkorDB, GraphML u Obsidian. Sin DB ni vector store |
| Incremental | `graphify update .` (sólo código, sin LLM), caché por fichero, `watch` (watchdog), hooks git post-commit/checkout y merge driver de `graph.json`; "shrink guard" |
| Consulta | CLI `query`/`path`/`explain`; servidor MCP (`query_graph`, `get_neighbors`, `shortest_path`, `get_pr_impact`…) |
| Integración con agentes | `graphify claude install` escribe una sección en **CLAUDE.md** y un hook **PreToolUse** |
| Runtime | Python ≥ 3.10; `networkx`, `numpy`, `rapidfuzz`, ~26 paquetes `tree-sitter-*`; extras opcionales (mcp, neo4j, anthropic, openai, gemini, ollama…) |
| Licencia | Apache-2.0 (también incluye texto MIT) |
| Actividad | Creado 2026-04-03. v0.9.67 el 2026-09-23; releases cada 1-3 días; ≥100 contribuidores; ~1.460 issues abiertas |
| Privacidad | Código: "nothing leaves your machine"; `--code-only` funciona sin conexión. Docs y medios van al backend LLM detectado automáticamente (Gemini → Kimi → Claude → OpenAI → …). Kimi envía a servidores de Moonshot. **Registra cada consulta por defecto** en `~/.cache/graphify-queries.log` (`GRAPHIFY_QUERY_LOG_DISABLE=1` para desactivarlo) |
| Seguridad | SECURITY.md cubre SSRF, prompt injection en la semantic pass y path traversal en MCP. Su tabla de versiones soportadas sigue diciendo 0.3.x |
| Comercial | "graphify Enterprise" alojado, en acceso anticipado |

---

## 4. Encaje con la arquitectura de AO

| Criterio | Graphify | codegraph en árbol (AO) |
|---|---|---|
| Lenguajes | ~37 (tree-sitter) | Go (AST), TS/JS, Python (lexer), SQL |
| **Java / JSP** (SIGE, ws_sigeseguros_crm) | **sí** (Java; JSP no consta) | **no** |
| Precisión Go | AST tree-sitter | AST oficial `go/parser` (mayor o igual) |
| Identidad/generación/CAS | no (`graph.json` completo) | sí (`served_generation`, CAS, retoma tras crash) |
| Autoridad/procedencia por hecho | `confidence` por arista, sin commit ni digest | commit, digest, autoridad, clase de evidencia |
| Ubicación del estado | **dentro del repo** + `~/.cache` | `~/.ao` (regla dura de AGENTS.md) |
| Runtime | Python + ~30 dependencias nativas | binario Go único |
| Estabilidad del esquema | alta rotación (0.9.x, releases casi diarias) | propio, versionado por migraciones |
| Coste LLM | 0 en modo `--code-only` | 0 |
| Integración por defecto | reescribe CLAUDE.md y hooks | no toca el repo |
| Aislamiento | proceso con acceso a todo el repo | dentro del daemon, con denylist y symlink-safe (codegraph) |

**INFERENCIA.** Las ventajas reales de Graphify para AO se reducen a dos:

1. **Cobertura de lenguajes**, sobre todo Java, que está verificada en dos
   proyectos del usuario.
2. **Aristas `calls` en más lenguajes**, marcadas `INFERRED`.

Todo lo demás (grafo incremental, retrieval, integración con agentes, UI) ya
existe en AO con las garantías que AO exige y Graphify no ofrece.

---

## 5. Riesgos si se integrara

1. **Viola la regla de estado bajo `~/.ao`.** Habría que forzar la salida fuera
   del repo y desactivar el log de consultas.
2. **Añade dependencia de runtime Python y procesos externos** al producto
   Electron + Go. Es la "fragile external dependency" que la auditoría de P2 ya
   rechazó.
3. **Rotación del esquema.** Habría que fijar la versión exacta y tener un test
   de contrato sobre `graph.json`.
4. **Faltan los metadatos de §19.** Por la regla vigente, sería de solo lectura
   (`TeeGraph` como espejo, nunca en lugar del canónico).
5. **Instaladores intrusivos** (`claude install`, `hook install`) en repos de
   clientes. Si se usara, AO sólo podría invocar `extract`/`update` en modo
   headless.
6. **Fuga por defecto de docs a un LLM** si hay claves en el entorno. Habría
   que usar `--code-only` forzado y un entorno depurado.
7. **Mayor superficie de ataque:** un parser nativo externo procesando código
   no confiable fuera del sandbox de AO.

---

## 6. Conclusión

- **Graphify como núcleo: NO.** Esto no es una cuestión de calidad de Graphify,
  sino de encaje. AO ya tiene el núcleo, con garantías (generación, procedencia,
  estado en `~/.ao`, un único binario) que Graphify no ofrece y que el propio
  repo declara obligatorias.
- **Graphify como adaptador opcional de solo lectura para lenguajes no
  soportados:** **decisión abierta**. Compite con escribir un extractor Java en
  Go, igual que el de TS o Python. El criterio para decidir está en el plan de
  benchmark (doc 06 §6) y en el ADR 0012 (pregunta abierta Q1). No se decide en
  3A.
- **"Grae":** no identificado. Hasta que el usuario lo aclare, "Grae/Graphify"
  se interpreta como "Graphify-Labs/graphify + el puerto genérico de grafo".

**PROPUESTA AO.** Si más adelante se aprueba un spike, las condiciones
mínimas son:
- versión fijada;
- `extract --code-only` headless;
- salida bajo `~/.ao/data/…`;
- `GRAPHIFY_QUERY_LOG_DISABLE=1`;
- entorno depurado sin claves de LLM;
- nunca `claude install` ni `hook install`;
- ejecución sobre una copia staged con la misma denylist;
- reportarse como backend `graphify` sólo cuando el adaptador exista de verdad.
