#!/usr/bin/env bash
# verify-run.sh <project-id> — reads back what the UI actually created.
#
# READ-ONLY against the daemon: every request below is a GET. It creates
# nothing, cancels nothing and deletes nothing. Run it after the manual UI step
# in run.md.
#
# Usage:  ./verify-run.sh ao-smoke-prux-20260909-101500
#
# The port comes from the daemon's own run file, resolved the same way
# preflight.sh resolves it. No port is assumed.
#
# JSON is handed to python through the environment rather than interpolated into
# the program text: a response containing a quote or a backslash must not be
# able to change what the checker does.

set -uo pipefail

PROJECT_ID="${1:-}"
if [ -z "$PROJECT_ID" ]; then
	echo "usage: $0 <project-id>" >&2
	exit 2
fi

RUN_FILE=""
for c in "${AO_RUN_FILE:-}" "$HOME/.ao/dev/running.json" "$HOME/.ao/running.json"; do
	if [ -n "$c" ] && [ -f "$c" ]; then RUN_FILE="$c"; break; fi
done
if [ -z "$RUN_FILE" ]; then
	echo "no run file; is the daemon up? run preflight.sh" >&2
	exit 1
fi

PORT=$(RF="$RUN_FILE" python3 -c 'import json,os;print(json.load(open(os.environ["RF"])).get("port",""))')
if [ -z "$PORT" ]; then
	echo "run file has no port: $RUN_FILE" >&2
	exit 1
fi
BASE="http://127.0.0.1:$PORT/api/v1"
echo "run file: $RUN_FILE"
echo "base    : $BASE"
echo

echo "== A. A workflow_run exists for this project =="
if ! RUNS=$(curl -sf -m 5 "$BASE/workflows?projectId=$PROJECT_ID"); then
	echo "  request failed against $BASE" >&2
	exit 1
fi

RUN_ID=$(RUNS_JSON="$RUNS" PID="$PROJECT_ID" python3 -c '
import json, os
runs = (json.loads(os.environ["RUNS_JSON"]).get("workflows") or [])
mine = [r for r in runs if r.get("projectId") == os.environ["PID"]]
print(mine[0]["id"] if mine else "", end="")
')
COUNT=$(RUNS_JSON="$RUNS" PID="$PROJECT_ID" python3 -c '
import json, os
runs = (json.loads(os.environ["RUNS_JSON"]).get("workflows") or [])
print(len([r for r in runs if r.get("projectId") == os.environ["PID"]]), end="")
')

if [ -z "$RUN_ID" ]; then
	echo "  FAIL: no workflow_run for project $PROJECT_ID"
	echo "  (If the UI reported success, this is the real defect: the surface"
	echo "   created something other than a workflow run.)"
	exit 1
fi
echo "  PASS: workflow_run id = $RUN_ID"
if [ "$COUNT" != "1" ]; then
	echo "  WARN: $COUNT runs for this project; the smoke expects exactly one."
fi

echo
echo "== B. Strategy, checks and advice, as the daemon actually froze them =="
if ! DETAIL=$(curl -sf -m 5 "$BASE/workflows/$RUN_ID"); then
	echo "  request failed" >&2
	exit 1
fi

printf '%s' "$DETAIL" | python3 "$(dirname "$0")/check_run_detail.py"
DETAIL_RC=$?
if [ "$DETAIL_RC" -ne 0 ]; then
	echo
	echo "  One or more assertions FAILED above. Leave everything in place and report."
fi
echo
echo "== C. Evidence to keep =="
echo "  Nothing here is deleted. Save the run id and this output:"
echo "    project : $PROJECT_ID"
echo "    run     : $RUN_ID"
