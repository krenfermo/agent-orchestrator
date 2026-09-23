#!/bin/sh
# agent-shim.sh stands in for the `claude` and `codex` CLIs in the workflow-cycle
# E2E. It is installed under both names in a private bin directory that is the
# ONLY agent source on the scratch daemon's PATH, so no real agent can run.
#
# It plays the part of an agent, not of AO: it never talks to AO except through
# the same surfaces a real agent uses -- the native hook command
# (`ao hooks <harness> <event>`), the review verdict CLI (`ao review submit`) and
# the workspace itself. Every invocation that is a session appends a trace file
# under $AO_E2E_TRACE_DIR, which is how the test proves what ran, where, and how
# many times.
set -u

name=$(basename "$0")
trace_dir=${AO_E2E_TRACE_DIR:-}

# ---- probes: answered like the real CLIs, never traced as sessions ----------
case "$name:${1:-}:${2:-}" in
codex:login:status)
	echo "Logged in using an API key - ao-e2e"
	exit 0
	;;
claude:auth:status)
	echo '{"loggedIn":true,"authMethod":"ao-e2e"}'
	exit 0
	;;
esac
case "${1:-}" in
--version | -v | version)
	echo "$name 0.0.0-ao-e2e"
	exit 0
	;;
esac

if [ -z "${AO_SESSION_ID:-}" ] && [ -z "${AO_REVIEW_SESSION_ID:-}" ]; then
	# Not a session AO launched (a capability probe or similar): record it and
	# do nothing.
	[ -n "$trace_dir" ] && printf '%s %s\n' "$name" "$*" >>"$trace_dir/probes.log"
	exit 0
fi

hook_agent=claude-code
[ "$name" = codex ] && hook_agent=codex

role=worker
[ -n "${AO_REVIEW_SESSION_ID:-}" ] && role=reviewer

trace="$trace_dir/$role-$name-$$.trace"
{
	echo "name=$name"
	echo "argv0=$0"
	echo "role=$role"
	echo "pid=$$"
	echo "pwd=$(pwd)"
	echo "tmux=${TMUX:-}"
	echo "tmux_pane=${TMUX_PANE:-}"
	echo "tmux_session=$(tmux display-message -p '#{session_name}' 2>/dev/null)"
	echo "ao_session_id=${AO_SESSION_ID:-}"
	echo "ao_review_session_id=${AO_REVIEW_SESSION_ID:-}"
	echo "ao_review_worker_session_id=${AO_REVIEW_WORKER_SESSION_ID:-}"
	echo "ao_runtime_launch_id=${AO_RUNTIME_LAUNCH_ID:-}"
	echo "ao_bin=$(command -v ao)"
} >"$trace"

if [ "$role" = reviewer ]; then
	# The review prompt names the exact submit command, run id included. A
	# prompt can arrive inline in argv or as a file argv points at; read both.
	run_id=""
	for arg in "$@"; do
		for src in "$arg" "${arg#@}"; do
			if [ -f "$src" ]; then
				found=$(grep -o -- '--run [A-Za-z0-9_-]*' "$src" 2>/dev/null | head -n1 | cut -d' ' -f2)
			else
				found=$(printf '%s' "$src" | grep -o -- '--run [A-Za-z0-9_-]*' | head -n1 | cut -d' ' -f2)
			fi
			[ -n "$found" ] && [ -z "$run_id" ] && run_id=$found
		done
	done
	echo "review_run_id=$run_id" >>"$trace"
	ao hooks "$hook_agent" session-start </dev/null >/dev/null 2>&1 || true
	if ao review submit "$AO_REVIEW_WORKER_SESSION_ID" --run "$run_id" --verdict approved >>"$trace.submit" 2>&1; then
		echo "review_submit=ok" >>"$trace"
	else
		echo "review_submit=failed" >>"$trace"
	fi
	ao hooks "$hook_agent" stop </dev/null >/dev/null 2>&1 || true
	exit 0
fi

# ---- worker: do the task the E2E plan asks for -------------------------------
ao hooks "$hook_agent" session-start </dev/null >/dev/null 2>&1 || true
ao hooks "$hook_agent" user-prompt-submit </dev/null >/dev/null 2>&1 || true
mkdir -p src
printf 'cycle-e2e-deliverable\n' >src/cycle.txt
printf 'package src\n\n// Cycle is written by the workflow-cycle E2E worker.\nconst Cycle = "e2e"\n' >src/cycle.go
echo "wrote=src/cycle.txt,src/cycle.go" >>"$trace"
ao hooks "$hook_agent" stop </dev/null >/dev/null 2>&1 || true
echo "turn=done" >>"$trace"

if [ "$name" = codex ]; then
	# A codex worker is supervised: its exit is the completion signal.
	exit 0
fi
# A claude worker is an interactive TUI that stays alive and idle after its
# turn. Model that: remain until AO tears the session down.
while :; do sleep 1; done
