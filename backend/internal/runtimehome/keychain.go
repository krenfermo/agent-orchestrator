package runtimehome

// keychain.go — the platform-independent vocabulary for AO's own per-user
// credential store, and the one question every unattended launch needs
// answered about it: can a provider subprocess OPEN it with nobody present?
//
// THE INCIDENT. A real workflow stopped at `planner_auth_unavailable` while
// macOS put up "security wants to use the keychain 'login'". The prompt was
// unanswerable: the keychain it names is not the person's login keychain, it
// is AO's own `login.keychain-db` inside the isolated runtime home (macOS
// titles the dialog with the keychain's FILE name, and AO had named its file
// the same thing), and its password is a random secret AO generated — not
// anything a person knows. AO had also lost that password's usefulness:
// `security unlock-keychain` with the stored secret returned "The user name or
// passphrase you entered is not correct", and every error on this path was
// discarded with `_ =`, so AO never noticed it was handing provider launches a
// credential store it could no longer open.
//
// Two rules follow, and they are why this file exists.
//
// A keychain AO cannot open is worse than no keychain, because it does not
// fail — it BLOCKS, on a GUI dialog, in a subprocess nobody is watching. So
// EnsureKeychain repairs that state instead of ignoring it, and reports what
// it found.
//
// And the answer must be obtainable WITHOUT prompting. Every probe here is
// either a filesystem check or an unlock attempt with a password AO already
// holds; none of them can raise SecurityAgent. `security show-keychain-info`
// and `security find-generic-password` both CAN, which is exactly why neither
// is used to answer this question.

// KeychainState is what AO knows about the credential store a subprocess
// launched with a given runtime home would resolve to.
type KeychainState string

const (
	// KeychainOK means AO manages this keychain and opened it, without a person.
	KeychainOK KeychainState = "ok"
	// KeychainRequiresInteraction means AO manages this keychain and could NOT
	// open it. A provider subprocess reaching for a credential here will
	// raise the OS unlock dialog and hang. This is the state that must never
	// reach a launch.
	KeychainRequiresInteraction KeychainState = "requires_interaction"
	// KeychainAbsent means AO manages no keychain for this home — the launch will
	// resolve whatever the host provides. Not a fault, and deliberately not
	// an answer about the host's own store: see providerauth for why AO
	// refuses to claim knowledge it does not have about a working desktop.
	KeychainAbsent KeychainState = "absent"
	// KeychainUnsupported means this platform has no OS keychain in the launch
	// path (everything but macOS today).
	KeychainUnsupported KeychainState = "unsupported"
)

// Usable reports whether a launch may proceed against this state. Unsupported
// and Absent are usable because they are statements about AO's own store, not
// verdicts on the provider's credentials — the credential check itself belongs
// to providerauth.
func (s KeychainState) Usable() bool { return s != KeychainRequiresInteraction }

// KeychainReport is one inspection's full result. Path and Detail are safe to
// persist and to show a person: a filesystem path and an OS status sentence,
// never a password and never a token.
type KeychainReport struct {
	State KeychainState
	// Path is the keychain file the state describes, empty when there is none.
	Path string
	// Detail is the OS's own words when something went wrong, bounded and
	// free of any secret AO passed in.
	Detail string
	// Repaired is true when this call found an unopenable keychain, moved it
	// aside and provisioned a fresh one. The credentials that were inside it
	// were already unreadable, but a person needs to be told they are gone.
	Repaired bool
	// QuarantinedPath names where the unopenable keychain was moved, so the
	// repair is reversible and auditable.
	QuarantinedPath string
}
