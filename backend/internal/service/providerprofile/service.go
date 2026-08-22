// Package providerprofile implements Checkpoint 8P-B: per-user provider
// connections. Every method takes an explicit, server-resolved UserID and
// every store call is scoped by it in SQL -- there is no code path here
// that can be handed a client-supplied user id (the controller is
// responsible for resolving it from the authenticated session before ever
// calling into this package; see httpd/controllers/provider_profiles.go).
package providerprofile

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
)

// Store is the durable persistence surface this service depends on. Backed
// by storage/sqlite/store.Store in production. Every method already takes
// (or is scoped to) a user id -- see storage/sqlite/queries/provider_profiles.sql.
type Store interface {
	InsertProviderProfile(ctx context.Context, p domain.ProviderProfile) (domain.ProviderProfile, error)
	ListProviderProfilesByUser(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error)
	GetProviderProfileByIDForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID) (domain.ProviderProfile, bool, error)
	UpdateProviderProfileForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID, displayName string, enabled bool, defaultModel string, updatedAt time.Time) (bool, error)
	UpdateProviderProfileAuthStateForUser(ctx context.Context, id domain.ProviderProfileID, userID domain.UserID, state domain.ProviderAuthState, updatedAt time.Time) (bool, error)
}

// Prober performs a best-effort auth-state check for one profile, run
// against that profile owner's isolated runtime-home -- never the daemon
// host's own real environment. See probe.go for the Claude Code/Codex
// implementations.
type Prober interface {
	Probe(ctx context.Context, harness domain.AgentHarness, env runtimehome.Environment) (domain.ProviderAuthState, error)
}

// PolicySyncer keeps the owner's stored UserExecutionPolicy priority lists in
// step with the profiles they actually own (Checkpoint 8P-E.13A.5). Satisfied
// by *service/executionpolicy.Service. A narrow one-method seam rather than
// importing that package's Manager wholesale, matching this package's existing
// DataDirer/Prober convention -- and nil means "not wired", in which case
// connecting a profile simply leaves the policy alone exactly as before.
type PolicySyncer interface {
	SyncPriorities(ctx context.Context, userID domain.UserID) error
}

// DataDirer supplies AO_DATA_DIR so this service can prepare a user's
// runtime-home on demand. A narrow single-method seam instead of importing
// config directly, mirroring reviewgateway's dataDir-string convention.
type DataDirer interface {
	DataDir() string
}

// Manager is the controller-facing contract for the /api/v1/provider-profiles
// surface.
type Manager interface {
	List(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error)
	Create(ctx context.Context, userID domain.UserID, in CreateInput) (domain.ProviderProfile, error)
	Get(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error)
	Update(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID, in UpdateInput) (domain.ProviderProfile, error)
	Connect(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error)
	Disconnect(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error)
	Test(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (TestResult, error)
	Registry(ctx context.Context) []domain.ProviderAdapterDescriptor
}

// CreateInput is client-supplied input for creating a profile. UserID is
// deliberately absent -- the service always takes it as an explicit,
// server-resolved parameter instead (see Manager.Create).
type CreateInput struct {
	Provider     string
	Harness      domain.AgentHarness
	DisplayName  string
	DefaultModel string
}

// UpdateInput is client-supplied input for updating a profile's mutable
// fields. Provider/Harness/AuthMethod are immutable after creation.
type UpdateInput struct {
	DisplayName  string
	Enabled      bool
	DefaultModel string
}

// TestResult is the outcome of a connection test.
type TestResult struct {
	AuthState domain.ProviderAuthState
	OK        bool
	Message   string
}

// Service implements Manager.
type Service struct {
	Store     Store
	Prober    Prober
	DataDir   DataDirer
	IDFactory func() string
	Clock     func() time.Time
	// PolicySync keeps the owner's stored execution policy in step with their
	// profiles (Checkpoint 8P-E.13A.5). Optional.
	PolicySync PolicySyncer
}

// syncPolicy is the single place every profile mutation touches the owner's
// execution policy. Deliberately best-effort: a profile was already durably
// created/enabled/re-probed by the time this runs, and failing the caller's
// request because a downstream policy touch-up failed would be strictly
// worse than leaving the policy stale (Get repairs it on the next Settings
// read anyway). Mirrors this package's existing "a nil optional dependency is
// a silent no-op" convention.
func (s *Service) syncPolicy(ctx context.Context, userID domain.UserID) {
	if s.PolicySync == nil {
		return
	}
	_ = s.PolicySync.SyncPriorities(ctx, userID)
}

var _ Manager = (*Service)(nil)

func (s *Service) newID() string {
	if s.IDFactory != nil {
		return s.IDFactory()
	}
	return uuid.NewString()
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

// List returns every profile owned by userID.
func (s *Service) List(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error) {
	profiles, err := s.Store.ListProviderProfilesByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("providerprofile: list: %w", err)
	}
	return profiles, nil
}

// Registry returns the static, non-user-scoped provider descriptor list.
func (s *Service) Registry(ctx context.Context) []domain.ProviderAdapterDescriptor {
	return registry.ProviderDescriptors()
}

func (s *Service) descriptorFor(provider string, harness domain.AgentHarness) (domain.ProviderAdapterDescriptor, bool) {
	for _, d := range registry.ProviderDescriptors() {
		if d.Provider == provider && d.Harness == harness {
			return d, true
		}
	}
	return domain.ProviderAdapterDescriptor{}, false
}

// Create registers a new profile owned by userID, rejecting an unknown or
// unavailable provider/harness pair.
func (s *Service) Create(ctx context.Context, userID domain.UserID, in CreateInput) (domain.ProviderProfile, error) {
	if userID == "" {
		return domain.ProviderProfile{}, apierr.Unauthorized("NOT_AUTHENTICATED", "no user resolved")
	}
	desc, ok := s.descriptorFor(in.Provider, in.Harness)
	if !ok {
		return domain.ProviderProfile{}, apierr.Invalid("PROVIDER_UNKNOWN", "unknown provider/harness pair", nil)
	}
	if !desc.Available {
		return domain.ProviderProfile{}, apierr.Invalid("PROVIDER_UNAVAILABLE", "provider has no adapter implementation yet", map[string]any{"reason": desc.Unavailable})
	}
	displayName := in.DisplayName
	if displayName == "" {
		displayName = desc.DisplayName
	}
	authMethod := domain.AuthMethodUnsupported
	if len(desc.AuthMethods) > 0 {
		authMethod = desc.AuthMethods[0]
	}
	now := s.now()
	profile := domain.ProviderProfile{
		ID:           domain.ProviderProfileID(s.newID()),
		UserID:       userID,
		Provider:     in.Provider,
		Harness:      in.Harness,
		DisplayName:  displayName,
		Enabled:      true,
		AuthState:    domain.ProviderAuthStateUnknown,
		AuthMethod:   authMethod,
		DefaultModel: in.DefaultModel,
		Capabilities: desc.Capabilities,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	created, err := s.Store.InsertProviderProfile(ctx, profile)
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: create: %w", err)
	}
	// A newly connected provider must become selectable without the user
	// having to re-save Settings (Checkpoint 8P-E.13A.5).
	s.syncPolicy(ctx, userID)
	return created, nil
}

// Get returns one profile, or apierr.NotFound if it doesn't exist or isn't
// owned by userID -- the two cases are indistinguishable to the caller by
// design (see package doc and Checkpoint 8P-B's IDOR security tests).
func (s *Service) Get(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error) {
	profile, ok, err := s.Store.GetProviderProfileByIDForUser(ctx, id, userID)
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: get: %w", err)
	}
	if !ok {
		return domain.ProviderProfile{}, apierr.NotFound("PROVIDER_PROFILE_NOT_FOUND", "provider profile not found")
	}
	return profile, nil
}

// Update replaces a profile's mutable fields, scoped to userID.
func (s *Service) Update(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID, in UpdateInput) (domain.ProviderProfile, error) {
	existing, err := s.Get(ctx, userID, id)
	if err != nil {
		return domain.ProviderProfile{}, err
	}
	displayName := in.DisplayName
	if displayName == "" {
		displayName = existing.DisplayName
	}
	ok, err := s.Store.UpdateProviderProfileForUser(ctx, id, userID, displayName, in.Enabled, in.DefaultModel, s.now())
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: update: %w", err)
	}
	if !ok {
		return domain.ProviderProfile{}, apierr.NotFound("PROVIDER_PROFILE_NOT_FOUND", "provider profile not found")
	}
	// Enabling a previously disabled profile makes it eligible for the first
	// time; disabling one never removes it from a priority list (out of scope
	// by design -- see domain.SyncExecutionPolicyPriorities).
	s.syncPolicy(ctx, userID)
	return s.Get(ctx, userID, id)
}

// Connect prepares the owner's runtime-home and probes current auth state.
// For CLI-bootstrap providers (Claude Code, Codex today), AO does not drive
// an OAuth flow itself: connecting means making sure the isolated
// runtime-home this user's sessions will launch under exists, then
// reporting whether it already looks authenticated -- the user completes
// any interactive login themselves, inside a session that now runs under
// that same isolated home (see internal/runtimehome and
// session_manager wiring).
func (s *Service) Connect(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error) {
	return s.refreshAuthState(ctx, userID, id)
}

// Test re-probes auth state (same mechanism as Connect) and reports whether
// it currently looks authenticated.
func (s *Service) Test(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (TestResult, error) {
	profile, err := s.refreshAuthState(ctx, userID, id)
	if err != nil {
		return TestResult{}, err
	}
	return TestResult{
		AuthState: profile.AuthState,
		OK:        profile.AuthState == domain.ProviderAuthStateAuthenticated,
		Message:   testMessage(profile.DisplayName, profile.AuthState),
	}, nil
}

// Disconnect clears AO's cached belief about this profile's auth state. It
// never deletes the provider CLI's own on-disk credentials -- AO does not
// manage secrets it doesn't own (see migration 0110's doc comment).
func (s *Service) Disconnect(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error) {
	if _, err := s.Get(ctx, userID, id); err != nil {
		return domain.ProviderProfile{}, err
	}
	ok, err := s.Store.UpdateProviderProfileAuthStateForUser(ctx, id, userID, domain.ProviderAuthStateUnauthenticated, s.now())
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: disconnect: %w", err)
	}
	if !ok {
		return domain.ProviderProfile{}, apierr.NotFound("PROVIDER_PROFILE_NOT_FOUND", "provider profile not found")
	}
	return s.Get(ctx, userID, id)
}

func (s *Service) refreshAuthState(ctx context.Context, userID domain.UserID, id domain.ProviderProfileID) (domain.ProviderProfile, error) {
	profile, err := s.Get(ctx, userID, id)
	if err != nil {
		return domain.ProviderProfile{}, err
	}
	if s.DataDir == nil || s.Prober == nil {
		return profile, nil
	}
	env, err := runtimehome.Prepare(s.DataDir.DataDir(), userID)
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: prepare runtime-home: %w", err)
	}
	state, probeErr := s.Prober.Probe(ctx, profile.Harness, env)
	if probeErr != nil {
		state = domain.ProviderAuthStateError
	}
	ok, err := s.Store.UpdateProviderProfileAuthStateForUser(ctx, id, userID, state, s.now())
	if err != nil {
		return domain.ProviderProfile{}, fmt.Errorf("providerprofile: persist auth state: %w", err)
	}
	if !ok {
		return domain.ProviderProfile{}, apierr.NotFound("PROVIDER_PROFILE_NOT_FOUND", "provider profile not found")
	}
	// Connect/Test is where a CLI-bootstrap provider usually first becomes
	// genuinely usable, so it is the most likely moment for a stale policy to
	// need repairing.
	s.syncPolicy(ctx, userID)
	return s.Get(ctx, userID, id)
}

// testMessage produces the human-readable Message returned alongside a
// TestResult. It never includes credential material, tokens, or filesystem
// paths -- only the classified auth state and generic remediation text.
func testMessage(displayName string, state domain.ProviderAuthState) string {
	switch state {
	case domain.ProviderAuthStateAuthenticated:
		return displayName + " is authenticated and ready for AO workflows."
	case domain.ProviderAuthStateUnauthenticated:
		return displayName + " is installed, but this AO user is not authenticated. Run the provider's own login inside a session for this profile, then test again."
	case domain.ProviderAuthStateNotInstalled:
		return displayName + " is not installed on this AO instance."
	case domain.ProviderAuthStateError:
		return "The connection test for " + displayName + " failed unexpectedly. Try again."
	default:
		return displayName + "'s authentication state could not be determined."
	}
}
