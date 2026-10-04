package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

func TestPriorSupervisorEndpoint_onlyAnExitedPredecessorIsCleanable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "running.json")
	write := func(info runfile.Info) {
		t.Helper()
		if err := runfile.Write(path, info); err != nil {
			t.Fatal(err)
		}
	}
	base := runfile.Info{PID: 4242, Port: 3002, StartedAt: time.Now().UTC(), InstanceID: "aod-1", SupervisorAddress: filepath.Join(dir, "supervise-x.sock")}
	dead := func(int) bool { return false }
	live := func(int) bool { return true }

	if got := priorSupervisorEndpoint(path, dead); got != nil {
		t.Fatalf("no run-file: prior = %+v, want nil", got)
	}

	write(base)
	if got := priorSupervisorEndpoint(path, dead); got == nil || !got.Exited || got.InstanceID != "aod-1" || got.Address != base.SupervisorAddress {
		t.Fatalf("dead predecessor: prior = %+v", got)
	}
	// A live (or reused) PID is never marked exited: its endpoint is not even probed.
	if got := priorSupervisorEndpoint(path, live); got == nil || got.Exited {
		t.Fatalf("live predecessor: prior = %+v, want Exited=false", got)
	}

	noAddr := base
	noAddr.SupervisorAddress = ""
	write(noAddr)
	if got := priorSupervisorEndpoint(path, dead); got != nil {
		t.Fatalf("predecessor without an endpoint: prior = %+v, want nil", got)
	}

	noID := base
	noID.InstanceID = ""
	write(noID)
	if got := priorSupervisorEndpoint(path, dead); got != nil {
		t.Fatalf("predecessor without an instance identity: prior = %+v, want nil", got)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := priorSupervisorEndpoint(path, dead); got != nil {
		t.Fatalf("unreadable run-file: prior = %+v, want nil", got)
	}
}
