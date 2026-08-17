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
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AuthSession is one durable session-token row. The raw token is only ever
// handed to the client once, in the Set-Cookie response at login; TokenHash
// (its SHA-256) is the only form stored server-side, and — like
// PasswordHash — must never be exposed in any JSON-serializable DTO.
type AuthSession struct {
	ID         string
	UserID     UserID
	TokenHash  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}
