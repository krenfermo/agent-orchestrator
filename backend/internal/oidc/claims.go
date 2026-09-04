package oidc

import (
	"encoding/json"
	"strings"
)

// rawClaims is the wire shape of an ID token payload / userinfo response. Two
// fields are deliberately typed as json.RawMessage because the specs allow
// more than one JSON type for them and real providers use both:
//
//   - aud is "a string or an array of strings" (Core 1.0 §2),
//   - email_verified is a boolean, but several providers emit the string
//     "true"/"false" instead.
//
// Decoding those into a fixed Go type is the classic way an integration works
// against one vendor and 400s against the next.
type rawClaims struct {
	Issuer            string          `json:"iss"`
	Subject           string          `json:"sub"`
	Audience          json.RawMessage `json:"aud"`
	AuthorizedParty   string          `json:"azp"`
	Expiry            int64           `json:"exp"`
	IssuedAt          int64           `json:"iat"`
	NotBefore         int64           `json:"nbf"`
	Nonce             string          `json:"nonce"`
	Email             string          `json:"email"`
	EmailVerified     json.RawMessage `json:"email_verified"`
	Name              string          `json:"name"`
	GivenName         string          `json:"given_name"`
	FamilyName        string          `json:"family_name"`
	PreferredUsername string          `json:"preferred_username"`
	// Extra keeps every remaining claim so an operator-configured claim
	// constraint can be evaluated against claims AO has no field for
	// (groups, roles, hd, …) without this struct having to know them.
	Extra map[string]json.RawMessage `json:"-"`
}

// IDTokenClaims is the verified, normalized identity a provider asserted.
// It is what leaves this package; no raw token ever does.
type IDTokenClaims struct {
	Issuer   string
	Subject  string
	Audience []string
	Nonce    string
	Expiry   int64
	IssuedAt int64

	Email             string
	EmailVerified     bool
	Name              string
	GivenName         string
	FamilyName        string
	PreferredUsername string

	// Extra carries every other claim, for operator-configured constraints.
	Extra map[string]json.RawMessage
}

// DisplayName picks the best human label the provider gave us, falling back
// through the standard claims and finally to the local part of the email.
// It never falls back to the subject: an opaque provider id is not a name.
func (c *IDTokenClaims) DisplayName() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if full := strings.TrimSpace(strings.TrimSpace(c.GivenName) + " " + strings.TrimSpace(c.FamilyName)); full != "" {
		return full
	}
	if n := strings.TrimSpace(c.PreferredUsername); n != "" {
		return n
	}
	if i := strings.Index(c.Email, "@"); i > 0 {
		return c.Email[:i]
	}
	return ""
}

// EmailDomain returns the lowercased domain part of the email claim, or "".
func (c *IDTokenClaims) EmailDomain() string {
	i := strings.LastIndex(c.Email, "@")
	if i < 0 || i == len(c.Email)-1 {
		return ""
	}
	return strings.ToLower(c.Email[i+1:])
}

// HasClaimValue reports whether claim `name` carries `value`, matching either
// a string claim equal to it or a string-array claim containing it. This is
// what backs the operator's optional claim constraint (a groups/roles gate);
// anything more expressive would be a policy language, which is P4-B's
// problem, not P4-A's.
func (c *IDTokenClaims) HasClaimValue(name, value string) bool {
	raw, ok := c.Extra[name]
	if !ok {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == value
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, v := range many {
			if v == value {
				return true
			}
		}
	}
	return false
}

// mergeDisplayClaims tops up empty display fields from a userinfo response.
// It never overwrites a claim the ID token already asserted, and it never
// touches issuer/subject: the identity is settled before userinfo is called.
func (c *IDTokenClaims) mergeDisplayClaims(info rawClaims) {
	if c.Email == "" {
		c.Email = strings.ToLower(strings.TrimSpace(info.Email))
		c.EmailVerified = decodeFlexibleBool(info.EmailVerified)
	}
	if c.Name == "" {
		c.Name = info.Name
	}
	if c.GivenName == "" {
		c.GivenName = info.GivenName
	}
	if c.FamilyName == "" {
		c.FamilyName = info.FamilyName
	}
	if c.PreferredUsername == "" {
		c.PreferredUsername = info.PreferredUsername
	}
	for k, v := range info.Extra {
		if _, exists := c.Extra[k]; !exists {
			c.Extra[k] = v
		}
	}
}

// decodeAudience accepts both spec-legal shapes for `aud`.
func decodeAudience(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil
		}
		return []string{single}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// decodeFlexibleBool accepts a JSON boolean or the strings "true"/"false".
func decodeFlexibleBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(s, "true")
	}
	return false
}

// knownClaimKeys are the claims rawClaims has typed fields for; everything
// else lands in Extra.
var knownClaimKeys = map[string]struct{}{
	"iss": {}, "sub": {}, "aud": {}, "azp": {}, "exp": {}, "iat": {}, "nbf": {},
	"nonce": {}, "email": {}, "email_verified": {}, "name": {},
	"given_name": {}, "family_name": {}, "preferred_username": {},
}

// parseRawClaims decodes a claims payload twice: once into the typed struct,
// once into a generic map so unmodeled claims survive into Extra.
func parseRawClaims(payload []byte) (rawClaims, error) {
	var rc rawClaims
	if err := json.Unmarshal(payload, &rc); err != nil {
		return rawClaims{}, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(payload, &all); err != nil {
		return rawClaims{}, err
	}
	rc.Extra = map[string]json.RawMessage{}
	for k, v := range all {
		if _, known := knownClaimKeys[k]; known {
			continue
		}
		rc.Extra[k] = v
	}
	return rc, nil
}

func (rc rawClaims) normalize() *IDTokenClaims {
	return &IDTokenClaims{
		Issuer:            rc.Issuer,
		Subject:           rc.Subject,
		Audience:          decodeAudience(rc.Audience),
		Nonce:             rc.Nonce,
		Expiry:            rc.Expiry,
		IssuedAt:          rc.IssuedAt,
		Email:             strings.ToLower(strings.TrimSpace(rc.Email)),
		EmailVerified:     decodeFlexibleBool(rc.EmailVerified),
		Name:              strings.TrimSpace(rc.Name),
		GivenName:         strings.TrimSpace(rc.GivenName),
		FamilyName:        strings.TrimSpace(rc.FamilyName),
		PreferredUsername: strings.TrimSpace(rc.PreferredUsername),
		Extra:             rc.Extra,
	}
}
