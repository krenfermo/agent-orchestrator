package backup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/secretbox"
)

// Independent review §15/§16 — secret.key is deliberately not in a backup, yet
// durable rows depend on it (app_settings.smtp_password_encrypted, the work-item
// API token, skill_secrets.sealed_value -- private registry credentials
// included). These tests pin the semantics with the real secretbox: a restore
// never silently puts sealed rows next to a key that cannot open them.

func writeRandomKey(t *testing.T, dataDir string) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key))
	writeFile(t, filepath.Join(dataDir, secretKeyFile), string(encoded), 0o600)
	return encoded
}

func sealedValue(t *testing.T, dataDir string) string {
	t.Helper()
	db := openRW(t, dataDir)
	defer func() { _ = db.Close() }()
	var v string
	if err := db.QueryRow(`SELECT path FROM projects WHERE id = 'sealed'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRestoredSealedRowsAreNeverSilentlyUnreadable(t *testing.T) {
	parent := t.TempDir()
	dataDir := newInstallation(t, parent)
	root := filepath.Join(parent, "backups")
	originalKey := writeRandomKey(t, dataDir)
	sealed, err := secretbox.New(dataDir).Seal("smtp-password-review")
	if err != nil {
		t.Fatal(err)
	}
	// A durable row sealed with secret.key (a project path stands in for the
	// encrypted columns; the mechanism is the same).
	mutate(t, dataDir, `INSERT INTO projects (id, path, registered_at) VALUES ('sealed', '`+sealed+`', CURRENT_TIMESTAMP)`)
	res := mustCreate(t, dataDir, root)
	if fp := res.Manifest.Source.SecretKeyFingerprint; fp == "" {
		t.Fatal("the backup does not record which key its sealed rows need")
	}

	// Disaster: secret.key is lost and AO generated a new one.
	writeRandomKey(t, dataDir)
	before := stripSidecars(managedDigest(t, dataDir))
	opts := RestoreOptions{DataDir: dataDir, Source: res.Path, Root: root, CheckDaemon: noDaemon, Tool: ToolInfo{Name: "ao-test"}}
	if _, err := Restore(context.Background(), opts); codeOf(err) != CodeSecretKeyMismatch || classOf(err) != ClassRefused {
		t.Fatalf("restore next to a different key: %v", err)
	}
	if stripSidecars(managedDigest(t, dataDir)) != before {
		t.Fatal("the refused restore changed the data dir")
	}

	// Explicit consent: restored, loudly warned, and the row really is unreadable.
	withConsent := opts
	withConsent.AllowSecretKeyMismatch = true
	rep, err := Restore(context.Background(), withConsent)
	if err != nil || rep.Result != ResultRestored || !hasWarning(rep, CodeSecretKeyMismatch) {
		t.Fatalf("restore with --allow-secret-key-mismatch: err=%v rep=%+v", err, rep)
	}
	if _, err := secretbox.New(dataDir).Open(sealedValue(t, dataDir)); err == nil {
		t.Fatal("precondition: the sealed row should be unreadable with the new key")
	}

	// The original key back: the same rows open again, and a strict restore passes.
	writeFile(t, filepath.Join(dataDir, secretKeyFile), string(originalKey), 0o600)
	if plain, err := secretbox.New(dataDir).Open(sealedValue(t, dataDir)); err != nil || plain != "smtp-password-review" {
		t.Fatalf("with the original key: %q %v", plain, err)
	}
	if _, err := Restore(context.Background(), opts); err != nil {
		t.Fatalf("strict restore with the original key: %v", err)
	}

	// No key at all is a mismatch too.
	if err := os.Remove(filepath.Join(dataDir, secretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), opts); codeOf(err) != CodeSecretKeyMismatch || !strings.Contains(err.Error(), "no secret.key") {
		t.Fatalf("restore with no key: %v", err)
	}
}

// The fingerprint compares keys without revealing one: fixed length, never the
// key or its encoding, stable across reads and a trailing newline (which the
// key's base64 decoding ignores as well), different for a different key, empty
// when there is no key.
func TestSecretKeyFingerprintIsOneWayAndStable(t *testing.T) {
	dir := t.TempDir()
	encoded := writeRandomKey(t, dir)
	fp, err := secretKeyFingerprint(dir)
	if err != nil || !fingerprintPattern.MatchString(fp) {
		t.Fatalf("fingerprint %q (%v)", fp, err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(encoded))
	for _, secret := range []string{string(encoded), string(decoded)} {
		if strings.Contains(secret, fp) || strings.Contains(fp, secret) {
			t.Fatal("the fingerprint contains the key")
		}
	}
	if again, _ := secretKeyFingerprint(dir); again != fp {
		t.Fatal("the fingerprint is not stable")
	}
	writeFile(t, filepath.Join(dir, secretKeyFile), string(encoded)+"\n", 0o600)
	if nl, _ := secretKeyFingerprint(dir); nl != fp {
		t.Fatal("a trailing newline changed the fingerprint")
	}
	writeRandomKey(t, dir)
	if other, _ := secretKeyFingerprint(dir); other == fp {
		t.Fatal("two keys share a fingerprint")
	}
	if err := os.Remove(filepath.Join(dir, secretKeyFile)); err != nil {
		t.Fatal(err)
	}
	if none, err := secretKeyFingerprint(dir); err != nil || none != "" {
		t.Fatalf("no key: %q %v", none, err)
	}
}
