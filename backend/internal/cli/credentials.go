package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// credentials.go — where `ao` keeps the session the daemon issued it.
//
// P4-B left the CLI with no way to authenticate at all under
// AO_AUTH_MODE=oidc: it carries no cookie, trusted-local synthesis is off by
// construction, and every permission-gated route therefore answers
// 401 NOT_AUTHENTICATED. This file is half of the answer (auth.go is the
// other half): the CLI holds a REAL AO session — the same opaque, revocable,
// server-hashed token a browser holds — obtained by signing in at the
// configured identity provider, and presents it on every daemon call.
//
// What it deliberately is not:
//
//   - Not a token the CLI mints. Only the daemon issues sessions, and only
//     after the provider authenticated a human.
//   - Not a copied browser cookie. `ao auth login` runs its own OIDC
//     Authorization Code + PKCE flow through the loopback handoff the desktop
//     supervisor already uses (ssosvc's OIDCClientDesktop kind).
//   - Not a permanent credential. It is one `auth_sessions` row: it expires,
//     `ao auth logout` revokes it, and revoking the account's sessions from
//     the app revokes this one too.
//
// The file lives under the daemon's own data dir, so it is under ~/.ao (the
// hard rule in AGENTS.md) and is automatically scoped per profile: a
// credential for one AO_DATA_DIR is never presented to a daemon serving a
// different one.

// credentialFileName is the on-disk name inside the resolved data dir.
const credentialFileName = "cli-credentials.json"

// credentialFileMode is owner read/write only. The token is a bearer
// credential for the signed-in user, so the file is as private as an SSH key.
const credentialFileMode fs.FileMode = 0o600

// storedCredential is the on-disk shape. Only the session token is secret;
// the rest exists so `ao auth status` can answer without a daemon round-trip
// when the daemon is down.
type storedCredential struct {
	// Token is the raw session token. The daemon stores only its SHA-256.
	Token string `json:"token"`
	// ExpiresAt is when the daemon said the session stops being valid.
	ExpiresAt time.Time `json:"expiresAt"`
	// UserID/Email/Issuer identify who signed in, for display only. They are
	// never sent anywhere and are never trusted for authorization — the
	// daemon re-resolves identity from the token on every request.
	UserID string `json:"userId,omitempty"`
	Email  string `json:"email,omitempty"`
	Issuer string `json:"issuer,omitempty"`
	// SavedAt records when this file was written.
	SavedAt time.Time `json:"savedAt"`
}

// valid reports whether the credential can still be presented at `now`. An
// expired credential is not sent: presenting it would produce the same 401 as
// sending nothing, with a worse error message.
func (c storedCredential) valid(now time.Time) bool {
	if c.Token == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		return true
	}
	return c.ExpiresAt.After(now)
}

func credentialPath(dataDir string) string {
	return filepath.Join(dataDir, credentialFileName)
}

// readCredential loads the stored credential. A missing file is not an error:
// no credential is the ordinary state on a trusted-local install, where the
// CLI never needs one.
func readCredential(dataDir string) (storedCredential, bool, error) {
	raw, err := os.ReadFile(credentialPath(dataDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return storedCredential{}, false, nil
		}
		return storedCredential{}, false, fmt.Errorf("read CLI credentials: %w", err)
	}
	var out storedCredential
	if err := json.Unmarshal(raw, &out); err != nil {
		// A corrupt file is reported rather than silently ignored: "you are
		// not signed in" and "your credential file is damaged" are different
		// problems with different fixes.
		return storedCredential{}, false, fmt.Errorf("parse CLI credentials at %s: %w", credentialPath(dataDir), err)
	}
	if out.Token == "" {
		return storedCredential{}, false, nil
	}
	return out, true, nil
}

// writeCredential persists the credential 0600, creating the data dir if the
// daemon has not yet. It writes through a temp file in the same directory so a
// crashed write can never leave a half-written token behind.
func writeCredential(dataDir string, cred storedCredential) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	payload, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dataDir, credentialFileName+".*")
	if err != nil {
		return fmt.Errorf("write CLI credentials: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(credentialFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write CLI credentials: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write CLI credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write CLI credentials: %w", err)
	}
	if err := os.Rename(tmpName, credentialPath(dataDir)); err != nil {
		return fmt.Errorf("write CLI credentials: %w", err)
	}
	return nil
}

// removeCredential deletes the stored credential. A missing file is success:
// `ao auth logout` is idempotent.
func removeCredential(dataDir string) error {
	if err := os.Remove(credentialPath(dataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove CLI credentials: %w", err)
	}
	return nil
}
