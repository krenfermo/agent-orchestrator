// Package executionpolicy implements Checkpoint 8P-C: a per-user,
// frontend-editable UserExecutionPolicy replacing the fixed Claude<->Codex
// RoutingPolicy. Every method takes an explicit, server-resolved UserID and
// every store call is scoped by it -- this package never trusts a user id
// from client input (see httpd/controllers/execution_policy.go), mirroring
// service/providerprofile's own invariant.
package executionpolicy

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Store is the durable persistence surface this service depends on. Backed
// by storage/sqlite/store.Store in production.
type Store interface {
	GetUserExecutionPolicyByUser(ctx context.Context, userID domain.UserID) (domain.UserExecutionPolicy, bool, error)
	UpsertUserExecutionPolicy(ctx context.Context, p domain.UserExecutionPolicy) (domain.UserExecutionPolicy, error)
	ListProviderProfilesByUser(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error)
}

// Manager is the controller-facing contract for the
// /api/v1/execution-policy surface.
type Manager interface {
	Get(ctx context.Context, userID domain.UserID) (domain.UserExecutionPolicy, error)
	Put(ctx context.Context, userID domain.UserID, in PutInput) (domain.UserExecutionPolicy, error)
	// SyncPriorities is Checkpoint 8P-E.13A.5's stale-policy repair hook,
	// called by service/providerprofile whenever a profile is connected,
	// enabled or re-probed. See Service.SyncPriorities.
	SyncPriorities(ctx context.Context, userID domain.UserID) error
}

// PutInput is client-supplied input for PUT /api/v1/execution-policy.
type PutInput struct {
	AutonomousMode           bool
	PlannerPriority          []domain.ProviderProfileID
	WorkerPriority           []domain.ProviderProfileID
	ReviewerPriority         []domain.ProviderProfileID
	DecisionResolverPriority []domain.ProviderProfileID
	FallbackBehavior         domain.FallbackBehavior
	ReviewIndependence       domain.ReviewIndependence
}

// Service implements Manager.
type Service struct {
	Store     Store
	IDFactory func() string
	Clock     func() time.Time
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

// Get returns userID's stored policy, or the documented bootstrap default
// (domain.DefaultUserExecutionPolicy) built from their current profiles when
// none has been saved yet -- the frontend always has something concrete to
// render/edit, never a bare 404 for "no policy configured".
//
// Checkpoint 8P-E.13A.5: a stored policy is repaired before it is returned
// (syncStored), so simply opening Settings brings an existing installation's
// priority lists back in step with the profiles it actually owns. This is the
// self-heal path -- a user who connected Codex months after saving their
// policy never has to disconnect and reconnect it.
func (s *Service) Get(ctx context.Context, userID domain.UserID) (domain.UserExecutionPolicy, error) {
	if userID == "" {
		return domain.UserExecutionPolicy{}, apierr.Unauthorized("NOT_AUTHENTICATED", "no user resolved")
	}
	if p, ok, err := s.Store.GetUserExecutionPolicyByUser(ctx, userID); err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: get: %w", err)
	} else if ok {
		return s.syncStored(ctx, p)
	}
	profiles, err := s.Store.ListProviderProfilesByUser(ctx, userID)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: list profiles for default: %w", err)
	}
	// No stored row: DefaultUserExecutionPolicy is already built from the
	// user's CURRENT profiles, so it can never be stale. Nothing is persisted
	// here -- reading Settings has never created a policy row, and this
	// checkpoint does not change that.
	return domain.DefaultUserExecutionPolicy(userID, profiles), nil
}

// SyncPriorities repairs userID's STORED policy in place, appending every
// owned/enabled/capable profile its priority lists do not already mention
// (domain.SyncExecutionPolicyPriorities holds the rules and the guarantees).
//
// A no-op when the user has no stored policy: there is nothing stale to
// repair, and Get already derives a current default for that case. Callers
// treat a sync failure as non-fatal -- connecting a provider must still
// succeed even if its policy touch-up did not (see providerprofile.Service).
func (s *Service) SyncPriorities(ctx context.Context, userID domain.UserID) error {
	if userID == "" {
		return nil
	}
	stored, ok, err := s.Store.GetUserExecutionPolicyByUser(ctx, userID)
	if err != nil {
		return fmt.Errorf("executionpolicy: sync: get: %w", err)
	}
	if !ok {
		return nil
	}
	if _, err := s.syncStored(ctx, stored); err != nil {
		return err
	}
	return nil
}

// syncStored applies the sync to an already-loaded stored policy, persisting
// it only when something actually changed -- an unchanged policy must not
// churn updated_at on every Settings read.
func (s *Service) syncStored(ctx context.Context, stored domain.UserExecutionPolicy) (domain.UserExecutionPolicy, error) {
	profiles, err := s.Store.ListProviderProfilesByUser(ctx, stored.UserID)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: sync: list profiles: %w", err)
	}
	synced, changed := domain.SyncExecutionPolicyPriorities(stored, profiles, registry.ProviderDescriptors())
	if !changed {
		return stored, nil
	}
	synced.UpdatedAt = s.now()
	saved, err := s.Store.UpsertUserExecutionPolicy(ctx, synced)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: sync: upsert: %w", err)
	}
	return saved, nil
}

// Put validates and upserts userID's policy (Checkpoint 8P-C §14):
//   - every profile ID in every priority list must belong to userID (a
//     foreign or nonexistent profile id is rejected the same way
//     GetProviderProfileByIDForUser's not-found/forbidden collapse works --
//     see validatePriority);
//   - no duplicate IDs within a single priority list;
//   - a referenced profile's descriptor must support the role's required
//     capability;
//   - an unsupported provider (no real adapter -- e.g. MiniMax) can never
//     be referenced, even if a profile row somehow exists for it;
//   - fallbackBehavior/reviewIndependence must be part of their closed
//     enums.
//
// Disabled/unconnected profiles ARE allowed in the stored priority list
// (documented choice, checkpoint brief §14/§17): a user may pre-arrange
// their preferred order before actually connecting a provider. Only
// *routing eligibility* (domain.EligibleProfiles, evaluated fresh at
// dispatch time) filters a disabled/unconnected profile out of an actual
// decision -- PUT never rejects on that basis.
func (s *Service) Put(ctx context.Context, userID domain.UserID, in PutInput) (domain.UserExecutionPolicy, error) {
	if userID == "" {
		return domain.UserExecutionPolicy{}, apierr.Unauthorized("NOT_AUTHENTICATED", "no user resolved")
	}
	if in.FallbackBehavior == "" {
		in.FallbackBehavior = domain.FallbackUseNextAvailable
	}
	if in.ReviewIndependence == "" {
		in.ReviewIndependence = domain.ReviewIndependenceRequireDifferentProvider
	}
	if !in.FallbackBehavior.Valid() {
		return domain.UserExecutionPolicy{}, apierr.Invalid("INVALID_FALLBACK_BEHAVIOR", "unknown fallback behavior", nil)
	}
	if !in.ReviewIndependence.Valid() {
		return domain.UserExecutionPolicy{}, apierr.Invalid("INVALID_REVIEW_INDEPENDENCE", "unknown review independence", nil)
	}

	profiles, err := s.Store.ListProviderProfilesByUser(ctx, userID)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: list profiles: %w", err)
	}
	owned := make(map[domain.ProviderProfileID]domain.ProviderProfile, len(profiles))
	for _, p := range profiles {
		owned[p.ID] = p
	}
	descriptors := registry.ProviderDescriptors()

	if err := validatePriority(in.PlannerPriority, owned, descriptors, domain.CapabilityPlanner); err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	if err := validatePriority(in.WorkerPriority, owned, descriptors, domain.CapabilityWorker); err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	if err := validatePriority(in.ReviewerPriority, owned, descriptors, domain.CapabilityReviewer); err != nil {
		return domain.UserExecutionPolicy{}, err
	}
	if err := validatePriority(in.DecisionResolverPriority, owned, descriptors, domain.CapabilityDecisionResolver); err != nil {
		return domain.UserExecutionPolicy{}, err
	}

	existing, ok, err := s.Store.GetUserExecutionPolicyByUser(ctx, userID)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: get existing: %w", err)
	}
	id := domain.UserExecutionPolicyID(s.newID())
	createdAt := s.now()
	if ok {
		id = existing.ID
		createdAt = existing.CreatedAt
	}
	policy := domain.UserExecutionPolicy{
		ID:                       id,
		UserID:                   userID,
		Version:                  domain.UserExecutionPolicyVersion,
		AutonomousMode:           in.AutonomousMode,
		PlannerPriority:          in.PlannerPriority,
		WorkerPriority:           in.WorkerPriority,
		ReviewerPriority:         in.ReviewerPriority,
		DecisionResolverPriority: in.DecisionResolverPriority,
		FallbackBehavior:         in.FallbackBehavior,
		ReviewIndependence:       in.ReviewIndependence,
		CreatedAt:                createdAt,
		UpdatedAt:                s.now(),
	}
	saved, err := s.Store.UpsertUserExecutionPolicy(ctx, policy)
	if err != nil {
		return domain.UserExecutionPolicy{}, fmt.Errorf("executionpolicy: upsert: %w", err)
	}
	return saved, nil
}

// validatePriority enforces one priority list's rules: every id owned,
// no duplicates, capability supported, provider actually implemented.
// A foreign/nonexistent profile id and an owned-but-incapable one are
// deliberately reported with different codes (unlike provider_profiles.go's
// single-resource GET/PATCH, this is a bulk validation endpoint where
// leaking "this id exists but isn't yours" costs nothing extra beyond what
// "this id doesn't support this role" already reveals about the caller's
// OWN account -- neither reveals another user's data).
func validatePriority(ids []domain.ProviderProfileID, owned map[domain.ProviderProfileID]domain.ProviderProfile, descriptors []domain.ProviderAdapterDescriptor, capability domain.ProviderCapability) error {
	seen := make(map[domain.ProviderProfileID]struct{}, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			return apierr.Invalid("DUPLICATE_PROFILE_IN_PRIORITY", "a profile id appears more than once in one priority list", map[string]any{"profileId": string(id)})
		}
		seen[id] = struct{}{}
		profile, ok := owned[id]
		if !ok {
			return apierr.NotFound("PROVIDER_PROFILE_NOT_FOUND", "provider profile not found")
		}
		desc, ok := descriptorFor(descriptors, profile.Provider, profile.Harness)
		if !ok || !desc.Available {
			return apierr.Invalid("UNSUPPORTED_PROVIDER", "this provider has no real adapter implemented yet", map[string]any{"profileId": string(id)})
		}
		if !hasCapability(desc.Capabilities, capability) && !hasCapability(profile.Capabilities, capability) {
			return apierr.Invalid("CAPABILITY_NOT_SUPPORTED", "this profile does not support the required capability for this priority list", map[string]any{"profileId": string(id), "capability": string(capability)})
		}
	}
	return nil
}

func descriptorFor(descriptors []domain.ProviderAdapterDescriptor, provider string, harness domain.AgentHarness) (domain.ProviderAdapterDescriptor, bool) {
	for _, d := range descriptors {
		if d.Provider == provider && d.Harness == harness {
			return d, true
		}
	}
	return domain.ProviderAdapterDescriptor{}, false
}

func hasCapability(caps []domain.ProviderCapability, capability domain.ProviderCapability) bool {
	for _, c := range caps {
		if c == capability {
			return true
		}
	}
	return false
}
