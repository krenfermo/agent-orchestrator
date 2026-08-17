# Hetzner staging deployment (checkpoint 8O)

Status: **STAGING**, not production. This document and the artifacts under
`deploy/` prepare AO to run as a persistent Linux service on a Hetzner VM,
reachable over HTTPS with a minimal auth gate. No live server was touched to
produce this checkpoint — see "What was not done" at the end.

This builds on [`headless.md`](headless.md) (checkpoint 8G, local headless
web mode). Read that first for the daemon/web-mode split; this document only
covers what changes to run it as a systemd-managed remote service.

Guardrails carried over unchanged from `AGENTS.md`'s hard rules:

- AO's primary listener stays bound to `127.0.0.1` and unauthenticated,
  always — `ao server` enforces this at the flag-parsing level
  (`backend/internal/cli/server.go`), it is not a config toggle. Remote
  access is added entirely by a reverse proxy in front of it, never by
  changing AO's bind host.
- The separate LAN "Connect Mobile" listener (`docs/adr/0001-lan-listener-for-mobile.md`)
  is a distinct, deliberately weaker, home-network-only mechanism. It is
  **not** used for staging remote access — do not enable it as a substitute
  for the reverse-proxy auth described below.
- All AO state stays under one data root (`AO_DATA_DIR`), never
  `~/Library/Application Support` or another OS default — same rule as
  desktop, just pointed at a Linux path.

## 1. Linux portability audit

| Area | Finding | Classification |
|---|---|---|
| Codex/Claude/gh/git binary discovery | `backend/internal/adapters/agent/binaryutil` resolves via `exec.LookPath` on `$PATH` first, then platform candidate paths (`UnixPaths`/`UnixHomePaths`), never a hardcoded Homebrew path as the only source | **portable** |
| tmux runtime | `backend/internal/adapters/runtime/tmux` always launches its own isolated server via `tmux -L <socket>` (see `AO_TMUX_SOCKET`), never assumes or attaches to a caller's default tmux server | **portable** |
| Electron / desktop app | Not part of the server process at all — `ao server` is the Go binary + static web assets, no Electron dependency | **portable** (desktop-only concern doesn't apply to `ao server`) |
| macOS Keychain | No references found in `backend/` or `frontend/src` (`grep -rl -i keychain`) — auth for codex/claude/gh is delegated entirely to those CLIs' own credential stores | **portable** |
| `osascript` | No references found in `backend/` or `frontend/src` | **portable** |
| Homebrew paths (`/opt/homebrew`, `/usr/local/Cellar`) | No references found in `backend/` or `frontend/src` | **portable** |
| `~/Library/Application Support` | Referenced only inside `binaryutil`'s `fnm` (Node version manager) macOS-specific candidate path, guarded by `runtime.GOOS == "darwin"` — used only as one extra probe location, never required | **portable** (guarded, non-blocking) |
| Windows-specific files (`process_windows.go`, `sigpipe_windows.go`) | Build-tag separated from the Unix path; Linux uses the Unix/POSIX code paths (`process_unix.go`, `sigpipe_unix.go`) already exercised by macOS CI | **portable** |
| Electron `userData` (`frontend/src/main.ts` pin to `~/.ao/electron`) | Desktop-only; irrelevant to `ao server`, which never starts Electron | **desktop-only** |
| `AO_TMUX_SOCKET` comment scope | Config doc comment says "Darwin/Linux only" — Windows uses a different runtime adapter, not in scope here | **portable** (Linux is a first-class target already) |
| Bind host | `ao server --host` is hard-rejected unless it equals `127.0.0.1` (`config.LoopbackHost`) — this is correct and unchanged; remote exposure is proxy-only | **portable by design**, not a gap |
| Native OS folder picker (desktop) | Explicitly called out in `headless.md` as a browser-mode gap already solved by `AO_PROJECT_ROOTS`-scoped registration/clone in Settings | **desktop-only**, already mitigated for web/headless |

**No blocking findings.** Nothing in the current codebase requires
Linux-specific code changes to run `ao server` headless on Ubuntu/Debian; the
gap this checkpoint closes is operational (service supervision, filesystem
layout, reverse proxy, auth), not code portability.

## 2. Filesystem layout

```
/opt/agent-orchestrator/
    bin/ao                  # the compiled server binary (root:root, 755)
    web/                     # compiled frontend (npm run build:web output)

/var/lib/agent-orchestrator/
    data/                    # AO_DATA_DIR — SQLite, running.json, wake state
    repos/                   # AO_PROJECT_ROOTS — registered/cloned repositories
    worktrees/               # workflow git worktrees

/var/log/agent-orchestrator/
    caddy-access.log         # reverse proxy access log (AO itself logs to journald)

/etc/agent-orchestrator/
    ao.env                   # EnvironmentFile for the systemd unit (mode 640, root:ao)

/etc/systemd/system/
    agent-orchestrator.service

/etc/caddy/Caddyfile         # or /etc/nginx/... if nginx is chosen instead
```

Rationale: `/opt` for immutable application code (binary + web build,
replaced wholesale on update), `/var/lib` for durable state that must survive
reboots and updates, `/var/log` for logs journald doesn't already capture
(AO's own stdout/stderr goes to journald via systemd; the reverse proxy's
access log is the one thing that needs a file), `/etc` for config that is
host-specific and never checked into git. This mirrors the layout the
checkpoint brief asked for and matches how most systemd-native Linux daemons
(e.g. `postgresql`, `gitea`) are laid out on Debian/Ubuntu.

## 3. Service user

A dedicated system user `ao` (no login shell, home `/home/ao`), created by
`deploy/scripts/install-linux.sh`. It owns:

- `/var/lib/agent-orchestrator/**` (data, repos, worktrees)
- `/var/log/agent-orchestrator/**`
- its own tmux server socket (`AO_TMUX_SOCKET=ao-staging`, isolated from any
  operator's interactive tmux)
- `~ao` (`/home/ao`), where `codex`, `claude`, and `gh` each keep their own
  auth state (`~/.codex`, `~/.claude`, `~/.config/gh`)

It does **not** own `/opt/agent-orchestrator` (root-owned, read-only to
`ao`) — the service account can run the binary and web assets but cannot
modify them, and it never runs as root. See the `[Service]` hardening block
in `deploy/systemd/agent-orchestrator.service` (`ProtectSystem=strict`,
`ProtectHome=true` with an explicit `ReadWritePaths=` carve-out,
`NoNewPrivileges=true`).

## 4. systemd unit

[`deploy/systemd/agent-orchestrator.service`](../../deploy/systemd/agent-orchestrator.service).

Key properties: `Restart=on-failure` with `RestartSec=5` and a
`StartLimitBurst=5`/`StartLimitIntervalSec=300` circuit breaker (so a
persistently crashing binary doesn't spin forever), `KillSignal=SIGTERM`
mapped to AO's existing graceful-shutdown path (the same one `Ctrl-C`
triggers locally), `EnvironmentFile=/etc/agent-orchestrator/ao.env` for
host-specific config, and `WantedBy=multi-user.target` so it survives
reboot once `systemctl enable` has run.

## 5. Environment config

[`deploy/env/ao.env.example`](../../deploy/env/ao.env.example) — copy to
`/etc/agent-orchestrator/ao.env` (mode `640`, owner `root:ao`; the install
script does this automatically and never overwrites an existing file).
Covers `AO_DATA_DIR`, `AO_RUN_FILE`, `AO_PROJECT_ROOTS`, `AO_TMUX_SOCKET`,
`AO_PORT`, `PATH`, `HOME`. No secrets belong in this file for the staging
scope described here (codex/claude/gh each keep their own credential
stores under `$HOME`, not env vars AO reads).

## 6. Codex / Claude / gh on Linux

`ao doctor` (`backend/internal/cli/doctor.go`) and the Settings →
Environment probes already report install/auth status without assuming a
platform — use them post-install to confirm the `ao` user's view of the
world:

```bash
sudo -u ao -H env PATH="$PATH" which codex claude gh tmux git
```

Do not assume Homebrew-style paths; `binaryutil.ResolveBinary` checks `$PATH`
first, so the only Linux-specific requirement is that `ao.env`'s `PATH=`
line actually lists the directories these tools install into (commonly
`/usr/local/bin` or `~/.local/bin` depending on install method — verify with
the `which` command above once each CLI is installed for the `ao` user).

## 7. Authentication bootstrap (critical — human step, one time)

None of these are copied from macOS. Each CLI keeps its own credential store
under `$HOME`, so this is a one-time interactive step performed **as the
`ao` user, on the server, over SSH**, after `codex`/`claude`/`gh` are
installed and before starting the service for real:

```bash
sudo -u ao -H bash -l
codex login          # opens Codex's own device/browser login flow
claude auth login    # or export ANTHROPIC_API_KEY in ao.env-adjacent secret, see below
gh auth login        # choose SSH or HTTPS + device flow, for the account that owns clone/PR ops
exit
```

- **Codex CLI**: authenticates via its own login flow; AO only reads local
  auth status (`ao doctor`, Settings → Environment), never modifies or
  stores credentials itself.
- **Claude Code CLI**: either `claude auth login` (stores under `~/.claude`)
  or an `ANTHROPIC_API_KEY` environment variable. If using the API key
  route for staging, put it in a **separate, tighter-permission file**
  (e.g. `/etc/agent-orchestrator/ao-secrets.env`, mode `600`, owner
  `ao:ao`, referenced by a second `EnvironmentFile=` line in the unit) —
  do not add it to `ao.env` if that file is ever handed to anyone with
  read access to `root:ao` group membership beyond the `ao` account itself.
- **`gh`**: `gh auth login`, run once as `ao`.

Explicitly not done here: no Keychain copy, no blind credential-file copy
from a Mac, no token printed to a log or terminal capture, no insecure
workaround. If this bootstrap needs to run non-interactively (e.g. CI), that
is out of scope for this checkpoint — document it as a follow-up rather than
inventing a workaround now.

## 8. tmux under systemd

AO's tmux adapter creates its own named server (`tmux -L ao-staging` via
`AO_TMUX_SOCKET`) rather than attaching to whatever tmux server an SSH
session happens to have open. Because the systemd unit runs `ao server` as
a long-lived `Type=simple` service (not spawned from an interactive shell),
that tmux server is a child of the AO process group and is **not** tied to
any SSH session's lifetime — closing SSH does not close it. `KillMode=control-group`
in the unit ensures `systemctl stop` reaps every worker/reviewer tmux pane
the service spawned, so nothing orphaned is left running after a stop.

## 9. Web build / static serving

`ao server` serves the compiled frontend directly — no Vite dev server in
staging:

```bash
npm --prefix frontend install
npm --prefix frontend run build:web
cd backend && go build -o ../dist/ao ./cmd/ao
```

The systemd unit passes `--web-root /opt/agent-orchestrator/web` explicitly
(auto-detection via a `web/` directory beside the binary also works, per
`resolveWebRoot` in `backend/internal/cli/server.go`, but the unit is
explicit for anyone reading it in isolation).

## 10. Reverse proxy

[`deploy/caddy/Caddyfile.example`](../../deploy/caddy/Caddyfile.example).
**Caddy chosen over nginx**: automatic ACME TLS issuance/renewal built in
(no separate certbot timer), and HTTP Basic Auth is a one-line
`basicauth` directive — reaches the same staging security bar with less
moving config than nginx + certbot + an `htpasswd` cron job. Either is a
legitimate choice for this scope; this is not a strong architectural
commitment, just the lower-maintenance option for a staging box.

The proxy terminates TLS and Basic Auth, then forwards to
`127.0.0.1:3001` — AO's loopback listener is never bound to anything else.

## 11. HTTPS

Real ACME TLS requires a DNS name pointed at the server's IP; Caddy handles
issuance automatically once `staging.example.com` in the Caddyfile is a real
domain resolving to the box. **If no domain exists yet, stop and get one
DNS record before proceeding**: an `A`/`AAAA` record for the staging
hostname pointed at the Hetzner instance's public IP. A self-signed
certificate is explicitly rejected as a final answer here (it breaks mobile
browser trust and teaches operators to click through cert warnings) — if a
domain genuinely cannot be obtained yet, the documented fallback is to keep
the box SSH-only (port 22 + a local SSH tunnel: `ssh -L 3001:127.0.0.1:3001 ao@host`)
until one exists, rather than serving plaintext HTTP or a self-signed cert
over the public Internet.

## 12. Staging web auth

Reverse-proxy HTTP Basic Auth (Caddy's `basicauth`, bcrypt-hashed password,
generated with `caddy hash-password`, never plaintext in any file). This is
the smallest secure option that doesn't require building AO's own
multi-user system: one shared operator credential, TLS-protected transport
(Basic Auth over plain HTTP is not acceptable — it always sits behind
section 11's TLS termination), rotated by re-running
`caddy hash-password` and reloading Caddy. AO's own LAN listener/mobile
auth path is intentionally not reused here (see the guardrail note at the
top of this document).

## 13. Firewall

Recommended `ufw` rules (adjust for the actual SSH port if changed from 22):

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow OpenSSH        # or: sudo ufw allow 22/tcp
sudo ufw allow 80/tcp         # ACME HTTP-01 challenge + redirect to https
sudo ufw allow 443/tcp        # HTTPS
sudo ufw enable
```

AO's own port (`3001`) is **never** opened in the firewall — it is reached
only via the reverse proxy on the loopback interface. Verify after enabling:

```bash
sudo ufw status verbose
```

## 14. SSH

Deployment assumes key-based SSH access to the box (Hetzner's standard
provisioning flow supports injecting a public key at server creation).
Password authentication should already be off by the time this checkpoint's
scripts run; this document does not modify `sshd_config` — that is either
already Hetzner's default for the chosen image or a separate, explicit,
human-approved step.

## 15. Repository access

Staging targets test/fixture repositories only. `gh auth login` (section 7)
grants clone/fetch (and PR operations if the workflow needs them against a
disposable test repo) for the account used to authenticate — it is a human
decision which account and which repos that covers, not something this
checkpoint automates. AO's own integration refs (the internal
verification/merge-tracking refs workflows create) remain local to the
server's git objects, same as desktop — nothing here changes that.

## 16. Data durability

Everything that must survive a reboot lives under `/var/lib/agent-orchestrator/`
(`AO_DATA_DIR` for SQLite/workflow/wake state, `AO_PROJECT_ROOTS` for
repos, plus worktrees) — a directory outside `/opt` (replaced on update) and
outside `/tmp` (systemd/OS may clear it). `Restart=on-failure` plus
`WantedBy=multi-user.target` means both a crash and a full reboot bring the
service back with the same data root untouched.

## 17. Backup plan

What to back up (not a backup *system*, just the scope):

- `/var/lib/agent-orchestrator/data/` — SQLite database and `running.json`
- `/var/lib/agent-orchestrator/repos/` metadata is recoverable by re-cloning,
  but back it up anyway if a fixture repo has staging-only local changes
- `/etc/agent-orchestrator/ao.env` (and any secrets file from section 7) —
  back up **separately** from the data dir, since it's config, not data,
  and typically has a different retention/access policy
- **Not** tmux panes — they are runtime state, not a source of truth;
  AO's durable facts live in SQLite, and tmux is reconstructed from
  workflow state on demand

A simple starting point (cron or a systemd timer, not built here — staging
scope stops at documenting what to back up):

```bash
tar czf "ao-data-$(date +%F).tar.gz" -C /var/lib/agent-orchestrator data
```

## 18. Logging

AO's own stdout/stderr goes to journald (systemd default capture for
`Type=simple`):

```bash
systemctl status agent-orchestrator
journalctl -u agent-orchestrator -f          # follow
journalctl -u agent-orchestrator --since "1 hour ago"
```

The reverse proxy's access log is a plain file
(`/var/log/agent-orchestrator/caddy-access.log`, JSON lines) since access
logs are usually rotated/shipped differently than application logs; add
`logrotate` for it if retention becomes a concern (not set up in this
checkpoint). Neither AO nor the Caddyfile writes secrets to logs — no
bearer tokens, no `gh auth token` output, no Basic Auth password (Caddy logs
the request path, not the Authorization header value).

## 19. Health / readiness

`GET /healthz` and `GET /readyz` (existing, from checkpoint 8G) are the
integration points:

```bash
curl -s http://127.0.0.1:3001/readyz
```

`update-linux.sh` polls this after every update and auto-rolls-back if it
doesn't come up within 20 seconds. A reverse proxy or external monitor can
poll the same endpoint through HTTPS once deployed (`GET
https://staging.example.com/readyz` — note this is behind Basic Auth same
as everything else in this checkpoint's scope, so a monitor needs the
credential too, or should probe `127.0.0.1:3001` directly if it runs on-box).

## 20. Update / rollback

[`deploy/scripts/update-linux.sh`](../../deploy/scripts/update-linux.sh):
backs up the current binary + web assets to
`/var/backups/agent-orchestrator/<timestamp>/`, stops the service, swaps in
new artifacts, restarts, and polls `/readyz`. On failure it **automatically
rolls back** to the pre-update backup and restarts. Manual rollback if ever
needed:

```bash
sudo systemctl stop agent-orchestrator
sudo cp /var/backups/agent-orchestrator/<timestamp>/ao /opt/agent-orchestrator/bin/ao
sudo rm -rf /opt/agent-orchestrator/web
sudo cp -r /var/backups/agent-orchestrator/<timestamp>/web /opt/agent-orchestrator/web
sudo systemctl start agent-orchestrator
```

No Electron auto-updater involved — that mechanism is desktop-only and does
not apply to `ao server`.

## 21. Docker decision

**Not used.** The portability audit (section 1) found no blocking reason to
containerize: `codex`/`claude`/`gh`/`tmux` all resolve cleanly as native
Linux processes via `$PATH`, and `binaryutil` already handles Linux-vs-macOS
candidate paths without a container's help. Containerizing would instead
*add* friction here — codex/claude/gh authentication state lives under
`$HOME` and would need a volume mount plus careful UID mapping to avoid
re-doing the auth bootstrap (section 7) inside the container, and tmux's
socket-based IPC (section 8) is simpler as a same-host process than across
a container boundary. Native systemd is the right fit for this checkpoint;
revisit only if a concrete multi-tenant or reproducible-build requirement
emerges later.

## 22. What was not done (explicitly, per this checkpoint's scope)

- No SSH connection was made to any real server.
- No DNS record was created.
- No TLS certificate was requested or issued.
- No firewall was modified.
- No login to Codex, Claude, or GitHub was performed.
- No deployment to Hetzner occurred.
- Nothing was committed or pushed (per instruction).

All of the above are represented here as exact commands / config for a human
to run later, not as claims that they already happened.

## 23. Fresh Hetzner host: exact command sequence

Assumes a fresh Ubuntu/Debian Hetzner VM, SSH key access as a sudo-capable
user, and this repo checked out on the box (or artifacts copied over).

```bash
# 1. Base packages
sudo apt-get update
sudo apt-get install -y git tmux curl ca-certificates gnupg

# 2. Install Codex CLI, Claude Code CLI, gh — follow each vendor's official
#    Linux install instructions (not reproduced here; do not assume a
#    package manager path this document didn't verify). Confirm:
which codex claude gh tmux git

# 3. Install Caddy (Debian/Ubuntu, official repo)
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
  | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
  | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy

# 4. Build AO (needs Go + Node on the build host — can be this box or CI)
git clone <this-repo-url> /tmp/ao-src && cd /tmp/ao-src
npm --prefix frontend install
npm --prefix frontend run build:web
cd backend && go build -o ../dist/ao ./cmd/ao && cd ..

# 5. Install AO as a service
sudo BIN=./dist/ao WEB=./frontend/dist/web ./deploy/scripts/install-linux.sh

# 6. Auth bootstrap (interactive, one time)
sudo -u ao -H bash -l
codex login && claude auth login && gh auth login
exit

# 7. Start AO and verify locally
sudo systemctl start agent-orchestrator
curl -s http://127.0.0.1:3001/readyz

# 8. Configure and start the reverse proxy
sudo cp deploy/caddy/Caddyfile.example /etc/caddy/Caddyfile
# edit /etc/caddy/Caddyfile: real hostname + `caddy hash-password` output
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy

# 9. Firewall
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable

# 10. Verify from outside the box
curl -su 'ao_operator:<password>' https://staging.example.com/readyz

# 11. Confirm reboot survival
sudo reboot
# after reboot:
systemctl status agent-orchestrator caddy
```

Later updates:

```bash
cd /tmp/ao-src && git pull
npm --prefix frontend run build:web
cd backend && go build -o ../dist/ao ./cmd/ao && cd ..
sudo BIN=./dist/ao WEB=./frontend/dist/web ./deploy/scripts/update-linux.sh
```

---

**CHECKPOINT 8O HETZNER STAGING FOUNDATION: PASS** (docs/code/config scope
only — see section 22 for what remains a human/live-server step).
