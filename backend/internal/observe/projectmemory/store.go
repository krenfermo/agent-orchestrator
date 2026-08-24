package projectmemory

import (
	stdctx "context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// evidenceDirName is the subtree of the AO data dir that holds baseline
// evidence. Keeping it under the data dir (rather than beside the repository
// or in an OS app-data location) is the same hard rule the rest of AO follows:
// all app state lives under ~/.ao, overridable only by AO_DATA_DIR.
const evidenceDirName = "project-memory/baseline"

// ErrEvidencePath is the sentinel every rejected evidence location wraps.
var ErrEvidencePath = errors.New("invalid evidence directory")

// Sink is where a finished evidence record goes. The recorder depends on this
// narrow interface rather than on the filesystem so tests, and any later phase
// that wants to fan evidence somewhere else, do not have to touch the
// recorder.
type Sink interface {
	// Write persists one record and returns where it landed.
	Write(ctx stdctx.Context, record EvidenceRecord) (string, error)
}

// DataDir resolves AO's durable data directory the same way the daemon does:
// an explicit AO_DATA_DIR wins, otherwise it is ~/.ao/data. It deliberately
// does not fall back to any OS-default application-data location — see the
// hard rule in AGENTS.md.
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

// EvidenceDir is where baseline evidence is written: <data dir>/project-memory/baseline.
func EvidenceDir() (string, error) {
	dataDir, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, filepath.FromSlash(evidenceDirName)), nil
}

// forbiddenPathSegments are the OS-default application-data locations AO must
// never write to. A misconfigured AO_DATA_DIR pointing at one of them is a
// configuration error worth failing on, not a location to quietly accept.
var forbiddenPathSegments = []string{
	filepath.Join("Library", "Application Support"),
	filepath.Join("AppData", "Roaming"),
	filepath.Join("AppData", "Local"),
}

// ValidateEvidenceDir rejects an evidence location AO is not allowed to use:
// a relative path (which would resolve against whatever the process happened
// to chdir into, typically a repository checkout) or a path inside an
// OS-default application-data directory.
func ValidateEvidenceDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("%w: directory is required", ErrEvidencePath)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: %q must be absolute so it cannot resolve inside a repository checkout", ErrEvidencePath, dir)
	}
	clean := filepath.Clean(dir)
	for _, segment := range forbiddenPathSegments {
		if strings.Contains(clean, string(filepath.Separator)+segment) {
			return fmt.Errorf("%w: %q is inside the OS application-data directory; AO state belongs under ~/.ao (or AO_DATA_DIR)", ErrEvidencePath, dir)
		}
	}
	return nil
}

// DirSink writes each evidence record as its own JSON file, filed under the
// run it belongs to: <root>/<run key>/<record id>.json.
//
// One file per dispatch rather than one appended log per run, because the
// baseline is read by later phases as a set of independent records and a
// half-written append would corrupt every record before it.
type DirSink struct {
	root string
}

// NewDirSink validates dir and returns a sink writing beneath it. The
// directory itself is created lazily on the first write, so constructing a
// sink for a run that turns out to dispatch nothing leaves no empty tree
// behind.
func NewDirSink(dir string) (*DirSink, error) {
	if err := ValidateEvidenceDir(dir); err != nil {
		return nil, err
	}
	return &DirSink{root: filepath.Clean(dir)}, nil
}

// NewDefaultDirSink returns a sink at the standard evidence location under
// AO's data dir.
func NewDefaultDirSink() (*DirSink, error) {
	dir, err := EvidenceDir()
	if err != nil {
		return nil, err
	}
	return NewDirSink(dir)
}

// Root is where this sink writes.
func (s *DirSink) Root() string { return s.root }

// Write validates the record, then persists it atomically. Validation runs
// before the write so a record that violates the measured/estimated labeling
// rule never reaches disk: an evidence file that exists is one whose numbers
// are honestly labeled.
func (s *DirSink) Write(_ stdctx.Context, record EvidenceRecord) (string, error) {
	record = record.normalized()
	if err := record.Validate(); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, record.RunKey())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create evidence dir: %w", err)
	}
	name := sanitizePathSegment(record.RecordID)
	if name == "" {
		return "", fmt.Errorf("%w: recordId %q has no usable filename", ErrEvidenceInvalid, record.RecordID)
	}
	path := filepath.Join(dir, name+".json")
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode evidence record: %w", err)
	}
	payload = append(payload, '\n')
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create evidence temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write evidence record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close evidence record: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", fmt.Errorf("chmod evidence record: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("commit evidence record: %w", err)
	}
	return path, nil
}

// sanitizePathSegment reduces an identifier to something safe to use as a
// single path element: no separators, no traversal, no surprises from an id
// that came in from a provider or a tracker.
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
	if out == "" || out == "." || out == ".." {
		return ""
	}
	return out
}
