package workflow

import (
	stdctx "context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// store_decorator_contract_test.go — the invariant a Store decorator must keep.
//
// The Coordinator obtains its plan store by type-asserting its Store to the
// UNEXPORTED masterPlanStore interface (`d.Store.(masterPlanStore)`). A
// decorator that wraps the Store — P4-E's does, to announce every lifecycle
// transition to the external work-item sync — therefore has to satisfy that
// interface too, or the assertion fails and the master coordinator silently
// runs with no plan store at all. Nothing announces that failure: master runs
// would simply stop planning.
//
// So the invariant is pinned here, in the package that owns the assertion,
// against the SHAPE every decorator must have: embed the concrete *sqlite.Store
// so all twenty-odd plan methods are promoted, and override only the
// transitions. Switching the embed to a narrower interface breaks this test
// rather than production.

// shapedLikeSyncingStore mirrors daemon.syncingStore: the concrete store
// embedded, with exactly the methods P4-E overrides.
type shapedLikeSyncingStore struct {
	*sqlite.Store
}

func (s *shapedLikeSyncingStore) UpdateWorkflowRunState(
	ctx stdctx.Context, id string, expected, next domain.WorkflowRunState, now time.Time,
) (bool, error) {
	return s.Store.UpdateWorkflowRunState(ctx, id, expected, next, now)
}

func (s *shapedLikeSyncingStore) UpdateWorkflowTaskState(
	ctx stdctx.Context, id string, expected, next domain.WorkflowTaskState, now time.Time,
) (bool, error) {
	return s.Store.UpdateWorkflowTaskState(ctx, id, expected, next, now)
}

func (s *shapedLikeSyncingStore) ParkWorkflowTaskForAttention(
	ctx stdctx.Context, id string, expected domain.WorkflowTaskState, expectedAttempt int,
	reason string, attention domain.WorkflowTaskAttention, now time.Time,
) (bool, error) {
	return s.Store.ParkWorkflowTaskForAttention(ctx, id, expected, expectedAttempt, reason, attention, now)
}

func TestAStoreDecoratorStillSatisfiesTheMasterPlanStore(t *testing.T) {
	var decorated Store = &shapedLikeSyncingStore{}
	if _, ok := decorated.(masterPlanStore); !ok {
		t.Fatal("a Store decorator no longer satisfies masterPlanStore; " +
			"the master coordinator would run with no plan store and never say so")
	}
}
