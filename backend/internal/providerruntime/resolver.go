// Package providerruntime is Checkpoint 8P-B.1's single canonical
// implementation of "workflow run -> owner -> isolated provider subprocess
// env". Every real launch path (worker, reviewer, planner, decision
// resolver) calls the same Resolver instead of re-deriving this logic --
// see workflow.RuntimeIsolation, which this package's *Resolver implements.
package providerruntime

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimehome"
)

// OwnerStore resolves a workflow run's owner. Satisfied by
// storage/sqlite/store.Store (GetWorkflowRunOwner, Checkpoint 8P-A).
type OwnerStore interface {
	GetWorkflowRunOwner(ctx context.Context, id string) (*domain.UserID, error)
}

// ProfileStore lists a user's provider profiles. Satisfied by
// storage/sqlite/store.Store (Checkpoint 8P-B).
type ProfileStore interface {
	ListProviderProfilesByUser(ctx context.Context, userID domain.UserID) ([]domain.ProviderProfile, error)
}

// Resolver is the shared owner+env resolution used by every launcher.
// Every field is required for real isolation to take effect; a nil Owners
// or Profiles makes Resolve a permanent no-op (env=nil, owner="", err=nil)
// so wiring this into a build without full 8P-A/8P-B dependencies never
// blocks a launch that worked before this checkpoint.
type Resolver struct {
	Owners   OwnerStore
	Profiles ProfileStore
	DataDir  string
	// TrustedLocal mirrors config.Config.TrustedLocalMode. When true, a
	// resolved owner with no matching profile is NOT blocked (today's
	// desktop UX keeps working exactly as before this checkpoint --
	// otherwise every existing single-user install would suddenly refuse
	// to launch anything the moment it upgrades, having never been asked
	// to "connect" a provider it already uses via its real, already-
	// authenticated CLI). When false (real multi-user mode), a resolved
	// owner with no matching profile blocks the launch with
	// ports.ErrProviderProfileRequired -- multi-user mode never inherits
	// the daemon's own credentials.
	TrustedLocal bool
}

// Resolve returns the env overrides to apply for a workflow-run-owned
// launch of harness, and the resolved owner (empty if unresolved/unowned).
// A non-nil error is always ports.ErrProviderProfileRequired (or a durable
// lookup failure) and means: do not launch.
func (r *Resolver) Resolve(ctx context.Context, runID string, harness domain.AgentHarness) (env map[string]string, owner domain.UserID, err error) {
	if r == nil || r.Owners == nil {
		return nil, "", nil
	}
	ownerPtr, err := r.Owners.GetWorkflowRunOwner(ctx, runID)
	if err != nil {
		return nil, "", fmt.Errorf("providerruntime: resolve workflow run owner: %w", err)
	}
	if ownerPtr == nil {
		// Unowned run (predates ownership, or created while no user was
		// resolved) -- nothing to scope to; behave exactly as before this
		// checkpoint.
		return nil, "", nil
	}
	owner = *ownerPtr

	profile, ok, err := r.matchingProfile(ctx, owner, harness)
	if err != nil {
		return nil, owner, fmt.Errorf("providerruntime: list provider profiles: %w", err)
	}
	if !ok {
		if r.TrustedLocal {
			return nil, owner, nil
		}
		return nil, owner, ports.ErrProviderProfileRequired
	}
	_ = profile

	home, err := runtimehome.Prepare(r.DataDir, owner)
	if err != nil {
		return nil, owner, fmt.Errorf("providerruntime: prepare runtime-home: %w", err)
	}
	return home.SubprocessEnv(), owner, nil
}

func (r *Resolver) matchingProfile(ctx context.Context, owner domain.UserID, harness domain.AgentHarness) (domain.ProviderProfile, bool, error) {
	if r.Profiles == nil {
		return domain.ProviderProfile{}, false, nil
	}
	profiles, err := r.Profiles.ListProviderProfilesByUser(ctx, owner)
	if err != nil {
		return domain.ProviderProfile{}, false, err
	}
	for _, p := range profiles {
		if p.Harness == harness && p.Enabled {
			return p, true, nil
		}
	}
	return domain.ProviderProfile{}, false, nil
}
