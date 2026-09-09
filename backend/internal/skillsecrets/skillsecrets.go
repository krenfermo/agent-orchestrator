// Package skillsecrets is the reference-and-grant model for secrets a skill
// run may receive. It is deliberately NOT a vault.
//
// What it is: named references, values sealed at rest with internal/secretbox,
// grants bound to a scope narrow enough to answer "who may read this, on what,
// running what, until when", and single-use leases bound to one attempt.
//
// What it is not, and what nothing here should be mistaken for: rotation,
// versioned values, dynamic credentials, external backends, or an audit of who
// consumed a value outside AO. A real deployment wants those; this exists so a
// skill run can be given one string without the daemon handing over its own
// environment, and so the negative cases can be tested.
//
// # The control that does the most work
//
// SecretValue's String and MarshalJSON REDACT. A value cannot reach a log line,
// an error, a report or an API response by accident, because the two paths
// everything in Go eventually takes to become text both refuse. Reading the
// real bytes requires calling Reveal, which is greppable and rare.
package skillsecrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrInvalid marks a malformed reference, name or scope.
var ErrInvalid = errors.New("skillsecrets: invalid")

// nameRe is the shape of a secret name. It matches the manifest's
// scope.secrets.requested validation, so a package cannot request a name the
// store could never hold.
var nameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{2,63}$`)

// Ref is an opaque reference to a secret: a NAME, never a value.
//
// It is a distinct type rather than a string so a value can never be assigned
// where a reference is expected. Manifests, API requests, grants, prompts and
// logs all carry Refs; SecretValue is the only type that ever holds bytes, and
// it never crosses those boundaries.
type Ref string

// ParseRef validates a secret name.
func ParseRef(name string) (Ref, error) {
	trimmed := strings.TrimSpace(name)
	if !nameRe.MatchString(trimmed) {
		return "", fmt.Errorf("%w: secret name %q must be UPPER_SNAKE_CASE, 3-64 characters", ErrInvalid, name)
	}
	return Ref(trimmed), nil
}

// String renders the reference. A reference is a name and is safe to log —
// that is the whole point of separating it from the value.
func (r Ref) String() string { return string(r) }

// SecretValue holds plaintext and refuses to render it.
//
// String and MarshalJSON both return a redaction. Reveal is the only way to
// the bytes, so "did this value reach a log" is answerable by grepping for one
// method name rather than by auditing every format string in the codebase.
type SecretValue struct {
	// inner is unexported so no struct literal outside this package can build
	// one, and no reflection-based marshaller can reach it.
	inner string
}

// NewSecretValue wraps plaintext.
func NewSecretValue(plaintext string) SecretValue { return SecretValue{inner: plaintext} }

// Reveal returns the plaintext. Every call site is a place a secret leaves the
// type's protection, and there should be very few.
func (v SecretValue) Reveal() string { return v.inner }

// Len is the length, for a caller that must bound a write without reading.
func (v SecretValue) Len() int { return len(v.inner) }

// IsZero reports an unset value.
func (v SecretValue) IsZero() bool { return v.inner == "" }

// Redacted is what a SecretValue renders as, everywhere.
const Redacted = "[redacted]"

// String redacts. This is what stops fmt.Sprintf("%v", value) and every
// log/error path built on it from leaking.
func (v SecretValue) String() string { return Redacted }

// GoString redacts too, so %#v and dlv-style dumps do not defeat String.
func (v SecretValue) GoString() string { return Redacted }

// MarshalJSON redacts. A struct carrying a SecretValue can be serialized into
// a report or an API response without the value going with it.
func (v SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// UnmarshalJSON refuses. A secret must never arrive over the wire into a
// request body; it is registered through the one path that seals it.
func (v *SecretValue) UnmarshalJSON([]byte) error {
	return fmt.Errorf("%w: a secret value may not be decoded from JSON", ErrInvalid)
}

// Scope is what a grant is bound to. Every field narrows; none is optional,
// because a grant missing one of them answers "who may read this" with
// "somebody, somewhere, running something".
type Scope struct {
	// TenantID and ProjectID bind the grant to one organization's project. A
	// grant is unusable from any other, which is the isolation the negative
	// tests assert.
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	// SkillID and Version bind it to the exact package. A newer version is a
	// different package with a different manifest, so it needs its own grant.
	SkillID string
	Version string
	// ModeID binds it to one mode. A package that reads secrets in its
	// dependency mode does not get them in its pentest mode.
	ModeID string
}

// Validate rejects a partially-specified scope.
func (s Scope) Validate() error {
	missing := make([]string, 0, 5)
	if strings.TrimSpace(string(s.TenantID)) == "" {
		missing = append(missing, "tenantId")
	}
	if strings.TrimSpace(string(s.ProjectID)) == "" {
		missing = append(missing, "projectId")
	}
	if strings.TrimSpace(s.SkillID) == "" {
		missing = append(missing, "skillId")
	}
	if strings.TrimSpace(s.Version) == "" {
		missing = append(missing, "version")
	}
	if strings.TrimSpace(s.ModeID) == "" {
		missing = append(missing, "modeId")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: scope is missing %s; a grant without every field is a grant to "+
			"somebody, somewhere, running something", ErrInvalid, strings.Join(missing, ", "))
	}
	return nil
}

// Matches reports whether this scope is exactly the other. Every field must
// match: there is no wildcard, and adding one would be adding a way to write a
// grant nobody can reason about.
func (s Scope) Matches(other Scope) bool {
	return s.TenantID == other.TenantID &&
		s.ProjectID == other.ProjectID &&
		s.SkillID == other.SkillID &&
		s.Version == other.Version &&
		s.ModeID == other.ModeID
}

// String renders the scope for an audit line. It contains no secret material.
func (s Scope) String() string {
	return fmt.Sprintf("%s/%s %s@%s#%s", s.TenantID, s.ProjectID, s.SkillID, s.Version, s.ModeID)
}

// Grant is permission for one scope to receive one secret, until it expires or
// is revoked.
type Grant struct {
	ID        string
	Ref       Ref
	Scope     Scope
	GrantedBy string
	GrantedAt time.Time
	// ExpiresAt is required. A grant with no expiry is a grant nobody will
	// remember to remove, so the store refuses one.
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Active reports whether the grant may be used at the given moment. Expiry and
// revocation are checked here rather than at read time in SQL, so the same
// answer holds for any store.
func (g Grant) Active(now time.Time) bool {
	if g.RevokedAt != nil && !g.RevokedAt.After(now) {
		return false
	}
	return g.ExpiresAt.After(now)
}

// Validate rejects a grant that could not be enforced.
func (g Grant) Validate() error {
	if _, err := ParseRef(string(g.Ref)); err != nil {
		return err
	}
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(g.GrantedBy) == "" {
		return fmt.Errorf("%w: a grant records who made it", ErrInvalid)
	}
	if g.ExpiresAt.IsZero() {
		return fmt.Errorf("%w: a grant must expire; one that never does is one nobody removes", ErrInvalid)
	}
	if !g.ExpiresAt.After(g.GrantedAt) {
		return fmt.Errorf("%w: a grant expires after it is made", ErrInvalid)
	}
	return nil
}

// Lease binds a set of grants to ONE attempt of ONE run, once.
//
// It is what stops a replay: an attempt that already consumed its lease cannot
// consume another, and a lease minted for attempt N is not usable by attempt
// N-1 even when both belong to the same run and the grant is still active.
type Lease struct {
	ID    string
	Scope Scope
	// RunID and AttemptID pin the lease to one execution. Both are required.
	RunID     string
	AttemptID string
	Refs      []Ref
	IssuedAt  time.Time
	ExpiresAt time.Time
	// ConsumedAt is stamped when the values are delivered. A second delivery
	// against the same lease is refused.
	ConsumedAt *time.Time
}

// Usable reports whether this lease may still be redeemed.
func (l Lease) Usable(now time.Time) bool {
	return l.ConsumedAt == nil && l.ExpiresAt.After(now)
}

// Validate rejects a lease that could not be enforced.
func (l Lease) Validate() error {
	if err := l.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(l.RunID) == "" || strings.TrimSpace(l.AttemptID) == "" {
		return fmt.Errorf("%w: a lease binds to one run AND one attempt", ErrInvalid)
	}
	if len(l.Refs) == 0 {
		return fmt.Errorf("%w: a lease with no references delivers nothing", ErrInvalid)
	}
	if !l.ExpiresAt.After(l.IssuedAt) {
		return fmt.Errorf("%w: a lease expires after it is issued", ErrInvalid)
	}
	return nil
}
