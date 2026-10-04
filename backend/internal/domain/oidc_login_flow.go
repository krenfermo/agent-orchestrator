package domain

import "time"

// OIDCClientKind names which AO surface started a login, because the two
// finish differently and must not be interchangeable.
type OIDCClientKind string

const (
	// OIDCClientBrowser is a login started from a page served on the daemon's
	// own origin. Its callback sets the session cookie directly and redirects
	// the browser to a bounded in-app destination.
	OIDCClientBrowser OIDCClientKind = "browser"
	// OIDCClientDesktop is a login started by the Electron supervisor, which
	// hands the authorization request to the system browser. Its callback
	// stamps the resolved user on the flow and renders a terminal "you can
	// close this tab" page; the session itself is minted only when the
	// supervisor presents the handoff secret it kept on loopback. No token
	// and no code ever travels back through a renderer URL.
	OIDCClientDesktop OIDCClientKind = "desktop"
)

// OIDCLoginFlow is one in-flight Authorization Code request. ID is the OAuth
// `state` value, so an unknown, expired or already-consumed state simply finds
// no row and the callback fails closed.
//
// Nothing the provider issues is stored here: no authorization code, no
// access or refresh token, no raw ID token. CodeVerifier and Nonce are values
// AO itself generated and must keep in order to check the provider's answer.
type OIDCLoginFlow struct {
	ID           string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
	// ReturnTo is the already-validated in-app destination ("" meaning the
	// default). It is stored post-validation precisely so the callback has no
	// attacker-supplied redirect left to trust.
	ReturnTo   string
	ClientKind OIDCClientKind
	// HandoffSecretHash is the SHA-256 of the desktop supervisor's pickup
	// secret; empty for a browser flow. The secret itself never leaves
	// loopback and is never sent to the provider.
	HandoffSecretHash   string
	AuthenticatedUserID *UserID
	AuthenticatedAt     *time.Time
	CreatedAt           time.Time
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
}

// Pending reports whether the flow is still usable at the given instant: not
// consumed and not expired.
func (f OIDCLoginFlow) Pending(now time.Time) bool {
	return f.ConsumedAt == nil && f.ExpiresAt.After(now)
}
