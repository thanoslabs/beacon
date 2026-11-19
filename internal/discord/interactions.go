package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/notify"
)

// InteractionsPath is the route that Discord sends interactions to: the
// Interactions Endpoint URL of the application. {name} is the name of the
// Discord integration. It is the same route as Slack's; the name decides
// which service a request is checked as.
const InteractionsPath = "/api/v1/integrations/{name}/interactions"

// LinkCommand is the name of the slash command with which a person gives
// Beacon the code that links their Discord account, and linkOption the name
// of its one option.
const (
	LinkCommand = "beacon-link"
	linkOption  = "code"
)

// SnoozeDuration is how long the snooze button snoozes for.
const SnoozeDuration = time.Hour

// answerWithin bounds the work behind an interaction. Discord waits three
// seconds for the answer and then tells the person that the interaction
// failed.
const answerWithin = 2500 * time.Millisecond

// The types of interaction and of answer that Beacon uses, by Discord's
// numbers.
const (
	interactionPing      = 1
	interactionCommand   = 2
	interactionComponent = 3

	answerPong           = 1
	answerMessage        = 4
	answerDeferredUpdate = 6

	// flagEphemeral makes an answer visible to the person who caused it
	// only.
	flagEphemeral = 1 << 6
)

// Alerts is what the handler needs from the alert store.
type Alerts interface {
	ApplyToGroup(ctx context.Context, groupID string, action alert.Action, actor alert.Actor, opts alert.Options) ([]alert.Alert, error)
}

// Identities tells which Beacon user a Discord account belongs to, and
// links an account to the user who asked for a code.
type Identities interface {
	// UserFor returns the user to whom the account externalID of the
	// integration with this name belongs, and what to call them. found is
	// false when the account is linked to nobody.
	UserFor(ctx context.Context, integration, externalID string) (userID, name string, found bool, err error)
	// CompleteLink makes externalID the account of the user who was given
	// code for the integration with this name, and returns what to call
	// them. Its errors are identity.ErrBadCode and identity.ErrTaken, or
	// something else when it could not be done.
	CompleteLink(ctx context.Context, integration, externalID, code string) (name string, err error)
}

// Interactions handles what Discord sends when a person presses a button or
// uses the link command. Every request is verified with the public key of
// the integration named in its path.
type Interactions struct {
	logger *slog.Logger
	alerts Alerts
	// keys is replaced as a whole when the configuration is reloaded.
	keys atomic.Pointer[map[string]ed25519.PublicKey]
	// identities holds nil until SetIdentities is called, and then nobody is
	// known and nothing can be linked.
	identities atomic.Pointer[Identities]
	signInURL  atomic.Pointer[string]
	now        func() time.Time
}

// NewInteractions returns a handler that knows no integration yet.
func NewInteractions(logger *slog.Logger, alerts Alerts) *Interactions {
	h := &Interactions{logger: logger, alerts: alerts, now: time.Now}
	h.SetPublicKeys(nil)
	h.SetSignInURL("")
	return h
}

// SetPublicKeys replaces the public keys of the Discord applications, by
// integration name, as the 64 hexadecimal characters Discord shows. A key
// that is not one is left out, and requests for its integration are
// refused. It is safe while requests are being handled.
func (h *Interactions) SetPublicKeys(keys map[string]string) {
	decoded := make(map[string]ed25519.PublicKey, len(keys))
	for name, key := range keys {
		if raw, err := hex.DecodeString(key); err == nil && len(raw) == ed25519.PublicKeySize {
			decoded[name] = ed25519.PublicKey(raw)
		}
	}
	h.keys.Store(&decoded)
}

// SetIdentities makes the handler know which Discord accounts belong to
// Beacon users, and lets people link theirs. Call it before the handler is
// used.
func (h *Interactions) SetIdentities(i Identities) { h.identities.Store(&i) }

// SetSignInURL tells the handler where people sign in to Beacon, which it
// passes on to someone who has to. Empty means that nobody can.
func (h *Interactions) SetSignInURL(url string) { h.signInURL.Store(&url) }

// SetClock replaces the clock, so tests can move time.
func (h *Interactions) SetClock(now func() time.Time) { h.now = now }

// interaction is the part of an interaction that Beacon reads.
type interaction struct {
	Type int `json:"type"`
	// Member is set when the interaction comes from a server, and User when
	// it comes from a direct message.
	Member *struct {
		Nick string      `json:"nick"`
		User discordUser `json:"user"`
	} `json:"member"`
	User *discordUser `json:"user"`
	Data struct {
		// CustomID is the identifier of the button that was pressed.
		CustomID string `json:"custom_id"`
		// Name is the name of the command that was used.
		Name    string `json:"name"`
		Options []struct {
			Name  string `json:"name"`
			Value any    `json:"value"`
		} `json:"options"`
	} `json:"data"`
}

type discordUser struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
}

// who returns the account behind an interaction, and what Discord calls the
// person: their nickname in the server, or else their display name, or else
// their handle.
func (in interaction) who() (id, name string) {
	var u discordUser
	nick := ""
	switch {
	case in.Member != nil:
		u, nick = in.Member.User, in.Member.Nick
	case in.User != nil:
		u = *in.User
	}
	for _, candidate := range []string{nick, u.GlobalName, u.Username, u.ID} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return u.ID, candidate
		}
	}
	return u.ID, ""
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
	// the body is parsed. An unknown integration has no key and fails too.
	// Discord sends requests with a wrong signature on purpose, to check
	// that they are refused with 401.
	name := r.PathValue("name")
	if !verify((*h.keys.Load())[name], r.Header.Get("X-Signature-Timestamp"), r.Header.Get("X-Signature-Ed25519"), body) {
		h.logger.WarnContext(ctx, "discord request refused: the signature is missing or wrong")
		http.Error(w, "invalid request signature", http.StatusUnauthorized)
		return
	}

	var in interaction
	if json.Unmarshal(body, &in) != nil {
		http.Error(w, "the payload is not a Discord interaction", http.StatusBadRequest)
		return
	}
	// Whatever follows must be answered within Discord's three seconds.
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	switch in.Type {
	case interactionPing:
		// Discord's check that the endpoint is Beacon's.
		h.answer(ctx, w, map[string]any{"type": answerPong})
	case interactionComponent:
		id, groupID, _ := strings.Cut(in.Data.CustomID, ":")
		for _, b := range buttons {
			if id == b.id {
				h.apply(ctx, w, name, in, b.action, groupID)
				return
			}
		}
		h.say(ctx, w, "Beacon does not know this button.")
	case interactionCommand:
		if in.Data.Name != LinkCommand {
			h.say(ctx, w, "Beacon does not know this command.")
			return
		}
		h.link(ctx, w, name, in)
	default:
		http.Error(w, "this kind of interaction is not used by Beacon", http.StatusBadRequest)
	}
}

// answer writes the answer to an interaction.
func (h *Interactions) answer(ctx context.Context, w http.ResponseWriter, body map[string]any) {
	// Discord refuses an answer without the content type.
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.DebugContext(ctx, "write response", slog.Any("error", err))
	}
}

// say answers with a message that only the person who caused the
// interaction sees. The text is Beacon's own, and mentions nobody.
func (h *Interactions) say(ctx context.Context, w http.ResponseWriter, text string) {
	h.answer(ctx, w, map[string]any{"type": answerMessage, "data": map[string]any{
		"content": text, "flags": flagEphemeral, "allowed_mentions": allowedMentions{Parse: []string{}},
	}})
}

// who is the person behind a button press.
type who struct {
	actor alert.Actor
	// linked says that the Discord account belongs to a Beacon user.
	linked bool
	// unknown says that it could not be found out whether it does.
	unknown bool
}

// identify says who pressed a button: the Beacon user that the Discord
// account is linked to, or else the Discord account alone.
func (h *Interactions) identify(ctx context.Context, integration, discordID, discordName string) who {
	chat := alert.ChatUser(Provider, discordID, discordName)
	identities := h.identities.Load()
	if identities == nil || *identities == nil {
		// Nobody is known to be anybody.
		return who{actor: chat}
	}
	userID, name, found, err := (*identities).UserFor(ctx, integration, discordID)
	switch {
	case err != nil:
		h.logger.ErrorContext(ctx, "look up the user of a discord account", slog.String("discord_user", discordID), slog.Any("error", err))
		return who{actor: chat, unknown: true}
	case !found:
		return who{actor: chat}
	}
	return who{actor: alert.LinkedChatUser(userID, Provider, discordID, name), linked: true}
}

// notLinked tells someone whose Discord account belongs to no Beacon user
// why their press changed nothing, and what to do about it.
func (h *Interactions) notLinked(action alert.Action) string {
	text := "Nothing changed: only Beacon users can " + string(action) + " an alert, and your Discord account is not connected to one. "
	if url := *h.signInURL.Load(); url != "" {
		text += "Sign in to Beacon at " + url + ", ask it for a link code for Discord, and give the code to Beacon here with /" + LinkCommand + ". "
	} else {
		text += "Sign in to Beacon, ask it for a link code for Discord, and give the code to Beacon here with /" + LinkCommand + "; ask whoever runs Beacon for its address. "
	}
	return text + "You can still resolve the alert from here."
}

// apply takes the action on the group and answers Discord: with nothing to
// show when it was taken, because the message is edited through the
// notification pipeline, and otherwise with a note for the person who
// pressed the button.
func (h *Interactions) apply(ctx context.Context, w http.ResponseWriter, integration string, in interaction, action alert.Action, groupID string) {
	discordID, name := in.who()
	log := h.logger.With(slog.String("action", string(action)), slog.String("group_id", groupID), slog.String("discord_user", discordID))

	// Acknowledging and snoozing say "I have this", which only means
	// something from a person Beacon knows. Resolving states a fact about
	// the alert, and anyone who can see the message may state it.
	by := h.identify(ctx, integration, discordID, name)
	if !by.linked && action != alert.ActionResolve {
		if by.unknown {
			log.WarnContext(ctx, "discord action refused: it could not be checked whether the account is linked to a Beacon user")
			h.say(ctx, w, "Nothing changed: Beacon could not check who you are just now. Please try again.")
			return
		}
		log.InfoContext(ctx, "discord action refused: the account is not linked to a Beacon user", slog.String("integration", integration))
		h.say(ctx, w, h.notLinked(action))
		return
	}

	err := alert.ErrNotFound
	if uuidRE.MatchString(groupID) && snowflakeRE.MatchString(discordID) {
		var opts alert.Options
		if action == alert.ActionSnooze {
			opts.SnoozeUntil = h.now().Add(SnoozeDuration)
		}
		actor := by.actor
		if !by.linked {
			log.InfoContext(ctx, "discord action by an account that is not linked to a Beacon user; the timeline records the Discord account",
				slog.String("integration", integration))
			// Named as the server names them, with the note that they are
			// not connected. The name came with the press.
			actor = alert.ChatUser(Provider, discordID, notify.NotConnected(name, "Discord"))
		}
		_, err = h.alerts.ApplyToGroup(ctx, groupID, action, actor, opts)
	}

	var refused *alert.TransitionError
	switch {
	case err == nil:
		log.InfoContext(ctx, "discord action applied")
		// Nothing to say: the message is edited through the pipeline.
		h.answer(ctx, w, map[string]any{"type": answerDeferredUpdate})
	case errors.As(err, &refused):
		log.InfoContext(ctx, "discord action refused", slog.String("status", string(refused.From)))
		h.say(ctx, w, refusal(refused))
	case errors.Is(err, alert.ErrNotFound):
		log.WarnContext(ctx, "discord action for an alert that does not exist")
		h.say(ctx, w, "Beacon does not know this alert any more.")
	default:
		log.ErrorContext(ctx, "apply discord action", slog.Any("error", err))
		h.say(ctx, w, "Beacon could not apply that just now. Please try again.")
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

// link redeems a link code: the Discord account that used the command
// becomes the account of the Beacon user who asked for the code. That the
// request comes from Discord, and so from that account, was verified by its
// signature.
func (h *Interactions) link(ctx context.Context, w http.ResponseWriter, integration string, in interaction) {
	discordID, _ := in.who()
	code := ""
	for _, o := range in.Data.Options {
		if o.Name == linkOption {
			code, _ = o.Value.(string)
		}
	}
	log := h.logger.With(slog.String("integration", integration), slog.String("discord_user", discordID))
	identities := h.identities.Load()
	if identities == nil || *identities == nil || !snowflakeRE.MatchString(discordID) || strings.TrimSpace(code) == "" {
		h.say(ctx, w, "Nothing changed: "+identity.ErrBadCode.Error()+".")
		return
	}
	name, err := (*identities).CompleteLink(ctx, integration, discordID, code)
	switch {
	case err == nil:
		log.InfoContext(ctx, "discord account linked to a user by the user themselves")
		// The name is the user's own, shown to nobody else; it is escaped
		// all the same.
		h.say(ctx, w, "Connected: this Discord account now belongs to "+escape(clipPlain(oneLine(name), maxActor))+" in Beacon. You can acknowledge and snooze alerts from Discord, and Beacon can page you here.")
	case errors.Is(err, identity.ErrBadCode):
		// The code is not logged: it may be a valid one typed into the
		// wrong server.
		log.InfoContext(ctx, "discord link refused: the code is not valid")
		h.say(ctx, w, "Nothing changed: "+identity.ErrBadCode.Error()+".")
	case errors.Is(err, identity.ErrTaken):
		log.WarnContext(ctx, "discord link refused: the account is linked to another user")
		h.say(ctx, w, "Nothing changed: this Discord account is connected to another Beacon user already. That person must disconnect it first. The code is used up; ask Beacon for a new one afterwards.")
	default:
		log.ErrorContext(ctx, "link discord account", slog.Any("error", err))
		h.say(ctx, w, "Beacon could not connect your account just now. Please try again; the code may be used up, so ask Beacon for a new one if it is refused.")
	}
}

// verify reports whether a request was signed by Discord for the
// application with this public key: the signature must be the Ed25519
// signature of the timestamp followed by the body, hex encoded.
func verify(key ed25519.PublicKey, timestamp, signature string, body []byte) bool {
	if len(key) != ed25519.PublicKeySize || timestamp == "" {
		return false
	}
	sig, err := hex.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	signed := make([]byte, 0, len(timestamp)+len(body))
	signed = append(append(signed, timestamp...), body...)
	return ed25519.Verify(key, signed, sig)
}
