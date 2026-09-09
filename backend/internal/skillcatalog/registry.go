package skillcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// registryVersion is the on-disk schema version of registry.json.
const registryVersion = 1

// RegistryFileName is the catalog index inside the catalog root.
const RegistryFileName = "registry.json"

var (
	// ErrNotInstalled means no such skill/version is in the catalog.
	ErrNotInstalled = errors.New("skillcatalog: skill is not installed")
	// ErrAlreadyInstalled means that exact skill and version is already there.
	ErrAlreadyInstalled = errors.New("skillcatalog: skill version is already installed")
	// ErrNotEnabled means the skill exists but the project never enabled it.
	ErrNotEnabled = errors.New("skillcatalog: skill is not enabled for this project")
	// ErrInUse means an installed version is still activated somewhere.
	ErrInUse = errors.New("skillcatalog: skill version is still enabled for a project")
)

// Dir returns the catalog root for a data dir. It is a sibling of
// <dataDir>/skills/using-ao rather than a parent of it: internal/skillassets
// clobbers that directory on every daemon boot, so the catalog keeps its own
// namespace and never shares a path with it.
func Dir(dataDir string) string {
	return filepath.Join(dataDir, "skills", "catalog")
}

// Installed is one installed skill version.
type Installed struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	RiskLevel   RiskLevel `json:"riskLevel"`
	Origin      Origin    `json:"origin"`
	// Digest is the verified content digest at install time.
	Digest      string    `json:"digest"`
	InstalledAt time.Time `json:"installedAt"`
	InstalledBy string    `json:"installedBy"`
}

// Grant is the set of capabilities a project actually authorized, and who
// authorized it. It is stored per activation, not per skill: the same skill
// enabled on two projects can hold different grants.
type Grant struct {
	Capabilities []Capability `json:"capabilities"`
	ApprovedBy   string       `json:"approvedBy"`
	ApprovedAt   time.Time    `json:"approvedAt"`
}

// Activation is one skill enabled on one project, pinned to one version.
type Activation struct {
	ProjectID domain.ProjectID `json:"projectId"`
	SkillID   string           `json:"skillId"`
	Version   string           `json:"version"`
	Enabled   bool             `json:"enabled"`
	Grant     Grant            `json:"grant"`
}

type registryFile struct {
	Version     int          `json:"version"`
	Installed   []Installed  `json:"installed"`
	Activations []Activation `json:"activations"`
}

// Registry is the catalog of installed skills and their per-project
// activations, backed by one JSON file under the AO data dir.
//
// It is file-backed rather than a SQLite table because this phase adds no
// migration: the catalog has to be shippable and testable without touching the
// schema another workstream is actively changing. Moving it into SQLite is a
// later, deliberate migration — see the roadmap in docs/skills/roadmap.md.
type Registry struct {
	root string
	file registryFile
}

// OpenRegistry loads (or initialises) the catalog rooted at dir.
func OpenRegistry(dir string) (*Registry, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("skillcatalog: registry dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("skillcatalog: create catalog dir: %w", err)
	}
	r := &Registry{root: dir, file: registryFile{Version: registryVersion}}
	b, err := os.ReadFile(filepath.Join(dir, RegistryFileName)) //nolint:gosec // caller-supplied catalog root.
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r, nil
	case err != nil:
		return nil, fmt.Errorf("skillcatalog: read registry: %w", err)
	}
	var loaded registryFile
	if err := json.Unmarshal(b, &loaded); err != nil {
		return nil, fmt.Errorf("skillcatalog: parse registry: %w", err)
	}
	if loaded.Version != registryVersion {
		return nil, fmt.Errorf("skillcatalog: registry version %d is not supported (want %d)",
			loaded.Version, registryVersion)
	}
	r.file = loaded
	return r, nil
}

// Root is the catalog directory.
func (r *Registry) Root() string { return r.root }

// PackageDir is where one installed version's files live. Versions are kept
// side by side so a project pinned to an older one keeps working after a newer
// one is installed.
func (r *Registry) PackageDir(id, version string) string {
	return filepath.Join(r.root, "packages", id, version)
}

func (r *Registry) save() error {
	sort.Slice(r.file.Installed, func(i, j int) bool {
		if r.file.Installed[i].ID != r.file.Installed[j].ID {
			return r.file.Installed[i].ID < r.file.Installed[j].ID
		}
		return r.file.Installed[i].Version < r.file.Installed[j].Version
	})
	sort.Slice(r.file.Activations, func(i, j int) bool {
		if r.file.Activations[i].ProjectID != r.file.Activations[j].ProjectID {
			return r.file.Activations[i].ProjectID < r.file.Activations[j].ProjectID
		}
		return r.file.Activations[i].SkillID < r.file.Activations[j].SkillID
	})
	b, err := json.MarshalIndent(r.file, "", "  ")
	if err != nil {
		return fmt.Errorf("skillcatalog: encode registry: %w", err)
	}
	b = append(b, '\n')
	// Write-and-rename so an interrupted save cannot leave a half-written
	// registry that would read as "nothing is installed" on next boot.
	tmp := filepath.Join(r.root, RegistryFileName+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("skillcatalog: write registry: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(r.root, RegistryFileName)); err != nil {
		return fmt.Errorf("skillcatalog: commit registry: %w", err)
	}
	return nil
}

// Install copies a validated package into the catalog and records it. It does
// not enable the skill anywhere: installing and running are separate acts, and
// a freshly installed skill is reachable by exactly nothing.
func (r *Registry) Install(srcDir, installedBy string) (Installed, error) {
	pkg, err := LoadPackage(srcDir)
	if err != nil {
		return Installed{}, err
	}
	if r.isInstalled(pkg.Manifest.ID, pkg.Manifest.Version) {
		return Installed{}, fmt.Errorf("%w: %s@%s", ErrAlreadyInstalled, pkg.Manifest.ID, pkg.Manifest.Version)
	}
	dest := r.PackageDir(pkg.Manifest.ID, pkg.Manifest.Version)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return Installed{}, fmt.Errorf("skillcatalog: create package dir: %w", err)
	}
	if err := CopyPackage(srcDir, dest); err != nil {
		return Installed{}, err
	}
	// Re-verify from the installed copy, not the source: the digest that
	// matters is the one covering the bytes the catalog will actually read.
	if _, err := LoadPackage(dest); err != nil {
		_ = os.RemoveAll(dest)
		return Installed{}, fmt.Errorf("skillcatalog: installed copy failed verification: %w", err)
	}
	entry := Installed{
		ID:          pkg.Manifest.ID,
		Version:     pkg.Manifest.Version,
		Name:        pkg.Manifest.Name,
		Description: pkg.Manifest.Description,
		RiskLevel:   pkg.Manifest.RiskLevel,
		Origin:      pkg.Manifest.Origin,
		Digest:      pkg.Digest,
		InstalledAt: time.Now().UTC(),
		InstalledBy: installedBy,
	}
	r.file.Installed = append(r.file.Installed, entry)
	if err := r.save(); err != nil {
		_ = os.RemoveAll(dest)
		return Installed{}, err
	}
	return entry, nil
}

// Uninstall removes one installed version. It refuses while any project still
// has that version enabled, so a project can never resolve to a skill whose
// files are gone.
func (r *Registry) Uninstall(id, version string) error {
	if !r.isInstalled(id, version) {
		return fmt.Errorf("%w: %s@%s", ErrNotInstalled, id, version)
	}
	for _, a := range r.file.Activations {
		if a.SkillID == id && a.Version == version && a.Enabled {
			return fmt.Errorf("%w: %s@%s is enabled for project %s", ErrInUse, id, version, a.ProjectID)
		}
	}
	kept := r.file.Installed[:0]
	for _, in := range r.file.Installed {
		if in.ID == id && in.Version == version {
			continue
		}
		kept = append(kept, in)
	}
	r.file.Installed = kept
	// Drop disabled activations pinned to the removed version so a later
	// re-enable cannot silently resurrect a grant against absent files.
	keptActs := r.file.Activations[:0]
	for _, a := range r.file.Activations {
		if a.SkillID == id && a.Version == version {
			continue
		}
		keptActs = append(keptActs, a)
	}
	r.file.Activations = keptActs
	if err := r.save(); err != nil {
		return err
	}
	return os.RemoveAll(r.PackageDir(id, version))
}

// ListInstalled returns every installed version, ordered by id then version.
func (r *Registry) ListInstalled() []Installed {
	out := make([]Installed, len(r.file.Installed))
	copy(out, r.file.Installed)
	return out
}

// ListActivations returns every activation record, enabled or not.
func (r *Registry) ListActivations() []Activation {
	out := make([]Activation, len(r.file.Activations))
	copy(out, r.file.Activations)
	return out
}

// isInstalled reports whether this exact skill version is in the catalog.
func (r *Registry) isInstalled(id, version string) bool {
	for _, in := range r.file.Installed {
		if in.ID == id && in.Version == version {
			return true
		}
	}
	return false
}

// LatestVersion returns the highest installed version of a skill.
func (r *Registry) LatestVersion(id string) (string, bool) {
	var best Version
	var bestRaw string
	for _, in := range r.file.Installed {
		if in.ID != id {
			continue
		}
		v, err := ParseVersion(in.Version)
		if err != nil {
			continue
		}
		if bestRaw == "" || v.Compare(best) > 0 {
			best, bestRaw = v, in.Version
		}
	}
	return bestRaw, bestRaw != ""
}

// EnableRequest is one explicit per-project activation.
type EnableRequest struct {
	ProjectID domain.ProjectID
	SkillID   string
	// Version pins the activation. It is required: "latest" as an activation
	// target would let an install silently change what a project already
	// approved.
	Version string
	// GrantCapabilities are the capabilities the approver is granting. Any
	// capability the manifest requests but this omits will be refused at run
	// time.
	GrantCapabilities []Capability
	ApprovedBy        string
	// SubjectPermissions are the approver's AO permissions on this project.
	SubjectPermissions []domain.Permission
}

// Enable activates an installed skill on one project with an explicit grant.
//
// It checks at grant time that the approver actually holds the AO permission
// each capability is gated on, so a grant can never be broader than the person
// who made it. It deliberately does NOT check runner isolation here: a project
// may legitimately pre-authorize a capability whose runner does not exist yet,
// and Authorize refuses it at run time. Enabling is a statement of intent;
// running is where the boundary is enforced.
func (r *Registry) Enable(req EnableRequest) (Activation, error) {
	if strings.TrimSpace(string(req.ProjectID)) == "" {
		return Activation{}, errors.New("skillcatalog: projectId is required")
	}
	if strings.TrimSpace(req.Version) == "" {
		return Activation{}, errors.New("skillcatalog: version is required; activation is always pinned")
	}
	if !r.isInstalled(req.SkillID, req.Version) {
		return Activation{}, fmt.Errorf("%w: %s@%s", ErrNotInstalled, req.SkillID, req.Version)
	}
	pkg, err := LoadPackage(r.PackageDir(req.SkillID, req.Version))
	if err != nil {
		return Activation{}, err
	}
	if err := ValidateGrant(pkg.Manifest, req); err != nil {
		return Activation{}, err
	}

	activation := Activation{
		ProjectID: req.ProjectID,
		SkillID:   req.SkillID,
		Version:   req.Version,
		Enabled:   true,
		Grant: Grant{
			Capabilities: append([]Capability(nil), req.GrantCapabilities...),
			ApprovedBy:   req.ApprovedBy,
			ApprovedAt:   time.Now().UTC(),
		},
	}
	replaced := false
	for i, a := range r.file.Activations {
		if a.ProjectID == req.ProjectID && a.SkillID == req.SkillID {
			r.file.Activations[i] = activation
			replaced = true
			break
		}
	}
	if !replaced {
		r.file.Activations = append(r.file.Activations, activation)
	}
	if err := r.save(); err != nil {
		return Activation{}, err
	}
	return activation, nil
}

// ValidateGrant is the whole rule set for "may this person enable this skill
// on this project with this grant". It is exported and store-agnostic so the
// file-backed Registry and the SQLite-backed catalog decide identically; a
// second copy of these rules is how the two would drift apart.
func ValidateGrant(m Manifest, req EnableRequest) error {
	if strings.TrimSpace(req.ApprovedBy) == "" {
		return errors.New("skillcatalog: approvedBy is required; an activation records who authorized it")
	}
	declared := map[Capability]bool{}
	for _, c := range m.Capabilities {
		declared[c] = true
	}
	held := map[domain.Permission]bool{}
	for _, p := range req.SubjectPermissions {
		held[p] = true
	}
	for _, p := range m.Authorization.RequiredPermissions {
		if !held[domain.Permission(p)] {
			return fmt.Errorf("skillcatalog: enabling %s requires the %s permission", m.ID, p)
		}
	}
	seen := map[Capability]bool{}
	for _, c := range req.GrantCapabilities {
		spec, ok := c.Spec()
		if !ok {
			return invalidf("cannot grant unknown capability %q", c)
		}
		if !declared[c] {
			return fmt.Errorf("skillcatalog: cannot grant %q: skill %s does not request it", c, m.ID)
		}
		if seen[c] {
			return fmt.Errorf("skillcatalog: capability %q granted twice", c)
		}
		seen[c] = true
		if !held[spec.RequiredPermission] {
			return fmt.Errorf("skillcatalog: granting %q requires the %s permission", c, spec.RequiredPermission)
		}
	}
	return nil
}

// Disable deactivates a skill on a project and drops its grant. The record is
// kept (so the history of what was once approved survives) but with no
// capabilities: re-enabling requires granting again from scratch.
func (r *Registry) Disable(projectID domain.ProjectID, skillID string) error {
	for i, a := range r.file.Activations {
		if a.ProjectID != projectID || a.SkillID != skillID {
			continue
		}
		r.file.Activations[i].Enabled = false
		r.file.Activations[i].Grant = Grant{}
		return r.save()
	}
	return fmt.Errorf("%w: %s on %s", ErrNotEnabled, skillID, projectID)
}

// Resolved is an enabled skill, its pinned package, and the project's grant.
type Resolved struct {
	Activation Activation
	Package    Package
}

// Resolve returns the skill a project has enabled, fail-closed at every step:
// not installed, not enabled, disabled, pinned to a version that is gone, or a
// package whose bytes no longer match its digest all produce an error rather
// than a usable result.
func (r *Registry) Resolve(projectID domain.ProjectID, skillID string) (Resolved, error) {
	for _, a := range r.file.Activations {
		if a.ProjectID != projectID || a.SkillID != skillID {
			continue
		}
		if !a.Enabled {
			return Resolved{}, fmt.Errorf("%w: %s on %s is disabled", ErrNotEnabled, skillID, projectID)
		}
		if !r.isInstalled(skillID, a.Version) {
			return Resolved{}, fmt.Errorf("%w: %s on %s is pinned to %s, which is not installed",
				ErrNotInstalled, skillID, projectID, a.Version)
		}
		pkg, err := LoadPackage(r.PackageDir(skillID, a.Version))
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Activation: a, Package: pkg}, nil
	}
	return Resolved{}, fmt.Errorf("%w: %s on %s", ErrNotEnabled, skillID, projectID)
}

// ResolveForProject lists every skill currently enabled on a project.
func (r *Registry) ResolveForProject(projectID domain.ProjectID) ([]Resolved, error) {
	var out []Resolved
	for _, a := range r.file.Activations {
		if a.ProjectID != projectID || !a.Enabled {
			continue
		}
		resolved, err := r.Resolve(projectID, a.SkillID)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}
