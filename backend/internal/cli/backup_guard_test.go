package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonlock"
)

func writeRestoreJournal(t *testing.T, dataDir, phase string) {
	t.Helper()
	id := "aor-20260914T120000.000000000Z-0badc0de"
	body := `{"format":"ao.restore-journal/v1","restoreId":"` + id + `",` +
		`"sourceBackupId":"aob-20260914T110000.000000000Z-00000000","sourcePath":"/nowhere",` +
		`"workDir":".ao-restore-` + id + `","phase":"` + phase + `","updatedAt":"2026-09-14T12:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dataDir, ".ao-restore-journal.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// P10 review F3 — the offline writers (`ao import`, `ao usage
// backfill-cache-ttl`) refuse a data dir an interrupted restore may have left
// holding two states, exactly as the daemon's boot does, and release the lock.
func TestHoldDataDirOfflineRefusesAnInterruptedRestore(t *testing.T) {
	dir := t.TempDir()
	newTestDB(t, dir)
	cfg := config.Config{DataDir: dir, RunFilePath: filepath.Join(dir, "running.json")}

	writeRestoreJournal(t, dir, "swapping")
	if _, err := holdDataDirOffline(cfg, "importing"); ExitCode(err) != 2 || !strings.Contains(err.Error(), "ao backup recover") {
		t.Fatalf("offline write over a mid-swap restore: %v", err)
	}
	l, err := daemonlock.Acquire(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		t.Fatalf("the refusal kept daemon.lock: %v", err)
	}
	_ = l.Release()

	// A restore that never reached its swap changed nothing: no refusal.
	writeRestoreJournal(t, dir, "staged")
	release, err := holdDataDirOffline(cfg, "importing")
	if err != nil {
		t.Fatalf("a pre-swap journal blocked an offline command: %v", err)
	}
	release()
}
