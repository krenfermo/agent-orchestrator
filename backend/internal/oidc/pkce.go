package oidc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// NewCodeVerifier mints a PKCE code verifier: 32 random bytes rendered as
// base64url, which lands inside RFC 7636's 43–128 character range.
func NewCodeVerifier() (string, error) {
	return randomBase64URL(32)
}

// CodeChallengeS256 derives the S256 challenge for a verifier. Only S256 is
// offered; `plain` provides no protection against an intercepted code and
// exists in the RFC for clients that cannot compute a SHA-256.
func CodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// NewState mints an unguessable `state` value. In AO it doubles as the id of
// the durable login-flow row, so it must be collision-free as well as
// unpredictable: 32 bytes is both.
func NewState() (string, error) { return randomBase64URL(32) }

// NewNonce mints the nonce bound into the authorization request and checked
// against the ID token.
func NewNonce() (string, error) { return randomBase64URL(32) }

func randomBase64URL(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
