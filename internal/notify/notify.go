// Package notify delivers notifications. It keeps a durable queue in
// Postgres, sends each queued notification through the channel of its
// integration, retries failures with backoff, and records every attempt.
//
// A channel is anything that implements Channel. The pipeline knows nothing
// else about Slack, email or any other service.
package notify

import (
	"context"
	"errors"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
)

// Reason says why a notification was queued.
type Reason string

// The reasons.
const (
	// ReasonEscalationStep is a notification that asks someone to respond.
	ReasonEscalationStep Reason = "escalation_step"
	// ReasonGroupUpdate tells the destination that the state changed.
	ReasonGroupUpdate Reason = "group_update"
	// ReasonResolved tells the destination that the problem is over.
	ReasonResolved Reason = "resolved"
)

// The events the pipeline writes on the alert timeline, one per attempt.
const (
	EventSent   alert.EventType = "notification_sent"
	EventFailed alert.EventType = "notification_failed"
)

// The outcomes of a delivery attempt.
const (
	OutcomeSuccess        = "success"
	OutcomeRetryableError = "retryable_error"
	OutcomePermanentError = "permanent_error"
)

// Notification is one message to one destination.
type Notification struct {
	ID string
	// GroupID is the alert group the message is about.
	GroupID string
	// IntegrationID is the integration to send through.
	IntegrationID string
	// Destination is where the integration should deliver: a channel, an
	// address or a URL.
	Destination string
	Reason      Reason
	// Attempts counts delivery attempts, including the one in progress.
	Attempts  int
	CreatedAt time.Time
}

// Message is what a channel is asked to deliver. It describes the group as it
// is when the attempt is made, not as it was when the notification was
// queued, so a late delivery never shows a stale state.
type Message struct {
	Notification Notification
	// Alerts are the alerts of the group, oldest first.
	Alerts []alert.Alert
	// Events is the timeline of the group, oldest first.
	Events []alert.Event
	// PreviousRef is the reference the channel returned for the last message
	// it delivered about this group to this destination, or empty. A channel
	// that can change a message in place uses it to update that message
	// rather than send another.
	PreviousRef string
}

// Channel delivers messages through one integration.
type Channel interface {
	// Send delivers m and returns a reference to the delivered message, such
	// as a message identifier, which later messages receive as PreviousRef.
	//
	// A returned error means the message was not delivered. The pipeline
	// retries unless the error was made with Permanent. Send must honour ctx,
	// which carries the deadline for the attempt, and must not put
	// credentials in its errors: they are stored and shown on the timeline.
	Send(ctx context.Context, m Message) (ref string, err error)
}

// deliveryError carries what a channel knows about a failure.
type deliveryError struct {
	err        error
	permanent  bool
	retryAfter time.Duration
}

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

// Permanent marks err as a failure that retrying cannot fix, such as a
// destination that does not exist. The notification is marked failed at once.
func Permanent(err error) error {
	return &deliveryError{err: err, permanent: true}
}

// RetryAfter marks err as a failure to retry no sooner than after d, for a
// service that says how long to wait.
func RetryAfter(err error, d time.Duration) error {
	return &deliveryError{err: err, retryAfter: d}
}

// classify returns whether err is permanent and the wait the channel asked
// for, if any.
func classify(err error) (permanent bool, retryAfter time.Duration) {
	var de *deliveryError
	if errors.As(err, &de) {
		return de.permanent, de.retryAfter
	}
	return false, 0
}
