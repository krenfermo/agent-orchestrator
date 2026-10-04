package sessionmanager

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/workerownership"
)

// ObserveWorkerRuntime proves (or fails to prove) that the runtime answering
// for a session is the one AO launched for it (P9). Read-only: it never
// destroys, restarts or writes anything.
//
// A runtime that cannot read identity back (conpty) answers "unsupported",
// which every recovery path treats as fail-closed rather than as proof.
func (m *Manager) ObserveWorkerRuntime(ctx context.Context, id domain.SessionID) domain.WorkerRuntimeObservation {
	rec, ok, err := m.store.GetSession(ctx, id)
	switch {
	case err != nil:
		return domain.WorkerRuntimeObservation{SessionID: id, Proof: domain.WorkerRuntimeUnavailable,
			Detail: fmt.Sprintf("session %s could not be read", id)}
	case !ok:
		return domain.WorkerRuntimeObservation{SessionID: id, Proof: domain.WorkerRuntimeUnavailable,
			Detail: fmt.Sprintf("session %s has no row to compare the runtime against", id)}
	}
	var reader ports.SessionFactsReader
	if r, ok := m.runtime.(ports.SessionFactsReader); ok {
		reader = r
	}
	return workerownership.Classify(ctx, rec, reader, m.installationID)
}
