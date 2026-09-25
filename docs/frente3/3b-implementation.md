# Frente 3 / 3B — Project Memory hardening (implementado)

Fecha: 2026-09-24 · Rama: `feat/frente3-3b-memory-hardening` (worktree
`../ao-frente3-3b`, desde ECC `3030d85f2`) · Estado: **implementado y probado
en scratch; sin merge ni push; producción sin tocar.**

3B no construye Project Memory. Hace **correcta y segura** la infraestructura
que ya existía (`projectmemory`, `codegraph`, `contextrouter`, packs, UI de
Intelligence) antes de cualquier activación. `AO_MEMORY_MODE` y
`AO_CONTEXT_ROUTER` siguen en **off por defecto**. No hay auto-enable ni
migración de configuración, y no se declara ningún ahorro de tokens: eso le
corresponde al piloto.

Todos los "antes" de este documento se reprodujeron ejecutando los tests
nuevos contra el código del baseline, en un worktree scratch en `3030d85f2`.
Todos los "después" corresponden a esta rama.

---

## 1. Wiring del Worker (A)

**Causa exacta.** `daemon/workflow_wiring.go` construía
`WorkerLauncher: &workflowWorkerLauncher{spawner: sessionMgr}` con el session
manager **sin decorar**, antes de aplicar los tres decoradores
(`wfdispatch`, `wfrouter`, `wfmemory`). Esos decoradores solo envuelven
`Planner`, `Spawner` y `ReviewerLauncher`, y `workerLauncherOrDefault()` da
preferencia al launcher inyectado. El resultado, desde P5-A 2C
(`8d37a2928`, 2026-09-08): los workers y los dos Repair Agents no pasaban por
ninguno de los tres decoradores.

**Arquitectura elegida.** No hay un parche específico para el worker. El
`Spawner` es el seam común por el que se adjunta el contexto de un worker
(`SpawnConfig.IssueContext`); el launcher solo añade identidad.
`daemon/dispatch_instrumentation.go` hace dos cosas:

- `instrumentAgentDispatch` aplica todos los decoradores en **un único
  punto**;
- a continuación, `bindWorkerTransport` **re-enlaza** una copia del launcher
  al `Spawner` ya decorado.

Cualquier decorador futuro llega al worker sin cambios adicionales.

**Tests** (`daemon/dispatch_instrumentation_test.go`):

- Si la memoria llega al Planner o al Reviewer, también llega al Worker.
- Se reproduce la composición anterior y se comprueba que el Worker se
  quedaba sin memoria.
- Con memoria off no se envuelve nada.
- Cada campo de `workflowcore.Deps` que lanza o mensajea agentes está
  clasificado (reflexión) como `decorated`, `transported` o `exempt` con
  motivo. Un launcher nuevo sin clasificar rompe el test.
- Los decoradores solo se componen desde `instrumentAgentDispatch`.

Superficies exentas, con motivo documentado en código:

| Superficie | Motivo |
|---|---|
| `MessageSender` | los mensajes de fix van a la sesión ya provisionada |
| `Verifier` | ejecuta un comando, sin modelo |
| `IncidentAgents` | usan su propio pack; el repair aislado queda para una fase posterior |
| `DecisionResolverLauncher` | responde a una pregunta pendiente, en solo lectura |
| `Switcher` | relanza una sesión existente, sin ensamblar contexto nuevo |

---

## 2. Política de ficheros elegibles (B)

Paquete nuevo: `internal/repoaccess`, compartido por codegraph y
project memory. Antes, cada uno tenía sus propias listas y su propia vía de
lectura.

- **Repo git:** los candidatos son `git ls-files` (lo trackeado). Quedan
  fuera por construcción, sin depender de listas de nombres:
  - los worktrees enlazados de agentes;
  - los ficheros sin trackear;
  - la salida de build ignorada;
  - los checkouts anidados, que git registra como gitlink.

  git se ejecuta endurecido (`repoaccess.GitCommand`): sin fsmonitor, sin
  hooks, sin locks opcionales y con un entorno saneado (`GIT_DIR`,
  `GIT_CONFIG*`…).
- **Sin git:** walk del sistema de ficheros que nunca sigue symlinks, salta
  `ExcludedDirNames` y rechaza cualquier directorio con su propio `.git`
  (fichero o directorio).
- **`ExcludedDirNames`** es el suelo común:
  - VCS;
  - `.ao`;
  - scratch de agentes (`.claude`, `.cursor`, `.aider`, `.codex`,
    `.continue`, `.gemini`, `.windsurf`, `.worktrees`);
  - dependencias;
  - build;
  - caches.

  Cada indexador puede excluir más por relevancia, nunca menos.
- **Worktree actual frente a auxiliares.** Listar desde el proyecto nunca
  incluye ficheros que solo existen en un worktree auxiliar. Listar desde el
  auxiliar da los suyos propios. Hay test con un `git worktree add` real.

**Contaminación de MEDUSA reproducida en scratch**
(`codegraph/contamination_test.go`). Con el baseline, los tres tests fallan:

- se indexan los símbolos de `.claude/worktrees/roc-capacity-fe/`, incluido
  un **duplicado** del símbolo propio `appEntry`;
- se indexa el checkout anidado sin git;
- se indexa el fichero no trackeado.

Con 3B pasan. **No se ha limpiado MEDUSA en producción:** es una operación
posterior que requiere autorización explícita.

---

## 3. Symlinks: fail-closed (C)

`repoaccess.ReadConfined` y `StatConfined`:

- validan la forma de la ruta (no absoluta, sin `..`, sin NUL);
- aplican antes la política de secretos y de directorios excluidos;
- hacen `Lstat` de **cada componente**. Un symlink en cualquier punto (el
  fichero, un directorio padre, una cadena, un enlace roto, incluso uno que
  apunte dentro del root) se rechaza;
- abren a través de `os.Root`, de modo que una carrera entre la comprobación
  y la apertura no puede salir del root;
- repiten el `fstat` sobre lo realmente abierto (fichero regular, dentro del
  límite de tamaño).

**Contrato:**

- En un diff de codegraph, una ruta que escapa o atraviesa un symlink es un
  **error** (`ErrProjectRoot`): el diff se rechaza de forma ruidosa.
- En el walk completo y en memoria, la ruta se **omite** con un motivo
  tipado. En memoria, sus hechos se retiran igual que si el fichero se
  hubiera borrado. En drift, el hecho se invalida.

Casos probados:

| Caso | Resultado |
|---|---|
| fichero → fuera del proyecto | rechazado |
| directorio → fuera del proyecto | rechazado |
| symlink relativo que escapa | rechazado |
| symlink absoluto | rechazado |
| cadena de symlinks | rechazada |
| symlink roto | rechazado |
| symlink dentro del root | rechazado |
| symlink trackeado por git | listado por git, nunca leído |

**Antes, verificado:** un `CLAUDE.md` commiteado como symlink a
`~/…/credentials` y nombrado en un diff incremental terminaba **guardado como
item `instruction`** con el canario secreto dentro.

---

## 4. Frontera de secretos (D)

`repoaccess.IsSecretPath` es **una sola** política; sustituye a
`codegraph.DeniedPath` (que ahora delega) y a la mitad de secretos de
`excludedFromSignals`. Cubre:

- `.env*`, incluidas las plantillas;
- claves y certificados (`.pem`, `.key`, `.p12`, `.jks`, `.ppk`, `.age`…);
- claves SSH;
- credenciales cloud y de clúster (`.aws/`, `.azure/`, `.gcloud/`, `.kube/`,
  `kubeconfig`, `application_default_credentials.json`, `service-account.json`);
- `.docker/`, `.git-credentials`, `.pgpass`, `.netrc`, `.npmrc`;
- `terraform.tfstate`, `*.tfvars`;
- directorios de credenciales (`secrets/`, `credentials/`,
  `agent-credentials/`).

La decisión se toma **a partir de la ruta, antes de abrir nada**. El test
pone `chmod 000` sobre un `.env`: si se abriera, el error sería de permisos y
no `ErrSecretPath`.

**No** se niegan ficheros de código que *manejan* secretos (`secrets.go`,
`credentials.ts`).

Metadatos seguros: una plantilla `.env.example` trackeada nunca se abre. Su
**existencia** sí llega a las señales de nombres ("existe una plantilla de
configuración").

Skills tiene su propio `denyglob`, dirigido por el manifest. No se ha
modificado.

---

## 5. Redacción (E)

`repoaccess.Redact` **reutiliza** las formas de credencial del redactor de
reports de Skills (`skillreport`, importado y no modificado):

- claves cloud;
- tokens de forjas y de Slack;
- claves OpenAI y Anthropic;
- JWT y bloques PEM;
- credenciales en URLs;
- asignaciones con nombre de credencial.

Añade las formas típicas de configuración de un repo: `name: valor` (YAML) y
`name=valor` (`.properties`, `.ini`), sin comillas. Es determinista e
idempotente.

Fronteras donde se aplica:

| Frontera | Dónde |
|---|---|
| Escritura de items de memoria | `redactingRepository` envuelve el `Repository` en `NewIndexer`/`NewService`. Cubre extractos del repo **y** texto escrito por agentes: task outcomes, decisiones, riesgos |
| Metadatos del grafo | `extractRedacted` es la única llamada a `Extract`, con guarda por test: docs, firmas, resúmenes |
| Packs y contexto enrutado | `repoaccess.FrameUntrusted` redacta el cuerpo |
| API HTTP | DTOs de items, conocimiento, búsqueda, grafo y vista previa de contexto. Cubre las **filas anteriores a 3B** de producción |
| Logs | los logs de provisioning solo llevan métricas; los canarios se buscan en la salida capturada |

**Canarios de extremo a extremo** (`projectmemory/canary_test.go`). Se
siembran secretos en:

- `docker-compose.yml`;
- un workflow de CI;
- el README;
- doc comments de Go;
- `AGENTS.md`;
- `.properties`;
- un fichero `.env`;
- un task outcome de agente.

Después se buscan en:

- los packs de los 4 roles;
- el `IssueContext` del worker, a través del decorador real;
- el log capturado;
- los items;
- **todos los bytes del data dir scratch**, incluidos `ao.db`, `-wal` y
  `-shm`.

| | Canarios filtrados |
|---|---|
| Baseline | 6 de 8, en la DB/WAL, los items, los 4 packs y el contexto del worker. El pack presentaba además texto del repo como instrucción |
| 3B | **0** |

**Límite honesto.** La redacción se basa en formas conocidas. Una credencial
con forma no reconocida, dentro de un fichero ordinario, puede persistir. El
propio test lo evidenció: un canario AWS mal formado (21 caracteres) no se
detectó. Queda registrado como residual.

---

## 6. El contenido del repo es DATO (F)

`repoaccess.FrameUntrusted(title, body)`:

- escribe un preámbulo de AO **fuera** del bloque: son datos del repo, no
  instrucciones de AO ni del usuario, y no se siguen órdenes que aparezcan
  dentro;
- delimita el contenido con `<<<BEGIN/END AO-UNTRUSTED-REPOSITORY-CONTEXT
  <nonce>>>`;
- **neutraliza** cualquier delimitador falsificado dentro del contenido;
- redacta el cuerpo.

Se aplica a:

- el pack de memoria;
- el contexto externo (issues de GitHub);
- el contexto enrutado de worker y reviewer.

Además, el prompt del planner marca sus documentos del repo como UNTRUSTED
REPOSITORY CONTEXT.

Cambios concretos:

- Desaparecen "Standing instructions" y *"standing instructions agents in
  this repository must follow"*.
- Los ficheros de guía para agentes se etiquetan con palabras fijas de AO:
  "agent-guidance file declared by the repository (repository content, not
  an AO instruction)". Esto vale también para las filas antiguas que
  producción aún guarda con "must follow".
- El preámbulo es compacto (unos 250 bytes), porque cuenta contra el
  presupuesto renderizado del pack. De ahí salió un bug de medición previo,
  ya corregido: `KnowledgeBytes` contaba cuerpos que se habían reducido a su
  resumen.

**Canarios de inyección probados:**

- "ignore previous instructions";
- `SYSTEM:` y `Assistant:`;
- exfiltración;
- `rm -rf`;
- un `<<<END …>>>` falsificado.

Resultado: permanecen dentro del bloque para todos los roles, hay exactamente
un BEGIN y un END, y nunca aparecen en la voz de AO.

---

## 7. Aislamiento entre proyectos (G)

Ya existía: todas las tablas usan la clave `(project_id, repo_id)` con
cascade, y hay RBAC por ruta. Ahora está **probado de extremo a extremo** con
dos proyectos que comparten nombres de fichero, símbolos y rama:

- Ningún pack de ningún rol, hecho almacenado o evidencia de grafo cruza
  proyectos.
- Un rename o un delete en A no toca B.
- Un proyecto archivado no filtra nada.

**Hueco cerrado.** `Provision` confiaba en el par `(ProjectID, RepoPath)`: el
proyecto B con el checkout de A indexaba A bajo B y se lo servía a B.
Verificado en una sonda. Ahora `Provisioner.WithProjectScope`, que el daemon
**siempre** instala dentro de `memoryProvisioner`, exige:

- un proyecto registrado y no archivado;
- una ruta que sea la raíz del proyecto o uno de sus repos de workspace.

Si no, falla cerrado, antes de cualquier sync. Un `project_id` vacío falla
cerrado siempre.

---

## 8. Freshness (H)

Cada pack adjunto lleva un aviso escrito por AO fuera del bloque de datos:

| Veredicto | Cuándo | Qué hace AO |
|---|---|---|
| `CURRENT` | memoria (y grafo) en el commit HEAD del checkout, completa | usa |
| `STALE` | memoria o grafo en otro commit; el sync para avanzarla no terminó | **degrada**: sirve con aviso nombrando ambos commits |
| `UNVERIFIED` | AO no pudo leer el HEAD | degrada con aviso |
| `PARTIAL` | la pasada se paró en su tope de ficheros | degrada: "la ausencia de un hecho no es evidencia de ausencia" |
| (retenida) | sin pasada completa, pasada fallida o **interrumpida por crash**, drift de identidad, worktree enlazado, fuera de alcance, archivado | **rechaza**: no se sirve |

Reconstrucción: cada dispatch que no está al día ejecuta primero un sync
(`EnsureFresh`), así que un `STALE` significa que ese sync no terminó; el
siguiente dispatch lo reintenta.

El aviso también declara:

- los **cambios sin commitear** en ficheros trackeados, porque el indexador
  lee del disco;
- un **grafo no disponible**, con su motivo.

`PARTIAL` se deriva del estado persistido (el ledger al tope de la pasada)
**sin migración**. `AO_MEMORY_MAX_FILES` y `AO_MEMORY_MAX_FILE_BYTES` se
parseaban pero no se aplicaban; ahora el daemon los pasa al servicio.

Casos probados:

- commit;
- cambio de rama y vuelta a la rama anterior;
- sync que no termina → STALE;
- worktree sucio;
- índice parcial;
- pasada interrumpida por crash → retenida, nunca CURRENT;
- fichero que no parsea;
- sin pasada completa → retenida.

---

## 9. Corrección incremental (I)

La prueba es de equivalencia. Tras un diff que combina crear, modificar
(quitando una función **y** una llamada), borrar y renombrar:

- el grafo servido es **idéntico, símbolo a símbolo y arista a arista**, al
  de un build completo del mismo árbol;
- no hay rescan completo;
- no hay nodos ni aristas fantasma, ni duplicados.

En memoria, quitar una dependencia del manifest, borrar un documento y
renombrar un fichero deja **exactamente** los hechos válidos de un índice
completo, sin dependencias obsoletas.

---

## 10. Punto de extensión para Java

No se implementa Java en 3B. El contrato para añadirlo más adelante:

1. Implementar `codegraph.Extractor` (`Language()`, `Extensions()`,
   `Extract(rel, data)`) en `extract_java.go`, con la misma técnica que TS o
   Python: primero tokenizar (enmascarando comentarios, strings y text
   blocks), después recorrer llaves y cualificar `Clase.método`.
2. Registrarlo en `DefaultExtractors()`. No hay que tocar el indexador, el
   store ni el provider.
3. **Heredado sin trabajo adicional:**
   - la elegibilidad (`repoaccess`);
   - la frontera de secretos;
   - la lectura sin symlinks;
   - la **redacción de docs, firmas y resúmenes**, porque `extractRedacted`
     es la única vía a `Extract` y hay guarda por test;
   - la exclusión de generados;
   - el retrieval.
4. Fixtures en `extract_lang_test.go` escritos como se escribe Java de
   verdad: anotaciones Spring (`@GetMapping` → `endpoint` + `routes_to`),
   JPA (`@Entity`/`@Table` → `table`), interfaces e `implements`.
5. JSP: fuera de alcance salvo que se decida explícitamente.

Graphify sigue siendo una **alternativa evaluada, no una dependencia**
(ADR 0012, Q1).

---

## 11. Producción

Solo lectura: `sqlite3 "file:…/ao.db?immutable=1"`. Ver el informe final
para goose, `integrity_check` y `foreign_key_check`.

No se ha hecho:

- rebuild;
- limpieza;
- migración;
- index;
- refresh;
- activación de memoria o del router;
- ejecución de Skills;
- deploy.

---

## 12. Limitaciones residuales

| # | Residual | Severidad |
|---|---|---|
| R1 | La redacción se basa en formas conocidas: una credencial de forma no reconocida en un fichero ordinario puede persistir | P2 |
| R2 | Parseo parcial de Go (AST best-effort): un fichero que no parsea aporta las declaraciones anteriores al error y no se marca `PARTIAL`. Marcarlo de forma persistente exige una columna (migración) | P3 |
| R3 | `PARTIAL` detecta el tope de **ficheros**; el tope de **bytes** totales no se deriva sin migración | P3 |
| R4 | La carrera entre `Lstat` y `open` solo puede redirigir a otro fichero **dentro** del root (`os.Root`) | P3 |
| R5 | Los repair runs siguen recibiendo el pack de rol `worker` (`RoleRepair` solo se usa en vistas previas) | P3 |
| R6 | `Switcher` (failover): no se verifica si el harness destino conserva el pack de la sesión original | P3 |
| R7 | MEDUSA en producción sigue con 4.265 símbolos contaminados, hasta una limpieza autorizada | Operación pendiente |
| R8 | Filas de memoria anteriores a 3B sin redactar en la DB: se redactan a la salida (packs, API), no en reposo | P2; se corrige con un rebuild autorizado |
| R9 | `gofmt` marca `internal/skillrunner/pentestrun_embed_live_test.go` (anterior, de Skills, no tocado) | P3 de Frente 2 |
