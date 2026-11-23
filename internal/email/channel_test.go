package email_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/email"
	"github.com/thanoslabs/beacon/internal/email/mailpittest"
	"github.com/thanoslabs/beacon/internal/notify"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const (
	groupID = "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11"
	alertID = "9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90"
)

func firing() alert.Alert {
	return alert.Alert{
		ID: alertID, GroupID: groupID, Fingerprint: "fp", Status: alert.StatusFiring, Severity: alert.SeverityCritical,
		Labels: map[string]string{"alertname": "DiskFull", "host": "db1"},
		Annotations: map[string]string{
			"summary": "Disk is full on db1", "description": "/var/lib/postgresql has 2% free",
			"runbook_url": "https://runbooks.example.com/disk?a=1&b=2",
		},
		Links:     map[string]string{alert.LinkGenerator: "https://prometheus.example.com/graph?g0.expr=up"},
		StartedAt: t0,
	}
}

func message(to string, a alert.Alert) notify.Message {
	return notify.Message{
		Notification: notify.Notification{ID: "11111111-2222-4333-8444-555555555555", GroupID: groupID, Destination: to, Reason: notify.ReasonEscalationStep},
		Alerts:       []alert.Alert{a},
	}
}

// toMailpit returns a channel that sends to the Mailpit of compose.yaml as
// its one user.
func toMailpit(server *mailpittest.Server, password string) *email.Channel {
	cfg := config.EmailIntegration{Host: server.Host, Port: server.Port, From: "beacon@example.invalid", TLS: config.EmailTLSNone,
		Username: mailpittest.Username, Password: config.NewSecret(config.SecretRef{}, password)}
	ch := email.NewChannel(cfg, "https://beacon.example.com/")
	ch.SetClock(func() time.Time { return t0.Add(time.Minute) })
	return ch
}

// TestDeliversToARealServer sends through a real SMTP server and reads the
// message as that server parsed it: the headers, and both forms of the
// content.
func TestDeliversToARealServer(t *testing.T) {
	server := mailpittest.New(t)
	ch := toMailpit(server, mailpittest.Password)
	to := mailpittest.Address(t, "oncall")
	ctx := context.Background()

	ref, err := ch.Send(ctx, message(to, firing()))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if ref != "<beacon-11111111-2222-4333-8444-555555555555@example.invalid>" {
		t.Errorf("ref = %q, want the identifier of the message", ref)
	}
	msgs := server.Wait(t, to, 1)
	m := msgs[0]
	if m.Subject != "[FIRING] critical: Disk is full on db1" || m.From != "beacon@example.invalid" || len(m.To) != 1 || m.To[0] != to {
		t.Errorf("subject %q from %q to %v", m.Subject, m.From, m.To)
	}
	wantText := `FIRING · critical
Disk is full on db1

/var/lib/postgresql has 2% free

Labels:
  alertname = DiskFull
  host = db1

Status: Firing since 2026-03-01 12:00 UTC

Open in Beacon: https://beacon.example.com/alerts/` + alertID + `
Runbook: https://runbooks.example.com/disk?a=1&b=2
Source: https://prometheus.example.com/graph?g0.expr=up

-- 
Sent by Beacon because an escalation policy asks for a response to this alert.`
	if got := strings.TrimSpace(strings.ReplaceAll(m.Text, "\r\n", "\n")); got != wantText {
		t.Errorf("text:\n%s\nwant:\n%s", got, wantText)
	}
	for _, want := range []string{
		`<h2 style="margin: 4px 0 12px 0;">Disk is full on db1</h2>`, "FIRING · critical", "<code>host = db1</code>",
		`<a href="https://runbooks.example.com/disk?a=1&amp;b=2">Runbook</a>`,
		`<a href="https://beacon.example.com/alerts/` + alertID + `">Open in Beacon</a>`,
	} {
		if !strings.Contains(m.HTML, want) {
			t.Errorf("the HTML form does not contain %q:\n%s", want, m.HTML)
		}
	}
	header := func(m mailpittest.Message, name string) string { return strings.Join(m.Headers[name], "|") }
	for name, want := range map[string]string{
		"Message-Id":     ref,
		"In-Reply-To":    "<beacon-group-" + groupID + "@example.invalid>",
		"References":     "<beacon-group-" + groupID + "@example.invalid>",
		"Auto-Submitted": "auto-generated",
		"Date":           "Sun, 01 Mar 2026 12:01:00 +0000",
		"Mime-Version":   "1.0",
	} {
		if got := header(m, name); got != want {
			t.Errorf("%s = %q, want %q (headers: %v)", name, got, want, m.Headers)
		}
	}

	// The next message about the group answers the first, so that a mail
	// client shows one conversation.
	ack := firing()
	at := t0.Add(5 * time.Minute)
	ack.Status, ack.AcknowledgedAt = alert.StatusAcknowledged, &at
	update := message(to, ack)
	update.Notification.ID, update.Notification.Reason, update.PreviousRef = "22222222-2222-4333-8444-555555555555", notify.ReasonGroupUpdate, ref
	update.Events = []alert.Event{{AlertID: alertID, Type: alert.EventAcknowledged, Actor: alert.LinkedChatUser("u1", "slack", "U0ADA", "Ada Lovelace")}}
	ref2, err := ch.Send(ctx, update)
	if err != nil || ref2 == ref {
		t.Fatalf("second Send = %q, %v", ref2, err)
	}
	m = server.Wait(t, to, 2)[1]
	if m.Subject != "[ACKNOWLEDGED] critical: Disk is full on db1" || !strings.Contains(m.Text, "Status: Acknowledged by Ada Lovelace 2026-03-01 12:05 UTC") ||
		!strings.Contains(m.Text, "Sent by Beacon because this alert changed.") {
		t.Errorf("update: %q\n%s", m.Subject, m.Text)
	}
	if header(m, "In-Reply-To") != ref || header(m, "References") != "<beacon-group-"+groupID+"@example.invalid> "+ref {
		t.Errorf("the update does not answer the first message: %v", m.Headers)
	}
}

// TestAnAlertCannotAddHeadersOrMessages: whatever an alert says ends up as
// text in the one message to the one recipient.
func TestAnAlertCannotAddHeadersOrMessages(t *testing.T) {
	server := mailpittest.New(t)
	ch := toMailpit(server, mailpittest.Password)
	to, victim := mailpittest.Address(t, "oncall"), mailpittest.Address(t, "victim")

	a := firing()
	a.Annotations["summary"] = "Disk full\r\nBcc: " + victim + "\r\nX-Injected: yes\r\n\r\nbody of their own"
	// The end of the data, and a second message, as SMTP smuggling tries it.
	a.Annotations["description"] = "line one\r\n.\r\nMAIL FROM:<attacker@example.invalid>\r\nRCPT TO:<" + victim + ">\r\nDATA\r\nSubject: smuggled\r\n\r\nhello\r\n.\r\n" +
		"bare\rcarriage return, and <script>alert(1)</script> & é ✓\n.\n..leading dots"
	a.Labels["note"] = "x\r\nTo: " + victim
	a.Annotations["runbook_url"] = "https://runbooks.example.com/x\r\nX-Injected: yes"
	a.Links[alert.LinkGenerator] = "javascript:alert(1)"
	if _, err := ch.Send(context.Background(), message(to, a)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs := server.Wait(t, to, 1)
	if len(msgs) != 1 || len(server.Messages(t, victim)) != 0 {
		t.Fatalf("%d messages for the recipient and %d for someone the alert named, want 1 and 0", len(msgs), len(server.Messages(t, victim)))
	}
	m := msgs[0]
	if m.Subject != "[FIRING] critical: Disk full Bcc: "+victim+" X-Injected: yes body of their own" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, name := range []string{"X-Injected", "Bcc"} {
		if _, has := m.Headers[name]; has {
			t.Errorf("the message has the header %s: %v", name, m.Headers)
		}
	}
	if len(m.To) != 1 || m.To[0] != to {
		t.Errorf("to = %v", m.To)
	}
	text := strings.ReplaceAll(m.Text, "\r\n", "\n")
	for _, want := range []string{
		"line one\n.\nMAIL FROM:<attacker@example.invalid>\nRCPT TO:<" + victim + ">\nDATA\nSubject: smuggled\n\nhello\n.\nbare\ncarriage return, and <script>alert(1)</script> & é ✓\n.\n..leading dots",
		"  note = x To: " + victim,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the text does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "&lt;script&gt;alert(1)&lt;/script&gt; &amp; é ✓") {
		t.Errorf("the HTML form does not escape the alert's text:\n%s", m.HTML)
	}
	if strings.Contains(m.HTML, "javascript:") || strings.Contains(m.Text, "Runbook:") {
		t.Errorf("an address that is not a web address became a link:\n%s", m.HTML)
	}
}

// TestDestinationMustBeOneAddress: a destination goes into the To header and
// into the SMTP exchange, so anything but one plain address is refused
// before a connection is made.
func TestDestinationMustBeOneAddress(t *testing.T) {
	fake := newFakeSMTP(t)
	port := fake.start()
	ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: port, From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")
	for _, to := range []string{
		"", "nobody", "#alerts", "Ada <ada@example.invalid>", "ada@example.invalid, grace@example.invalid",
		"ada@example.invalid\r\nRCPT TO:<victim@example.invalid>", "ada@example.invalid\nBcc: victim@example.invalid", "<ada@example.invalid>", "ada @example.invalid",
	} {
		_, err := ch.Send(context.Background(), message(to, firing()))
		if permanent, _ := notify.Classify(err); err == nil || !permanent || err.Error() != "email: the destination is not an email address" {
			t.Errorf("Send to %q = %v, want it refused for good", to, err)
		}
	}
	if seen := fake.seen(); seen != "" {
		t.Errorf("the server was contacted: %s", seen)
	}
}

func TestSignInThatIsRefused(t *testing.T) {
	server := mailpittest.New(t)
	to := mailpittest.Address(t, "oncall")
	const wrong = "wrong-password-marker-71c4"
	_, err := toMailpit(server, wrong).Send(context.Background(), message(to, firing()))
	if permanent, _ := notify.Classify(err); err == nil || !permanent || !strings.Contains(err.Error(), "email: sign in to the SMTP server: the SMTP server answered 535") {
		t.Errorf("Send with a wrong password = %v, want a refusal that is not retried", err)
	}
	if err != nil && strings.Contains(err.Error(), wrong) {
		t.Errorf("the error contains the password: %v", err)
	}
	// Without credentials, the server that wants them refuses the sender.
	cfg := config.EmailIntegration{Host: server.Host, Port: server.Port, From: "beacon@example.invalid", TLS: config.EmailTLSNone}
	_, err = email.NewChannel(cfg, "").Send(context.Background(), message(to, firing()))
	if permanent, _ := notify.Classify(err); err == nil || !permanent || !strings.Contains(err.Error(), "MAIL FROM: the SMTP server answered 530") {
		t.Errorf("Send without credentials = %v", err)
	}
	if n := len(server.Messages(t, to)); n != 0 {
		t.Errorf("%d messages were delivered", n)
	}
}

// TestTLS covers the two ways a connection is secured, and that Beacon
// never falls back to an unencrypted one.
func TestTLS(t *testing.T) {
	const password = "smtp-password-marker-2c9e"
	channel := func(fake *fakeSMTP, mode string, trusted bool) *email.Channel {
		cfg := config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: mode,
			Username: "beacon", Password: config.NewSecret(config.SecretRef{}, password)}
		ch := email.NewChannel(cfg, "")
		if trusted {
			ch.SetRoots(fake.roots)
		}
		return ch
	}
	send := func(ch *email.Channel) error {
		_, err := ch.Send(context.Background(), message("oncall@example.invalid", firing()))
		return err
	}

	t.Run("STARTTLS, then sign in", func(t *testing.T) {
		fake := newFakeSMTP(t)
		fake.startTLS, fake.auth = true, "PLAIN LOGIN"
		if err := send(channel(fake, config.EmailTLSStartTLS, true)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		// The password travels only after the connection is encrypted.
		if got, want := fake.seen(), "EHLO, STARTTLS, EHLO (TLS), AUTH (TLS), MAIL (TLS), RCPT (TLS), DATA (TLS), QUIT (TLS)"; got != want {
			t.Errorf("exchange: %s\nwant:     %s", got, want)
		}
		if len(fake.received()) != 1 || len(fake.credentials) != 1 || fake.credentials[0] != "beacon:"+password {
			t.Errorf("%d messages, credentials %v", len(fake.received()), fake.credentials)
		}
	})
	t.Run("a server that does not offer STARTTLS is refused", func(t *testing.T) {
		fake := newFakeSMTP(t)
		fake.auth = "PLAIN"
		err := send(channel(fake, config.EmailTLSStartTLS, true))
		if permanent, _ := notify.Classify(err); err == nil || !permanent || !strings.Contains(err.Error(), "does not offer STARTTLS, and Beacon does not fall back to an unencrypted connection") {
			t.Fatalf("Send = %v", err)
		}
		if got := fake.seen(); got != "EHLO" || len(fake.credentials) != 0 {
			t.Errorf("exchange: %s; nothing may follow the refusal", got)
		}
	})
	t.Run("a certificate that is not trusted is refused", func(t *testing.T) {
		for _, mode := range []string{config.EmailTLSStartTLS, config.EmailTLSImplicit} {
			fake := newFakeSMTP(t)
			fake.startTLS, fake.implicit, fake.auth = mode == config.EmailTLSStartTLS, mode == config.EmailTLSImplicit, "PLAIN"
			err := send(channel(fake, mode, false))
			if err == nil || !strings.Contains(err.Error(), "certificate") {
				t.Fatalf("%s: Send = %v, want the certificate refused", mode, err)
			}
			if len(fake.credentials) != 0 || len(fake.received()) != 0 || strings.Contains(fake.seen(), "MAIL") {
				t.Errorf("%s: the exchange went on: %s", mode, fake.seen())
			}
		}
	})
	t.Run("implicit TLS", func(t *testing.T) {
		fake := newFakeSMTP(t)
		fake.implicit, fake.auth = true, "PLAIN"
		if err := send(channel(fake, config.EmailTLSImplicit, true)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got, want := fake.seen(), "EHLO (TLS), AUTH (TLS), MAIL (TLS), RCPT (TLS), DATA (TLS), QUIT (TLS)"; got != want {
			t.Errorf("exchange: %s\nwant:     %s", got, want)
		}
	})
	t.Run("a server that offers LOGIN only", func(t *testing.T) {
		fake := newFakeSMTP(t)
		fake.startTLS, fake.auth = true, "LOGIN XOAUTH2"
		if err := send(channel(fake, config.EmailTLSStartTLS, true)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(fake.credentials) != 1 || fake.credentials[0] != "beacon:"+password || len(fake.received()) != 1 {
			t.Errorf("credentials %v, %d messages", fake.credentials, len(fake.received()))
		}
	})
	t.Run("a server that offers no way to sign in that Beacon knows", func(t *testing.T) {
		fake := newFakeSMTP(t)
		fake.startTLS, fake.auth = true, "XOAUTH2"
		err := send(channel(fake, config.EmailTLSStartTLS, true))
		if permanent, _ := notify.Classify(err); err == nil || !permanent || !strings.Contains(err.Error(), "offers no way to sign in that Beacon knows") {
			t.Fatalf("Send = %v", err)
		}
	})
	t.Run("the password is not sent in the clear to another machine", func(t *testing.T) {
		// tls: none with credentials is refused by validation for any host
		// but this machine. Should it reach the client all the same, the
		// client refuses too. "localhost." is this machine by another
		// name, which the rule does not know.
		for _, mechanisms := range []string{"PLAIN", "LOGIN"} {
			fake := newFakeSMTP(t)
			fake.auth = mechanisms
			cfg := config.EmailIntegration{Host: "localhost.", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone,
				Username: "beacon", Password: config.NewSecret(config.SecretRef{}, password)}
			err := send(email.NewChannel(cfg, ""))
			if err == nil || !strings.Contains(err.Error(), "unencrypted connection") || len(fake.credentials) != 0 {
				t.Errorf("%s: Send = %v, credentials sent: %v", mechanisms, err, fake.credentials)
			}
		}
	})
}

// TestRepliesDecideWhetherToRetry: a reply that starts with 4 is temporary
// and one that starts with 5 is not.
func TestRepliesDecideWhetherToRetry(t *testing.T) {
	tests := []struct {
		at, reply string
		permanent bool
		want      string
	}{
		{"RCPT", "550 5.1.1 no such user here", true, "email: RCPT TO: the SMTP server answered 550 5.1.1 no such user here"},
		{"RCPT", "450 4.2.1 mailbox busy,\ttry later", false, "email: RCPT TO: the SMTP server answered 450 4.2.1 mailbox busy, try later"},
		{"MAIL", "451 4.3.0 try again later", false, "email: MAIL FROM: the SMTP server answered 451"},
		{"DATA", "554 5.7.1 no", true, "email: DATA: the SMTP server answered 554"},
		{".", "452 4.3.1 out of space", false, "email: send the message: the SMTP server answered 452"},
		{".", "552 5.3.4 too big " + strings.Repeat("x", 500), true, "email: send the message: the SMTP server answered 552 5.3.4 too big " + strings.Repeat("x", 150)},
		{"EHLO", "421 4.3.2 shutting down", false, "email: EHLO: the SMTP server answered 421"},
	}
	for _, tt := range tests {
		fake := newFakeSMTP(t)
		fake.replies[tt.at] = tt.reply
		// A server that refuses EHLO is asked again with HELO.
		fake.replies["HELO"] = tt.reply
		ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")
		_, err := ch.Send(context.Background(), message("oncall@example.invalid", firing()))
		permanent, _ := notify.Classify(err)
		if err == nil || permanent != tt.permanent || !strings.HasPrefix(err.Error(), tt.want) || len(err.Error()) > 300 {
			t.Errorf("%s answered %q: Send = %v (permanent %v), want %q (permanent %v)", tt.at, tt.reply[:20], err, permanent, tt.want, tt.permanent)
		}
		if len(fake.received()) != 0 {
			t.Errorf("%s: a message was taken", tt.at)
		}
	}
}

func TestSendHonoursTheContext(t *testing.T) {
	for _, at := range []string{"EHLO", "RCPT", "DATA"} {
		fake := newFakeSMTP(t)
		fake.stall = at
		ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := ch.Send(ctx, message("oncall@example.invalid", firing()))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
			t.Errorf("stalled at %s: Send = %v after %v, want the context deadline", at, err, time.Since(start))
		}
	}
	// A server that is not there.
	ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: 1, From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")
	_, err := ch.Send(context.Background(), message("oncall@example.invalid", firing()))
	if permanent, _ := notify.Classify(err); err == nil || permanent || !strings.Contains(err.Error(), "email: connect to the SMTP server: ") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("Send to a closed port = %v, want a failure that is retried and names no address", err)
	}
}

// TestGroupOfSeveralAlerts: the message of a larger group counts and lists
// its alerts, and says who was paged.
func TestGroupOfSeveralAlerts(t *testing.T) {
	fake := newFakeSMTP(t)
	ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "https://beacon.example.com")
	a, b := firing(), firing()
	at := t0.Add(time.Minute)
	b.ID, b.Labels, b.Annotations = "alert-2", map[string]string{"alertname": "DiskFull", "host": "db2"}, map[string]string{"summary": "Disk is full on db2"}
	b.Status, b.ResolvedAt = alert.StatusResolved, &at
	m := message("oncall@example.invalid", a)
	m.Alerts, m.Group = []alert.Alert{b, a}, alert.Group{Labels: map[string]string{"alertname": "DiskFull"}}
	m.Events = []alert.Event{{Type: notify.EventEscalated, Data: map[string]any{
		"notified": []any{map[string]any{"user": "ada@example.com", "name": "Ada Lovelace", "destination": "ada@example.com"}},
		"skipped":  []any{map[string]any{"user": "grace@example.com", "name": "Grace Hopper", "not_connected": "Slack"}},
	}}}
	if _, err := ch.Send(context.Background(), m); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw := fake.received()[0]
	for _, want := range []string{
		"Subject: [FIRING] critical: DiskFull (2 alerts)",
		"2 alerts: 1 firing, 1 resolved",
		"  - firing: Disk is full on db1 (host =3D db1)",
		"  - resolved: Disk is full on db2 (host =3D db2)",
		"Paged: Ada Lovelace, Grace Hopper (Slack not connected)",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the message does not contain %q:\n%s", want, raw)
		}
	}
	// No line of a message is longer than SMTP allows.
	for _, line := range strings.Split(raw, "\n") {
		if len(line) > 998 {
			t.Errorf("a line has %d characters", len(line))
		}
	}
}

// TestOnlyWebAddressesBecomeLinks: an address from an alert is offered as a
// link only when it is a plain http or https address of a bounded length.
func TestOnlyWebAddressesBecomeLinks(t *testing.T) {
	for _, good := range []string{"https://runbooks.example.com/disk?a=1&b=2#top", "http://10.0.0.1:9090/graph"} {
		if got := email.WebAddress(good); got != good {
			t.Errorf("WebAddress(%q) = %q, want it kept", good, got)
		}
	}
	for _, bad := range []string{
		"", "runbooks.example.com/disk", "javascript:alert(1)", "data:text/html,<p>", "ftp://files.example.com/x", "mailto:someone@example.invalid",
		"https:///path-without-a-host", "https://example.com/a b", "https://example.com/\"onmouseover=\"x", "https://example.com/<script>",
		"https://example.com/\r\nX-Injected: yes", "https://example.com/" + strings.Repeat("a", 2000),
	} {
		if got := email.WebAddress(bad); got != "" {
			t.Errorf("WebAddress(%.60q) = %q, want it refused", bad, got)
		}
	}
}

// TestHeadersAreASCII: a subject in another script is encoded as RFC 2047
// asks, and a real server reads it back as it was.
func TestHeadersAreASCII(t *testing.T) {
	server := mailpittest.New(t)
	to := mailpittest.Address(t, "oncall")
	a := firing()
	a.Annotations["summary"] = "Disque plein sur db1 — ça déborde ✓ " + strings.Repeat("磁盘已满", 20)
	if _, err := toMailpit(server, mailpittest.Password).Send(context.Background(), message(to, a)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m := server.Wait(t, to, 1)[0]
	if want := "[FIRING] critical: Disque plein sur db1 — ça déborde ✓ 磁盘已满"; !strings.HasPrefix(m.Subject, want) || !strings.HasSuffix(m.Subject, "…") {
		t.Errorf("subject = %q, want it to start with %q and be clipped", m.Subject, want)
	}
	if !strings.Contains(m.Text, "Disque plein sur db1 — ça déborde ✓") {
		t.Errorf("text:\n%s", m.Text)
	}

	// What goes over the wire has no byte outside ASCII in its headers.
	fake := newFakeSMTP(t)
	ch := email.NewChannel(config.EmailIntegration{Host: "127.0.0.1", Port: fake.start(), From: "beacon@example.invalid", TLS: config.EmailTLSNone}, "")
	if _, err := ch.Send(context.Background(), message("oncall@example.invalid", a)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	headers, _, _ := strings.Cut(fake.received()[0], "\n\n")
	if !strings.Contains(headers, "Subject: =?utf-8?q?") {
		t.Errorf("the subject is not encoded:\n%s", headers)
	}
	for _, r := range headers {
		if r > 126 {
			t.Fatalf("the headers hold %q:\n%s", r, headers)
		}
	}
}
