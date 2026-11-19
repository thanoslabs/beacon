package discordtest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// RouteRegisterCommand is the route that creates a global application
// command.
const RouteRegisterCommand = "POST /applications/{id}/commands"

// User is a person in the fake Discord.
type User struct {
	ID string
	// Username is the handle, GlobalName the display name the person chose,
	// and Nick their nickname in the server. Discord guarantees only the
	// first.
	Username   string
	GlobalName string
	Nick       string
}

// Answer is how Beacon answered an interaction.
type Answer struct {
	// Status is the HTTP status.
	Status int
	// Type is the type of the interaction response, such as 4 for a
	// message, and zero when the status is not 200.
	Type int
	// Content is the text of a message, and Ephemeral whether only the
	// person who caused the interaction sees it.
	Content   string
	Ephemeral bool
}

// newKey makes the key pair with which the fake Discord signs interactions.
func newKey(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return public, private
}

// PublicKey returns the public key of the application, as Discord shows it:
// 64 hexadecimal characters. It goes into the configuration of the
// integration.
func (s *Server) PublicKey() string { return hex.EncodeToString(s.public) }

// Sign returns the values of the X-Signature-Timestamp and
// X-Signature-Ed25519 headers for a body sent at the given time, computed
// the way Discord documents: the signature covers the timestamp followed by
// the body.
func (s *Server) Sign(at time.Time, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	return timestamp, hex.EncodeToString(ed25519.Sign(s.private, append([]byte(timestamp), body...)))
}

var (
	commandNameRE = regexp.MustCompile(`^[-_\p{Ll}\p{N}]{1,32}$`)
)

// registerCommand answers Create Global Application Command, checking the
// command against the limits of the documentation. A command of the same
// name is overwritten.
func (s *Server) registerCommand(w http.ResponseWriter, applicationID string, body map[string]any) {
	name, _ := body["name"].(string)
	description, _ := body["description"].(string)
	options, _ := body["options"].([]any)
	problem := ""
	switch {
	case !commandNameRE.MatchString(name):
		problem = "name: must be 1 to 32 lowercase letters, digits, hyphens and underscores"
	case length(description) < 1 || length(description) > 100:
		problem = "description: must be between 1 and 100 in length"
	case len(options) > 25:
		problem = "options: must be 25 or fewer in length"
	}
	if t, sent := body["type"]; sent && t != float64(1) {
		problem = "type: options can only be set for slash commands"
	}
	seenOptional := false
	for _, ro := range options {
		o, ok := ro.(map[string]any)
		if !ok {
			problem = "options: not an object"
			break
		}
		optionName, _ := o["name"].(string)
		optionDescription, _ := o["description"].(string)
		required, _ := o["required"].(bool)
		kind, _ := o["type"].(float64)
		switch {
		case !commandNameRE.MatchString(optionName):
			problem = "options.name: must be 1 to 32 lowercase letters, digits, hyphens and underscores"
		case length(optionDescription) < 1 || length(optionDescription) > 100:
			problem = "options.description: must be between 1 and 100 in length"
		case kind < 1 || kind > 11:
			problem = "options.type: not an option type"
		case required && seenOptional:
			problem = "options: required options must be listed before optional options"
		}
		seenOptional = seenOptional || !required
	}
	if problem != "" {
		refuse(w, http.StatusBadRequest, 50035, "Invalid Form Body: "+problem)
		return
	}
	status := http.StatusCreated
	for i, c := range s.commands[applicationID] {
		if c["name"] == name {
			s.commands[applicationID][i] = body
			status = http.StatusOK
		}
	}
	if status == http.StatusCreated {
		s.commands[applicationID] = append(s.commands[applicationID], body)
	}
	body["id"], body["application_id"] = s.next(), applicationID
	reply(w, status, body)
}

// Commands returns the global commands of an application.
func (s *Server) Commands(applicationID string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.commands[applicationID]...)
}

// userObject is a user as an interaction carries one.
func userObject(u User) map[string]any {
	out := map[string]any{"id": u.ID, "username": u.Username, "discriminator": "0", "global_name": nil}
	if u.GlobalName != "" {
		out["global_name"] = u.GlobalName
	}
	return out
}

// envelope returns the fields every interaction has. An interaction from a
// server carries the person as a member; one from a direct message carries
// them as a user.
func (s *Server) envelope(kind int, user User, direct bool) map[string]any {
	s.mu.Lock()
	id := s.next()
	s.mu.Unlock()
	in := map[string]any{
		"id": id, "application_id": "400000000000000001", "type": kind, "version": 1,
		"token": "interaction-token-" + id, "app_permissions": "19456", "locale": "en-GB",
		"entitlements": []any{}, "attachment_size_limit": 10485760,
	}
	if direct {
		in["user"], in["context"] = userObject(user), 1
	} else {
		member := map[string]any{"user": userObject(user), "nick": nil, "roles": []any{}, "permissions": "0"}
		if user.Nick != "" {
			member["nick"] = user.Nick
		}
		in["member"], in["guild_id"], in["context"] = member, "500000000000000001", 0
	}
	return in
}

// PingBody returns the body of the request with which Discord checks an
// Interactions Endpoint URL.
func (s *Server) PingBody() []byte {
	s.mu.Lock()
	id := s.next()
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"id": id, "application_id": "400000000000000001", "type": 1, "version": 1, "token": "ping-token"})
	return b
}

// PressBody returns the body Discord sends when user presses the button of
// m whose custom ID starts with prefix. direct says that the message is in
// a direct message with the bot. The test fails if m has no such button.
func (s *Server) PressBody(t testing.TB, m Message, prefix string, user User, direct bool) []byte {
	t.Helper()
	customID := ""
	for _, row := range m.Components {
		buttons, _ := row["components"].([]any)
		for _, rb := range buttons {
			if b, ok := rb.(map[string]any); ok {
				if id, _ := b["custom_id"].(string); strings.HasPrefix(id, prefix) {
					customID = id
				}
			}
		}
	}
	if customID == "" {
		t.Fatalf("the message has no button whose custom ID starts with %q: %s", prefix, m.JSON())
	}
	in := s.envelope(3, user, direct)
	in["channel_id"] = m.ChannelID
	in["message"] = map[string]any{"id": m.ID, "channel_id": m.ChannelID, "content": m.Content}
	in["data"] = map[string]any{"custom_id": customID, "component_type": 2}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode interaction: %v", err)
	}
	return b
}

// CommandBody returns the body Discord sends when user uses a slash
// command with text options.
func (s *Server) CommandBody(t testing.TB, name string, options map[string]string, user User) []byte {
	t.Helper()
	in := s.envelope(2, user, false)
	in["channel_id"] = Channel
	var opts []map[string]any
	for option, value := range options {
		opts = append(opts, map[string]any{"name": option, "type": 3, "value": value})
	}
	in["data"] = map[string]any{"id": "600000000000000001", "name": name, "type": 1, "options": opts}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode interaction: %v", err)
	}
	return b
}

// Deliver sends an interaction to endpoint, the interactions endpoint of
// Beacon, with the given signature headers, and returns the answer. It
// fails the test when an accepted interaction is answered in a way Discord
// would not take: late, without the JSON content type, or with a response
// that is not one.
func Deliver(t testing.TB, endpoint string, body []byte, timestamp, signature string) Answer {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Discord-Interactions/1.0 (+https://discord.com)")
	req.Header.Set("X-Signature-Timestamp", timestamp)
	req.Header.Set("X-Signature-Ed25519", signature)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send interaction: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper
	raw, _ := io.ReadAll(resp.Body)
	out := Answer{Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		return out
	}
	// Discord waits three seconds for the first answer.
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the interaction was answered after %v; Discord waits three seconds", took)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("the answer has the content type %q; Discord needs application/json", ct)
	}
	var answer struct {
		Type int `json:"type"`
		Data *struct {
			Content string `json:"content"`
			Flags   int    `json:"flags"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Errorf("the answer is not JSON: %s", raw)
		return out
	}
	out.Type = answer.Type
	switch answer.Type {
	case 1, 6:
	case 4:
		if answer.Data == nil || answer.Data.Content == "" || length(answer.Data.Content) > 2000 {
			t.Errorf("a message answer needs content of at most 2000 characters: %s", raw)
			return out
		}
		out.Content, out.Ephemeral = answer.Data.Content, answer.Data.Flags&(1<<6) != 0
	default:
		t.Errorf("the answer has type %d, which Beacon has no reason to send: %s", answer.Type, raw)
	}
	return out
}

// Send signs an interaction as Discord does and delivers it.
func (s *Server) Send(t testing.TB, endpoint string, body []byte) Answer {
	t.Helper()
	timestamp, signature := s.Sign(time.Now(), body)
	return Deliver(t, endpoint, body, timestamp, signature)
}
