# Prompt de revisión adversarial de Codex: PRECONDITION_3D_AUTH_DESIGN_V4

> HISTÓRICO / NON-NORMATIVE. No ejecutar para 3D-PRACTICAL. Usar
> [3d-practical-codex-review-prompt.md](3d-practical-codex-review-prompt.md).

Uso futuro, sólo con autorización explícita. Esta entrega prepara el prompt y
**no lo ejecuta**:

```sh
cd <clon en feat/frente3-3d-prerequisites>
mkdir -p ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v4/codex
codex exec --sandbox read-only --ask-for-approval never \
  "$(sed -n '/^---8<---$/,$p' docs/frente3/3d-auth-design-v4-codex-review-prompt.md | tail -n +2)" \
  > ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v4/codex/review.md
```

---8<---
You are the adversarial security and experimental-methodology reviewer for AO Frente 3 / 3D V4.
Your task is to refute the design, not reward added detail.

Read completely:
- docs/frente3/3d-auth-design-v4.md (only normative isolation/auth/causality spec);
- docs/frente3/06-benchmark-plan.md sections 3–5 (only normative decision function);
- docs/frente3/3d-auth-design-v3.md (historical, non-normative);
- ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v3/codex/review.md;
- backend/internal/projectmemory/provision.go and pack.go.

HARD RULES: static read-only analysis only. Do not modify files; run AO, Codex, agents, harnesses,
VMs, gateways, gates, preregistration, mini-E2E or 3D; create accounts/scopes/credentials; read
secrets; touch production; or call providers. Mark every empirical claim UNKNOWN and name its gate.
No unrun gate is evidence.

First map each V3-review finding F1..F12 and each reopened P1-1..P1-6, P2-1..P2-4, P3-1 to exactly
one of CLOSED_BY_DESIGN, REQUIRES_GATE or OPEN. Then attack:

1. Causal classification. Construct an allowed agent request that crashes every component in turn.
   Verify it always becomes sample failure, even when logs/metrics are lost. Try to smuggle ambiguous
   gateway/supervisor/parser faults, prior sanctions, resource exhaustion and unknown causes into
   INSTRUMENT_NO_GO or a pause. Test every independent allowlist item for a sample-controlled path.
2. Optional stopping across preregistrations. Try rewriting, omitting, forking or publishing only a
   successful successor. Test MAX_SUCCESSORS, seed derivation, changed caps/models/thresholds and
   whether readiness consumes the entire append-only lineage.
3. G6. For every A/B/C/D cell and all eleven layers, verify a positive control exercises the exact
   intended shared layer—not merely a workspace cap. Attack hidden parent/admin/billing/contract,
   egress, device, routing and TLS identities. Check binary, counter and latency bounds and the
   ethical INCONCLUSIVE route for abuse/risk/sanction.
4. Response state machine. Walk all success/retryable/rate/policy/terminal transitions, retries,
   deterministic backoff, partial streams, compaction, terminal states and token accounting. Find
   any behavior not compared with direct-provider positive controls or any state leak needed for
   compatibility.
5. Role separation. Attempt bearer extraction, FD passing, peer spoofing, stale epoch frames,
   connection carry-over, client-supplied session spoofing, confused-deputy control/helper paths and
   provider-level worker→reviewer coupling. Require G6+G7+G9.
6. Q6. Attack exact target/version/line definitions with whole-file, 20-line grazing, K=3 tiling,
   enum guessing, near miss, duplicate symbol, misleading README, duplicate finding, line drift and
   legitimate alternative defects. Verify explanation is absent from the quality construct, all
   mandatory defects fit K and post-run adjudication cannot see arm/results or rewrite the oracle.
7. Total decision function. Enumerate every state and every combination of COMPLETED/null/malformed/
   out-of-domain, timeout, provider class, sanction, retry, tasks A-D, Q1/Q4/Q6, M1u/M2/M3, OFF=0,
   ties, N=5, instrument failure and unrun position. Verify TOKEN_CAP/CALL_CAP algebra and that no
   analyst choice remains.
8. Treatment provenance. Attack adapter wrapping, escaping, truncation, duplication, relocation,
   delimiter collision, quoted JSON, tool output and external context. Verify model-decoded treatment
   equals Provisioned.Render byte-for-byte and AO_EXTERNAL_CONTEXT absence is a separate assertion.
9. G4. Check cluster independence, fresh randomized eligible prefix, new scope pairs, temporal
   blocking, routing epochs, same-project positive control, cross-project and cross-org bounds, and
   exact PASS/FAIL/INCONCLUSIVE fallback.
10. Gate contamination, host state and extraction. Track failed gate attempts and every org/parent/
    billing/egress identity. Attack conntrack, pools, TLS, resolver, SSH, hypervisor/cache, source
    ports and egress identity. Attack Unicode/case fold, xattrs/forks, ACLs, flags, long lines,
    descriptor pinning, TOCTOU and hostile SSH output. Verify the receiver/extractor has no plan,
    secrets, gateway or network and every isolation promise has a gate assertion.
11. Search all normative text for replacement, relot, retry, N reduction, null, pass-through,
    provider adapter, INSTRUMENT_NO_GO, pause, reviewer credential, S_k and nothing persists. Treat
    any unmarked historical document claiming authority as a contradiction.

For each finding provide P0/P1/P2/P3 severity, concrete attack, exact file:line evidence, validity or
security impact, whether a gate can settle it, and the smallest correction. Do not pass a design
because gates are expensive; do not fail a design merely because a correctly specified gate is unrun.

End with exactly one line:
PRECONDITION_3D_AUTH_DESIGN_V4 = READY_FOR_IMPLEMENTATION | NEEDS_CHANGES | NO-GO
