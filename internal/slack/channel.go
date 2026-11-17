package slack

import (
	"context"
	"errors"
	"strings"

	"github.com/thanoslabs/beacon/internal/notify"
)

// Channel delivers notifications to Slack. It implements notify.Channel.
//
// The first notification about an alert group posts a message. Later ones
// edit that message in place, so a channel shows one message per alert with
// its current state.
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
	if channelID, ts, ok := strings.Cut(m.PreviousRef, ":"); ok {
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
