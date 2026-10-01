# Frente 3 / 3D — PRECONDITION_3D_AUTH_DESIGN: identidades dedicadas del laboratorio

> **SUSTITUIDO (2026-09-28).** La revisión REAL de Codex dio **NO-GO** ([3d-auth-design-codex-review.md](3d-auth-design-codex-review.md)). La única especificación vigente es [3d-auth-design-v4.md](3d-auth-design-v4.md). Este documento se conserva solo como historial: **no se implementa** y ninguna de sus secciones es normativa. La revisión provisional de §9.2 (no Codex) queda anulada.

Fecha: 2026-09-29. Rama: `feat/frente3-3d-prerequisites`, partiendo de `48149d1bc`.

**Alcance.** Solo diseño y revisión. Sustituye el intento de cerrar con Seatbelt el P1-2 del cycle6 (relanzamiento del proveedor con las credenciales de la cuenta, ver [3d-preflight.md](3d-preflight.md), última sección).

**No se ha hecho nada de lo siguiente:** crear o leer credenciales, tocar cuentas o conectores, implementar el gateway, preregistrar, ejecutar un mini-E2E, ejecutar 3D. El harness del laboratorio (`~/.ao/scratch/frente3/tools/lab3d/`) vive en la máquina del operador y **no** se modificó.

Este documento se escribió desde un contenedor en la nube que no tiene el harness ni acceso a las APIs de los proveedores. Por eso todo lo que dice sobre el comportamiento en macOS es diseño y queda marcado **VERIFICAR** hasta que se compruebe en la máquina del laboratorio.

## 1. Problema de partida

Hallazgos del ciclo anterior, verificados en la máquina del laboratorio:

- El agente legítimo necesita autenticarse con el proveedor.
- Hoy usa las credenciales **personales** del operador: la entrada `Claude Code-credentials` del llavero de login y `~/.codex/auth.json`.
- Seatbelt no puede negárselas a los hijos del agente sin negárselas al agente, porque los perfiles son estáticos, se heredan y `process-exec` se evalúa también en el primer `exec`.
- Con esas credenciales, cualquier hijo puede abrir una sesión nueva con la cuenta personal, sin las flags del shim. Eso da acceso a los conectores de claude.ai, `RemoteTrigger`, sesiones remotas, apps de ChatGPT, Files y conversaciones de la cuenta: **estado compartido fuera del sandbox**.
- Bloquear los binarios `claude`/`codex` no lo cierra: la credencial sirve igual con `curl` o con `node`.

**Idea de la solución.** El agente no tiene por qué ver ninguna credencial personal. Si ningún proceso del sandbox la necesita, Seatbelt **sí** puede negarla a todos, y la pregunta de distinguir padre e hijo desaparece. El agente usa en su lugar una identidad del laboratorio, sin estado personal, cuyo alcance en el servidor está limitado por diseño.

## 2. Mecanismos oficiales (evidencia)

No se inventa ningún mecanismo: todo lo de abajo está soportado por el proveedor.

### Claude Code (versión del laboratorio: 2.1.284)

| Mecanismo | Qué hace | Fuente |
|---|---|---|
| `ANTHROPIC_BASE_URL` | Envía las llamadas del modelo a un gateway (LLM gateway). El gateway tiene que servir `/v1/messages` y `/v1/messages/count_tokens` y reenviar `anthropic-beta`/`anthropic-version` | code.claude.com/docs/en/llm-gateway-connect, `network-config` |
| `ANTHROPIC_AUTH_TOKEN` | Token *bearer* hacia el gateway. Tiene precedencia sobre `ANTHROPIC_API_KEY`, `apiKeyHelper` y OAuth | code.claude.com/docs/en/authentication |
| `ANTHROPIC_API_KEY` | API key de Console. En `-p` se usa sin aprobación | ídem |
| `apiKeyHelper` + `CLAUDE_CODE_API_KEY_HELPER_TTL_MS` | Script que entrega la clave. Un hijo puede ejecutarlo igual, así que **no** oculta nada | ídem |
| `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token`) | Token de suscripción de claude.ai | ídem. **Descartado**: es una cuenta de claude.ai y trae conectores y `RemoteTrigger` |
| Conectores de claude.ai | Solo existen con autenticación de claude.ai. Con API key o gateway no hay conectores, `RemoteTrigger`, sesiones remotas ni Claude in Chrome | ídem; y `ENABLE_CLAUDEAI_MCP_SERVERS` / `disableClaudeAiConnectors` en el binario 2.1.284 |
| `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` | Quita de los subprocesos (Bash, hooks) `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_CUSTOM_HEADERS` y otros | Cadenas del binario 2.1.284; no aparece en la documentación pública. En Linux exige bubblewrap. **No se usa como control**: en macOS el entorno inicial de un proceso del mismo usuario es legible vía `KERN_PROCARGS2` (hallazgo del ciclo 3) |
| Claves de Console | Se crean **solo en la UI de Console**; la Admin API solo lista, renombra y desactiva. Una clave puede limitarse a un único *workspace* | platform.claude.com/docs/en/manage-claude/admin-api, `authentication` |
| Caché de prompts | Aislada **por workspace**, no por organización. TTL de 5 min por defecto; 1 h opcional (`ttl: "1h"`), a elección del cliente | platform.claude.com/docs/en/build-with-claude/prompt-caching |

### Codex CLI (versión del laboratorio: 0.157.1)

Fuente: código de `openai/codex` en `main` (0.158.x). Rutas relativas a `codex-rs/`. La 0.157.1 del laboratorio puede diferir: **VERIFICAR** en el ensayo.

| Mecanismo | Qué hace | Fuente |
|---|---|---|
| `[model_providers.<id>]` con `base_url`, `env_key`, `wire_api="responses"`, `requires_openai_auth=false` | *Provider* propio hacia un gateway. El bearer sale de la variable `env_key`. Con `requires_openai_auth=false` y sin `auth`, las peticiones no usan ninguna credencial de Codex/OpenAI | `model-provider-info/src/lib.rs:135-200,184-188`; `model-provider/src/auth.rs:205-207,292-304` |
| Rutas de backend de ChatGPT (`/wham/*`, `/api/codex/*`, plugins, apps, uso, workspace) | Solo con el *provider* `openai` y base `/backend-api/codex`. Con un *provider* propio quedan apagadas | `model-provider-info/src/lib.rs:610-617` |
| `store` | Siempre `false` | `core/src/client.rs:997` |
| `previous_response_id` | Solo por el transporte WebSocket | `core/src/client.rs:1994-2013`. El laboratorio fija `supports_websockets=false` |
| `prompt_cache_key` | Id de sesión (enrutamiento de caché, no aislamiento) | `core/src/client.rs:575-587` |
| `prompt_cache_retention` / `prompt_cache_options.ttl` | Codex no los envía, pero la API los acepta: un hijo con acceso al endpoint podría pedirlos | guía incluida en el binario |
| Compactación remota | Solo con el *provider* `openai` o Azure; con uno propio es local | `model-provider/src/provider.rs:36-43,421-433` |
| Otros endpoints en el binario | `GET models`, `alpha/search`, `memories/trace_summarize`, `images/*`, `realtime`, `/v1/files` | `codex-api/src/endpoint/*`. El gateway los rechaza |
| Funciones que exigen ChatGPT | Catálogo y cambios de plugins, apps y conectores, remote control, `/usage`, workspace, analítica | cadenas del binario |
| `codex login --with-api-key` (stdin) → `CODEX_HOME/auth.json` | Clave de OpenAI guardada en el home de Codex, legible para los hijos | `cli/src/login.rs:277-283`; `login/src/auth/manager.rs:1021-1044`. **No se usa**: el agente no debe ver la clave |
| `CODEX_API_KEY` | Solo `codex exec` la honra. El laboratorio usa el TUI | `exec/src/lib.rs:726`; `manager.rs:1499-1505` |
| `forced_login_method = "api"` | Evita que se use una sesión de ChatGPT | `config/src/config_toml.rs:279-281`; `manager.rs:1350-1383` |
| `shell_environment_policy` | Por defecto **no** quita `*KEY*`/`*TOKEN*` a los hijos (`ignore_default_excludes = true`) | `config/src/shell_environment_policy.rs:135-136`. Da igual: el token del agente es solo de capacidad |

## 3. Modelo de amenazas: canales entre repeticiones

**Adversario.** Un agente del laboratorio (worker o reviewer), o cualquier proceso que lance, que intenta:

- **escribir** algo que una repetición posterior pueda **leer**; o
- **leer** algo que revele su brazo (OFF/ASSISTED), el `memoryMode`, el tratamiento, resultados anteriores, prompts o evidencia privada, o el estado de otra repetición.

Esto incluye cualquier modelo que colabore consigo mismo entre repeticiones: se asume que el adversario es el mismo modelo en todas ellas.

Un canal existe si hay estado persistente que (a) una repetición puede escribir y (b) otra puede leer. Tanto el estado del agente como el de los proveedores.

**Columnas:**

- **Hoy**: credencial personal alcanzable desde el sandbox.
- **A**: una identidad dedicada por proveedor para todo el experimento, con la clave al alcance del agente.
- **B**: una clave y un workspace o proyecto **por repetición**, con la clave al alcance del agente.
- **C**: gateway de credenciales fuera del sandbox, con allowlist; el agente solo recibe un token de capacidad por repetición.

Símbolos: ✗ = canal abierto; ✓ = cerrado; ~ = cerrado con una condición indicada.

| # | Canal | Hoy | A | B | C |
|---|---|---|---|---|---|
| 1 | Conectores de claude.ai, MCP remoto de la cuenta | ✗ | ✓ (no hay claude.ai) | ✓ | ✓ |
| 2 | `RemoteTrigger`, rutinas, sesiones remotas, `SendMessage` | ✗ | ✓ | ✓ | ✓ |
| 3 | Apps y conectores de ChatGPT, tareas en la nube de Codex | ✗ | ✓ (no hay ChatGPT) | ✓ | ✓ |
| 4 | Conversaciones, proyectos e historial de claude.ai y ChatGPT; memoria del proveedor | ✗ | ✓ | ✓ | ✓ |
| 5 | Files API de Anthropic (subir y listar; el nombre y el tamaño bastan como canal) | ✗ | ✗ (mismo workspace) | ✓ | ✓ (endpoint bloqueado) |
| 6 | Message Batches, Skills API, Managed Agents (agentes, entornos, sesiones, *memory stores*) | ✗ | ✗ | ✓ si todo es por workspace (**VERIFICAR** Skills) | ✓ (bloqueado) |
| 7 | Contenedores de code execution reutilizables (`container`) | ✗ | ✗ | ✓ | ✓ (herramienta de servidor bloqueada) |
| 8 | MCP remoto por la API (`mcp_servers`), `web_fetch`/`web_search` de servidor (una URL con datos es una escritura hacia fuera) | ✗ | ✗ | ✗ | ✓ (parámetro y herramientas bloqueados) |
| 9 | OpenAI: Files, vector stores, batches, asistentes/hilos, chat completions guardadas (listables), contenedores, evals | ✗ | ✗ (mismo proyecto) | ✓ (**VERIFICAR** que todo es por proyecto) | ✓ (solo `POST /v1/responses`) |
| 10 | OpenAI: respuestas guardadas y `previous_response_id`, Conversations API | ✗ | ✗ | ✓ | ✓ (`store:false` forzado; `previous_response_id` y `conversation` rechazados) |
| 11 | **Caché de prompts**, activa: el agente escribe un prefijo con `cache_control`, y la repetición siguiente sondea `cache_read_input_tokens` / `cached_tokens` | ✗ | ✗ | Anthropic ✓ (workspace); OpenAI ✗ (la caché no es por proyecto y el cliente puede pedir retención de 24 h, **VERIFICAR**) | ~ (ver §4.3) |
| 12 | **Caché de prompts**, pasiva: el primer turno de la repetición *k+1* acierta en la caché que dejó *k*, y el agente lo ve en su propio transcript (`usage`). Revela si el prefijo de *k* coincidía, y con él el brazo de *k*. Además contamina la covariable de caché | ✗ | ✗ | igual que 11 | ~ (ver §4.3) |
| 13 | Rate limits compartidos (cabeceras `ratelimit-remaining`) | ✗ | ~ | ~ | ~ (el gateway elimina las cabeceras; hay enfriamiento entre repeticiones) |
| 14 | **Internet público**: pastebins, webhooks, gists; `curl` o WebFetch desde el agente | ✗ | ✗ | ✗ | ✓ **solo si** además se niega todo el egress salvo el gateway (§4.4) |
| 15 | Credenciales personales ajenas a los LLM (llavero con git osxkeychain o `gh`, `~/.config/gh`, `~/.ssh`, `~/.netrc`): push a un repo o gist como canal | ✗ | ✗ | ✗ | ✓ **solo si** se niegan el llavero y esos archivos (§4.4) |
| 16 | Estado local: HOME, `CODEX_HOME`, `~/.claude*`, cachés, tmux, sockets, Docker | lecturas cerradas en ciclos anteriores; **escrituras fuera del árbol de la repetición NO** (`/private/tmp`, home real, defaults, pasteboard). Procesos que sobreviven al teardown tampoco | ✗ | ✗ | ✗ sin §9.2 (usuario de macOS dedicado o write-deny + reaping) |
| 17 | Telemetría y flags del proveedor (GrowthBook, Statsig, Datadog) | congelado con plantilla | igual | igual | ✓ (el egress directo queda negado; el gateway no los reenvía) |

**Conclusiones del modelo:**

- Las filas 1–4 se cierran con **cualquier** identidad dedicada, porque las API keys no tienen estado de claude.ai ni de ChatGPT. Esto cierra el P1-2 original: los conectores, `RemoteTrigger` y las apps.
- Pero una identidad dedicada **por sí sola** (A) abre o conserva los canales 5–14: con la clave, cualquier hijo puede usar las APIs con estado del propio workspace o proyecto.
- Las filas 14 y 15 no dependen del proveedor. **Hoy ya están abiertas**, y el ciclo anterior no las cubrió: WebFetch y `curl` se consideraron "lectura sin estado compartido", y no lo son, porque un `GET` con datos en la URL o un `POST` a un servicio público escribe estado externo. Ningún diseño de credenciales las cierra sin negar el egress. Se tratan aquí como hallazgo nuevo (P1, §7).

## 4. Arquitectura propuesta (C + alcance por repetición)

### 4.1 Identidades

**Anthropic.** Una **organización de Console dedicada**, por ejemplo `ao-lab-3d`.

- Sin miembros ni facturación compartidos con la cuenta personal. El login de Console puede ser del operador; ninguna clave de esa organización toca claude.ai.
- Dentro, **un workspace por repetición**, `rep-<tag opaco>`, con una clave de service account limitada a ese workspace.
- Así la caché queda aislada por construcción (§2, fila 11) y cualquier estado que se escape al allowlist queda confinado a la repetición.

**OpenAI.** Una **organización de Platform dedicada**, que no es la de ChatGPT del operador.

- Dentro, ~~un proyecto por repetición~~ **un único proyecto** con una clave de service account (enmendado en §9.2).

**Límite en ambas.** Límite de gasto por workspace o proyecto, y ningún modelo más allá de los preregistrados (worker `opus[1m]`, reviewer `gpt-5.6-sol`).

### 4.2 Gateway de credenciales (**NO implementado**; requiere autorización)

Es un proceso del harness, fuera de Seatbelt, en un puerto loopback del laboratorio. Es el **único** destino de red permitido al agente, además del gateway de AO que ya existe.

**Secretos.**

- El gateway lee las claves de la repetición de un **llavero dedicado del laboratorio**: un archivo `*.keychain-db` propio, no el de login, con la contraseña en el llavero de login del operador y acceso restringido al binario del gateway.
- Alternativa equivalente: 1Password CLI (`op run`), si el operador ya lo usa.
- El harness nunca escribe las claves en disco, en logs ni en el entorno del daemon de AO.

**Token de capacidad.**

- El agente recibe `ANTHROPIC_AUTH_TOKEN` (Claude) o la variable `env_key` del *provider* de Codex con un token aleatorio de la repetición, generado por el harness.
- Solo vale en ese gateway, solo durante la repetición, y se revoca en el teardown.
- Por eso filtrarlo no da nada fuera, ni en la repetición siguiente.

**Allowlist de Anthropic** (lo que el agente puede hacer):

- `POST /v1/messages`, `POST /v1/messages/count_tokens`. Nada más.
- Cuerpo:
  - se rechazan los campos `mcp_servers`, `container` y `skills`, y los bloques `document`/`image` con `file_id`;
  - se rechazan las herramientas de servidor (`web_search_*`, `web_fetch_*`, `code_execution_*`, `memory_*`, `tool_search_*`, `computer_*`): solo se permiten herramientas cliente (`custom`);
  - se fuerza `cache_control.ttl` = `5m` (se reescribe `1h`);
  - el modelo debe estar en la lista preregistrada.
- `anthropic-beta`: solo las betas observadas en el ensayo (inventario preregistrado); cualquier otra se rechaza.
- Respuesta: se reenvía tal cual, salvo que se eliminan las cabeceras `anthropic-ratelimit-*` y `request-id`.

**Allowlist de OpenAI:**

- `POST /v1/responses` y nada más. La compactación es local con un *provider* propio; `GET /models` se rechaza (Codex usa su catálogo incluido; **VERIFICAR** que arranca sin él).
- Transporte solo HTTP (`supports_websockets=false`).
- Cuerpo:
  - `store` se fuerza a `false`;
  - se rechazan `previous_response_id`, `conversation`, `background`, `prompt` (plantillas guardadas), `prompt_cache_retention` y `prompt_cache_options`;
  - se rechazan las herramientas alojadas (`web_search*`, `file_search`, `code_interpreter`, `image_generation`, `mcp`, `computer_use*`): solo pasan las que Codex ejecuta en local (`function`, `custom`, `local_shell`, según el inventario del ensayo);
  - modelo preregistrado.
- Respuesta: se eliminan las cabeceras `x-ratelimit-*`.

**Registro.**

- Método, ruta, modelo, betas, herramientas, tamaños y códigos, **sin** contenido ni secretos.
- Se registra en la evidencia privada de la repetición y el runner lo comprueba (§4.5).
- Cualquier petición rechazada es una violación que **aborta** la repetición, igual que las violaciones del gateway de AO.

**Por qué gateway y no clave directa.** Es la única opción que cumple a la vez:

- que el agente no vea ninguna credencial real (pedido explícito del operador);
- que las filas 5–10 se cierren sin depender de cómo defina cada proveedor el alcance de cada API;
- que las filas 8, 11 y 13 se cierren, porque dependen de parámetros que controla el cliente.

Con clave directa (A o B), un hijo del agente puede mandar cualquier parámetro.

**Por qué no es un "sistema casero de secretos".** Las dos CLIs soportan oficialmente un gateway:

- Claude Code: `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`.
- Codex: `[model_providers.<id>] base_url` + `env_key`.

El secreto se guarda con un almacén estándar (llavero de macOS o 1Password). Lo único propio es la política de allowlist. Puede montarse sobre un gateway existente, por ejemplo LiteLLM, que la documentación de Claude Code cita, **o** como un proxy mínimo en el harness al estilo de `gateway.py`. **Recomendación:** proxy mínimo propio y auditado. Un gateway genérico trae muchas más rutas (Files, Batches, Assistants), y cerrarlas es justo lo que hay que demostrar; es más fácil demostrarlo sobre 2 rutas que sobre 200.

### 4.3 Caché de prompts: aislamiento por repetición

La caché es el único canal que el gateway no puede cerrar solo, porque un hijo puede fabricar peticiones legítimas contra `/v1/messages`.

- **Anthropic.** Un workspace por repetición: la caché está aislada por workspace (documentado). ✓ estructural.
- **OpenAI.** La caché se comparte dentro de la organización; que un proyecto la aísle está **sin demostrar**. Dos capas:
  1. el gateway rechaza `prompt_cache_retention` y `prompt_cache_options`, así que la retención se queda en la de memoria, documentada como de minutos y como máximo 1 h (**VERIFICAR** con la documentación vigente de OpenAI);
  2. el runner impone un **enfriamiento de al menos 65 min** entre el fin del último reviewer Codex de una repetición y el primer turno Codex de la siguiente.

  Alternativa sin enfriamiento: una organización de OpenAI por repetición (inviable a mano para ~30 repeticiones).
- **Comprobación empírica (preregistro).** El primer turno de cada agente debe tener lectura de caché = 0. Si no, ABORT: se asume un canal de caché entre repeticiones.

  **Nota:** esto convierte la caché, que hasta ahora era una "covariable no aislable" (ver [3d-preflight.md](3d-preflight.md) §3), en un **control**: cada repetición arranca en frío.

### 4.4 Sandbox: lo que cambia en `agent.sb` y en la sonda

**Se retira el acceso a lo personal, que ya nadie necesita:**

- se elimina el symlink `home/Library/Keychains` → llavero real;
- se niega la lectura de `~/Library/Keychains/**`, `~/.codex/**` (incluidos `auth.json`, `skills` y `plugins`), `~/.claude*`, `~/.config/gh`, `~/.ssh`, `~/.netrc`, `~/.git-credentials`, `~/.docker/config.json` y `~/.aws`/`~/.config/gcloud`;
- `mach-lookup` de los servicios del llavero (`com.apple.securityd*`, `com.apple.SecurityServer`): **VERIFICAR** que negarlo no rompe TLS (usa `trustd`, que es otro servicio) ni git.

**Egress denegado por defecto:** `network-outbound` solo hacia `127.0.0.1:<gateway AO>` y `127.0.0.1:<gateway de credenciales>`.

- Se retira la excepción de `mDNSResponder`: con `base_url` en loopback no hace falta DNS.
- Consecuencias:
  - WebFetch y WebSearch dejan de funcionar, igual en ambos brazos;
  - el fixture debe compilar y pasar tests **sin red**: `GOFLAGS=-mod=readonly` (o vendor), `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off` y telemetría de Go desactivada. La caché de módulos debe ser una copia por repetición o de solo lectura; si no, es un canal (**VERIFICAR** en el ensayo);
  - esto se añade a la restricción de tareas preregistrada.

**Nuevos canales obligatorios de la sonda** (fail-closed, mismo esquema que los 10 actuales):

- `personalCredentials`:
  - `security find-generic-password -s "Claude Code-credentials"` **sin** `-w`/`-g`, descartando la salida y comprobando solo el rc: debe fallar;
  - lectura de `~/.codex/auth.json` y de los archivos listados arriba: debe dar `EPERM`;
  - la sonda nunca imprime ni guarda contenido.
- `egress`:
  - `connect` a un canario TCP del harness en la LAN y a una IP pública fija por puerto 443: DENIED;
  - resolución DNS: DENIED;
  - el único loopback alcanzable son los dos gateways.
- `providerAuth`: dentro del sandbox, `claude` y `codex` relanzados **sin** flags del shim solo pueden autenticar contra el gateway de credenciales.
  - Canario: una sesión sin restricciones en el `HOME` del laboratorio debe reportar `mcp_servers=[]`, ningún conector y `apiKeySource` = gateway.
  - Es la reproducción directa del P1-2: relanzar ya no da nada que el agente no tuviera.

### 4.5 Comprobaciones del runner (antes y después de cada repetición)

**Antes:**

- el workspace y el proyecto de la repetición están vacíos;
- verificación con la clave **en el gateway** (el agente no la ve): 0 files, 0 batches, 0 skills y 0 agentes en Anthropic; 0 files, 0 vector stores, 0 batches y 0 stored completions en OpenAI.

**Después:**

- los mismos listados deben seguir vacíos: el allowlist no dejó crear estado;
- log del gateway sin violaciones;
- lectura de caché del primer turno = 0.

**Teardown:** revocar el token de capacidad y archivar el workspace/proyecto, o desactivar su clave. Nada de lo que quede es alcanzable desde la repetición siguiente.

### 4.6 Controles ya cerrados que se conservan

No cambia ninguno de estos:

- sockets Unix denegados por defecto;
- Docker, Colima, Lima y Podman bloqueados;
- `HOME` del proveedor aislado por repetición (se mantiene; ahora sin llavero);
- MCP y conectores desactivados: flags del shim, `ENABLE_CLAUDEAI_MCP_SERVERS=0`, `--strict-mcp-config`, `--disallowedTools` y la config de Codex sin MCP, apps ni plugins. Se mantienen como defensa en profundidad;
- comprobaciones de herramientas externas por transcript y rollout;
- sonda fail-closed con esquema completo;
- `reviewDepth` efectivo exigido;
- cegamiento: shim, gateway de AO, daemon `-o restrict`;
- tratamiento congelado;
- capacidad como covariable, **sin** reemplazo de muestras;
- toda muestra preregistrada cuenta;
- la deuda `TestP9Crash_C10` sigue separada (no se toca).

**Cambio de método que se preregistra:**

- la vía de autenticación pasa de suscripción a API key a través del gateway, igual en ambos brazos;
- el modelo, el esfuerzo y el costo se congelan en el preregistro;
- las flags del servidor dejan de depender de la cuenta personal, y la plantilla de `HOME` se reconstruye con la nueva identidad.

## 5. ¿Una credencial por experimento o por repetición?

Se demuestra por eliminación:

- **A** (una por proveedor para todo el experimento) **no basta**, con o sin gateway.
  - Sin gateway, las filas 5–10 quedan abiertas con el mismo workspace o proyecto.
  - Con gateway, la fila 11/12 (caché) sigue abierta en Anthropic, porque la caché es por workspace y todas las repeticiones compartirían uno. Contraejemplo concreto: la repetición *k* hace `POST /v1/messages` por el gateway con un prefijo de ≥1024 tokens que codifica un bit y `cache_control`; la *k+1*, en los 5 min siguientes, envía los dos prefijos candidatos y mira cuál trae `cache_read_input_tokens > 0`.
  - Solo se cerraría con un enfriamiento > TTL, que depende de que el TTL sea una cota superior.
- **B** (una por repetición, con la clave al alcance del agente) **no basta**.
  - Deja abierta la fila 8: `mcp_servers` y `web_fetch` de servidor escriben en internet.
  - Deja abierta la caché de OpenAI (fila 11), con retención elegida por el cliente.
  - Expone la credencial al agente, contra lo pedido.
- **C + alcance por repetición** cierra todas las filas; la caché de OpenAI queda con la condición de §4.3.

**Respuesta:**

- **Identidad (organización) dedicada:** una por proveedor para todo el experimento.
- **Credencial y alcance de servidor:**
  - Anthropic: uno **por repetición** (workspace + clave limitada);
  - OpenAI: un único proyecto más el enfriamiento de §4.3 y el registro de prefijos del gateway (enmendado en §9.2).
- **El agente nunca ve la credencial**: solo un token de capacidad por repetición del gateway.
- **Mecanismo adicional necesario:** el gateway con allowlist y el egress denegado (§4.4).

## 6. Qué tiene que crear o configurar el operador (cuando se autorice; **no ahora**)

0. **Un usuario de macOS dedicado al laboratorio** (obligatorio tras la revisión, §9.2 P1-A). El harness, los daemons del laboratorio y los agentes corren con ese usuario. No tiene ninguna sesión personal de Claude, ChatGPT, GitHub ni ssh.
1. **Anthropic Console:**
   - una organización nueva y dedicada al laboratorio, con facturación propia y límite de gasto;
   - sin SSO ni miembros compartidos con otros usos.
2. En esa organización:
   - **un workspace por repetición preregistrada**, más 1 para el ensayo y 1 para la plantilla de `HOME`;
   - una clave de service account limitada a cada workspace.

   Las claves solo se crean en la UI de Console. Para 3D (3 tareas × 2 brazos × 5 = 30 repeticiones) son ~32 workspaces y claves. **Decisión del operador:** aceptar ese trabajo manual, o reducirlo con el enfriamiento de §4.3 (un solo workspace, con enfriamiento > 1 h entre repeticiones; más débil, porque depende del TTL).
3. **OpenAI Platform:**
   - una organización nueva y dedicada (no la de ChatGPT);
   - **un único proyecto** con una clave de service account (enmendado en §9.2: un proyecto por repetición no aísla la caché);
   - límite de gasto;
   - sin ChatGPT ni Codex cloud en esa organización.
4. **Almacenar las claves** en un llavero dedicado del laboratorio o en 1Password, con los nombres que fije el runner. **No** pegarlas en el chat, en el repo ni en `~/.ao`.
5. **Autorizar la implementación** del gateway de credenciales (§4.2) y los cambios de `agent.sb` y la sonda (§4.4).
6. **No tocar** la cuenta personal: los conectores, las apps y `RemoteTrigger` personales siguen como están. El diseño deja de depender de ellos.

## 7. Estado externo que sigue accesible y cómo queda aislado

| Estado | ¿Alcanzable por el agente? | Aislamiento |
|---|---|---|
| Cuenta personal de claude.ai y ChatGPT (conectores, `RemoteTrigger`, Files, conversaciones, memoria, apps, plugins, marketplaces) | No | Ninguna credencial personal en el sandbox (llavero y archivos negados, verificado por la sonda). Relanzar `claude`/`codex` solo alcanza el gateway |
| Organizaciones del laboratorio (Files, Batches, Skills, agentes, vector stores, stored completions) | Solo `/v1/messages` y `/v1/responses` sin estado | Allowlist del gateway, más workspace o proyecto por repetición, más listados vacíos antes y después |
| Caché de prompts del proveedor | Sí, dentro de la repetición | Workspace por repetición (Anthropic); retención limitada y enfriamiento (OpenAI); lectura de caché del primer turno = 0 |
| Rate limits y cuotas de la organización del laboratorio | Solo indirectamente | Cabeceras eliminadas y enfriamiento; los fallos de capacidad son covariable |
| Datos que el proveedor retiene (abuso, retención) | No legibles por API | Fuera del modelo: no es un canal legible |
| Internet público | No | Egress denegado |
| GitHub y otras credenciales personales | No | Llavero y archivos negados; egress denegado |

## 8. Riesgos residuales y preguntas abiertas

1. **Seatbelt comparte UID con el operador.** Un escape con código nativo arbitrario sigue fuera del modelo, igual que hasta ahora. El aislamiento fuerte sería otro usuario del SO o una VM.
   - Con este diseño, el daño de un escape **baja**: el llavero del laboratorio contiene solo claves de alcance por repetición.
   - Pero el llavero de login personal seguiría alcanzable para un escape. Mitigación opcional: correr el laboratorio con otro usuario de macOS.
2. **Caché de OpenAI.** El aislamiento por proyecto no está demostrado. La comprobación de lectura = 0 en el primer turno **no es un control suficiente** (revisión, P1-C). Ver §9.2: registro de prefijos en el gateway, eliminación de los campos de caché y enfriamiento que empieza al revocar el token.
3. **Inventario de rutas y betas.** El allowlist se fija con un ensayo que registre cada petición real de Claude Code 2.1.284 y Codex 0.157.1 contra el gateway. Si alguna ruta necesaria tiene estado, hay que decidir: bloquearla (la función se degrada igual en ambos brazos) o aceptarla con alcance por repetición. La evidencia de Codex viene de `main` (0.158.x), no de la 0.157.1 instalada.
4. **Flags del servidor de Claude Code.** Con API key y `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, y sin egress directo, se espera que no haya flags remotas. Hay que reconstruir la plantilla de `HOME` y demostrar su estabilidad, como en el ciclo 4.
5. **Cambio de autenticación.** El modelo servido y los límites pueden diferir entre suscripción y API. Es igual en ambos brazos, pero invalida la comparación con los lotes anteriores: 3D empieza de cero con la nueva configuración.
6. **Costo:** 30 repeticiones con worker `opus[1m]` más reviewer `gpt-5.6-sol` por API. Se estima en el preregistro.
7. **OpenAI Admin API y claves de proyecto:** falta **VERIFICAR** si permite crear service accounts con clave por API. Si sí, el harness podría provisionarlas, pero eso le daría una clave de administrador, que es una superficie mayor. **Recomendación:** creación manual.


## 9. Revisión adversarial

### 9.1 Codex: **NO EJECUTADA (bloqueada por el entorno)**

Este ciclo se hizo desde un contenedor en la nube. En él:

- no está el binario `codex`;
- `api.openai.com` está bloqueado por el proxy de salida;
- no hay credenciales, y la instrucción era no pedirlas.

Por eso **no hay revisión de Codex**, y **no se afirma lo contrario**. El prompt está listo en [3d-auth-design-codex-review-prompt.md](3d-auth-design-codex-review-prompt.md). Tiene reglas de solo lectura: sin sesiones, sin credenciales y sin tocar producto, producción ni evidencia. Se ejecuta en la máquina del laboratorio.

### 9.2 Revisión adversarial independiente provisional (NO es Codex) — ANULADA por la revisión real de Codex

La hizo un subagente de contexto limpio, en solo lectura, sobre este documento, `3d-preflight.md`, las cadenas del binario de Claude Code 2.1.284 y el código de `openai/codex` `main`. No tuvo acceso al harness del laboratorio.

**Veredicto: NEEDS_CHANGES (P0 0 / P1 4 / P2 6 / P3 5).** Todo lo que sigue queda incorporado al diseño como enmiendas obligatorias.

**Confirmado por la revisión:** las filas 1–4 y 8–10 quedan cerradas con un gateway que solo usa API key y negando el llavero y los archivos personales. **Esto cierra el P1-2 original.**

| # | Hallazgo | Enmienda al diseño |
|---|---|---|
| P1-A | La fila 16 no estaba cerrada. `agent.sb` es allow-by-default y nunca negó **escrituras** fuera del árbol de la repetición: `/private/tmp`, el home real por ruta absoluta, defaults, pasteboard, notificaciones. Ni la sonda ni `analyze.py` miran ahí. Es un canal local entre repeticiones, **abierto también en el cycle6** | **Usuario de macOS dedicado al laboratorio, obligatorio** (deja de ser opcional, §8.1): su home, llavero, `/tmp` por usuario y procesos son distintos de los del operador. Además, escrituras denegadas por defecto salvo el árbol de la repetición, y canarios de escritura fuera del árbol en la sonda |
| P1-B | Nada garantiza que los procesos de una repetición hayan muerto antes de la siguiente. Un proceso desligado de tmux sobrevive y puede pasar estado adelante (puertos loopback, `procargs2`) | En el teardown, enumerar y matar todo proceso del UID del laboratorio (o descendiente del shim) y exigir **cero** antes de la siguiente repetición; si no, ABORT. Cada repetición recibe un puerto nuevo para el gateway de credenciales, ya ocupado antes de lanzar el agente |
| P1-C | "Lectura de caché = 0 en el primer turno" es insuficiente por los dos lados. **No detecta** el sondeo activo, que hacen los hijos en peticiones posteriores. **Da falsos positivos correlacionados con el resultado**: canarios, probes de AO, un segundo reviewer, un worker relanzado o subagentes que acierten prefijos de la propia repetición. Como el lote se detiene, eso es selección | Canarios y probes en workspaces o proyectos **separados**. El gateway guarda un registro por repetición de hashes de prefijo y aborta ante una lectura de caché cuyo prefijo no se envió antes **en la misma repetición**. El gateway quita de las respuestas `cache_read_input_tokens`, `cache_creation_input_tokens` y `cached_tokens` y los registra en privado: así el agente pierde el oráculo. El canal de tiempo se cubre con el aislamiento estructural y el enfriamiento |
| P1-D | Abortar ante cualquier petición rechazada, con las herramientas por defecto, provoca abortos sistemáticos o disparables por el agente. Codex `main` tiene `web_search_mode = Cached` por defecto (`core/src/config/mod.rs:3768-3769`), así que se enviaría `web_search` alojado en cada petición. Claude Code tiene como herramientas de servidor `web_search_20250305` y `advisor_20260301`. Y un agente que va perdiendo podría abortar a propósito | Apagarlas en el cliente, igual en ambos brazos: `--disallowedTools` suma `WebSearch` y `WebFetch`; Codex `web_search = "disabled"` (**VERIFICAR** en 0.157.1). Una petición rechazada **no aborta**: se rechaza, se registra y la muestra cuenta con su resultado (coherente con "toda muestra cuenta"). Solo aborta lo que indique una fuga (credencial, egress o prefijo ajeno) |
| P2-E | El enfriamiento de OpenAI descansa en hechos no verificados: retención ≤ 1 h, ausencia de retención extendida por defecto en el modelo, alcance por organización | El gateway **fija** explícitamente la retención en memoria si la API lo acepta. El enfriamiento cuenta desde la revocación del token o la última petición del gateway. Canario de caché positivo en el ensayo para medir la cota |
| P2-F | El aislamiento de caché por workspace de Anthropic viene de la documentación, no de una medición | Canario en el ensayo: prefijo escrito en el workspace X, leído desde el Y; debe dar 0 |
| P2-G | Claude Code llama a `https://api.anthropic.com` sin pasar por `ANTHROPIC_BASE_URL` para `/api/claude_code/settings`, `policy_limits` y `organizations/metrics_enabled` (esta última se salta `DISABLE_NONESSENTIAL_TRAFFIC`) | Con el egress denegado, fallan. **VERIFICAR** que el arranque no se bloquea ni cambia de comportamiento, y construir la plantilla de `HOME` bajo la misma política de egress |
| P2-H | Detalles del egress denegado: `GOFLAGS=-mod=mod` era incorrecto (ya corregido en §4.4); la caché de módulos compartida es un canal; el `base_url` debe ser `127.0.0.1` literal; `AO_BROWSER_RUNTIME_ADDRESS` queda bloqueado (era un egress residual); ¿alguna ruta del gateway de AO hace que el daemon, que no está en sandbox, salga a la red (confused deputy)? | §4.4 corregido. Auditar las rutas del gateway de AO para ese caso (**VERIFICAR**) |
| P2-I | Endurecimiento del gateway de credenciales | Binario compilado y firmado con hardened runtime y `-o restrict`. Nada de intérprete: una ACL del llavero sobre `python3` confía en cualquier script. Claves fuera de argv y env. Logs y llavero del laboratorio negados al sandbox por ruta. Mensajes de rechazo genéricos |
| P2-J | Regresiones de §4.6 que hay que declarar | Plantilla de `HOME` reconstruida con la identidad del laboratorio. Los symlinks de `skills` y `plugins` de Codex a `~/.codex` pasan a ser **copias preregistradas**. No hay otras regresiones |
| P3 | Nombre del *provider* ≠ "OpenAI" (`is_openai()` compara el nombre, `model-provider-info/src/lib.rs:606`); fijar `model_catalog_url` o verificar `{base_url}/models`; allowlist con `?beta=true`; normalizar el cuerpo de los 429; límite de gasto por workspace obligatorio | Incorporado |

**Revisión de §5 (alcance).**

- **Anthropic:** workspace por repetición confirmado como la única clausura estructural de la caché.
- **OpenAI:** un proyecto por repetición no aporta nada a la caché, que es al menos por organización. Los endpoints con estado ya los cierra el allowlist. **Enmienda:** OpenAI usa **un único proyecto** dedicado. El control es el enfriamiento, más el registro y el borrado de campos del gateway (P1-C, P2-E). Así se evitan ~30 proyectos manuales.
- **Falta, y ahora es obligatorio:** el usuario de macOS dedicado y el *reaping* de procesos (P1-A, P1-B).

**Alcance final:**

- **Anthropic:** una organización dedicada, un workspace y una clave limitada por repetición, más 1 para el ensayo y 1 para los canarios.
- **OpenAI:** una organización dedicada, un proyecto y una clave, más el enfriamiento.
- **Local:** un usuario de macOS dedicado.
- **El agente no ve nunca ninguna clave.**

## 10. Veredicto

**PRECONDITION_3D_AUTH_DESIGN = NEEDS_CHANGES.**

- El diseño cierra el P1-2 original: sin credencial personal en el sandbox, relanzar el proveedor no da nada nuevo.
- La revisión provisional encontró 4 P1 más:
  - dos locales y **preexistentes**: escrituras fuera del árbol y procesos que sobreviven;
  - dos sobre la verificación de la caché y los abortos.
- Todos tienen enmienda de diseño, pero nada está implementado ni medido.
- **La revisión de Codex exigida no se pudo ejecutar.**
- El cierre **requiere un gateway de credenciales**. Según la instrucción, su arquitectura queda documentada (§4.2 y §9.2) y el trabajo se detiene antes de implementarlo.

**Siguiente paso exacto:**

1. El operador ejecuta la revisión de Codex con [3d-auth-design-codex-review-prompt.md](3d-auth-design-codex-review-prompt.md) en la máquina del laboratorio, en solo lectura, y la guarda en `~/.ao/scratch/frente3/reviews/3d-preflight/auth-design/codex/`.
2. Se incorporan sus hallazgos a este documento.
3. Con un veredicto sin P0/P1 abiertos, el operador decide y autoriza:
   - las cuentas y el usuario de macOS (§6, con el alcance final de §9.2);
   - la implementación del gateway y de los cambios de sandbox, sonda y teardown.
4. Solo después: ensayo con canarios (§9.2: P1-C, P2-E, P2-F y P2-G), preregistro nuevo, mini-E2E y revisión de Codex.

3D sigue sin ejecutarse.
