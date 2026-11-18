// Package slacktest is a fake Slack for tests. It implements the parts of the
// Slack Web API that Beacon calls, checks requests the way Slack documents
// them, and remembers the messages it was sent.
//
// It is not Slack: it proves that Beacon sends what the documentation
// describes, not that Slack accepts it.
package slacktest

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Message is a message as the fake Slack holds it.
type Message struct {
	Channel string
	TS      string
	Text    string
	Blocks  []map[string]any
	// Updates counts how many times chat.update changed the message.
	Updates int
}

// JSON returns the blocks encoded, for searching their content.
func (m Message) JSON() string {
	b, _ := json.Marshal(m.Blocks)
	return string(b)
}

// Call is one request to a Web API method.
type Call struct {
	Method string
	Body   map[string]any
}

// Failure makes the fake Slack fail a call.
type Failure struct {
	// Status is an HTTP status to answer with, such as 429 or 503.
	Status int
	// RetryAfter is the value of the Retry-After header, if any.
	RetryAfter string
	// Error is an error code to answer with in a 200 response, such as
	// "channel_not_found".
	Error string
}

// Server is a fake Slack.
type Server struct {
	// URL is the base URL to use as the Slack API URL.
	URL string

	token string

	mu       sync.Mutex
	channels map[string]string
	messages map[string]*Message
	order    []string
	calls    []Call
	failures map[string][]Failure
	seq      int

	responses []map[string]any
}

// New starts a fake Slack that accepts the given bot token and has one
// channel, named "alerts", with the ID "C0ALERTS". It stops when the test
// ends.
func New(t testing.TB, token string) *Server {
	t.Helper()
	s := &Server{
		token:    token,
		channels: map[string]string{"alerts": "C0ALERTS", "C0ALERTS": "C0ALERTS"},
		messages: map[string]*Message{},
		failures: map[string][]Failure{},
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// AddChannel gives the workspace another channel, with a name and an ID.
func (s *Server) AddChannel(name, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channels[name], s.channels[id] = id, id
}

// Fail makes the next calls to method fail, one failure per call, in order.
func (s *Server) Fail(method string, failures ...Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[method] = append(s.failures[method], failures...)
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

// Calls returns the requests made to method, including the ones that failed.
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Delete removes a message, as a person deleting it in Slack would.
func (s *Server) Delete(channel, ts string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.messages, channel+":"+ts)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/")
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.HasPrefix(method, "response/") {
		s.responses = append(s.responses, body)
		reply(w, map[string]any{"ok": true})
		return
	}
	s.calls = append(s.calls, Call{Method: method, Body: body})

	if queue := s.failures[method]; len(queue) > 0 {
		f := queue[0]
		s.failures[method] = queue[1:]
		if f.Status != 0 {
			if f.RetryAfter != "" {
				w.Header().Set("Retry-After", f.RetryAfter)
			}
			w.WriteHeader(f.Status)
			return
		}
		reply(w, map[string]any{"ok": false, "error": f.Error})
		return
	}

	switch {
	case r.Method != http.MethodPost:
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.Header.Get("Authorization") != "Bearer "+s.token:
		reply(w, map[string]any{"ok": false, "error": "invalid_auth"})
	case !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json"):
		reply(w, map[string]any{"ok": false, "error": "invalid_form_data"})
	case body == nil:
		reply(w, map[string]any{"ok": false, "error": "invalid_json"})
	case method == "chat.postMessage":
		reply(w, s.postMessage(body))
	case method == "chat.update":
		reply(w, s.update(body))
	default:
		reply(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (s *Server) postMessage(body map[string]any) map[string]any {
	name, _ := body["channel"].(string)
	channel, ok := s.channels[name]
	if !ok {
		return map[string]any{"ok": false, "error": "channel_not_found"}
	}
	m, errCode := content(body)
	if errCode != "" {
		return map[string]any{"ok": false, "error": errCode}
	}
	s.seq++
	m.Channel, m.TS = channel, fmt.Sprintf("1700000000.%06d", s.seq)
	key := channel + ":" + m.TS
	s.messages[key] = &m
	s.order = append(s.order, key)
	return map[string]any{"ok": true, "channel": channel, "ts": m.TS}
}

func (s *Server) update(body map[string]any) map[string]any {
	channel, _ := body["channel"].(string)
	ts, _ := body["ts"].(string)
	// chat.update takes a channel ID, not a name.
	if s.channels[channel] != channel {
		return map[string]any{"ok": false, "error": "channel_not_found"}
	}
	old, ok := s.messages[channel+":"+ts]
	if !ok {
		return map[string]any{"ok": false, "error": "message_not_found"}
	}
	m, errCode := content(body)
	if errCode != "" {
		return map[string]any{"ok": false, "error": errCode}
	}
	m.Channel, m.TS, m.Updates = channel, ts, old.Updates+1
	s.messages[channel+":"+ts] = &m
	return map[string]any{"ok": true, "channel": channel, "ts": ts}
}

// content reads the text and blocks of a request and checks them against the
// limits in the Block Kit reference.
func content(body map[string]any) (Message, string) {
	var m Message
	m.Text, _ = body["text"].(string)
	rawBlocks, _ := body["blocks"].([]any)
	if m.Text == "" && len(rawBlocks) == 0 {
		return m, "no_text"
	}
	for _, rb := range rawBlocks {
		b, ok := rb.(map[string]any)
		if !ok || !validBlock(b) {
			return m, "invalid_blocks"
		}
		m.Blocks = append(m.Blocks, b)
	}
	return m, ""
}

func validBlock(b map[string]any) bool {
	elements, _ := b["elements"].([]any)
	switch b["type"] {
	case "section":
		return validText(b["text"], 3000, "mrkdwn", "plain_text")
	case "context":
		if len(elements) < 1 || len(elements) > 10 {
			return false
		}
		for _, e := range elements {
			if !validText(e, 3000, "mrkdwn", "plain_text") {
				return false
			}
		}
		return true
	case "actions":
		if len(elements) < 1 || len(elements) > 25 {
			return false
		}
		seen := map[string]bool{}
		for _, e := range elements {
			btn, ok := e.(map[string]any)
			if !ok || btn["type"] != "button" || !validText(btn["text"], 75, "plain_text") {
				return false
			}
			actionID, _ := btn["action_id"].(string)
			value, _ := btn["value"].(string)
			style, _ := btn["style"].(string)
			if actionID == "" || len(actionID) > 255 || seen[actionID] || len(value) > 2000 ||
				(style != "" && style != "primary" && style != "danger") {
				return false
			}
			seen[actionID] = true
		}
		return true
	default:
		return false
	}
}

func validText(v any, maxLen int, types ...string) bool {
	t, ok := v.(map[string]any)
	if !ok {
		return false
	}
	s, _ := t["text"].(string)
	if s == "" || len([]rune(s)) > maxLen {
		return false
	}
	for _, typ := range types {
		if t["type"] == typ {
			return true
		}
	}
	return false
}

func reply(w http.ResponseWriter, body map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(body)
}

// User is a person in the fake Slack workspace.
type User struct {
	ID       string
	Username string
}

// Responses returns the messages Beacon sent through the response URL of an
// interaction, which Slack shows to the person who pressed the button.
func (s *Server) Responses() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.responses...)
}

// InteractionBody returns the body Slack sends when user presses the button
// with the given action ID on m: a form with one field, payload, holding a
// block_actions payload as JSON. The test fails if m has no such button.
func (s *Server) InteractionBody(t testing.TB, m Message, actionID string, user User) []byte {
	t.Helper()
	value, found := "", false
	for _, b := range m.Blocks {
		elements, _ := b["elements"].([]any)
		for _, e := range elements {
			if btn, ok := e.(map[string]any); ok && btn["action_id"] == actionID {
				value, _ = btn["value"].(string)
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the message has no button with action ID %q: %s", actionID, m.JSON())
	}
	s.mu.Lock()
	s.seq++
	n := s.seq
	s.mu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"type":       "block_actions",
		"team":       map[string]any{"id": "T0FAKE", "domain": "fake"},
		"user":       map[string]any{"id": user.ID, "username": user.Username, "team_id": "T0FAKE"},
		"api_app_id": "A0FAKE",
		"token":      "deprecated-verification-token",
		"container":  map[string]any{"type": "message", "message_ts": m.TS, "channel_id": m.Channel, "is_ephemeral": false},
		"trigger_id": fmt.Sprintf("%d.1.abc", n),
		"channel":    map[string]any{"id": m.Channel, "name": "alerts"},
		"message":    map[string]any{"type": "message", "ts": m.TS, "text": m.Text},
		// A real response URL is on hooks.slack.com.
		"response_url": fmt.Sprintf("%s/response/%d", s.URL, n),
		"actions": []map[string]any{{
			"action_id": actionID,
			"block_id":  "b1",
			"text":      map[string]any{"type": "plain_text", "text": "Button", "emoji": true},
			"value":     value,
			"type":      "button",
			"action_ts": "1700000000.000000",
		}},
	})
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return []byte(url.Values{"payload": {string(payload)}}.Encode())
}

// Sign returns the values of the X-Slack-Request-Timestamp and
// X-Slack-Signature headers for a body sent at the given time, computed the
// way Slack documents.
func Sign(signingSecret string, at time.Time, body []byte) (timestamp, signature string) {
	timestamp = strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte("v0:" + timestamp + ":"))
	mac.Write(body)
	return timestamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

// Click presses a button on m as user: it sends the signed interaction to
// requestURL, the interactions endpoint of Beacon, and returns the status of
// the answer.
func (s *Server) Click(t testing.TB, requestURL, signingSecret string, m Message, actionID string, user User) int {
	t.Helper()
	body := s.InteractionBody(t, m, actionID, user)
	req, err := http.NewRequest(http.MethodPost, requestURL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	timestamp, signature := Sign(signingSecret, time.Now(), body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", timestamp)
	req.Header.Set("X-Slack-Signature", signature)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send interaction: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}
