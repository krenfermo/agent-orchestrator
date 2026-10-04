package workflow_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
)

// laneStubExternal is an in-memory integration lane for the external test
// package, mirroring the internal package's laneStub.
//
// It is deliberately a real lane and not a way around one. Task 5 made the
// Integration Coordinator the single authoritative path every ready task takes,
// and a stub that handed out the lane unconditionally — or a fixture that
// skipped the coordinator to stay green — would put back exactly the second
// promotion route the reviewer found. So this enforces the property the lane
// exists for: one holder per repository+branch at a time, and a busy lane
// returns integration.ErrLockBusy rather than blocking or pretending.
type laneStubExternal struct {
	mu   sync.Mutex
	held map[string]string // lane key -> handle id
	seq  int
}

func newLaneStubExternal() *laneStubExternal {
	return &laneStubExternal{held: map[string]string{}}
}

func (l *laneStubExternal) key(req integration.LockRequest) string {
	return req.RepoPath + "\x00" + req.TargetBranch
}

func (l *laneStubExternal) Acquire(_ context.Context, req integration.LockRequest) (integration.LockHandle, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.key(req)
	if _, taken := l.held[k]; taken {
		return integration.LockHandle{}, integration.ErrLockBusy
	}
	l.seq++
	id := fmt.Sprintf("lane-%d", l.seq)
	l.held[k] = id
	return integration.LockHandle{ID: id}, nil
}

func (l *laneStubExternal) Release(_ context.Context, handle integration.LockHandle, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, id := range l.held {
		if id == handle.ID {
			delete(l.held, k)
			return nil
		}
	}
	return nil
}
