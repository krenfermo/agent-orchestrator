package practical3d

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sentinel errors for unsafe roots and forbidden reruns.
var (
	ErrUnsafeRoot           = errors.New("3d practical: unsafe run root")
	ErrExperimentRegistered = errors.New("3d practical: experiment_id already registered; a rerun, replacement or relot is forbidden")
)

// ScratchRoot is the only home-relative location official runs may use.
func ScratchRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ao", "scratch", "frente3"), nil
}

func productionDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ao", "data"), nil
}

// ValidateRunRoot fails closed unless root is a not-yet-existing directory
// below ~/.ao/scratch/frente3 (or, only when allowExplicitTemp is set, below
// the OS temp directory). It always refuses the production AO data dir,
// directly, through symlinks, or through an inherited AO_DATA_DIR/AO_RUN_FILE.
func ValidateRunRoot(root string, allowExplicitTemp bool) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%w: root required", ErrUnsafeRoot)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	prod, err := productionDataDir()
	if err != nil {
		return "", err
	}
	if err := RefuseProductionEnvironment(); err != nil {
		return "", err
	}
	scratch, err := ScratchRoot()
	if err != nil {
		return "", err
	}
	allowed := []string{scratch}
	if allowExplicitTemp {
		allowed = append(allowed, os.TempDir())
	}
	resolved, err := resolveThroughExistingAncestor(abs)
	if err != nil {
		return "", err
	}
	resolvedProd := resolveOrSelf(prod)
	if within(abs, prod) || within(resolved, resolvedProd) {
		return "", fmt.Errorf("%w: production AO data path %q is forbidden", ErrUnsafeRoot, abs)
	}
	for _, base := range allowed {
		rb := resolveOrSelf(base)
		if within(abs, base) && within(resolved, rb) && resolved != rb {
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: root must be a new directory below %s", ErrUnsafeRoot, scratch)
}

// RefuseProductionEnvironment rejects an inherited AO_DATA_DIR/AO_RUN_FILE
// that points at production state, so no child process can inherit it.
func RefuseProductionEnvironment() error {
	prod, err := productionDataDir()
	if err != nil {
		return err
	}
	for _, name := range []string{"AO_DATA_DIR", "AO_RUN_FILE"} {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			continue
		}
		abs, err := filepath.Abs(v)
		if err != nil {
			return fmt.Errorf("%w: %s unresolvable", ErrUnsafeRoot, name)
		}
		resolved, _ := resolveThroughExistingAncestor(abs)
		legacy := filepath.Join(filepath.Dir(filepath.Dir(prod)), ".agent-orchestrator")
		if within(abs, prod) || (resolved != "" && within(resolved, resolveOrSelf(prod))) || abs == filepath.Join(filepath.Dir(prod), "running.json") || within(abs, legacy) {
			return fmt.Errorf("%w: inherited %s points at production AO state; unset it", ErrUnsafeRoot, name)
		}
	}
	return nil
}

func resolveOrSelf(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

func resolveThroughExistingAncestor(path string) (string, error) {
	missing := []string{}
	cur := path
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(cur)
		if next == cur {
			return "", err
		}
		missing = append(missing, filepath.Base(cur))
		cur = next
	}
	resolved, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		resolved = filepath.Join(resolved, missing[i])
	}
	return filepath.Clean(resolved), nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."))
}

// Registry is the append-only list of every experiment_id ever started from
// one scratch parent, with its outcome. There is no "last good batch": an ID
// that is already registered can never run again.
type Registry struct{ path string }

// RegistryEntry is one registry line.
type RegistryEntry struct {
	Type         string    `json:"type"`
	ExperimentID string    `json:"experiment_id"`
	RunRoot      string    `json:"run_root"`
	Kind         string    `json:"kind"`
	Timestamp    time.Time `json:"timestamp"`
	Verdict      string    `json:"verdict,omitempty"`
	ReasonCode   string    `json:"reason_code,omitempty"`
	LedgerSHA256 string    `json:"ledger_sha256,omitempty"`
}

// OpenRegistry opens <dir>/registry.jsonl.
func OpenRegistry(dir string) Registry { return Registry{path: filepath.Join(dir, "registry.jsonl")} }

// Path is the registry file.
func (r Registry) Path() string { return r.path }

// Entries reads every registry line (strict).
func (r Registry) Entries() ([]RegistryEntry, error) {
	f, err := os.Open(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []RegistryEntry
	s := bufio.NewScanner(f)
	for s.Scan() {
		var e RegistryEntry
		if err := strictUnmarshal(s.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("registry %s is malformed: %w", r.path, err)
		}
		out = append(out, e)
	}
	return out, s.Err()
}

// Claim atomically reserves an experiment_id (O_EXCL marker), so two
// concurrent runs of the same manifest cannot both start.
func (r Registry) Claim(id string) error {
	if !validSHA256(id) {
		return fmt.Errorf("invalid experiment_id")
	}
	dir := filepath.Join(filepath.Dir(r.path), "claims")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if os.IsExist(err) {
		return fmt.Errorf("%w: %s", ErrExperimentRegistered, id)
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// RefuseProductionPath rejects an output path inside production AO data.
func RefuseProductionPath(path string) error {
	prod, err := productionDataDir()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	resolved, _ := resolveThroughExistingAncestor(abs)
	if within(abs, prod) || (resolved != "" && within(resolved, resolveOrSelf(prod))) {
		return fmt.Errorf("%w: %s is inside production AO data", ErrUnsafeRoot, abs)
	}
	return nil
}

// DecideRun re-decides a finished run from disk. It accepts only a run root
// that the registry recorded (so a copied or fabricated directory cannot be
// decided), verifies the envelope identity, the ledger hash chain and, when
// the run has a RESULT, the anchored ledger digest.
func DecideRun(root string, allowExplicitTemp bool, now time.Time) (Report, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Report{}, err
	}
	if _, err := ValidateRunRoot(filepath.Join(abs, "x"), allowExplicitTemp); err != nil {
		return Report{}, err
	}
	reg, err := registryFor(abs)
	if err != nil {
		return Report{}, err
	}
	entries, err := reg.Entries()
	if err != nil {
		return Report{}, err
	}
	env, m, err := ReadEnvelope(filepath.Join(abs, "envelope.json"))
	if err != nil {
		return Report{}, err
	}
	registered, results, anchored := 0, 0, ""
	for _, e := range entries {
		if e.ExperimentID != env.ExperimentID {
			continue
		}
		switch e.Type {
		case "REGISTERED":
			registered++
			if e.RunRoot != abs {
				return Report{}, fmt.Errorf("%w: experiment %s is registered for another run root", ErrUnsafeRoot, env.ExperimentID)
			}
		case "RESULT":
			results++
			if e.RunRoot == abs {
				anchored = e.LedgerSHA256
			}
		}
	}
	if registered != 1 || results != 1 || !validSHA256(anchored) {
		return Report{}, fmt.Errorf("%w: run root %s needs exactly one registration and one anchored RESULT for experiment %s", ErrUnsafeRoot, abs, env.ExperimentID)
	}
	path := filepath.Join(abs, "ledger.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	if sha256Hex(raw) != anchored {
		return Report{}, fmt.Errorf("%w: ledger digest differs from the registry anchor", ErrInvalidManifest)
	}
	return EvaluateLedgerFile(m, path, now), nil
}

// Contains reports whether an experiment_id was ever registered.
func (r Registry) Contains(id string) (bool, error) {
	entries, err := r.Entries()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.ExperimentID == id {
			return true, nil
		}
	}
	return false, nil
}

// Append adds one durable line.
func (r Registry) Append(e RegistryEntry) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// Ledger is the append-only, fsynced JSONL ledger of one run.
type Ledger struct {
	mu   sync.Mutex
	file *os.File
	path string
	head string // SHA-256 of the last appended line (hash chain)
}

// CreateRunDirectory creates a fresh run root (never reusing an existing one),
// writes the immutable envelope and opens the append-only ledger.
func CreateRunDirectory(root string, manifest Manifest, metadata EnvelopeMetadata, allowExplicitTemp bool) (*Ledger, string, error) {
	abs, err := ValidateRunRoot(root, allowExplicitTemp)
	if err != nil {
		return nil, "", err
	}
	canonical, err := CanonicalManifest(manifest)
	if err != nil {
		return nil, "", err
	}
	if err := os.Mkdir(abs, 0o700); err != nil {
		return nil, "", fmt.Errorf("create fresh run root: %w", err)
	}
	id := sha256Hex(canonical)
	if err := writeExclusive(filepath.Join(abs, "manifest.json"), append(append([]byte{}, canonical...), '\n')); err != nil {
		return nil, "", err
	}
	env, err := json.Marshal(Envelope{ExperimentID: id, ManifestSHA256: id, Manifest: canonical, Metadata: metadata})
	if err != nil {
		return nil, "", err
	}
	if err := writeExclusive(filepath.Join(abs, "envelope.json"), append(env, '\n')); err != nil {
		return nil, "", err
	}
	ledgerPath := filepath.Join(abs, "ledger.jsonl")
	lf, err := os.OpenFile(ledgerPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	return &Ledger{file: lf, path: ledgerPath}, abs, nil
}

// ReadEnvelope loads an envelope and verifies experiment_id and
// manifest_sha256 against canonical_bytes(manifest).
func ReadEnvelope(path string) (Envelope, Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Envelope{}, Manifest{}, err
	}
	var env Envelope
	if err := strictUnmarshal(raw, &env); err != nil {
		return Envelope{}, Manifest{}, fmt.Errorf("envelope: %w", err)
	}
	m, err := DecodeManifest(env.Manifest)
	if err != nil {
		return env, Manifest{}, err
	}
	id, err := ExperimentID(m)
	if err != nil {
		return env, Manifest{}, err
	}
	if env.ExperimentID != id || env.ManifestSHA256 != id {
		return env, m, fmt.Errorf("%w: %s: envelope identity differs from canonical manifest digest", ErrInvalidManifest, ReasonIdentityMismatch)
	}
	return env, m, nil
}

// writeExclusive writes a new read-only (0400) file; it never overwrites.
func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// Append writes and fsyncs one event line; it never rewrites.
func (l *Ledger) Append(event Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return errors.New("ledger closed")
	}
	event.PrevEventSHA256 = l.head
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if bytes.Contains(raw, []byte{'\n'}) {
		return errors.New("ledger event contains newline")
	}
	if _, err = l.file.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err := l.file.Sync(); err != nil {
		return err
	}
	l.head = sha256Hex(raw)
	return nil
}

// Close closes the ledger file.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Path is the ledger file.
func (l *Ledger) Path() string { return l.path }

// reopenLedger opens an existing ledger for append, continuing its hash chain.
func reopenLedger(path string) (*Ledger, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	head := ""
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	if len(raw) > 0 {
		head = sha256Hex(lines[len(lines)-1])
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return nil, err
	}
	return &Ledger{file: f, path: path, head: head}, nil
}

// VerifyLedgerChain checks that every line names the SHA-256 of the previous
// line, so no line can be removed, inserted, reordered or edited without
// rewriting every later line.
func VerifyLedgerChain(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	prev := ""
	for i, line := range bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'}) {
		var e struct {
			Prev string `json:"prev_event_sha256"`
		}
		if err := json.Unmarshal(line, &e); err != nil || e.Prev != prev {
			return fmt.Errorf("ledger hash chain broken at line %d", i+1)
		}
		prev = sha256Hex(line)
	}
	return nil
}

// ReadLedger strictly decodes every ledger line.
func ReadLedger(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 64*1024), 64*1024*1024)
	var out []Event
	line := 0
	for s.Scan() {
		line++
		var e Event
		if err := strictUnmarshal(s.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("ledger line %d: %w", line, err)
		}
		out = append(out, e)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// WriteReport writes a new report file (never overwrites).
func WriteReport(path string, report Report) error {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := io.Copy(f, bytes.NewReader(append(raw, '\n')))
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
