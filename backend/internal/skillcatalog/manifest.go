package skillcatalog

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// APIVersion is the only skill-manifest contract this build understands. A
// manifest declaring anything else is rejected rather than best-effort parsed:
// a catalog that guesses at an unknown contract is a catalog that grants
// permissions it did not understand.
const APIVersion = "ao.skill/v1"

// ManifestFileName is the manifest's fixed name inside a skill package
// directory. It is fixed so the package digest can exclude exactly one file.
const ManifestFileName = "skill.yaml"

// ErrInvalidManifest wraps every validation failure so callers can classify a
// bad package without matching on message text.
var ErrInvalidManifest = errors.New("skillcatalog: invalid manifest")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
}

// RiskLevel is the declared blast radius of a skill or one of its modes. It is
// advisory to a human approver, never an input to the capability decision:
// nothing is allowed because it called itself low risk.
type RiskLevel string

// The four declared risk levels, in increasing severity.
const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// Valid reports whether r is one of the four declared levels.
func (r RiskLevel) Valid() bool {
	switch r {
	case RiskLow, RiskMedium, RiskHigh, RiskCritical:
		return true
	}
	return false
}

func (r RiskLevel) rank() int {
	switch r {
	case RiskLow:
		return 1
	case RiskMedium:
		return 2
	case RiskHigh:
		return 3
	case RiskCritical:
		return 4
	}
	return 0
}

// AtLeast reports whether r is at least as severe as other.
func (r RiskLevel) AtLeast(other RiskLevel) bool { return r.rank() >= other.rank() }

// ApprovalMode is how often a human must say yes before a run proceeds.
type ApprovalMode string

const (
	// ApprovalNone still requires the skill to be installed and explicitly
	// enabled for the project. "None" means no extra prompt per run, not
	// unattended by default.
	ApprovalNone ApprovalMode = "none"
	// ApprovalPerActivation asks once, when the skill is enabled on a project.
	ApprovalPerActivation ApprovalMode = "per_activation"
	// ApprovalPerRun asks every time the skill runs.
	ApprovalPerRun ApprovalMode = "per_run"
	// ApprovalPerTarget asks for each concrete target (a host, a repository, a
	// deployed environment) named in the run's inputs.
	ApprovalPerTarget ApprovalMode = "per_target"
)

// Valid reports whether a is a supported approval mode.
func (a ApprovalMode) Valid() bool {
	switch a {
	case ApprovalNone, ApprovalPerActivation, ApprovalPerRun, ApprovalPerTarget:
		return true
	}
	return false
}

func (a ApprovalMode) rank() int {
	switch a {
	case ApprovalNone:
		return 1
	case ApprovalPerActivation:
		return 2
	case ApprovalPerRun:
		return 3
	case ApprovalPerTarget:
		return 4
	}
	return 0
}

// StricterApproval returns the more demanding of two approval modes. Modes and
// capabilities each raise the bar; neither may lower it.
func StricterApproval(a, b ApprovalMode) ApprovalMode {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// OriginType records where a package came from. It is provenance metadata for
// a human reviewing an install, not a trust decision on its own.
type OriginType string

const (
	// OriginBuiltin ships inside the AO binary or repository.
	OriginBuiltin OriginType = "builtin"
	// OriginLocal was installed from a directory on this machine.
	OriginLocal OriginType = "local"
	// OriginGit was fetched from a git remote.
	OriginGit OriginType = "git"
)

// Valid reports whether o is a supported origin type.
func (o OriginType) Valid() bool {
	switch o {
	case OriginBuiltin, OriginLocal, OriginGit:
		return true
	}
	return false
}

// Origin is where the package came from.
type Origin struct {
	Type OriginType `yaml:"type" json:"type"`
	// Ref is the URL, path or revision the package was taken from. Required
	// for git and local origins, forbidden for builtin (which has no ref other
	// than the binary itself).
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// NetworkMode is the declared network posture of a skill.
type NetworkMode string

const (
	// NetworkNone is no outbound network at all.
	NetworkNone NetworkMode = "none"
	// NetworkAllowlist is outbound network limited to explicit host:port
	// entries. It is only meaningful with a runner that can actually enforce
	// it; declaring it does not create the restriction.
	NetworkAllowlist NetworkMode = "allowlist"
)

// Valid reports whether n is a supported network mode.
func (n NetworkMode) Valid() bool {
	switch n {
	case NetworkNone, NetworkAllowlist:
		return true
	}
	return false
}

// FileScope declares the paths a skill reads and writes, as repo-relative
// globs. Enforcement belongs to a runner; this is the declaration a reviewer
// and a future runner both read.
type FileScope struct {
	Read  []string `yaml:"read,omitempty" json:"read,omitempty"`
	Write []string `yaml:"write,omitempty" json:"write,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// RepoMode is how much of the project repository a skill sees.
type RepoMode string

const (
	// RepoNone is no repository access.
	RepoNone RepoMode = "none"
	// RepoWorktree is a read-only checkout of the project.
	RepoWorktree RepoMode = "worktree"
)

// Valid reports whether r is a supported repository mode.
func (r RepoMode) Valid() bool {
	switch r {
	case RepoNone, RepoWorktree:
		return true
	}
	return false
}

// RepoScope declares repository access.
type RepoScope struct {
	Mode RepoMode `yaml:"mode" json:"mode"`
}

// NetworkScope declares outbound network access.
type NetworkScope struct {
	Mode NetworkMode `yaml:"mode" json:"mode"`
	// Allow are host:port entries the skill always needs, only valid when
	// Mode is allowlist.
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	// PerRunTargets declares that further hosts are supplied per run rather
	// than fixed in the manifest — the shape a pentest skill genuinely has,
	// since its target is chosen by whoever authorizes the run. Each such
	// target still has to be authorized individually at run time; this field
	// only says the manifest's allowlist is deliberately incomplete rather
	// than accidentally empty.
	PerRunTargets bool `yaml:"perRunTargets,omitempty" json:"perRunTargets,omitempty"`
}

// SecretScope names the secrets a skill asks for. Names only: a manifest that
// carried a value would put a credential in a file meant to be shared, read
// and diffed, so a value-shaped entry is rejected outright.
type SecretScope struct {
	Requested []string `yaml:"requested,omitempty" json:"requested,omitempty"`
}

// Scope is the full declared reach of a skill.
type Scope struct {
	Files   FileScope    `yaml:"files,omitempty" json:"files,omitempty"`
	Repos   RepoScope    `yaml:"repos" json:"repos"`
	Network NetworkScope `yaml:"network" json:"network"`
	Secrets SecretScope  `yaml:"secrets,omitempty" json:"secrets,omitempty"`
}

// ToolPolicy is the agent tool allow/deny list a launch should apply. It maps
// onto ports.LaunchConfig.AllowedTools / DisallowedTools, which is a real
// enforcement point at the agent CLI — but only when the launch is NOT in
// bypass permission mode, because bypass ignores both lists. The runner phase
// owns that guarantee; the manifest only states the intent.
type ToolPolicy struct {
	Allowed []string `yaml:"allowed,omitempty" json:"allowed,omitempty"`
	Denied  []string `yaml:"denied,omitempty" json:"denied,omitempty"`
}

// InputParam is one declared parameter of a skill run.
type InputParam struct {
	Name        string   `yaml:"name" json:"name"`
	Type        string   `yaml:"type" json:"type"`
	Required    bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Enum        []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	Description string   `yaml:"description" json:"description"`
}

var inputTypes = map[string]bool{
	"string": true, "bool": true, "int": true, "enum": true, "path": true, "list": true,
}

// OutputSchema names the machine-readable shape a run must produce. The schema
// file itself lives in the package so a consumer can validate a report without
// trusting the producer's prose.
type OutputSchema struct {
	Format string `yaml:"format" json:"format"`
	// SchemaRef is a package-relative path to the JSON Schema document.
	SchemaRef string `yaml:"schemaRef" json:"schemaRef"`
}

// Authorization is what AO must confirm before this skill may be activated or
// run. RequiredPermissions are AO's own RBAC permissions, so activation reuses
// domain.Permission instead of inventing a second permission vocabulary.
type Authorization struct {
	RequiredPermissions []string     `yaml:"requiredPermissions" json:"requiredPermissions"`
	Approval            ApprovalMode `yaml:"approval" json:"approval"`
	// RequiresIsolatedRunner declares that this skill must not run in the
	// daemon's own process or an ordinary worker worktree. Setting it false
	// does not make a skill safe; the capability table raises this
	// independently and a manifest cannot lower it.
	RequiresIsolatedRunner bool `yaml:"requiresIsolatedRunner,omitempty" json:"requiresIsolatedRunner,omitempty"`
}

// Compatibility bounds the AO versions a package supports.
type Compatibility struct {
	AOMinVersion string `yaml:"aoMinVersion" json:"aoMinVersion"`
	AOMaxVersion string `yaml:"aoMaxVersion,omitempty" json:"aoMaxVersion,omitempty"`
}

// Integrity is the package content digest, covering every file in the package
// except the manifest itself (which carries the digest and so cannot cover it).
type Integrity struct {
	Algorithm string `yaml:"algorithm" json:"algorithm"`
	Digest    string `yaml:"digest" json:"digest"`
}

// Provenance is who published the package.
//
// Signature is parsed but MUST be empty in v1: AO has no signature
// verification, and accepting a signature field it cannot check would let a
// package advertise an assurance nobody validated. Rejecting is the
// fail-closed answer until verification exists.
type Provenance struct {
	Publisher string `yaml:"publisher" json:"publisher"`
	SourceURL string `yaml:"sourceUrl,omitempty" json:"sourceUrl,omitempty"`
	Signature string `yaml:"signature,omitempty" json:"signature,omitempty"`
}

// UpdatePolicy is how a newer version of an installed skill is adopted.
type UpdatePolicy string

const (
	// UpdateManual means a newer version can be installed but never replaces
	// the version a project is pinned to without an explicit re-activation.
	UpdateManual UpdatePolicy = "manual"
	// UpdatePinned means the project's version never changes until a human
	// changes it, and the catalog will not even offer an upgrade.
	UpdatePinned UpdatePolicy = "pinned"
)

// Valid reports whether u is a supported update policy.
func (u UpdatePolicy) Valid() bool {
	switch u {
	case UpdateManual, UpdatePinned:
		return true
	}
	return false
}

// Policy is the install/update/deactivation contract.
//
// There is deliberately no "auto" update mode and no auto-enable: installing a
// skill must never be the same act as running it, and a background upgrade
// would silently change what an approved activation actually does.
type Policy struct {
	Update UpdatePolicy `yaml:"update" json:"update"`
	// AutoEnable must be false. It exists as a named, rejected field so a
	// package author who tries it gets an explicit error instead of believing
	// an ignored key worked.
	AutoEnable bool `yaml:"autoEnable,omitempty" json:"autoEnable,omitempty"`
	// DeactivateRevokesGrants must be true: disabling a skill on a project
	// drops its capability grants rather than parking them for silent reuse.
	DeactivateRevokesGrants bool `yaml:"deactivateRevokesGrants" json:"deactivateRevokesGrants"`
}

// Mode is one selectable operating mode of a skill. A mode may request a
// subset of the skill's capabilities and may raise (never lower) the approval
// bar, which is how one package can offer both a read-only code review and an
// active scan without the read-only path inheriting the scan's permissions.
type Mode struct {
	ID           string       `yaml:"id" json:"id"`
	Name         string       `yaml:"name" json:"name"`
	Description  string       `yaml:"description" json:"description"`
	RiskLevel    RiskLevel    `yaml:"riskLevel" json:"riskLevel"`
	Capabilities []Capability `yaml:"capabilities" json:"capabilities"`
	Approval     ApprovalMode `yaml:"approval" json:"approval"`
	// Guide is a package-relative Markdown file with the mode's instructions.
	Guide string `yaml:"guide" json:"guide"`
}

// Manifest is the versioned skill contract.
type Manifest struct {
	APIVersion    string        `yaml:"apiVersion" json:"apiVersion"`
	ID            string        `yaml:"id" json:"id"`
	Name          string        `yaml:"name" json:"name"`
	Version       string        `yaml:"version" json:"version"`
	Description   string        `yaml:"description" json:"description"`
	Origin        Origin        `yaml:"origin" json:"origin"`
	RiskLevel     RiskLevel     `yaml:"riskLevel" json:"riskLevel"`
	Capabilities  []Capability  `yaml:"capabilities" json:"capabilities"`
	Tools         ToolPolicy    `yaml:"tools,omitempty" json:"tools,omitempty"`
	Scope         Scope         `yaml:"scope" json:"scope"`
	Inputs        []InputParam  `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs       OutputSchema  `yaml:"outputs" json:"outputs"`
	Authorization Authorization `yaml:"authorization" json:"authorization"`
	Compatibility Compatibility `yaml:"compatibility" json:"compatibility"`
	Integrity     Integrity     `yaml:"integrity" json:"integrity"`
	Provenance    Provenance    `yaml:"provenance" json:"provenance"`
	Policy        Policy        `yaml:"policy" json:"policy"`
	Modes         []Mode        `yaml:"modes" json:"modes"`
}

// reservedIDs are names skillcatalog must not own. using-ao belongs to
// internal/skillassets, which clobbers its directory on every daemon boot; a
// catalog entry by that name would be silently overwritten.
var reservedIDs = map[string]bool{
	"using-ao": true,
	"catalog":  true,
	"registry": true,
}

var (
	idRe      = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	digestRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?:\d{1,5}$`)
	secretRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	modeIDRe  = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	pkgPathRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
)

// Validate enforces the whole contract. It is strict on purpose: every branch
// here is a case where accepting the manifest would mean acting on a
// declaration nobody could interpret unambiguously.
func (m Manifest) Validate() error {
	if m.APIVersion != APIVersion {
		return invalidf("apiVersion %q is not supported (want %q)", m.APIVersion, APIVersion)
	}
	if err := validateID(m.ID); err != nil {
		return err
	}
	if strings.TrimSpace(m.Name) == "" {
		return invalidf("name is required")
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return err
	}
	if strings.TrimSpace(m.Description) == "" {
		return invalidf("description is required")
	}
	if err := m.Origin.validate(); err != nil {
		return err
	}
	if !m.RiskLevel.Valid() {
		return invalidf("riskLevel %q is not one of low, medium, high, critical", m.RiskLevel)
	}
	if err := validateCapabilityList("capabilities", m.Capabilities); err != nil {
		return err
	}
	if err := m.Scope.validate(); err != nil {
		return err
	}
	if err := m.validateScopeAgainstCapabilities(); err != nil {
		return err
	}
	if err := validateInputs(m.Inputs); err != nil {
		return err
	}
	if err := m.Outputs.validate(); err != nil {
		return err
	}
	if err := m.Authorization.validate(); err != nil {
		return err
	}
	if err := m.Compatibility.validate(); err != nil {
		return err
	}
	if err := m.Integrity.validate(); err != nil {
		return err
	}
	if err := m.Provenance.validate(); err != nil {
		return err
	}
	if err := m.Policy.validate(); err != nil {
		return err
	}
	return m.validateModes()
}

func validateID(id string) error {
	if !idRe.MatchString(id) {
		return invalidf("id %q must be lowercase kebab-case", id)
	}
	if len(id) > 64 {
		return invalidf("id %q is longer than 64 characters", id)
	}
	if reservedIDs[id] {
		return invalidf("id %q is reserved by AO", id)
	}
	return nil
}

func (o Origin) validate() error {
	if !o.Type.Valid() {
		return invalidf("origin.type %q is not one of builtin, local, git", o.Type)
	}
	ref := strings.TrimSpace(o.Ref)
	if o.Type == OriginBuiltin && ref != "" {
		return invalidf("origin.ref must be empty for a builtin origin")
	}
	if o.Type != OriginBuiltin && ref == "" {
		return invalidf("origin.ref is required for a %s origin", o.Type)
	}
	return nil
}

func (s Scope) validate() error {
	for _, g := range append(append([]string{}, s.Files.Read...), append(s.Files.Write, s.Files.Deny...)...) {
		if strings.TrimSpace(g) == "" {
			return invalidf("scope.files entries must not be empty")
		}
		if strings.HasPrefix(g, "/") || strings.Contains(g, "..") {
			return invalidf("scope.files entry %q must be repo-relative and must not traverse upward", g)
		}
	}
	if !s.Repos.Mode.Valid() {
		return invalidf("scope.repos.mode %q is not one of none, worktree", s.Repos.Mode)
	}
	if !s.Network.Mode.Valid() {
		return invalidf("scope.network.mode %q is not one of none, allowlist", s.Network.Mode)
	}
	if s.Network.Mode == NetworkNone && len(s.Network.Allow) > 0 {
		return invalidf("scope.network.allow must be empty when mode is none")
	}
	if s.Network.Mode == NetworkNone && s.Network.PerRunTargets {
		return invalidf("scope.network.perRunTargets requires mode allowlist")
	}
	if s.Network.Mode == NetworkAllowlist && len(s.Network.Allow) == 0 && !s.Network.PerRunTargets {
		return invalidf("scope.network.allow must list at least one host:port when mode is allowlist, " +
			"or set perRunTargets to declare that targets are authorized per run")
	}
	for _, entry := range s.Network.Allow {
		if !hostRe.MatchString(entry) {
			return invalidf("scope.network.allow entry %q must be host:port", entry)
		}
	}
	for _, name := range s.Secrets.Requested {
		if !secretRe.MatchString(name) {
			return invalidf("scope.secrets.requested entry %q must be an UPPER_SNAKE_CASE name, never a value", name)
		}
	}
	return nil
}

// validateScopeAgainstCapabilities rejects the two ways a manifest can be
// internally inconsistent: asking for reach it never declared a capability
// for, and declaring a capability it then gives no reach to use.
func (m Manifest) validateScopeAgainstCapabilities() error {
	has := func(c Capability) bool {
		for _, got := range m.Capabilities {
			if got == c {
				return true
			}
		}
		return false
	}
	if m.Scope.Network.Mode == NetworkAllowlist && !has(CapNetEgress) {
		return invalidf("scope.network.mode allowlist requires the %q capability", CapNetEgress)
	}
	if m.Scope.Network.Mode == NetworkNone && has(CapNetEgress) {
		return invalidf("capability %q requires scope.network.mode allowlist", CapNetEgress)
	}
	if has(CapNetActiveScan) && !has(CapNetEgress) {
		return invalidf("capability %q requires %q", CapNetActiveScan, CapNetEgress)
	}
	if has(CapNetActiveScan) && !m.Scope.Network.PerRunTargets {
		return invalidf("capability %q requires scope.network.perRunTargets: an active scan's target "+
			"is chosen and authorized per run, never fixed in the manifest", CapNetActiveScan)
	}
	if m.Scope.Repos.Mode == RepoNone && (has(CapRepoRead) || has(CapRepoWrite)) {
		return invalidf("repository capabilities require scope.repos.mode worktree")
	}
	if len(m.Scope.Secrets.Requested) > 0 && !has(CapSecretsRead) {
		return invalidf("scope.secrets.requested requires the %q capability", CapSecretsRead)
	}
	if has(CapSecretsRead) && len(m.Scope.Secrets.Requested) == 0 {
		return invalidf("capability %q requires at least one scope.secrets.requested name", CapSecretsRead)
	}
	if len(m.Scope.Files.Write) > 0 && !has(CapRepoWrite) && !has(CapReportWrite) {
		return invalidf("scope.files.write requires %q or %q", CapRepoWrite, CapReportWrite)
	}
	return nil
}

func validateInputs(inputs []InputParam) error {
	seen := map[string]bool{}
	for i, in := range inputs {
		if strings.TrimSpace(in.Name) == "" {
			return invalidf("inputs[%d].name is required", i)
		}
		if seen[in.Name] {
			return invalidf("inputs declares %q twice", in.Name)
		}
		seen[in.Name] = true
		if !inputTypes[in.Type] {
			return invalidf("inputs[%q].type %q is not a supported type", in.Name, in.Type)
		}
		if in.Type == "enum" && len(in.Enum) == 0 {
			return invalidf("inputs[%q] is an enum but lists no values", in.Name)
		}
		if in.Type != "enum" && len(in.Enum) > 0 {
			return invalidf("inputs[%q] lists enum values but is type %q", in.Name, in.Type)
		}
		if strings.TrimSpace(in.Description) == "" {
			return invalidf("inputs[%q].description is required", in.Name)
		}
	}
	return nil
}

func (o OutputSchema) validate() error {
	if o.Format != "json" {
		return invalidf("outputs.format %q is not supported (want json)", o.Format)
	}
	if err := validatePackagePath("outputs.schemaRef", o.SchemaRef); err != nil {
		return err
	}
	return nil
}

func validatePackagePath(field, p string) error {
	if strings.TrimSpace(p) == "" {
		return invalidf("%s is required", field)
	}
	if !pkgPathRe.MatchString(p) {
		return invalidf("%s %q must be a package-relative path", field, p)
	}
	if strings.Contains(p, "..") {
		return invalidf("%s %q must not traverse upward", field, p)
	}
	return nil
}

func (a Authorization) validate() error {
	if len(a.RequiredPermissions) == 0 {
		return invalidf("authorization.requiredPermissions must name at least one AO permission")
	}
	known := map[string]bool{}
	for _, p := range domain.AllPermissions {
		known[string(p)] = true
	}
	seen := map[string]bool{}
	for _, p := range a.RequiredPermissions {
		if !known[p] {
			return invalidf("authorization.requiredPermissions %q is not an AO permission", p)
		}
		if seen[p] {
			return invalidf("authorization.requiredPermissions lists %q twice", p)
		}
		seen[p] = true
	}
	if !a.Approval.Valid() {
		return invalidf("authorization.approval %q is not a supported mode", a.Approval)
	}
	return nil
}

func (c Compatibility) validate() error {
	minV, err := ParseVersion(c.AOMinVersion)
	if err != nil {
		return invalidf("compatibility.aoMinVersion: %v", err)
	}
	if strings.TrimSpace(c.AOMaxVersion) == "" {
		return nil
	}
	maxV, err := ParseVersion(c.AOMaxVersion)
	if err != nil {
		return invalidf("compatibility.aoMaxVersion: %v", err)
	}
	if maxV.Compare(minV) < 0 {
		return invalidf("compatibility.aoMaxVersion %s is below aoMinVersion %s", c.AOMaxVersion, c.AOMinVersion)
	}
	return nil
}

func (i Integrity) validate() error {
	if i.Algorithm != "sha256" {
		return invalidf("integrity.algorithm %q is not supported (want sha256)", i.Algorithm)
	}
	if !digestRe.MatchString(i.Digest) {
		return invalidf("integrity.digest must be 64 lowercase hex characters")
	}
	return nil
}

func (p Provenance) validate() error {
	if strings.TrimSpace(p.Publisher) == "" {
		return invalidf("provenance.publisher is required")
	}
	if strings.TrimSpace(p.Signature) != "" {
		return invalidf("provenance.signature is set but AO cannot verify signatures yet; " +
			"remove it rather than advertise an assurance nothing checks")
	}
	return nil
}

func (p Policy) validate() error {
	if !p.Update.Valid() {
		return invalidf("policy.update %q is not one of manual, pinned", p.Update)
	}
	if p.AutoEnable {
		return invalidf("policy.autoEnable must be false: installing a skill never enables it on a project")
	}
	if !p.DeactivateRevokesGrants {
		return invalidf("policy.deactivateRevokesGrants must be true: disabling a skill revokes its grants")
	}
	return nil
}

func (m Manifest) validateModes() error {
	if len(m.Modes) == 0 {
		return invalidf("modes must declare at least one mode")
	}
	declared := map[Capability]bool{}
	for _, c := range m.Capabilities {
		declared[c] = true
	}
	seen := map[string]bool{}
	for i, mode := range m.Modes {
		if !modeIDRe.MatchString(mode.ID) {
			return invalidf("modes[%d].id %q must be lowercase kebab-case", i, mode.ID)
		}
		if seen[mode.ID] {
			return invalidf("modes declares %q twice", mode.ID)
		}
		seen[mode.ID] = true
		if strings.TrimSpace(mode.Name) == "" {
			return invalidf("modes[%q].name is required", mode.ID)
		}
		if strings.TrimSpace(mode.Description) == "" {
			return invalidf("modes[%q].description is required", mode.ID)
		}
		if !mode.RiskLevel.Valid() {
			return invalidf("modes[%q].riskLevel %q is invalid", mode.ID, mode.RiskLevel)
		}
		if mode.RiskLevel.rank() > m.RiskLevel.rank() {
			return invalidf("modes[%q].riskLevel %s exceeds the skill riskLevel %s", mode.ID, mode.RiskLevel, m.RiskLevel)
		}
		if err := validateCapabilityList(fmt.Sprintf("modes[%q].capabilities", mode.ID), mode.Capabilities); err != nil {
			return err
		}
		for _, c := range mode.Capabilities {
			if !declared[c] {
				return invalidf("modes[%q] requests %q which the skill does not declare", mode.ID, c)
			}
		}
		if !mode.Approval.Valid() {
			return invalidf("modes[%q].approval %q is not a supported mode", mode.ID, mode.Approval)
		}
		if mode.Approval.rank() < m.Authorization.Approval.rank() {
			return invalidf("modes[%q].approval %s is weaker than the skill approval %s; a mode may only raise the bar",
				mode.ID, mode.Approval, m.Authorization.Approval)
		}
		if err := validatePackagePath(fmt.Sprintf("modes[%q].guide", mode.ID), mode.Guide); err != nil {
			return err
		}
	}
	return nil
}

// Mode returns the named mode.
func (m Manifest) Mode(id string) (Mode, bool) {
	for _, mode := range m.Modes {
		if mode.ID == id {
			return mode, true
		}
	}
	return Mode{}, false
}

// Version is a parsed MAJOR.MINOR.PATCH version with an optional prerelease
// suffix. AO has no semver dependency and this package is a leaf, so the
// catalog carries the small comparison it actually needs rather than pulling
// one in.
type Version struct {
	Major, Minor, Patch int
	Prerelease          string
}

var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?$`)

// ParseVersion parses a strict MAJOR.MINOR.PATCH[-prerelease] version.
func ParseVersion(s string) (Version, error) {
	match := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if match == nil {
		return Version{}, invalidf("version %q is not MAJOR.MINOR.PATCH[-prerelease]", s)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	return Version{Major: major, Minor: minor, Patch: patch, Prerelease: match[4]}, nil
}

// String renders the version back to its canonical form.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// Compare orders two versions: negative if v is older, 0 if equal, positive if
// newer. A prerelease sorts before the same release, per semver.
func (v Version) Compare(other Version) int {
	if d := v.Major - other.Major; d != 0 {
		return d
	}
	if d := v.Minor - other.Minor; d != 0 {
		return d
	}
	if d := v.Patch - other.Patch; d != 0 {
		return d
	}
	switch {
	case v.Prerelease == other.Prerelease:
		return 0
	case v.Prerelease == "":
		return 1
	case other.Prerelease == "":
		return -1
	}
	return strings.Compare(v.Prerelease, other.Prerelease)
}
