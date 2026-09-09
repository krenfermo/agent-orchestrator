// Package skillimage is AO's trust root for container images: which image this
// installation is authorized to execute, for which scope, decided by a named
// administrator.
//
// # What this policy trusts, and what it does not
//
// It trusts TWO things and nothing else: an administrator's decision, and the
// bytes that administrator inspected, identified by an immutable digest.
//
// It does NOT trust:
//
//   - a publisher's signature. There is none. Nothing here verifies a
//     Notary/cosign/sigstore attestation, and an approval must never be read as
//     "the publisher vouches for this". It says "an administrator of this
//     installation looked at this digest and said yes".
//   - a registry. Nothing here contacts one, and nothing pulls. An image that
//     is not already on the host is a refusal.
//   - the presence of an image on the host. Phase 8's earlier resolution took
//     whatever `alpine:3.19` happened to be locally, which trusts whoever last
//     ran `docker pull` — including a compromised base image nobody reviewed.
//     Presence is now necessary and nowhere near sufficient.
//   - a tag. A tag is a mutable pointer, and "the image we approved" has to
//     mean one thing forever.
//
// So the residual risk is stated rather than papered over: an administrator who
// approves a malicious digest has approved a malicious image, and AO will run
// it. What AO guarantees is that the decision was made by somebody with
// administrative permission, that it is recorded with their name, that it
// covers exactly one scope, and that the bytes which run are the bytes that
// were approved.
//
// # Why the scope is the full one
//
// An approval binds tenant + project + skill + version + mode, the same
// skillscope.Scope that secrets (phase 5) and egress (phase 7) use. Approving
// an image for "the static-code mode of security-audit 1.2.0 on this project"
// is a statement somebody can evaluate. Approving it for "this installation" is
// not: a newer package version is a different manifest asking for different
// capabilities, and it must be looked at again.
package skillimage

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillscope"
)

// ErrInvalid marks an approval that could not be enforced.
var ErrInvalid = errors.New("skillimage: invalid")

// ErrNotApproved means no active approval covers what was asked. It is the one
// error every refusal path resolves to, because from the caller's side "no
// approval", "expired", "revoked" and "for a different scope" are one
// condition: AO is not authorized to run this image here.
var ErrNotApproved = errors.New("skillimage: no active approval for this image and scope")

// Scope is re-exported so an approval, a secret grant and an egress lease all
// mean the same thing by "which run may do this".
type Scope = skillscope.Scope

// digestRe is the only shape a digest may have. Sixty-four lowercase hex
// characters after sha256: — an uppercase or short digest is a typo, and a typo
// that resolved to something would be the worst possible outcome here.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ParseDigest validates an image digest.
func ParseDigest(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a digest is required; an approval without one approves nothing "+
			"in particular", ErrInvalid)
	}
	if !digestRe.MatchString(trimmed) {
		return "", fmt.Errorf("%w: %q is not a sha256 digest (sha256: followed by 64 lowercase hex "+
			"characters); a tag is a mutable pointer and cannot be approved", ErrInvalid, trimmed)
	}
	return trimmed, nil
}

// referenceRe is the shape of the repository NAME an approval records. It is
// documentation for whoever reads the row -- resolution goes by digest and
// never by this string -- but it must still be unambiguous, so a tag or a
// digest suffix inside it is refused.
var referenceRe = regexp.MustCompile(`^[a-z0-9]+([._\-/][a-z0-9]+)*$`)

// ParseReference validates the repository name an approval is recorded against.
//
// It refuses anything carrying a tag or a digest, including the specific string
// "latest". The reference exists so a person reading an approval knows what
// they looked at; a reference that could ALSO be resolved would invite somebody
// to resolve it, and then the digest would stop being the identity.
func ParseReference(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an approval records which repository the digest came from", ErrInvalid)
	}
	if strings.Contains(trimmed, "@") || strings.Contains(trimmed, ":") {
		return "", fmt.Errorf("%w: reference %q carries a tag or a digest; record the bare repository "+
			"name and let the digest be the identity", ErrInvalid, trimmed)
	}
	base := trimmed
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		base = trimmed[idx+1:]
	}
	if base == "latest" {
		return "", fmt.Errorf("%w: %q names a moving target; approve a digest, not a floating name",
			ErrInvalid, trimmed)
	}
	if !referenceRe.MatchString(trimmed) {
		return "", fmt.Errorf("%w: reference %q is not a plain repository name", ErrInvalid, trimmed)
	}
	return trimmed, nil
}

// Approval is one administrator's decision that this installation may execute
// one image, for one scope, to back one AO tool.
type Approval struct {
	ID string
	// Scope is what the approval covers. Every field, no wildcard.
	Scope Scope
	// Tool is the AO tool contract this image backs (e.g. ao.static-scan/v1).
	// An image approved to back the static scan is not thereby approved to
	// back a future tool with a different command and a different blast radius.
	Tool string
	// Reference is the repository the digest came from, recorded for a human.
	// Nothing resolves through it.
	Reference string
	// Digest is the identity. Immutable, and the only thing resolution uses.
	Digest string
	// ApprovedBy is the administrator. Required: an approval nobody signed is
	// an approval nobody can be asked about.
	ApprovedBy string
	ApprovedAt time.Time
	// ExpiresAt is optional, unlike a secret grant's. A base image is a
	// long-lived artifact and forcing a re-approval date on one would train
	// people to set it far away; what makes this safe is that revocation is
	// immediate for new runs and that the row names who to ask.
	ExpiresAt *time.Time
	RevokedAt *time.Time
	// Note is what the administrator says they checked. It is required, and it
	// is the only field here that is prose: an approval with no stated reason
	// is indistinguishable from a mistake six months later.
	Note string
}

// Active reports whether this approval may authorize a launch right now.
func (a Approval) Active(now time.Time) bool {
	if a.RevokedAt != nil && !a.RevokedAt.After(now) {
		return false
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(now) {
		return false
	}
	return true
}

// InactiveReason says why not, for an error a person can act on. It returns ""
// when the approval is active.
func (a Approval) InactiveReason(now time.Time) string {
	switch {
	case a.RevokedAt != nil && !a.RevokedAt.After(now):
		return fmt.Sprintf("the approval was revoked at %s", a.RevokedAt.UTC().Format(time.RFC3339))
	case a.ExpiresAt != nil && !a.ExpiresAt.After(now):
		return fmt.Sprintf("the approval expired at %s", a.ExpiresAt.UTC().Format(time.RFC3339))
	default:
		return ""
	}
}

// Validate rejects an approval that could not be enforced.
func (a Approval) Validate() error {
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Tool) == "" {
		return fmt.Errorf("%w: an approval names the tool the image backs", ErrInvalid)
	}
	if _, err := ParseReference(a.Reference); err != nil {
		return err
	}
	if _, err := ParseDigest(a.Digest); err != nil {
		return err
	}
	if strings.TrimSpace(a.ApprovedBy) == "" {
		return fmt.Errorf("%w: an approval records who made it; one nobody signed is one nobody "+
			"can be asked about", ErrInvalid)
	}
	if a.ApprovedAt.IsZero() {
		return fmt.Errorf("%w: an approval records when it was made", ErrInvalid)
	}
	if strings.TrimSpace(a.Note) == "" {
		return fmt.Errorf("%w: an approval records what was checked; one with no stated reason is "+
			"indistinguishable from a mistake six months later", ErrInvalid)
	}
	if a.ExpiresAt != nil && !a.ExpiresAt.After(a.ApprovedAt) {
		return fmt.Errorf("%w: an approval expires after it is made", ErrInvalid)
	}
	if a.RevokedAt != nil && a.RevokedAt.Before(a.ApprovedAt) {
		return fmt.Errorf("%w: an approval cannot be revoked before it was made", ErrInvalid)
	}
	return nil
}

// Covers reports whether this approval authorizes running digest for scope and
// tool at this moment.
//
// Every part must match exactly. There is no "the same image for a neighbouring
// mode" and no "a newer version of the same package": both are decisions
// somebody has to make again.
func (a Approval) Covers(scope Scope, tool, digest string, now time.Time) bool {
	return a.Active(now) &&
		a.Scope.Matches(scope) &&
		a.Tool == tool &&
		a.Digest == digest
}

// Describe renders the approval for an audit line or an operator. It carries no
// credential and no path.
func (a Approval) Describe() string {
	window := "no expiry"
	if a.ExpiresAt != nil {
		window = "until " + a.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if a.RevokedAt != nil {
		window = "revoked " + a.RevokedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s %s@%s for %s, approved by %s, %s",
		a.Tool, a.Reference, a.Digest, a.Scope, a.ApprovedBy, window)
}
