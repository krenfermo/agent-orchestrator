# Code graph: provider boundary and native indexer

`backend/internal/codegraph` gives AO a code graph — files, symbols, imports,
and call edges — behind an adapter interface, so the graph can come from AO's
own indexer today and from an external tool (Graphify, an LSP-backed indexer,
a hosted service) later without changing any caller.

## The boundary

`CodeGraphProvider` is the whole contract:

```go
type CodeGraphProvider interface {
	Name() string
	Index(ctx context.Context, req IndexRequest) (IndexResult, error)
	IncrementalUpdate(ctx context.Context, req UpdateRequest) (IndexResult, error)
	Query(ctx context.Context, req QueryRequest) (QueryResult, error)
}
```

Rules an adapter must honor (the full contract is documented on the interface
in `provider.go`):

- Every method keys its state by `ProjectRoot`. Indexing project A must never
  observe, mutate, or return project B's entries. An empty, relative, or
  non-directory root returns `ErrProjectRoot`.
- `Index` is a full pass and is idempotent over an unchanged tree.
- `IncrementalUpdate` applies a `Diff` — added, modified, deleted, renamed
  paths — and touches nothing the diff does not name. With no persisted graph
  an adapter either falls back to a full index (setting `IndexResult.FullIndex`)
  or returns `ErrNotIndexed`.
- `Query` is read-only. No symbol and no file returns `ErrEmptyQuery`; an
  unindexed project returns `ErrNotIndexed`; no match returns an empty result
  and a nil error.
- Any persisted state lives under AO's data dir, never an OS-default
  application-data location.

`Diff` carries paths only, not hunks. `ParseGitNameStatus` builds one from
`git diff --name-status -M <base>..<head>`; renames and copies are normalized
onto the four statuses the graph understands.

## The native indexer

`NativeIndexer` is the in-tree implementation. It has no external process, no
daemon, and no network dependency:

- **Go** is parsed with `go/parser`, so functions, methods (recorded as
  `Receiver.Method`), types, package-level constants and variables, imports,
  and call sites come from a real AST. A file with syntax errors keeps whatever
  the parser still produced.
- **TypeScript, JavaScript, and Python** go through a line-oriented declaration
  scanner that records symbols and import edges. It deliberately emits no call
  edges: resolving a callee needs real name resolution, and an edge guessed
  from a regex is worse than an absent one.
- Adding a language means adding an `Extractor` and registering it in
  `DefaultExtractors`; nothing in the indexer, the store, or the boundary
  changes.

Call edges keep the callee **as written** (`fmt.Println`, `helper`,
`rcv.Method`). The indexer builds no type information, and a truthful
unresolved name is more useful than a guess; callers resolve by name through
`Query`.

### Hashing is what makes updates cheap

Every file entry stores a content hash of the whole file, and every symbol
stores a hash of its own source text.

- `IncrementalUpdate` reads and hashes only the paths in the diff. A path whose
  hash still matches the persisted entry is counted in `FilesSkipped` and is
  never re-parsed — the hash, not the diff, decides what gets reprocessed, so a
  diff that over-reports costs a read, not a parse.
- A **rename with identical bytes** is re-keyed in place: the entry moves to
  the new path and its path-derived symbol IDs and edges are rewritten, with no
  parsing at all (`FilesRenamed`).
- A **rename that also changed** drops the old entry and parses the new path.
- A **delete** drops the entry; because a file's symbols and the edges
  originating in it are stored together, there is no second index left holding
  a stale reference.
- Per-symbol hashes let a caller tell an edited symbol from an untouched one
  inside a file that did change.

`IndexResult` is the audit trail: `FilesParsed` / `FilesSkipped` /
`FilesRemoved` / `FilesRenamed`, plus the sorted `ParsedFiles` and
`RemovedFiles`.

## Storage and multirepo isolation

Graphs live at `<data dir>/codegraph/projects/<project key>/graph.json`, where
the data dir is `AO_DATA_DIR` if set and `~/.ao/data` otherwise. This follows
the hard rule in `AGENTS.md`: all app state lives under `~/.ao` only.
`ValidateStoreDir` rejects a relative path or one inside
`Library/Application Support`, `AppData/Roaming`, or `AppData/Local` outright,
so a misconfigured data dir fails instead of quietly writing to an OS-default
location.

Isolation between projects is structural, not conventional:

- A project root must be absolute; a relative one is refused with
  `ErrProjectRoot` rather than resolved against the process working directory,
  which would key a project's index by an accident of the daemon's lifecycle.
- An absolute root is canonicalized (symlinks resolved, cleaned) so one
  checkout spelled two ways lands on one key.
- The project key is a readable directory-name prefix plus a hash of the
  **full** root path, so sibling checkouts sharing a base name (`orgA/api` and
  `orgB/api`) never collide.
- Each project gets its own file. There is no shared file for two projects to
  leak entries through.
- A graph records the root it was built for, and `Store.Load` refuses one whose
  recorded root does not match the caller's — a planted or stale file yields an
  empty graph and a re-index, never another project's symbols.
- Paths coming in from a diff are refused if they escape the project root —
  lexically (`../../etc/passwd`) or through a symlinked directory inside the
  checkout (`linked/secret.go`). A candidate's parent directory is
  symlink-resolved and proven to sit under the canonical root before anything
  is opened, and the final component is left unresolved so a symlinked file is
  declined as non-regular.

## Tests

`backend/internal/codegraph` covers the initial index, incremental update of a
single changed file (including a diff that over-reports), rename with and
without edits, deletion, cross-project isolation, storage-path resolution and
the app-data rejection, query behavior, and per-language extraction.

```bash
cd backend && go test ./internal/codegraph/...
```
