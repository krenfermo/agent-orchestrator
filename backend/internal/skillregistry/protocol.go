package skillregistry

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// protocol.go -- the wire contract a private AO registry serves.
//
// # The one property the shape enforces
//
// SEARCHING NEVER DOWNLOADS. Metadata and artifacts are different endpoints
// returning different media types, and the provider method that reads bytes is
// a different method from the four that read metadata. A registry cannot
// smuggle a package into a search answer because there is nowhere in the
// metadata schema to put one: a release names its digests, and the digests are
// what AO will check the bytes against, not a place to hide them.
//
// # Every path is built here, never received
//
// The endpoints below are constructed from the configured origin plus a path
// this file chose, with every dynamic segment escaped. No URL that arrives in a
// registry response is ever fetched -- sourceUrl, changelogUrl and
// attestationUrl are reference metadata for a human, and following one would
// hand a compromised registry the daemon's network position.
//
// # Versioning
//
// A response declaring an apiVersion this build does not know is refused
// rather than best-effort parsed, exactly as skillcatalog refuses an unknown
// manifest apiVersion. A reader that guesses at an unknown contract is a reader
// that installs something it did not understand.

// ProtocolVersion is the registry-protocol contract this build speaks. It is
// deliberately the same string the local registry index uses: the two are the
// same vocabulary carried over different transports, and a private registry is
// meant to be the directory layout put behind HTTPS.
const ProtocolVersion = IndexAPIVersion

// Endpoint paths, relative to the configured baseURL.
const (
	pathRegistry    = "/v1/registry"
	pathSearch      = "/v1/skills"
	pathRevocations = "/v1/revocations"
)

// Timeouts. They are explicit and different, because a search that hangs for
// five minutes is a broken settings screen and a download that gives up after
// fifteen seconds is a registry nobody can install from.
const (
	// MetadataTimeout bounds one metadata request end to end.
	MetadataTimeout = 20 * time.Second
	// ProbeTimeout bounds a connection test. Shorter, because a person is
	// watching it.
	ProbeTimeout = 10 * time.Second
	// ArtifactTimeout bounds one artifact download end to end.
	ArtifactTimeout = 5 * time.Minute
	// MaxRedirects is how many same-origin redirects one request may follow.
	// Cross-origin is not "more redirects"; it is refused outright.
	MaxRedirects = 3
)

// registryDescriptor is the handshake body: who this registry says it is.
//
// The registryId it reports is CHECKED against the configured one and never
// adopted. A registry that could name itself could impersonate another one, and
// every install AO recorded would carry the wrong origin.
type registryDescriptor struct {
	APIVersion  string `json:"apiVersion"`
	RegistryID  string `json:"registryId"`
	DisplayName string `json:"displayName,omitempty"`
	// Publisher, when present, is what this registry claims to publish under.
	// It is display metadata; the pinned_publisher trust policy compares
	// against the RELEASE's publisher and the manifest inside the bytes.
	Publisher string `json:"publisher,omitempty"`
}

// releaseListBody is the shape of every metadata answer that returns many.
type releaseListBody struct {
	APIVersion string    `json:"apiVersion"`
	Releases   []Release `json:"releases"`
}

// releaseBody is the shape of the one-release answer.
type releaseBody struct {
	APIVersion string  `json:"apiVersion"`
	Release    Release `json:"release"`
}

// Revocation is one release a registry has withdrawn.
//
// It is a separate endpoint from the release metadata on purpose. A revocation
// has to be answerable when the release itself is gone -- a registry that
// deletes a compromised release and serves 404 has told AO nothing, and 404 is
// indistinguishable from a typo.
type Revocation struct {
	// Subject is WHAT was withdrawn. Empty means "release", which is what
	// every phase-11 registry serves and what keeps the protocol version
	// unchanged: a field whose absence has the old meaning is a compatible
	// addition.
	//
	// A registry may also withdraw a signing key, a publisher or a trust root.
	// AO accepts those and scopes them to installs from THAT registry, because
	// a registry that could revoke AO's official key globally could switch off
	// every trusted install on the machine. A registry's word can refuse; it
	// can never accept, and it can never reach past its own installs.
	Subject RevocationSubject `json:"subject,omitempty"`
	// SubjectID identifies a non-release subject: a key id, a publisher, or a
	// trust root id. It is ignored for a release revocation, which is
	// identified by SkillID and Version.
	SubjectID string `json:"subjectId,omitempty"`

	SkillID string `json:"skillId"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
	// RevokedAt is when the registry says it withdrew the release. AO records
	// it alongside the moment AO itself observed it, because those are
	// different facts and only the second one is AO's.
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// EffectiveSubject is what this revocation withdraws. An empty subject is a
// release, which is the only thing the phase-11 protocol could express.
func (r Revocation) EffectiveSubject() RevocationSubject {
	if strings.TrimSpace(string(r.Subject)) == "" {
		return SubjectRelease
	}
	return r.Subject
}

// Ref is the identity this revocation names, for a message and an audit line.
func (r Revocation) Ref() string {
	if s := r.EffectiveSubject(); s != SubjectRelease {
		return string(s) + " " + r.SubjectID
	}
	return r.SkillID + "@" + r.Version
}

// Validate enforces the revocation contract.
func (r Revocation) Validate() error {
	subject := r.EffectiveSubject()
	if !subject.Valid() {
		return invalidf("revocation subject %q is not one of release, signing_key, publisher, "+
			"trust_root", r.Subject)
	}
	if subject != SubjectRelease {
		if strings.TrimSpace(r.SubjectID) == "" {
			return invalidf("a %s revocation must name what it withdraws", subject)
		}
		if len(r.SubjectID) > 128 {
			return invalidf("revocation subjectId is longer than 128 characters")
		}
		if strings.TrimSpace(r.Reason) == "" {
			return invalidf("revocation of %s must say why", r.Ref())
		}
		return nil
	}
	if !idRe.MatchString(r.SkillID) {
		return invalidf("revocation skillId %q must be lowercase kebab-case", r.SkillID)
	}
	if strings.TrimSpace(r.Version) == "" {
		return invalidf("a revocation names one exact version")
	}
	if strings.TrimSpace(r.Reason) == "" {
		// Same rule as Release.Validate: a revocation that does not say why is
		// indistinguishable from a mistake, and it is about to block installs.
		return invalidf("revocation %s must say why", r.Ref())
	}
	return nil
}

// revocationListBody is the shape of the revocation answer.
type revocationListBody struct {
	APIVersion  string       `json:"apiVersion"`
	Revocations []Revocation `json:"revocations"`
}

// endpoints builds every URL one registry's provider will ever request.
type endpoints struct {
	origin   Origin
	basePath string
}

func (e endpoints) url(p string, query url.Values) string {
	u := url.URL{Scheme: e.origin.Scheme, Host: e.origin.Authority(), Path: e.basePath + p}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func (e endpoints) registry() string { return e.url(pathRegistry, nil) }

func (e endpoints) search(q Query) string {
	values := url.Values{}
	if q.Text != "" {
		values.Set("q", q.Text)
	}
	if q.Publisher != "" {
		values.Set("publisher", q.Publisher)
	}
	if q.Capability != "" {
		values.Set("capability", q.Capability)
	}
	if q.IncludeDeprecated {
		values.Set("includeDeprecated", "true")
	}
	if q.IncludeRevoked {
		values.Set("includeRevoked", "true")
	}
	values.Set("limit", strconv.Itoa(q.EffectiveLimit()))
	return e.url(pathSearch, values)
}

// versions and release escape their segments. A skill id is kebab-case by
// validation and a version is MAJOR.MINOR.PATCH, so neither can contain a
// slash today -- escaping them anyway is what keeps that true when somebody
// widens one of those rules.
func (e endpoints) versions(skillID string) string {
	return e.url(pathSearch+"/"+url.PathEscape(skillID)+"/versions", nil)
}

func (e endpoints) release(skillID, version string) string {
	return e.url(pathSearch+"/"+url.PathEscape(skillID)+"/versions/"+url.PathEscape(version), nil)
}

func (e endpoints) artifact(skillID, version string) string {
	return e.url(pathSearch+"/"+url.PathEscape(skillID)+"/versions/"+url.PathEscape(version)+"/artifact", nil)
}

func (e endpoints) revocations() string { return e.url(pathRevocations, nil) }

// checkAPIVersion refuses a body from a contract this build does not speak.
func checkAPIVersion(got string) error {
	if got != ProtocolVersion {
		return fmt.Errorf("%w: apiVersion %q is not supported (want %q)",
			ErrRegistryResponse, got, ProtocolVersion)
	}
	return nil
}
