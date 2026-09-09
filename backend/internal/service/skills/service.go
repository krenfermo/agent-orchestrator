// Package skills is the durable half of AO's skill catalog: it turns the rules
// in internal/skillcatalog into installs, per-project activations and an audit
// trail that survive a restart.
//
// The split is deliberate. skillcatalog owns every DECISION -- is this manifest
// valid, does this digest match, may this person grant this capability, is this
// run authorized. This package owns PERSISTENCE and SEQUENCING -- verify, then
// write, then audit, and refuse before writing anything when a check fails.
// Nothing here re-implements a rule; a second copy of the capability table is
// how the file-backed registry and the database would come to disagree.
//
// Authorization is not done here either. Installation-wide routes are gated by
// controllers.GlobalAuthzMiddleware and per-project routes by Guard.AllowProject,
// which is the same choke point every other AO surface uses. What this service
// does enforce is the second, narrower question the middleware cannot answer:
// a grant may never exceed the permissions of the person making it.
package skills

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Store is the persistence this service needs. It is an interface so the
// service can be tested without a database, and so the set of row operations
// the catalog depends on stays visible in one place.
type Store interface {
	UpsertSkillInstall(ctx context.Context, rec store.SkillInstallRecord) (store.SkillInstallRecord, error)
	GetSkillInstall(ctx context.Context, skillID, version string) (store.SkillInstallRecord, bool, error)
	ListSkillInstalls(ctx context.Context) ([]store.SkillInstallRecord, error)
	DeleteSkillInstall(ctx context.Context, skillID, version string) (bool, error)

	UpsertSkillActivation(ctx context.Context, rec store.SkillActivationRecord) (store.SkillActivationRecord, error)
	GetSkillActivation(ctx context.Context, projectID domain.ProjectID, skillID string) (store.SkillActivationRecord, bool, error)
	ListSkillActivationsForProject(ctx context.Context, projectID domain.ProjectID) ([]store.SkillActivationRecord, error)
	ListSkillActivationsForSkillVersion(ctx context.Context, skillID, version string) ([]store.SkillActivationRecord, error)
	DeleteSkillActivation(ctx context.Context, projectID domain.ProjectID, skillID string) (bool, error)

	AppendSkillAudit(ctx context.Context, entry store.SkillAuditEntry) error
	ListSkillAuditForSkill(ctx context.Context, skillID string) ([]store.SkillAuditEntry, error)
	ListSkillAuditForProject(ctx context.Context, projectID domain.ProjectID) ([]store.SkillAuditEntry, error)
}

// Service installs, activates and resolves skills.
type Service struct {
	store Store
	// root is the catalog directory, <dataDir>/skills/catalog. It is a sibling
	// of <dataDir>/skills/using-ao, which internal/skillassets clobbers on
	// every daemon boot; nothing here writes inside that path, so a boot can
	// never overwrite an installed package.
	root string
	now  func() time.Time
	// newID mints audit row ids; injectable so tests get stable output.
	newID func() string
	// runner is the execution environment AO would use. The service asks IT
	// what controls exist; there is deliberately no path for a caller to
	// supply an attestation, because a self-declared guarantee is exactly the
	// claim this design refuses as proof (ADR 0004).
	//
	// The default attests nothing, so every capability needing a control is
	// refused until a runner is wired in explicitly.
	runner skillcatalog.Runner
	// runnerUnavailable explains an environmental refusal ("no container
	// runtime on this host") as distinct from "no runner is configured".
	runnerUnavailable string
}

// Option configures a Service at construction.
type Option func(*Service)

// WithRunner supplies the execution environment. The service reads its
// attestation and never accepts one from a request.
func WithRunner(r skillcatalog.Runner, unavailable string) Option {
	return func(s *Service) {
		if r != nil {
			s.runner = r
		}
		s.runnerUnavailable = unavailable
	}
}

// New builds the service over a catalog rooted at dataDir. With no options it
// has no runner, so nothing can execute and every control-requiring capability
// is refused.
func New(st Store, dataDir string, opts ...Option) *Service {
	svc := &Service{
		store:  st,
		root:   skillcatalog.Dir(dataDir),
		now:    func() time.Time { return time.Now().UTC() },
		newID:  randomAuditID,
		runner: skillcatalog.UnavailableRunner{},
	}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Root is the catalog directory this service owns.
func (s *Service) Root() string { return s.root }

func randomAuditID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a condition an audit id can paper over.
		panic(fmt.Sprintf("skills: read random audit id: %v", err))
	}
	return "skaud-" + hex.EncodeToString(b[:])
}

// packageDir is where one installed version's files live. Versions sit side by
// side so a project pinned to an older one keeps working after a newer one is
// installed.
func (s *Service) packageDir(skillID, version string) string {
	return filepath.Join(s.root, "packages", skillID, version)
}

// InstallRequest installs a package from a local directory.
type InstallRequest struct {
	// SourceDir is a directory on this machine holding skill.yaml. It is the
	// only install source this phase supports: fetching a remote package means
	// deciding a trust root, and skillcatalog rejects the signature field
	// precisely because AO has not made that decision yet.
	SourceDir string
	// Actor is the principal performing the install, for the audit trail.
	Actor string
}

// Install validates a package, copies it into the catalog, records it and
// audits the install. It enables the skill on nothing: installing and running
// are separate acts, and a freshly installed skill is reachable by nothing.
//
// A rejected install is audited too. "Somebody tried to install a package that
// failed verification" is the entry an operator most wants to find later, and
// it is exactly the one a happy-path-only trail would be missing.
func (s *Service) Install(ctx context.Context, req InstallRequest) (store.SkillInstallRecord, error) {
	source := strings.TrimSpace(req.SourceDir)
	if source == "" {
		return store.SkillInstallRecord{}, apierr.Invalid("SKILL_SOURCE_REQUIRED",
			"a source directory is required", nil)
	}
	if !filepath.IsAbs(source) {
		return store.SkillInstallRecord{}, apierr.Invalid("SKILL_SOURCE_NOT_ABSOLUTE",
			"the source directory must be an absolute path", nil)
	}
	// Refuse to install the catalog into itself: the copy would race with the
	// read and the digest would describe a moving target.
	if withinRoot(s.root, source) {
		return store.SkillInstallRecord{}, apierr.Invalid("SKILL_SOURCE_INSIDE_CATALOG",
			"the source directory is inside AO's own skill catalog", nil)
	}

	pkg, err := skillcatalog.LoadPackage(source)
	if err != nil {
		s.auditRejectedInstall(ctx, req.Actor, source, err)
		return store.SkillInstallRecord{}, invalidPackage(err)
	}

	if existing, ok, err := s.store.GetSkillInstall(ctx, pkg.Manifest.ID, pkg.Manifest.Version); err != nil {
		return store.SkillInstallRecord{}, err
	} else if ok {
		// Re-installing the SAME bytes is idempotent; re-installing different
		// bytes under a version somebody already approved is not, because an
		// activation pins a version and would silently start meaning something
		// else.
		if existing.Digest != pkg.Digest {
			return store.SkillInstallRecord{}, apierr.Conflict("SKILL_VERSION_CONTENT_CHANGED",
				fmt.Sprintf("%s@%s is already installed with different contents; publish a new version",
					pkg.Manifest.ID, pkg.Manifest.Version), nil)
		}
	}

	dest := s.packageDir(pkg.Manifest.ID, pkg.Manifest.Version)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return store.SkillInstallRecord{}, fmt.Errorf("create package dir: %w", err)
	}
	if err := skillcatalog.CopyPackage(source, dest); err != nil {
		return store.SkillInstallRecord{}, err
	}
	// Re-verify from the INSTALLED copy, not the source: the digest that
	// matters is the one covering the bytes the catalog will actually read.
	installed, err := skillcatalog.LoadPackage(dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		s.auditRejectedInstall(ctx, req.Actor, source, err)
		return store.SkillInstallRecord{}, apierr.Internal("SKILL_INSTALL_VERIFICATION_FAILED",
			fmt.Sprintf("the installed copy failed verification: %v", err))
	}

	rec, err := s.store.UpsertSkillInstall(ctx, store.SkillInstallRecord{
		Manifest:    installed.Manifest,
		Digest:      installed.Digest,
		PackageDir:  dest,
		InstalledAt: s.now(),
		InstalledBy: req.Actor,
	})
	if err != nil {
		_ = os.RemoveAll(dest)
		return store.SkillInstallRecord{}, err
	}
	s.audit(ctx, store.SkillAuditEntry{
		Actor:   req.Actor,
		Action:  store.SkillAuditInstall,
		SkillID: rec.Manifest.ID,
		Version: rec.Manifest.Version,
		Digest:  rec.Digest,
		Detail:  "installed from " + source,
	})
	return rec, nil
}

// Uninstall removes one installed version. It refuses while any project still
// has that version enabled, so a project can never resolve to a skill whose
// files are gone. Disabled activation rows pinned to the removed version are
// dropped, since a later re-enable would otherwise point at absent files.
func (s *Service) Uninstall(ctx context.Context, skillID, version, actor string) error {
	rec, ok, err := s.store.GetSkillInstall(ctx, skillID, version)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.NotFound("SKILL_NOT_INSTALLED",
			fmt.Sprintf("%s@%s is not installed", skillID, version))
	}

	activations, err := s.store.ListSkillActivationsForSkillVersion(ctx, skillID, version)
	if err != nil {
		return err
	}
	var enabledFor []string
	for _, a := range activations {
		if a.Enabled {
			enabledFor = append(enabledFor, string(a.ProjectID))
		}
	}
	if len(enabledFor) > 0 {
		sort.Strings(enabledFor)
		return apierr.Conflict("SKILL_STILL_ENABLED",
			fmt.Sprintf("%s@%s is still enabled for %s; disable it there first",
				skillID, version, strings.Join(enabledFor, ", ")),
			map[string]any{"projects": enabledFor})
	}
	for _, a := range activations {
		if _, err := s.store.DeleteSkillActivation(ctx, a.ProjectID, a.SkillID); err != nil {
			return err
		}
	}
	if _, err := s.store.DeleteSkillInstall(ctx, skillID, version); err != nil {
		return err
	}
	if err := os.RemoveAll(rec.PackageDir); err != nil {
		return fmt.Errorf("remove package files: %w", err)
	}
	s.audit(ctx, store.SkillAuditEntry{
		Actor:   actor,
		Action:  store.SkillAuditUninstall,
		SkillID: skillID,
		Version: version,
		Digest:  rec.Digest,
	})
	return nil
}

// ListInstalledRecords returns every installed version, ordered by id then
// version. The controller-facing projection is ListInstalled in api.go.
func (s *Service) ListInstalledRecords(ctx context.Context) ([]store.SkillInstallRecord, error) {
	recs, err := s.store.ListSkillInstalls(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Manifest.ID != recs[j].Manifest.ID {
			return recs[i].Manifest.ID < recs[j].Manifest.ID
		}
		return compareVersions(recs[i].Manifest.Version, recs[j].Manifest.Version) < 0
	})
	return recs, nil
}

// InstalledRecord returns one installed version. The controller-facing
// projection is GetInstalled in api.go.
func (s *Service) InstalledRecord(ctx context.Context, skillID, version string) (store.SkillInstallRecord, error) {
	rec, ok, err := s.store.GetSkillInstall(ctx, skillID, version)
	if err != nil {
		return store.SkillInstallRecord{}, err
	}
	if !ok {
		return store.SkillInstallRecord{}, apierr.NotFound("SKILL_NOT_INSTALLED",
			fmt.Sprintf("%s@%s is not installed", skillID, version))
	}
	return rec, nil
}

// EnableRequest activates one installed skill on one project.
type EnableRequest struct {
	ProjectID domain.ProjectID
	SkillID   string
	// Version is required. There is no "latest" activation target: an install
	// must not silently change what a project already approved.
	Version string
	// Capabilities are what the approver is granting. Anything the manifest
	// requests but this omits is refused at run time.
	Capabilities []skillcatalog.Capability
	// Actor and ActorPermissions are the approver and what they may do on this
	// project. The permissions are resolved by the caller from AO's RBAC; this
	// service checks the grant against them rather than re-deriving authority.
	Actor            string
	ActorPermissions []domain.Permission
}

// Enable activates an installed skill on a project with an explicit grant.
//
// It never runs anything. Enabling records what a run WOULD be allowed to ask
// for; whether a run may proceed is decided separately, at plan time, against
// the runner that would carry it.
func (s *Service) Enable(ctx context.Context, req EnableRequest) (store.SkillActivationRecord, error) {
	if strings.TrimSpace(string(req.ProjectID)) == "" {
		return store.SkillActivationRecord{}, apierr.Invalid("PROJECT_REQUIRED", "a project id is required", nil)
	}
	if strings.TrimSpace(req.Version) == "" {
		return store.SkillActivationRecord{}, apierr.Invalid("SKILL_VERSION_REQUIRED",
			"a version is required; an activation is always pinned to one", nil)
	}
	if strings.TrimSpace(req.Actor) == "" {
		return store.SkillActivationRecord{}, apierr.Invalid("SKILL_APPROVER_REQUIRED",
			"an approver is required; an activation records who authorized it", nil)
	}

	rec, ok, err := s.store.GetSkillInstall(ctx, req.SkillID, req.Version)
	if err != nil {
		return store.SkillActivationRecord{}, err
	}
	if !ok {
		return store.SkillActivationRecord{}, apierr.NotFound("SKILL_NOT_INSTALLED",
			fmt.Sprintf("%s@%s is not installed", req.SkillID, req.Version))
	}
	// Verify the package on disk before granting anything against it. The
	// stored manifest is a convenience for listing; a grant is made against
	// the bytes that will actually be read.
	pkg, err := skillcatalog.LoadPackage(rec.PackageDir)
	if err != nil {
		return store.SkillActivationRecord{}, invalidPackage(err)
	}
	if pkg.Digest != rec.Digest {
		return store.SkillActivationRecord{}, apierr.Conflict("SKILL_PACKAGE_ALTERED",
			fmt.Sprintf("%s@%s on disk no longer matches the digest recorded at install",
				req.SkillID, req.Version), nil)
	}

	if err := skillcatalog.ValidateGrant(pkg.Manifest, skillcatalog.EnableRequest{
		ProjectID:          req.ProjectID,
		SkillID:            req.SkillID,
		Version:            req.Version,
		GrantCapabilities:  req.Capabilities,
		ApprovedBy:         req.Actor,
		SubjectPermissions: req.ActorPermissions,
	}); err != nil {
		return store.SkillActivationRecord{}, grantRefused(err)
	}

	previous, hadPrevious, err := s.store.GetSkillActivation(ctx, req.ProjectID, req.SkillID)
	if err != nil {
		return store.SkillActivationRecord{}, err
	}

	now := s.now()
	activation, err := s.store.UpsertSkillActivation(ctx, store.SkillActivationRecord{
		ProjectID: req.ProjectID,
		SkillID:   req.SkillID,
		Version:   req.Version,
		Enabled:   true,
		Grant: skillcatalog.Grant{
			Capabilities: append([]skillcatalog.Capability(nil), req.Capabilities...),
			ApprovedBy:   req.Actor,
			ApprovedAt:   now,
		},
		UpdatedAt: now,
	})
	if err != nil {
		return store.SkillActivationRecord{}, err
	}

	// An already-enabled skill whose grant or version changed is a different
	// event from a first activation: it is the one a reviewer reads to see
	// authority widening over time.
	action := store.SkillAuditEnable
	if hadPrevious && previous.Enabled {
		action = store.SkillAuditGrantChanged
	}
	projectID := req.ProjectID
	s.audit(ctx, store.SkillAuditEntry{
		Actor:        req.Actor,
		Action:       action,
		SkillID:      req.SkillID,
		Version:      req.Version,
		ProjectID:    &projectID,
		Digest:       rec.Digest,
		Capabilities: req.Capabilities,
	})
	return activation, nil
}

// Disable deactivates a skill on a project and drops its grant. The row is
// kept, so the history of what was once approved survives, but with no
// capabilities: re-enabling means granting again from scratch.
func (s *Service) Disable(ctx context.Context, projectID domain.ProjectID, skillID, actor string) error {
	existing, ok, err := s.store.GetSkillActivation(ctx, projectID, skillID)
	if err != nil {
		return err
	}
	if !ok {
		return apierr.NotFound("SKILL_NOT_ENABLED",
			fmt.Sprintf("%s is not enabled for %s", skillID, projectID))
	}
	now := s.now()
	if _, err := s.store.UpsertSkillActivation(ctx, store.SkillActivationRecord{
		ProjectID: projectID,
		SkillID:   skillID,
		Version:   existing.Version,
		Enabled:   false,
		Grant:     skillcatalog.Grant{},
		UpdatedAt: now,
	}); err != nil {
		return err
	}
	pid := projectID
	s.audit(ctx, store.SkillAuditEntry{
		Actor:     actor,
		Action:    store.SkillAuditDisable,
		SkillID:   skillID,
		Version:   existing.Version,
		ProjectID: &pid,
	})
	return nil
}

// ListForProject returns every activation row for a project, enabled or not,
// ordered by skill id.
func (s *Service) ListForProject(ctx context.Context, projectID domain.ProjectID) ([]store.SkillActivationRecord, error) {
	recs, err := s.store.ListSkillActivationsForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].SkillID < recs[j].SkillID })
	return recs, nil
}

// Resolve returns the skill a project has enabled, fail-closed at every step:
// not enabled, disabled, pinned to a version that is gone, or a package whose
// bytes no longer match its digest all produce an error rather than a usable
// result.
func (s *Service) Resolve(ctx context.Context, projectID domain.ProjectID, skillID string) (skillcatalog.Resolved, error) {
	activation, ok, err := s.store.GetSkillActivation(ctx, projectID, skillID)
	if err != nil {
		return skillcatalog.Resolved{}, err
	}
	if !ok {
		return skillcatalog.Resolved{}, apierr.NotFound("SKILL_NOT_ENABLED",
			fmt.Sprintf("%s is not enabled for %s", skillID, projectID))
	}
	if !activation.Enabled {
		return skillcatalog.Resolved{}, apierr.Invalid("SKILL_DISABLED",
			fmt.Sprintf("%s is disabled for %s", skillID, projectID), nil)
	}
	rec, ok, err := s.store.GetSkillInstall(ctx, skillID, activation.Version)
	if err != nil {
		return skillcatalog.Resolved{}, err
	}
	if !ok {
		return skillcatalog.Resolved{}, apierr.Conflict("SKILL_PINNED_VERSION_MISSING",
			fmt.Sprintf("%s on %s is pinned to %s, which is not installed",
				skillID, projectID, activation.Version), nil)
	}
	pkg, err := skillcatalog.LoadPackage(rec.PackageDir)
	if err != nil {
		return skillcatalog.Resolved{}, invalidPackage(err)
	}
	if pkg.Digest != rec.Digest {
		return skillcatalog.Resolved{}, apierr.Conflict("SKILL_PACKAGE_ALTERED",
			fmt.Sprintf("%s@%s on disk no longer matches the digest recorded at install",
				skillID, activation.Version), nil)
	}
	return skillcatalog.Resolved{
		Activation: skillcatalog.Activation{
			ProjectID: activation.ProjectID,
			SkillID:   activation.SkillID,
			Version:   activation.Version,
			Enabled:   activation.Enabled,
			Grant:     activation.Grant,
		},
		Package: pkg,
	}, nil
}

// AuditForSkill returns the catalog trail for one skill, newest first.
func (s *Service) AuditForSkill(ctx context.Context, skillID string) ([]store.SkillAuditEntry, error) {
	entries, err := s.store.ListSkillAuditForSkill(ctx, skillID)
	if err != nil {
		return nil, err
	}
	sortAuditNewestFirst(entries)
	return entries, nil
}

// AuditForProject returns the catalog trail for one project, newest first.
func (s *Service) AuditForProject(ctx context.Context, projectID domain.ProjectID) ([]store.SkillAuditEntry, error) {
	entries, err := s.store.ListSkillAuditForProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sortAuditNewestFirst(entries)
	return entries, nil
}

func sortAuditNewestFirst(entries []store.SkillAuditEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].OccurredAt.Equal(entries[j].OccurredAt) {
			return entries[i].OccurredAt.After(entries[j].OccurredAt)
		}
		return entries[i].ID < entries[j].ID
	})
}

// audit appends one trail entry. A failure to write the trail is logged into
// the returned error path nowhere: the operation it describes already
// succeeded, and failing the caller now would leave the catalog and its
// history disagreeing in the other direction. The write is best-effort by
// design and the store's own errors surface in the daemon log.
func (s *Service) audit(ctx context.Context, entry store.SkillAuditEntry) {
	entry.ID = s.newID()
	entry.OccurredAt = s.now()
	_ = s.store.AppendSkillAudit(ctx, entry)
}

func (s *Service) auditRejectedInstall(ctx context.Context, actor, source string, cause error) {
	s.audit(ctx, store.SkillAuditEntry{
		Actor:  actor,
		Action: store.SkillAuditInstallRejected,
		// The package failed verification, so its self-declared id is not
		// trustworthy enough to record as fact. The source path is.
		SkillID: "",
		Detail:  fmt.Sprintf("rejected %s: %v", source, cause),
	})
}

// invalidPackage maps skillcatalog's validation failures onto the API error
// envelope without losing the reason, which is the only useful part.
func invalidPackage(err error) error {
	if errors.Is(err, skillcatalog.ErrInvalidManifest) {
		return apierr.Invalid("SKILL_MANIFEST_INVALID", err.Error(), nil)
	}
	if errors.Is(err, os.ErrNotExist) {
		return apierr.NotFound("SKILL_PACKAGE_NOT_FOUND", err.Error())
	}
	return apierr.Invalid("SKILL_PACKAGE_INVALID", err.Error(), nil)
}

func grantRefused(err error) error {
	if errors.Is(err, skillcatalog.ErrInvalidManifest) {
		return apierr.Invalid("SKILL_MANIFEST_INVALID", err.Error(), nil)
	}
	return apierr.Forbidden("SKILL_GRANT_REFUSED", err.Error())
}

func compareVersions(a, b string) int {
	av, aErr := skillcatalog.ParseVersion(a)
	bv, bErr := skillcatalog.ParseVersion(b)
	if aErr != nil || bErr != nil {
		return strings.Compare(a, b)
	}
	return av.Compare(bv)
}

// withinRoot reports whether path is root or sits inside it.
func withinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
