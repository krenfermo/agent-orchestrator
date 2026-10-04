package projectmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// storeDirName is the subtree of the AO data dir that holds durable project
// memory. It sits next to the baseline evidence the measurement side writes
// (project-memory/baseline) rather than inside it, because evidence is an
// input that may be pruned and memory is the durable output.
const storeDirName = "project-memory/items"

// projectFileName is the single file one project's memory is persisted to.
const projectFileName = "memory.json"

// ErrStorePath is the sentinel every rejected storage location wraps.
var ErrStorePath = errors.New("projectmemory: invalid store directory")

// DataDir resolves AO's durable data directory the same way the daemon does:
// an explicit AO_DATA_DIR wins, otherwise it is ~/.ao/data. It deliberately
// never falls back to an OS-default application-data location — see the hard
// rule in AGENTS.md.
func DataDir() (string, error) {
	if raw, ok := os.LookupEnv("AO_DATA_DIR"); ok && strings.TrimSpace(raw) != "" {
		abs, err := filepath.Abs(strings.TrimSpace(raw))
		if err != nil {
			return "", fmt.Errorf("resolve AO_DATA_DIR: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve AO home: %w", err)
	}
	return filepath.Join(home, ".ao", "data"), nil
}

// StoreRoot is where project memory is written: <data dir>/project-memory/items.
func StoreRoot() (string, error) {
	dataDir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, filepath.FromSlash(storeDirName)), nil
}

// forbiddenPathSegments are the OS-default application-data locations AO must
// never write to. A misconfigured AO_DATA_DIR pointing at one of them is a
// configuration error worth failing on, not a location to quietly accept.
var forbiddenPathSegments = []string{
	filepath.Join("Library", "Application Support"),
	filepath.Join("AppData", "Roaming"),
	filepath.Join("AppData", "Local"),
}

// ValidateStoreDir rejects a storage location AO is not allowed to use: a
// relative path (which would resolve against whatever the process happened to
// chdir into, typically a repository checkout) or a path inside an OS-default
// application-data directory.
func ValidateStoreDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("%w: directory is required", ErrStorePath)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q must be absolute so it cannot resolve inside a repository checkout", ErrStorePath, dir)
	}
	clean := filepath.Clean(dir)
	for _, segment := range forbiddenPathSegments {
		if strings.Contains(clean, string(filepath.Separator)+segment) {
			return fmt.Errorf("%w: %q is inside the OS application-data directory; AO state belongs under ~/.ao (or AO_DATA_DIR)", ErrStorePath, dir)
		}
	}
	return nil
}

// ProjectKey is the storage key for a project id: a readable prefix plus a
// hash of the full id. The hash — not the readable part — is what keeps two
// projects apart, so ids that sanitize to the same label (two checkouts named
// "api", a project id with punctuation) never collide.
func ProjectKey(project string) string {
	trimmed := strings.TrimSpace(project)
	sum := sha256.Sum256([]byte(trimmed))
	digest := hex.EncodeToString(sum[:8])
	label := sanitizePathSegment(filepath.Base(trimmed))
	if label == "" {
		return digest
	}
	if len(label) > 40 {
		label = label[:40]
	}
	return label + "-" + digest
}

// projectFile is one project's whole memory as it sits on disk. Items are
// stored as a sorted slice rather than a map so the file is stable across
// writes and reviewable in a diff.
type projectFile struct {
	Version   int          `json:"version"`
	Project   string       `json:"project"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Items     []MemoryItem `json:"items"`
}

// Store persists project memory, one file per project, under a validated root
// directory.
type Store struct {
	root string
	now  func() time.Time
}

// StoreOption customizes a Store.
type StoreOption func(*Store)

// WithClock pins the store's clock. Production uses the wall clock; the option
// exists so tests can prove exactly which writes move UpdatedAt.
func WithClock(now func() time.Time) StoreOption {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// NewStore validates dir and returns a store writing beneath it. The directory
// is created lazily on first write, so constructing a store for a project that
// records nothing leaves no empty tree behind.
func NewStore(dir string, opts ...StoreOption) (*Store, error) {
	if err := ValidateStoreDir(dir); err != nil {
		return nil, err
	}
	s := &Store{
		root: filepath.Clean(dir),
		now:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// NewDefaultStore returns a store at the standard location under AO's data
// dir.
func NewDefaultStore(opts ...StoreOption) (*Store, error) {
	dir, err := StoreRoot()
	if err != nil {
		return nil, err
	}
	return NewStore(dir, opts...)
}

// Root is where this store writes.
func (s *Store) Root() string { return s.root }

// PathFor returns the file a project's memory is persisted to. Each project
// gets its own directory keyed by its id, which is the whole of the
// multi-project isolation guarantee: there is no shared file for two projects
// to leak items through.
func (s *Store) PathFor(project string) string {
	return filepath.Join(s.root, "projects", ProjectKey(project), projectFileName)
}

// List returns a project's items, sorted by id. A project that has never been
// written yields an empty slice and a nil error: "nothing recorded yet" is an
// answer, not a failure.
func (s *Store) List(project string) ([]MemoryItem, error) {
	file, err := s.load(project)
	if err != nil {
		return nil, err
	}
	return file.Items, nil
}

// Get returns one item by id.
func (s *Store) Get(project, id string) (MemoryItem, bool, error) {
	items, err := s.List(project)
	if err != nil {
		return MemoryItem{}, false, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, true, nil
		}
	}
	return MemoryItem{}, false, nil
}

// UpsertResult reports what an upsert actually did. Unchanged is the number
// that matters most: it is the audit trail for "re-ingesting the same evidence
// is free", and a test that expects idempotence asserts on it rather than on
// the absence of a second row alone.
type UpsertResult struct {
	Created   int
	Updated   int
	Unchanged int
	// Items are the stored rows for the upserted items, in the order they were
	// supplied — including the untouched rows for the unchanged ones, so a
	// caller can read back the original CreatedAt/UpdatedAt without a
	// follow-up List.
	Items []MemoryItem
	// Paths are the project files written, one per project touched. An upsert
	// in which nothing changed writes nothing and returns none.
	Paths []string
}

// Upsert stores items, addressing each by its identity (see
// MemoryItem.Identity).
//
// The contract, in one sentence: an item whose content and provenance already
// match what is stored is left completely alone — same row, same CreatedAt,
// same UpdatedAt — while a genuine change updates the existing row in place
// and keeps its CreatedAt. Nothing here ever appends a second row for a fact
// the store already holds.
//
// Items for several projects may be mixed in one call; they are grouped and
// each project's file is written at most once.
func (s *Store) Upsert(ctx context.Context, items ...MemoryItem) (UpsertResult, error) {
	result := UpsertResult{Items: make([]MemoryItem, 0, len(items))}
	if len(items) == 0 {
		return result, nil
	}

	normalized := make([]MemoryItem, 0, len(items))
	order := make([]string, 0, len(items))
	byProject := map[string][]int{}
	for idx, item := range items {
		item = item.normalized()
		if err := item.Validate(); err != nil {
			return UpsertResult{}, err
		}
		normalized = append(normalized, item)
		if _, seen := byProject[item.Project]; !seen {
			order = append(order, item.Project)
		}
		byProject[item.Project] = append(byProject[item.Project], idx)
	}

	stored := make([]MemoryItem, len(normalized))
	now := s.now().UTC()
	for _, project := range order {
		if err := ctx.Err(); err != nil {
			return UpsertResult{}, err
		}
		file, err := s.load(project)
		if err != nil {
			return UpsertResult{}, err
		}
		index := map[string]int{}
		for i, existing := range file.Items {
			index[existing.ID] = i
		}
		dirty := false
		for _, idx := range byProject[project] {
			incoming := normalized[idx]
			at, found := index[incoming.ID]
			if !found {
				incoming.CreatedAt = timestampOr(incoming.CreatedAt, now)
				incoming.UpdatedAt = incoming.CreatedAt
				file.Items = append(file.Items, incoming)
				index[incoming.ID] = len(file.Items) - 1
				stored[idx] = incoming
				result.Created++
				dirty = true
				continue
			}
			existing := file.Items[at]
			if existing.sameFactAs(incoming) {
				// Nothing to write. Not even UpdatedAt: an ingestion that
				// re-derives an identical fact has learned nothing new about
				// when the fact changed, and moving the timestamp would make
				// every re-run look like a change to anything reading it.
				stored[idx] = existing
				result.Unchanged++
				continue
			}
			incoming.CreatedAt = existing.CreatedAt
			incoming.UpdatedAt = now
			// A freshly derived fact carries no staleness: whatever made the
			// stored row stale was a statement about the version just
			// replaced. StaleCheck re-decides against the new provenance.
			incoming.Stale = false
			incoming.StaleReason = ""
			file.Items[at] = incoming
			stored[idx] = incoming
			result.Updated++
			dirty = true
		}
		if !dirty {
			continue
		}
		path, err := s.save(file, now)
		if err != nil {
			return UpsertResult{}, err
		}
		result.Paths = append(result.Paths, path)
	}
	result.Items = stored
	return result, nil
}

// Replace overwrites the items of one project wholesale. It exists for the
// staleness pass, which rewrites annotations on rows it did not otherwise
// change; ingestion always goes through Upsert.
func (s *Store) Replace(project string, items []MemoryItem) (string, error) {
	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return "", fmt.Errorf("%w: project is required", ErrItemInvalid)
	}
	file := &projectFile{Version: ItemSchemaVersion, Project: trimmed, Items: make([]MemoryItem, 0, len(items))}
	for _, item := range items {
		if item.Project != trimmed {
			return "", fmt.Errorf("%w: item %s belongs to project %q, not %q", ErrItemInvalid, item.ID, item.Project, trimmed)
		}
		if err := item.Validate(); err != nil {
			return "", err
		}
		file.Items = append(file.Items, item)
	}
	return s.save(file, s.now().UTC())
}

// Delete removes one item by id and reports whether it was there.
func (s *Store) Delete(project, id string) (bool, error) {
	file, err := s.load(project)
	if err != nil {
		return false, err
	}
	for i, item := range file.Items {
		if item.ID != id {
			continue
		}
		file.Items = append(file.Items[:i], file.Items[i+1:]...)
		if _, err := s.save(file, s.now().UTC()); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// load reads a project's file. A project that has never been written, and a
// file written under a different schema version or recorded against a
// different project, all yield an empty set: re-ingesting is always safe,
// returning another project's items never is.
func (s *Store) load(project string) (*projectFile, error) {
	trimmed := strings.TrimSpace(project)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: project is required", ErrItemInvalid)
	}
	empty := &projectFile{Version: ItemSchemaVersion, Project: trimmed, Items: []MemoryItem{}}
	path := s.PathFor(trimmed)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return empty, nil
		}
		return nil, fmt.Errorf("read project memory: %w", err)
	}
	var loaded projectFile
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, fmt.Errorf("decode project memory %s: %w", path, err)
	}
	if loaded.Version != ItemSchemaVersion || loaded.Project != trimmed {
		return empty, nil
	}
	if loaded.Items == nil {
		loaded.Items = []MemoryItem{}
	}
	for i := range loaded.Items {
		// A row on disk that names a different project cannot be served for
		// this one, whatever the file header says.
		if loaded.Items[i].Project != trimmed {
			return empty, nil
		}
	}
	sortItems(loaded.Items)
	return &loaded, nil
}

// save writes a project's file atomically: a crash mid-write leaves the
// previous memory intact rather than a truncated file.
func (s *Store) save(file *projectFile, at time.Time) (string, error) {
	file.Version = ItemSchemaVersion
	file.UpdatedAt = at
	sortItems(file.Items)

	path := s.PathFor(file.Project)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create project memory dir: %w", err)
	}
	payload, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode project memory: %w", err)
	}
	payload = append(payload, '\n')
	tmp, err := os.CreateTemp(dir, projectFileName+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create project memory temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write project memory: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close project memory: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("chmod project memory: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("commit project memory: %w", err)
	}
	return path, nil
}

func sortItems(items []MemoryItem) {
	sort.Slice(items, func(a, b int) bool { return items[a].ID < items[b].ID })
}

func timestampOr(t, fallback time.Time) time.Time {
	if t.IsZero() {
		return fallback
	}
	return t.UTC()
}

// sanitizePathSegment reduces an identifier to something safe to use as a
// single path element: no separators, no traversal, no surprises from a
// project id that came in from a tracker or a branch name.
func sanitizePathSegment(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "." || out == ".." {
		return ""
	}
	return out
}
