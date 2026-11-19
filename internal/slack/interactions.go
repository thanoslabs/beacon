package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
)

// InteractionsPath is the route that Slack sends button presses to: the
// Request URL of the Slack app's interactivity settings. {name} is the name
// of the Slack integration.
const InteractionsPath = "/api/v1/integrations/{name}/interactions"

// Provider is the name under which Slack identities are recorded, as in
// user_identities.provider.
const Provider = "slack"

// maxSignatureAge is how far a request's timestamp may be from the current
// time. Slack recommends five minutes, to stop a captured request being
// replayed later.
const maxSignatureAge = 5 * time.Minute

// Alerts is what the handler needs from the alert store.
type Alerts interface {
	ApplyToGroup(ctx context.Context, groupID string, action alert.Action, actor alert.Actor, opts alert.Options) ([]alert.Alert, error)
}

// Interactions handles the button presses that Slack sends. Every request is
// verified with the signing secret of the integration named in its path.
type Interactions struct {
	logger *slog.Logger
	alerts Alerts
	// secrets is replaced as a whole when the configuration is reloaded.
	secrets atomic.Pointer[map[string]config.Secret]
	// identities tells which Beacon user a Slack account belongs to. It
	// holds nil until SetIdentities is called, and then nobody is known.
	identities atomic.Pointer[Identities]
	now        func() time.Time
	http       *http.Client
}

// NewInteractions returns a handler. secrets maps the name of each Slack
// integration to its signing secret.
func NewInteractions(logger *slog.Logger, alerts Alerts, secrets map[string]config.Secret) *Interactions {
	h := &Interactions{
		logger: logger, alerts: alerts, now: time.Now,
		// Slack waits three seconds for the answer to a button press, so the
		// reply to the person who pressed it must not take long.
		http: &http.Client{Timeout: 2 * time.Second},
	}
	h.SetSecrets(secrets)
	return h
}

// SetSecrets replaces the signing secrets, by integration name. It is safe
// while requests are being handled.
func (h *Interactions) SetSecrets(secrets map[string]config.Secret) {
	h.secrets.Store(&secrets)
}

// Identities tells which Beacon user a chat account belongs to.
type Identities interface {
	// UserFor returns the user to whom the account externalID of the
	// integration with this name belongs, and what to call them. found is
	// false when the account is linked to nobody.
	UserFor(ctx context.Context, integration, externalID string) (userID, name string, found bool, err error)
}

// SetIdentities makes the handler attribute what people do in Slack to the
// Beacon users their accounts are linked to. Call it before the handler is
// used.
func (h *Interactions) SetIdentities(i Identities) { h.identities.Store(&i) }

// actor returns who pressed a button: the Beacon user that the Slack
// account is linked to, or else the Slack account alone. An account that is
// linked to nobody may still act. Slack decides who can see a message and
// press its buttons; Beacon records who it was.
func (h *Interactions) actor(ctx context.Context, integration, slackUser, slackName string) alert.Actor {
	chat := alert.ChatUser(Provider, slackUser, slackName)
	identities := h.identities.Load()
	if identities == nil || *identities == nil {
		return chat
	}
	userID, name, found, err := (*identities).UserFor(ctx, integration, slackUser)
	switch {
	case err != nil:
		// The press is answered within three seconds whatever happens to
		// the lookup; the event keeps the Slack account.
		h.logger.ErrorContext(ctx, "look up the user of a slack account", slog.String("slack_user", slackUser), slog.Any("error", err))
		return chat
	case !found:
		h.logger.InfoContext(ctx, "slack action by an account that is not linked to a Beacon user; the timeline records the Slack account",
			slog.String("integration", integration), slog.String("slack_user", slackUser))
		return chat
	}
	return alert.LinkedChatUser(userID, Provider, slackUser, name)
}

// SetClock replaces the clock, so tests can move time.
func (h *Interactions) SetClock(now func() time.Time) { h.now = now }

// Register attaches the endpoint to mux.
func (h *Interactions) Register(mux *http.ServeMux) {
	mux.Handle("POST "+InteractionsPath, h)
}

// interaction is the part of a block_actions payload that Beacon reads.
type interaction struct {
	Type string `json:"type"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"user"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (h *Interactions) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "could not read the body", http.StatusBadRequest)
		return
	}
	// The signature covers the raw body, so it is checked before anything in
	// the body is parsed. An unknown integration has no secret and fails too.
	secret := (*h.secrets.Load())[r.PathValue("name")]
	if !verify(secret, r.Header.Get("X-Slack-Request-Timestamp"), r.Header.Get("X-Slack-Signature"), body, h.now()) {
		h.logger.WarnContext(ctx, "slack request refused: the signature is missing, wrong or too old")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	form, err := url.ParseQuery(string(body))
	var in interaction
	if err != nil || json.Unmarshal([]byte(form.Get("payload")), &in) != nil {
		http.Error(w, "the payload is not a Slack interaction", http.StatusBadRequest)
		return
	}
	// Slack needs a 200 for every interaction, including kinds Beacon does
	// not use.
	if in.Type != "block_actions" {
		return
	}
	for _, pressed := range in.Actions {
		for _, b := range buttons {
			if pressed.ActionID == b.id {
				h.apply(ctx, w, r.PathValue("name"), in, b.action, pressed.Value)
				return
			}
		}
	}
}

// apply takes the action on the group and, when it could not be taken, tells
// the person who pressed the button why.
func (h *Interactions) apply(ctx context.Context, w http.ResponseWriter, integration string, in interaction, action alert.Action, groupID string) {
	name := in.User.Username
	if name == "" {
		name = in.User.Name
	}
	log := h.logger.With(slog.String("action", string(action)), slog.String("group_id", groupID), slog.String("slack_user", in.User.ID))

	err := alert.ErrNotFound
	if uuidRE.MatchString(groupID) && in.User.ID != "" {
		var opts alert.Options
		if action == alert.ActionSnooze {
			opts.SnoozeUntil = h.now().Add(SnoozeDuration)
		}
		_, err = h.alerts.ApplyToGroup(ctx, groupID, action, h.actor(ctx, integration, in.User.ID, name), opts)
	}

	var refused *alert.TransitionError
	switch {
	case err == nil:
		// The message is updated through the notification pipeline.
		log.InfoContext(ctx, "slack action applied")
	case errors.As(err, &refused):
		log.InfoContext(ctx, "slack action refused", slog.String("status", string(refused.From)))
		h.respond(ctx, in.ResponseURL, refusal(refused))
	case errors.Is(err, alert.ErrNotFound):
		log.WarnContext(ctx, "slack action for an alert that does not exist")
		h.respond(ctx, in.ResponseURL, "Beacon does not know this alert any more.")
	default:
		log.ErrorContext(ctx, "apply slack action", slog.Any("error", err))
		h.respond(ctx, in.ResponseURL, "Beacon could not apply that just now. Please try again.")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// refusal explains, to the person who pressed a button, why nothing happened.
func refusal(e *alert.TransitionError) string {
	done := map[alert.Action]alert.Status{
		alert.ActionAcknowledge: alert.StatusAcknowledged,
		alert.ActionSnooze:      alert.StatusSnoozed,
		alert.ActionResolve:     alert.StatusResolved,
	}
	if e.From == done[e.Action] || e.From == alert.StatusResolved {
		return "Nothing changed: this alert is already " + string(e.From) + "."
	}
	return "Nothing changed: you cannot " + string(e.Action) + " an alert that is " + string(e.From) + "."
}

// respond sends a message that only the person who pressed the button sees,
// through the response URL of the interaction. The URL comes from a payload
// whose signature was verified, so it is Slack's.
func (h *Interactions) respond(ctx context.Context, responseURL, text string) {
	if responseURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{"response_type": "ephemeral", "replace_original": false, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(body))
	if err != nil {
		h.logger.WarnContext(ctx, "slack response URL is not usable")
		return
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := h.http.Do(req)
	if err != nil {
		// The URL is a credential for the interaction, so it is not logged.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		h.logger.WarnContext(ctx, "answer slack interaction", slog.Any("error", err))
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.logger.WarnContext(ctx, "answer slack interaction", slog.Int("status", resp.StatusCode))
	}
}

// verify reports whether a request was signed by Slack with this signing
// secret: the signature must be the HMAC-SHA256 of "v0:<timestamp>:<body>",
// hex encoded after "v0=", and the timestamp must be recent.
func verify(secret config.Secret, timestamp, signature string, body []byte, now time.Time) bool {
	if !secret.IsSet() {
		return false
	}
	sent, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if age := now.Sub(time.Unix(sent, 0)); age > maxSignatureAge || age < -maxSignatureAge {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret.Reveal()))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}
