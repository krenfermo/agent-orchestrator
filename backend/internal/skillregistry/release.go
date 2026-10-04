package skillregistry

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillcatalog"
)

// ErrInvalidRelease wraps every release-validation failure so callers can
// classify a malformed registry entry without matching on message text.
var ErrInvalidRelease = errors.New("skillregistry: invalid release")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRelease, fmt.Sprintf(format, args...))
}

// TrustState is how much AO can honestly say about a release. The four values
// are ordered by how much was actually checked, not by how good the release
// looks.
type TrustState string

const (
	// TrustRevoked means the registry declared this exact release withdrawn.
	// It blocks new installs and never removes an existing one.
	TrustRevoked TrustState = "revoked"
	// TrustUnverified means nothing has been hashed yet, or nothing beyond the
	// provider's own description backs the release. Every search result is
	// unverified: a search moves no bytes, so there is nothing to have checked.
	TrustUnverified TrustState = "unverified"
	// TrustVerified means AO fetched the bytes and computed, itself, that the
	// manifest digest and the artifact digest both equal what the pinned
	// release declared.
	//
	// It does NOT mean AO knows who produced them. A registry that lies about
	// a digest is caught here; one that honestly serves malicious code under a
	// correct digest is not.
	TrustVerified TrustState = "verified"
	// TrustTrusted means AO verified a signature over a canonical description
	// of these exact bytes, made by a key that chains to a trust root this
	// installation configured, held by the publisher the release names.
	//
	// As of phase 12 it is REACHABLE. What it does not mean is unchanged and
	// is the part worth repeating: it says who signed, never that the code is
	// safe, that it has no vulnerabilities, that its capabilities are benign
	// or that anybody read it. See ADR 0008.
	TrustTrusted TrustState = "trusted"
)

// Valid reports whether t is one of the four states.
func (t TrustState) Valid() bool {
	switch t {
	case TrustRevoked, TrustUnverified, TrustVerified, TrustTrusted:
		return true
	}
	return false
}

// IntegrityChecked reports whether AO itself hashed the bytes behind this
// state. It is the question a UI should ask instead of comparing to a string,
// and it is false for unverified and revoked.
func (t TrustState) IntegrityChecked() bool {
	return t == TrustVerified || t == TrustTrusted
}

// Provenance is what a release SAYS about its own origin, in the pre-signature
// vocabulary phase 10 defined.
//
// Every field here is still an uninterpreted claim: SignatureFormat and
// Signature are free-form strings from some other ecosystem (a cosign bundle,
// a minisign line) that AO records and never checks. The field AO DOES check
// is Release.Signature -- a ReleaseSignature in AO's own scheme -- and the two
// are deliberately separate types so that a cosign blob in this struct can
// never be mistaken for something that was verified.
type Provenance struct {
	// SignatureFormat names the scheme, e.g. "cosign" or "minisign". Empty
	// means the release declares no signature at all.
	SignatureFormat string `json:"signatureFormat,omitempty"`
	// Signature is the detached signature material, recorded verbatim and
	// never interpreted. It is kept so that an installation which later gains
	// verification can re-check what was installed under the old rules.
	Signature string `json:"signature,omitempty"`
	// KeyID identifies the signing key or certificate identity the publisher
	// claims. A claim, not a fact.
	KeyID string `json:"keyId,omitempty"`
	// AttestationURL points at an external attestation (an in-toto statement,
	// a transparency-log entry). AO fetches nothing from it; it is a link for
	// a human reviewing an install.
	AttestationURL string `json:"attestationUrl,omitempty"`
}

// Declared reports whether the release claims any provenance at all.
func (p Provenance) Declared() bool {
	return strings.TrimSpace(p.SignatureFormat) != "" || strings.TrimSpace(p.Signature) != ""
}

// ReleaseMode is one execution mode the release advertises. It mirrors the
// manifest's mode list so a person can see what a package offers WITHOUT
// installing it -- which is the entire point of a marketplace listing, and the
// reason this is a copy of the publisher's claim rather than a read of a
// manifest AO holds.
type ReleaseMode struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	RiskLevel    string   `json:"riskLevel"`
	Capabilities []string `json:"capabilities"`
}

// Compatibility bounds the AO versions a release supports. It is the same pair
// the manifest carries, restated here because a listing has to be readable
// before there is a manifest on this host to read.
type Compatibility struct {
	AOMinVersion string `json:"aoMinVersion"`
	AOMaxVersion string `json:"aoMaxVersion,omitempty"`
}

// Release is one immutable, exactly-identified thing a registry offers.
//
// Every field here is the REGISTRY's claim. Nothing in it is trusted on
// arrival: the digests are what AO will check the bytes against, the publisher
// is what AO will check the manifest against, and the capabilities are what AO
// will refuse an install for if the manifest asks for more.
type Release struct {
	// RegistryID is the configured registry this came from. AO sets it; a
	// provider payload that carried its own would let a registry claim to be
	// another one.
	RegistryID string `json:"registryId"`

	SkillID     string `json:"skillId"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Publisher   string `json:"publisher"`
	Description string `json:"description"`
	RiskLevel   string `json:"riskLevel"`

	// SourceURL is where the publisher says the source lives. Reference
	// metadata for a human; AO fetches nothing from it.
	SourceURL string `json:"sourceUrl,omitempty"`
	// ChangelogURL is the same kind of thing for what changed.
	ChangelogURL string `json:"changelogUrl,omitempty"`
	// Changelog is a short inline summary, when the registry carries one.
	Changelog string `json:"changelog,omitempty"`

	// ManifestDigest is sha256 over the release's skill.yaml bytes.
	//
	// It is separate from ArtifactDigest because skillcatalog's package digest
	// deliberately EXCLUDES the manifest (which carries that digest and so
	// cannot cover it). Two digests is what it takes to cover every byte.
	ManifestDigest string `json:"manifestDigest"`
	// ArtifactDigest is the package-content digest, computed exactly as
	// skillcatalog.ComputePackageDigest computes it, over every file except
	// the manifest.
	ArtifactDigest string `json:"artifactDigest"`

	Provenance Provenance `json:"provenance"`

	// Source is where the bytes physically live when the registry is an
	// EXTERNAL one -- a repository, a tag, and above all a commit SHA.
	//
	// It is empty for a local directory and for a private HTTPS registry,
	// which serve one immutable artifact per version and have nothing a commit
	// would add. It is REQUIRED for an external registry, because there the
	// version string is a label the publisher chose and the commit is the only
	// thing that identifies one exact tree.
	//
	// It is deliberately NOT covered by the release signature. See
	// SigningPayload: the signature already covers both digests, and the
	// digests are what the fetched bytes are checked against -- so a signature
	// that also covered the commit would add nothing to the integrity claim
	// while making every existing signature unverifiable and forcing a
	// publisher to know the commit before signing. Where the bytes were
	// FETCHED FROM is AO's own observation, recorded in provenance; whether
	// they are the right bytes is the digests' job.
	Source GitSource `json:"source,omitzero"`

	// Signature is the detached AO signature over this release, when the
	// registry serves one. Absent means unsigned, which is a legitimate state
	// for a digest-policy registry and a refusal under signed or official.
	//
	// It is DETACHED and outside the package because ManifestDigest is one of
	// the things it covers: a signature inside the manifest would have to
	// cover itself.
	Signature ReleaseSignature `json:"signature,omitzero"`

	// RequestedCapabilities is the union the package declares. AO compares it
	// against the installed manifest and refuses a mismatch in EITHER
	// direction: understating is a lie, and overstating means the listing a
	// person approved described a different package.
	RequestedCapabilities []string `json:"requestedCapabilities"`

	ExecutionModes []ReleaseMode `json:"executionModes"`
	Compatibility  Compatibility `json:"compatibility"`

	PublishedAt time.Time `json:"publishedAt"`

	// Deprecated is "there is something better"; Revoked is "do not install
	// this". They are separate because a deprecated release is still safe to
	// keep running and a revoked one is a decision waiting for a human.
	Deprecated       bool   `json:"deprecated,omitempty"`
	DeprecationNote  string `json:"deprecationNote,omitempty"`
	Revoked          bool   `json:"revoked,omitempty"`
	RevocationReason string `json:"revocationReason,omitempty"`
}

var (
	idRe        = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	digestRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	publisherRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._@:/-]{0,127}$`)
)

// Validate enforces the release contract. It is strict for the same reason
// skillcatalog.Manifest.Validate is: a field AO cannot interpret unambiguously
// is a field it would end up acting on with a guess.
func (r Release) Validate() error {
	if !idRe.MatchString(r.SkillID) {
		return invalidf("skillId %q must be lowercase kebab-case", r.SkillID)
	}
	if len(r.SkillID) > 64 {
		return invalidf("skillId %q is longer than 64 characters", r.SkillID)
	}
	if strings.TrimSpace(r.Name) == "" {
		return invalidf("name is required")
	}
	if _, err := skillcatalog.ParseVersion(r.Version); err != nil {
		return invalidf("version %q is not MAJOR.MINOR.PATCH[-prerelease]", r.Version)
	}
	if !publisherRe.MatchString(r.Publisher) {
		return invalidf("publisher %q is required and must be a plain identifier", r.Publisher)
	}
	if strings.TrimSpace(r.Description) == "" {
		return invalidf("description is required")
	}
	if !skillcatalog.RiskLevel(r.RiskLevel).Valid() {
		return invalidf("riskLevel %q is not one of low, medium, high, critical", r.RiskLevel)
	}
	if !digestRe.MatchString(r.ManifestDigest) {
		return invalidf("manifestDigest must be 64 lowercase hex characters")
	}
	if !digestRe.MatchString(r.ArtifactDigest) {
		return invalidf("artifactDigest must be 64 lowercase hex characters")
	}
	if len(r.RequestedCapabilities) == 0 {
		return invalidf("requestedCapabilities must name at least one capability")
	}
	seenCap := map[string]bool{}
	for _, c := range r.RequestedCapabilities {
		if _, ok := skillcatalog.Capability(c).Spec(); !ok {
			return invalidf("requestedCapabilities %q is not an AO capability", c)
		}
		if seenCap[c] {
			return invalidf("requestedCapabilities lists %q twice", c)
		}
		seenCap[c] = true
	}
	if len(r.ExecutionModes) == 0 {
		return invalidf("executionModes must declare at least one mode")
	}
	seenMode := map[string]bool{}
	for i, m := range r.ExecutionModes {
		if !idRe.MatchString(m.ID) {
			return invalidf("executionModes[%d].id %q must be lowercase kebab-case", i, m.ID)
		}
		if seenMode[m.ID] {
			return invalidf("executionModes declares %q twice", m.ID)
		}
		seenMode[m.ID] = true
		if !skillcatalog.RiskLevel(m.RiskLevel).Valid() {
			return invalidf("executionModes[%q].riskLevel %q is invalid", m.ID, m.RiskLevel)
		}
		for _, c := range m.Capabilities {
			if !seenCap[c] {
				return invalidf("executionModes[%q] requests %q which the release does not declare", m.ID, c)
			}
		}
	}
	if _, err := skillcatalog.ParseVersion(r.Compatibility.AOMinVersion); err != nil {
		return invalidf("compatibility.aoMinVersion: %v", err)
	}
	if maxV := strings.TrimSpace(r.Compatibility.AOMaxVersion); maxV != "" {
		if _, err := skillcatalog.ParseVersion(maxV); err != nil {
			return invalidf("compatibility.aoMaxVersion: %v", err)
		}
	}
	if r.PublishedAt.IsZero() {
		return invalidf("publishedAt is required")
	}
	if r.Revoked && strings.TrimSpace(r.RevocationReason) == "" {
		return invalidf("a revoked release must say why; one that does not is indistinguishable from a mistake")
	}
	// A source is optional -- most releases have none -- and one that is
	// PRESENT and malformed is refused rather than ignored, because the fields
	// in it become path segments in requests AO builds.
	if r.Source.Declared() {
		if err := r.Source.Validate(); err != nil {
			return invalidf("release %s: %v", r.Ref(), err)
		}
	}
	// A signature is optional; one that is PRESENT and unparseable is not.
	// Accepting malformed signature material and deciding about it later would
	// mean a listing could display "signed" for bytes nothing could ever
	// check, which is the assurance-without-a-check this whole phase exists to
	// prevent.
	if r.Signature.Declared() {
		if err := r.Signature.Validate(); err != nil {
			return invalidf("release %s: %v", r.Ref(), err)
		}
	}
	return nil
}

// Signed reports whether the release carries AO signature material at all. It
// says nothing about whether that material verifies.
func (r Release) Signed() bool { return r.Signature.Declared() }

// External reports whether this release names a git source.
func (r Release) External() bool { return r.Source.Declared() }

// Ref is the "<skillId>@<version>" identity used in messages and audit lines.
func (r Release) Ref() string { return r.SkillID + "@" + r.Version }

// AssessAvailable is the trust state of a release AO has NOT fetched.
//
// It can only ever return revoked or unverified, and that is the point: a
// search moves no bytes, so there is nothing for a listing to have verified.
// A listing that showed "verified" before an install would be describing a
// check nobody ran.
func AssessAvailable(r Release) TrustState {
	if r.Revoked {
		return TrustRevoked
	}
	return TrustUnverified
}

// AssessInstalled is the trust state of a release whose bytes AO fetched,
// hashed and matched against the pinned release.
//
// Callers pass integrityMatched and signatureVerified explicitly rather than
// having this function infer either, so the one place that decides "we
// checked" is the one place that actually did the comparison.
//
// The ladder is strict and it only goes up when the step below it held:
// a signature is never allowed to substitute for the hash. A release whose
// signature verified but whose bytes did not match is UNVERIFIED, not trusted,
// because a signature over a description of bytes AO does not have says
// nothing about the bytes AO does have.
func AssessInstalled(r Release, integrityMatched, signatureVerified bool) TrustState {
	if r.Revoked {
		return TrustRevoked
	}
	if !integrityMatched {
		return TrustUnverified
	}
	if signatureVerified {
		return TrustTrusted
	}
	return TrustVerified
}

// CompatibilityVerdict is what a compatibility check concluded. It is a
// three-value answer rather than a boolean because "AO does not know its own
// version" is a real state on a developer build and must not read as "yes".
type CompatibilityVerdict string

const (
	// CompatibilityOK means the running AO version is inside the release's range.
	CompatibilityOK CompatibilityVerdict = "compatible"
	// CompatibilityTooOld means AO is below aoMinVersion.
	CompatibilityTooOld CompatibilityVerdict = "ao-too-old"
	// CompatibilityTooNew means AO is above aoMaxVersion.
	CompatibilityTooNew CompatibilityVerdict = "ao-too-new"
	// CompatibilityUnknown means the running AO version is not a parseable
	// release version -- a source build reporting "dev", for instance. The
	// check DID NOT RUN, and callers record that rather than claiming a pass.
	CompatibilityUnknown CompatibilityVerdict = "unknown"
)

// CheckCompatibility compares a release's declared range against the running
// AO version.
//
// Note what this is not: it is not a trust control, and it fails OPEN with
// CompatibilityUnknown on an unparseable AO version. A trust check that cannot
// run must refuse, because the thing it protects against is an attacker; a
// compatibility check that cannot run must say so, because the thing it
// protects against is a bad afternoon. Conflating the two would make every
// developer build unable to install anything, which teaches people to route
// around the checks that do matter.
func CheckCompatibility(r Release, aoVersion string) CompatibilityVerdict {
	current, err := skillcatalog.ParseVersion(aoVersion)
	if err != nil {
		return CompatibilityUnknown
	}
	minV, err := skillcatalog.ParseVersion(r.Compatibility.AOMinVersion)
	if err != nil {
		return CompatibilityUnknown
	}
	if current.Compare(minV) < 0 {
		return CompatibilityTooOld
	}
	if maxRaw := strings.TrimSpace(r.Compatibility.AOMaxVersion); maxRaw != "" {
		maxV, err := skillcatalog.ParseVersion(maxRaw)
		if err != nil {
			return CompatibilityUnknown
		}
		if current.Compare(maxV) > 0 {
			return CompatibilityTooNew
		}
	}
	return CompatibilityOK
}

// SortReleases orders releases by skill id, then by version newest first. It
// is exported because every provider and every listing must agree on order:
// two surfaces that sorted differently would make "the first result" mean two
// things.
func SortReleases(rels []Release) {
	sort.SliceStable(rels, func(i, j int) bool {
		if rels[i].SkillID != rels[j].SkillID {
			return rels[i].SkillID < rels[j].SkillID
		}
		return compareVersions(rels[i].Version, rels[j].Version) > 0
	})
}

func compareVersions(a, b string) int {
	av, aErr := skillcatalog.ParseVersion(a)
	bv, bErr := skillcatalog.ParseVersion(b)
	if aErr != nil || bErr != nil {
		return strings.Compare(a, b)
	}
	return av.Compare(bv)
}

// NewerThan reports whether version a is strictly newer than version b.
func NewerThan(a, b string) bool { return compareVersions(a, b) > 0 }
