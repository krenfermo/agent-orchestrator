package secretbox

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	box := New(t.TempDir())
	const password = "app specific password"
	sealed, err := box.Seal(password)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == password || strings.Contains(sealed, password) {
		t.Fatalf("ciphertext contains the plaintext: %q", sealed)
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened != password {
		t.Fatalf("Open = %q, want %q", opened, password)
	}
}

// GCM is randomized, so the same password sealed twice must not produce the
// same bytes — otherwise a stored value would leak "these two accounts share a
// password" to anyone reading the database.
func TestSealIsRandomized(t *testing.T) {
	box := New(t.TempDir())
	first, err := box.Seal("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	second, err := box.Seal("same")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if first == second {
		t.Fatal("sealing the same plaintext twice produced identical ciphertext")
	}
}

// "No password set" has to be representable without a ciphertext that decrypts
// to the empty string, or callers cannot tell the two apart.
func TestEmptyRoundTripsAsEmpty(t *testing.T) {
	box := New(t.TempDir())
	sealed, err := box.Seal("")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed != "" {
		t.Fatalf("Seal(\"\") = %q, want the empty string", sealed)
	}
	opened, err := box.Open("")
	if err != nil || opened != "" {
		t.Fatalf("Open(\"\") = %q, %v", opened, err)
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	box := New(t.TempDir())
	sealed, err := box.Seal("app specific password")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw[len(raw)-1] ^= 0xff
	if _, err := box.Open(base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("Open accepted tampered ciphertext")
	}
}

func TestOpenRejectsGarbage(t *testing.T) {
	box := New(t.TempDir())
	for _, value := range []string{"not base64!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := box.Open(value); err == nil {
			t.Fatalf("Open(%q) succeeded", value)
		}
	}
}

// A different key must not open another box's ciphertext: the key file, not
// the database, is what protects the password.
func TestADifferentKeyCannotOpen(t *testing.T) {
	sealed, err := New(t.TempDir()).Seal("app specific password")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := New(t.TempDir()).Open(sealed); err == nil {
		t.Fatal("a box with a different key opened the ciphertext")
	}
}

// The key is the only thing standing between a copied database and a readable
// password, so its file must not be world- or group-readable.
func TestKeyFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir).Seal("app specific password"); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
}

// The key persists: a daemon restart must still be able to read the password
// it stored yesterday.
func TestKeySurvivesANewBoxOverTheSameDir(t *testing.T) {
	dir := t.TempDir()
	sealed, err := New(dir).Seal("app specific password")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	opened, err := New(dir).Open(sealed)
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	if opened != "app specific password" {
		t.Fatalf("Open = %q", opened)
	}
}

func TestNewWithKeyRejectsAWrongSizedKey(t *testing.T) {
	if _, err := NewWithKey([]byte("too short")); err == nil {
		t.Fatal("NewWithKey accepted a short key")
	}
}
