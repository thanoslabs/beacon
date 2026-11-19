// Package discordtest is a fake Discord for tests. It implements the parts
// of the Discord HTTP API that Beacon calls, checks requests the way
// Discord's developer documentation describes them (API version 10, as of
// November 2026), and remembers the messages it was sent.
//
// It is not Discord: it proves that Beacon sends what the documentation
// describes, not that Discord accepts it.
package discordtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"
)

// Message is a message as the fake Discord holds it.
type Message struct {
	ChannelID string
	ID        string
	Content   string
	Embeds    []map[string]any
	// Components are the action rows of the message.
	Components []map[string]any
	// Edits counts how many times the message was edited.
	Edits int
}

// JSON returns the whole message encoded, for searching its content.
func (m Message) JSON() string {
	b, _ := json.Marshal(map[string]any{"content": m.Content, "embeds": m.Embeds, "components": m.Components})
	return string(b)
}

// Call is one request to the API.
type Call struct {
	// Route is the method and the path with its IDs replaced, such as
	// "POST /channels/{id}/messages".
	Route string
	Path  string
	Body  map[string]any
}

// The routes of the fake, as Call.Route and Fail name them.
const (
	RouteCreateMessage = "POST /channels/{id}/messages"
	RouteEditMessage   = "PATCH /channels/{id}/messages/{id}"
	RouteCreateDM      = "POST /users/@me/channels"
)

// Failure makes the fake Discord fail a call.
type Failure struct {
	// Status is the HTTP status to answer with, such as 429 or 503.
	Status int
	// Code is the error code of the JSON body, such as 50001.
	Code int
	// RetryAfter is the number of seconds a 429 asks to wait, as Discord
	// sends it: in the Retry-After header and in the body. It may have
	// decimals.
	RetryAfter string
	// Global marks a 429 as the global rate limit.
	Global bool
}

// Server is a fake Discord.
type Server struct {
	// URL is the base URL to use as the Discord API URL.
	URL string

	token string

	mu       sync.Mutex
	channels map[string]bool
	// users maps the ID of each user to whether they accept direct messages.
	users    map[string]bool
	dms      map[string]string
	messages map[string]*Message
	order    []string
	calls    []Call
	failures map[string][]Failure
	// exhausted makes the next successful answer of a route say that its
	// rate limit is used up, for so many seconds.
	exhausted map[string]string
	seq       int64
}

// Channel is the ID of the one channel a new fake Discord has.
const Channel = "100000000000000001"

// New starts a fake Discord that accepts the given bot token and has one
// channel, with the ID Channel. It stops when the test ends.
func New(t testing.TB, token string) *Server {
	t.Helper()
	s := &Server{
		token:     token,
		channels:  map[string]bool{Channel: true},
		users:     map[string]bool{},
		dms:       map[string]string{},
		messages:  map[string]*Message{},
		failures:  map[string][]Failure{},
		exhausted: map[string]string{},
		seq:       200000000000000000,
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// AddChannel gives the server another channel that the bot can post to.
func (s *Server) AddChannel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[id] = true
}

// AddUser gives Discord a user with an ID who shares a server with the bot
// and accepts direct messages from it.
func (s *Server) AddUser(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[id] = true
}

// BlockDMs makes a user refuse direct messages from the bot, as their
// privacy settings can.
func (s *Server) BlockDMs(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[id] = false
}

// DM returns the ID of the direct message channel between the bot and a
// user, or "" when none was opened.
func (s *Server) DM(userID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dms[userID]
}

// Fail makes the next calls to a route fail, one failure per call, in order.
func (s *Server) Fail(route string, failures ...Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[route] = append(s.failures[route], failures...)
}

// Exhaust makes the next successful answer of a route carry the headers
// that say its rate limit is used up and resets after the given number of
// seconds.
func (s *Server) Exhaust(route, resetAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exhausted[route] = resetAfter
}

// Messages returns the messages in the order they were posted.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, 0, len(s.order))
	for _, key := range s.order {
		if m, ok := s.messages[key]; ok {
			out = append(out, *m)
		}
	}
	return out
}

// Calls returns the requests made to a route, including the ones that
// failed.
func (s *Server) Calls(route string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Route == route {
			out = append(out, c)
		}
	}
	return out
}

// Delete removes a message, as a person deleting it in Discord would.
func (s *Server) Delete(channelID, messageID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.messages, channelID+":"+messageID)
}

var (
	snowflakeRE = regexp.MustCompile(`^[0-9]{15,22}$`)
	idRE        = regexp.MustCompile(`/[0-9]{15,22}`)
)

// next returns a new ID. The caller holds the lock.
func (s *Server) next() string {
	s.seq++
	return strconv.FormatInt(s.seq, 10)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	route := r.Method + " " + idRE.ReplaceAllString(r.URL.Path, "/{id}")

	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Route: route, Path: r.URL.Path, Body: body})

	if queue := s.failures[route]; len(queue) > 0 {
		f := queue[0]
		s.failures[route] = queue[1:]
		fail(w, f)
		return
	}
	if reset, ok := s.exhausted[route]; ok {
		delete(s.exhausted, route)
		w.Header().Set("X-RateLimit-Limit", "5")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", reset)
		w.Header().Set("X-RateLimit-Bucket", "fake")
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Header.Get("Authorization") != "Bot "+s.token:
		refuse(w, http.StatusUnauthorized, 0, "401: Unauthorized")
	case !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot ("):
		// Discord may block clients without a valid user agent.
		refuse(w, http.StatusForbidden, 0, "a DiscordBot user agent is required")
	case r.Method != http.MethodGet && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
		refuse(w, http.StatusBadRequest, 50035, "Invalid form body: the content type must be application/json")
	case r.Method != http.MethodGet && body == nil:
		refuse(w, http.StatusBadRequest, 50109, "The request body contains invalid JSON.")
	case route == RouteCreateDM:
		s.createDM(w, body)
	case route == RouteCreateMessage:
		s.createMessage(w, parts[1], body)
	case route == RouteEditMessage:
		s.editMessage(w, parts[1], parts[3], body)
	default:
		refuse(w, http.StatusNotFound, 0, "404: Not Found")
	}
}

func fail(w http.ResponseWriter, f Failure) {
	if f.Status == http.StatusTooManyRequests {
		body := map[string]any{"message": "You are being rate limited.", "global": f.Global}
		if f.RetryAfter != "" {
			w.Header().Set("Retry-After", f.RetryAfter)
			seconds, _ := strconv.ParseFloat(f.RetryAfter, 64)
			body["retry_after"] = seconds
		}
		if f.Global {
			w.Header().Set("X-RateLimit-Global", "true")
			w.Header().Set("X-RateLimit-Scope", "global")
		} else {
			w.Header().Set("X-RateLimit-Scope", "user")
		}
		reply(w, http.StatusTooManyRequests, body)
		return
	}
	refuse(w, f.Status, f.Code, "failure asked for by the test")
}

func refuse(w http.ResponseWriter, status, code int, message string) {
	reply(w, status, map[string]any{"code": code, "message": message})
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) createDM(w http.ResponseWriter, body map[string]any) {
	id, _ := body["recipient_id"].(string)
	if !snowflakeRE.MatchString(id) {
		refuse(w, http.StatusBadRequest, 50035, "Invalid form body: recipient_id is not a snowflake")
		return
	}
	if _, known := s.users[id]; !known {
		refuse(w, http.StatusBadRequest, 50033, "Invalid Recipient(s)")
		return
	}
	// The channel that exists is returned again.
	if s.dms[id] == "" {
		s.dms[id] = s.next()
	}
	reply(w, http.StatusOK, map[string]any{"id": s.dms[id], "type": 1, "recipients": []map[string]any{{"id": id}}})
}

// postable reports whether the bot can post to a channel, and answers when
// it cannot. A direct message channel is one, unless its user refuses
// direct messages.
func (s *Server) postable(w http.ResponseWriter, channelID string) bool {
	for user, dm := range s.dms {
		if dm == channelID {
			if !s.users[user] {
				refuse(w, http.StatusForbidden, 50007, "Cannot send messages to this user")
				return false
			}
			return true
		}
	}
	if !s.channels[channelID] {
		refuse(w, http.StatusNotFound, 10003, "Unknown Channel")
		return false
	}
	return true
}

func (s *Server) createMessage(w http.ResponseWriter, channelID string, body map[string]any) {
	if !s.postable(w, channelID) {
		return
	}
	m, problem := content(body)
	if problem != "" {
		refuse(w, http.StatusBadRequest, 50035, "Invalid Form Body: "+problem)
		return
	}
	if m.Content == "" && len(m.Embeds) == 0 && len(m.Components) == 0 {
		refuse(w, http.StatusBadRequest, 50006, "Cannot send an empty message")
		return
	}
	m.ChannelID, m.ID = channelID, s.next()
	key := channelID + ":" + m.ID
	s.messages[key] = &m
	s.order = append(s.order, key)
	reply(w, http.StatusOK, map[string]any{"id": m.ID, "channel_id": channelID, "content": m.Content})
}

func (s *Server) editMessage(w http.ResponseWriter, channelID, messageID string, body map[string]any) {
	if !s.postable(w, channelID) {
		return
	}
	old, ok := s.messages[channelID+":"+messageID]
	if !ok {
		refuse(w, http.StatusNotFound, 10008, "Unknown Message")
		return
	}
	m, problem := content(body)
	if problem != "" {
		refuse(w, http.StatusBadRequest, 50035, "Invalid Form Body: "+problem)
		return
	}
	// A field that an edit leaves out keeps its value.
	if _, sent := body["content"]; !sent {
		m.Content = old.Content
	}
	if _, sent := body["embeds"]; !sent {
		m.Embeds = old.Embeds
	}
	if _, sent := body["components"]; !sent {
		m.Components = old.Components
	}
	m.ChannelID, m.ID, m.Edits = channelID, messageID, old.Edits+1
	s.messages[channelID+":"+messageID] = &m
	reply(w, http.StatusOK, map[string]any{"id": messageID, "channel_id": channelID, "content": m.Content})
}

// length counts characters the way Discord's limits do, which this fake
// takes to be UTF-16 code units: the strictest reading of "characters".
func length(s string) int {
	return len(utf16.Encode([]rune(s)))
}

// content reads the content, embeds and components of a request and checks
// them against the limits in Discord's documentation. It returns what is
// wrong, or "".
func content(body map[string]any) (Message, string) {
	var m Message
	m.Content, _ = body["content"].(string)
	if length(m.Content) > 2000 {
		return m, "content: must be 2000 or fewer in length"
	}
	if am, sent := body["allowed_mentions"]; sent {
		if problem := validAllowedMentions(am); problem != "" {
			return m, problem
		}
	}
	rawEmbeds, _ := body["embeds"].([]any)
	if len(rawEmbeds) > 10 {
		return m, "embeds: must be 10 or fewer in length"
	}
	total := 0
	for _, re := range rawEmbeds {
		e, ok := re.(map[string]any)
		if !ok {
			return m, "embeds: not an object"
		}
		n, problem := validEmbed(e)
		if problem != "" {
			return m, problem
		}
		total += n
		m.Embeds = append(m.Embeds, e)
	}
	if total > 6000 {
		return m, "embeds: the embeds of a message may hold 6000 characters together"
	}
	rawRows, _ := body["components"].([]any)
	if len(rawRows) > 5 {
		return m, "components: a message may have 5 action rows"
	}
	seen := map[string]bool{}
	for _, rr := range rawRows {
		row, ok := rr.(map[string]any)
		if !ok {
			return m, "components: not an object"
		}
		if problem := validRow(row, seen); problem != "" {
			return m, problem
		}
		m.Components = append(m.Components, row)
	}
	return m, ""
}

func validAllowedMentions(v any) string {
	am, ok := v.(map[string]any)
	if !ok {
		return "allowed_mentions: not an object"
	}
	parse, _ := am["parse"].([]any)
	for _, p := range parse {
		if p != "users" && p != "roles" && p != "everyone" {
			return "allowed_mentions.parse: unknown mention type"
		}
	}
	return ""
}

// validEmbed checks an embed against the embed limits and returns how many
// characters count towards the limit of the message.
func validEmbed(e map[string]any) (int, string) {
	total := 0
	for name, limit := range map[string]int{"title": 256, "description": 4096} {
		s, _ := e[name].(string)
		if length(s) > limit {
			return 0, fmt.Sprintf("embeds.%s: must be %d or fewer in length", name, limit)
		}
		total += length(s)
	}
	if footer, ok := e["footer"].(map[string]any); ok {
		s, _ := footer["text"].(string)
		if length(s) > 2048 {
			return 0, "embeds.footer.text: must be 2048 or fewer in length"
		}
		total += length(s)
	}
	if color, sent := e["color"]; sent {
		if c, ok := color.(float64); !ok || c < 0 || c > 0xFFFFFF || c != float64(int(c)) {
			return 0, "embeds.color: not a colour"
		}
	}
	fields, _ := e["fields"].([]any)
	if len(fields) > 25 {
		return 0, "embeds.fields: must be 25 or fewer in length"
	}
	for _, rf := range fields {
		f, ok := rf.(map[string]any)
		if !ok {
			return 0, "embeds.fields: not an object"
		}
		name, _ := f["name"].(string)
		value, _ := f["value"].(string)
		if name == "" || length(name) > 256 {
			return 0, "embeds.fields.name: must be between 1 and 256 in length"
		}
		if value == "" || length(value) > 1024 {
			return 0, "embeds.fields.value: must be between 1 and 1024 in length"
		}
		total += length(name) + length(value)
	}
	return total, ""
}

// validRow checks an action row of buttons. seen holds the custom IDs of the
// message so far, which must be unique.
func validRow(row map[string]any, seen map[string]bool) string {
	if row["type"] != float64(1) {
		return "components: the top level of a message holds action rows"
	}
	buttons, _ := row["components"].([]any)
	if len(buttons) < 1 || len(buttons) > 5 {
		return "components: an action row holds between 1 and 5 buttons"
	}
	for _, rb := range buttons {
		b, ok := rb.(map[string]any)
		if !ok || b["type"] != float64(2) {
			return "components: not a button"
		}
		style, _ := b["style"].(float64)
		if style < 1 || style > 4 {
			return "components: a button with a custom_id has a style between 1 and 4"
		}
		label, _ := b["label"].(string)
		if label == "" || length(label) > 80 {
			return "components: a label must be between 1 and 80 in length"
		}
		id, _ := b["custom_id"].(string)
		if id == "" || length(id) > 100 {
			return "components: a custom_id must be between 1 and 100 in length"
		}
		if seen[id] {
			return "components: custom_id must be unique in a message"
		}
		if _, hasURL := b["url"]; hasURL {
			return "components: a button with a custom_id cannot have a url"
		}
		seen[id] = true
	}
	return ""
}
