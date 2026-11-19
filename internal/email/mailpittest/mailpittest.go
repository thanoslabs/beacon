// Package mailpittest gives tests a real SMTP server: the Mailpit that
// compose.yaml runs, which keeps every message it is sent and delivers none.
//
// Tests that use it need Mailpit running, and its addresses in two
// environment variables:
//
//	docker compose up -d --wait mailpit
//	export BEACON_TEST_SMTP_ADDR='127.0.0.1:1025'
//	export BEACON_TEST_MAILPIT_URL='http://127.0.0.1:8025'
//
// Without the variables the tests are skipped, except in CI, where a
// missing server is a failure. The user below is defined in
// dev/mailpit/auth.
package mailpittest

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The environment variables that hold the address of Mailpit's SMTP server
// and of its API.
const (
	EnvSMTP = "BEACON_TEST_SMTP_ADDR"
	EnvAPI  = "BEACON_TEST_MAILPIT_URL"
)

// The one user Mailpit takes mail from.
const (
	Username = "beacon"
	Password = "beacon-local-development-password"
)

// Server is the Mailpit of compose.yaml.
type Server struct {
	// Host and Port are where its SMTP server listens.
	Host string
	Port int
	api  string
}

// Configured reports whether there is a Mailpit to test against, or there
// must be one because the tests run in CI. A test that has other things to
// check as well asks before it calls New, which would skip it.
func Configured() bool {
	return os.Getenv("CI") != "" || (os.Getenv(EnvSMTP) != "" && os.Getenv(EnvAPI) != "")
}

// New returns the Mailpit to test against, or skips the test when there is
// none.
func New(t testing.TB) *Server {
	t.Helper()
	smtp, api := os.Getenv(EnvSMTP), os.Getenv(EnvAPI)
	if smtp == "" || api == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s and %s are not set; email tests must not be skipped in CI", EnvSMTP, EnvAPI)
		}
		t.Skipf("%s and %s are not set; skipping test that needs an SMTP server", EnvSMTP, EnvAPI)
	}
	host, port, err := net.SplitHostPort(smtp)
	if err != nil {
		t.Fatalf("%s: %v", EnvSMTP, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("%s: %v", EnvSMTP, err)
	}
	return &Server{Host: host, Port: n, api: strings.TrimRight(api, "/")}
}

// Address returns an email address that no other test uses, so that tests
// which share the server find only their own messages.
func Address(t testing.TB, name string) string {
	t.Helper()
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("random: %v", err)
	}
	return name + "-" + hex.EncodeToString(raw) + "@example.invalid"
}

// Message is an email as Mailpit received and parsed it.
type Message struct {
	ID      string
	Subject string
	From    string
	To      []string
	// Text and HTML are the two forms of the content, decoded.
	Text string
	HTML string
	// Headers are the headers of the message, by name.
	Headers map[string][]string
}

func (s *Server) get(t testing.TB, path string, out any) {
	t.Helper()
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(s.api + path)
	if err != nil {
		t.Fatalf("ask Mailpit: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test helper
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Mailpit answered %d to %s: %s", resp.StatusCode, path, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("Mailpit answered %s with %q, not JSON", path, raw)
	}
}

// Messages returns the messages Mailpit holds for a recipient, oldest
// first.
func (s *Server) Messages(t testing.TB, to string) []Message {
	t.Helper()
	var found struct {
		Messages []struct{ ID string }
	}
	s.get(t, "/api/v1/search?query="+url.QueryEscape(`to:"`+to+`"`), &found)
	out := make([]Message, 0, len(found.Messages))
	// Mailpit lists the newest first.
	for i := len(found.Messages) - 1; i >= 0; i-- {
		var m struct {
			ID      string
			Subject string
			From    struct{ Address string }
			To      []struct{ Address string }
			Text    string
			HTML    string
		}
		s.get(t, "/api/v1/message/"+found.Messages[i].ID, &m)
		msg := Message{ID: m.ID, Subject: m.Subject, From: m.From.Address, Text: m.Text, HTML: m.HTML}
		for _, a := range m.To {
			msg.To = append(msg.To, a.Address)
		}
		s.get(t, "/api/v1/message/"+m.ID+"/headers", &msg.Headers)
		out = append(out, msg)
	}
	return out
}

// Wait returns the messages for a recipient once there are at least n.
func (s *Server) Wait(t testing.TB, to string, n int) []Message {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		msgs := s.Messages(t, to)
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("Mailpit has %d messages for %s, want %d", len(msgs), to, n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
