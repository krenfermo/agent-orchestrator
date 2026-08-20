#!/usr/bin/env bash
# Normal local-dev startup for AO against permanent (non-/tmp) state.
#
# Usage: scripts/ao-local.sh
#
# Sets the permanent AO_DATA_DIR/AO_RUN_FILE/AO_PROJECT_ROOTS (Checkpoint
# 8P-E.5) and runs the same web-mode daemon invocation used during manual
# testing, so Claude Code auth / the isolated per-user keychain under
# AO_DATA_DIR/users/<id>/runtime-home keep working unchanged (they're always
# recomputed from AO_DATA_DIR at Prepare() time, never hardcoded).
#
# Checkpoint 8P-E.8.1: this used to default to ~/.agent-orchestrator/data,
# which collided with legacyimport's reserved read-only legacy-tool root
# (see backend/internal/legacyimport/paths.go) and violated CLAUDE.md's hard
# rule that all AO state lives under ~/.ao. Defaults now match that rule --
# i.e. match what Electron and the bare `ao` binary already resolve to when
# AO_DATA_DIR/AO_RUN_FILE aren't set (backend/internal/config/config.go's
# resolveDataDir/resolveRunFilePath) -- so this script, Electron, and a
# no-flags `ao start` all land on the same on-disk state by default.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
backend_dir="${repo_root}/backend"

export AO_DATA_DIR="${AO_DATA_DIR:-${HOME}/.ao/data}"
export AO_RUN_FILE="${AO_RUN_FILE:-${HOME}/.ao/running.json}"
export AO_PROJECT_ROOTS="${AO_PROJECT_ROOTS:-/Users/joaquinmora/Downloads/proyectos_resp}"

if [[ -f "${AO_RUN_FILE}" ]]; then
  running_pid="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('pid',''))" "${AO_RUN_FILE}" 2>/dev/null || true)"
  if [[ -n "${running_pid}" ]] && kill -0 "${running_pid}" 2>/dev/null; then
    printf 'AO is already running (pid %s) against AO_RUN_FILE=%s\n' "${running_pid}" "${AO_RUN_FILE}" >&2
    printf 'Not starting a second instance. Stop it first if you want to restart.\n' >&2
    exit 1
  fi
  printf 'Found stale %s (process not running); continuing.\n' "${AO_RUN_FILE}" >&2
fi

mkdir -p "${AO_DATA_DIR}" "$(dirname "${AO_RUN_FILE}")"

printf 'Starting AO\n  AO_DATA_DIR=%s\n  AO_RUN_FILE=%s\n  AO_PROJECT_ROOTS=%s\n' \
  "${AO_DATA_DIR}" "${AO_RUN_FILE}" "${AO_PROJECT_ROOTS}"

cd "${backend_dir}"
exec go run ./cmd/ao server --host 127.0.0.1 --port 3001 --web-root ../frontend/dist/web
