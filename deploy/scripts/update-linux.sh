#!/usr/bin/env bash
# Update flow for an already-installed AO staging service: build (or accept
# prebuilt artifacts), stop, swap binary + web assets, restart, verify
# health, and print the exact rollback command if verification fails.
#
# Usage (run as root, artifacts built the same way as install-linux.sh):
#
#   sudo BIN=./dist/ao WEB=./frontend/dist/web ./deploy/scripts/update-linux.sh
#
set -euo pipefail

BIN="${BIN:-}"
WEB="${WEB:-}"
APP_DIR=/opt/agent-orchestrator
SERVICE_NAME=agent-orchestrator
BACKUP_DIR="/var/backups/agent-orchestrator/$(date +%Y%m%d-%H%M%S)"

if [[ $EUID -ne 0 ]]; then
	echo "must run as root (sudo)" >&2
	exit 1
fi
if [[ -z "$BIN" || ! -x "$BIN" ]]; then
	echo "BIN must point at a built ao binary" >&2
	exit 1
fi
if [[ -z "$WEB" || ! -f "$WEB/index.html" ]]; then
	echo "WEB must point at compiled frontend assets" >&2
	exit 1
fi
if [[ ! -f /etc/systemd/system/agent-orchestrator.service ]]; then
	echo "no existing install found; run install-linux.sh first" >&2
	exit 1
fi

echo "==> backing up current binary + web to $BACKUP_DIR"
mkdir -p "$BACKUP_DIR"
cp "$APP_DIR/bin/ao" "$BACKUP_DIR/ao"
cp -r "$APP_DIR/web" "$BACKUP_DIR/web"
echo "rollback command if needed:"
echo "  sudo systemctl stop $SERVICE_NAME && sudo cp $BACKUP_DIR/ao $APP_DIR/bin/ao && sudo rm -rf $APP_DIR/web && sudo cp -r $BACKUP_DIR/web $APP_DIR/web && sudo systemctl start $SERVICE_NAME"

echo "==> stopping $SERVICE_NAME"
systemctl stop "$SERVICE_NAME"

echo "==> installing new binary + web assets"
cp "$BIN" "$APP_DIR/bin/ao.new"
chmod 755 "$APP_DIR/bin/ao.new"
mv "$APP_DIR/bin/ao.new" "$APP_DIR/bin/ao"
rm -rf "$APP_DIR/web.new"
cp -r "$WEB" "$APP_DIR/web.new"
rm -rf "$APP_DIR/web"
mv "$APP_DIR/web.new" "$APP_DIR/web"
chown -R root:root "$APP_DIR/bin/ao" "$APP_DIR/web"

echo "==> starting $SERVICE_NAME"
systemctl start "$SERVICE_NAME"

echo "==> waiting for /readyz"
ok=0
for _ in $(seq 1 20); do
	if curl -fsS http://127.0.0.1:3001/readyz >/dev/null 2>&1; then
		ok=1
		break
	fi
	sleep 1
done

if [[ "$ok" != "1" ]]; then
	echo "FAILED: /readyz did not come up after update. Rolling back automatically." >&2
	systemctl stop "$SERVICE_NAME"
	cp "$BACKUP_DIR/ao" "$APP_DIR/bin/ao"
	rm -rf "$APP_DIR/web"
	cp -r "$BACKUP_DIR/web" "$APP_DIR/web"
	systemctl start "$SERVICE_NAME"
	echo "rolled back to pre-update binary/web from $BACKUP_DIR" >&2
	exit 1
fi

echo "update complete, service healthy. Backup retained at $BACKUP_DIR"
