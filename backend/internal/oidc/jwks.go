package oidc

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"
)

// jwk is one JSON Web Key. Only the fields AO needs to reconstruct a public
// key are modeled; a key of an unsupported type is skipped, not an error, so
// a provider that publishes an exotic key alongside its signing keys still
// works.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	// RSA
	N string `json:"n"`
	E string `json:"e"`
	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

// parsedKey is a usable public key with the identifiers needed to select it.
type parsedKey struct {
	kid string
	alg string
	key crypto.PublicKey
}

type keySet struct {
	keys []parsedKey
}

// keysFor returns the candidate keys for a token header. An exact kid match
// wins; a token with no kid, or a kid the set does not know, falls back to
// every key whose declared alg is compatible — which is what makes a provider
// that publishes a single unlabeled key work, without ever letting a token
// pick a key of the wrong family.
func (s *keySet) keysFor(kid, alg string) []parsedKey {
	if kid != "" {
		var exact []parsedKey
		for _, k := range s.keys {
			if k.kid == kid {
				exact = append(exact, k)
			}
		}
		if len(exact) > 0 {
			return exact
		}
	}
	var compatible []parsedKey
	for _, k := range s.keys {
		if k.alg != "" && k.alg != alg {
			continue
		}
		compatible = append(compatible, k)
	}
	return compatible
}

// jwks returns the provider's key set, refetching when the cache is stale.
// forceRefresh bypasses the cache once, which is what a token naming an
// unknown kid triggers: the standard response to a key rotation is one extra
// fetch, not a failed login until the TTL lapses.
func (c *Client) jwks(ctx context.Context, forceRefresh bool) (*keySet, error) {
	if !forceRefresh {
		c.mu.Lock()
		if c.keys != nil && c.now().Sub(c.keysFetchedAt) < jwksTTL {
			ks := c.keys
			c.mu.Unlock()
			return ks, nil
		}
		c.mu.Unlock()
	}

	meta, err := c.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	var doc jwksDocument
	if err := c.getJSON(ctx, meta.JWKSURI, &doc); err != nil {
		return nil, fmt.Errorf("%w: jwks: %w", ErrProviderUnreachable, err)
	}
	ks := &keySet{}
	for _, k := range doc.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil || pub == nil {
			continue
		}
		ks.keys = append(ks.keys, parsedKey{kid: k.Kid, alg: k.Alg, key: pub})
	}
	if len(ks.keys) == 0 {
		return nil, fmt.Errorf("%w: jwks carried no usable signing key", ErrProviderUnreachable)
	}

	c.mu.Lock()
	c.keys = ks
	c.keysFetchedAt = c.now()
	c.mu.Unlock()
	return ks, nil
}

// jwksRefreshedAt reports when the cached key set was fetched, so a caller can
// avoid a second refetch inside one verification.
func (c *Client) jwksRefreshedAt() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keysFetchedAt
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64uBigInt(k.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		if len(eBytes) == 0 || len(eBytes) > 8 {
			return nil, fmt.Errorf("rsa exponent out of range")
		}
		padded := make([]byte, 8)
		copy(padded[8-len(eBytes):], eBytes)
		e := binary.BigEndian.Uint64(padded)
		if e == 0 || e > 1<<31 {
			return nil, fmt.Errorf("rsa exponent out of range")
		}
		return &rsa.PublicKey{N: n, E: int(e)}, nil
	case "EC":
		var curve elliptic.Curve
		var ecdhCurve ecdh.Curve
		switch k.Crv {
		case "P-256":
			curve, ecdhCurve = elliptic.P256(), ecdh.P256()
		case "P-384":
			curve, ecdhCurve = elliptic.P384(), ecdh.P384()
		case "P-521":
			curve, ecdhCurve = elliptic.P521(), ecdh.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err := b64uBigInt(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64uBigInt(k.Y)
		if err != nil {
			return nil, err
		}
		// Validate the point through crypto/ecdh's uncompressed-point parser
		// rather than elliptic.IsOnCurve (deprecated, and a low-level unsafe
		// API): a key off the curve is a key an attacker chose, and the
		// signature check must never be handed one.
		byteLen := (curve.Params().BitSize + 7) / 8
		uncompressed := make([]byte, 1+2*byteLen)
		uncompressed[0] = 4
		x.FillBytes(uncompressed[1 : 1+byteLen])
		y.FillBytes(uncompressed[1+byteLen:])
		if _, err := ecdhCurve.NewPublicKey(uncompressed); err != nil {
			return nil, fmt.Errorf("ec point is not on curve %s: %w", k.Crv, err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
	default:
		// oct (symmetric) keys are never taken from a public JWKS: doing so is
		// the classic algorithm-confusion foothold. HS* verification uses the
		// configured client secret and nothing else.
		return nil, nil
	}
}

func b64uBigInt(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty big-endian integer")
	}
	return new(big.Int).SetBytes(b), nil
}
