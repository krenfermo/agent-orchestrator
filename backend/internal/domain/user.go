package domain

import "time"

// UserID identifies an AO user account (Checkpoint 8P-A). It is a distinct
// string type, mirroring ProjectID, so a raw string cannot be passed where a
// resolved user id is expected without an explicit conversion.
type UserID string

// UserStatus is the lifecycle state of a user account.
type UserStatus string

const (
	// UserStatusActive is a normal, loginable account.
	UserStatusActive UserStatus = "active"
	// UserStatusDisabled is an account that exists (its rows/ownership stay
	// intact) but can no longer authenticate.
	UserStatusDisabled UserStatus = "disabled"
)

// UserRole distinguishes the installation owner from ordinary members
// (Checkpoint 8P-E.8). Exactly one active row may ever hold UserRoleOwner --
// enforced by the users(role) WHERE role='owner' partial unique index, not
// by application code -- so "the owner" is a durable, race-safe concept
// instead of the "earliest-created active user" heuristic 8P-A used.
type UserRole string

const (
	// UserRoleOwner is the installation owner/admin: the account created by
	// Bootstrap (env vars) or RegisterFirstUser (in-product first-run flow).
	UserRoleOwner UserRole = "owner"
	// UserRoleMember is every other account.
	UserRoleMember UserRole = "member"
)

// User is the durable identity record a resolved session belongs to.
//
// PasswordHash must never be exposed in any JSON-serializable DTO -- it is
// deliberately excluded from every response shape in httpd/controllers; only
// this internal domain type carries it, and only as far as authsvc needs to
// compare/rotate it.
type User struct {
	ID           UserID
	DisplayName  string
	Email        string
	Username     string
	PasswordHash string
	Status       UserStatus
	Role         UserRole
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AuthSession is one durable session-token row. The raw token is only ever
// handed to the client once, in the Set-Cookie response at login; TokenHash
// (its SHA-256) is the only form stored server-side, and — like
// PasswordHash — must never be exposed in any JSON-serializable DTO.
//
// P4-A: a session also records how it was authenticated. AuthMethod is
// AuthMethodPassword for every session predating that work (migration 0151
// backfills it), and Issuer/Subject are populated only for AuthMethodOIDC.
// The provider's own access/refresh tokens are deliberately absent: AO issues
// its own session and never re-presents a provider token as one.
type AuthSession struct {
	ID         string
	UserID     UserID
	TokenHash  string
	AuthMethod AuthMethod
	Issuer     string
	Subject    string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}
