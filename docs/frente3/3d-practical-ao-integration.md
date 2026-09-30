# 3D-PRACTICAL — real AO integration

Status note for the integration of the 3D-PRACTICAL harness with a real AO
daemon (`backend/internal/observe/practical3d`, `backend/cmd/ao3dpractical`).
It records how the three P0s are closed, and which items remain open.
It does not change the norm in `3d-practical.md` / `06-benchmark-plan.md`.

## Architecture finding

AO never calls a provider itself. Every role is an external CLI that AO
launches in a tmux pane:

- worker and repair (the fix turn runs in the worker's session): Claude Code;
- reviewer: Codex. AO requires a cross-provider reviewer for high-risk tasks.

AO learns about usage only after the fact, from the transcripts, through 3C
and the usage ingestor. Observation therefore happens at the network boundary
of those CLIs, not inside AO.

## P0-1 — provider observation

- **The boundary.** Each position runs a `ProviderProxy`, which the
  supervisor owns. The launch shim (`ao3dpractical`, installed as
  `claude`/`codex` on the daemon's PATH) points the CLIs at it:
  - Claude uses `ANTHROPIC_BASE_URL=http://127.0.0.1:P/t/<token>`.
  - Codex uses a custom `model_provider` whose `base_url` is
    `{BASE}/backend-api/codex`, with `wire_api=responses`.

  Tokens are issued over a private unix control socket. Each token is bound
  to the AO subject (`AO_USAGE_SUBJECT` or `session:<AO_SESSION_ID>`).
- **Automatic events.** Every forwarded request writes
  `ATTEMPT_DISPATCHED` before bytes leave for the upstream, and
  `ATTEMPT_FINALIZED` after the response ends.
- **Rejected requests.** The following are refused, recorded, and make the
  position malformed:
  - any non-inference endpoint (`/api/hello` is answered locally);
  - an unbound token;
  - a role that AO's control plane cannot resolve.
- **Where role and call_class come from.**
  - `role` is read from AO's `usage_attribution_windows`, at request time.
  - `call_class` is derived from the request structure (proxy rules v1).
  - A byte-identical replay after RETRYABLE / RATE_LIMITED is classed as
    `retry`, and counts against the frozen budget.
- **Deadlines.**
  - The frozen `provider_attempt_seconds` bounds each forwarded attempt; when
    it expires the attempt ends as `TIMEOUT`.
  - A dispatch after a role's frozen deadline is refused, and the position
    ends as `TIMEOUT`.
  - `position_seconds` bounds the whole AO run.
- **Escape detection.** After the run:
  - every Claude Code `model_usage_events` key AO recorded must map to a
    message id the proxy saw;
  - AO's Codex usage totals per subject must not exceed what the proxy
    observed.

  Anything else is `MALFORMED_POSITION`. The Seatbelt profile allows network
  only to the proxy and daemon ports, so a direct call cannot connect in the
  first place.

## P0-2 — M3 from 3C

`DeriveM3` opens the position DB read-only and requires complete
`agent_tool_coverage` for the measured role's sessions. It then matches every
`agent_tool_observations` event key against the proxy's message ids
(`usage.ClaudeMessageEventKey`), and the tool-use counts per message must be
equal. Calls are ordered by proxy `call_index`. The milestone is:

- A/B/D: the first edit by the worker;
- C: the reviewer's `ao review submit`.

The driver never declares M3.

## P0-3 — isolation

`sandbox-exec` wraps every agent process, using the profile in `sandbox.go`
(ported from the lab `agent.sb`).

- **Denied:**
  - `~/.ao`, except the position's own work, worktrees, runtime-home, tmp,
    run file, prompts, hook-bin, and the read-only tools;
  - the AO source tree;
  - the private control directory (ledger, registry, oracle, other
    positions);
  - operator provider state, except reading `~/.codex/auth.json`;
  - all network except the two local ports and unix sockets in the position
    tmp;
  - exec of `launchctl`, `open`, `osascript` and `tmux`.
- **Environment.** The shim applies an env allowlist and forces HOME,
  TMPDIR and CODEX_HOME to position-local paths.
- **Tests.** Adversarial probes in `sandbox_test.go` cover reading and
  writing the denied paths, direct network access, and nested
  `sandbox-exec` escapes.

## Related P1s

- **Provenance.** A request is classed as Project Memory by AO's render
  markers (`AO-UNTRUSTED-REPOSITORY-CONTEXT`, `MEMORY FRESHNESS:`,
  `AO project memory`) plus the exact frozen attachment. Repository lines
  that legitimately reach OFF agents do not count.
- **Treatment targeting.** `AO_MEMORY_ROLES` (new AO environment variable)
  limits Project Memory to the task's measured role:
  - worker for A/B/D, and repair with it, since repair is the same session;
  - reviewer for C.
- **Calibration.** A provider-free AO run captures the real initial worker
  prompt, and the ASSISTED attachment is extracted from it by AO's
  `pack_digest`.
- **Fixture.** `fixture_subtree_sha256` is `sha256(git ls-tree -r
  --full-tree <commit>)` and is verified in `Prepare`.
- **Account attestation.**
  - Claude: SHA-256 of the `anthropic-organization-id` response header.
  - Codex: SHA-256 of the `Chatgpt-Account-Id` request header.

  The manifest's `account_ref_sha256` is the digest of the canonical
  per-provider map. Each finalized attempt carries its own reference.

## External anchor assessment

The ledger is append-only and hash-chained inside the supervisor's private
control directory, and agents cannot reach that directory (P0-3). This
protects the ledger against the agent. It does not protect it against the
operator, who could rewrite it before reporting.

No external timestamping or remote anchor is implemented. The norm does not
require research-grade anchoring, and adding one was out of scope ("NO
research-grade"). The residual risk is operator tampering, not agent
tampering. It is mitigated by committing the ledger head digest to the
report, and by the raw request/response evidence the proxy stores next to it.

## Open items

- **Task C.** The reviewer calibration is not implemented: provider-free
  calibration cannot reach AO's review stage, because the worker must finish
  first. The M3 cross-check for Codex (reviewer) calls and Q6 scoring are not
  wired to the real oracle either. Task C positions cannot run, so C is
  BLOCKED.
- **Codex reviewer bootstrap (blocks every high-risk task).** Real
  mini-E2E run 10 (A/OFF, `wf-77f23c2f`) got through the observed worker:
  10 dispatched/finalized pairs, all SUCCESS, role from the control plane.
  AO then launched the Codex reviewer, as its high-risk cross-provider
  independence requires. The Codex TUI bootstrap failed with
  `account/read failed: workspace routing discovery failed`: that call goes
  to `chatgpt.com`, which the sandbox denies. `chatgpt_base_url` only accepts
  an HTTPS origin, so it cannot be pointed at the plain-HTTP proxy. Serving a
  local TLS origin with a per-position CA (`CODEX_CA_CERTIFICATE`) is
  technically possible but is a TLS-interception change that needs explicit
  operator approval. The alternatives are:
  - (a) that local TLS origin, with an allowlist of bootstrap endpoints;
  - (b) a sandbox egress to chatgpt.com for the reviewer: weaker isolation,
    with direct calls detected only after the run by the AO usage totals
    check;
  - (c) a same-provider (Claude) reviewer, which departs from AO's default
    high-risk routing.

  Until one is chosen, every position of a high-risk task ends at the
  reviewer.
- **Role deadlines.** Enforced at dispatch, plus a per-attempt deadline on
  every forwarded attempt. A role that stops calling the provider (for
  example a stuck reviewer) is bounded only by `position_seconds` through
  the run timeout.
