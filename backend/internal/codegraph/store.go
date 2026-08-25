package codegraph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// storeDirName is the subtree of the AO data dir that holds code graphs.
// Keeping it there (rather than beside the checkout or in an OS app-data
// location) is the same hard rule the rest of AO follows: all app state lives
// under ~/.ao, overridable only by AO_DATA_DIR.
const storeDirName = "codegraph"

// graphFileName is the single file one project's graph is persisted to.
const graphFileName = "graph.json"

// ErrStorePath is the sentinel every rejected storage location wraps.
var ErrStorePath = errors.New("codegraph: invalid store directory")

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

// StoreRoot is where code graphs are written: <data dir>/codegraph.
func StoreRoot() (string, error) {
	dataDir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, storeDirName), nil
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

// CanonicalRoot turns a project root into the absolute, symlink-resolved,
// cleaned path the graph is keyed by. Two spellings of the same checkout
// (a relative path, a path through /tmp on macOS) must land on one key, or
// the same project would be indexed twice; two genuinely different checkouts
// must never collapse onto one.
func CanonicalRoot(root string) (string, error) {
	trimmed := strings.TrimSpace(root)
	if trimmed == "" {
		return "", fmt.Errorf("%w: project root is required", ErrProjectRoot)
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %q: %w", ErrProjectRoot, root, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// ProjectKey is the storage key for a canonical project root: a readable
// prefix plus a hash of the full path. The hash — not the directory name — is
// what keeps two projects apart, so sibling checkouts that share a base name
// ("api" in two different orgs) never collide.
func ProjectKey(canonicalRoot string) string {
	sum := sha256.Sum256([]byte(canonicalRoot))
	digest := hex.EncodeToString(sum[:8])
	label := sanitizePathSegment(filepath.Base(canonicalRoot))
	if label == "" {
		return digest
	}
	if len(label) > 40 {
		label = label[:40]
	}
	return label + "-" + digest
}

// Store persists one graph per project under a validated root directory.
type Store struct {
	root string
}

// NewStore validates dir and returns a store writing beneath it. The
// directory is created lazily on first write, so constructing a store for a
// project that is never indexed leaves no empty tree behind.
func NewStore(dir string) (*Store, error) {
	if err := ValidateStoreDir(dir); err != nil {
		return nil, err
	}
	return &Store{root: filepath.Clean(dir)}, nil
}

// NewDefaultStore returns a store at the standard location under AO's data
// dir.
func NewDefaultStore() (*Store, error) {
	dir, err := StoreRoot()
	if err != nil {
		return nil, err
	}
	return NewStore(dir)
}

// Root is where this store writes.
func (s *Store) Root() string { return s.root }

// PathFor returns the file a project's graph is persisted to. Each project
// gets its own directory keyed by its root, which is the whole of the
// multirepo isolation guarantee: there is no shared file for two projects to
// leak entries through.
func (s *Store) PathFor(canonicalRoot string) string {
	return filepath.Join(s.root, "projects", ProjectKey(canonicalRoot), graphFileName)
}

// Load reads the persisted graph for a project root. A project that has never
// been indexed, a graph written by an older schema version, and a graph whose
// recorded root does not match the requested one all yield a fresh empty
// graph with found=false: re-indexing is always safe, returning another
// project's entries never is.
func (s *Store) Load(canonicalRoot string) (graph *Graph, found bool, err error) {
	path := s.PathFor(canonicalRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return NewGraph(canonicalRoot), false, nil
		}
		return nil, false, fmt.Errorf("read code graph: %w", err)
	}
	var loaded Graph
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return nil, false, fmt.Errorf("decode code graph %s: %w", path, err)
	}
	if loaded.Version != graphVersion || loaded.ProjectRoot != canonicalRoot {
		return NewGraph(canonicalRoot), false, nil
	}
	if loaded.Files == nil {
		loaded.Files = map[string]FileEntry{}
	}
	loaded.ProjectKey = ProjectKey(canonicalRoot)
	return &loaded, true, nil
}

// Save writes a project's graph atomically: a crash mid-write leaves the
// previous graph intact rather than a truncated one.
func (s *Store) Save(graph *Graph) (string, error) {
	if graph == nil {
		return "", fmt.Errorf("%w: graph is required", ErrStorePath)
	}
	if graph.ProjectRoot == "" {
		return "", fmt.Errorf("%w: graph has no project root", ErrProjectRoot)
	}
	graph.Version = graphVersion
	graph.ProjectKey = ProjectKey(graph.ProjectRoot)
	if graph.Files == nil {
		graph.Files = map[string]FileEntry{}
	}

	path := s.PathFor(graph.ProjectRoot)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create code graph dir: %w", err)
	}
	payload, err := json.MarshalIndent(graph, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode code graph: %w", err)
	}
	payload = append(payload, '\n')
	tmp, err := os.CreateTemp(dir, graphFileName+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create code graph temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write code graph: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close code graph: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("chmod code graph: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("commit code graph: %w", err)
	}
	return path, nil
}

// sanitizePathSegment reduces a directory name to something safe to use as a
// single path element: no separators, no traversal, no surprises from a
// checkout directory whose name came from a branch or a ticket title.
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
