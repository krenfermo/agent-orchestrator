package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Independent review §23 (MEDIUM): a destination inside the backup being
// restored was accepted -- verify ran before the data dir existed, and the
// restore then wrote the data dir INTO the backup, which from then on failed
// verification. The same for a pre-restore root inside it. Both now refuse
// before anything is created, and the backup stays VALID and byte-identical.
func TestRestoreRefusesToWriteIntoItsOwnSource(t *testing.T) {
	for _, tc := range []struct {
		name          string
		dataDir, root func(f *fixture) string
		created       func(f *fixture) string
	}{
		{"data dir inside the backup",
			func(f *fixture) string { return filepath.Join(f.backupA.Path, "data") },
			func(f *fixture) string { return filepath.Join(f.parent, "other-root") },
			func(f *fixture) string { return filepath.Join(f.backupA.Path, "data") }},
		{"pre-restore root inside the backup",
			func(f *fixture) string { return f.dataDir },
			func(f *fixture) string { return filepath.Join(f.backupA.Path, "roots") },
			func(f *fixture) string { return filepath.Join(f.backupA.Path, "roots") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			before := treeDigest(t, f.backupA.Path)
			_, err := Restore(context.Background(), RestoreOptions{DataDir: tc.dataDir(f), Source: f.backupA.Path, Root: tc.root(f),
				CheckDaemon: noDaemon, AllowSecretKeyMismatch: true, Tool: ToolInfo{Name: "ao-test"}})
			if codeOf(err) != CodeDestinationInsideSource || classOf(err) != ClassRefused {
				t.Fatalf("err=%v", err)
			}
			if _, err := os.Lstat(tc.created(f)); err == nil {
				t.Fatal("the refused restore created a directory inside its source")
			}
			if treeDigest(t, f.backupA.Path) != before {
				t.Fatal("the source backup changed")
			}
			if vr, err := Verify(context.Background(), f.backupA.Path, VerifyOptions{}); err != nil || vr.Status != StatusValid {
				t.Fatalf("the source backup no longer verifies: %+v %v", vr, err)
			}
		})
	}
}

// Independent review §18 (LOW): encoding/json keeps the LAST of two equal keys,
// so a manifest could read as "manual" here and "pre-restore" -- or v1 here and
// v2 -- to a reader that keeps the first. Ambiguous documents are INVALID.
func TestManifestAndJournalRejectDuplicateKeys(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	res := mustCreate(t, dataDir, filepath.Join(parent, "backups"))
	raw, err := os.ReadFile(filepath.Join(res.Path, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for name, doc := range map[string]string{
		"kind":             strings.Replace(s, `"kind": "manual"`, `"kind": "pre-restore", "kind": "manual"`, 1),
		"format":           strings.Replace(s, "{", `{"format": "ao.backup/v2",`, 1),
		"nested in assets": strings.Replace(s, `"role": "database"`, `"role": "skill_package", "role": "database"`, 1),
	} {
		if doc == s {
			t.Fatalf("%s: fixture replacement did not apply", name)
		}
		if _, status, findings := ParseManifest([]byte(doc)); status != StatusInvalid || len(findings) == 0 || findings[0].Code != CodeManifestInvalid {
			t.Errorf("%s: status=%s findings=%v", name, status, findings)
		}
	}
	if _, status, _ := ParseManifest(raw); status != StatusValid {
		t.Fatal("the untouched manifest no longer parses")
	}
	if err := os.WriteFile(filepath.Join(res.Path, ManifestName), []byte(strings.Replace(s, `"kind": "manual"`, `"kind": "pre-restore", "kind": "manual"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if vr, err := Verify(context.Background(), res.Path, VerifyOptions{}); err != nil || vr.Status != StatusInvalid {
		t.Fatalf("verify of an ambiguous manifest: %+v %v", vr, err)
	}

	id, _ := newID("aor-", testTime())
	if err := writeJournal(dataDir, &journal{RestoreID: id, WorkDir: restoreWorkPrefix + id, Phase: PhaseStaged}); err != nil {
		t.Fatal(err)
	}
	jp := filepath.Join(dataDir, journalName)
	jraw, _ := os.ReadFile(jp)
	ambiguous := strings.Replace(string(jraw), `"phase": "staged"`, `"phase": "swapping", "phase": "staged"`, 1)
	if ambiguous == string(jraw) {
		t.Fatal("journal replacement did not apply")
	}
	if err := os.WriteFile(jp, []byte(ambiguous), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(dataDir); err == nil {
		t.Fatal("an ambiguous journal was accepted")
	}
	if CheckStartup(dataDir) == nil {
		t.Fatal("the boot gate accepted an ambiguous journal")
	}
}
