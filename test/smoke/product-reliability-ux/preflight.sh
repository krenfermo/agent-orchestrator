#!/usr/bin/env bash
# preflight.sh — answers "is it safe and meaningful to run this smoke right now?"
#
# READ-ONLY. It starts nothing, stops nothing, writes nothing outside its own
# stdout. Run it before the smoke and again if anything about the environment
# changes.
#
# It refuses to assume paths. The run file is resolved the way the Electron
# supervisor resolves it (frontend/src/main.ts:runFilePath):
#
#   1. $AO_RUN_FILE, when the supervisor was launched with it set
#   2. dev  -> $HOME/.ao/dev/running.json
#   3. prod -> the platform default beside the daemon data dir
#
# and the OWNER and PORT are read out of that file rather than guessed, because
# the daemon records them there itself (frontend/src/main/daemon-owner.ts).

set -uo pipefail

EXPECTED_HEAD="${EXPECTED_HEAD:-54cba2cdd69258ccc844978a3bb49b51fa74d0b8}"
REPO="${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)}"

ok()   { printf '  \033[32mOK\033[0m    %s\n' "$1"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$1"; }
bad()  { printf '  \033[31mBLOCK\033[0m %s\n' "$1"; BLOCKED=1; }
BLOCKED=0

echo "== 1. Which run file is authoritative here =="
CANDIDATES=()
[ -n "${AO_RUN_FILE:-}" ] && CANDIDATES+=("$AO_RUN_FILE")
CANDIDATES+=("$HOME/.ao/dev/running.json" "$HOME/.ao/running.json")

RUN_FILE=""
for c in "${CANDIDATES[@]}"; do
	if [ -f "$c" ]; then
		echo "  present: $c"
		[ -z "$RUN_FILE" ] && RUN_FILE="$c"
	else
		echo "  absent : $c"
	fi
done

if [ -z "$RUN_FILE" ]; then
	bad "No run file exists. No AO daemon is registered, so there is nothing to smoke against."
	echo
	echo "  A run file appears only once the daemon is actually up. Ask the user to"
	echo "  start the desktop app themselves; do NOT start or restart it from here."
else
	ok "using $RUN_FILE"
fi

echo
echo "== 2. Owner, port and data dir, read from the daemon's own record =="
if [ -n "$RUN_FILE" ]; then
	python3 - "$RUN_FILE" <<'PY'
import json, sys
raw = json.load(open(sys.argv[1]))
# Print every field that identifies WHICH daemon this is. Unknown keys are shown
# too: the record is the daemon's, and hiding a field it chose to write would be
# exactly the kind of assumption this script exists to avoid.
for k in sorted(raw):
    if "secret" in k.lower() or "token" in k.lower() or "password" in k.lower():
        print(f"  {k}: <redacted>")
    else:
        print(f"  {k}: {raw[k]}")
owner = raw.get("owner", "")
print()
print(f"  owner interpretation: " + {
    "app": "app-owned — quitting the desktop app stops this daemon",
    "persistent": "keep-alive — survives the app quitting, stops only on `ao stop`",
}.get(owner, "headless `ao start` (owner unset) — survives the app quitting"))
PY
else
	echo "  (skipped: no run file)"
fi

echo
echo "== 3. Is the daemon actually answering =="
PORT=""
if [ -n "$RUN_FILE" ]; then
	PORT=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1])).get('port',''))" "$RUN_FILE" 2>/dev/null)
fi
if [ -n "$PORT" ] && curl -sf -m 3 "http://127.0.0.1:$PORT/api/v1/projects" >/dev/null 2>&1; then
	ok "daemon answering on 127.0.0.1:$PORT"
else
	bad "no daemon answering (port='${PORT:-unknown}'). The record may be stale."
fi

echo
echo "== 4. Does the running app carry the code under test =="
echo "  expected HEAD: $EXPECTED_HEAD"
echo "  repo HEAD:     $(git -C "$REPO" rev-parse HEAD 2>/dev/null || echo '?')"
if git -C "$REPO" merge-base --is-ancestor "$EXPECTED_HEAD" HEAD 2>/dev/null; then
	ok "the worktree contains the expected HEAD"
else
	bad "the worktree does NOT contain $EXPECTED_HEAD"
fi

# In dev the daemon is `go run ./cmd/ao daemon` from <worktree>/backend and the
# renderer is Vite-served from the same worktree, so both follow whatever was
# checked out WHEN THE APP WAS STARTED. A supervisor older than the merge is
# running older UI even though the files on disk are new.
echo
echo "  supervisors currently running (start time matters, not just presence):"
ps -eo pid,lstart,command 2>/dev/null | grep '[e]lectron-forge start' | while read -r line; do
	echo "    $line" | cut -c1-140
done
echo "  (nothing listed above = no desktop app running)"
echo
echo "  NOTE: this integration changed 0 files under backend/, so the daemon's own"
echo "  code is unaffected by it. What must be current is the RENDERER. Any app"
echo "  supervisor started before the merge is serving pre-merge UI."

echo
echo "== 5. Concurrency hazards =="
N=$(ps -eo command 2>/dev/null | grep -c '[e]lectron-forge start')
if [ "$N" -gt 1 ]; then
	warn "$N desktop supervisors are running. Two supervisors racing one data dir is"
	echo "         a known source of confusing state. Ask the user which to keep; do not"
	echo "         kill anything from here."
else
	ok "$N desktop supervisor(s)"
fi

echo
if [ "$BLOCKED" -eq 1 ]; then
	echo "RESULT: BLOCKED — do not run the smoke. Report the blocking lines above."
	exit 1
fi
echo "RESULT: preflight clean — the smoke may run once the user authorizes it."
