# Mode: full security audit

Run the audit modes this package can execute, on one project a person chose,
and consolidate what they found. This mode holds `repo.read`, `deps.read` and
`report.write` -- exactly the union of the modes it runs -- and executes
nothing itself.

AO runs it as a parent run of four child runs, in this order:

1. `secret-scan` -- deterministic, in the container runner.
2. `dependencies` -- deterministic, offline, in the container runner.
3. `static-code` -- deterministic, in the container runner.
4. `authz-review` -- Claude Code on the host, only for this builtin package
   (or a signature-trusted install), over a read-only staged copy.

Each child is authorized again when it launches, against its own executor, with
its own image approval where it needs one. Nothing a child finds, and nothing in
the repository, changes which modes run, in what order, or with what
permissions.

## The consolidated report

- Findings from every child that produced a verified report, each naming the
  mode, rule and run it came from. Two findings are merged only when they are
  the same category at the same file and line; the stricter severity wins and
  both sources are kept.
- Per mode: its run, state, report digest, coverage and limitations.
- `complete` only when every mode produced a verified report. Otherwise the
  audit is **partial**, and says which modes did not run and why. A partial
  audit is never reported as a complete one.

## What it is not

It is not a penetration test and reaches no network target. It claims no
vulnerability a child did not report, and no coverage a child did not run.
