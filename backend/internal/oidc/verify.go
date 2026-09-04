package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"math/big"
	"strings"
	"time"
)

// clockSkew is the tolerance applied to exp/iat/nbf. Provider and daemon
// clocks differ in practice, and a login that fails because two machines are
// four seconds apart is a support ticket, not a security win.
const clockSkew = 2 * time.Minute

// maxIDTokenAge bounds how old an ID token's `iat` may be. It is a replay
// bound independent of `exp`, which some providers set generously.
const maxIDTokenAge = 15 * time.Minute

// jwtHeader is the JOSE header AO reads.
type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// VerifyIDToken validates an ID token end to end and returns its claims:
// structure, signature, issuer, audience, expiry/issued-at, and the nonce
// binding it to one specific login request.
//
// expectedNonce is required. An ID token with no nonce, or with a different
// one, is rejected — that binding is the only thing preventing a token
// obtained in one login from being replayed into another.
func (c *Client) VerifyIDToken(ctx context.Context, rawToken, expectedNonce string) (*IDTokenClaims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: not a three-part JWS", ErrInvalidToken)
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable header", ErrInvalidToken)
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: unparseable header", ErrInvalidToken)
	}
	// "none" is a legal JWS alg and an illegal ID token alg. Rejecting it
	// explicitly, before any key lookup, is the difference between verifying
	// a token and reading one.
	if hdr.Alg == "" || strings.EqualFold(hdr.Alg, "none") {
		return nil, fmt.Errorf("%w: unsigned tokens are not accepted", ErrInvalidToken)
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable payload", ErrInvalidToken)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: undecodable signature", ErrInvalidToken)
	}
	signingInput := []byte(parts[0] + "." + parts[1])

	if err := c.verifySignature(ctx, hdr, signingInput, signature); err != nil {
		return nil, err
	}

	rc, err := parseRawClaims(payloadBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: unparseable claims", ErrInvalidToken)
	}
	claims := rc.normalize()

	if claims.Issuer != c.cfg.Issuer {
		return nil, fmt.Errorf("%w: token issuer %q is not the configured issuer", ErrIssuerMismatch, claims.Issuer)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, fmt.Errorf("%w: token carries no sub", ErrInvalidToken)
	}
	if !audienceContains(claims.Audience, c.cfg.ClientID) {
		return nil, fmt.Errorf("%w: token was not issued for this client", ErrAudienceMismatch)
	}
	// With multiple audiences the azp claim MUST be present and MUST be this
	// client (Core 1.0 §3.1.3.7 rules 4 and 5). Without that check, a token
	// minted for another relying party at the same issuer is accepted here.
	if len(claims.Audience) > 1 {
		if rc.AuthorizedParty == "" || rc.AuthorizedParty != c.cfg.ClientID {
			return nil, fmt.Errorf("%w: multi-audience token is not authorized for this client", ErrAudienceMismatch)
		}
	}

	now := c.now().UTC()
	if claims.Expiry == 0 {
		return nil, fmt.Errorf("%w: token carries no exp", ErrInvalidToken)
	}
	if now.After(time.Unix(claims.Expiry, 0).Add(clockSkew)) {
		return nil, ErrTokenExpired
	}
	if rc.NotBefore != 0 && now.Add(clockSkew).Before(time.Unix(rc.NotBefore, 0)) {
		return nil, fmt.Errorf("%w: token is not valid yet", ErrInvalidToken)
	}
	if claims.IssuedAt != 0 {
		issued := time.Unix(claims.IssuedAt, 0)
		if now.Add(clockSkew).Before(issued) {
			return nil, fmt.Errorf("%w: token was issued in the future", ErrInvalidToken)
		}
		if now.After(issued.Add(maxIDTokenAge + clockSkew)) {
			return nil, fmt.Errorf("%w: token is too old to be a fresh login", ErrInvalidToken)
		}
	}
	if expectedNonce == "" {
		return nil, fmt.Errorf("%w: no nonce was recorded for this login", ErrNonceMismatch)
	}
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return nil, ErrNonceMismatch
	}
	return claims, nil
}

// verifySignature checks the JWS signature against the right key material for
// the header's algorithm: HS* against the configured client secret, everything
// else against the provider's published JWKS. The two never mix — an
// asymmetric-signed token can never be verified with the secret, and a JWKS
// key can never be used as an HMAC key (see jwk.publicKey).
func (c *Client) verifySignature(ctx context.Context, hdr jwtHeader, signingInput, signature []byte) error {
	if strings.HasPrefix(hdr.Alg, "HS") {
		if c.cfg.ClientSecret == "" {
			return fmt.Errorf("%w: %s requires a client secret and none is configured", ErrInvalidToken, hdr.Alg)
		}
		h, err := hashForAlg(hdr.Alg)
		if err != nil {
			return err
		}
		mac := hmac.New(h.New, []byte(c.cfg.ClientSecret))
		mac.Write(signingInput)
		if !hmac.Equal(mac.Sum(nil), signature) {
			return fmt.Errorf("%w: signature does not verify", ErrInvalidToken)
		}
		return nil
	}

	ks, err := c.jwks(ctx, false)
	if err != nil {
		return err
	}
	fetchedAt := c.jwksRefreshedAt()
	if verifyWithKeySet(ks, hdr, signingInput, signature) {
		return nil
	}
	// A rotated signing key looks exactly like a bad signature until the key
	// set is refetched, so try exactly once more with a forced refresh —
	// unless another goroutine already refreshed it during this call.
	if c.jwksRefreshedAt().Equal(fetchedAt) {
		if ks, err = c.jwks(ctx, true); err != nil {
			return err
		}
		if verifyWithKeySet(ks, hdr, signingInput, signature) {
			return nil
		}
	}
	return fmt.Errorf("%w: signature does not verify against the provider's keys", ErrInvalidToken)
}

func verifyWithKeySet(ks *keySet, hdr jwtHeader, signingInput, signature []byte) bool {
	for _, k := range ks.keysFor(hdr.Kid, hdr.Alg) {
		if verifyOne(k.key, hdr.Alg, signingInput, signature) {
			return true
		}
	}
	return false
}

func verifyOne(pub crypto.PublicKey, alg string, signingInput, signature []byte) bool {
	h, err := hashForAlg(alg)
	if err != nil {
		return false
	}
	hasher := h.New()
	hasher.Write(signingInput)
	digest := hasher.Sum(nil)

	switch {
	case strings.HasPrefix(alg, "RS"):
		key, ok := pub.(*rsa.PublicKey)
		if !ok {
			return false
		}
		return rsa.VerifyPKCS1v15(key, h, digest, signature) == nil
	case strings.HasPrefix(alg, "PS"):
		key, ok := pub.(*rsa.PublicKey)
		if !ok {
			return false
		}
		return rsa.VerifyPSS(key, h, digest, signature, &rsa.PSSOptions{
			SaltLength: rsa.PSSSaltLengthEqualsHash,
			Hash:       h,
		}) == nil
	case strings.HasPrefix(alg, "ES"):
		key, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return false
		}
		// JWS ECDSA signatures are the fixed-width R||S concatenation, not
		// the ASN.1 form crypto/ecdsa's VerifyASN1 expects.
		keySize := (key.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*keySize {
			return false
		}
		r := new(big.Int).SetBytes(signature[:keySize])
		s := new(big.Int).SetBytes(signature[keySize:])
		return ecdsa.Verify(key, digest, r, s)
	default:
		return false
	}
}

// hashForAlg maps a JOSE alg to its digest. An algorithm not listed here is
// not supported, and an unsupported algorithm is a rejected token — never a
// skipped signature check.
func hashForAlg(alg string) (crypto.Hash, error) {
	switch alg {
	case "RS256", "PS256", "ES256", "HS256":
		return crypto.SHA256, nil
	case "RS384", "PS384", "ES384", "HS384":
		return crypto.SHA384, nil
	case "RS512", "PS512", "ES512", "HS512":
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("%w: unsupported signing algorithm %q", ErrInvalidToken, alg)
	}
}

func audienceContains(aud []string, clientID string) bool {
	for _, a := range aud {
		if a == clientID {
			return true
		}
	}
	return false
}

// keep the sha imports referenced: crypto.SHA256/384/512 need their packages
// linked in for hash.Hash construction via crypto.Hash.New.
var _ = []func() hash.Hash{sha256.New, sha512.New384, sha512.New}
