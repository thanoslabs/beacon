package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/thanoslabs/beacon/internal/notify"
)

// Channel delivers notifications to Slack. It implements notify.Channel.
//
// The first notification about an alert group posts a message. Updates
// edit that message in place, so a destination shows one message per group
// with its current state. An escalation step that reaches a destination
// again posts a new message, because an edit notifies nobody; the message it
// replaces is turned into a pointer to it.
type Channel struct {
	client *Client
}

// NewChannel returns a channel that sends through client.
func NewChannel(client *Client) *Channel {
	return &Channel{client: client}
}

// Send posts the message for m, or updates the one posted before. The
// reference it returns is the channel ID and message timestamp, which is what
// Slack needs to update the message later.
func (c *Channel) Send(ctx context.Context, m notify.Message) (string, error) {
	msg := render(m)
	channelID, ts, hasPrevious := strings.Cut(m.PreviousRef, ":")
	if hasPrevious && m.Notification.Reason == notify.ReasonEscalationStep {
		// Asking again must be heard. Editing the message that is there
		// would change it silently, so the alert is said again.
		newChannel, newTS, err := c.client.postMessage(ctx, strings.TrimPrefix(m.Notification.Destination, "#"), msg)
		if err != nil {
			return "", err
		}
		// The old message would go stale: updates follow the new one. It
		// becomes a pointer. If that fails the page was still delivered,
		// and the old message merely keeps its last state.
		_ = c.client.updateMessage(ctx, channelID, ts, superseded(msg))
		return newChannel + ":" + newTS, nil
	}
	if hasPrevious {
		err := c.client.updateMessage(ctx, channelID, ts, msg)
		if err == nil {
			return m.PreviousRef, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !gone[apiErr.Code] {
			return "", err
		}
		// The message was deleted or can no longer be edited: say it again.
	}
	// Slack takes a public channel by name without the leading "#".
	channelID, ts, err := c.client.postMessage(ctx, strings.TrimPrefix(m.Notification.Destination, "#"), msg)
	if err != nil {
		return "", err
	}
	return channelID + ":" + ts, nil
}
