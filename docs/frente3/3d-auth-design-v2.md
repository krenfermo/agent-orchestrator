# Frente 3 / 3D — PRECONDITION_3D_AUTH_DESIGN_V2: frontera desechable por repetición

Fecha: 2026-09-28. Rama: `feat/frente3-3d-prerequisites`, partiendo de `faec13599`.

**Qué es.** La especificación **única y autoritativa** del aislamiento y la autenticación del laboratorio de 3D. Responde a la revisión REAL de Codex del diseño v1 ([3d-auth-design-codex-review.md](3d-auth-design-codex-review.md): **NO-GO**, P0 1 / P1 5 / P2 5 / P3 1).

- [3d-auth-design.md](3d-auth-design.md) (v1) queda **sustituido**. Se conserva como historial: no se implementa ni se cita como norma. Su §9.2 (revisión provisional, no Codex) queda anulada por la revisión real.
- Este documento no enmienda v1: lo reemplaza. Donde v1 y v2 difieren, vale v2 (P3-1 de Codex).

**No se ha hecho nada de lo siguiente:** crear o leer credenciales, crear cuentas, workspaces o VMs, implementar el gateway, tocar el harness, preregistrar, ejecutar un mini-E2E o 3D. El harness del laboratorio (`~/.ao/scratch/frente3/tools/lab3d/`) se leyó en solo lectura.

Lo que depende de un hecho no medido se marca **VERIFICAR**, y cada VERIFICAR tiene su gate en §10.

---

## 1. Definición corregida del experimento

**Pregunta.** ¿El pack de memoria de AO, entregado como lo entrega el producto (`AO_MEMORY_MODE=assisted`), cambia el consumo de contexto y la exploración de los agentes de un workflow de AO sin degradar la calidad, frente al producto sin pack (`off`)?

**Estimando.** Efecto de **intención de tratar** de la asignación `assisted` frente a `off` sobre cada muestra preregistrada, con la configuración congelada de worker y reviewer. Toda muestra planificada cuenta con su resultado (§9).

**Contraste de tratamiento.** El único elemento que difiere entre brazos al lanzar un agente es **el bloque del pack** que AO inserta en su prompt (`projectmemory.ContextPack.Render()`, encabezado `## AO project memory`). Todo lo demás (prompt, entorno, archivos, herramientas, red, identidad del proveedor) es idéntico. Se comprueba por muestra (§9, gate de contraste).

**Unidad.** Una repetición = un workflow de AO completo (worker, verify, reviewer, ciclos de fix) sobre un clon del fixture, dentro de **una frontera desechable propia** (§4).

**Diseño.** Tareas A, B, C y D (control) de [06-benchmark-plan.md](06-benchmark-plan.md) §4, × 2 brazos × N ≥ 5, en orden intercalado por bloques y sorteado con semilla preregistrada (`labcore.plan_from_seed`, sin cambios). Las repeticiones son **estrictamente secuenciales**: nunca hay dos fronteras vivas a la vez.

**Resultados y quién los mide** (cambio respecto a v1: se separan las medidas objetivas de los juicios de agentes expuestos al tratamiento):

| Medida | Fuente | ¿Ciega respecto al brazo? |
|---|---|---|
| M1, M1u, M2 (tokens y llamadas por rol) | Registro de uso del **gateway de credenciales en el host**, por petición, fuera del alcance del agente (§5.2). Los transcripts solo corroboran | Sí: la mide una máquina |
| M3 (exploración antes de la 1.ª edición) | Transcripts y rollouts extraídos de la frontera (I1, I6) | Sí: se calcula de forma mecánica, con reglas preregistradas |
| Q1 (verify) y Q4 (tests ocultos) | Q1: AO. Q4: el supervisor, en una **VM de oráculo** nueva, sin agente y sin red, sobre el diff extraído (§6, paso 8). Los tests ocultos **no** están en la imagen ni en el repo del fixture, así que la reconstrucción de memoria no los indexa y el brazo ASSISTED no los recibe | Sí |
| Q6 (oráculo mecánico de la tarea C y de las trampas del fixture) | El supervisor, con reglas preregistradas: el reviewer **localiza** el defecto sembrado si su veredicto o sus cambios pedidos citan el archivo y el rango de líneas del defecto (o su identificador sembrado). Las trampas (símbolo duplicado, README engañoso) tienen tests ocultos propios en Q4 | Sí: comparación mecánica con un objetivo fijado antes del lote |
| Q5 (revisión humana de diffs) | Humano que ve solo diffs normalizados, en orden aleatorio y sin transcript | Sí, por procedimiento (§3.3) |
| Q2 (veredicto del reviewer) y Q3 (ciclos de fix) | El reviewer de AO, que **sí ve el tratamiento** (su propio pack y el transcript del worker de la misma repetición) | **No**. Son comportamiento del sistema bajo prueba, no una evaluación independiente |

**Consecuencia para el criterio de decisión** (se preregistra; es una corrección de la lectura de [06-benchmark-plan.md](06-benchmark-plan.md) §5): la puerta de calidad independiente es **Q1 + Q4 + Q6** (y Q5 cuando exista).

- En la tarea C, la calidad es Q6: ¿el reviewer localizó el defecto sembrado?
- La regla NO-GO de 06 §5 ("las trampas inducen errores con memoria") se mide con los tests de trampa de Q4.
- Q2 y Q3 se reportan como comportamiento del workflow, con su sesgo declarado. No se presentan como evaluación ciega.

## 2. Por qué el cegamiento de v1 era imposible (P0-1)

v1 exigía que el agente no pudiera saber su tratamiento. Pero el tratamiento **es** texto dentro de su prompt: un agente ASSISTED lee `## AO project memory` y un agente OFF no lo lee. Además, el reviewer lee el transcript del worker. Ningún sandbox puede ocultarle a un modelo lo que recibe como entrada.

Tres cosas estaban mezcladas en v1:

1. **Ocultar el contenido del tratamiento.** Es imposible y **no hace falta**: en producción el agente también ve el pack. Su reacción a él (incluida la de "saber que tiene memoria") forma parte del efecto real que se quiere medir.
2. **Ocultar la asignación y el experimento.** Es posible y **sí hace falta**: la etiqueta, el plan, la semilla, el estado y los resultados de otras repeticiones.
3. **Impedir la coordinación entre repeticiones.** Si el mismo modelo pudiera dejar señales de una repetición a otra, podría sesgar el lote (por ejemplo, un worker OFF que "sabe" que otros tienen memoria). Esto se resuelve con **aislamiento** (§4), no con cegamiento.

## 3. Definición corregida del cegamiento

### 3.1 Qué puede observar el agente (declarado, no es una fuga)

- Todo el contenido que recibe: prompt, incluido el pack en ASSISTED; herramientas; archivos de su propio árbol.
- El reviewer: además, el transcript y el diff del worker de **su misma repetición**, y su propio pack.
- Que existe un encabezado `## AO project memory`, y por tanto que AO tiene memoria de proyecto. No hay forma de ocultarlo sin cambiar el tratamiento.
- Que AO **tiene** modos de memoria. El binario de AO de la imagen contiene `AO_MEMORY_MODE`, `assisted` y `preferred` en sus cadenas y su ayuda. No se oculta: es producto, no asignación.
- **Lo que infiera** a partir de lo anterior. Un agente ASSISTED puede deducir "tengo memoria", y uno OFF puede deducir que AO tiene un modo que él no recibe. Ninguno puede verificar si está en un experimento, cuál es su asignación formal ni qué reciben otros.

### 3.2 Qué debe permanecer oculto (requisitos verificables)

| Oculto | Dónde vive | Control |
|---|---|---|
| Etiqueta formal OFF/ASSISTED | Plan, `results.json`, evidencia privada | **Solo en el host**; nunca entra en la frontera |
| `memoryMode` (y `AO_MEMORY_*`, `AO_CONTEXT_ROUTER`) | Entorno del daemon y `policy_snapshot` en `ao.db` dentro de la frontera | El daemon lo recibe por la cadena `exec` del programa de control (§4.3) y lo retira de su entorno (producto, ciclo 3). Además: daemon firmado `-o restrict`; `ao.db*`, `daemon.lock` y logs del daemon inaccesibles para el agente; gateway de AO con allowlist. Son los controles del lab actual, que se **vuelven a verificar** dentro de la VM (gates G0 y G9). La evidencia de los ciclos 3–6 **no** se transfiere |
| Metadatos de asignación (semilla, plan, `planSha`, mapeo tag → brazo, preregistro) | Host | Nunca entran en la frontera |
| Resultados, estado y evidencia de otras repeticiones | Host (evidencia extraída); proveedor (ámbito de otra repetición) | Frontera desechable (§4) y ámbito de proveedor exclusivo (§5) |
| Evidencia privada de la propia repetición | El registro del gateway, en el host. Los informes de sonda y los marcadores se escriben **dentro** de la VM, en rutas que el agente no alcanza (`agent.sb`), y se extraen | El host no es alcanzable desde la VM. Lo extraído se trata como dato no confiable (§6, paso 8) |
| Documentación y código de AO que describen el experimento | Repo de AO | El árbol de AO **no** está en la imagen; solo el binario |

**Invariante de prompt.** Es una comprobación de **lote**, no de muestra, y usa solo datos del host.

- El gateway guarda en el host el **cuerpo completo** de cada petición. Es contenido del fixture y del laboratorio, sin datos personales.
- Se compara el primer prompt de cada sesión de worker (texto que compone AO antes de que el agente actúe) con una **plantilla OFF por tarea**, fijada en el preregistro a partir del ensayo:
  - OFF debe coincidir con la plantilla;
  - ASSISTED debe ser la plantilla más el bloque del pack, y nada más;
  - la normalización (IDs opacos, puertos, rutas, fechas) se congela en el preregistro.
- Para los mensajes posteriores que compone AO (prompt del reviewer, ciclos de fix), la comprobación se limita a lo que AO escribe **fuera** de las partes que citan salida del agente: el bloque del pack aparece si y solo si el brazo es ASSISTED, y OFF no contiene ningún texto de AO sobre memoria.
- Hoy AO no entrega ningún pack con `off` (mini-E2E: manifests "—"). **VERIFICAR** (gate G5) que no hay ninguna línea de enlace ni puntero al pack fuera del bloque, y que los segmentos que escribe AO y los que citan al agente se pueden delimitar.

**Diferencial de lanzamiento** (`analyze.py`): es un **diagnóstico de lote**.

- Busca 0 diferencias consistentes con el brazo en entorno, sistema de archivos y red al lanzar los agentes.
- Solo el **primer lanzamiento del worker** (antes de cualquier acción del agente) tiene valor de precondición.
- En lanzamientos posteriores, el árbol ya refleja lo que hizo el agente tratado, así que las diferencias se reportan, pero no detienen nada (§9).

### 3.3 Cegamiento de quien evalúa

- Métricas mecánicas (M1–M3, Q1, Q4): el código de análisis se congela en el preregistro y se ejecuta sobre todo el lote a la vez, sin mirar resultados parciales.
- Q5: los diffs se exportan sin transcript, sin pack, con nombres de rama y rutas normalizados y en orden aleatorio. El revisor humano no es el operador que tiene el mapeo.
- Q2/Q3: no son ciegos (§1).

### 3.4 ¿Placebo en OFF? No se adopta

Se evaluó dar a OFF un bloque de contexto de forma y tamaño iguales al pack. Se **rechaza**:

1. **Cambia la pregunta.** La decisión de producto es "pack o nada". Con placebo, el experimento mediría "pack frente a contexto de relleno", que es otra pregunta: no dice qué pasa al activar la memoria en producción.
2. **Contamina la métrica primaria.** El placebo añade tokens de entrada en cada llamada de OFF, así que M1 sube en OFF por construcción. Restarlos supondría que el placebo no afecta a la conducta, y eso es falso: el contexto irrelevante distrae y dirige lecturas.
3. **No hay placebo neutro.**
   - Si se deriva del repo, **es memoria**, y el baseline deja de ser "sin memoria".
   - Si es irrelevante (aleatorio o de otro repo), el modelo lo distingue, así que no ciega.
   - Si además se presenta como `## AO project memory`, induce a error a OFF y degrada su baseline, lo que sesga a favor del pack.
4. **No hace falta.** Lo que el placebo intentaría evitar (que el agente se sepa tratado) forma parte del efecto real en producción (§2.1). Lo que sí sesgaría el lote (coordinación entre repeticiones y evaluación no ciega) se cierra con aislamiento (§4–§5) y con medidas objetivas (§1).

Si alguna vez interesa separar el efecto del **contenido** del efecto de la **longitud**, se haría como un brazo adicional, preregistrado aparte (p. ej. un pack de otro repo con el mismo tamaño). No forma parte de 3D.

---

## 4. Frontera de aislamiento: una VM desechable por repetición

### 4.1 Principio

Un **canal** entre repeticiones es un estado que la repetición *k* puede **escribir** y otra *k'* puede **leer**. Hay cuatro clases:

| Clase | Ejemplos | Cómo se cierra en v2 |
|---|---|---|
| **L — local** | archivos, `/tmp`, defaults, pasteboard, notificaciones, Mach/XPC, IPC y memoria compartida, llaveros, procesos huérfanos, cachés de módulos | **Estructural**: todo lo que *k* ejecuta vive en la VM de *k*, que se destruye antes de crear la de *k+1* (§4.3) |
| **P — proveedor** | caché de prompts, Files, Batches, Skills, respuestas guardadas, cuotas | Ámbito de proveedor **exclusivo** de *k*, más un gateway que solo deja pasar inferencia sin estado (§5) |
| **I — internet público** | pastebins, gists, webhooks, MCP remoto, `web_fetch` de servidor | La VM solo alcanza el gateway, y el gateway bloquea las herramientas de servidor (§5.2) |
| **H — host y operador** | cuentas personales, llavero, `~/.ssh`, `gh`, el AO vivo del operador, sockets del host | La VM no tiene montajes ni reenvíos, y su única salida es el gateway (§4.4) |

**Ruido exógeno no es un canal.** La carga global del proveedor, sus caídas o un cambio de modelo en el servidor no los puede **escribir** *k*. Afectan igual a los dos brazos en promedio (intercalado), se registran como covariables y no se intentan aislar. Esto responde a la reserva de Codex en P1-3 ("no puede demostrar aislamiento del throttling global").

### 4.2 Alternativas comparadas

| | **v1**: usuario de macOS dedicado compartido + Seatbelt + gateway + ~32 workspaces + enfriamiento de 65 min | **A**: usuario de macOS desechable por repetición (mismo kernel) | **B**: **VM macOS desechable** por repetición | **C**: VM Linux desechable por repetición |
|---|---|---|---|---|
| Estado local de usuario (Mach/XPC, defaults, pasteboard, IPC) | Abierto (P1-1). Cerrarlo exige una política Mach/XPC deny-by-default que no existe ni se puede probar completa | Mejor (brokers por UID), pero `/private/tmp`, IPC global y dominios de launchd siguen compartidos; borrar un usuario en macOS deja restos | **Cerrado** por destrucción | **Cerrado** por destrucción |
| Procesos huérfanos | Abierto (P1-2): el runner comparte UID | Matar el UID funciona, pero reparentado y nombres de ejecutable siguen siendo un problema | **Cerrado**: se apaga y se borra la máquina | **Cerrado** |
| Credenciales y estado personales | Denegar ruta a ruta (lista abierta) | Fuera de alcance, salvo escape del kernel | **No existen** en la imagen | **No existen** |
| Custodia del secreto (P2-1, P2-2) | Conflicto entre llaveros de usuarios; `op run` expone el entorno a `procargs2` | Igual que v1 | **Desaparece**: el secreto vive en el host y el agente está en otro kernel | Igual que B |
| Egress | Reglas de Seatbelt (allow-default) | Seatbelt o pf por UID | Filtro **fuera** de la VM, más Seatbelt dentro | Filtro fuera de la VM, más nftables por UID dentro |
| Cegamiento dentro de la repetición | Lab actual (Seatbelt, shim, gateway de AO, daemon `-o restrict`, sonda) | Igual | Se parte del lab actual (shim, gateway de AO, daemon `-o restrict`, sonda). `agent.sb` pasa a **denegar escrituras por defecto** y recibe parámetros nuevos para la VM. Todo se vuelve a verificar: la evidencia de los ciclos 3–6 no se transfiere | Hay que portarlo (separar UIDs, `/proc`, sonda nueva) y **VERIFICAR** AO sobre Linux |
| Plataforma frente a la evidencia previa | Igual | Igual | Igual (macOS) | Cambia |
| Mecanismos nuevos que hay que auditar | Muchos (política Mach, reaping, ACL del llavero, ledger de prefijos, enfriamientos) | Automatizar crear y borrar usuarios con admin en el Mac personal | Hipervisor, imagen base y filtro de red | Hipervisor, imagen, filtro y todo el cegamiento portado |
| Coste en el host | Bajo | Bajo, pero con cambios de sistema en el Mac personal | **Alto: ~60–80 GB de disco y ~8 GB de RAM** | Medio: ~10–15 GB de disco y ~4–6 GB de RAM |
| Validez | Depende de cerrar P1-1/P1-2, que v1 no logra | Media | **Alta** | Alta, sujeta al port |

**Elección: B, una VM macOS desechable por repetición.** Es la que menos mecanismos nuevos introduce: cambia una larga lista de reglas locales (Seatbelt allow-default, política Mach, reaping, llaveros) por **una sola** frontera cuya propiedad es trivial de enunciar y de comprobar (la máquina deja de existir). Además, parte del cegamiento dentro de la repetición que ya existe, en la misma plataforma. Hay que adaptarlo y volver a verificarlo, pero no portarlo.

**C** queda como alternativa si el operador no puede liberar disco para B. Su coste real es portar y volver a verificar el cegamiento, no la VM.

**Lo que la VM no resuelve.** La VM cierra las clases L y H y, con el filtro, la I. **No** cierra la clase P: la caché, las cuotas y el estado del servidor viven en el proveedor, y destruir la VM no los borra. Por eso siguen haciendo falta un **gateway de credenciales** y un **ámbito de proveedor por repetición** (§5). Lo que desaparece son el enfriamiento de 65 min, el registro de prefijos, el borrado de campos de caché, el llavero entre usuarios y la política Mach.

### 4.3 Componentes y fronteras de confianza

```
HOST (macOS del operador) — confiable
  usuario de host dedicado "aolabsup" (no admin; sin las cuentas del operador)
    supervisor del lote (plan, semilla, mapeo, preregistro, evidencia privada)
    gateway de credenciales, UNA instancia por repetición
      (secretos del ámbito S_k, en el llavero propio de aolabsup; token T_k; allowlist; registro)
    parsers de la evidencia extraída
    todo bajo Seatbelt deny-by-default: sin acceso al home ni al llavero del operador
  filtro de red del segmento de la VM (lo carga el operador durante el lote)
        │  host → VM: ssh SOLO al usuario "ctl", con un comando forzado
        │  VM → host: solo HTTP al gateway_k (+ DHCP del hipervisor)
        ▼
VM_k (clon de la imagen base sellada) — NO confiable; se destruye al terminar
  "ctl": destino único del ssh; clave en un AuthorizedKeysFile propiedad de root;
         PermitUserRC no; ForceCommand = programa de control fijo, propiedad de root
  "lab": sin admin, sin sudo ni su; ejecuta el daemon de AO y los agentes
    daemon AO (firmado -o restrict) + gateway de AO + servidor tmux
    shim + agent.sb + sonda → worker (Claude Code), reviewer (Codex)
  sin ninguna cuenta admin con contraseña conocida; root solo por el programa de control
```

- **Canal de control** (hallazgo de la comprobación interna: sin esto, el agente podía secuestrar el ssh del supervisor).
  - El supervisor **nunca** entra por ssh como `lab`. Entra como `ctl`, y el `ForceCommand` ejecuta un programa de control fijo, propiedad de root, con un conjunto cerrado de verbos: arrancar el daemon, llamadas a la API de AO, estado, extraer y apagar.
  - Ese programa lanza los procesos de `lab` con `env -i` y `exec` directo, **sin shell de login ni lectura de dotfiles**. Lee `ao.db` y los logs como root, en solo lectura.
  - Nada que `lab` pueda escribir se ejecuta ni se interpreta con privilegios: ni dotfiles, ni `~/.ssh/rc`, ni configuración de shell.
  - `agent.sb` pasa a **denegar escrituras por defecto**, salvo en el árbol de la repetición y el `TMPDIR` del agente. Con eso, el agente tampoco puede tocar el home real de `lab`.
  - Gate G9.
- **Usuario de host dedicado** (`aolabsup`). El gateway y los parsers procesan HTTP hostil y datos extraídos de una VM comprometible. Por eso no corren como el operador: corren con un usuario propio y bajo un perfil deny-by-default. Las claves del laboratorio están en el llavero de ese usuario, que se desbloquea a mano al empezar el lote. Esto es lo que Codex pedía en P2-1: una identidad confiable distinta del agente y del operador.
- **Imagen base sellada.** Contiene:
  - macOS y las herramientas de línea de comandos;
  - Go con la caché de módulos del fixture (el build es sin red);
  - git, tmux y python3;
  - el binario de AO preregistrado;
  - Claude Code y Codex en versiones **fijas**, sin autoactualización;
  - el fixture **sin** los tests ocultos;
  - la plantilla de `HOME` de Claude, construida con la identidad del laboratorio;
  - el lab (shim, `agent.sb`, sonda, gateway de AO) y el programa de control.

  No contiene nada del operador: ni cuentas ni llavero con secretos, ni `~/.ssh`, `gh`, código ni documentación de AO. Su digest se preregistra y sustituye al "fingerprint FROZEN": la autoactualización de Claude del 28-sep ya no puede ocurrir a mitad de lote.
- **Orden de construcción** (B0):
  - **B0a.** Instalación con red.
  - **B0b.** Se pasa a la red filtrada (solo el gateway, hacia el workspace de plantilla), se inicializa la plantilla de `HOME` hasta que queda estable en dos corridas seguidas, y se limpia todo lo transitorio.
  - **B0c.** Apagado y sellado (digest).

  Después del sellado, la imagen **nunca vuelve a arrancar**: cada repetición y cada gate arrancan un clon.
- **Nada compartido entre host y VM** salvo el tráfico al gateway y el ssh de control: sin carpetas compartidas, sin reenvío de puertos, de sockets ni de agente ssh, sin portapapeles ni pantalla compartidos.

### 4.4 Red de la VM

- El filtro vive **fuera** de la VM, en el host o en el hipervisor: desde la VM solo se permiten `gateway_k:puerto` y el DHCP del hipervisor. No hay DNS ni internet, ni acceso a otros puertos del host (el AO vivo del operador, AirPlay, etc.) ni a la LAN. **IPv6 queda desactivado o filtrado igual**, y también el multicast (mDNS/Bonjour) y el `bootpd`/DNS del puente, salvo DHCP.
- Dentro de la VM, `agent.sb` conserva su política actual (sockets Unix denegados por defecto, loopback solo al gateway de AO) y **añade** la negación de todo `network-outbound` IP salvo `gateway_k`. Es una segunda capa, no la primera.
- El usuario `lab` no es administrador, así que no puede cambiar la red de la VM desde dentro.
- Consecuencias, iguales en ambos brazos y preregistradas: sin WebFetch ni WebSearch, sin navegador de AO (`AO_BROWSER_RUNTIME_ADDRESS` inalcanzable) y con build y tests del fixture sin red.
- **VERIFICAR** (gate G2): el mecanismo concreto del filtro (pf sobre la interfaz del hipervisor, o la red restringida del propio hipervisor) y que el sondeo desde la VM (puertos del host, LAN, IP pública, DNS) solo alcanza el gateway.

---

## 5. Proveedor: ámbito por repetición y gateway

### 5.1 Identidades y ámbitos

- **Anthropic**: una organización de Console dedicada, sin relación con la cuenta personal ni con claude.ai.
  - Un **workspace por repetición y por rol que use Anthropic** (`S_k`), con una clave limitada a ese workspace, y **límites de tasa y de gasto propios** fijados en Console.
  - Aparte: workspaces para el ensayo, para los canarios y para construir la plantilla.
  - La caché está aislada por workspace (documentado; **VERIFICAR** con el gate G3). Un workspace nuevo arranca con la caché fría, así que **cada repetición empieza en frío**. Esto cierra el canal y, además, la confusión de la métrica M1u que el plan original quería tratar con esperas de 6 min.
- **OpenAI** (reviewer Codex): una organización de Platform dedicada.
  - Hay un **gate de decisión, G4**, antes del preregistro, porque ni la documentación ni el código demuestran un aislamiento de caché por proyecto (P1-4):
    - **G4 pasa** (la caché no cruza proyectos, medido con control positivo): **un proyecto por repetición**, con clave de service account y límites propios. Mismo razonamiento que en Anthropic.
    - **G4 falla o no concluye**: **el reviewer pasa a ser Claude**. Es un cambio de configuración que se decide y preregistra **antes** del lote, igual en ambos brazos, y se declara como limitación (no se evalúa el reviewer Codex).
      - El reviewer usa un **workspace propio de la repetición**, distinto del del worker. Así, la caché del reviewer no hereda la que dejó el worker, y la comprobación de caché fría (§6, paso 12) no da falsos positivos.
      - El reviewer Codex siempre tuvo su propia caché (otro proveedor), así que esto mantiene la comparabilidad.
  - **No** se usan enfriamientos como control de caché de OpenAI: no hay una cota documentada que sea dura.
- **Ningún ámbito se reutiliza.** El supervisor lleva un registro en el host de ámbitos usados y se niega a arrancar una repetición con uno ya usado.

### 5.2 Gateway de credenciales (**no implementado**)

Es un proceso del host, **una instancia por repetición**, en un puerto nuevo. Solo escucha en la interfaz de la VM.

**Secretos.**

- Corre como `aolabsup` (§4.3). Lee de **su** llavero **solo** las claves de `S_k`, y las usa únicamente para construir la cabecera de autorización hacia el proveedor.
- El secreto nunca entra en la VM. El adversario está en otro kernel: `procargs2`, el entorno de `op run` y las ACL del llavero dejan de ser un problema de frontera (P2-1, P2-2).
- El resto de requisitos siguen: el secreto nunca va a disco, logs ni argv.

**Token de capacidad.**

- `T_k` es aleatorio (256 bits), **uno por rol** (worker, reviewer).
  - Los dos están en la VM, así que el worker podría usar el del reviewer.
  - La atribución por rol de M1 y M2 se **cruza** con el ID de sesión nativo de cada petición y con el mapa sesión → rol de AO.
  - Una discrepancia se reporta como manipulación de la atribución (covariable por brazo), no se corrige.
- Solo vale en la instancia *k*, que muere en el teardown.
- Filtrarlo no da nada fuera de la repetición. Un token de otra repetición no puede estar nunca dentro de `VM_k`: si apareciera, es prueba de una fuga (§9).

**Política** (petición → reconstrucción estricta, P2-3):

- Solo `POST /v1/messages` y `POST /v1/messages/count_tokens` (con `?beta=true` si el inventario lo exige) en Anthropic, y `POST /v1/responses` en OpenAI.
- Se parsea contra un **esquema tipado**, y a partir de él se reconstruye **una petición nueva**, nunca se reenvía la original.
- Se rechazan:
  - claves JSON duplicadas;
  - `Content-Encoding` y `Transfer-Encoding` que no sean identidad;
  - destinos en forma absoluta;
  - parámetros de consulta no listados;
  - cuerpos de más de X MB.
- Cabeceras permitidas solo por lista (p. ej. `anthropic-version` y las `anthropic-beta` del inventario del ensayo). `Host` y la autorización las pone el gateway.
- Upstream sin redirecciones y con respuesta limitada en tamaño.
- **Se rechazan** los campos con estado o con salida a la red:
  - Anthropic: `mcp_servers`, `container`, `skills`, bloques con `file_id`, herramientas de servidor;
  - OpenAI: `previous_response_id`, `conversation`, `background`, `prompt`, `prompt_cache_retention`, `prompt_cache_options`, herramientas alojadas.
- `store:false` se fuerza.
- Modelos solo por **ID exacto** preregistrado, no por alias.
- Se prueba por mutación: cada campo prohibido en cada posición de anidamiento.

**Respuestas.**

- Pasan **sin modificar**, con límite de tamaño.
- **Cambio frente a v1:** ya no se eliminan los campos de caché ni las cabeceras de rate limit.
  - Con un ámbito exclusivo y frío, lo que el agente vea de caché lo escribió él mismo.
  - Tras la pausa de §5.3, las cabeceras no traen nada de otra repetición.
  - Quitarlos alteraría la conducta del cliente: Claude Code usa `usage` para su contabilidad de contexto y su compactación, y los reintentos dependen de `retry-after`.

**Presupuesto.** Límite por repetición de peticiones, tokens y coste, **igual para ambos brazos**. Se fija en el preregistro en ≈ 3 × el máximo observado en el ensayo **en cualquiera de los dos brazos**.

- El pack añade tokens, así que ASSISTED se acerca antes al límite. Es parte del tratamiento, con una salvaguarda:
  - se reportan por brazo las muestras que tocan el presupuesto;
  - si alguna lo toca, se hace un análisis de sensibilidad.
- Agotarlo es un error visible para el agente y **cuenta** (§9).

**Registro** (en el host, fuera del alcance de la VM):

- Por petición: rol, ruta, modelo, betas, herramientas declaradas, tamaños, código y **`usage` completo** (incluidos `cache_read`/`cache_creation`/`cached_tokens`).
- También nombres de herramienta y **rutas** objetivo de los `tool_use` de la respuesta, normalizadas al worktree (I1).
- Además, el **cuerpo completo** de cada petición, cifrado en la evidencia privada del host. Lo necesita el invariante de prompt (§3.2). Es contenido del fixture y del laboratorio; no hay datos personales ni secretos, porque la autorización se añade después.
- Es la **fuente primaria** de M1, M1u y M2, y un respaldo de M3, resistente a que el agente manipule sus transcripts.

**Por qué el gateway sigue siendo necesario aunque haya VM.** Con una clave directa dentro de la VM, el agente podría usar `mcp_servers`, `web_fetch` de servidor o herramientas alojadas: el proveedor funcionaría como proxy de egress, un canal I que el filtro de red no ve. También podría crear estado del servidor. El gateway es lo único que cierra la clase I en el proveedor.

**Config de cliente** (igual en ambos brazos, para que el agente legítimo no provoque denegaciones):

- Claude Code: `ANTHROPIC_BASE_URL` = gateway, `ANTHROPIC_AUTH_TOKEN` = `T_k`; `--disallowedTools` añade `WebSearch` y `WebFetch`.
- Codex: `model_providers.<lab>`, cuyo nombre no es "OpenAI", con `base_url` = gateway, `env_key` → `T_k`, `requires_openai_auth=false`, `supports_websockets=false`, `web_search="disabled"` y `forced_login_method="api"`. **VERIFICAR** todo en 0.157.1 (gate G5).

### 5.3 Cuotas y rate limits (P1-3)

Regla única. Para cada límite *L* del proveedor que comparten los ámbitos (organización), con ventana *W*, debe cumplirse una de dos:

1. **La ventana se vacía antes de la siguiente repetición.** Pausa preregistrada *G* ≥ *W* entre la última petición de *k* y la primera de *k+1* (límites por minuto: *G* de pocos minutos, no 65).
2. **El límite es inalcanzable.** Σ presupuestos del lote < *L* con margen: límites diarios y mensuales, y límite de gasto de la organización.

Además, cada workspace o proyecto tiene sus propios límites de tasa y de gasto fijados en el proveedor, **por debajo** de los de la organización: así ninguna repetición puede llevar a la organización a su límite (control del proveedor, no solo del gateway).

**VERIFICAR** (gate G6): el inventario de límites de cada organización (por minuto, diarios, gasto) y que los límites por workspace o proyecto existen y se aplican.

Resultado: *k* no puede dejar a la organización en un estado que *k+1* observe. Un 429 o una latencia alta que ocurran de todos modos son ruido exógeno (§4.1): se registran por brazo y se reparten con el intercalado.

---

## 6. Ciclo de vida exacto de una repetición

**Una vez por lote (antes del preregistro):**

- **B0.** Construir y sellar la imagen base en el orden B0a → B0b → B0c de §4.3. La plantilla de `HOME` se inicializa **antes** del sellado, con red filtrada y a través del gateway (sin llamadas directas a `api.anthropic.com`). Se registra el digest de la imagen.
- **B1.** Gates G0–G9 (§10) en **clones y ámbitos de canario**, nunca en los de las muestras (P2-5).
- **B2.** Preregistro publicado **y empujado a `origin`** antes del lote. Incluye: digest de imagen, sha del gateway, esquema del allowlist, IDs de modelo, presupuestos, *G*, lista opaca de ámbitos, resultado de G4 (con qué reviewer se corre), plan (`planSha`), reglas de §9 y código de análisis congelado.

**Por repetición *k*** (el supervisor ejecuta los pasos en orden; ninguno se solapa con otra repetición):

1. **Gates previas (host).**
   - No existe ninguna VM del laboratorio (`VM_{k-1}` borrada y comprobada).
   - La instancia del gateway *k−1* terminó y sus tokens ya no valen.
   - Pasó el tiempo *G* desde la última petición de *k−1* (se **espera**, no se aborta).
   - Memoria libre suficiente (se **espera**).
   - Gasto acumulado + presupuesto de *k* ≤ tope.
   - Digest de la imagen correcto.
2. **Ámbito.**
   - El supervisor toma el siguiente ámbito sin usar, `S_k` (por orden, no por brazo), y lo marca como usado.
   - Genera `T_k` por rol.
   - Arranca `gateway_k` en un puerto nuevo con los secretos de `S_k`.
3. **Clonar y arrancar** `VM_k` desde la imagen. La red queda restringida desde el primer arranque.
4. **Aprovisionar** (host → `ctl` por ssh, con los verbos del programa de control; lo único que entra):
   - `T_k` y la URL del gateway, en la config del shim;
   - el tag opaco y el puerto;
   - el brazo, **solo** como entorno del `exec` del daemon. Lo lanza el programa de control con `env -i`, sin shell, y ningún proceso intermedio sobrevive. **VERIFICAR** con `procargs2` desde la sonda, igual que hoy.
5. **Precondiciones dentro de la VM:** fixture (SHA y árbol limpio), binario de AO, versiones de Claude y Codex, config de Codex, plantilla de `HOME`, `codex mcp list` = `[]`.
6. **Ejecución** (el supervisor habla con AO solo a través del programa de control):
   - el daemon arranca y reconstruye la memoria (ambos brazos);
   - **exposición** = la **primera petición de cualquier tipo** que llega a `gateway_k`, incluidos los probes de proveedor que el daemon lanza al arrancar. Todo lo anterior es "antes de la exposición" (§9, S1);
   - se crea el workflow con `reviewDepth=light`;
   - worker y reviewer se lanzan con el shim y `agent.sb`, con la sonda en cada lanzamiento;
   - el `gateway_k` registra el uso.
7. **Fin:** la run llega a un estado terminal o al timeout preregistrado. Después, exploración estable, como hoy.
8. **Extracción** (el programa de control, como root, empaqueta; el host inicia la copia):
   - Se copian `ao.db`, logs del daemon, informes de sonda y marcadores (rutas que el agente no alcanza), más transcripts, rollouts y el diff (que el agente **sí** puede escribir).
   - Todo se trata como **dato no confiable**: los parsers corren como `aolabsup` bajo Seatbelt; sin symlinks ni rutas fuera del destino; con límites de tamaño; hasheado.
   - Si un artefacto que el agente puede escribir falta, es enorme o está corrupto, es un **dato faltante** de esa muestra, no un fallo del instrumento (§9).
   - **Q4:** el supervisor clona una **VM de oráculo** nueva desde la imagen, sin agente y sin red. Le aplica el diff extraído, **inyecta los tests ocultos** (que solo existen en el host) y ejecuta el comando de test preregistrado. Después la destruye.
   - El código del agente **nunca** se ejecuta en el host.
9. **Revocación:** `gateway_k` termina; su registro se cierra y se hashea.
10. **Destrucción:** apagado forzado de `VM_k` y borrado de su disco. Se comprueba que el hipervisor ya no la lista y que su directorio no existe.
11. **Retirada del ámbito:** `S_k` no se vuelve a usar (registro del host). Su clave se desactiva y el workspace se archiva al final del lote (operador, o `admin` en un proceso de control separado; nunca durante una muestra).
12. **Post-checks (host, sobre la evidencia extraída):**
    - `reviewDepth`, `externalContext`, y `memoryMode` entregado = asignado;
    - sondas: la del primer lanzamiento del worker cuenta como precondición; las posteriores solo en rutas que el agente no puede escribir (§9);
    - exposición de herramientas;
    - **primera petición en cada workspace o proyecto de la repetición con `cache_read`/`cached_tokens` = 0**. Cada ámbito es nuevo y exclusivo de un solo rol, así que otro valor prueba una fuga;
    - ninguna petición con un token de otra repetición;
    - el invariante de prompt y el diferencial de lanzamiento son **de lote** (§3.2) y se calculan al final, con el código congelado.

    Resultado: VALID, o STOP según §9.

## 7. Qué puede persistir

| Persiste | Dónde | ¿Lo puede escribir una repetición? | Por qué es aceptable |
|---|---|---|---|
| Imagen base sellada | Host | No (nunca arranca; digest comprobado antes de cada clon) | Idéntica para todas |
| Supervisor, gateway, preregistro, plan, mapeo | Host | No | Fuera de la VM |
| Evidencia extraída de *k* | Host | Sí, su contenido, que es dato no confiable | Nunca vuelve a entrar en ninguna VM |
| Registro de ámbitos y tokens usados | Host | No | Solo el supervisor |
| Workspaces o proyectos retirados, con lo que el proveedor guarde en ellos | Proveedor | Sí, dentro de `S_k` (limitado a caché por el allowlist) | Nadie vuelve a usar `S_k` |
| Gasto acumulado de la organización | Proveedor | Sí, hasta su presupuesto | Σ presupuestos < tope (§5.3) |
| Ventanas de rate limit | Proveedor | Sí | Vacías tras la pausa *G* |
| Retención del proveedor por abuso o cumplimiento | Proveedor | Sí | Ninguna API la hace legible. **Excepción:** *k* podría provocar a propósito una sanción de política (throttling o suspensión de la organización), que *k+1* observaría. Es un canal de un bit, irreversible y de un solo uso. Se trata como denegación de servicio: los errores de política del proveedor se registran en el host, y una suspensión de la organización detiene el lote (S3, instrumento) |

**No persiste nada más.** Ninguna VM, disco, proceso, archivo escrito dentro de la VM (salvo la evidencia extraída), token ni objeto de proveedor creado por el agente (el allowlist no permite crearlos).

---

## 8. Resolución de los hallazgos de Codex

| Hallazgo | Resolución en v2 | Estado |
|---|---|---|
| **P0-1** El cegamiento exigido es imposible | Nueva definición: se oculta la asignación, no el contenido (§2–§3); sin placebo (§3.4); separación de medidas objetivas y juicios expuestos (§1) | **Cerrado** (metodológico) |
| **P1-1** Estado de usuario de macOS entre repeticiones | La VM se destruye; nada local sobrevive (§4) | **Cerrado** por diseño; la prueba es la destrucción (paso 10), más G1 |
| **P1-2** Procesos huérfanos | El supervisor está en el host y la VM se apaga y borra; no hay barrido por UID (§4.3, paso 10) | **Cerrado** por diseño |
| **P1-3** Cuotas compartidas | Regla ventana/presupuesto con límites por ámbito del proveedor (§5.3); lo exógeno es covariable (§4.1) | **Cerrado** por diseño, sujeto al inventario de G6 |
| **P1-4** Enfriamiento de la caché de OpenAI | Sin enfriamiento. Proyecto por repetición **solo si** G4 demuestra aislamiento; si no, reviewer Claude (§5.1) | **Cerrado** por decisión preregistrada; el camino concreto depende de G4 |
| **P1-5** Peticiones rechazadas y selección de muestras | Norma única en §9: una denegación es un fallo visible y la muestra cuenta. El STOP solo se basa en evidencia que el agente **no** puede escribir. Número de relotes acotado y preregistrado, sin agregar lotes. Nunca se reemplaza una muestra | **Cerrado** |
| **P2-1** Llavero y usuario dedicado | El secreto está en el llavero de `aolabsup`, una identidad del host distinta del operador y del agente, y el adversario está en otro kernel (§4.3, §5.2) | **Cerrado** |
| **P2-2** `op run` y el entorno | Igual: el entorno del host no es legible desde la VM | **Cerrado** |
| **P2-3** Protocolo del gateway | Parseo tipado y reconstrucción, cabeceras por lista, sin compresión, sin redirecciones, límites, mutación en cada posición (§5.2) | Requisito de implementación |
| **P2-4** Comprobar que la cuenta está vacía antes y después | **Se retira como gate**: no se puede hacer completo. Se sustituye por ámbito nuevo y nunca reutilizado, allowlist que no deja crear estado, y mutación del gateway. Opcional: inventario informativo en un proceso de control separado, fuera de las muestras | Cerrado por sustitución |
| **P2-5** El canario contamina la muestra | Todos los canarios, en clones y ámbitos propios y antes del preregistro (B1) | **Cerrado** |
| **P3-1** v1 contradictorio | v1 sustituido entero por este documento | **Cerrado** |
| Egress a internet público | Filtro fuera de la VM, más Seatbelt, más el gateway sin herramientas de servidor (§4.4, §5.2) | Cerrado por diseño, sujeto a G2 |
| Credenciales personales ajenas a los LLM | No existen en la imagen | **Cerrado** |
| Regresión de §4.6 de v1 | Se declara: **laboratorio nuevo** (VM, autenticación por gateway, sin egress, sin WebFetch, WebSearch ni navegador, plantilla nueva). La evidencia previa no se transfiere; los controles se vuelven a verificar con probes y mutaciones en la VM y con un preregistro nuevo | Declarado, no se asume nada |

## 9. Reglas normativas de muestra (única versión)

**Principio.** Una muestra nunca se excluye. El lote solo se detiene por evidencia que **el agente no puede escribir**. Así, el agente no puede, ni intentándolo, provocar la salida de una muestra en función de su resultado.

1. Cada posición del plan se ejecuta **una vez**. No hay reintento ni reemplazo, por ninguna causa.
2. **Exposición** = la primera petición de cualquier tipo que llega a `gateway_k` (§6, paso 6).
3. **Cuenta con su resultado** (completed, failed, needs_attention, timeout) todo lo que ocurra después de la exposición, incluido:
   - denegaciones del gateway (campo o herramienta prohibidos);
   - presupuesto agotado;
   - 429, 5xx o errores del proveedor;
   - intentos **bloqueados** de egress, sockets o rutas prohibidas;
   - crash del agente;
   - **cualquier anomalía en datos que el agente puede escribir**: transcripts, rollouts o diff borrados, enormes o corruptos; etiquetas o palabras como "assisted" escritas por el agente en su árbol o en código; una sonda posterior que falle al leer el árbol del agente.

   Las métricas primarias salen del registro del gateway. Lo que falte (p. ej. M3) se marca como **dato faltante**, con su tasa reportada por brazo. Todo eso se registra como covariable por brazo. **Intentar** una violación nunca detiene nada.
4. **STOP del lote**, solo en estos casos:
   - **S1. Precondición antes de la exposición.** Imagen, binario, fixture, ámbito ya usado, gateway, programa de control o sonda del **primer** lanzamiento del worker. En ese momento el árbol es el clon del fixture y el agente todavía no ha actuado.
   - **S2. Prueba de que una frontera falló**, siempre a partir de evidencia **fuera del alcance de escritura del agente**:
     - registro del gateway: `cache_read` > 0 en la primera petición de un ámbito nuevo, o un token de otra repetición;
     - `ao.db` (inaccesible para el agente) o el programa de control: `memoryMode` entregado ≠ asignado;
     - el filtro de red del host: tráfico **completado** hacia algo que no sea el gateway;
     - una sonda que **alcanzó** una etiqueta en una ubicación que el agente **no** puede escribir (`ao.db`, logs del daemon, entorno de otros procesos, rutas de control);
     - el invariante de prompt de lote (§3.2), calculado sobre los cuerpos de petición del gateway y limitado al texto que compone AO.
   - **S3. Fallo del instrumento del lado del host**: falta el registro del gateway, el programa de control no responde, excepción del supervisor o suspensión de la organización del proveedor. **No** es S3 una anomalía en datos que el agente puede escribir (punto 3).
5. **Lote detenido.** No se analiza para la decisión. Se reporta con su causa, con el recuento de STOP por brazo y con la muestra en que ocurrió.
   - Un lote nuevo exige que la causa se corrija y se publique, y un **preregistro nuevo**.
   - El número máximo de relotes se fija en el primer preregistro (**2**). Si se agota, la decisión es NO-GO del instrumento.
   - Los lotes **nunca se combinan**: solo se analiza el último completo.

   Con estas reglas, "repetir hasta que no haya STOP" no puede actuar como filtro de muestras. S2 y S3 no dependen de nada que el agente escriba, y los relotes están acotados y se reportan.
6. **Residual declarado.** Solo un fallo real de una frontera, o una sanción del proveedor provocada a propósito (§7), puede detener el lote tras la exposición. Lo segundo es denegación de servicio, no sesgo.
7. §4.2 y §9.2 de v1 quedan sin efecto.

---

## 10. Gates empíricos (antes del preregistro, en ámbitos y clones de canario)

Cada gate tiene un resultado preregistrado y una salida si falla. Ninguno usa ámbitos de muestra.

| Gate | Qué se mide | Si falla |
|---|---|---|
| **G0** Viabilidad | AO, Claude Code 2.1.284 y Codex 0.157.1 corren en la VM invitada con el lab adaptado (escrituras denegadas por defecto, parámetros nuevos de `agent.sb`); un workflow completo pasa, y la sonda y las mutaciones del harness se vuelven a pasar dentro de la VM | Probar la alternativa C (Linux) o NO-GO |
| **G1** Destrucción | Tras borrar la VM no queda proceso, disco ni directorio en el host. Un canario escribe marcas en todos los almacenes locales imaginables (archivos, defaults, pasteboard, llavero, `/tmp`) y un clon nuevo no ve ninguna | NO-GO de la frontera |
| **G2** Egress | Desde la VM (usuario `lab` y dentro de `agent.sb`): barrido de puertos del host por IPv4 e IPv6 (incluida link-local), LAN, IP pública:443, DNS, mDNS/multicast y los servicios del puente. Solo el gateway (y DHCP) responde. Sin montajes ni reenvíos | Corregir el filtro; sin eso, NO-GO |
| **G3** Caché de Anthropic por workspace | Prefijo ≥ 1024 tokens con `cache_control` en el workspace X; lectura desde el Y = 0 en varios intentos y en ambos órdenes. **Control positivo**: lectura desde X > 0 | NO-GO del diseño de ámbitos de Anthropic (no hay fallback con enfriamiento) |
| **G4** Caché de OpenAI por proyecto | El mismo protocolo con proyectos P y Q de la organización, incluido tiempo de respuesta, más un control entre organizaciones | Reviewer Claude (§5.1) |
| **G5** Inventario de clientes | Rutas, betas, campos y herramientas reales de Claude Code 2.1.284 y Codex 0.157.1 contra un gateway en modo registro; arranque sin `GET /models`; `web_search="disabled"` | Ajustar el allowlist o la config del cliente, igual en ambos brazos |
| **G6** Límites | Inventario de límites de cada organización y existencia de límites por workspace o proyecto | Presupuestos más bajos o reducir *N* |
| **G7** Gateway | Mutación de cada campo prohibido en cada posición; framing HTTP hostil (P2-3); ninguna respuesta de error revela nada | Corregir antes de preregistrar |
| **G8** Q4, Q6 y extracción | Los tests ocultos solo existen en el host y en la VM de oráculo. El oráculo de C y de las trampas acierta sobre soluciones de referencia y falla sobre soluciones rotas. La extracción resiste symlinks, archivos enormes y sqlite o JSONL malformados | Corregir |
| **G9** Canal de control | Un agente adversarial en un clon (dentro de `agent.sb`) intenta: escribir dotfiles, `~/.ssh/rc` y `authorized_keys`; escalar con `su` o `sudo`; plantar ejecutables que el programa de control pueda invocar. Después, un ciclo completo de control no ejecuta nada del agente. No existe ninguna cuenta admin con contraseña conocida | Corregir; sin eso, NO-GO |

## 11. Qué tiene que crear o configurar el operador (cuando se autorice; **no ahora**)

1. **Disco y RAM del host.** Hoy el volumen de datos tiene **6,4 GB libres (99 %)**.
   - La alternativa B necesita **~60–80 GB libres**: una imagen macOS base de ~50 GB, más los deltas de los clones y la evidencia.
   - Durante el lote, la VM necesita ~8 GB de RAM de los 18 del host. Colima ocupa hoy 8 GiB y está `Running`, así que habría que pararla durante el lote (decisión del operador; no se toca aquí).
   - Si no se puede liberar el disco: alternativa C (~10–15 GB).
2. **Hipervisor**, elegido por el operador. Por ejemplo, `tart` (verificar que su licencia cubre este uso) o Lima con invitado macOS. Lima 2.1.1 ya está instalado, pero hoy solo se usa para Colima (Linux).

   Requisitos, sea cual sea: clonado por copia en escritura, red restringible a un destino, sin carpetas compartidas ni reenvíos, y borrado verificable. macOS permite hasta 2 VMs macOS simultáneas en hardware Apple; aquí siempre corre 1.
3. **Filtro de red** del segmento de la VM, si el hipervisor no lo trae: un anchor de pf cargado solo durante el lote. Requiere `sudo` en el host: es un cambio de sistema que el operador aplica y retira.
4. **Usuario de host `aolabsup`**: no admin y con su propio llavero, donde van las claves del laboratorio (§4.3). Crearlo es un cambio de sistema en el Mac; lo hace el operador.
5. **Anthropic Console:**
   - una organización dedicada con facturación propia y límite de gasto;
   - un workspace por **(muestra × rol que use Anthropic)**, más los del mini-E2E y el ensayo, más ~6 de canario y plantilla (G0, G3 X/Y, G5, G7, plantilla). Cada uno con su clave limitada y **límites de tasa y gasto propios**.

   Con 4 tareas × 2 brazos × 5 = 40 muestras y un mini-E2E de 4:
   - **~52** workspaces si G4 pasa (solo el worker usa Anthropic);
   - **~94** si G4 falla (worker y reviewer, cada uno en el suyo).

   Los ámbitos para un posible relote se crean solo si hace falta. Las claves solo se crean en la UI. **VERIFICAR** (G6) si Console limita el número de workspaces por organización.
6. **OpenAI Platform** (solo si G4 pasa):
   - una organización dedicada (no la de ChatGPT) con límite de gasto;
   - un proyecto por muestra, más mini-E2E y canarios (~50), con una clave de service account y límites propios.

   **VERIFICAR** si la Admin API puede crear proyectos y service accounts. Si puede, el operador lo hace en una sesión de control separada, **antes** del lote; el supervisor nunca tiene la clave de admin.

   Si G4 falla, no hace falta nada de OpenAI.
7. **Almacenar las claves** en el llavero de `aolabsup`, con nombres opacos por índice (`lab3d-a-017`) que no indiquen el brazo. **No** pegarlas en el chat, en el repo ni en `~/.ao`.
8. **Tests ocultos, oráculo de la tarea C y trampas:** se escriben y se guardan **solo en el host**, fuera del fixture y de la imagen, antes del preregistro.
9. **Decisiones previas que hay que preregistrar:**
   - aceptar que Q2 y Q3 no son medidas ciegas y que la calidad se decide con Q1, Q4 y Q6 (§1);
   - el máximo de relotes (§9);
   - aceptar el reviewer Claude si G4 falla;
   - aceptar el coste (worker Opus y reviewer por API, ~40 muestras más gates);
   - la pausa *G* y los presupuestos;
   - quién hace Q5.
10. **Autorizar**, por fases: gates G0–G2 y G9 (VM, red y canal de control, sin cuentas); después las cuentas y G3–G8; después la implementación del supervisor y el gateway. Solo entonces: preregistro, mini-E2E y revisión de Codex.

## 12. P0/P1 abiertos en v2

### 12.1 Comprobación interna antes de entregar (NO es Codex)

Un subagente de contexto limpio, en solo lectura, atacó la primera redacción de v2 sobre este documento, la revisión de Codex, v1, el plan de benchmark y el harness. **Veredicto: NEEDS_CHANGES, con 6 P1 de diseño.** Todos quedan incorporados en el texto de arriba:

| # | Hallazgo | Dónde se corrigió |
|---|---|---|
| A | El agente podía secuestrar el ssh del supervisor: dotfiles de `lab`, porque `agent.sb` es allow-default. Verificado: `agent.sb` permite escribir fuera de las rutas negadas | Usuario `ctl` con comando forzado, sin shell ni dotfiles; `agent.sb` pasa a denegar escrituras por defecto; sin admin conocido; gate G9 (§4.3, §10) |
| B | Q4 podía ejecutar código del agente en el host, y los tests ocultos podían acabar indexados en la memoria | VM de oráculo; tests solo en el host (§1, §6, paso 8) |
| C | El agente podía provocar STOP: el regex `\bassisted\b` de la sonda recorre archivos que el agente escribe (verificado en `probe.py`), y la extracción podía fallar a propósito. Además, relotar sin límite actuaría como filtro de muestras | S2 y S3 solo con evidencia que el agente no puede escribir; anomalías del agente = dato faltante; relotes acotados y nunca combinados (§9) |
| D | La comprobación de caché fría daba falsos STOP con el reviewer Claude en el mismo workspace, y la exposición no estaba definida | Workspace por rol; exposición = primera petición al gateway (§5.1, §6) |
| E | El invariante de prompt no estaba bien definido y se contradecía con la regla de transcripts | Comprobación de lote sobre los cuerpos que registra el gateway en el host, con plantilla OFF preregistrada y limitada al texto de AO (§3.2) |
| F | La tarea C se quedaba sin puerta de calidad | Q6, oráculo mecánico del defecto sembrado, más tests de trampa (§1) |

También se corrigieron sus P2 y P3:

- usuario de host `aolabsup` para el gateway y los parsers;
- IPv6, multicast y DHCP en el filtro;
- orden de construcción de la imagen;
- "reutilizar tal cual" → volver a verificar;
- presupuesto igual en ambos brazos, con sensibilidad;
- sanción del proveedor como denegación de servicio;
- recuento de workspaces;
- cadenas del binario de AO;
- atribución de tokens por sesión.

La comprobación consideró **sólido** el rechazo del placebo.

### 12.2 Estado

**No conozco ningún P0/P1 de diseño abierto.** Pero la primera redacción tenía 6 P1 que no vi hasta que la atacó un tercero, y sus correcciones **no han pasado un segundo ataque**. Además:

- **Cierres que dependen de una medida que aún no existe:**
  - la frontera y el canal de control: G1, G2 y G9;
  - la caché de Anthropic, G3. Si falla, el diseño de ámbitos de Anthropic no tiene fallback y es NO-GO;
  - la de OpenAI, G4, que tiene salida preregistrada.
- **Precondiciones que bloquean:** la viabilidad (G0) y el disco.

Riesgos residuales declarados:

- escape del hipervisor;
- ruido exógeno del proveedor, como covariable;
- sanción del proveedor provocada a propósito (denegación de servicio);
- Q2 y Q3 no son ciegos (declarado; no deciden la calidad);
- la atribución por rol se puede manipular dentro de la repetición (detectable, como covariable).

**Revisión:** este diseño **no** tiene revisión de Codex.

## 13. Recomendación mínima viable

1. **Experimento:** estimando de intención de tratar, contraste = el bloque del pack, sin placebo; puertas de calidad Q1 + Q4 + Q6; Q2 y Q3 declarados no ciegos.
2. **Frontera:** una VM macOS por repetición, clonada de una imagen sellada y destruida al terminar, con el lab actual dentro, adaptado: apunta al gateway, niega la egress IP, deniega escrituras por defecto fuera del árbol de la repetición y se controla por un canal de control separado (§4.3). Todo se vuelve a verificar.
3. **Proveedor:** una organización de Anthropic dedicada, **un workspace por repetición y rol** y el gateway en el host, bajo `aolabsup`, con un token por repetición y rol.
4. **Reviewer:** Codex solo si G4 pasa; si no, Claude.
5. **Cuotas:** repeticiones secuenciales, pausa *G* de minutos y presupuestos por repetición por debajo de los límites de la organización.
6. **Norma de muestra:** §9. Todo cuenta; STOP del lote solo por evidencia que el agente no puede escribir; ningún reemplazo; relotes acotados.
7. **Orden de trabajo:** revisión de Codex de v2 → G0–G2 y G9 → cuentas → G3–G8 → implementación → preregistro → mini-E2E → revisión de Codex → 3D.

**Qué se elimina frente a v1:**

- el usuario de macOS dedicado compartido;
- la política Mach/XPC;
- el reaping por UID;
- el llavero entre usuarios;
- el enfriamiento de 65 min;
- el ledger de prefijos;
- el borrado de campos de caché y cabeceras;
- los listados de "cuenta vacía";
- las negaciones de rutas personales como control primario.

**Qué se añade:**

- la VM desechable, con el canal de control `ctl` y la VM de oráculo;
- el filtro de red fuera de la VM;
- el usuario de host `aolabsup`;
- la regla ventana/presupuesto;
- el gate G4 con salida;
- el registro del gateway como fuente primaria de las métricas.

## 14. Veredicto

**PRECONDITION_3D_AUTH_DESIGN_V2 = NEEDS_CHANGES.**

- **A favor.** El P0 queda resuelto metodológicamente y los 5 P1 de Codex, por diseño. Los 6 P1 de la comprobación interna están incorporados. La arquitectura es más simple que la de v1: una frontera que se destruye sustituye a una lista de reglas locales. No conozco ningún P0/P1 abierto.
- **Por qué no GO_FOR_IMPLEMENTATION.** La primera redacción de este mismo documento tenía 6 P1 de diseño, visibles sin ejecutar nada. Sus correcciones (canal de control, reglas de STOP, invariante de prompt, Q6) son texto nuevo que nadie ha atacado todavía. Declarar GO repetiría el patrón de los ciclos 2–6.
- **Qué lo convierte en GO_FOR_IMPLEMENTATION:** una revisión de Codex de este v2 sin P0/P1. No hace falta nada más de diseño que yo sepa. Los gates G0–G9 son de implementación y cada uno tiene su salida.
- **Bloqueo operativo:** 6,4 GB libres en el disco; la alternativa B necesita ~60–80 GB.

**PRECONDITION_3D = NO-GO.** 3D no se ejecuta.
