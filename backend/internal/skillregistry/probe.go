package skillregistry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// probe.go -- "Test connection", and the six answers it is allowed to give.
//
// # What a test does
//
// One GET of one small metadata endpoint, parsed and checked. That is all.
// It downloads no artifact, changes no catalog, installs nothing, enables
// nothing, approves no image and writes no cache entry. A connection test that
// had a side effect would be a button people are afraid to press.
//
// # Why CONNECTED is the hardest state to reach
//
// A TCP connection proves a socket opened. TLS proves a certificate verified.
// A 200 proves something answered. NONE of those is "this is the registry I
// configured, and it will answer AO's questions": a load balancer, a captive
// portal, an unrelated service on the right port and a login page all produce a
// 200. So CONNECTED requires the descriptor to parse, to declare a protocol
// version this build speaks, and to carry the registry id that was configured.
// Everything short of that is INVALID_RESPONSE, which is the honest answer.

// ProbeState is the outcome of one connection test.
type ProbeState string

const (
	// ProbeConnected means the configured registry answered as itself, over
	// verified TLS, with a protocol version this build speaks.
	ProbeConnected ProbeState = "CONNECTED"
	// ProbeAuthFailed means the registry rejected the credential. The detail
	// names the secretRef and never the value.
	ProbeAuthFailed ProbeState = "AUTH_FAILED"
	// ProbeTLSFailed means the certificate did not verify. Deliberately not
	// merged into UNREACHABLE: an unreachable registry is an operations
	// problem, and a certificate that does not verify is a misconfiguration or
	// somebody in the middle.
	ProbeTLSFailed ProbeState = "TLS_FAILED"
	// ProbeUnreachable means nothing answered -- DNS, connection, timeout, 5xx.
	ProbeUnreachable ProbeState = "UNREACHABLE"
	// ProbeInvalidResponse means something answered and it was not this
	// registry speaking this protocol.
	ProbeInvalidResponse ProbeState = "INVALID_RESPONSE"
	// ProbePolicyBlocked means AO refused to make the request: the origin
	// resolved into a blocked range, the configuration is not reachable under
	// the network policy, or the credential could not be resolved.
	ProbePolicyBlocked ProbeState = "POLICY_BLOCKED"
)

// Valid reports whether s is one of the six.
func (s ProbeState) Valid() bool {
	switch s {
	case ProbeConnected, ProbeAuthFailed, ProbeTLSFailed,
		ProbeUnreachable, ProbeInvalidResponse, ProbePolicyBlocked:
		return true
	}
	return false
}

// OK reports whether the registry is usable.
func (s ProbeState) OK() bool { return s == ProbeConnected }

// ProbeResult is one connection test, in a shape a settings screen can render
// without deciding anything.
type ProbeResult struct {
	RegistryID string     `json:"registryId"`
	State      ProbeState `json:"state"`
	// Detail is the sentence to show. It names a secretRef when auth failed,
	// and never a credential value.
	Detail string `json:"detail"`
	// Origin is what AO actually tried to reach, so "it works in my browser"
	// and "AO cannot reach it" can be compared.
	Origin string `json:"origin,omitempty"`
	// ReportedRegistryID is what the far end called itself. It is shown on a
	// mismatch, because "you have pointed this at a different registry" is the
	// single most useful thing to say when it happens.
	ReportedRegistryID string    `json:"reportedRegistryId,omitempty"`
	ProtocolVersion    string    `json:"protocolVersion,omitempty"`
	AuthType           string    `json:"authType,omitempty"`
	SecretRef          string    `json:"secretRef,omitempty"`
	TestedAt           time.Time `json:"testedAt"`
	// Latency is how long the round trip took.
	Latency time.Duration `json:"latencyMs"`
}

// ConnectionProbe is a provider that can be asked to identify itself.
//
// It is an OPTIONAL interface on Provider rather than a sixth method, because
// four of the five Provider methods are about releases and this one is about
// the registry. A fixture provider that implements the five and not this one is
// a fixture with nothing to test, and should say so rather than be forced to
// invent an answer.
type ConnectionProbe interface {
	Probe(ctx context.Context, now func() time.Time) ProbeResult
}

// ProbeRefused is the result for a registry AO would not even open.
//
// It is POLICY_BLOCKED and not UNREACHABLE, deliberately: nothing was sent, AO
// itself refused, and "unreachable" would send an operator to look at the
// network instead of at the configuration in front of them.
func ProbeRefused(reg Registry, err error, now func() time.Time) ProbeResult {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	result := ProbeResult{
		RegistryID: reg.ID,
		State:      ProbePolicyBlocked,
		AuthType:   string(reg.EffectiveAuthType()),
		SecretRef:  reg.CredentialSecretName,
		TestedAt:   now(),
	}
	switch {
	case errors.Is(err, ErrSecretUnavailable):
		result.Detail = fmt.Sprintf("the credential %s could not be resolved: %v",
			reg.CredentialSecretName, err)
	default:
		result.Detail = err.Error()
	}
	if origin, _, parseErr := ParseBaseURL(reg.Location); parseErr == nil {
		result.Origin = origin.String()
	}
	return result
}

// Probe implements ConnectionProbe for a local directory registry.
//
// A directory has no connection to test, and the honest answer says so rather
// than showing a green badge for something that never opened a socket -- a
// badge that meant two different things would teach people it means nothing.
func (p *FileProvider) Probe(_ context.Context, now func() time.Time) ProbeResult {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	result := ProbeResult{RegistryID: p.registryID, Origin: p.root, TestedAt: now()}
	if _, err := p.load(); err != nil {
		result.State, result.Detail = ProbeInvalidResponse, err.Error()
		return result
	}
	result.State = ProbeConnected
	result.Detail = "This is a local directory registry and its index reads correctly. " +
		"No socket was opened and nothing was fetched over the network."
	return result
}

// Probe implements ConnectionProbe: one GET of the smallest endpoint the
// protocol has, checked against the configuration.
func (p *HTTPSProvider) Probe(ctx context.Context, now func() time.Time) ProbeResult {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	result := ProbeResult{
		RegistryID: p.registryID,
		Origin:     p.endpoints.origin.String(),
		AuthType:   string(p.creds.AuthType()),
		SecretRef:  p.creds.Ref(),
		TestedAt:   now(),
	}
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	started := now()
	descriptor, err := p.describe(probeCtx)
	result.Latency = now().Sub(started)
	if err != nil {
		result.State, result.Detail = classifyProbeError(err, p.creds, p.endpoints.origin)
		return result
	}
	result.ProtocolVersion = descriptor.APIVersion
	result.ReportedRegistryID = descriptor.RegistryID

	if err := checkAPIVersion(descriptor.APIVersion); err != nil {
		result.State, result.Detail = ProbeInvalidResponse, err.Error()
		return result
	}
	if strings.TrimSpace(descriptor.RegistryID) != p.registryID {
		// Not CONNECTED. Something answered, and it is not the registry this
		// row is configured for.
		result.State = ProbeInvalidResponse
		result.Detail = fmt.Sprintf("this endpoint calls itself %q and this registry is configured as %q; "+
			"AO will not treat one registry's answers as another's",
			strings.TrimSpace(descriptor.RegistryID), p.registryID)
		return result
	}
	result.State = ProbeConnected
	result.Detail = fmt.Sprintf("%s answered as %s over verified TLS, speaking %s. "+
		"Metadata only: no artifact was downloaded and nothing was installed, enabled or approved.",
		p.endpoints.origin, p.registryID, descriptor.APIVersion)
	return result
}

// classifyProbeError maps a failed handshake onto one of the six states.
func classifyProbeError(err error, creds Credentials, origin Origin) (ProbeState, string) {
	switch {
	case errors.Is(err, ErrNetworkPolicy):
		return ProbePolicyBlocked, err.Error()
	case errors.Is(err, ErrRegistryTLS):
		return ProbeTLSFailed, err.Error()
	case errors.Is(err, ErrRegistryAuth):
		if creds.AuthType() == AuthNone || creds.AuthType() == "" {
			return ProbeAuthFailed, fmt.Sprintf(
				"the registry requires a credential and this registry is configured with authType none: %v", err)
		}
		return ProbeAuthFailed, err.Error()
	case errors.Is(err, ErrRegistryUnreachable), errors.Is(err, context.DeadlineExceeded):
		return ProbeUnreachable, err.Error()
	case errors.Is(err, ErrNoSuchSkill):
		// The handshake endpoint answering 404 means the base URL points at
		// something that is not an AO registry -- a web root, a different API.
		return ProbeInvalidResponse, fmt.Sprintf(
			"%s answered 404; this base URL does not serve the AO registry protocol", origin)
	case errors.Is(err, ErrRegistryResponse):
		return ProbeInvalidResponse, err.Error()
	}
	return ProbeUnreachable, err.Error()
}

// describe performs the handshake GET. It is the smallest metadata read the
// protocol has, and it exists so a connection test never has to run a search.
func (p *HTTPSProvider) describe(ctx context.Context) (registryDescriptor, error) {
	reqCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, p.endpoints.registry(), http.NoBody)
	if err != nil {
		return registryDescriptor{}, fmt.Errorf("%w: %w", ErrRegistryUnreadable, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if err := p.creds.applyTo(req); err != nil {
		return registryDescriptor{}, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return registryDescriptor{}, classifyTransportError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := checkStatus(resp, p.creds.Ref()); err != nil {
		return registryDescriptor{}, err
	}
	if err := checkContentType(resp, "application/json"); err != nil {
		return registryDescriptor{}, err
	}
	// A handshake body is tiny. Bounding it at the metadata ceiling would let a
	// misrouted endpoint stream 8 MiB into a settings screen.
	body, err := readBounded(resp.Body, 64<<10)
	if err != nil {
		return registryDescriptor{}, err
	}
	var descriptor registryDescriptor
	if err := decodeStrict(body, &descriptor); err != nil {
		return registryDescriptor{}, err
	}
	return descriptor, nil
}
