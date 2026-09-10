package skillregistry

import (
	"fmt"
	"strings"
)

// publisheridentity.go -- who a release from a git host is BY, and the four
// different things that question can mean.
//
// # The confusion this file exists to prevent
//
// On a forge, "who published this" has at least four answers and they are
// routinely different people:
//
//	the HOST identity      the account or organization the repository sits
//	                       under. Anybody can create one, and names are
//	                       recycled when accounts are deleted.
//	the DECLARED publisher the name inside skill.yaml. It is a string the
//	                       author typed, and typing "anthropic" costs nothing.
//	the SIGNING publisher  the identity a trust root anchors and a key signs
//	                       for. This is the only cryptographic one.
//	the CONFIGURED expectation what THIS installation said it would accept.
//
// A system that collapses them ends up saying "published by acme" because a
// repository at github.com/acme said so, which is the impersonation this whole
// phase is built to refuse. So an ExternalIdentity carries all four, renders
// all four, and the checks below compare them explicitly and name which pair
// disagreed.
//
// # The rule, in one sentence
//
// A GitHub username is not a cryptographic publisher, and AO never treats one
// as the other. Where they happen to match, that is a coincidence worth
// SHOWING and never a check that passed.

// ExternalIdentity is everything AO knows about who is behind one external
// release, kept as separate facts rather than merged into a name.
type ExternalIdentity struct {
	// Provider and Owner are the HOST identity: where the bytes are hosted and
	// under whose account. Public, unauthenticated, and not evidence.
	Provider SourceProvider `json:"provider,omitempty"`
	Owner    string         `json:"owner,omitempty"`
	// Repository completes the host identity.
	Repository string `json:"repository,omitempty"`
	// DeclaredPublisher is the publisher the release names and the manifest
	// repeats. AO checks the two agree with each other; it cannot check that
	// either is true.
	DeclaredPublisher string `json:"declaredPublisher,omitempty"`
	// SigningPublisher is the identity the verified signing key speaks for.
	// Empty when nothing was verified, which is the ordinary case for an
	// unsigned external release.
	SigningPublisher string `json:"signingPublisher,omitempty"`
	// ExpectedPublisher is what this installation configured, or empty when it
	// configured none.
	ExpectedPublisher string `json:"expectedPublisher,omitempty"`
}

// ExternalIdentityOf builds the identity from a release and its registry.
func ExternalIdentityOf(rel Release, reg Registry) ExternalIdentity {
	return ExternalIdentity{
		Provider:          rel.Source.Provider,
		Owner:             rel.Source.Owner,
		Repository:        rel.Source.Repository,
		DeclaredPublisher: rel.Publisher,
		ExpectedPublisher: strings.TrimSpace(reg.PinnedPublisher),
	}
}

// WithSigner records the identity a verified signature established.
func (i ExternalIdentity) WithSigner(v Verification) ExternalIdentity {
	if v.Verified {
		i.SigningPublisher = v.Publisher
	}
	return i
}

// HostSlug is "owner/repository".
func (i ExternalIdentity) HostSlug() string {
	if i.Owner == "" {
		return ""
	}
	return i.Owner + "/" + i.Repository
}

// CryptographicallyBound reports whether a verified signature ties the
// declared publisher to a key AO trusts.
//
// It is the ONLY question whose "yes" means anything about identity. Every
// other field on this struct is a claim or a location.
func (i ExternalIdentity) CryptographicallyBound() bool {
	return i.SigningPublisher != "" && i.SigningPublisher == i.DeclaredPublisher
}

// OwnerMatchesPublisher reports whether the host account and the declared
// publisher happen to be spelled the same.
//
// It exists to be DISPLAYED, never to be relied on. A repository under
// github.com/acme declaring publisher "acme" is exactly as unverified as one
// declaring "globex"; what the coincidence buys a reader is one fewer thing to
// look up, and what it must never buy is a trust state.
func (i ExternalIdentity) OwnerMatchesPublisher() bool {
	return i.Owner != "" && strings.EqualFold(i.Owner, i.DeclaredPublisher)
}

// CheckExpectedPublisher refuses a release whose declared publisher is not the
// one this installation configured.
//
// It fails CLOSED and it is checked before any byte moves, because the point
// of pinning a publisher on an external registry is to catch the day the
// repository starts publishing under a different name -- which is the same day
// somebody would like the install to go through unnoticed.
func (i ExternalIdentity) CheckExpectedPublisher() error {
	expected := strings.TrimSpace(i.ExpectedPublisher)
	if expected == "" {
		return nil
	}
	if expected == strings.TrimSpace(i.DeclaredPublisher) {
		return nil
	}
	return fmt.Errorf("%w: %s publishes as %q and this registry is pinned to %q. AO refuses "+
		"rather than installing under a publisher nobody approved",
		ErrPublisherRefused, i.HostSlug(), i.DeclaredPublisher, expected)
}

// Describe renders the identity as the four separate facts it is.
//
// The sentence is built here rather than in a UI for the reason every other
// explanatory sentence in this package is: a screen that wrote its own would
// eventually write "published by acme", and that is the one thing this file
// exists to stop anybody saying.
func (i ExternalIdentity) Describe() string {
	parts := []string{}
	if slug := i.HostSlug(); slug != "" {
		parts = append(parts, fmt.Sprintf("hosted at %s %s", i.Provider, slug))
	}
	if i.DeclaredPublisher != "" {
		parts = append(parts, fmt.Sprintf("declares publisher %q (a claim in the package, not a "+
			"fact AO checked)", i.DeclaredPublisher))
	}
	switch {
	case i.CryptographicallyBound():
		parts = append(parts, fmt.Sprintf("and a signature AO verified binds those bytes to %q "+
			"under a trust root this installation configured", i.SigningPublisher))
	case i.SigningPublisher != "":
		parts = append(parts, fmt.Sprintf("and the signature AO verified speaks for %q, which is "+
			"not the publisher this package declares", i.SigningPublisher))
	default:
		parts = append(parts, "and nothing cryptographic ties that name to whoever wrote the code")
	}
	return strings.Join(parts, "; ") + "."
}

// ErrPublisherRefused marks a publisher that failed a configured expectation.
var ErrPublisherRefused = fmt.Errorf("%w: publisher refused", ErrInvalidRelease)

// ExternalHostingNotice is the sentence a client must show before an install
// from an external registry.
//
// It is served by the daemon rather than written in a UI for the same reason
// TrustExplanation is, and this one matters more than most: the comfortable
// version of this sentence is "from GitHub", which reads to almost everybody
// as an endorsement.
const ExternalHostingNotice = "Source: GitHub. Hosting on GitHub does not mean AO trusts the " +
	"publisher. AO checks that the bytes it fetched are the bytes the release named, at one exact " +
	"commit; it does not know who wrote them unless a signature chains to a trust root you " +
	"configured. Installing changes nothing else: no project is enabled, no capability is granted, " +
	"no image is approved and nothing is executed."
