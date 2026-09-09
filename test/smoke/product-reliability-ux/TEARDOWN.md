# Teardown — deliberately manual

Nothing in this smoke deletes anything. Not on success, not on failure, not on
interrupt. That is a decision, not an omission:

- A failed smoke is only diagnosable from what it left behind. A cleanup step that
  fires on failure destroys precisely the state somebody needs to read.
- Automatic deletion of a *project* is one `--id` typo away from deleting a real one.
  MEDUSA, POSEIDÓN and CRM live in the same daemon.
- Registered worktrees may be dirty. AO itself refuses to force-delete a dirty
  registered worktree; a smoke script has no business being less careful.

## What a run leaves behind

| Artifact | Where |
| --- | --- |
| Fixture repo | `~/.ao/smoke/prux-<stamp>/` |
| AO project | id `ao-smoke-prux-<stamp>` |
| Workflow run | one `wf-…`, in the daemon's database |
| Worktree/branch for the run | wherever the run's placement put it — read it from the run's `placement` |
| Evidence | whatever you saved from `verify-run.sh` and the diagnostics paste |

## When you decide to clean up

Do it deliberately, one at a time, checking each id first.

```bash
# 1. Look at exactly what you are about to remove.
ao project get ao-smoke-prux-<stamp>

# 2. Cancel the run from the UI if it is still moving. Do not delete rows from
#    SQLite; use the app's own cancel/archive so the daemon stays consistent.

# 3. Remove the project. Read the id twice.
ao project rm ao-smoke-prux-<stamp>

# 4. Only then the fixture directory.
rm -rf ~/.ao/smoke/prux-<stamp>
```

Keep the evidence — the run id, the `verify-run.sh` output, the diagnostics paste —
somewhere outside `~/.ao/smoke/` before removing anything.

## Never

- `git checkout` anywhere in this tree to "reset" a fixture. There is uncommitted
  checkpoint work in sibling worktrees; a restore there is a delete.
- Deleting a project by pattern or in a loop.
- Touching `~/.ao/data` directly.
