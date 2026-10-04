package domain

import "time"

// ProviderProfileID identifies one user-owned provider connection.
type ProviderProfileID string

// ProviderCapability is one thing a provider profile can be used for by the
// workflow engine. Only capabilities a provider's adapter genuinely
// implements today may be declared -- see ProviderAdapterDescriptor.
type ProviderCapability string

// Known provider capabilities (Checkpoint 8P-B). This is the closed set the
// workflow engine (8P-C) will eventually route against; do not add a value
// here without a real, tested adapter behavior backing it.
const (
	CapabilityPlanner            ProviderCapability = "planner"
	CapabilityWorker             ProviderCapability = "worker"
	CapabilityReviewer           ProviderCapability = "reviewer"
	CapabilityDecisionResolver   ProviderCapability = "decision_resolver"
	CapabilityStructuredCallback ProviderCapability = "structured_callback"
	CapabilityReadOnlyReview     ProviderCapability = "read_only_review"
	CapabilityUsageTelemetry     ProviderCapability = "usage_telemetry"
	CapabilityCapacityTelemetry  ProviderCapability = "capacity_telemetry"
)

// ProviderAuthMethod is how a profile authenticates against its provider.
// Deliberately not username/password -- every real provider today is CLI
// or browser driven.
type ProviderAuthMethod string

// Known auth methods.
const (
	AuthMethodBrowserOAuth  ProviderAuthMethod = "browser_oauth"
	AuthMethodDeviceFlow    ProviderAuthMethod = "device_flow"
	AuthMethodCLIBootstrap  ProviderAuthMethod = "cli_bootstrap"
	AuthMethodAPIKey        ProviderAuthMethod = "api_key"
	AuthMethodExternalLogin ProviderAuthMethod = "external_login"
	AuthMethodUnsupported   ProviderAuthMethod = "unsupported"
)

// ProviderAuthState is AO's last-known belief about a profile's auth
// status. It is a cached, best-effort signal refreshed by Connect/Test --
// never a live guarantee.
type ProviderAuthState string

// Known auth states.
const (
	ProviderAuthStateUnknown         ProviderAuthState = "unknown"
	ProviderAuthStateAuthenticated   ProviderAuthState = "authenticated"
	ProviderAuthStateUnauthenticated ProviderAuthState = "unauthenticated"
	ProviderAuthStateError           ProviderAuthState = "error"
	// ProviderAuthStateNotInstalled means the probe could not even resolve
	// the provider's CLI binary on this AO instance -- distinct from
	// ProviderAuthStateUnknown (binary present, but auth state couldn't be
	// determined) so the UI never conflates "not installed" with "logged
	// out but installed".
	ProviderAuthStateNotInstalled ProviderAuthState = "not_installed"
)

// ProviderProfile is one user's connection to one provider/harness pair.
// It is always owned: UserID is set at creation from the resolved request
// identity and is never trusted from client input (see
// httpd/controllers/provider_profiles.go).
//
// ProviderProfile never holds a raw provider secret in memory beyond what a
// future api_key-authenticated provider strictly requires to authenticate a
// single call; SecretCiphertext (when present) must never be serialized
// into any JSON-facing DTO, mirroring User.PasswordHash.
type ProviderProfile struct {
	ID               ProviderProfileID
	UserID           UserID
	Provider         string
	Harness          AgentHarness
	DisplayName      string
	Enabled          bool
	AuthState        ProviderAuthState
	AuthMethod       ProviderAuthMethod
	DefaultModel     string
	Capabilities     []ProviderCapability
	SecretCiphertext []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// RequiredCapability returns the ProviderCapability a role needs to route
// through a profile at all (Checkpoint 8P-C §6), and false for a role that
// never routes through a provider (e.g. WorkflowRoleVerify).
func RequiredCapability(role WorkflowRole) (ProviderCapability, bool) {
	switch role {
	case WorkflowRolePlanner:
		return CapabilityPlanner, true
	case WorkflowRoleWorker, WorkflowRoleFixWorker:
		return CapabilityWorker, true
	case WorkflowRoleReviewer:
		return CapabilityReviewer, true
	case WorkflowRoleDecisionResolver:
		return CapabilityDecisionResolver, true
	default:
		return "", false
	}
}

// EligibleProfiles is Checkpoint 8P-C's pure (no IO) eligibility filter: a
// profile only ever becomes routable if it is enabled, its cached auth state
// is authenticated, its provider's adapter is actually implemented
// (descriptor.Available -- rules out MiniMax), and it advertises the role's
// required capability. Returns both the eligible set and, for every
// filtered-out profile, the specific reason it was excluded (so a routing
// decision that lands on an ineligible most-preferred entry can explain
// exactly why, per the closed RoutingReason enum) -- never a bare "not
// eligible".
func EligibleProfiles(profiles []ProviderProfile, descriptors []ProviderAdapterDescriptor, capability ProviderCapability) (eligible map[ProviderProfileID]ProviderProfile, ineligible map[ProviderProfileID]RoutingReason) {
	byKey := make(map[string]ProviderAdapterDescriptor, len(descriptors))
	for _, d := range descriptors {
		byKey[d.Provider+"|"+string(d.Harness)] = d
	}
	eligible = make(map[ProviderProfileID]ProviderProfile)
	ineligible = make(map[ProviderProfileID]RoutingReason)
	for _, p := range profiles {
		desc, ok := byKey[p.Provider+"|"+string(p.Harness)]
		if !ok || !desc.Available {
			ineligible[p.ID] = RoutingReasonUnsupportedProvider
			continue
		}
		if !p.Enabled {
			ineligible[p.ID] = RoutingReasonProviderDisabled
			continue
		}
		if p.AuthState != ProviderAuthStateAuthenticated {
			ineligible[p.ID] = RoutingReasonProfileNotConnected
			continue
		}
		if !hasCapability(p.Capabilities, capability) {
			ineligible[p.ID] = RoutingReasonCapabilityMissing
			continue
		}
		eligible[p.ID] = p
	}
	return eligible, ineligible
}

// HasCapability reports whether caps advertises capability. Exported for
// callers outside EligibleProfiles' role filter that need to check a single
// non-role capability (e.g. capacity_telemetry before running an active
// capacity probe).
func HasCapability(caps []ProviderCapability, capability ProviderCapability) bool {
	return hasCapability(caps, capability)
}

func hasCapability(caps []ProviderCapability, capability ProviderCapability) bool {
	for _, c := range caps {
		if c == capability {
			return true
		}
	}
	return false
}

// ProviderAdapterDescriptor is the provider-neutral, non-user-scoped
// description of what a provider adapter supports. It comes from the
// registry (adapters/agent/registry), not from storage -- it describes code
// capability, not a user's connection state.
type ProviderAdapterDescriptor struct {
	Provider     string
	Harness      AgentHarness
	DisplayName  string
	Capabilities []ProviderCapability
	AuthMethods  []ProviderAuthMethod
	Models       []string
	// Available is false for a provider that is known/named but has no real
	// adapter wired yet (e.g. MiniMax as of Checkpoint 8P-B) -- surfaced to
	// the frontend as "unsupported" rather than fabricated as functional.
	Available bool
	// Unavailable, when Available is false, is a short user-facing reason
	// (e.g. "no adapter implemented yet").
	Unavailable string
}
