package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// Phase is where a restore is. The journal records it durably BEFORE the step
// it names begins, so after a crash the journal never claims less than happened.
type Phase string

// Restore phases, in order.
const (
	PhasePreparing      Phase = "preparing"
	PhaseRollbackReady  Phase = "rollback_ready"
	PhaseStaged         Phase = "staged"
	PhaseSwapping       Phase = "swapping"
	PhaseSwapped        Phase = "swapped"
	PhaseRollingBack    Phase = "rolling_back"
	PhaseRollbackFailed Phase = "rollback_failed"
	PhaseRolledBack     Phase = "rolled_back"
	PhaseComplete       Phase = "complete"
)

// critical reports whether the data dir may hold a mix of the old and restored
// states. Before swapping nothing in the data dir was replaced; rolled_back and
// complete are both whole states with only cleanup pending.
func (p Phase) critical() bool {
	switch p {
	case PhaseSwapping, PhaseSwapped, PhaseRollingBack, PhaseRollbackFailed:
		return true
	}
	return false
}

func (p Phase) known() bool {
	switch p {
	case PhasePreparing, PhaseRollbackReady, PhaseStaged, PhaseSwapping, PhaseSwapped,
		PhaseRollingBack, PhaseRollbackFailed, PhaseRolledBack, PhaseComplete:
		return true
	}
	return false
}

const journalFormat = "ao.restore-journal/v1"

var restoreIDPattern = regexp.MustCompile(`^aor-\d{8}T\d{6}\.\d{9}Z-[0-9a-f]{8}$`)

// sqliteSidecars are the files SQLite keeps beside ao.db. Moved aside FIRST,
// so a crash mid-swap can never leave a sidecar next to a database it does not
// belong to without the journal being able to tell.
var sqliteSidecars = []string{DatabaseAsset + "-wal", DatabaseAsset + "-shm", DatabaseAsset + "-journal"}

// managedEntries are the only data-dir entries a restore moves, in swap order.
// Nothing else in the data dir is ever touched.
var managedEntries = append(slices.Clone(sqliteSidecars), DatabaseAsset, IdentityAsset, SkillCatalogPath)

var promotableEntries = []string{DatabaseAsset, IdentityAsset, SkillCatalogPath}

type fileStamp struct {
	Size            int64 `json:"size"`
	ModTimeUnixNano int64 `json:"modTimeUnixNano"`
}

func stampOf(fi os.FileInfo) *fileStamp {
	return &fileStamp{Size: fi.Size(), ModTimeUnixNano: fi.ModTime().UnixNano()}
}

// journal is <data>/.ao-restore-journal.json.
type journal struct {
	Format             string     `json:"format"`
	RestoreID          string     `json:"restoreId"`
	SourceBackupID     string     `json:"sourceBackupId"`
	SourcePath         string     `json:"sourcePath"`
	RollbackBackupID   string     `json:"rollbackBackupId,omitempty"`
	RollbackBackupPath string     `json:"rollbackBackupPath,omitempty"`
	WorkDir            string     `json:"workDir"`
	Phase              Phase      `json:"phase"`
	Promote            []string   `json:"promote,omitempty"`
	PreExisting        []string   `json:"preExisting,omitempty"`
	PreDatabase        *fileStamp `json:"preDatabase,omitempty"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

// validate refuses a journal that would make recovery move anything other than
// the managed entries inside the data dir -- a crafted or corrupted journal
// must never be able to point a rename somewhere else.
func (j *journal) validate() error {
	switch {
	case j.Format != journalFormat:
		return fmt.Errorf("unknown journal format %q", j.Format)
	case !restoreIDPattern.MatchString(j.RestoreID):
		return fmt.Errorf("malformed restore id %q", j.RestoreID)
	case j.WorkDir != restoreWorkPrefix+j.RestoreID:
		return fmt.Errorf("work dir %q does not belong to restore %s", j.WorkDir, j.RestoreID)
	case !j.Phase.known():
		return fmt.Errorf("unknown phase %q", j.Phase)
	}
	for _, p := range j.Promote {
		if !slices.Contains(promotableEntries, p) {
			return fmt.Errorf("journal promotes %q, which a restore never replaces", p)
		}
	}
	for _, p := range j.PreExisting {
		if !slices.Contains(managedEntries, p) {
			return fmt.Errorf("journal records %q, which a restore never moves", p)
		}
	}
	return nil
}

// unsettled reports whether the data dir may hold a mix of two states: the
// phase is critical, or it reads "complete" although a rollback has started
// since (a rollback creates failed/ before it moves anything). The second is a
// completion record that reached the disk while its write reported failure,
// which the rollback that followed could not overwrite.
func (j *journal) unsettled(dataDir string) bool {
	if j.Phase.critical() {
		return true
	}
	if j.Phase == PhaseComplete {
		if _, err := os.Lstat(filepath.Join(dataDir, j.WorkDir, "failed")); err == nil {
			return true
		}
	}
	return false
}

func writeJournal(dataDir string, j *journal) error {
	j.Format = journalFormat
	j.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dataDir, journalName), append(data, '\n'), 0o600)
}

// readJournal returns (nil, nil) when no restore is recorded.
func readJournal(dataDir string) (*journal, error) {
	f, err := openRegular(filepath.Join(dataDir, journalName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, errors.New("parse restore journal: not valid JSON")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, fmt.Errorf("parse restore journal: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var j journal
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("parse restore journal: %w", err)
	}
	if err := j.validate(); err != nil {
		return nil, fmt.Errorf("restore journal: %w", err)
	}
	return &j, nil
}

func removeJournal(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, journalName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(dataDir)
}

// CheckStartup is the daemon's boot gate. It must run after the daemon holds
// daemon.lock (so no live restore is writing the journal) and before the store
// opens. A restore interrupted while the data dir may mix two states refuses
// the boot; a restore interrupted before it replaced anything does not, since
// its leftover staging is harmless.
func CheckStartup(dataDir string) error {
	j, err := readJournal(dataDir)
	if err != nil {
		return fmt.Errorf("a restore journal exists in %s but cannot be trusted (%w); run `ao backup recover` before starting AO", dataDir, err)
	}
	if j != nil && j.unsettled(dataDir) {
		return fmt.Errorf("restore %s was interrupted in phase %s and the data dir %s may hold a mix of two states; run `ao backup recover` before starting AO",
			j.RestoreID, j.Phase, dataDir)
	}
	return nil
}
