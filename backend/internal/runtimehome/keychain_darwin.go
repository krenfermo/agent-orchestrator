//go:build darwin

package runtimehome

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// keychainFileName is the name of AO's own per-user provider keychain, and it
// is deliberately NOT "login".
//
// THE BUG. It used to be "login.keychain-db", on the reasoning that macOS
// resolves "the login keychain" through $HOME and a differently named file
// would not be found. Both halves of that were wrong, and together they are
// the keychain incident.
//
// "login" is reserved. On macOS a keychain at <name>/login.keychain-db is
// bound to the user's login-keychain semantics, and SecKeychainUnlock on it
// does NOT accept the password the file was created with:
//
//	security create-keychain -p PW .../login.keychain-db   -> ok
//	security unlock-keychain -p PW .../login.keychain-db   -> "The user name or
//	                                                          passphrase you
//	                                                          entered is not
//	                                                          correct."
//	security create-keychain -p PW .../ao-provider.keychain-db -> ok
//	security unlock-keychain -p PW .../ao-provider.keychain-db -> ok
//
// So AO could create its per-user keychain but could never reopen it. That
// went unnoticed for as long as it did because create-keychain leaves the new
// keychain UNLOCKED in the current securityd session: writes and reads worked
// right up until the first sleep, logout or reboot, after which the keychain
// was locked forever and every provider launch that touched it raised the
// unanswerable "security wants to use the keychain 'login'" dialog -- named
// "login" because that is what AO had called its file.
//
// And the name was never needed. Claude Code stores its credential as a
// generic password with no keychain specified, which resolves through the
// DEFAULT keychain and the user search list -- both of which this file sets
// explicitly, under the isolated HOME, to whatever this name is.
const keychainFileName = "ao-provider.keychain-db"

// legacyKeychainFileName is the name AO used to give the same keychain. Any
// file still bearing it inside a runtime home is unopenable by construction
// (see above), so Prepare moves it aside rather than leaving a permanent
// dialog source sitting in the search list.
const legacyKeychainFileName = "login.keychain-db"

// securityTimeout bounds every security(1) call. None of them are interactive
// in the form used here, but a hung credential subprocess must never hold a
// launch: a timeout is reported as "requires interaction", the conservative
// answer, since a security(1) that is not returning is most often one sitting
// behind a dialog.
const securityTimeout = 10 * time.Second

// keychainDetailLimit bounds how much of security(1)'s own error prose is
// carried into a durable report.
const keychainDetailLimit = 300

// ensureIsolatedKeychain provisions (idempotently) a per-user macOS login
// keychain rooted at env.RuntimeHome, so Claude Code's own OAuth token
// storage -- which on macOS resolves via the process's $HOME (macOS resolves
// the default keychain and search list from $HOME/Library/Keychains and
// $HOME/Library/Preferences, not from CLAUDE_CONFIG_DIR or any other env var
// Claude Code exposes) -- lands in this user's isolated runtime-home instead
// of either silently failing to persist (Checkpoint 8P-E.2: a HOME with no
// Library/Keychains resolves to an empty keychain search list, so `security
// add-generic-password` has nowhere to write and the write is dropped) or
// falling back onto the daemon host's own real login keychain, which would
// leak one AO user's provider credentials into another's isolated environment
// and violate Checkpoint 8P-B.
//
// Every operation is scoped to env.RuntimeHome via an explicit HOME override
// on the security(1) subprocess, so a failure never mutates the daemon host's
// real keychain search list.
//
// What changed after the keychain incident: this is no longer best-effort. It
// still never fails Prepare() -- a broken credential store must not stop a
// launch that does not need one -- but it now REPORTS what it found, and it
// repairs the one state that cannot be tolerated. An AO-owned keychain whose
// stored password no longer opens it is not a degraded keychain, it is a GUI
// dialog waiting to happen inside an unattended subprocess. Such a keychain is
// moved aside and replaced with one AO can actually open, and the report says
// so.
func ensureIsolatedKeychain(env Environment) KeychainReport {
	keychainDir := filepath.Join(env.RuntimeHome, "Library", "Keychains")
	keychainPath := filepath.Join(keychainDir, keychainFileName)
	report := KeychainReport{State: KeychainRequiresInteraction, Path: keychainPath}

	if err := os.MkdirAll(keychainDir, 0o700); err != nil {
		report.Detail = boundDetail("create keychain directory: " + err.Error())
		return report
	}
	// Library/Preferences is where `security default-keychain -s` and
	// `list-keychains -s` persist this HOME's keychain domain. Without it both
	// commands still exit 0 and write nothing, so the next process resolves NO
	// default keychain and every credential write blocks on a chooser prompt.
	// The old code got away without it only because a keychain named "login"
	// is found by convention rather than through this preference -- the same
	// reserved name that made it impossible to unlock.
	if err := os.MkdirAll(filepath.Join(env.RuntimeHome, "Library", "Preferences"), 0o700); err != nil {
		report.Detail = boundDetail("create keychain preferences directory: " + err.Error())
		return report
	}
	password, err := loadOrCreateKeychainSecret(filepath.Join(env.Root, ".keychain-secret"))
	if err != nil {
		report.Detail = boundDetail("read keychain secret: " + err.Error())
		return report
	}
	run := securityRunner(env.RuntimeHome, password)

	// Migration. A runtime home provisioned before the rename still holds a
	// login.keychain-db that nothing can open, and it is still named in this
	// home's default-keychain/search-list preferences. Leaving it there leaves
	// the dialog there, so it is moved aside before the real keychain is set
	// up -- the preference writes below then repoint this home at the new one.
	if legacy := filepath.Join(keychainDir, legacyKeychainFileName); fileExists(legacy) {
		if moved, err := quarantineKeychain(legacy); err == nil {
			report.Repaired = true
			report.QuarantinedPath = moved
			report.Detail = "AO's per-user provider keychain was named \"login\", which macOS reserves and never reopens with AO's own password; it was moved aside and replaced, so the provider must be re-connected for this user"
		}
	}

	if _, statErr := os.Stat(keychainPath); errors.Is(statErr, os.ErrNotExist) {
		if err := createKeychain(run, keychainPath); err != nil {
			report.Detail = boundDetail(err.Error())
			return report
		}
	}

	if err := run("unlock-keychain", "-p", secretPlaceholder, keychainPath); err != nil {
		// The keychain exists and AO's own password does not open it. Nothing
		// AO can do makes that file usable again -- and leaving it in place
		// guarantees the unanswerable dialog this whole file exists to
		// prevent. It is moved aside (never deleted: an operator may want to
		// look at it, and AO does not destroy credential material it cannot
		// read) and replaced.
		quarantined, qerr := quarantineKeychain(keychainPath)
		if qerr != nil {
			report.Detail = boundDetail(fmt.Sprintf("keychain cannot be unlocked with AO's stored secret (%v) and could not be moved aside (%v)", err, qerr))
			return report
		}
		report.Repaired = true
		report.QuarantinedPath = quarantined
		if cerr := createKeychain(run, keychainPath); cerr != nil {
			report.Detail = boundDetail("replacement keychain: " + cerr.Error())
			return report
		}
		if uerr := run("unlock-keychain", "-p", secretPlaceholder, keychainPath); uerr != nil {
			report.Detail = boundDetail("replacement keychain could not be unlocked: " + uerr.Error())
			return report
		}
		report.Detail = "AO's per-user provider keychain could not be opened with its stored secret and was replaced; any provider credential it held is gone and the provider must be re-connected"
	}

	// Scoped to the isolated HOME, so the host's real search list is untouched.
	if err := run("default-keychain", "-s", keychainPath); err != nil {
		report.Detail = boundDetail("set default keychain: " + err.Error())
		return report
	}
	if err := run("list-keychains", "-d", "user", "-s", keychainPath, "/Library/Keychains/System.keychain"); err != nil {
		report.Detail = boundDetail("set keychain search list: " + err.Error())
		return report
	}
	report.State = KeychainOK
	return report
}

// createKeychain provisions a new keychain and pins its settings.
func createKeychain(run securityFunc, path string) error {
	if err := run("create-keychain", "-p", secretPlaceholder, path); err != nil {
		return fmt.Errorf("create keychain: %w", err)
	}
	// No idle timeout, no lock-on-sleep: AO's daemon must be able to launch
	// provider subprocesses unattended, long after the OAuth login that first
	// populated this keychain. A keychain that re-locks on sleep is the same
	// unanswerable dialog on a delay.
	if err := run("set-keychain-settings", path); err != nil {
		return fmt.Errorf("set keychain settings: %w", err)
	}
	return nil
}

// quarantineKeychain moves an unopenable keychain aside, returning its new
// path. Renaming rather than deleting keeps the change reversible and keeps
// AO out of the business of destroying credential material.
func quarantineKeychain(path string) (string, error) {
	dest := fmt.Sprintf("%s.unusable-%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// InspectKeychain reports the state of the AO-managed keychain a subprocess
// launched with runtimeHome would resolve to, WITHOUT provisioning or
// repairing anything and WITHOUT any call that can raise a dialog.
//
// It is the read-only half of EnsureKeychain, for preflight: a launch about to
// happen needs to know whether the credential store it is about to touch will
// open, and asking that question must not itself be the thing that opens a
// prompt. `security show-keychain-info` and `security find-generic-password`
// both prompt on a locked keychain and are therefore never used here; the only
// probe is an unlock attempt with the password AO already stores, which fails
// with an error rather than a dialog.
func InspectKeychain(runtimeHome string) KeychainReport {
	runtimeHome = strings.TrimSpace(runtimeHome)
	if runtimeHome == "" {
		return KeychainReport{State: KeychainAbsent}
	}
	// AO only ever makes claims about a keychain it MANAGES, and the marker is
	// the runtime-home layout Prepare builds. Without it this is somebody's real
	// home directory, whose credential store AO does not inspect and must not
	// judge -- the host's own login keychain is named login.keychain-db too, and
	// reporting THAT as "AO's unopenable legacy keychain" would refuse every
	// launch on a working desktop.
	root := filepath.Dir(runtimeHome)
	if !aoManagedRoot(root) {
		return KeychainReport{State: KeychainAbsent}
	}
	keychainDir := filepath.Join(runtimeHome, "Library", "Keychains")
	// A runtime home still carrying the legacy "login" keychain is the incident
	// state itself, whether or not the correctly named one exists beside it:
	// macOS never reopens it with AO's password, and it is still named in this
	// home's search list. Reported without probing -- probing a locked keychain
	// named "login" is the operation that raises the dialog.
	if legacy := filepath.Join(keychainDir, legacyKeychainFileName); fileExists(legacy) {
		return KeychainReport{
			State:  KeychainRequiresInteraction,
			Path:   legacy,
			Detail: "this runtime home still holds AO's legacy provider keychain named \"login\", a name macOS reserves and never reopens with AO's own password; reaching a credential through it raises an unlock dialog",
		}
	}
	keychainPath := filepath.Join(keychainDir, keychainFileName)
	if !fileExists(keychainPath) {
		// AO manages this home but has provisioned no keychain in it yet. Not a
		// fault and not a prompt: providerauth decides what an isolated home
		// with no credential means.
		return KeychainReport{State: KeychainAbsent}
	}
	report := KeychainReport{Path: keychainPath}
	secret, err := os.ReadFile(filepath.Join(root, ".keychain-secret"))
	if err != nil {
		report.State = KeychainRequiresInteraction
		report.Detail = boundDetail("AO holds a provider keychain for this runtime home but no secret to open it: " + err.Error())
		return report
	}
	if err := securityRunner(runtimeHome, string(secret))("unlock-keychain", "-p", secretPlaceholder, keychainPath); err != nil {
		report.State = KeychainRequiresInteraction
		report.Detail = boundDetail("AO's per-user provider keychain does not open with its stored secret: " + err.Error())
		return report
	}
	report.State = KeychainOK
	return report
}

// aoManagedRoot reports whether root is one of AO's own per-user runtime roots
// (AO_DATA_DIR/users/<id>), by the layout Prepare creates there. Both markers
// are checked because a home can legitimately have one without the other: a
// freshly prepared root before its first keychain has no secret file, and a
// hand-cleaned one may have lost the providers directory.
func aoManagedRoot(root string) bool {
	if fileExists(filepath.Join(root, ".keychain-secret")) {
		return true
	}
	info, err := os.Stat(filepath.Join(root, "providers"))
	return err == nil && info.IsDir()
}

// secretPlaceholder is the argument callers pass where the keychain password
// belongs. securityRunner substitutes the real secret at exec time, so the
// password never appears in a caller's argv slice, in a log line, or in an
// error built from one.
// It is a marker, not a credential -- gosec's G101 pattern match on the
// name is exactly backwards here: the whole point of this constant is that
// the real secret never appears in source or in an argv a caller builds.
//
//nolint:gosec // G101: a sentinel argv marker, not a hardcoded credential.
const secretPlaceholder = "\x00ao-keychain-secret\x00"

type securityFunc func(args ...string) error

// securityRunner returns a bounded security(1) invoker scoped to one HOME.
//
// The HOME override is the entire isolation mechanism: macOS resolves the
// default keychain and the user search list from $HOME, so a security(1) run
// with the isolated HOME cannot read, write or reorder the host's real
// keychains however wrong its arguments are.
//
// It also owns two secret-handling rules. The password is substituted into
// argv only at exec time (see secretPlaceholder), and security(1)'s output is
// scrubbed of it before ever becoming an error string -- security(1) does not
// echo passwords today, but an error path is exactly where a future version
// would, and a report built here is persisted and shown to people.
func securityRunner(home, password string) securityFunc {
	return func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), securityTimeout)
		defer cancel()
		argv := make([]string, len(args))
		for i, a := range args {
			if a == secretPlaceholder {
				argv[i] = password
				continue
			}
			argv[i] = a
		}
		cmd := exec.CommandContext(ctx, "security", argv...)
		// A minimal, explicit environment. HOME is the isolation; PATH is
		// there so security(1) can find whatever it shells out to. Nothing
		// else from the daemon's environment is inherited, so no AO or
		// provider credential in the daemon's env can reach this subprocess.
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		detail := scrubSecret(strings.TrimSpace(string(out)), password)
		if ctx.Err() != nil {
			return fmt.Errorf("security %s timed out after %s", args[0], securityTimeout)
		}
		if detail == "" {
			return fmt.Errorf("security %s: %w", args[0], err)
		}
		return errors.New(detail)
	}
}

// scrubSecret removes the keychain password from text destined for a report.
func scrubSecret(text, password string) string {
	if password == "" {
		return text
	}
	return strings.ReplaceAll(text, password, "[redacted]")
}

// boundDetail truncates report prose. Detail is provider- and OS-authored
// text on an error path, so it is bounded before it can be persisted.
func boundDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= keychainDetailLimit {
		return s
	}
	cut := keychainDetailLimit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// loadOrCreateKeychainSecret returns the password guarding this user's
// isolated login keychain, generating and persisting one on first use. This
// secret only ever unlocks an AO-created, per-user keychain holding that
// same user's own provider CLI tokens -- it is not the user's Anthropic
// credential itself and is never logged or returned to any caller.
func loadOrCreateKeychainSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return string(b), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	password := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(password), 0o600); err != nil {
		return "", err
	}
	return password, nil
}

// fileExists reports whether path names an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
