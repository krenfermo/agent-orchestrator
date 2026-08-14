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

A browser cannot select a repository directory on the server. Web mode uses
projects already registered in AO. Register new repositories with the CLI or
desktop app, then select/view them in the browser. Native folder dialogs,
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

## Linux prerequisites

Runtime requirements are:

- the built `ao` binary and the compiled `web` asset directory;
- Git and repositories accessible on the server;
- `tmux` for the existing Linux runtime adapter;
- Codex CLI and Claude Code CLI, installed and authenticated for the service
  account running AO;
- a writable AO data directory (default `~/.ao/data`);
- loopback TCP port 3001 by default.

Go and Node.js are build-time dependencies when compiling from source. They are
not required by a prebuilt AO binary with precompiled web assets. Provider and
SCM credentials use the existing AO/CLI environment; do not place secrets in
the frontend assets.
