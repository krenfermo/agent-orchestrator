package backup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

const (
	// FormatV1 is the only manifest format this binary reads or writes. Any
	// other ao.backup/vN is UNSUPPORTED: a parser that guessed at a future
	// format could certify a backup it does not understand.
	FormatV1     = "ao.backup/v1"
	ManifestName = "manifest.json"

	DatabaseAsset    = "ao.db"
	IdentityAsset    = daemonmeta.InstallationIDFile
	SkillCatalogPath = "skills/catalog"
	secretKeyFile    = "secret.key"

	MethodVacuumInto         = "vacuum_into"
	ConsistencySingleReadTxn = "single_read_transaction"

	maxManifestBytes = 4 << 20
	maxAssets        = 10000
	maxAssetPathLen  = 1024
)

// Kind says why a backup exists. Only manual backups are ever pruned.
type Kind string

const (
	KindManual       Kind = "manual"
	KindPreRestore   Kind = "pre-restore"
	KindPreMigration Kind = "pre-migration"
)

func (k Kind) valid() bool {
	return k == KindManual || k == KindPreRestore || k == KindPreMigration
}

// Protected reports whether retention must never delete a backup of this kind.
func (k Kind) Protected() bool { return k == KindPreRestore || k == KindPreMigration }

// Role says what an asset is, and constrains where it may live.
type Role string

const (
	RoleDatabase             Role = "database"
	RoleInstallationIdentity Role = "installation_identity"
	RoleSkillPackage         Role = "skill_package"
)

// Manifest is manifest.json. It carries no absolute paths, environment,
// credentials or prompt text, and never its own hash.
type Manifest struct {
	Format      string    `json:"format"`
	BackupID    string    `json:"backupId"`
	Kind        Kind      `json:"kind"`
	CreatedAt   time.Time `json:"createdAt"`
	Tool        ToolInfo  `json:"tool"`
	Source      Source    `json:"source"`
	Schema      Schema    `json:"schema"`
	Method      string    `json:"method"`
	Consistency string    `json:"consistency"`
	Checks      Checks    `json:"checks"`
	Assets      []Asset   `json:"assets"`
	Notes       string    `json:"notes,omitempty"`
}

// ToolInfo identifies the binary that wrote the backup.
type ToolInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// Source describes where the backup came from without naming a path.
type Source struct {
	// DataDirFingerprint lets a restore notice it targets another data dir
	// (the database stores absolute paths into its own data dir).
	DataDirFingerprint string `json:"dataDirFingerprint"`
	// InstallationID is the P9 installation identity. It is not a secret: the
	// daemon already publishes it in running.json and its logs.
	InstallationID string `json:"installationId,omitempty"`
	// SecretKeyFingerprint is a one-way fingerprint of secret.key, empty when
	// the source had none. The key itself is never backed up.
	SecretKeyFingerprint string `json:"secretKeyFingerprint,omitempty"`
	// JournalMode is the source database's journal mode as observed.
	JournalMode string `json:"journalMode"`
}

// Schema records the goose version of the snapshot and the binary's head.
type Schema struct {
	GooseVersion int64 `json:"gooseVersion"`
	BinaryHead   int64 `json:"binaryHead"`
}

// Checks are the results of the checks run on the snapshot at creation.
type Checks struct {
	IntegrityCheck       string `json:"integrityCheck"`
	ForeignKeyViolations int    `json:"foreignKeyViolations"`
}

// Asset is one file of the backup.
type Asset struct {
	Path   string `json:"path"`
	Role   Role   `json:"role"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

var (
	formatPattern      = regexp.MustCompile(`^ao\.backup/v[0-9]+$`)
	backupIDPattern    = regexp.MustCompile(`^aob-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{8}$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	modePattern        = regexp.MustCompile(`^0[0-7]{3}$`)
)

// NewBackupID mints a sortable, collision-safe id: UTC with nanoseconds plus
// 32 random bits, so concurrent creates in the same instant never collide.
func NewBackupID(now time.Time) (string, error) {
	return newID("aob-", now)
}

func newID(prefix string, now time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return prefix + now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(b[:]), nil
}

// ValidBackupID reports whether id has the shape NewBackupID produces.
func ValidBackupID(id string) bool { return backupIDPattern.MatchString(id) }

// fingerprint is a domain-separated, truncated SHA-256: enough to compare two
// values for equality, useless for recovering either.
func fingerprint(label string, value []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, FormatV1+" "+label+"\x00")
	_, _ = h.Write(value)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ParseManifest decodes and validates manifest bytes. It never returns VALID
// with findings, and never guesses: unknown fields, trailing data, an unknown
// format and a future format all fail closed.
func ParseManifest(data []byte) (*Manifest, Status, []Finding) {
	invalid := func(code Code, format string, args ...any) (*Manifest, Status, []Finding) {
		return nil, StatusInvalid, []Finding{{Code: code, Detail: fmt.Sprintf(format, args...)}}
	}
	if len(data) > maxManifestBytes {
		return invalid(CodeManifestInvalid, "manifest exceeds %d bytes", maxManifestBytes)
	}
	var head struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return invalid(CodeManifestInvalid, "manifest is not valid JSON: %v", err)
	}
	if head.Format != FormatV1 {
		if formatPattern.MatchString(head.Format) {
			return nil, StatusUnsupported, []Finding{{Code: CodeUnsupportedManifest,
				Detail: fmt.Sprintf("manifest format %q is not supported by this binary (supports %s)", head.Format, FormatV1)}}
		}
		return invalid(CodeManifestInvalid, "manifest format %q is not an AO backup format", head.Format)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return invalid(CodeManifestInvalid, "manifest does not match %s: %v", FormatV1, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return invalid(CodeManifestInvalid, "manifest has trailing data")
	}
	if findings := m.validate(); len(findings) > 0 {
		return &m, StatusInvalid, findings
	}
	return &m, StatusValid, nil
}

func (m *Manifest) validate() []Finding {
	var out []Finding
	add := func(code Code, format string, args ...any) {
		out = append(out, Finding{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	if !ValidBackupID(m.BackupID) {
		add(CodeManifestInvalid, "backupId %q is not a valid backup id", m.BackupID)
	}
	if !m.Kind.valid() {
		add(CodeManifestInvalid, "kind %q is not known", m.Kind)
	}
	if _, off := m.CreatedAt.Zone(); m.CreatedAt.IsZero() || off != 0 {
		add(CodeManifestInvalid, "createdAt must be a UTC timestamp")
	}
	if m.Tool.Name == "" {
		add(CodeManifestInvalid, "tool.name is empty")
	}
	if m.Method != MethodVacuumInto {
		add(CodeManifestInvalid, "method %q is not known", m.Method)
	}
	if m.Consistency != ConsistencySingleReadTxn {
		add(CodeManifestInvalid, "consistency %q is not known", m.Consistency)
	}
	if m.Schema.GooseVersion <= 0 {
		add(CodeManifestInvalid, "schema.gooseVersion must be positive")
	}
	if !fingerprintPattern.MatchString(m.Source.DataDirFingerprint) {
		add(CodeManifestInvalid, "source.dataDirFingerprint is malformed")
	}
	if m.Source.SecretKeyFingerprint != "" && !fingerprintPattern.MatchString(m.Source.SecretKeyFingerprint) {
		add(CodeManifestInvalid, "source.secretKeyFingerprint is malformed")
	}
	if m.Source.InstallationID != "" && !daemonmeta.ValidInstallationID(m.Source.InstallationID) {
		add(CodeManifestInvalid, "source.installationId is malformed")
	}
	if m.Checks.IntegrityCheck != "ok" || m.Checks.ForeignKeyViolations != 0 {
		add(CodeManifestInvalid, "the snapshot was not clean when the backup was created")
	}
	if len(m.Assets) == 0 || len(m.Assets) > maxAssets {
		add(CodeManifestInvalid, "assets must list between 1 and %d files", maxAssets)
		return out
	}

	seen := make(map[string]bool, len(m.Assets))
	folded := make(map[string]string, len(m.Assets))
	paths := make([]string, 0, len(m.Assets))
	var databases, identities int
	for _, a := range m.Assets {
		if err := ValidateAssetPath(a.Path); err != nil {
			add(CodeUnsafePath, "asset path %q: %v", a.Path, err)
			continue
		}
		if a.Path == ManifestName {
			add(CodeUnsafePath, "asset path %q collides with the manifest", a.Path)
			continue
		}
		if seen[a.Path] {
			add(CodeManifestInvalid, "asset path %q is listed twice", a.Path)
			continue
		}
		lower := strings.ToLower(a.Path)
		if other, ok := folded[lower]; ok {
			add(CodeManifestInvalid, "asset paths %q and %q collide on a case-insensitive filesystem", other, a.Path)
			continue
		}
		seen[a.Path], folded[lower] = true, a.Path
		paths = append(paths, a.Path)

		switch a.Role {
		case RoleDatabase:
			databases++
			if a.Path != DatabaseAsset {
				add(CodeUnsafePath, "database asset must be %q, not %q", DatabaseAsset, a.Path)
			}
		case RoleInstallationIdentity:
			identities++
			if a.Path != IdentityAsset {
				add(CodeUnsafePath, "installation identity asset must be %q, not %q", IdentityAsset, a.Path)
			}
		case RoleSkillPackage:
			if !strings.HasPrefix(a.Path, SkillCatalogPath+"/") {
				add(CodeUnsafePath, "skill package asset %q is outside %s", a.Path, SkillCatalogPath)
			}
		default:
			add(CodeManifestInvalid, "asset %q has unknown role %q", a.Path, a.Role)
		}
		if a.Size < 0 {
			add(CodeManifestInvalid, "asset %q has a negative size", a.Path)
		}
		if !sha256Pattern.MatchString(a.SHA256) {
			add(CodeManifestInvalid, "asset %q has a malformed sha256", a.Path)
		}
		if !modePattern.MatchString(a.Mode) {
			add(CodeManifestInvalid, "asset %q has a malformed mode %q", a.Path, a.Mode)
		}
	}
	// A path that is a directory prefix of another cannot be a file.
	sort.Strings(paths)
	for i := 1; i < len(paths); i++ {
		if strings.HasPrefix(paths[i], paths[i-1]+"/") {
			add(CodeManifestInvalid, "asset %q is both a file and the parent of %q", paths[i-1], paths[i])
		}
	}
	if databases != 1 {
		add(CodeManifestInvalid, "a backup must contain exactly one database asset, found %d", databases)
	}
	if identities > 1 {
		add(CodeManifestInvalid, "a backup may contain at most one installation identity, found %d", identities)
	}
	if (identities == 1) != (m.Source.InstallationID != "") {
		add(CodeManifestInvalid, "source.installationId and the installation identity asset disagree")
	}
	return out
}

// ValidateAssetPath accepts only a canonical, relative, forward-slash path that
// stays inside the backup: no absolute or volume paths, no "." or ".."
// segments, no backslashes, colons or NUL bytes.
func ValidateAssetPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case len(p) > maxAssetPathLen:
		return errors.New("path too long")
	case strings.ContainsAny(p, "\\:\x00"):
		return errors.New("path contains a backslash, colon or NUL")
	case strings.HasPrefix(p, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "":
		return errors.New("absolute path")
	case path.Clean(p) != p:
		return errors.New("path is not canonical")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("path escapes the backup")
		}
	}
	return nil
}

func formatMode(mode uint32) string { return fmt.Sprintf("%04o", mode&0o777) }

func parseMode(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v) & 0o777, nil
}
