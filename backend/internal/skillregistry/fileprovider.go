package skillregistry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// fileprovider.go -- the deterministic, offline registry.
//
// It reads a directory: one index file naming releases, and one package tree
// per release beside it. That makes it three things at once, which is why it
// is the implementation this phase ships rather than a stub:
//
//   - the fixture every test uses, with no network, no clock and no ordering
//     that depends on the filesystem;
//   - an offline or air-gapped mirror, which is a real deployment;
//   - the shape a company-private registry has once somebody puts the same
//     layout behind HTTPS.
//
// It is deliberately not a client for any particular marketplace. Coupling AO
// to a public endpoint is a decision this phase does not make.

// IndexFileName is the registry index inside a local registry root.
const IndexFileName = "registry.json"

// IndexAPIVersion is the only registry-index contract this build understands.
// A directory declaring anything else is refused rather than best-effort
// parsed, for the same reason skillcatalog refuses an unknown manifest
// apiVersion: a reader that guesses at an unknown contract is a reader that
// installs something it did not understand.
const IndexAPIVersion = "ao.registry/v1"

// indexFile is the on-disk shape of registry.json.
type indexFile struct {
	APIVersion string       `json:"apiVersion"`
	RegistryID string       `json:"registryId"`
	Releases   []indexEntry `json:"releases"`
}

// indexEntry is one release plus where its bytes live, relative to the
// registry root.
type indexEntry struct {
	Release
	// ArtifactPath is the package tree, as a slash-separated path RELATIVE to
	// the registry root. Absolute paths and upward traversal are refused: a
	// registry that could name /etc or ../.. would be a registry that reads
	// this host rather than serving it.
	ArtifactPath string `json:"artifactPath"`
}

// FileProvider serves releases from a directory on this host.
type FileProvider struct {
	registryID string
	root       string
}

// NewFileProvider opens the registry rooted at dir. It reads and validates the
// index eagerly: a registry whose index is malformed must be unreadable from
// the first call, not quietly empty on the ones that happen to miss the bad
// entry.
func NewFileProvider(registryID, dir string) (*FileProvider, error) {
	if strings.TrimSpace(registryID) == "" {
		return nil, errors.New("skillregistry: registryId is required")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("%w: %s is not an absolute path", ErrRegistryUnreadable, dir)
	}
	p := &FileProvider{registryID: registryID, root: filepath.Clean(dir)}
	if _, err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

// RegistryID implements Provider.
func (p *FileProvider) RegistryID() string { return p.registryID }

// load reads and validates the whole index. It is re-read on every call rather
// than cached, which is what makes ResolveExactRelease a genuine re-read of the
// authoritative source: a cached index would let an install act on a release
// that was revoked since the search.
func (p *FileProvider) load() ([]indexEntry, error) {
	b, err := os.ReadFile(filepath.Join(p.root, IndexFileName)) //nolint:gosec // configured registry root.
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrRegistryUnreadable, IndexFileName, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var idx indexFile
	if err := dec.Decode(&idx); err != nil {
		return nil, fmt.Errorf("%w: parse %s: %w", ErrRegistryUnreadable, IndexFileName, err)
	}
	if idx.APIVersion != IndexAPIVersion {
		return nil, fmt.Errorf("%w: index apiVersion %q is not supported (want %q)",
			ErrRegistryUnreadable, idx.APIVersion, IndexAPIVersion)
	}
	seen := map[string]bool{}
	for i := range idx.Releases {
		entry := &idx.Releases[i]
		// The registry id is AO's, from the configuration. An index that
		// carried its own would let a directory claim to be another registry,
		// and every install AO recorded would name the wrong origin.
		entry.RegistryID = p.registryID
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("%w: release %s: %w", ErrRegistryUnreadable, entry.Ref(), err)
		}
		if err := p.validateArtifactPath(entry.ArtifactPath); err != nil {
			return nil, fmt.Errorf("%w: release %s: %w", ErrRegistryUnreadable, entry.Ref(), err)
		}
		// One entry per (skillId, version). A duplicate is the mutable-version
		// attack in its simplest form: two rows under one identity, and
		// whichever the reader hits first wins.
		if seen[entry.Ref()] {
			return nil, fmt.Errorf("%w: %s is listed twice; a version is one immutable thing",
				ErrRegistryUnreadable, entry.Ref())
		}
		seen[entry.Ref()] = true
	}
	return idx.Releases, nil
}

// validateArtifactPath refuses anything that could leave the registry root.
func (p *FileProvider) validateArtifactPath(rel string) error {
	if strings.TrimSpace(rel) == "" {
		return errors.New("artifactPath is required")
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return fmt.Errorf("artifactPath %q must be relative to the registry root", rel)
	}
	if strings.Contains(rel, "..") {
		return fmt.Errorf("artifactPath %q must not traverse upward", rel)
	}
	if strings.ContainsAny(rel, `\:`) {
		return fmt.Errorf("artifactPath %q must be a slash-separated relative path", rel)
	}
	return nil
}

// artifactDir resolves an entry's package tree and re-checks containment
// against the RESOLVED path, not only the declared one.
//
// The string check in validateArtifactPath catches "../..". This catches the
// case it cannot: a path that stays inside the root textually and leaves it
// through a symlink somebody planted in the registry directory.
func (p *FileProvider) artifactDir(entry indexEntry) (string, error) {
	joined := filepath.Join(p.root, filepath.FromSlash(entry.ArtifactPath))
	resolvedRoot, err := filepath.EvalSymlinks(p.root)
	if err != nil {
		return "", fmt.Errorf("%w: resolve registry root: %w", ErrRegistryUnreadable, err)
	}
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("%w: resolve artifact for %s: %w", ErrRegistryUnreadable, entry.Ref(), err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: artifact for %s resolves outside the registry root",
			ErrRegistryUnreadable, entry.Ref())
	}
	return resolved, nil
}

// Search implements Provider. It moves no package bytes: it reads the index
// and filters it.
func (p *FileProvider) Search(_ context.Context, q Query) ([]Release, error) {
	entries, err := p.load()
	if err != nil {
		return nil, err
	}
	q = q.Normalized()
	out := make([]Release, 0, len(entries))
	for _, e := range entries {
		if e.Revoked && !q.IncludeRevoked {
			continue
		}
		if e.Deprecated && !q.IncludeDeprecated {
			continue
		}
		if q.Publisher != "" && e.Publisher != q.Publisher {
			continue
		}
		if q.Capability != "" && !containsString(e.RequestedCapabilities, q.Capability) {
			continue
		}
		if q.Text != "" && !matchesText(e.Release, q.Text) {
			continue
		}
		out = append(out, e.Release)
	}
	SortReleases(out)
	if limit := q.EffectiveLimit(); len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func matchesText(r Release, needle string) bool {
	for _, field := range []string{r.SkillID, r.Name, r.Description, r.Publisher} {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}
	return false
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// Get implements Provider.
func (p *FileProvider) Get(_ context.Context, skillID, version string) (Release, error) {
	entries, err := p.load()
	if err != nil {
		return Release{}, err
	}
	entry, err := findEntry(entries, skillID, version)
	if err != nil {
		return Release{}, err
	}
	return entry.Release, nil
}

// ListVersions implements Provider, including deprecated and revoked releases.
func (p *FileProvider) ListVersions(_ context.Context, skillID string) ([]Release, error) {
	entries, err := p.load()
	if err != nil {
		return nil, err
	}
	out := make([]Release, 0, len(entries))
	for _, e := range entries {
		if e.SkillID == skillID {
			out = append(out, e.Release)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchSkill, skillID)
	}
	SortReleases(out)
	return out, nil
}

// ResolveExactRelease implements Provider. It re-reads the index and refuses
// anything that is not one complete, immutable version.
func (p *FileProvider) ResolveExactRelease(_ context.Context, skillID, version string) (Release, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return Release{}, fmt.Errorf("%w: a version is required; there is no latest to install",
			ErrNoSuchRelease)
	}
	if _, err := skillcatalog.ParseVersion(version); err != nil {
		return Release{}, fmt.Errorf("%w: %q is not one exact MAJOR.MINOR.PATCH version",
			ErrNoSuchRelease, version)
	}
	entries, err := p.load()
	if err != nil {
		return Release{}, err
	}
	entry, err := findEntry(entries, skillID, version)
	if err != nil {
		return Release{}, err
	}
	// Resolving proves the bytes are reachable and inside the root before the
	// caller commits to an install. A release that indexes an artifact nobody
	// can read is an unreadable registry, not a successful resolve.
	if _, err := p.artifactDir(entry); err != nil {
		return Release{}, err
	}
	return entry.Release, nil
}

func findEntry(entries []indexEntry, skillID, version string) (indexEntry, error) {
	sawSkill := false
	for _, e := range entries {
		if e.SkillID != skillID {
			continue
		}
		sawSkill = true
		if e.Version == version {
			return e, nil
		}
	}
	if sawSkill {
		return indexEntry{}, fmt.Errorf("%w: %s@%s", ErrNoSuchRelease, skillID, version)
	}
	return indexEntry{}, fmt.Errorf("%w: %s", ErrNoSuchSkill, skillID)
}

// FetchArtifact implements Provider by copying the package tree into destDir.
//
// It copies with skillcatalog.CopyPackage, which refuses any entry that is not
// a regular file. That is the control that stops a symlink in a registry from
// becoming a symlink in AO's catalog pointing at a credential on this host --
// and it runs BEFORE anything reads the tree, so a hostile link is refused
// rather than followed.
func (p *FileProvider) FetchArtifact(_ context.Context, rel Release, destDir string) error {
	entries, err := p.load()
	if err != nil {
		return err
	}
	entry, err := findEntry(entries, rel.SkillID, rel.Version)
	if err != nil {
		return err
	}
	src, err := p.artifactDir(entry)
	if err != nil {
		return err
	}
	return skillcatalog.CopyPackage(src, destDir)
}

// FileDigest is sha256 over one file's bytes, hex-encoded. It is what a caller
// uses to compute a release's manifestDigest over the skill.yaml it fetched.
func FileDigest(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // caller-supplied quarantine path.
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// DefaultProviderFactory opens the provider a registry's type calls for.
//
// It is the ONE place that maps a configured type onto an implementation, so
// adding an AO official registry, a company-private HTTPS one or a
// GitHub-backed one is a case here rather than a change at every call site. A
// type with no implementation is refused rather than silently answering nothing.
type DefaultProviderFactory struct{}

// Open implements ProviderFactory.
func (DefaultProviderFactory) Open(_ context.Context, reg Registry) (Provider, error) {
	switch reg.Type {
	case RegistryLocal:
		return NewFileProvider(reg.ID, reg.Location)
	case RegistryHTTPS, RegistryGit:
		return nil, fmt.Errorf("%w: registry type %q is declared but not implemented in this build",
			ErrRegistryUnreadable, reg.Type)
	}
	return nil, fmt.Errorf("%w: registry type %q is not a type AO knows", ErrRegistryUnreadable, reg.Type)
}
