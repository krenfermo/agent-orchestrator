#!/usr/bin/env bash
# Idempotent first-install / reinstall for AO on Ubuntu/Debian (Hetzner
# staging). Safe to re-run: every step checks current state before acting.
#
# What this does NOT do (by design, per checkpoint 8O scope):
#   - does not touch the firewall
#   - does not install/configure Caddy or nginx
#   - does not request or install a TLS certificate
#   - does not log into codex/claude/gh
#   - does not start the service unless AO_INSTALL_START=1
#
# Usage (run as root, from a checked-out repo with a build already produced
# by `npm --prefix frontend run build:web` and `cd backend && go build -o
# ../dist/ao ./cmd/ao`):
#
#   sudo BIN=./dist/ao WEB=./frontend/dist/web ./deploy/scripts/install-linux.sh
#
set -euo pipefail

BIN="${BIN:-}"
WEB="${WEB:-}"
APP_DIR=/opt/agent-orchestrator
DATA_DIR=/var/lib/agent-orchestrator
LOG_DIR=/var/log/agent-orchestrator
CONF_DIR=/etc/agent-orchestrator
SERVICE_NAME=agent-orchestrator
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [[ $EUID -ne 0 ]]; then
	echo "must run as root (sudo)" >&2
	exit 1
fi

if [[ -z "$BIN" || ! -x "$BIN" ]]; then
	echo "BIN must point at a built ao binary (BIN=./dist/ao). Build it first:" >&2
	echo "  cd backend && go build -o ../dist/ao ./cmd/ao" >&2
	exit 1
fi
if [[ -z "$WEB" || ! -f "$WEB/index.html" ]]; then
	echo "WEB must point at compiled frontend assets (WEB=./frontend/dist/web). Build it first:" >&2
	echo "  npm --prefix frontend install && npm --prefix frontend run build:web" >&2
	exit 1
fi

FIRST_INSTALL=1
if id -u ao >/dev/null 2>&1; then
	FIRST_INSTALL=0
fi

echo "==> service user"
if ! id -u ao >/dev/null 2>&1; then
	useradd --system --home-dir /home/ao --create-home --shell /usr/sbin/nologin ao
	echo "created system user 'ao'"
else
	echo "user 'ao' already exists, skipping"
fi

echo "==> directories"
install -d -o root -g root -m 755 "$APP_DIR" "$APP_DIR/bin" "$APP_DIR/web"
install -d -o ao -g ao -m 750 "$DATA_DIR" "$DATA_DIR/data" "$DATA_DIR/repos" "$DATA_DIR/worktrees"
install -d -o ao -g ao -m 750 "$LOG_DIR"
install -d -o root -g ao -m 750 "$CONF_DIR"

echo "==> binary + web assets"
cp "$BIN" "$APP_DIR/bin/ao"
chown root:root "$APP_DIR/bin/ao"
chmod 755 "$APP_DIR/bin/ao"
rm -rf "$APP_DIR/web.new"
cp -r "$WEB" "$APP_DIR/web.new"
rm -rf "$APP_DIR/web"
mv "$APP_DIR/web.new" "$APP_DIR/web"
chown -R root:root "$APP_DIR/web"

echo "==> environment file"
if [[ -f "$CONF_DIR/ao.env" ]]; then
	echo "$CONF_DIR/ao.env already exists, leaving it untouched"
else
	cp "$REPO_ROOT/deploy/env/ao.env.example" "$CONF_DIR/ao.env"
	chown root:ao "$CONF_DIR/ao.env"
	chmod 640 "$CONF_DIR/ao.env"
	echo "wrote $CONF_DIR/ao.env from template — review PATH/HOME before starting"
fi

echo "==> systemd unit"
cp "$REPO_ROOT/deploy/systemd/agent-orchestrator.service" /etc/systemd/system/agent-orchestrator.service
systemctl daemon-reload
systemctl enable "$SERVICE_NAME"

echo "==> ownership recheck"
chown -R ao:ao "$DATA_DIR"

if [[ "${AO_INSTALL_START:-0}" == "1" ]]; then
	systemctl restart "$SERVICE_NAME"
	systemctl status --no-pager "$SERVICE_NAME"
else
	echo "==> not starting the service (set AO_INSTALL_START=1 to auto-start)"
fi

if [[ "$FIRST_INSTALL" == "1" ]]; then
	cat <<EOF

First install complete. Before starting AO for real:
  1. Review $CONF_DIR/ao.env (PATH must include codex/claude/gh/tmux/git).
  2. Bootstrap auth as the 'ao' user (see docs/deployment/hetzner-staging.md
     section 8): sudo -u ao -H bash -lc 'codex login && claude auth login && gh auth login'
  3. Start the service: sudo systemctl start $SERVICE_NAME
  4. Check health: curl -s http://127.0.0.1:3001/readyz
  5. Put a reverse proxy in front (deploy/caddy/Caddyfile.example) before
     exposing this beyond localhost.
EOF
else
	echo "Reinstall complete. Restart when ready: sudo systemctl restart $SERVICE_NAME"
fi
