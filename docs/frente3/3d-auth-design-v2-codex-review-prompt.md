# Prompt de la revisión adversarial de Codex: PRECONDITION_3D_AUTH_DESIGN_V2

Uso (en la máquina del laboratorio, sandbox de solo lectura, sin tocar producto, producción ni evidencia):

```sh
cd <clon de agent-orchestrator en feat/frente3-3d-prerequisites>
mkdir -p ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v2/codex
codex exec --sandbox read-only --ask-for-approval never \
  "$(sed -n '/^---8<---$/,$p' docs/frente3/3d-auth-design-v2-codex-review-prompt.md | tail -n +2)" \
  > ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design-v2/codex/review.md
```

El resultado va a la evidencia privada. Al repo va una copia literal, como con la revisión de v1.

---8<---
You are an adversarial security and methodology reviewer. Your job is to REFUTE the design in
docs/frente3/3d-auth-design-v2.md (read it fully). It replaces docs/frente3/3d-auth-design.md (v1),
which you reviewed as NO-GO; your v1 review is docs/frente3/3d-auth-design-codex-review.md. Context:
docs/frente3/06-benchmark-plan.md (metrics, tasks, decision rule) and the last sections of
docs/frente3/3d-preflight.md. You may read the lab harness under ~/.ao/scratch/frente3/tools/lab3d/
READ-ONLY, and backend/internal/projectmemory/pack.go (how the pack is rendered).

HARD RULES: do not modify any file; do not run AO, the lab, claude or codex sessions, or any VM;
do not read, print or copy any credential; do not touch ~/.ao/data/ao.db; do not call provider
APIs. Static analysis and reasoning only. If a check would need any of that, write UNKNOWN and
name the experiment (V2 section 10 gates) that would settle it.

Judge first whether each of your v1 findings (P0-1, P1-1..P1-5, P2-1..P2-5, P3-1) is actually
closed by V2, or survives in a new form. Then try specifically to break:
1. the redefined experiment and blinding (V2 sections 1-3): is hiding the assignment but not the
   content a valid design? is rejecting a placebo sound? are Q1/Q4/Q6 really blind? is the
   batch-level prompt invariant well-defined and not agent-triggerable?
2. the disposable-VM boundary (section 4): any state that survives destruction; the host->VM
   control channel (ctl user, ForceCommand, control program); arm injection into the guest
   daemon; the host network filter (IPv4/IPv6/multicast/DHCP); image build and sealing order;
   the host user aolabsup and parsing of untrusted extracted evidence;
3. provider channels (section 5): per-repetition-and-role Anthropic workspaces, the cold-cache
   check, the G4 decision for OpenAI, the gateway allowlist and pass-through responses, the
   window/budget rule for shared limits, policy-sanction DoS;
4. the sample rules (section 9): can an agent, by anything it writes or does, cause
   outcome-dependent exclusion, or can re-batching act as a sample filter?
5. internal contradictions anywhere in V2 (P3-1 was exactly this for v1).

Output: findings as P0/P1/P2/P3, each with a concrete attack path, evidence (section/line or
file:line), and a proposed fix; then a verdict line exactly:
PRECONDITION_3D_AUTH_DESIGN_V2 = GO_FOR_IMPLEMENTATION | NEEDS_CHANGES | NO-GO.
