package ports

import (
	"context"
	"errors"
)

// EmailMessage is one rendered message handed to a transport. It is already
// addressed by the transport's own configuration -- AO resolves the recipient
// from backend-owned settings, never from anything a notification carries -- so
// this is only the content.
//
// P4-D section 9: bodies are concise summaries. They name the project, the run,
// what happened and whether a person is needed. They never carry prompts,
// terminal transcripts, secrets, or raw provider output.
type EmailMessage struct {
	Subject string
	Body    string
}

// EmailTransport delivers one rendered message. It is the seam every outbound
// email goes through, so a future channel (SES, Postmark, a test double) is a
// new implementation rather than a change to the outbox.
//
// Errors are classified by the caller with PermanentDeliveryError: a permanent
// failure is dead-lettered immediately, anything else is retried on the
// entry's remaining budget.
type EmailTransport interface {
	// Send delivers msg, or reports why it could not.
	Send(ctx context.Context, msg EmailMessage) error
}

// ErrEmailTransportUnavailable reports that no transport is configured, or that
// the configured one cannot currently produce a send (email disabled, SMTP
// settings incomplete).
//
// This is deliberately NOT a delivery failure: P4-D section 8 requires that
// in-app notifications keep working when email is absent, so an unavailable
// transport suppresses the entry rather than retrying it forever or failing the
// work being reported.
var ErrEmailTransportUnavailable = errors.New("email transport unavailable")

// ErrEmailSuppressed reports that a transport deliberately declined to send:
// the user did not select this event, or email is switched off. Like
// ErrEmailTransportUnavailable it is a normal outcome, not a failure.
var ErrEmailSuppressed = errors.New("email suppressed by settings")

// ErrPermanentEmailFailure marks a failure that retrying cannot fix -- a
// rejected recipient, a refused authentication, a malformed message. Wrapping
// an error with it moves the outbox entry straight to dead instead of spending
// its remaining attempts on something that will fail identically each time.
var ErrPermanentEmailFailure = errors.New("permanent email failure")

// PermanentDeliveryError reports whether err should end delivery attempts.
func PermanentDeliveryError(err error) bool {
	return errors.Is(err, ErrPermanentEmailFailure)
}
