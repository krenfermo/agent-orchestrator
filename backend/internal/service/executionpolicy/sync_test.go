package executionpolicy_test

// Checkpoint 8P-E.13A.5: a stored execution policy must stay in step with the
// provider profiles its owner actually has, without ever disturbing an
// explicit user preference. Tests run against the real sqlite store, so what
// they prove is what is durably persisted -- not what an in-memory fake
// agreed to.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const (
	userID   = domain.UserID("user-sync")
	claudeID = domain.ProviderProfileID("prof-claude")
	codexID  = domain.ProviderProfileID("prof-codex")
)

// baseTime keeps profile CreatedAt ordering explicit: Claude was connected
// first, Codex later. The sync appends in connection order, so this is what
// makes "newly connected lands last" deterministic rather than incidental.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newSyncFixture(t *testing.T) (*executionpolicy.Service, *sqlite.Store) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	if _, err := store.InsertUser(t.Context(), domain.User{
		ID: userID, DisplayName: "sync", Email: "sync@example.com", Username: "sync",
		PasswordHash: "x", Status: domain.UserStatusActive, Role: domain.UserRoleMember,
		CreatedAt: baseTime, UpdatedAt: baseTime,
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return &executionpolicy.Service{Store: store, Clock: func() time.Time { return baseTime.Add(time.Hour) }}, store
}

func seedClaude(t *testing.T, store *sqlite.Store) {
	t.Helper()
	insertProfile(t, store, domain.ProviderProfile{
		ID: claudeID, UserID: userID, Provider: "anthropic", Harness: domain.HarnessClaudeCode,
		DisplayName: "Claude Code", Enabled: true, AuthState: domain.ProviderAuthStateAuthenticated,
		AuthMethod: domain.AuthMethodCLIBootstrap,
		Capabilities: []domain.ProviderCapability{
			domain.CapabilityPlanner, domain.CapabilityWorker,
			domain.CapabilityReviewer, domain.CapabilityDecisionResolver,
		},
		CreatedAt: baseTime, UpdatedAt: baseTime,
	})
}

// seedCodex mirrors the real Codex descriptor: worker/reviewer/
// decision_resolver, and deliberately NO planner.
func seedCodex(t *testing.T, store *sqlite.Store, enabled bool) {
	t.Helper()
	insertProfile(t, store, domain.ProviderProfile{
		ID: codexID, UserID: userID, Provider: "openai", Harness: domain.HarnessCodex,
		DisplayName: "Codex", Enabled: enabled, AuthState: domain.ProviderAuthStateAuthenticated,
		AuthMethod: domain.AuthMethodCLIBootstrap,
		Capabilities: []domain.ProviderCapability{
			domain.CapabilityWorker, domain.CapabilityReviewer, domain.CapabilityDecisionResolver,
		},
		CreatedAt: baseTime.Add(time.Minute), UpdatedAt: baseTime.Add(time.Minute),
	})
}

func insertProfile(t *testing.T, store *sqlite.Store, p domain.ProviderProfile) {
	t.Helper()
	if _, err := store.InsertProviderProfile(t.Context(), p); err != nil {
		t.Fatalf("seed profile %s: %v", p.ID, err)
	}
}

// storePolicy writes the stale stored row a test starts from.
func storePolicy(t *testing.T, store *sqlite.Store, p domain.UserExecutionPolicy) domain.UserExecutionPolicy {
	t.Helper()
	p.ID, p.UserID, p.Version = "policy-1", userID, domain.UserExecutionPolicyVersion
	p.CreatedAt, p.UpdatedAt = baseTime, baseTime
	if p.FallbackBehavior == "" {
		p.FallbackBehavior = domain.FallbackUseNextAvailable
	}
	if p.ReviewIndependence == "" {
		p.ReviewIndependence = domain.ReviewIndependenceRequireDifferentProvider
	}
	saved, err := store.UpsertUserExecutionPolicy(t.Context(), p)
	if err != nil {
		t.Fatalf("seed policy: %v", err)
	}
	return saved
}

func readPolicy(t *testing.T, store *sqlite.Store) domain.UserExecutionPolicy {
	t.Helper()
	p, ok, err := store.GetUserExecutionPolicyByUser(t.Context(), userID)
	if err != nil || !ok {
		t.Fatalf("read policy: ok=%v err=%v", ok, err)
	}
	return p
}

func wantIDs(t *testing.T, label string, got, want []domain.ProviderProfileID) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// A: the exact ~/.ao/data shape -- reviewer_priority names only Claude while
// an enabled, reviewer-capable Codex profile exists. Sync appends Codex AFTER
// Claude.
func TestSync_AppendsNewlyConnectedReviewerProfile(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		ReviewerPriority: []domain.ProviderProfileID{claudeID},
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	wantIDs(t, "reviewer_priority", readPolicy(t, store).ReviewerPriority,
		[]domain.ProviderProfileID{claudeID, codexID})
}

// B: capability drives which lists a profile joins. Codex is worker/reviewer/
// decision_resolver capable but NOT planner-capable, so planner_priority must
// be left alone.
func TestSync_AppendsPerCapabilityAndLeavesPlannerAlone(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		PlannerPriority:          []domain.ProviderProfileID{claudeID},
		WorkerPriority:           []domain.ProviderProfileID{claudeID},
		ReviewerPriority:         []domain.ProviderProfileID{claudeID},
		DecisionResolverPriority: []domain.ProviderProfileID{claudeID},
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	got := readPolicy(t, store)
	both := []domain.ProviderProfileID{claudeID, codexID}
	wantIDs(t, "worker_priority", got.WorkerPriority, both)
	wantIDs(t, "reviewer_priority", got.ReviewerPriority, both)
	wantIDs(t, "decision_resolver_priority", got.DecisionResolverPriority, both)
	wantIDs(t, "planner_priority", got.PlannerPriority, []domain.ProviderProfileID{claudeID})
}

// C: syncing a policy that is already complete changes nothing -- no
// duplicate entries, and no updated_at churn on every Settings read.
func TestSync_AlreadyPresentProfileIsNotDuplicated(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	before := storePolicy(t, store, domain.UserExecutionPolicy{
		WorkerPriority:   []domain.ProviderProfileID{claudeID, codexID},
		ReviewerPriority: []domain.ProviderProfileID{codexID, claudeID},
	})

	for i := 0; i < 3; i++ {
		if err := svc.SyncPriorities(context.Background(), userID); err != nil {
			t.Fatalf("SyncPriorities #%d: %v", i, err)
		}
	}
	got := readPolicy(t, store)
	wantIDs(t, "worker_priority", got.WorkerPriority, []domain.ProviderProfileID{claudeID, codexID})
	wantIDs(t, "reviewer_priority", got.ReviewerPriority, []domain.ProviderProfileID{codexID, claudeID})
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("updated_at moved from %v to %v on a no-op sync", before.UpdatedAt, got.UpdatedAt)
	}
}

// D: an explicit ordering the user chose is never rearranged, even when it is
// the reverse of the bootstrap default.
func TestSync_ExplicitOrderingIsPreserved(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		WorkerPriority:   []domain.ProviderProfileID{codexID, claudeID},
		ReviewerPriority: []domain.ProviderProfileID{codexID, claudeID},
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	got := readPolicy(t, store)
	wantIDs(t, "worker_priority", got.WorkerPriority, []domain.ProviderProfileID{codexID, claudeID})
	wantIDs(t, "reviewer_priority", got.ReviewerPriority, []domain.ProviderProfileID{codexID, claudeID})
}

// E: everything outside the priority lists survives a sync untouched --
// especially the settings a user deliberately changed away from the defaults.
func TestSync_UnrelatedPolicyFieldsAreUnchanged(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	before := storePolicy(t, store, domain.UserExecutionPolicy{
		AutonomousMode:     true,
		ReviewerPriority:   []domain.ProviderProfileID{claudeID},
		FallbackBehavior:   domain.FallbackWaitForPreferred,
		ReviewIndependence: domain.ReviewIndependenceAllowSameProviderFallback,
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	got := readPolicy(t, store)
	if got.ID != before.ID || got.UserID != before.UserID || got.Version != before.Version {
		t.Fatalf("identity changed: %+v -> %+v", before, got)
	}
	if got.AutonomousMode != true {
		t.Fatalf("autonomous_mode = %v, want true", got.AutonomousMode)
	}
	if got.FallbackBehavior != domain.FallbackWaitForPreferred {
		t.Fatalf("fallback_behavior = %q, want wait_for_preferred", got.FallbackBehavior)
	}
	if got.ReviewIndependence != domain.ReviewIndependenceAllowSameProviderFallback {
		t.Fatalf("review_independence = %q, want allow_same_provider_fallback", got.ReviewIndependence)
	}
	if !got.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("created_at moved from %v to %v", before.CreatedAt, got.CreatedAt)
	}
}

// F: the self-heal path. An existing installation whose profiles are all
// already connected but whose policy is stale repairs itself the moment
// Settings is read -- no disconnect/reconnect required. Get must also RETURN
// the repaired policy, so the UI renders the truth (requirement §8).
func TestGet_RepairsStalePolicyWithoutReconnect(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		ReviewerPriority: []domain.ProviderProfileID{claudeID},
	})

	got, err := svc.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []domain.ProviderProfileID{claudeID, codexID}
	wantIDs(t, "returned reviewer_priority", got.ReviewerPriority, want)
	wantIDs(t, "persisted reviewer_priority", readPolicy(t, store).ReviewerPriority, want)
}

// G: a disabled profile is not appended, and -- separately -- an already
// listed profile is never REMOVED when it becomes disabled. Removal is a
// deliberate non-goal of this checkpoint.
func TestSync_DisabledProfileIsNeitherAppendedNorRemoved(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, false) // disabled
	storePolicy(t, store, domain.UserExecutionPolicy{
		ReviewerPriority: []domain.ProviderProfileID{claudeID},
		WorkerPriority:   []domain.ProviderProfileID{claudeID, codexID},
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	got := readPolicy(t, store)
	wantIDs(t, "reviewer_priority", got.ReviewerPriority, []domain.ProviderProfileID{claudeID})
	wantIDs(t, "worker_priority", got.WorkerPriority, []domain.ProviderProfileID{claudeID, codexID})
}

// Empty-list semantics: migration 0112 stores every list as `DEFAULT '[]'`
// and has no marker separating "the user cleared this on purpose" from "this
// was generated empty before any capable profile existed". Extending only
// lists the user has actually populated is the safe reading -- it can never
// overwrite an explicit choice -- and matches RouteExecution's own
// completePriority rule.
func TestSync_EmptyListIsLeftEmpty(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		WorkerPriority:   []domain.ProviderProfileID{claudeID},
		ReviewerPriority: nil,
	})

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	got := readPolicy(t, store)
	if len(got.ReviewerPriority) != 0 {
		t.Fatalf("reviewer_priority = %v, want it left empty", got.ReviewerPriority)
	}
	wantIDs(t, "worker_priority", got.WorkerPriority, []domain.ProviderProfileID{claudeID, codexID})
}

// A user with no stored policy at all has nothing stale to repair, and
// reading Settings must not start creating policy rows as a side effect.
func TestSync_NoStoredPolicyIsANoOp(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)

	if err := svc.SyncPriorities(context.Background(), userID); err != nil {
		t.Fatalf("SyncPriorities: %v", err)
	}
	if _, ok, err := store.GetUserExecutionPolicyByUser(t.Context(), userID); err != nil || ok {
		t.Fatalf("policy row ok=%v err=%v, want none created", ok, err)
	}

	// Get still returns the bootstrap default, built from current profiles.
	got, err := svc.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.ReviewerPriority) != 2 {
		t.Fatalf("default reviewer_priority = %v, want both profiles", got.ReviewerPriority)
	}
	if _, ok, _ := store.GetUserExecutionPolicyByUser(t.Context(), userID); ok {
		t.Fatalf("Get must not persist a policy row")
	}
}

// A synced list must still pass the PUT validator, or the next Settings save
// would fail on a list the server itself wrote.
func TestSync_ResultSurvivesPutValidation(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	seedCodex(t, store, true)
	storePolicy(t, store, domain.UserExecutionPolicy{
		PlannerPriority:          []domain.ProviderProfileID{claudeID},
		WorkerPriority:           []domain.ProviderProfileID{claudeID},
		ReviewerPriority:         []domain.ProviderProfileID{claudeID},
		DecisionResolverPriority: []domain.ProviderProfileID{claudeID},
	})

	synced, err := svc.Get(context.Background(), userID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := svc.Put(context.Background(), userID, executionpolicy.PutInput{
		AutonomousMode:           synced.AutonomousMode,
		PlannerPriority:          synced.PlannerPriority,
		WorkerPriority:           synced.WorkerPriority,
		ReviewerPriority:         synced.ReviewerPriority,
		DecisionResolverPriority: synced.DecisionResolverPriority,
		FallbackBehavior:         synced.FallbackBehavior,
		ReviewIndependence:       synced.ReviewIndependence,
	}); err != nil {
		t.Fatalf("Put of a synced policy failed validation: %v", err)
	}
}

// The trigger wiring, end to end through the real providerprofile service:
// connecting a provider must repair the policy immediately, without the user
// opening Settings at all. This is what makes requirement §6's "do not
// require users to disconnect/reconnect Codex" hold in the other direction
// too -- a fresh connection is enough on its own.
func TestProviderProfileCreate_SyncsPolicy(t *testing.T) {
	svc, store := newSyncFixture(t)
	seedClaude(t, store)
	storePolicy(t, store, domain.UserExecutionPolicy{
		WorkerPriority:   []domain.ProviderProfileID{claudeID},
		ReviewerPriority: []domain.ProviderProfileID{claudeID},
	})

	profiles := &providerprofile.Service{
		Store:      store,
		Clock:      func() time.Time { return baseTime.Add(time.Minute) },
		IDFactory:  func() string { return string(codexID) },
		PolicySync: svc,
	}
	created, err := profiles.Create(context.Background(), userID, providerprofile.CreateInput{
		Provider: "openai", Harness: domain.HarnessCodex,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := readPolicy(t, store)
	want := []domain.ProviderProfileID{claudeID, created.ID}
	wantIDs(t, "worker_priority", got.WorkerPriority, want)
	wantIDs(t, "reviewer_priority", got.ReviewerPriority, want)
	// Codex is not planner-capable, so an untouched planner list stays empty.
	if len(got.PlannerPriority) != 0 {
		t.Fatalf("planner_priority = %v, want untouched", got.PlannerPriority)
	}
}

// A nil PolicySync keeps the exact pre-Checkpoint-8P-E.13A.5 behavior:
// creating a profile succeeds and simply leaves the policy alone.
func TestProviderProfileCreate_NilSyncerIsANoOp(t *testing.T) {
	_, store := newSyncFixture(t)
	seedClaude(t, store)
	storePolicy(t, store, domain.UserExecutionPolicy{
		ReviewerPriority: []domain.ProviderProfileID{claudeID},
	})

	profiles := &providerprofile.Service{Store: store, Clock: func() time.Time { return baseTime }}
	if _, err := profiles.Create(context.Background(), userID, providerprofile.CreateInput{
		Provider: "openai", Harness: domain.HarnessCodex,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantIDs(t, "reviewer_priority", readPolicy(t, store).ReviewerPriority,
		[]domain.ProviderProfileID{claudeID})
}
