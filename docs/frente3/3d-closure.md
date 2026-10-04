# Cierre de 3D-PRACTICAL — CLOSED / NO-GO (2026-10-01)

## Qué significa este NO-GO

El experimento congelado no produjo evidencia válida suficiente para activar
Project Memory. No es un juicio sobre AO ni sobre Project Memory como
producto: dice solo que este protocolo, ejecutado sobre las superficies
reales actuales de AO, no puede dar la evidencia que su decisión exige.

**Project Memory sigue OFF por defecto.** 3E no está autorizado.

## Objetivo

Responder con una comparación controlada OFF/ASSISTED (40 posiciones, tareas
A–D) esta pregunta: ¿Project Memory ASSISTED reduce M1u, M2 y/o M3 sin degradar
Q1/Q4/Q6, bajo condiciones controladas y representativas de AO? La norma
congelada está en [3d-practical.md](3d-practical.md) y
[06-benchmark-plan.md](06-benchmark-plan.md).

## Metodología Practical (lo que se construyó)

Todo se ejecutó sobre AO real. Las piezas fueron:

- Un proxy de proveedor por posición: Claude (Messages) y Codex (Responses).
  Cada intento queda en el ledger con su dispatch y su finalización.
- Inyección de credenciales desde el supervisor: los agentes solo tienen
  placeholders.
- Un sandbox exterior (Seatbelt) para todos los agentes.
- Un gateway de allowlist hacia el daemon.
- Un oráculo Q1/Q4 en sandbox, sobre una copia preparada y verificada.
- M3 derivado de la telemetría 3C, ligado a los `tool_use`/`call_id` que
  devolvió el proveedor.
- El runner oficial con el mismo executor real que el mini-E2E.

Detalle y evidencia: [3d-practical-ao-integration.md](3d-practical-ao-integration.md).

## Resultado

| Tarea | Resultado | Causa |
|---|---|---|
| A (real) | **FAIL** | La regla del primer edit de M3 exige una ruta de proyecto estructurada. El worker real edita legítimamente con Bash (`python3 - <<…`), que 3C registra como `command_edit` sin ruta, así que M3 queda MALFORMED. Todo lo demás pasó en el mini-E2E real (ejecución 19): worker y reviewer de Codex completos, todo por el proxy, sin bypass y sin exponer credenciales. |
| C | **PRACTICALLY_UNIMPLEMENTABLE_UNDER_FROZEN_SPEC** | Q6 puntúa findings sobre un diff dado (HEAD de la fixture). El reviewer de AO revisa el `git diff` del worker contra ese mismo HEAD, y emite markdown libre sin findings estructurados. Su pack depende de los archivos que cambió el worker, y el modo código de Codex es opaco para 3C. |

**Las 40 posiciones no se ejecutaron.** No se corrigió A ni se rediseñó C, por
decisión del operador.

## Limitaciones conocidas

- La regla del primer edit de M3 es incompatible con ediciones vía Bash sin
  ruta estructurada en 3C.
- C necesitaría otra arquitectura de reviewer (una revisión sobre un objetivo
  dado con salida estructurada), y eso queda fuera del alcance.
- Por defecto (`code_mode_host`), las llamadas de Codex son JavaScript opaco
  para 3C.
- Si el código de la fixture es hostil, puede falsear su propio veredicto
  dentro del binario de test. Sin VMs, este residuo se aceptó.

## Qué queda como trabajo válido (integrado en ECC)

- **Hardening del flujo de review:**
  - generation fence del lanzamiento del reviewer;
  - claim en vuelo reservado antes del CAS;
  - el dispatch vivo no se declara ausente.
- **Fuga de entorno:** los interruptores solo-daemon (`AO_MEMORY_MODE`,
  `AO_MEMORY_EXTERNAL`, `AO_CONTEXT_ROUTER`) se leen una vez y se retiran del
  entorno. tmux ya no los pasa a los paneles de agentes.
- **Observabilidad:** `contextSources.externalContext` en el
  `policy_snapshot` de cada run (API/CLI/OpenAPI).
- **Project Memory:**
  - `AO_MEMORY_EXTERNAL`: activado por defecto, sin cambio de comportamiento;
  - `AO_MEMORY_ROLES`: sin definir equivale a todos los roles;
  - el orden del pack es determinista e independiente de la ubicación del
    checkout.

## Qué no se integra

- El código del experimento: `backend/internal/observe/practical3d`,
  `backend/cmd/ao3dpractical`, el override `AO_REVIEWER_PERMISSIONS`, las
  exportaciones de `usage` que solo usaba el experimento y `golang.org/x/text`.
- Su último estado se conserva en el historial de la rama
  (`feat/frente3-3d-prerequisites` @ `9554ba761`).
- La migración 0175 es de 3C, ya está en ECC y esta rama no la modifica.
