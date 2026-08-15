# Headless web mode (local)

Checkpoint 8G runs the existing AO daemon and React renderer without Electron.
It is intentionally local-first: the primary listener remains
`127.0.0.1`, has no production authentication, and cannot be changed to a
non-loopback host.

## Build and run

From the repository root:

```bash
npm --prefix frontend install
npm --prefix frontend run build:web
cd backend
go run ./cmd/ao server --port 3001 --web-root ../frontend/dist/web
```

Open <http://127.0.0.1:3001>. Deep links such as
<http://127.0.0.1:3001/workflows> and
`/workflows/<workflow-id>` are served through the SPA fallback.

For isolated state:

```bash
cd backend
go run ./cmd/ao server \
  --port 43127 \
  --data-dir /absolute/path/to/ao-data \
  --web-root ../frontend/dist/web
```

`--data-dir` also places that server's `running.json` inside the selected
directory so it does not collide with the desktop daemon. Stop the foreground
server with Ctrl-C; SIGINT and SIGTERM use the daemon's existing graceful
shutdown and recovery path.

The web build is static. Node.js is needed to build it, not to serve it. A
packaged server can place the `dist/web` contents in a `web` directory beside
the `ao` binary; `ao server` auto-detects that layout. `--web-root` is the
explicit override.

## Browser and desktop capability boundary

The renderer chooses one platform adapter at startup:

- Desktop uses the Electron preload bridge for daemon discovery and native
  capabilities. API traffic continues to target the discovered loopback port.
- Web probes same-origin `/readyz` and uses same-origin `/api/v1`, `/mux`, and
  SSE. No CORS exception is needed.

Workflow list/detail/create, master-plan generation and approval, execution,
polling, cancellation, review/fix/verify state, and recovery all use the same
daemon API in both modes. Workflow detail polling remains at two seconds; 8G
does not add another realtime transport.

A browser cannot use a native OS folder picker on the server. Web mode instead
lets an operator register an existing repository or clone one from GitHub
through Settings → Projects, confined to the directories configured via
`AO_PROJECT_ROOTS` (see "Setup UX and integrations" below) — no CLI/desktop
step is required for this anymore. Registering outside those configured
roots still requires the CLI or desktop app. Native folder dialogs,
embedded Electron BrowserViews, window chrome, tray/dock integration,
auto-update, and desktop lifecycle supervision are desktop-only and degrade to
no-ops or unavailable controls in browser mode. Clipboard and external links
use browser APIs where available.

## Health and security

`GET /healthz` and `GET /readyz` retain the daemon probes and include
`frontendAvailable`. Storage is opened and migrations/recovery wiring complete
before the HTTP listener starts, so readiness implies storage initialization.
Provider health is intentionally outside 8G.

The server does not allow wildcard CORS, arbitrary origins, arbitrary command
execution, or generic filesystem access. It serves only the selected compiled
asset directory and the existing API. Remote deployment, TLS, and
authentication belong to a later checkpoint.

## Setup UX and integrations (checkpoint 8H.5)

Settings is a real "is this box ready to run autonomous workflows" console in
both web and desktop mode, driven entirely by real local probes — never
invented status. Open it via the gear icon (desktop) or the same in-app
Settings modal (web); it has four sections relevant to headless setup:

- **Environment** — a top-level readiness summary (Codex / Claude / GitHub /
  Projects / Headless), plus an overall "Ready for autonomous workflows" vs.
  "Setup required" verdict. Overall readiness today requires at least one
  registered project and at least one installed-and-authorized development
  agent; GitHub is reported but does not currently gate it.
- **Development Agents** — Codex and Claude Code: installed, resolved binary
  path, version, auth state (`authorized` / `unauthorized` / `unknown` — never
  shown as "Connected" unless authentication was actually verified), and a
  cheap "Test connection" button that reuses the existing agent probe
  (`POST /api/v1/agents/{agent}/probe`); it never runs a real task or changes
  auth.
- **Source Control** — the `gh` CLI: installed, version, and auth state via
  `gh auth status` (never `--show-token`; no GitHub REST call, no token is
  ever read, stored, or displayed).
- **Projects** — every AO-registered project (display name, path, origin URL,
  default branch, kind, and a validity check), plus "Register existing
  repository" and "Clone from GitHub".

### Agent prerequisites

| Tool | Install | Auth |
|---|---|---|
| Codex CLI | `codex` on `PATH` (or a well-known install location) | `codex`'s own login flow; AO only reads its local auth status, never modifies it |
| Claude Code CLI | `claude` on `PATH` | `ANTHROPIC_API_KEY` env, or `claude`'s own login (`~/.claude` config / `claude auth status`) |
| GitHub CLI (`gh`) | `gh` on `PATH` | `gh auth login` (device or web flow) for the account that should own clones and PR operations |

### Allowed project roots

Web-originated project registration, the Settings → Projects browse listing,
and Clone-from-GitHub are confined to directories set via `AO_PROJECT_ROOTS`
(comma-separated absolute paths). This is unset by default (no restriction —
the historical desktop behavior, where the OS file picker is the trust
boundary); set it before exposing AO's Settings surface beyond your own
machine:

```bash
export AO_PROJECT_ROOTS=/srv/ao/repos
# or multiple roots:
export AO_PROJECT_ROOTS=/srv/ao/repos,/home/ao/repos
```

Paths are canonicalized (symlinks resolved) and checked against these roots
before any registration, browse, or clone proceeds; a path that resolves
outside every configured root — including via a symlink planted inside one —
is rejected. Clone always targets the first configured root.

### Register an existing repository (web)

Settings → Projects → "Register existing repository": type a path relative to
an allowed root (or click "Browse" to list its subdirectories, which flags
which ones already look like Git repos), then "Register". The repository must
have at least one commit.

### Clone from GitHub (web)

Settings → Projects → "Clone from GitHub": enter `owner/repo` or an
`https://github.com/owner/repo` URL and an optional destination folder name.
Requires `gh` to be authenticated (checked before the clone runs); the clone
target must not already exist. On success the repository is registered
automatically.

### Creating a workflow (web or desktop)

The Workflows page's "New workflow" form selects a project from a dropdown
populated by AO's registered projects — the internal project id is never
typed by hand. If no projects are registered, the form shows a "No projects
registered. Register or clone a repository first." message with a direct link
into Settings → Projects instead of a raw `WORKFLOW_NOT_FOUND` error.

## Linux prerequisites

Runtime requirements are:

- the built `ao` binary and the compiled `web` asset directory;
- Git and repositories accessible on the server;
- `tmux` for the existing Linux runtime adapter;
- Codex CLI and Claude Code CLI, installed and authenticated for the service
  account running AO;
- `gh` (GitHub CLI), installed and authenticated, for GitHub-backed clone/PR
  flows;
- a writable AO data directory (default `~/.ao/data`);
- `AO_PROJECT_ROOTS` set to the directory(ies) repositories may be
  registered/cloned into from the web Settings UI (see "Setup UX and
  integrations" above);
- loopback TCP port 3001 by default.

Go and Node.js are build-time dependencies when compiling from source. They are
not required by a prebuilt AO binary with precompiled web assets. Provider and
SCM credentials use the existing AO/CLI environment; do not place secrets in
the frontend assets.
