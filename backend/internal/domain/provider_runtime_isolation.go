package domain

import (
	"fmt"
	"strings"
)

// ProviderRuntimeIsolation is where a provider CLI subprocess keeps its
// configuration and credentials: in an AO-owned per-user runtime home, or in
// the desktop user's own.
//
// It is its own setting because it used to be a consequence of an unrelated
// one. Enabling OIDC sign-in set TrustedLocalMode=false, and providerruntime
// read that flag to decide isolation -- so a single-user desktop that added
// Google sign-in silently moved every provider launch onto a substituted HOME.
// On macOS a substituted HOME also substitutes the keychain domain (the
// default keychain and the user search list resolve out of
// $HOME/Library/Preferences), so the Claude credential the person had been
// using was no longer reachable and unattended launches began hanging on a
// keychain unlock dialog nobody could answer.
//
// "Who is signed in to AO" and "where AO's provider CLIs keep credentials" are
// different questions. This type lets the second one be answered on its own.
type ProviderRuntimeIsolation string

const (
	// ProviderRuntimeIsolationAuto derives isolation from the identity mode,
	// exactly as AO did before this setting existed: isolated in multi-user
	// mode, host runtime in trusted-local. It is the default so an install
	// that never sets this behaves as it always has.
	ProviderRuntimeIsolationAuto ProviderRuntimeIsolation = "auto"
	// ProviderRuntimeIsolationHost keeps the desktop user's own HOME,
	// CLAUDE_CONFIG_DIR and OS credential store for every provider launch,
	// whatever the identity mode. This is the supported posture for a
	// single-user desktop that uses SSO to sign in to AO but still wants its
	// provider CLIs to use the credentials that person already established.
	//
	// It is deliberately explicit: it means every AO user on this instance
	// shares the host's provider credentials, which is correct for one desktop
	// and wrong for a shared deployment.
	ProviderRuntimeIsolationHost ProviderRuntimeIsolation = "host"
	// ProviderRuntimeIsolationStrict always prepares an AO-owned per-user
	// runtime home, whatever the identity mode. No AO user can reach another's
	// -- or the daemon host's -- provider credentials.
	ProviderRuntimeIsolationStrict ProviderRuntimeIsolation = "strict"
)

// IsolateFor resolves the effective decision for one launch. trustedLocal is
// config.TrustedLocalMode, which auto continues to defer to.
func (i ProviderRuntimeIsolation) IsolateFor(trustedLocal bool) bool {
	switch i {
	case ProviderRuntimeIsolationHost:
		return false
	case ProviderRuntimeIsolationStrict:
		return true
	default:
		return !trustedLocal
	}
}

// ParseProviderRuntimeIsolation validates an operator-supplied value.
func ParseProviderRuntimeIsolation(raw string) (ProviderRuntimeIsolation, error) {
	switch v := ProviderRuntimeIsolation(strings.ToLower(strings.TrimSpace(raw))); v {
	case "", ProviderRuntimeIsolationAuto:
		return ProviderRuntimeIsolationAuto, nil
	case ProviderRuntimeIsolationHost, ProviderRuntimeIsolationStrict:
		return v, nil
	default:
		return "", fmt.Errorf("invalid provider runtime isolation %q: must be auto, host or strict", raw)
	}
}
