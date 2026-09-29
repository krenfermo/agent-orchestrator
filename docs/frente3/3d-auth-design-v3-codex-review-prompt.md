# Prompt de revisión adversarial de Codex: PRECONDITION_3D_AUTH_DESIGN_V3

Uso futuro, sólo después de autorización explícita del operador. Esta entrega
prepara el prompt pero **no lo ejecuta**:

```sh
cd <clon en feat/frente3-3d-prerequisites>
mkdir -p ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v3/codex
codex exec --sandbox read-only --ask-for-approval never \
  "$(sed -n '/^---8<---$/,$p' docs/frente3/3d-auth-design-v3-codex-review-prompt.md | tail -n +2)" \
  > ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v3/codex/review.md
```

---8<---
You are the adversarial security and experimental-methodology reviewer for the third design of
AO Frente 3 / 3D. Your task is to REFUTE it, not to improve its rhetoric.

Read fully:
- docs/frente3/3d-auth-design-v3.md (the only normative auth/isolation/sample specification);
- docs/frente3/06-benchmark-plan.md, especially sections 3–5 (the only normative decision rule);
- docs/frente3/3d-auth-design-v2.md (historical, non-normative);
- the real V2 review at
  ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v2/codex/review.md;
- backend/internal/projectmemory/provision.go and pack.go for the real treatment renderer.

HARD RULES: static read-only analysis only. Do not modify files; run AO, clients, harnesses, VMs,
gates, preregistration, mini-E2E or 3D; create accounts/projects/workspaces; read or request
credentials; touch production data; or call provider APIs. If a fact needs measurement, mark it
UNKNOWN and identify the exact V3 gate that must settle it. Do not treat an unrun gate as passed.

First map every V2-review finding P1-1..P1-6, P2-1..P2-4 and P3-1 to exactly one of:
CLOSED_BY_DESIGN, REQUIRES_GATE, or OPEN. Challenge V3's own mapping rather than copying it.

Then attack at least these paths:

1. Sample selection. Find any post-start event reachable by an agent, provider, gateway or host
   that can exclude, replace, retry or retrospectively select a sample. Test sanctions, bounded-log
   exhaustion, pre/post-start ambiguity, pauses, active instrument failure, unrun assignments, and
   whether INSTRUMENT_NO_GO can be used as a clean efficacy batch reset.
2. Provider state. Enumerate every header/body/timing/error/cache/quota/risk/billing/sanction
   signal that crosses scopes. Check whether response reconstruction breaks clients or still leaks
   organization state. Decide whether G6 really distinguishes workspace/project partition from
   organization-only partition and whether the independent-organization fallback has a hidden
   common identity or policy scope.
3. Role separation. Try to let worker obtain, consume, replay, prewarm or deny reviewer scope via
   files, argv/env, inherited descriptors, connections surviving epoch transition, session-ID
   spoofing, listener confusion, helper/control services, or provider identities. Check that
   attribution is enforcement, not a covariable.
4. Q6. Exercise whole-file, maximum-span tiling, K=3 shotgun, near misses, duplicate symbols,
   misleading README, duplicate findings, alternative real defects, enum guessing, explanation
   gaming and line drift. Determine whether FP=0 and rank 1 make it a valid quality oracle rather
   than merely deterministic.
5. Decision rule. Implement mentally the function for every task and for ties, OFF median zero,
   missing fields, timeouts, provider failures, client retries, task C, task D, M1u/M2/M3, N=5,
   instrument failure and unrun positions. Find any remaining choice between V3 and 06 or any
   analyst discretion after seeing results.
6. Treatment provenance. Compare the specification byte-for-byte with Provisioned.Render(),
   FreshnessNotice(), Pack.Render() and External.Render(). Try delimiter collisions, quoted JSON,
   transcript/tool-output injection, dynamic freshness/dirty/graph state and provider adapter
   wrapping. Verify only the first worker prompt can stop the experiment and that origin spans are
   created before serialization without exposing arm metadata.
7. VM/host. Attack static networking/DHCP fallback, ARP/NDP/NAT/firewall persistence, SSH return
   traffic, the declarative image manifest, client machine IDs, the forced-command boundary,
   hostile extraction (FIFO/device/symlink/hardlink/sparse/races/archive/Git filters), helper-to-
   secrets isolation and the persistent-state inventory.
8. Internal consistency. Search both normative documents for relot/replacement/retry, reduction of
   N, conflicting quality thresholds, singular S_k, pass-through response claims, reviewer secrets
   present during worker, "nothing persists" claims, and gates whose failure silently changes the
   estimand or sample.

For each finding output severity P0/P1/P2/P3, a concrete attack path, precise file:line evidence,
impact on validity/security, whether a gate could settle it, and the smallest design correction.
Do not grant GO because implementation will be hard or because a gate might pass.

End with exactly one verdict line:
PRECONDITION_3D_AUTH_DESIGN_V3 = READY_FOR_IMPLEMENTATION | NEEDS_CHANGES | NO-GO
