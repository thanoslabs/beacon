// Package webhook is the outbound webhook channel: it posts notifications
// about alert groups, as JSON, to an address the configuration names.
// docs/outbound-webhook.md is the contract for the payload and the
// signature.
//
// The generic webhook that receives alerts is another thing, in
// internal/ingest.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/version"
)

// The headers Beacon sets on every request.
const (
	// HeaderTimestamp is when the request was signed, in Unix seconds.
	HeaderTimestamp = "X-Beacon-Timestamp"
	// HeaderSignature is the signature of the request: see Sign.
	HeaderSignature = "X-Beacon-Signature"
	// HeaderReason says why the notification was queued, as the payload
	// does.
	HeaderReason = "X-Beacon-Reason"
	// HeaderIdempotencyKey identifies the notification, as the payload's id
	// does. It is the same in every attempt.
	HeaderIdempotencyKey = "Idempotency-Key"
)

// signatureVersion is the scheme of the signature, which begins both the
// signed text and the header's value.
const signatureVersion = "v1"

// How much of a response is read before the connection is given up, and how
// long a receiver may ask Beacon to wait.
const (
	maxResponseBytes = 64 << 10
	maxRetryAfter    = time.Hour
)

// Sign returns the value of the signature header for a body sent at a time:
// "v1=" and the HMAC-SHA256, in hexadecimal, of "v1:<timestamp>:<body>"
// under the signing secret. The timestamp is part of what is signed, so a
// receiver that refuses old timestamps cannot be replayed an old request.
func Sign(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signatureVersion + ":" + timestamp + ":"))
	mac.Write(body)
	return signatureVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether a request was signed with the secret no longer
// ago than maxAge. It is what a receiver does, and what the tests do in a
// receiver's place.
func Verify(secret, timestamp, signature string, body []byte, now time.Time, maxAge time.Duration) bool {
	sent, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || secret == "" {
		return false
	}
	if age := now.Sub(time.Unix(sent, 0)); age > maxAge || age < -maxAge {
		return false
	}
	return hmac.Equal([]byte(Sign(secret, timestamp, body)), []byte(signature))
}

// Channel delivers notifications to one address. It implements
// notify.Channel.
//
// Every notification is a request of its own: there is nothing to edit at
// the other end. A request carries the notification's identifier, which is
// the same in every attempt, so that a receiver can make a repeat harmless.
type Channel struct {
	cfg config.WebhookIntegration
	// publicURL is the address of Beacon, for the links to alerts.
	publicURL string
	http      *http.Client
	now       func() time.Time
}

// NewChannel returns a channel that posts to the address of cfg. publicURL
// is the address at which people reach Beacon, or empty.
func NewChannel(cfg config.WebhookIntegration, publicURL string) *Channel {
	return &Channel{cfg: cfg, publicURL: strings.TrimRight(publicURL, "/"), http: newClient(), now: time.Now}
}

// SetClock replaces the clock, so tests can move time.
func (c *Channel) SetClock(now func() time.Time) { c.now = now }

// errBlockedAddress is why a connection was not made.
var errBlockedAddress = errors.New("the address is a link-local one, such as the metadata service of a cloud, which Beacon does not send to")

// newClient returns the HTTP client of a channel.
//
// The address comes from the configuration, which is as trusted as whoever
// may change it, and it is often an internal one: a ticket system, an
// automation. So private and loopback addresses are allowed. What is not:
//
//   - Redirects are not followed. The request goes to the address in the
//     file and nowhere else, with its signature and its headers.
//   - Link-local addresses are refused when the connection is made, after
//     the name was resolved: that is where clouds serve instance
//     credentials, and nothing that takes webhooks lives there.
//   - Every attempt is bounded in time, and only so much of an answer is
//     read.
func newClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
				return errBlockedAddress
			}
			return nil
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	transport.MaxIdleConnsPerHost = 2
	transport.ResponseHeaderTimeout = 10 * time.Second
	return &http.Client{
		Transport: transport,
		// The pipeline also bounds each attempt; this covers other callers.
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Send posts the payload for m. It returns no reference: nothing at the
// other end is updated later.
//
// Its errors say whether a retry can help, in the pipeline's terms. They
// name the status of the answer and never the address, a header or
// anything the receiver sent back: an address can hold a token, and an
// answer can repeat one.
func (c *Channel) Send(ctx context.Context, m notify.Message) (string, error) {
	now := c.now()
	body, err := json.Marshal(build(m, now, c.alertURL, c.alertPage))
	if err != nil {
		return "", notify.Permanent(fmt.Errorf("webhook: encode the payload: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return "", notify.Permanent(errors.New("webhook: the address in the configuration is not one a request can be made to"))
	}
	// The configured headers first, so that none of them can stand in for
	// one of Beacon's.
	for name, value := range c.cfg.Headers {
		req.Header.Set(name, value.Reveal())
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Beacon/"+version.Version)
	req.Header.Set(HeaderReason, string(m.Notification.Reason))
	req.Header.Set(HeaderIdempotencyKey, m.Notification.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderSignature, Sign(c.cfg.SigningSecret.Reveal(), timestamp, body))

	resp, err := c.http.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		if errors.Is(err, errBlockedAddress) {
			return "", notify.Permanent(fmt.Errorf("webhook: %w", errBlockedAddress))
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Err != nil {
			// Without the address.
			err = opErr.Err
		}
		return "", fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do if closing a read body fails
	// What the receiver says is not kept. A little of it is read so that the
	// connection can be used again.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return "", nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return "", notify.Permanent(fmt.Errorf("webhook: the receiver answered HTTP status %d, a redirect, which Beacon does not follow; put the final address in the configuration", resp.StatusCode))
	case resp.StatusCode == http.StatusGone:
		return "", notify.Permanent(fmt.Errorf("webhook: the receiver answered HTTP status %d: it is gone for good", resp.StatusCode))
	}
	failure := fmt.Errorf("webhook: the receiver answered HTTP status %d", resp.StatusCode)
	// A receiver that says how long to wait is not asked sooner.
	if secs, convErr := strconv.Atoi(resp.Header.Get("Retry-After")); convErr == nil && secs > 0 {
		return "", notify.RetryAfter(failure, min(time.Duration(secs)*time.Second, maxRetryAfter))
	}
	return "", failure
}

// alertURL returns the address of an alert in Beacon, or nothing when
// Beacon does not know its own address.
func (c *Channel) alertURL(id string) string {
	if c.publicURL == "" || id == "" {
		return ""
	}
	return c.publicURL + "/api/v1/alerts/" + url.PathEscape(id)
}

// alertPage returns the address of an alert's page in the web interface, or
// nothing when Beacon does not know its own address.
func (c *Channel) alertPage(id string) string {
	if c.publicURL == "" || id == "" {
		return ""
	}
	return c.publicURL + "/alerts/" + url.PathEscape(id)
}
