# AR-1a — Authority prerequisites: closure record

Branch: `feat/ar1-authority-prereqs`, based on `feat/engineering-control-center` @ `3b990b53b`.
Design: AR-0 (`docs/autonomous-roadmap/ar0-audit-and-design.md` on `docs/autonomous-roadmap-ar0`), §4.3 and §16.

## Scope delivered

| Item | Change |
|---|---|
| D-SEC-1 | A presented but invalid `X-AO-Agent-Token` is answered `401 AGENT_CREDENTIAL_INVALID` on every installation; a request presenting no agent credential keeps trusted-local synthesis. |
| D-SEC-2 | A review verdict is bound to the reviewer credential minted for that review run and worker session. A non-agent verdict is refused while the run is running and a reviewer identity speaks for it, decided in one guarded `UPDATE` with `review_run.reviewer_identity_expected` (migration 0177) written in the run's own insert; the reviewer credential follows that marker. |
| D-SEC-3 | Amendment and fresh-review-exception approvers are derived from the authenticated principal (agents refused, body `approvedBy` must agree); `approved_by_user_id` / `approved_auth_method` recorded (migration 0176); amendment write is a CAS; one exception grant per generation. |
| D-SEC-4 | Codex defaults to the `workspace-write` sandbox with approvals off and network on (TUI and Chat); full bypass only via explicit `bypass-permissions`, audited on every launch/escalation path. |
| Cancel trace | `worker_runtime_reclaim_on_cancel` replaces the misleading `worker_left_running_on_cancel`; behaviour unchanged. |

## Known residual — AR1A-01 (P1, accepted, tracked)

**Status: ACCEPTED by the user on 2026-10-02 as an explicit residual until AR-5. Architectural deadline: AR-5.**

On a trusted-local (default desktop) installation, a request that presents **no** agent credential resolves to the bootstrap owner. An AO-launched agent can therefore omit its own `X-AO-Agent-Token` (for example `AO_AGENT_CREDENTIAL_FILE= ao …`, or a raw loopback request) and act with owner authority, including approving a criterion amendment or a fresh-review exception, which accept `trusted_local` approvers.

- **Pre-existing:** the base `3b990b53b` already synthesized the owner for header-less requests on every owner-level route. AR-1a does not make it worse (confirmed by the independent Codex review) and narrows the adjacent case (a *presented invalid* token is now 401).
- **Why not fixed in AR-1a:** frozen decision D5 requires preserving legitimate trusted-local behaviour for requests that present no agent credential, and the correct closure is the durable human-presence mechanism of D1 (assurance levels A1–A3, challenge, Electron IPC, user gesture, Touch ID), scheduled for **AR-5**. Fixing it now would break trusted-local or pre-empt AR-5; both are out of scope by decision.
- **Mitigations already in place:** trusted-local approvals are recorded as `approved_auth_method = 'trusted_local'` and are never upgraded, so every such approval is distinguishable in the audit trail; agent principals are refused outright when they do present their credential.
- **Closure criterion:** **AR-5 cannot be declared closed while AR1A-01 is still exploitable.** AR-5's acceptance tests must include an agent runtime that deliberately omits its credential and attempts an A2/A3 action (freeze, amendment, exception, HDR resolution), and that attempt must be refused.

## Compatibility notes

- Late reviewer verdicts after AO closes a run arrive header-less (credential and file swept together) and are still preserved.
- Unowned legacy runs keep their previous review behaviour (no identity marker, header-less reviewer on trusted-local).
- Inside the sandboxed default, a Codex session's explicit extra writable roots are its workspace project's child roots and the common git directories of its repositories (`internal/codexsandbox`, `--add-dir` / `writable_roots`), so `git add` / `git commit` work in AO worktrees and child repositories (AR1A-INT-01, verified against the installed Codex). Anything else outside the worktree -- `~/.ao`, `$HOME`, the main repository's checkout -- stays denied; a Codex session that genuinely needs it requires the explicit `bypass-permissions` policy.
- `approvedBy` is no longer required in the amendment / exception request bodies; when present it must name the authenticated principal.
