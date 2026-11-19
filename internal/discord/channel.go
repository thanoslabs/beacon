package discord

import (
	"context"
	"errors"
	"strings"

	"github.com/thanoslabs/beacon/internal/notify"
)

// Provider is the name under which Discord identities are recorded, as in
// user_identities.provider.
const Provider = "discord"

// Channel delivers notifications to Discord. It implements notify.Channel.
//
// The first notification about an alert group posts a message. Updates edit
// that message in place, so a destination shows one message per group with
// its current state. An escalation step that reaches a destination again
// posts a new message, because an edit notifies nobody; the message it
// replaces is turned into a pointer to it.
//
// A destination is the ID of a channel, or, for a notification to one
// person, the ID of their Discord account: the message then goes to the
// direct message channel between the bot and that person.
type Channel struct {
	client *Client
}

// NewChannel returns a channel that sends through client.
func NewChannel(client *Client) *Channel {
	return &Channel{client: client}
}

// Send posts the message for m, or edits the one posted before. The
// reference it returns is the channel ID and the message ID, which is what
// Discord needs to edit the message later.
func (c *Channel) Send(ctx context.Context, m notify.Message) (string, error) {
	msg := render(m)
	channelID, messageID, hasPrevious := strings.Cut(m.PreviousRef, ":")
	if hasPrevious && m.Notification.Reason == notify.ReasonEscalationStep {
		// Asking again must be heard. Editing the message that is there
		// would change it silently, so the alert is said again, in the
		// channel the last message went to.
		newID, err := c.client.createMessage(ctx, channelID, msg)
		if err != nil {
			return "", err
		}
		// The old message would go stale: updates follow the new one. It
		// becomes a pointer. If that fails the page was still delivered, and
		// the old message merely keeps its last state.
		_ = c.client.editMessage(ctx, channelID, messageID, superseded(msg))
		return channelID + ":" + newID, nil
	}
	if hasPrevious {
		err := c.client.editMessage(ctx, channelID, messageID, msg)
		if err == nil {
			return m.PreviousRef, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != codeUnknownMessage {
			return "", err
		}
		// The message was deleted: say it again.
	}
	channelID, err := c.open(ctx, m.Notification)
	if err != nil {
		return "", err
	}
	messageID, err = c.client.createMessage(ctx, channelID, msg)
	if err != nil {
		return "", err
	}
	return channelID + ":" + messageID, nil
}

// open returns the channel a notification goes to: its destination, or, for
// a notification to one person, the direct message channel with them.
// Discord returns the channel that exists when there is one, so opening it
// again after a failed attempt is harmless.
func (c *Channel) open(ctx context.Context, n notify.Notification) (string, error) {
	if !snowflakeRE.MatchString(n.Destination) {
		return "", notify.Permanent(errors.New("discord: the destination is not a Discord ID; use the numeric ID of the channel"))
	}
	if n.UserID == "" {
		return n.Destination, nil
	}
	return c.client.createDM(ctx, n.Destination)
}
