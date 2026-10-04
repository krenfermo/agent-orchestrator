# Contract: a stable code for an unprovable change set

**Status:** specified, not implemented. **Owner of the files it touches:** the lifecycle
front (`backend/internal/workflow/`). Do not implement this from a UI branch.

## Why

When AO cannot establish what a task changed, it records a sentence explaining why, and
the renderer now shows it (`WorkflowChangeSet`). That sentence is composed in Go, in
English, with no accompanying code — so it is the one string on that panel that stays
English in every locale.

Every other stop on these surfaces already works the other way round: the daemon emits a
stable code from a closed vocabulary, the renderer owns the copy per locale, and the
daemon's English is only a fallback for a code the renderer has not learned yet. That is
exactly what `workflow.Advice` documents about `Summary`/`Explanation`, and it is why the
advice panel is now fully localized. The change-set reason is the outlier, not the rule.

## The vocabulary

`taskChangedFiles` in `backend/internal/workflow/review_changed_files.go` sets
`taskChangeSet.Unprovable` at seven points. Each is already a distinct cause, so the
vocabulary is a rename of what is there, not a new classification:

| Code | Cause | Detail to carry |
| --- | --- | --- |
| `no_base_commit` | the work step recorded no base commit | — |
| `head_unreadable` | AO could not read the worktree's HEAD | — |
| `base_check_failed` | the base commit could not be checked | the Git error |
| `base_missing` | the base commit is not present in this checkout (history rewritten or replaced) | short base SHA |
| `ancestry_check_failed` | ancestry could not be checked | the Git error |
| `base_not_ancestor` | the base is no longer an ancestor of HEAD (history rewritten) | short base + head SHA |
| `diff_failed` | the diff against the base failed | the Git error |

Three carry a Git error string. Those stay **untranslated** and are shown beside the
localized sentence, exactly as technical identifiers are treated everywhere else on these
panels — a Git error is evidence, not copy.

## The change

1. `review_changed_files.go` — add `UnprovableCode string` to `taskChangeSet`, set it
   alongside every existing `Unprovable` assignment. `Unprovable` **stays**: it remains
   the fallback, and removing it would break any client with no copy yet.
2. `review_policy.go` — carry it on `ReviewRiskFacts` as
   `UnprovableChangeSetCode string` / `json:"unprovableChangeSetCode,omitempty"`.
3. `backend/internal/httpd/controllers/dto.go` — mirror the field on the risk-facts DTO.
4. `backend/internal/httpd/apispec/specgen/build.go` — no new named type, so no
   `schemaNames` entry; the field rides the existing schema.
5. `npm run api` — regenerates `openapi.yaml` and `frontend/src/api/schema.ts`. Commit
   both with the Go change. **Note:** `npm run api` can exit 0 while leaving `schema.ts`
   stale; confirm both generated files actually moved.

Purely additive: no migration, no persisted contract, no lifecycle behaviour change. A run
created before this simply has no code and renders the existing sentence.

## The renderer side, once the field exists

`WorkflowChangeSet` already has the fallback shape this needs. One change:

```tsx
const localized = facts.unprovableChangeSetCode
  ? translateDynamic(t, `wf.changeSet.unprovableReason.${facts.unprovableChangeSetCode}`, "")
  : "";
// AO's English sentence only when the renderer has no copy for the code.
{localized || facts.unprovableChangeSetReason}
```

plus seven keys per locale under `wf.changeSet.unprovableReason.*`, and the Git error
rendered as its own `<dd>` in the technical font, unlocalized.

## Acceptance

- A run stopped on each of the seven causes renders a Spanish sentence with Spanish
  selected.
- A code with no copy still renders AO's English sentence rather than blank.
- The Git error text is never translated and never dropped.
