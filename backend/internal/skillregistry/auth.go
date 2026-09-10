package skillregistry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/skillsecrets"
)

// auth.go -- how a private registry's credential reaches a request, and every
// place it deliberately does not.
//
// # The value never lands in this package's data
//
// A Registry carries an AuthType and a SecretRef. It never carries a value, and
// there is no field it could be put in: the credential is fetched from the
// sealed store at the moment a request is built, wrapped in a
// skillsecrets.SecretValue whose String, GoString and MarshalJSON all redact,
// and dropped when the request is done. Nothing persists it, nothing logs it,
// and no error message quotes it -- errors name the REF, which is a name and is
// safe to print.
//
// # Where it is attached, and where it is stripped
//
// Exactly one place: Credentials.apply, on a request whose URL this package
// built from the registry's own origin. Any redirect is refused before it is
// followed (see httpsprovider.go), so there is no cross-origin case in which
// the header could travel -- but applyTo re-checks the origin anyway, because
// "the caller already checked" is how a header eventually travels.

// ErrSecretUnavailable means the configured secretRef could not be resolved.
// It is separate from an auth REJECTION: a missing secret is this
// installation's problem and a rejected one is the registry's answer.
var ErrSecretUnavailable = errors.New("skillregistry: registry credential is unavailable")

// AuthType is how a registry expects to be authenticated.
//
// There is no "basic" and no "credential in the query string". Basic sends a
// password the same way a bearer token goes, with worse ergonomics and a
// browser-cache story nobody wants; a query string ends up in access logs,
// proxy logs and referrers, which is the definition of a credential in a place
// it should not be.
type AuthType string

const (
	// AuthNone is an unauthenticated registry. It is the default, and it is
	// legitimate for a read-only mirror inside a network that already
	// authenticates.
	AuthNone AuthType = "none"
	// AuthBearer sends "Authorization: Bearer <secret>".
	//
	//nolint:gosec // G101 flags the word "bearer"; this is the NAME of an auth
	// scheme, and this package holds no credential value anywhere.
	AuthBearer AuthType = "bearer"
	// AuthAPIKeyHeader sends the secret in a named header, which is what a
	// registry that predates OAuth vocabulary usually wants. The header NAME
	// is configuration and is printed freely; the value is not.
	//
	//nolint:gosec // G101 flags "api_key"; same as above -- an auth scheme's
	// name, in a package with no field a credential value could live in.
	AuthAPIKeyHeader AuthType = "api_key_header"
)

// Valid reports whether a is a supported auth type.
func (a AuthType) Valid() bool {
	switch a {
	case AuthNone, AuthBearer, AuthAPIKeyHeader:
		return true
	}
	return false
}

// NeedsSecret reports whether this auth type requires a secretRef.
func (a AuthType) NeedsSecret() bool { return a == AuthBearer || a == AuthAPIKeyHeader }

// DefaultAPIKeyHeader is the header an api_key registry uses when the
// configuration names none.
//
// clear by design, and never a value.
//
//nolint:gosec // G101 flags "API-Key"; this is a header NAME, sent in the
const DefaultAPIKeyHeader = "X-API-Key"

// SecretResolver turns a registry's secretRef into a value.
//
// It takes the REGISTRY, not a bare name, so the implementation can enforce
// that a caller only ever resolves a ref through a registry it can see -- which
// is what keeps tenant A from reading tenant B's credential by naming it. A
// resolver that accepted a string would be a resolver whose tenant check lived
// somewhere else.
type SecretResolver interface {
	ResolveRegistrySecret(ctx context.Context, reg Registry) (skillsecrets.SecretValue, error)
}

// SecretResolverFunc adapts a function to SecretResolver.
type SecretResolverFunc func(ctx context.Context, reg Registry) (skillsecrets.SecretValue, error)

// ResolveRegistrySecret implements SecretResolver.
func (f SecretResolverFunc) ResolveRegistrySecret(
	ctx context.Context, reg Registry,
) (skillsecrets.SecretValue, error) {
	return f(ctx, reg)
}

// Credentials is one registry's resolved authentication, held for the life of a
// provider and never written anywhere.
type Credentials struct {
	authType AuthType
	header   string
	// ref is the NAME. It is what every error and every audit line says.
	ref string
	// value redacts in every rendering path Go has. It is a value rather than
	// a pointer so a zero Credentials is a working "no auth".
	value skillsecrets.SecretValue
	// origin is the only place this credential may be sent.
	origin Origin
}

// Ref returns the secret NAME this credential came from, or empty. Safe to log.
func (c Credentials) Ref() string { return c.ref }

// AuthType returns how this credential is presented. Safe to log.
func (c Credentials) AuthType() AuthType { return c.authType }

// String redacts. Credentials is a struct somebody will eventually %v, and the
// default rendering of a struct prints its fields.
func (c Credentials) String() string {
	if c.authType == AuthNone || c.authType == "" {
		return "skillregistry.Credentials{none}"
	}
	return fmt.Sprintf("skillregistry.Credentials{%s from %s: %s}",
		c.authType, c.ref, skillsecrets.Redacted)
}

// GoString redacts too, so %#v does not defeat String.
func (c Credentials) GoString() string { return c.String() }

// MarshalJSON redacts, so a struct carrying Credentials cannot be serialized
// into a response or a log line and leak.
func (c Credentials) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", c.String())), nil
}

// resolveCredentials fetches the registry's credential, once, for one provider.
func resolveCredentials(
	ctx context.Context, reg Registry, origin Origin, resolver SecretResolver,
) (Credentials, error) {
	authType := reg.EffectiveAuthType()
	if !authType.NeedsSecret() {
		return Credentials{authType: AuthNone, origin: origin}, nil
	}
	if resolver == nil {
		return Credentials{}, fmt.Errorf("%w: %s needs the secret %s and no secret backend is configured",
			ErrSecretUnavailable, reg.ID, reg.CredentialSecretName)
	}
	value, err := resolver.ResolveRegistrySecret(ctx, reg)
	if err != nil {
		// The resolver's error is included, and every error on that path names
		// the ref rather than the value. TestCredentialsNeverLeak holds it.
		return Credentials{}, fmt.Errorf("%w: %s: %w", ErrSecretUnavailable, reg.CredentialSecretName, err)
	}
	if value.IsZero() {
		return Credentials{}, fmt.Errorf("%w: the secret %s is empty; a registry credential that is "+
			"blank would authenticate as nobody and read as configured",
			ErrSecretUnavailable, reg.CredentialSecretName)
	}
	return Credentials{
		authType: authType,
		header:   reg.EffectiveAPIKeyHeader(),
		ref:      reg.CredentialSecretName,
		value:    value,
		origin:   origin,
	}, nil
}

// applyTo attaches the credential to one request, or refuses.
//
// The origin re-check is the belt to the redirect refusal's braces. A request
// whose URL is not this registry's exact origin gets NO credential and an
// error, so the only way this header travels is to the host an administrator
// configured.
func (c Credentials) applyTo(req *http.Request) error {
	if c.authType == AuthNone || c.authType == "" {
		return nil
	}
	if !c.origin.Matches(req.URL) {
		return fmt.Errorf("%w: refusing to send the credential %s to %s://%s, which is not %s",
			ErrNetworkPolicy, c.ref, req.URL.Scheme, req.URL.Host, c.origin)
	}
	switch c.authType {
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+c.value.Reveal())
	case AuthAPIKeyHeader:
		name := strings.TrimSpace(c.header)
		if name == "" {
			name = DefaultAPIKeyHeader
		}
		req.Header.Set(name, c.value.Reveal())
	}
	return nil
}
