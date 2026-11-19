// Package email sends alert notifications as email, over SMTP. docs/email.md
// describes the configuration and what a message holds.
package email

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
)

// Channel delivers notifications as email. It implements notify.Channel.
//
// An email cannot be changed once it is sent, so every notification about a
// group is a message of its own, with the group as it is at that moment.
// The messages about one group refer to each other, so that a mail client
// shows them as one conversation.
//
// A destination is an email address: the one a channel target names, or
// the address of the user a notification is for.
type Channel struct {
	server server
	// publicURL is the address of Beacon, for the link to the alert. It is
	// empty when Beacon does not know it.
	publicURL string
	// domain is the part of the sender's address after the at sign, under
	// which message identifiers are made.
	domain string
	now    func() time.Time
}

// NewChannel returns a channel that sends through the SMTP server of cfg.
// publicURL is the address at which people reach Beacon, or empty.
func NewChannel(cfg config.EmailIntegration, publicURL string) *Channel {
	_, domain, _ := strings.Cut(cfg.From, "@")
	if !domainRE.MatchString(domain) {
		domain = "beacon.invalid"
	}
	return &Channel{server: server{cfg: cfg}, publicURL: publicURL, domain: domain, now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (c *Channel) SetClock(now func() time.Time) { c.now = now }

var (
	domainRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	// tokenRE matches what Beacon puts before the at sign of a message
	// identifier: the identifiers of its own rows.
	tokenRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	// messageIDRE matches a message identifier that Beacon made, which is
	// the only kind it puts back into a header.
	messageIDRE = regexp.MustCompile(`^<beacon-[A-Za-z0-9.-]{1,80}@[A-Za-z0-9.-]{1,253}>$`)
)

// Send sends one email about the group to the destination. The reference
// it returns is the identifier of the message, which the next message about
// the group answers.
//
// The identifier is made from the notification, so an attempt that is
// repeated after the server had already taken the message sends the same
// message again, under the same identifier, and a mail system that removes
// duplicates can.
func (c *Channel) Send(ctx context.Context, m notify.Message) (string, error) {
	to, err := recipient(m.Notification.Destination)
	if err != nil {
		return "", notify.Permanent(err)
	}
	id := m.Notification.ID
	if !tokenRE.MatchString(id) {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return "", fmt.Errorf("email: make a message identifier: %w", err)
		}
		id = hex.EncodeToString(raw)
	}
	env := envelope{from: c.server.cfg.From, to: to, date: c.now(), id: "<beacon-" + id + "@" + c.domain + ">"}
	if tokenRE.MatchString(m.Notification.GroupID) {
		env.thread = "<beacon-group-" + m.Notification.GroupID + "@" + c.domain + ">"
		env.inReplyTo = env.thread
	}
	// Only an identifier that Beacon made goes back into a header.
	if messageIDRE.MatchString(m.PreviousRef) {
		env.inReplyTo = m.PreviousRef
	}
	message, err := compose(env, describe(m, c.publicURL))
	if err != nil {
		return "", notify.Permanent(fmt.Errorf("email: build the message: %w", err))
	}
	if err := c.server.send(ctx, to, message); err != nil {
		return "", err
	}
	return env.id, nil
}

// recipient checks that a destination is one plain email address, and
// returns it. An address with a display name, a list of addresses, or
// anything with a line break in it is refused: the destination goes into
// the To header and into the SMTP exchange as it is.
func recipient(destination string) (string, error) {
	addr, err := mail.ParseAddress(destination)
	if err != nil || addr.Address != destination || strings.ContainsAny(destination, "\r\n<>\" ") {
		return "", errors.New("email: the destination is not an email address")
	}
	return destination, nil
}
