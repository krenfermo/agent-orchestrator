package backup

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validManifest() *Manifest {
	return &Manifest{
		Format:      FormatV1,
		BackupID:    "aob-20260914T101530.123456789Z-1a2b3c4d",
		Kind:        KindManual,
		CreatedAt:   time.Date(2026, 9, 14, 10, 15, 30, 123456789, time.UTC),
		Tool:        ToolInfo{Name: "ao"},
		Source:      Source{DataDirFingerprint: strings.Repeat("a", 32), JournalMode: "wal"},
		Schema:      Schema{GooseVersion: 170, BinaryHead: 170},
		Method:      MethodVacuumInto,
		Consistency: ConsistencySingleReadTxn,
		Checks:      Checks{IntegrityCheck: "ok"},
		Assets: []Asset{
			{Path: DatabaseAsset, Role: RoleDatabase, Size: 10, SHA256: strings.Repeat("0", 64), Mode: "0600"},
		},
	}
}

func encode(t *testing.T, m any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseManifestAcceptsAValidV1(t *testing.T) {
	m, status, findings := ParseManifest(encode(t, validManifest()))
	if status != StatusValid || len(findings) != 0 || m == nil {
		t.Fatalf("status=%s findings=%v", status, findings)
	}
}

func TestParseManifestVersions(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   Status
		code   Code
	}{
		{"ao.backup/v2", StatusUnsupported, CodeUnsupportedManifest},
		{"ao.backup/v10", StatusUnsupported, CodeUnsupportedManifest},
		{"ao.backup/v1.1", StatusInvalid, CodeManifestInvalid},
		{"something-else", StatusInvalid, CodeManifestInvalid},
		{"", StatusInvalid, CodeManifestInvalid},
	} {
		m := validManifest()
		m.Format = tc.format
		_, status, findings := ParseManifest(encode(t, m))
		if status != tc.want || len(findings) == 0 || findings[0].Code != tc.code {
			t.Errorf("format %q: status=%s findings=%v, want %s/%s", tc.format, status, findings, tc.want, tc.code)
		}
	}
}

func TestParseManifestFailsClosed(t *testing.T) {
	withField := func(f func(map[string]any)) []byte {
		var raw map[string]any
		_ = json.Unmarshal(encode(t, validManifest()), &raw)
		f(raw)
		return encode(t, raw)
	}
	cases := map[string][]byte{
		"not json":      []byte("{nope"),
		"unknown field": withField(func(r map[string]any) { r["token"] = "x" }),
		"trailing data": append(encode(t, validManifest()), []byte(` {"x":1}`)...),
		"oversized":     []byte(`{"format":"ao.backup/v1","notes":"` + strings.Repeat("x", maxManifestBytes) + `"}`),
	}
	for name, data := range cases {
		if _, status, _ := ParseManifest(data); status != StatusInvalid {
			t.Errorf("%s: status=%s, want INVALID", name, status)
		}
	}
}

func TestManifestAssetRules(t *testing.T) {
	db := Asset{Path: DatabaseAsset, Role: RoleDatabase, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"}
	skill := func(p string) Asset {
		return Asset{Path: p, Role: RoleSkillPackage, Size: 1, SHA256: strings.Repeat("1", 64), Mode: "0600"}
	}
	cases := map[string][]Asset{
		"traversal":             {db, skill("skills/catalog/../../etc/passwd")},
		"parent escape":         {db, skill("../../foo")},
		"absolute":              {db, skill("/etc/passwd")},
		"backslash":             {db, skill(`skills\catalog\x`)},
		"not canonical":         {db, skill("skills/catalog//x")},
		"dot segment":           {db, skill("skills/catalog/./x")},
		"outside catalog":       {db, skill("prompts/x.md")},
		"duplicate":             {db, skill("skills/catalog/a"), skill("skills/catalog/a")},
		"case collision":        {db, skill("skills/catalog/A"), skill("skills/catalog/a")},
		"file and parent":       {db, skill("skills/catalog/a"), skill("skills/catalog/a/b")},
		"no database":           {skill("skills/catalog/a")},
		"two databases":         {db, db},
		"database elsewhere":    {{Path: "data/ao.db", Role: RoleDatabase, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"}},
		"manifest name":         {db, {Path: ManifestName, Role: RoleSkillPackage, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"}},
		"bad sha":               {{Path: DatabaseAsset, Role: RoleDatabase, Size: 1, SHA256: "abc", Mode: "0600"}},
		"bad mode":              {{Path: DatabaseAsset, Role: RoleDatabase, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "777"}},
		"negative size":         {{Path: DatabaseAsset, Role: RoleDatabase, Size: -1, SHA256: strings.Repeat("0", 64), Mode: "0600"}},
		"unknown role":          {db, {Path: "skills/catalog/x", Role: "credentials", Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"}},
		"identity without id":   {db, {Path: IdentityAsset, Role: RoleInstallationIdentity, Size: 1, SHA256: strings.Repeat("0", 64), Mode: "0600"}},
		"sidecar via traversal": {db, skill("skills/catalog/ao.db-wal"), skill("skills/catalog/../ao.db-wal")},
	}
	for name, assets := range cases {
		m := validManifest()
		m.Assets = assets
		if _, status, findings := ParseManifest(encode(t, m)); status != StatusInvalid || len(findings) == 0 {
			t.Errorf("%s: status=%s findings=%v, want INVALID", name, status, findings)
		}
	}
}

func TestValidateAssetPath(t *testing.T) {
	for _, p := range []string{"ao.db", "installation_id", "skills/catalog/pkg/SKILL.md", "skills/catalog/.hidden"} {
		if err := ValidateAssetPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{"", "/abs", "../x", "a/../b", "a/./b", "a//b", "a/", "C:x", `a\b`, "a\x00b", strings.Repeat("a", 2000)} {
		if err := ValidateAssetPath(p); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestBackupIDsAreSortableAndDistinct(t *testing.T) {
	now := time.Date(2026, 9, 14, 10, 0, 0, 0, time.FixedZone("CEST", 2*3600))
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 200; i++ {
		id, err := NewBackupID(now.Add(time.Duration(i) * time.Nanosecond))
		if err != nil {
			t.Fatal(err)
		}
		if !ValidBackupID(id) || seen[id] {
			t.Fatalf("id %q invalid or repeated", id)
		}
		if !strings.HasPrefix(id, "aob-20260914T080000.") {
			t.Fatalf("id %q is not UTC", id)
		}
		if prev != "" && id[:len("aob-20260914T080000.000000000Z")] < prev[:len("aob-20260914T080000.000000000Z")] {
			t.Fatalf("ids not sortable: %s after %s", id, prev)
		}
		seen[id], prev = true, id
	}
	// Same instant: the random suffix still separates them.
	a, _ := NewBackupID(now)
	b, _ := NewBackupID(now)
	if a == b {
		t.Fatal("two ids minted in the same instant collided")
	}
}

func TestCompatibility(t *testing.T) {
	for _, tc := range []struct {
		goose, head int64
		want        Compatibility
	}{{170, 170, CompatCompatible}, {169, 170, CompatUpgradeRequired}, {171, 170, CompatNewerThanBinary}, {0, 170, CompatUnknown}} {
		if got := compatibility(tc.goose, tc.head); got != tc.want {
			t.Errorf("compatibility(%d,%d)=%s want %s", tc.goose, tc.head, got, tc.want)
		}
	}
}

func TestProtectedKinds(t *testing.T) {
	if KindManual.Protected() || !KindPreRestore.Protected() || !KindPreMigration.Protected() {
		t.Fatal("only pre-restore and pre-migration backups are protected")
	}
}
