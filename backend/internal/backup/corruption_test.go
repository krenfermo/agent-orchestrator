package backup

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func testTime() time.Time { return time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC) }

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, _ := d.Info()
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func editManifest(t *testing.T, dir string, fn func(m *Manifest)) {
	t.Helper()
	p := filepath.Join(dir, ManifestName)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	fn(&m)
	out, _ := json.MarshalIndent(&m, "", "  ")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func flipByte(t *testing.T, p string, at func(size int64) int64) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi, _ := f.Stat()
	off := at(fi.Size())
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, off); err != nil {
		t.Fatal(err)
	}
	buf[0] ^= 0xFF
	if _, err := f.WriteAt(buf, off); err != nil {
		t.Fatal(err)
	}
}

// §25/§D — every kind of damage fails verification closed, and a restore from
// it refuses before touching the destination.
func TestCorruptBackupsFailClosedAndNeverRestore(t *testing.T) {
	f := newFixture(t)
	skillFile := "skills/catalog/pkg-a/SKILL.md"

	cases := []struct {
		name   string
		damage func(t *testing.T, dir string) string // returns the path to verify
		status Status
		codes  []Code
	}{
		{"db byte flipped", func(t *testing.T, d string) string {
			flipByte(t, filepath.Join(d, DatabaseAsset), func(n int64) int64 { return n / 2 })
			return d
		}, StatusInvalid, []Code{CodeHashMismatch}},
		{"db truncated", func(t *testing.T, d string) string {
			fi, _ := os.Stat(filepath.Join(d, DatabaseAsset))
			if err := os.Truncate(filepath.Join(d, DatabaseAsset), fi.Size()/2); err != nil {
				t.Fatal(err)
			}
			return d
		}, StatusInvalid, []Code{CodeSizeMismatch}},
		{"db missing", func(t *testing.T, d string) string {
			_ = os.Remove(filepath.Join(d, DatabaseAsset))
			return d
		}, StatusInvalid, []Code{CodeAssetMissing}},
		{"manifest missing", func(t *testing.T, d string) string {
			_ = os.Remove(filepath.Join(d, ManifestName))
			return d
		}, StatusInvalid, []Code{CodeManifestMissing}},
		{"manifest garbage", func(t *testing.T, d string) string {
			_ = os.WriteFile(filepath.Join(d, ManifestName), []byte("{\"format\":"), 0o600)
			return d
		}, StatusInvalid, []Code{CodeManifestInvalid}},
		{"manifest hash wrong", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) {
				for i := range m.Assets {
					if m.Assets[i].Role == RoleDatabase {
						m.Assets[i].SHA256 = strings.Repeat("a", 64)
					}
				}
			})
			return d
		}, StatusInvalid, []Code{CodeHashMismatch}},
		{"wrong goose metadata", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) { m.Schema.GooseVersion-- })
			return d
		}, StatusInvalid, []Code{CodeSchemaMismatch}},
		{"unsupported manifest version", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) { m.Format = "ao.backup/v2" })
			return d
		}, StatusUnsupported, []Code{CodeUnsupportedManifest}},
		{"asset hash mismatch", func(t *testing.T, d string) string {
			p := filepath.Join(d, filepath.FromSlash(skillFile))
			b, _ := os.ReadFile(p)
			b[0] ^= 0x01
			_ = os.WriteFile(p, b, 0o600)
			return d
		}, StatusInvalid, []Code{CodeHashMismatch}},
		{"identity replaced", func(t *testing.T, d string) string {
			_ = os.WriteFile(filepath.Join(d, IdentityAsset), []byte("aoi-"+uuid.NewString()+"\n"), 0o600)
			return d
		}, StatusInvalid, []Code{CodeHashMismatch}},
		{"path traversal asset", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) {
				m.Assets = append(m.Assets, Asset{Path: "../../foo", Role: RoleSkillPackage, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"})
			})
			return d
		}, StatusInvalid, []Code{CodeUnsafePath}},
		{"absolute asset", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) {
				m.Assets = append(m.Assets, Asset{Path: "/etc/passwd", Role: RoleSkillPackage, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"})
			})
			return d
		}, StatusInvalid, []Code{CodeUnsafePath}},
		{"duplicate asset", func(t *testing.T, d string) string {
			editManifest(t, d, func(m *Manifest) { m.Assets = append(m.Assets, m.Assets[0]) })
			return d
		}, StatusInvalid, []Code{CodeManifestInvalid}},
		{"stray wal beside the snapshot", func(t *testing.T, d string) string {
			_ = os.WriteFile(filepath.Join(d, DatabaseAsset+"-wal"), []byte("junk"), 0o600)
			return d
		}, StatusInvalid, []Code{CodeStraySidecar}},
		{"unexpected file", func(t *testing.T, d string) string {
			_ = os.WriteFile(filepath.Join(d, "notes.txt"), []byte("x"), 0o600)
			return d
		}, StatusInvalid, []Code{CodeAssetUnexpected}},
		{"asset replaced by a symlink", func(t *testing.T, d string) string {
			p := filepath.Join(d, filepath.FromSlash(skillFile))
			outside := filepath.Join(t.TempDir(), "SKILL.md")
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(outside, b, 0o600)
			_ = os.Remove(p)
			if err := os.Symlink(outside, p); err != nil {
				t.Fatal(err)
			}
			return d
		}, StatusInvalid, []Code{CodeSymlinkRejected}},
		{"extra partial staging", func(t *testing.T, d string) string {
			staging := filepath.Join(filepath.Dir(d), stagingPrefix+filepath.Base(d))
			if err := os.Rename(d, staging); err != nil {
				t.Fatal(err)
			}
			return staging
		}, StatusInvalid, []Code{CodeStagingIncomplete}},
		{"corruption re-hashed into the manifest", func(t *testing.T, d string) string {
			p := filepath.Join(d, DatabaseAsset)
			// Destroy the schema table's page, then make the manifest agree with
			// the damaged bytes: only SQLite's own checks can catch this.
			fh, _ := os.OpenFile(p, os.O_RDWR, 0)
			_, _ = fh.WriteAt([]byte(strings.Repeat("\xff", 400)), 100)
			_ = fh.Close()
			n, sum, err := hashFile(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			editManifest(t, d, func(m *Manifest) {
				for i := range m.Assets {
					if m.Assets[i].Role == RoleDatabase {
						m.Assets[i].Size, m.Assets[i].SHA256 = n, sum
					}
				}
			})
			return d
		}, StatusInvalid, []Code{CodeIntegrityFailed, CodeDBUnreadable}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), filepath.Base(f.backupA.Path))
			copyTree(t, f.backupA.Path, dir)
			target := tc.damage(t, dir)

			rep, err := Verify(context.Background(), target, VerifyOptions{})
			if err != nil {
				t.Fatalf("verify errored instead of reporting: %v", err)
			}
			if rep.Status != tc.status || rep.Restorable() {
				t.Fatalf("status=%s reasons=%+v, want %s", rep.Status, rep.Reasons, tc.status)
			}
			found := false
			for _, c := range tc.codes {
				found = found || hasReason(rep, c)
			}
			if !found {
				t.Fatalf("reasons %+v lack %v", rep.Reasons, tc.codes)
			}

			before := managedDigest(t, f.dataDir)
			rrep, err := f.restore(t, func(o *RestoreOptions) { o.Source = target })
			if err == nil || rrep.Result != ResultRefused || rrep.DestinationTouched {
				t.Fatalf("restore from a damaged backup: err=%v rep=%+v", err, rrep)
			}
			if c := classOf(err); c != ClassInvalid && c != ClassIncompatible {
				t.Fatalf("class=%s err=%v", c, err)
			}
			if managedDigest(t, f.dataDir) != before {
				t.Fatal("a refused restore changed the destination")
			}
			assertNoRestoreLeftovers(t, f.dataDir)
		})
	}
	if countBackups(t, f.root, KindPreRestore) != 0 {
		t.Fatal("a damaged backup got as far as a pre-restore backup")
	}
}

// Verify never writes: not into the backup, not beside it.
func TestVerifyWritesNothing(t *testing.T) {
	f := newFixture(t)
	parentBefore := treeDigest(t, filepath.Dir(f.backupA.Path))
	if _, err := Verify(context.Background(), f.backupA.Path, VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
	if treeDigest(t, filepath.Dir(f.backupA.Path)) != parentBefore {
		t.Fatal("verify changed the backup root")
	}
}
