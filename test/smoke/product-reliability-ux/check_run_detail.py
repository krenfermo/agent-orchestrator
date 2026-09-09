#!/usr/bin/env python3
"""Checks one workflow run detail against what the smoke asked for.

Reads the run detail JSON on stdin so nothing is interpolated into a program
text: a response containing a quote or a backslash must not be able to change
what this checks.

Exit 0 when every assertion passes, 1 otherwise, so the caller can branch.
"""

import json
import sys


def main() -> int:
    detail = json.load(sys.stdin)["workflow"]
    run = detail["run"]
    advice = detail.get("advice") or {}
    failed = False

    def line(label: str, value: object, verdict: str = "") -> None:
        print(f"  {label:<17}: {value}{'   ' + verdict if verdict else ''}")

    line("state", run.get("state"))
    line("phase", run.get("phase"))

    strategy = (run.get("executionStrategy") or {}).get("effectiveStrategy")
    ok = strategy == "task"
    failed |= not ok
    line("strategy", strategy, "PASS" if ok else "FAIL (expected task)")

    # The advice block is the surface this integration added; an empty category
    # means the daemon sent nothing for the panel to render.
    category = advice.get("category")
    failed |= not category
    line("advice.category", category or "MISSING", "PASS" if category else "FAIL")
    line("requiresHuman", advice.get("requiresHuman"))

    for blocked in advice.get("blockedActions") or []:
        line("blocked", f"{blocked['action']} - {blocked['reason']}")

    line("steps", [(s["ordinal"], s["kind"], s["state"]) for s in detail.get("steps", [])])

    # The structured checks must be BOUND INTO the run, not merely accepted by
    # the form. This is the half of "checks were sent" that only the daemon can
    # confirm, and it is the assertion the wf-aee38f69 incident was about.
    plan = detail.get("plan") or {}
    tasks = detail.get("tasks") or []
    blob = json.dumps(plan) + json.dumps(tasks)
    bound = "check.sh" in blob
    failed |= not bound
    line("verify bound", bound, "PASS" if bound else "FAIL (dump below)")
    if not bound:
        print("  --- plan/tasks dump, to see where the checks landed ---")
        print(json.dumps({"plan": plan, "tasks": tasks}, indent=2)[:4000])

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
