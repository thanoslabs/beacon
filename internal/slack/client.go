// Package slack sends alert notifications to Slack through the Slack Web API
// and keeps each message up to date as the alert changes. docs/slack.md
// describes how to set up the Slack app.
package slack

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
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
)

// DefaultAPIURL is the base URL of the Slack Web API.
const DefaultAPIURL = "https://slack.com/api"

// maxResponseBytes bounds how much of a response is read.
const maxResponseBytes = 1 << 20

// Client calls the Slack Web API with a bot token.
type Client struct {
	baseURL string
	token   config.Secret
	http    *http.Client
}

// NewClient returns a client. baseURL is DefaultAPIURL except in tests, which
// point it at a fake Slack.
func NewClient(baseURL string, token config.Secret) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		// The pipeline also bounds each attempt; this covers other callers.
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// APIError is an error code that the Slack API returned for a method.
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string {
	msg := "slack " + e.Method + ": " + e.Code
	if hint, ok := hints[e.Code]; ok {
		msg += " (" + hint + ")"
	}
	return msg
}

// hints explain the error codes an operator is most likely to meet.
var hints = map[string]string{
	"not_in_channel":        "invite the app to the channel, or add the chat:write.public scope for public channels",
	"channel_not_found":     "check the channel; a private channel must be given by its ID and the app must be invited to it",
	"invalid_auth":          "the bot token is not valid",
	"missing_scope":         "the app needs the chat:write scope, users:read.email to find people by email address, and users:read to name people who are not linked",
	"messages_tab_disabled": "the app cannot send direct messages: turn on the Messages tab under App Home in the app's settings",
}

// permanent lists the error codes that sending the same request again cannot
// fix: the credentials, the destination or the message itself is at fault.
// Any other code is retried, so a code Slack adds later is not given up on at
// the first attempt.
var permanent = map[string]bool{
	"access_denied": true, "account_inactive": true, "as_user_not_supported": true, "channel_not_found": true,
	"deprecated_endpoint": true, "ekm_access_denied": true, "enterprise_is_restricted": true,
	"invalid_arg_name": true, "invalid_arguments": true, "invalid_array_arg": true, "invalid_auth": true,
	"invalid_blocks": true, "invalid_blocks_format": true, "invalid_channel_id": true, "invalid_charset": true,
	"invalid_form_data": true, "invalid_post_type": true, "is_archived": true, "method_deprecated": true,
	"missing_argument": true, "missing_post_type": true, "missing_scope": true, "msg_blocks_too_long": true,
	"msg_too_long": true, "no_permission": true, "no_text": true, "not_allowed_token_type": true,
	"not_authed": true, "not_in_channel": true, "org_login_required": true, "posting_to_channel_denied": true,
	"restricted_action": true, "restricted_action_non_threadable_channel": true,
	"restricted_action_read_only_channel": true, "restricted_action_thread_locked": true,
	"restricted_action_thread_only_channel": true, "team_access_not_granted": true, "team_not_found": true,
	"messages_tab_disabled": true, "user_not_found": true,
	"token_expired": true, "token_revoked": true, "too_many_attachments": true, "two_factor_setup_required": true,
}

// gone lists the error codes of chat.update that mean the message can no
// longer be edited, so the update has to be sent as a new message.
var gone = map[string]bool{"message_not_found": true, "cant_update_message": true, "edit_window_closed": true}

// response is the part of a Web API response that Beacon reads.
type response struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	// User is the user that users.lookupByEmail found.
	User struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		RealName string `json:"real_name"`
		Deleted  bool   `json:"deleted"`
		IsBot    bool   `json:"is_bot"`
		Profile  struct {
			DisplayName string `json:"display_name"`
			RealName    string `json:"real_name"`
		} `json:"profile"`
	} `json:"user"`
}

// call invokes a Web API method with a JSON body. Its errors say whether a
// retry can help, in the pipeline's terms, and never contain the token.
func (c *Client) call(ctx context.Context, method string, body any) (response, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return response{}, notify.Permanent(fmt.Errorf("slack %s: encode request: %w", method, err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(encoded))
	if err != nil {
		return response{}, notify.Permanent(fmt.Errorf("slack %s: %w", method, err))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	return c.do(req, method)
}

// do sends a request for a Web API method with the bot token and reads the
// answer.
func (c *Client) do(req *http.Request, method string) (response, error) {
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())

	resp, err := c.http.Do(req)
	if err != nil {
		// The error names the method, never a header, and not the URL
		// either: a lookup carries an email address in it.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return response{}, fmt.Errorf("slack %s: %w", method, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do if closing a read body fails
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return response{}, fmt.Errorf("slack %s: read response: %w", method, err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		err := fmt.Errorf("slack %s: rate limited", method)
		// Retry-After is a number of seconds.
		if secs, convErr := strconv.Atoi(resp.Header.Get("Retry-After")); convErr == nil && secs > 0 {
			return response{}, notify.RetryAfter(err, time.Duration(secs)*time.Second)
		}
		return response{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return response{}, fmt.Errorf("slack %s: HTTP status %d", method, resp.StatusCode)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return response{}, fmt.Errorf("slack %s: the response is not JSON", method)
	}
	if !out.OK {
		apiErr := &APIError{Method: method, Code: out.Error}
		if permanent[out.Error] {
			return response{}, notify.Permanent(apiErr)
		}
		return response{}, apiErr
	}
	return out, nil
}

// postMessage sends a new message to a channel, given by ID or by name, and
// returns the ID of the channel and the timestamp that identifies the message.
func (c *Client) postMessage(ctx context.Context, channel string, m message) (channelID, ts string, err error) {
	out, err := c.call(ctx, "chat.postMessage", request{Channel: channel, message: m})
	if err != nil {
		return "", "", err
	}
	if out.Channel == "" || out.TS == "" {
		return "", "", errors.New("slack chat.postMessage: the response has no channel or ts")
	}
	return out.Channel, out.TS, nil
}

// updateMessage replaces the content of a message. The channel must be given
// by ID.
func (c *Client) updateMessage(ctx context.Context, channelID, ts string, m message) error {
	_, err := c.call(ctx, "chat.update", request{Channel: channelID, TS: ts, message: m})
	return err
}

// request is the body of chat.postMessage and chat.update.
type request struct {
	Channel string `json:"channel"`
	TS      string `json:"ts,omitempty"`
	message
}

// LookupByEmail returns the ID of the workspace member with the email
// address, through users.lookupByEmail, which needs the users:read.email
// scope. found is false when the workspace has no such member; Slack answers
// the same for a member who has been deactivated. A bot is nobody's account.
//
// The method takes its arguments in the query, not as JSON.
func (c *Client) LookupByEmail(ctx context.Context, email string) (userID string, found bool, err error) {
	const method = "users.lookupByEmail"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+method+"?"+url.Values{"email": {email}}.Encode(), nil)
	if err != nil {
		return "", false, fmt.Errorf("slack %s: the request could not be built", method)
	}
	out, err := c.do(req, method)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "users_not_found" {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if out.User.ID == "" || out.User.Deleted || out.User.IsBot {
		return "", false, nil
	}
	return out.User.ID, true, nil
}

// DisplayName returns what a member of the workspace is called there,
// through users.info, which needs the users:read scope: the display name
// they chose, or else their real name, or else their handle. Slack
// guarantees none of the three, so the result may be empty.
//
// Like users.lookupByEmail, the method takes its argument in the query.
func (c *Client) DisplayName(ctx context.Context, userID string) (string, error) {
	const method = "users.info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+method+"?"+url.Values{"user": {userID}}.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("slack %s: the request could not be built", method)
	}
	out, err := c.do(req, method)
	if err != nil {
		return "", err
	}
	for _, name := range []string{out.User.Profile.DisplayName, out.User.Profile.RealName, out.User.RealName, out.User.Name} {
		if name = strings.TrimSpace(name); name != "" {
			return name, nil
		}
	}
	return "", nil
}
