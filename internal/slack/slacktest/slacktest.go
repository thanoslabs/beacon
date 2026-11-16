// Package slacktest is a fake Slack for tests. It implements the parts of the
// Slack Web API that Beacon calls, checks requests the way Slack documents
// them, and remembers the messages it was sent.
//
// It is not Slack: it proves that Beacon sends what the documentation
// describes, not that Slack accepts it.
package slacktest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
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
