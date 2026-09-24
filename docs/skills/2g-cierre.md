# Frente 2 / Fase 2G — Hardening y cierre de Skills (informe)

Rama `feat/2g-skills-hardening` desde ECC `c4cf7ba2` (sin merge, sin push).
Producción intacta en **goose 174** (sin migración en 2G, como manda la directiva).
AO detenido durante todo el trabajo. Stacks de contenedores ajenos intactos.

## Veredicto de la auditoría de cierre (ETAPA 1)

Tres agentes read-only auditaron paridad/propiedad/recuperación, honestidad de
reportes/provenance/secretos, y migraciones/versionado. **P0 = ninguno.**
**Un P1** (resuelto). El resto, deuda P2/P3 (tests y defensa en profundidad).

## Matriz requisito → implementación → prueba → evidencia → deuda/riesgo

| Requisito (ETAPA) | Implementación | Prueba | Evidencia | Deuda / Riesgo |
| --- | --- | --- | --- | --- |
| **Builder canónico / provenance** (2) | build reproducible (`scripts/build-*.sh`), digests re-registrados con builder canónico | `--check` (build ×2 idéntico); build fail-closed sin tags | `bcc5ead2f`; provenance.json proxy 7da01d2b/030612ce, checker d4b7009a/d60353a3, go1.26.6 | — |
| **E2E release-like con embeds reales** (3) | `Select()` re-hashea y verifica contra provenance; corre con `Embedded()` no StoreForTest | `pentestrun_embed_live_test.go` (tagged) | `bd56767e5`; Colima arm64: digest observado == registrado; PASS 6.2s; sin residuos | requiere Docker (skip si no hay runtime) |
| **Redes privadas fail-closed en prod** (4) | prod usa `PolicyFor(lease)`; `Lease` sin campo CIDR → nunca abre rangos privados | `TestPolicyFor_ProductionPentestPolicyIsFailClosedOnPrivateRanges` + negativas metadata existentes | `3a2e8a628` | — |
| **Honestidad de reportes** (5) | `notAttempted`/`Limitations`/`Completeness`; hallazgos = regla+ubicación | negativas y schema existentes (2A–2F) | auditoría agente 2: sin over-claim | test invariante "static sin free text" pendiente (P2) |
| **Matriz de autorización** (6) | gate server-side idéntico en API/CLI/UI; `AuthorizesRun` liga exacto | 11 negativas 2F + old-pin (8) | `pentestrun_test.go`; auditoría agente 1 | — |
| **Reinicio/crash/recuperación** (7) | reconcile marca interrumpidos `failed`, nunca reanuda; ReapRun/SweepOwned reclaman contenedores **y redes** por propiedad, confirmado | 2 tests fakeDocker network reap/sweep; live E2E `assertNoLeftovers` | `d1d84ac37` | — |
| **Versiones / activaciones** (8) | modo se resuelve del manifiesto **fijado**; install ≠ enable; boot no bumpea | `TestPentest_CannotRunOnAPinThatPredatesTheMode` | `42a0558ed`; auditoría agente 3 | — |
| **Migraciones 0161–0174** (9) | rebuilds con FK guardians (pragma-off + NO TRANSACTION o park-restore); 0174 additive-only | guardians `migrate_rebuild_fk_safety_test.go`; ledger bidireccional | auditoría agente 3: limpio | tests up/down 0161/0162/0166/0169 preexistentes faltan (P2) |
| **Paridad API/CLI/UI** (10) | mismas rutas server-side | auditoría agente 1 | doc de operador | CLI sin `cancel`; UI sin revoke/list de autorizaciones (P2, documentado) |
| **i18n 8 locales** (11) | 13 claves pentest | key-set idéntico en 8 locales; valores traducidos | verificado (es/ja/de) | — |
| **Fuga de secretos** (14) | pipeline validate→redact→validate; `error_message` redactado en chokepoint | `TestPentest_CheckerStderrSecretIsRedactedInTheStoredErrorMessage` | `b31440772`; auditoría agente 2 | error `%v` de transport (P3, mitigado aguas abajo) |
| **Residuos** (15) | cleanup confirmado por inspect | E2E sin leftovers; check final | 0 contenedores/redes AO; fixtures limpias | — |
| **Documentación** (16) | doc de operador + roadmap al día | — | `d86914135` | — |

## Gates pesados (ETAPA 13)

- `go build ./...` ✓ · `go vet ./...` ✓
- `go test -short ./...` (repo completo) ✓ (exit 0)
- lint delta `--new-from-rev=c4cf7ba2` = **0 issues** ✓
- `-race -short` en paquetes afectados (skillrunner, service/skills, skillegress) = **0 data races** ✓
- sqlc drift: N/A (2G no toca queries) · API drift: N/A (2G no toca openapi)
- frontend: 2G no cambia código de frontend; i18n verificado
- build por defecto (sin tags) fail-closed ✓ · build con tags compila + E2E pasa ✓

## Pendiente consciente

- **ETAPA 12 — E2E de sistema 2A→2F con Claude REAL en `authz-review`:** los modos
  de agente se prueban con un `fakeAgent` determinista en ECC; no hay test que
  invoque Claude real. Re-ejecutarlo implica gasto real de API + runner de agente
  configurado. Los modos **tool** (incl. active-pentest con embeds reales) están
  probados en vivo en 2G; el camino de agente no lo cambia 2G salvo el chokepoint
  de redacción (ya probado). Decisión de si ejecutarlo antes del cierre formal:
  del usuario.
