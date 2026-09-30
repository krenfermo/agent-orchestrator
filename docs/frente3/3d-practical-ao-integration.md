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

## Hardening from the Codex adversarial reviews (R1-R3)

- **Accounting.** The norm is applied literally (3d-practical §accounting):
  a provider response without usage is `MISSING` and the position becomes
  `MALFORMED_RESULT`. Zero is never imputed: not for error responses (429,
  5xx, error envelopes), not for absent cache fields (Anthropic
  `cache_read`/`cache_creation`, OpenAI `cached_tokens`). The consequence is
  that any provider error without usage malforms its position.
- **M3.**
  - Every 3C tool row is bound to a `tool_use` id the provider returned in
    that message (`usage.ClaudeToolObservationKey`).
  - Its op is recomputed from the provider's input with 3C's own classifier
    (`usage.ClaudeToolOp`). Its path must be the provider's target, and a
    target inside the project may not appear as outside/unresolved.
  - Order within a message is the provider's.
  - Every provider tool use must have a row.
  - The worker milestone is the first edit of a project file.
  - The reviewer milestone is an actual `ao review submit` invocation, and
    the number of submissions must equal the verdicts AO recorded.
- **One upstream request per attempt.** The proxy never follows redirects,
  and never lets net/http replay a request (HTTP/1 reuse retry, HTTP/2
  GOAWAY): the body is not rewindable.
- **Call class.** An AO subject has exactly one `initial` request; a later
  request with no assistant turn is refused. Claude's subagent tools
  (`Task`, `Agent`) are disallowed by flag and refused on the wire, because
  sidechain exploration would escape M3. Known limitation: Claude Code's
  auto-compaction restarts a conversation, and would therefore malform a
  position; it is not expected at task scale.
- **M3 milestone.** The worker's first edit must also have succeeded (3C
  `result_error = 0`), and an absolute target must lie under the project
  roots.
- **Harness state isolation.** Inside the shared position HOME, Claude Code
  cannot read Codex's state (`codex-home`) and Codex cannot read Claude's
  (`.claude`). The ASSISTED reviewer therefore cannot read the worker's
  transcript with its Project Memory. What the reviewer does see is the
  worker's diff, which is the treatment's legitimate downstream effect.
- **Oracle execution (R5).** Q1 and Q4 compile and run the worker's
  commit, i.e. agent-authored code. Both now run under an oracle Seatbelt
  profile:
  - no network at all;
  - `~/.ao`, the AO source, the operator's oracle directory, provider
    state and keychains are denied;
  - the environment is scrubbed.

  They run on a private staged copy of `oracle.sh` and the hidden tests.
  The copy is hidden during Q1, read-only during Q4, and digest-verified
  before Q1, before Q4 and after Q4. The clone's origin is removed.

  Residual, accepted for Practical (no VMs): code in the worker's commit
  runs inside the same test binary as the hidden tests, so a deliberately
  adversarial commit could fake its own test output. The oracle's inputs
  and the environment are protected; the in-process verdict of hostile
  code is not.
- **Wire truth for M3 and dose (R5).**
  - Whether a tool call succeeded is taken from the `tool_result` the
    client sent back to the provider, not from the agent-writable
    transcript; a result that changes between requests is refused.
  - A subject's first non-helper request must be `initial`.
  - The frozen attachment must occur exactly once per ASSISTED request.
  - Bash rows carry no path in 3C, so rewriting a Bash command in the
    transcript cannot change M3's file count (R5 claim checked and not
    reproducible).
- **Oracle.** The Q4 oracle directory is denied to agents wherever the
  operator keeps it.
- **Daemon gateway.** AO's API also serves Project Memory (items,
  knowledge, graph, manifests) and session views carrying the worker's
  prompt. Agents reach the daemon only through a harness gateway, which
  they find through a forced `AO_RUN_FILE`; the sandbox allows only the
  gateway port.
  - The gateway forwards only what AO's agent-side CLI uses: health/ready
    probes, the usage subject hook, session/review activity hooks, review
    submit/list.
  - Everything else is refused and recorded
    (`daemon-gateway-refused.json`).
- **Provenance.**
  - Every Project Memory render marker in a request must lie inside a copy
    of the frozen attachment, and a request without an attachment may carry
    none.
  - With the gateway and the sandbox, the frozen attachment is the only way
    memory content enters a conversation. Paraphrased memory is therefore
    the treatment's downstream effect, not a second input.

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
