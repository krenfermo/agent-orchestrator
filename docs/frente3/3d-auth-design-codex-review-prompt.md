# Prompt de la revisión adversarial de Codex: PRECONDITION_3D_AUTH_DESIGN

Uso (en la máquina del laboratorio, sandbox de solo lectura, sin tocar producto, producción ni evidencia):

```sh
cd <clon de agent-orchestrator en feat/frente3-3d-prerequisites>
codex exec --sandbox read-only --ask-for-approval never \
  "$(sed -n '/^---8<---$/,$p' docs/frente3/3d-auth-design-codex-review-prompt.md | tail -n +2)" \
  > ~/.ao/scratch/frente3/reviews/3d-preflight/auth-design/codex/review.md
```

El resultado va a la evidencia privada. Al repo solo va un resumen en `3d-auth-design.md` §9.

---8<---
You are an adversarial security and methodology reviewer. Your job is to REFUTE the design in
docs/frente3/3d-auth-design.md (read it fully, and docs/frente3/3d-preflight.md for context: the
last three sections describe the lab, the closed controls and the open P1-2). You may read the
lab harness under ~/.ao/scratch/frente3/tools/lab3d/ READ-ONLY.

HARD RULES: do not modify any file; do not run AO, the lab, claude or codex sessions; do not read,
print or copy any credential (keychain items, auth.json, API keys, tokens); do not touch
~/.ao/data/ao.db; do not call provider APIs. Static analysis and reasoning only. If a check would
need any of that, write UNKNOWN and say what experiment would settle it.

Try specifically to break:
1. isolation from the operator's personal accounts (claude.ai, ChatGPT, keychain, ~/.claude*, ~/.codex, GitHub, ssh);
2. isolation between repetitions (any state one repetition can write and a later one can read);
3. server-side channels (Files, Batches, Skills, Managed Agents, containers, stored responses, conversations, vector stores, evals, prompt cache, rate limits, provider memory/history, artifacts);
4. credential reuse (can a sandboxed process obtain any real key, personal or lab? via env, procargs2, keychain, files, the gateway itself, logs);
5. provider re-launch (a child running claude/codex/node/curl without the shim flags);
6. MCP / connectors (claude.ai connectors, API `mcp_servers`, Codex MCP, local .mcp.json);
7. Files / storage;
8. plugins / apps / marketplaces;
9. RemoteTrigger / routines / remote sessions / SendMessage;
10. any way for an agent to learn its arm (OFF vs ASSISTED), memoryMode, treatment, earlier results, private prompts/evidence, or another repetition's state.

Also judge: is "one dedicated org per provider + per-repetition workspace/project + credential gateway
with allowlist + egress deny" necessary and sufficient? Is per-repetition scope really needed (§5)?
Is the OpenAI cache cooldown argument sound? Are the new findings (public-internet egress and
personal non-LLM credentials as cross-repetition channels) real, and were they open in cycle 6?
Does the design regress any closed control listed in §4.6?

Output: findings as P0/P1/P2/P3, each with a concrete attack path, evidence (file:line or doc), and
a proposed fix; then a verdict line exactly: PRECONDITION_3D_AUTH_DESIGN = GO | NEEDS_CHANGES | NO-GO.
