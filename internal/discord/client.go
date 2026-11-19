// Package discord sends alert notifications to Discord through its HTTP API
// and keeps each message up to date as the alert changes. docs/discord.md
// describes how to set up the Discord application.
//
// It was written from Discord's developer documentation, API version 10, as
// it stood in November 2026, and is tested against discordtest, a fake that
// checks requests against the documented shapes and limits.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/version"
)

// DefaultAPIURL is the base URL of the Discord HTTP API, with its version.
const DefaultAPIURL = "https://discord.com/api/v10"

// maxResponseBytes bounds how much of a response is read.
const maxResponseBytes = 1 << 20

// Client calls the Discord HTTP API as a bot.
type Client struct {
	baseURL string
	token   config.Secret
	http    *http.Client
	now     func() time.Time

	// mu guards limited, which remembers until when a rate limit that Discord
	// reported is in force, by route. Discord asks applications not to send
	// requests they know will be refused: refused requests count towards a
	// ban of the sender's address. It is a memory of this process only, and
	// losing it costs one refused request.
	mu      sync.Mutex
	limited map[string]time.Time
}

// NewClient returns a client. baseURL is DefaultAPIURL except in tests, which
// point it at a fake Discord.
func NewClient(baseURL string, token config.Secret) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// The pipeline also bounds each attempt; this covers other callers.
		http:    &http.Client{Timeout: 10 * time.Second},
		now:     time.Now,
		limited: map[string]time.Time{},
	}
}

// SetClock replaces the clock, so tests can move time.
func (c *Client) SetClock(now func() time.Time) { c.now = now }

// APIError is a refusal by the Discord API.
type APIError struct {
	// Operation says what Beacon was doing, such as "create message".
	Operation string
	// Status is the HTTP status, and Code the error code of the JSON body, or
	// zero when the body had none.
	Status int
	Code   int
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("discord %s: HTTP status %d", e.Operation, e.Status)
	if e.Code != 0 {
		msg += fmt.Sprintf(", error code %d", e.Code)
	}
	if hint, ok := hints[e.Code]; ok {
		msg += " (" + hint + ")"
	} else if hint, ok := statusHints[e.Status]; ok {
		msg += " (" + hint + ")"
	}
	return msg
}

// The error codes Beacon knows by name.
const (
	codeUnknownChannel = 10003
	codeUnknownMessage = 10008
	codeMissingAccess  = 50001
	codeCannotDM       = 50007
	codeMissingPerms   = 50013
	codeNoMutualGuild  = 50278
	codeInvalidForm    = 50035
	codeUnknownUser    = 10013
)

// globalRateLimitKey is the route under which a limit on every route is
// remembered.
const globalRateLimitKey = "global"

// hints explain the error codes an operator is most likely to meet. The text
// of Discord's own message is not repeated: it is not needed, and nothing
// that Discord sends is put into an error unseen.
var hints = map[int]string{
	codeUnknownChannel: "the channel does not exist, or the bot cannot see it; use the channel's numeric ID",
	codeUnknownMessage: "the message no longer exists",
	codeUnknownUser:    "Discord does not know this user",
	codeMissingAccess:  "the bot cannot see the channel: add it to the server and give it the View Channel permission there",
	codeCannotDM:       "the person does not accept direct messages from the bot: they must share a server with it and allow direct messages from server members",
	codeNoMutualGuild:  "the person shares no server with the bot, so it cannot send them a direct message",
	codeMissingPerms:   "the bot lacks a permission in the channel: it needs View Channel, Send Messages and Embed Links",
	codeInvalidForm:    "Discord refused the content of the request",
}

var statusHints = map[int]string{
	http.StatusUnauthorized: "the bot token is not valid",
	http.StatusForbidden:    "the bot is not allowed to do this",
	http.StatusNotFound:     "Discord does not know the channel, the message or the user",
}

// do sends one request to the API and decodes the answer into out, when out
// is not nil. operation names what is being done, for errors; route names
// the rate limit that the request falls under.
//
// Its errors say whether a retry can help, in the pipeline's terms, and never
// contain the token.
func (c *Client) do(ctx context.Context, method, path, operation, route string, body, out any) error {
	if wait := c.wait(route); wait > 0 {
		// Not sent: Discord said that it would be refused until then.
		return notify.RetryAfter(fmt.Errorf("discord %s: not sent, the rate limit Discord reported is still in force", operation), wait)
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return notify.Permanent(fmt.Errorf("discord %s: encode request: %w", operation, err))
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return notify.Permanent(fmt.Errorf("discord %s: the request could not be built", operation))
	}
	req.Header.Set("Authorization", "Bot "+c.token.Reveal())
	// Discord asks for this form, and may block requests without it.
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/thanoslabs/beacon, "+version.Version+")")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// The error names the operation, never a header or the URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("discord %s: %w", operation, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do if closing a read body fails
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("discord %s: read response: %w", operation, err)
	}
	c.remember(route, resp, raw)

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		err := fmt.Errorf("discord %s: rate limited", operation)
		if wait := retryAfter(resp, raw); wait > 0 {
			return notify.RetryAfter(err, wait)
		}
		return err
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusRequestTimeout:
		return fmt.Errorf("discord %s: HTTP status %d", operation, resp.StatusCode)
	case resp.StatusCode >= 400:
		// The request itself is at fault: the token, the destination or the
		// content. Sending it again would be refused again, and Discord
		// counts refusals against the sender.
		apiErr := &APIError{Operation: operation, Status: resp.StatusCode}
		var problem struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(raw, &problem) == nil {
			apiErr.Code = problem.Code
		}
		return notify.Permanent(apiErr)
	case resp.StatusCode >= 300:
		return notify.Permanent(fmt.Errorf("discord %s: unexpected HTTP status %d", operation, resp.StatusCode))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("discord %s: the response is not the expected JSON", operation)
	}
	return nil
}

// wait returns how long the route is still rate limited, as far as this
// process has been told.
func (c *Client) wait(route string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	var wait time.Duration
	for key, until := range c.limited {
		switch {
		case !until.After(now):
			// Forgotten once over, which also keeps the map small.
			delete(c.limited, key)
		case key == route || key == globalRateLimitKey:
			wait = max(wait, until.Sub(now))
		}
	}
	return wait
}

// maxRememberedLimit bounds how long a limit is remembered, whatever a
// response says, so that one odd header cannot silence a route for a day.
const maxRememberedLimit = 10 * time.Minute

// remember notes what a response says about the rate limit of its route:
// that the route is used up until it resets, or, on a 429, how long to wait
// and whether that goes for every route.
func (c *Client) remember(route string, resp *http.Response, raw []byte) {
	var wait time.Duration
	key := route
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		wait = retryAfter(resp, raw)
		if resp.Header.Get("X-RateLimit-Global") == "true" || resp.Header.Get("X-RateLimit-Scope") == "global" {
			key = globalRateLimitKey
		}
	case resp.Header.Get("X-RateLimit-Remaining") == "0":
		wait = seconds(resp.Header.Get("X-RateLimit-Reset-After"))
	}
	if wait <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limited[key] = c.now().Add(min(wait, maxRememberedLimit))
}

// retryAfter returns how long a 429 response asks to wait: the Retry-After
// header, or the retry_after field of the body. Both are seconds and may
// have decimals.
func retryAfter(resp *http.Response, raw []byte) time.Duration {
	if d := seconds(resp.Header.Get("Retry-After")); d > 0 {
		return d
	}
	var body struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(raw, &body) == nil && body.RetryAfter > 0 {
		return time.Duration(body.RetryAfter * float64(time.Second))
	}
	return 0
}

// seconds reads a number of seconds, with or without decimals. It returns
// zero for anything else.
func seconds(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 || f > 1e6 {
		return 0
	}
	// Rounded up to the millisecond, so that the wait is never too short.
	return time.Duration(f*1000+0.999) * time.Millisecond
}

// createMessage posts a message to a channel, given by its ID, and returns
// the ID of the message.
func (c *Client) createMessage(ctx context.Context, channelID string, m message) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	path := "/channels/" + url.PathEscape(channelID) + "/messages"
	if err := c.do(ctx, http.MethodPost, path, "create message", "POST "+path, m, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("discord create message: the response has no message ID")
	}
	return out.ID, nil
}

// editMessage replaces the content of a message that the bot posted.
func (c *Client) editMessage(ctx context.Context, channelID, messageID string, m message) error {
	channel := "/channels/" + url.PathEscape(channelID) + "/messages"
	// Rate limits are per channel, not per message.
	return c.do(ctx, http.MethodPatch, channel+"/"+url.PathEscape(messageID), "edit message", "PATCH "+channel, m, nil)
}

// createDM opens the direct message channel between the bot and a user, or
// returns the one that exists, and returns its ID.
func (c *Client) createDM(ctx context.Context, userID string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	const path = "/users/@me/channels"
	if err := c.do(ctx, http.MethodPost, path, "open direct message", "POST "+path, map[string]string{"recipient_id": userID}, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("discord open direct message: the response has no channel ID")
	}
	return out.ID, nil
}
