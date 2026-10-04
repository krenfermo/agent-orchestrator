# agent-orchestrator status

Current `main` ships a working single-user local loop: the Go daemon and the
Electron/React frontend both drive a live daemon over HTTP/SSE/WebSocket. The
core GitHub flow works end-to-end: add project → spawn session/orchestrator →
attach terminal → observe PR → merge.

This file tracks progress. For what the product _is_ and how to run it, see the
top-level [`README.md`](../README.md); for the backend mental model see
[`architecture.md`](architecture.md).

## Build & test

The local gate is the backend Go build and race-enabled test suite:

```bash
cd backend && go build ./... && go test -race ./...
```

`npm run lint` (from the repo root) runs `go test ./...` plus golangci-lint.
Frontend checks live under `frontend/` (`npm run typecheck`, `npm run build`).
See [`AGENTS.md`](../AGENTS.md) for the regen workflow when touching the API
surface (`npm run sqlc`, `npm run api`).

## Shipped

### Backend (Go daemon)

- Loopback-only HTTP daemon (chi router, CORS, per-request timeout,
  `/healthz` / `/readyz` / `/shutdown`).
- SQLite store with goose migrations and sqlc-generated queries; DB
  trigger-based change-data-capture into `change_log`.
- CDC poller + broadcaster feeding in-process subscribers and the SSE stream
  at `GET /api/v1/events` (with `Last-Event-ID` replay).
- Full session lifecycle over HTTP: list, get, spawn, kill, restore, rename,
  rollback, cleanup, send, activity, PR claim/list. Orchestrator routes
  (list/spawn/get) are wired too.
- One daemon-committed interface per session. TUI sessions retain the established
  tmux/conpty agent runtime; Chat sessions use runtime-less native controllers,
  persist provider conversation identity, and dispatch lifecycle reactions
  through the same mode-aware session manager. A durable, capability-gated
  drain/interrupt handoff can move the same Claude Code or Codex native
  conversation between TUI and Chat without changing the AO session/worktree;
  rollback, restart recovery, controller-generation fencing, and a transition
  message outbox preserve the one-controller invariant.
- Durable Chat conversations with project-scoped orchestrator continuity,
  session-scoped worker history, bounded history pages, transactional raw-event
  archive/projection, controller-generation fencing, turns, messages,
  activities, approvals, structured input, usage, compaction, and rollback.
- Chat drivers for the user's installed Codex (native app-server), Claude Code
  (claude-agent-acp), OpenCode, and Droid. AO reuses each harness's existing
  binary/auth resolution and does not bundle provider CLIs.
- Project CRUD plus per-project config (`PUT /projects/{id}/config`).
- PR action engine wired into the API: `POST /prs/{id}/merge` and
  `/prs/{id}/resolve-comments`.
- Review routes registered: `GET /reviews`, `POST /reviews/execute`,
  `POST /reviews/{id}/send`.
- Interactive reviewer panes for Aider, Agy, Amp, Auggie, Autohand,
  Claude Code, Cline, Codex, Continue, GitHub Copilot, Crush, Cursor, Devin,
  Droid, Goose, Grok, Kilo Code, Kimchi, Kiro, Kimi, OpenCode, Pi, Qwen, and Vibe. Pi uses an AO-data-owned extension with built-in/project
  resources disabled, structured read-only inspection/reporting tools, and
  Escape-based turn cancellation. Kiro also uses its native Escape
  cancellation. Continue, Qwen, and Vibe also use Escape cancellation. Agy,
  Continue, Devin, Droid, Goose, Kimchi, Kimi, Qwen, and Vibe are explicitly experimental and host-trusted. Grok, Crush, Auggie, Cline, and Autohand are experimental user-approved reviewers that retain their native approval prompts instead of receiving broad unattended flags:
  native modes, autonomous settings, and prompts are not OS or network containment.
- The provider-neutral interactive-reviewer capability gateway and neutral
  AO-owned working-directory contract are available. The experimental
  host-trusted adapters remain candidates for future contained execution once
  their documented sandbox, environment-replacement, broker, and gateway
  prerequisites are implemented.
- Durable dashboard notifications for `needs_input`, `ready_to_merge`,
  `pr_merged`, and `pr_closed_unmerged`: backend enrichment/persistence,
  cursor-paginated read/unread history, live notification stream, and read
  acknowledgement API.
- SCM observer (`internal/observe/scm`) wired into the daemon: GitHub provider,
  lazy/non-blocking auth, per-PR polling with ETag guards and semantic diffing,
  feeding PR facts into lifecycle, which sends agent nudges for CI failures,
  review feedback, and merge conflicts
  ([#75](https://github.com/aoagents/agent-orchestrator/issues/75),
  [#108](https://github.com/aoagents/agent-orchestrator/issues/108),
  [#109](https://github.com/aoagents/agent-orchestrator/issues/109)).
- Terminal mux over WebSocket (`/mux`): per-client `tmux attach` PTY on
  Darwin/Linux; conpty loopback pty-host on Windows.
- Lifecycle reducer plus reaper (`internal/observe/reaper`).
- Truthful terminal state for an ordinary task. A task IS a session, and a task
  that succeeds stays alive and idle, so "finished" and "never did anything"
  used to be the same row and both rendered "Idle"/"Inactivo". Lifecycle now
  records a durable completion receipt (`sessions.turn_completed_at`) when the
  agent itself reports its turn ended — a Stop-class hook or a Chat driver's
  turn-completed event — and clears it the moment work is in flight again.
  `DeriveStatus` turns that receipt into `completed`, ranked below every
  failure, teardown, attention and pull-request state, so a failed, cancelled or
  question-asking task can never read as finished. Nothing is inferred from
  idleness, a stopped runtime or elapsed time, and because the fact is durable
  the status survives inactivity and daemon restarts. Historical sessions are
  promoted only where AO already recorded the proof (a session-owned execution
  lock released because the task's turn ended).
- Agent adapter platform under `internal/adapters/agent/` (25 adapters) with a
  registry and `ao hooks` activity dispatch.
- **Durable project memory (P2-A)**: a bounded, provenance-carrying summary of
  a project — modules, conventions, instructions, dependencies, build/test
  commands, architecture documents, and per-task outcomes/decisions/risks —
  kept in SQLite under generation-conditioned CAS, with a restart-safe bounded
  indexer, a git-diff-driven incremental update path, drift detection with
  four explicit states, and bounded deterministic per-role context packs for
  Planner, Worker, Reviewer and Repair. It is a **cache, never a source of
  truth**: where memory and the working tree disagree the working tree wins and
  the fact is withheld rather than served. Inspectable via `ao memory
  status|inspect|rebuild|invalidate` and `/api/v1/projects/{id}/memory`.
  Delivery to agents rides the existing `AO_CONTEXT_ROUTER` flag and is off by
  default; nothing schedules an indexing pass automatically yet. See
  [`project-memory.md`](project-memory.md).
- **Project memory in the execution cycle (P2-B)**: indexing and incremental
  sync now happen automatically at the Planner, Worker, Reviewer and Repair
  boundaries, coalesced so four roles cost at most one sync and a warm project
  costs none. Memory is attached as a bounded, per-role budgeted pack and
  deduplicated against the context AO was already sending, with three rollout
  modes (`AO_MEMORY_MODE=off|assisted|preferred`, default **off**). A verified,
  committed task records its bounded outcome and — only where the placement
  proves integration — promotes it; a cancelled one discards it. Measured on
  this repository: planner AO-assembled context 87,209 → 40,378 bytes (−53.7%)
  in `preferred` mode, with a warm second task reading zero files. Those figures
  cover AO-assembled context only; harness reads remain unobservable. Inspect
  with `ao memory report`. See
  [`project-memory-optimization.md`](project-memory-optimization.md).
- **Shared task knowledge (P2-C)**: what one task learns is reusable by the
  next one, and only by the ones it is relevant and safe for. Task outcomes,
  decisions and risks carry a bounded lifecycle on the existing memory item —
  active, superseded, resolved, obsolete, conflicting — so a re-decided topic
  retires its predecessor and names it rather than overwriting it, and a
  resolved risk stops being served while keeping the task that closed it. Three
  sharing scopes (task / workflow / canonical) encode sibling safety directly:
  an unintegrated worktree's knowledge reaches only the tasks that declared a
  dependency on it inside its own run, and becomes canonical only when the
  integration authority can prove the work landed. A relevance gate keyed on
  EVIDENCE keeps prior task history away from unrelated work. Measured on a
  store of twelve prior tasks in one module: a task in the same module receives
  32 shared facts (6,203 pack bytes, ~1,551 tokens); a task in a different
  module receives **zero** from the same 32 candidates (1,450 bytes). Every
  execution's context is frozen into a durable manifest of fact identities, so
  what a Worker knew is checkable rather than suspected. Inspect with `ao memory
  knowledge|decisions|risks|task|context`. See
  [`shared-task-knowledge.md`](shared-task-knowledge.md).
- OpenAPI spec generated from Go DTOs; frontend TS types generated from it and
  drift-checked in CI.

### Frontend (Electron + React)

- Electron + React 19 + TanStack Router/Query + Tailwind + shadcn primitives.
- Target-isolated per-session browser-control spike: a dedicated local
  daemon↔Electron bridge drives only the selected session's `WebContentsView`
  through Electron's bound debugger transport. `ao browser` supports open,
  compact accessibility snapshots and refs, click/fill/type, keyboard input,
  hover and non-mutating element highlighting, scrolling, selection and checked
  state, property reads, stable logical tabs and captured popups, a compact
  user-facing tab selector for switching/closing tabs and popup notices, waits,
  including load/disappearance/DOM-stability conditions, screenshots, console
  messages, page errors, and explicit temporary network-metadata capture while
  the Browser panel is hidden. Network capture is off by default, tab-scoped,
  bounded, automatically expires, and omits bodies and sensitive values. Tabs
  within one worker share an ephemeral Electron profile; different workers
  have isolated cookies and web storage. The browser tab menu is only a tab
  navigation control: it does not render a global activity pill or a
  tab-specific agent marker. Annotation progress is separate and its
  successful-delivery confirmation clears automatically.
- Chromium's official DevTools frontend is available from the direct Browser
  toolbar button, `Ctrl+Shift+I` (Cmd+Option+I on macOS), the titlebar View menu,
  and `ao browser devtools`. It opens in a detached desktop window with normal
  OS close controls and is attached through the same worker-scoped CDP
  multiplexer as the agent, so Elements, Console, Network, Sources, and other
  DevTools panels can remain open while agent automation continues. The
  user-facing DevTools connection is unrestricted; agent CDP commands remain
  policy-limited.
- Preview targets are explicit: `ao preview`, `ao preview <target>`, or
  `ao preview start` selects what the panel shows. The desktop poller no longer
  auto-discovers a static entry point merely because a fresh worker exists.
- Real daemon wiring via the generated `openapi-fetch` typed client
  (`src/api/schema.ts`); mock data only in `VITE_NO_ELECTRON` web-preview mode.
- Electron main handles daemon discovery, launch, and status reporting.
- Shell: sidebar (projects + sessions, add/remove project), sessions board,
  session view + inspector, project settings, pull-requests page,
  spawn-orchestrator flow.
- SessionView renders from the session's persisted mode: the existing terminal
  surface for TUI, or the durable Chat timeline/composer for Chat. Chat retains
  access to session-scoped worktree shells without creating an agent tmux pane.
- Compatible Claude Code and Codex sessions expose an in-session “Open Chat” /
  “Open Terminal UI” action. Idle sessions switch directly; busy sessions offer
  an explicit finish-and-drain or stop-and-interrupt policy and show durable
  progress/recovery state.
- Desktop status and SCM summary V1: session status comes from
  `GET /api/v1/sessions`; visible/active PR context comes from
  `GET /api/v1/sessions/{sessionId}/pr`; `GET /api/v1/events` is kept open as
  an invalidation stream rather than a full PR payload stream.
- Concise PR summaries include PR identity, CI state with failing check names
  and links, human reviewer IDs/counts/links for unresolved review comments,
  and mergeability reasons. Raw CI logs and review comment bodies are
  intentionally not part of the desktop V1 API/UI.
- Terminal pane (xterm) over the mux WebSocket, with a live SSE events
  connection and port-rebind on daemon restart.
- Chat history uses bounded pages and targeted CDC/SSE invalidation rather than
  polling and transferring the full lifetime of a conversation.
- In-app notification center with click access, Unread/All filters, paginated
  REST catch-up, live notification stream updates, separate PR/session target
  actions, persistent read history, mark-read controls, and Electron app toasts
  while the app is running.

### Mobile (Expo + React Native)

- Connect Mobile pairs with the daemon's opt-in authenticated LAN listener; the
  loopback listener and its security model remain unchanged.
- New mobile workers and orchestrators request Chat mode by default. Worker
  creation filters to the daemon-advertised Chat harnesses, while Terminal UI
  remains an explicit compatibility choice and typed Chat preflight failures
  offer that fallback.
- Session routing uses the same daemon-committed mode as desktop. TUI keeps
  the existing authenticated mux/xterm surface; Chat uses the same durable,
  paged conversation projection and CDC/SSE invalidation stream as desktop.
- Mobile exposes the same capability-gated TUI↔Chat handoff, busy-turn policy,
  cancellation window, progress overlay, and automatic renderer swap after the
  daemon commits the new controller.
- Native Chat includes prose/Markdown, provider activity, commands, plans,
  changed files, approvals, structured input, model/effort/provider controls,
  compaction, rollback, MCP recovery, skills and file references, staged/native
  image delivery, embedded text resources, voice dictation, retryable delivery,
  persisted drafts, and a session-scoped worktree shell through the existing
  terminal mux.

## In flight / not yet a runtime feature

- **Task queue for direct-branch contention**: an ordinary task that cannot take
  its project's repository+branch execution lock currently **fails fast** with a
  409 naming the current owner (Checkpoint 8P-E.14). A workflow in the same
  situation parks in a durable `waiting_for_branch` state and resumes by itself
  when the lock frees, because a run has an outbox, checkpoints and a wake
  scheduler to park in; a task has none of those, so the equivalent behavior
  needs a queued-task subsystem rather than a flag. Still to build: a durable
  queued-task state, restart recovery for queued tasks, wake-on-lock-release,
  automatic spawn/resume, and a truthful "Waiting for branch" state on the
  Board. Failing fast is deliberately the safe interim: the one thing a blocked
  task must never do is create a derived branch to work around the contention.
- **A follow-up turn that loses its branch**: a task session owns its
  repository+branch for the duration of a turn and gives it back when the turn
  ends (Checkpoint 8P-E.14A), so a second task may legitimately take that branch
  while the first sits finished and idle. If the user then sends the first task
  more work, its turn start finds the branch owned by someone else. AO logs that
  truthfully and the turn proceeds without the lock, because the signal that a
  turn started is a hook that fires *after* the prompt was submitted — there is
  nothing left to refuse, and AO will not silently reroute the work to another
  branch. Still to build: a refusal at the mediated send/resume paths (before
  the prompt reaches the agent) and a Board state saying the session's branch
  was taken over. The task queue above is the same missing subsystem seen from
  the other end.
- **A non-PR "ready to merge" for isolated-worktree tasks**: a finished task now
  reads `completed` from its own durable completion receipt (see Shipped), but
  in isolated-worktree mode "finished" and "ready for you to take the branch"
  are still the same word. Distinguishing them needs a durable review/merge
  readiness fact, not another status derived from the same receipt.
- **Browser automation acceptance**: the runtime implementation is complete.
  AO packages one
  checksum-pinned Vercel `agent-browser` Rust binary and routes a deliberately
  limited semantic command set through an authenticated, worker-scoped CDP
  bridge to the existing AO Preview. The binary is prepared automatically for
  desktop development and releases and is the single engine behind ordinary
  `ao browser` inspection and interaction commands. AO retains only its
  sanitized network observer and temporary highlight cleanup as safety/UI
  plumbing. Focused checks and a fresh Windows x64 package pass; macOS/Linux
  packaging and manual lifecycle acceptance remain release verification work.
- **Cross-interface visual history import**: provider-native context continues
  across a compatible handoff, and Chat history already recorded by AO remains
  durable. A first TUI→Chat switch does not reconstruct terminal screen output
  as structured AO messages/tool cards; doing so requires a provider history
  import contract with stable identities and deduplication.
- **In-flight tool portability**: drain can finish accepted work and interrupt
  can cancel it, but no common provider protocol serializes a currently executing
  tool call or detached background process for adoption by another controller.

- **Tracker lane**: GitHub tracker adapter exists, but there is no daemon
  observer loop or agent-lifecycle→issue mirroring yet, so the tracker does
  nothing at runtime ([#112](https://github.com/aoagents/agent-orchestrator/issues/112)).
- **Full raw PR/tracker fact surfacing**: the SCM observer writes facts and the
  desktop consumes concise PR summaries, but exposing the full raw `pr_*` /
  `tracker_*` CDC events to live consumers
  ([#110](https://github.com/aoagents/agent-orchestrator/issues/110)) and in
  `ao session get` ([#111](https://github.com/aoagents/agent-orchestrator/issues/111))
  is still open.

Tracking milestone:
[`rewrite`](https://github.com/aoagents/agent-orchestrator/milestone/1).
