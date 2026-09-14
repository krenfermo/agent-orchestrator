#!/bin/sh
# P9 deterministic worker fixture. No network, no LLM, no secrets.
#
#   p9worker.sh <dir> <mode>
#
# It speaks to the test only through files in <dir>, each written atomically
# (write to .tmp, then mv), so a reader never sees a partial file:
#
#   ready      this process's pid, once it is running
#   heartbeat  a counter that advances while it stays alive
#   done       "ok" / "fail" / "stopped" when it ends
#
# modes:
#   stay   run until <dir>/stop appears
#   exit0  end at once, successfully
#   exit1  end at once, failing
#   delay  stay alive until <dir>/release appears, then finish "ok"
dir="$1"
mode="$2"
put() { printf '%s\n' "$2" > "$dir/$1.tmp" && mv "$dir/$1.tmp" "$dir/$1"; }
put ready "$$"
case "$mode" in
  exit0) put done ok; exit 0 ;;
  exit1) put done fail; exit 1 ;;
esac
n=0
while [ ! -f "$dir/stop" ]; do
  n=$((n + 1))
  put heartbeat "$n"
  if [ "$mode" = delay ] && [ -f "$dir/release" ]; then
    put done ok
    exit 0
  fi
  sleep 0.05
done
put done stopped
