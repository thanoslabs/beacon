package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/provision/provisiontest"
	"github.com/thanoslabs/beacon/internal/webhook"
)

var update = flag.Bool("update", false, "rewrite the golden payload")

const (
	signingSecret = "webhook-signing-secret-marker-8f2b"
	headerToken   = "Bearer header-token-marker-66c0"
	queryToken    = "query-token-marker-19de"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// delivery is one request as a receiver saw it.
type delivery struct {
	header http.Header
	body   []byte
	query  string
}

// receiver is the other end of a webhook: an HTTP server that keeps what it
// is sent and answers as a test says.
type receiver struct {
	*httptest.Server
	mu  sync.Mutex
	got []delivery
	// answers are the statuses to answer with, one per request; then 200.
	answers []int
	// retryAfter is sent with every answer that is not a success.
	retryAfter string
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.got = append(r.got, delivery{header: req.Header.Clone(), body: body, query: req.URL.RawQuery})
		status := http.StatusOK
		if len(r.answers) > 0 {
			status, r.answers = r.answers[0], r.answers[1:]
		}
		retryAfter := r.retryAfter
		r.mu.Unlock()
		if status >= 300 && retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
		}
		w.WriteHeader(status)
		// A receiver may say anything, including what it was sent.
		_, _ = w.Write([]byte("you sent " + req.Header.Get("Authorization") + " to ?" + req.URL.RawQuery))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) deliveries() []delivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivery(nil), r.got...)
}

func channelTo(url string) *webhook.Channel {
	ch := webhook.NewChannel(config.WebhookIntegration{
		URL:           url + "/hooks/beacon?token=" + queryToken,
		SigningSecret: config.NewSecret(config.SecretRef{}, signingSecret),
		Headers: map[string]config.HeaderValue{
			"X-Team":        {Plain: "platform"},
			"Authorization": {Secret: config.NewSecret(config.SecretRef{Env: "HOOK_TOKEN"}, headerToken)},
		},
	}, "https://beacon.example.com/")
	ch.SetClock(func() time.Time { return t0.Add(90 * time.Second) })
	return ch
}

const (
	groupID = "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11"
	alertID = "9b2d6c1e-0a4f-4a57-8a0c-5f3c7d2e1b90"
	noteID  = "11111111-2222-4333-8444-555555555555"
)

func firing() alert.Alert {
	return alert.Alert{
		ID: alertID, GroupID: groupID, Fingerprint: "fp-1", Status: alert.StatusFiring, Severity: alert.SeverityCritical,
		Labels:      map[string]string{"alertname": "DiskFull", "host": "db1"},
		Annotations: map[string]string{"summary": "Disk is full on db1", "runbook_url": "https://runbooks.example.com/disk"},
		Links:       map[string]string{alert.LinkGenerator: "https://prometheus.example.com/graph?g0.expr=up"},
		Service:     "postgres", StartedAt: t0,
	}
}

func message(alerts ...alert.Alert) notify.Message {
	return notify.Message{
		Notification: notify.Notification{ID: noteID, GroupID: groupID, Destination: "tickets", Reason: notify.ReasonEscalationStep},
		Group:        alert.Group{ID: groupID, Labels: map[string]string{"alertname": "DiskFull"}},
		Alerts:       alerts,
	}
}

// TestPostsASignedPayload: the request a receiver gets, and how it checks
// that Beacon sent it.
func TestPostsASignedPayload(t *testing.T) {
	r := newReceiver(t)
	ch := channelTo(r.URL)
	ack := firing()
	at := t0.Add(time.Minute)
	ack.Status, ack.AcknowledgedAt = alert.StatusAcknowledged, &at
	m := message(ack)
	m.Notification.Reason = notify.ReasonGroupUpdate
	m.Events = []alert.Event{
		{AlertID: alertID, Type: alert.EventAcknowledged, Actor: alert.LinkedChatUser("u1", "slack", "U0ADA", "Ada Lovelace"), OccurredAt: at},
		{Type: notify.EventEscalated, Data: map[string]any{
			"notified": []any{map[string]any{"user": "ada@example.com", "name": "Ada Lovelace", "destination": "U0ADA"}},
			"skipped":  []any{map[string]any{"user": "grace@example.com", "name": "Grace Hopper", "not_connected": "Slack"}},
		}},
	}
	ref, err := ch.Send(context.Background(), m)
	if err != nil || ref != "" {
		t.Fatalf("Send = %q, %v", ref, err)
	}
	got := r.deliveries()
	if len(got) != 1 {
		t.Fatalf("%d requests, want 1", len(got))
	}
	d := got[0]
	for name, want := range map[string]string{
		"Content-Type": "application/json", "User-Agent": "Beacon/dev", "X-Beacon-Reason": "group_update", "Idempotency-Key": noteID,
		"X-Beacon-Timestamp": "1772366490", "X-Team": "platform", "Authorization": headerToken,
	} {
		if d.header.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, d.header.Get(name), want)
		}
	}

	// The payload is the documented one.
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, d.body, "", "  "); err != nil {
		t.Fatalf("the body is not JSON: %s", d.body)
	}
	const golden = "testdata/payload.golden.json"
	if *update {
		if err := os.WriteFile(golden, append(pretty.Bytes(), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(want)) != pretty.String() {
		t.Errorf("payload:\n%s\nwant %s; run with -update after a deliberate change, and update docs/outbound-webhook.md", pretty.String(), golden)
	}

	// What a receiver does: check the signature over the timestamp and the
	// body as they arrived, and refuse an old timestamp.
	now := t0.Add(2 * time.Minute)
	timestamp, signature := d.header.Get("X-Beacon-Timestamp"), d.header.Get("X-Beacon-Signature")
	if !webhook.Verify(signingSecret, timestamp, signature, d.body, now, 5*time.Minute) {
		t.Fatalf("the signature %q does not verify", signature)
	}
	if signature != webhook.Sign(signingSecret, timestamp, d.body) || !strings.HasPrefix(signature, "v1=") || len(signature) != 3+64 {
		t.Errorf("signature = %q", signature)
	}
	tampered := bytes.Replace(d.body, []byte("acknowledged"), []byte("resolved"), 1)
	for name, ok := range map[string]bool{
		"another secret":       webhook.Verify("another-secret-of-some-length", timestamp, signature, d.body, now, 5*time.Minute),
		"a body that changed":  webhook.Verify(signingSecret, timestamp, signature, tampered, now, 5*time.Minute),
		"another timestamp":    webhook.Verify(signingSecret, "1772366491", signature, d.body, now, 5*time.Minute),
		"a request replayed":   webhook.Verify(signingSecret, timestamp, signature, d.body, now.Add(time.Hour), 5*time.Minute),
		"no secret":            webhook.Verify("", timestamp, signature, d.body, now, 5*time.Minute),
		"a timestamp of words": webhook.Verify(signingSecret, "yesterday", webhook.Sign(signingSecret, "yesterday", d.body), d.body, now, 5*time.Minute),
	} {
		if ok {
			t.Errorf("%s: the signature verified", name)
		}
	}
}

// TestALargeGroupIsBounded: a payload lists at most fifty alerts, those
// that fire first, and counts the rest.
func TestALargeGroupIsBounded(t *testing.T) {
	r := newReceiver(t)
	var alerts []alert.Alert
	for i := range 60 {
		a := firing()
		a.ID = alertID[:34] + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if i < 20 {
			at := t0
			a.Status, a.ResolvedAt = alert.StatusResolved, &at
		}
		alerts = append(alerts, a)
	}
	if _, err := channelTo(r.URL).Send(context.Background(), message(alerts...)); err != nil {
		t.Fatal(err)
	}
	var p webhook.Payload
	if err := json.Unmarshal(r.deliveries()[0].body, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Alerts) != 50 || p.AlertsOmitted != 10 || p.Group.Counts[alert.StatusFiring] != 40 || p.Group.Counts[alert.StatusResolved] != 20 ||
		p.Alerts[0].Status != alert.StatusFiring || p.Alerts[49].Status != alert.StatusResolved || p.Group.Title != "DiskFull" {
		t.Errorf("%d alerts, %d omitted, counts %v, title %q", len(p.Alerts), p.AlertsOmitted, p.Group.Counts, p.Group.Title)
	}
}

// TestAnswersDecideWhatHappensNext: a success ends it, a redirect is never
// followed, and everything else is tried again, no sooner than the receiver
// asks.
func TestAnswersDecideWhatHappensNext(t *testing.T) {
	tests := []struct {
		status     int
		retryAfter string
		ok         bool
		permanent  bool
		wait       time.Duration
		want       string
	}{
		{status: 200, ok: true},
		{status: 202, ok: true},
		{status: 204, ok: true},
		{status: 500, want: "webhook: the receiver answered HTTP status 500"},
		{status: 503, retryAfter: "120", wait: 2 * time.Minute, want: "webhook: the receiver answered HTTP status 503"},
		{status: 429, retryAfter: "30", wait: 30 * time.Second, want: "webhook: the receiver answered HTTP status 429"},
		// A receiver cannot park a notification for a day.
		{status: 429, retryAfter: "86400", wait: time.Hour, want: "webhook: the receiver answered HTTP status 429"},
		{status: 429, retryAfter: "soon", want: "webhook: the receiver answered HTTP status 429"},
		{status: 400, want: "webhook: the receiver answered HTTP status 400"},
		{status: 401, want: "webhook: the receiver answered HTTP status 401"},
		{status: 404, want: "webhook: the receiver answered HTTP status 404"},
		{status: 410, permanent: true, want: "webhook: the receiver answered HTTP status 410: it is gone for good"},
		{status: 301, permanent: true, want: "webhook: the receiver answered HTTP status 301, a redirect, which Beacon does not follow"},
		{status: 302, permanent: true, want: "a redirect, which Beacon does not follow"},
		{status: 307, permanent: true, want: "a redirect, which Beacon does not follow"},
		{status: 308, permanent: true, want: "a redirect, which Beacon does not follow"},
	}
	for _, tt := range tests {
		r := newReceiver(t)
		r.answers, r.retryAfter = []int{tt.status}, tt.retryAfter
		_, err := channelTo(r.URL).Send(context.Background(), message(firing()))
		permanent, wait := notify.Classify(err)
		switch {
		case tt.ok && err != nil:
			t.Errorf("%d: Send = %v, want success", tt.status, err)
		case !tt.ok && (err == nil || !strings.Contains(err.Error(), tt.want) || permanent != tt.permanent || wait != tt.wait):
			t.Errorf("%d: Send = %v (permanent %v, wait %v), want %q (permanent %v, wait %v)", tt.status, err, permanent, wait, tt.want, tt.permanent, tt.wait)
		}
		// One request, whatever the answer: a redirect is not followed.
		if n := len(r.deliveries()); n != 1 {
			t.Errorf("%d: %d requests, want 1", tt.status, n)
		}
		// Nothing of the address, the headers or the answer is in an error.
		if err != nil {
			for _, secret := range []string{queryToken, "header-token-marker", signingSecret, "you sent", "127.0.0.1", "hooks/beacon"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%d: the error contains %q: %v", tt.status, secret, err)
				}
			}
		}
	}
}

// TestARedirectIsNotFollowed: the request goes to the address in the
// configuration and nowhere else, with its signature and its headers.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := newReceiver(t)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, elsewhere.URL+"/internal/admin", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	_, err := channelTo(redirecting.URL).Send(context.Background(), message(firing()))
	if permanent, _ := notify.Classify(err); err == nil || !permanent {
		t.Errorf("Send = %v, want a failure that is not retried", err)
	}
	if n := len(elsewhere.deliveries()); n != 0 {
		t.Errorf("the redirect was followed: %d requests reached its target", n)
	}
}

// TestLinkLocalAddressesAreRefused: Beacon does not send to the addresses
// where clouds serve instance credentials, whatever name leads to them.
func TestLinkLocalAddressesAreRefused(t *testing.T) {
	for _, url := range []string{"http://169.254.169.254/latest/meta-data", "http://[fe80::1]/", "http://0.0.0.0:9/", "http://224.0.0.1/"} {
		start := time.Now()
		_, err := channelTo(url).Send(context.Background(), message(firing()))
		if permanent, _ := notify.Classify(err); err == nil || !permanent || !strings.Contains(err.Error(), "link-local") || time.Since(start) > 3*time.Second {
			t.Errorf("Send to %s = %v after %v, want it refused at once and for good", url, err, time.Since(start))
		}
	}
	// A private or loopback address is where receivers usually are.
	r := newReceiver(t)
	if _, err := channelTo(r.URL).Send(context.Background(), message(firing())); err != nil {
		t.Errorf("Send to a loopback address: %v", err)
	}
}

func TestFailuresOfTheNetwork(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err := channelTo(closed.URL).Send(context.Background(), message(firing()))
	if permanent, _ := notify.Classify(err); err == nil || permanent || !strings.Contains(err.Error(), "connection refused") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("Send to a closed port = %v, want a failure that is retried and names no address", err)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = channelTo(slow.URL).Send(ctx, message(firing()))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second || strings.Contains(err.Error(), queryToken) {
		t.Errorf("Send to a receiver that does not answer = %v after %v, want the context deadline", err, time.Since(start))
	}

	// A receiver that answers without end is not read without end.
	endless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for req.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer endless.Close()
	start = time.Now()
	if _, err := channelTo(endless.URL).Send(context.Background(), message(firing())); err != nil || time.Since(start) > 5*time.Second {
		t.Errorf("Send to a receiver that answers without end = %v after %v, want success at once", err, time.Since(start))
	}
}

// TestPipelineDeliversEveryChange runs the real pipeline with the webhook
// channel against a real database: a request when the group fires, one when
// it is acknowledged and one when it is resolved, and a failed attempt
// repeated under the same identifier.
func TestPipelineDeliversEveryChange(t *testing.T) {
	pool := postgrestest.NewMigrated(t)
	ctx := context.Background()
	alerts := alert.NewStore(pool)
	source := provisiontest.Row(t, pool, provision.Sources, "test")
	integration := provisiontest.Row(t, pool, provision.Integrations, "tickets")
	policy := provisiontest.Row(t, pool, provision.EscalationPolicies, "default")
	r := newReceiver(t)
	r.answers = []int{503, 500}

	p := notify.New(pool, alerts, slog.New(slog.DiscardHandler), notify.Config{
		PollInterval: 10 * time.Millisecond, MaxAttempts: 4, BackoffBase: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond,
	})
	ch := webhook.NewChannel(config.WebhookIntegration{URL: r.URL, SigningSecret: config.NewSecret(config.SecretRef{}, signingSecret)}, "")
	p.Register(integration, ch)
	p.SetPolicies(map[string]notify.Policy{policy: {Name: "default", Steps: []notify.Step{{
		Targets: []notify.StepTarget{notify.ChannelTarget(integration, "tickets", "tickets")}, Timeout: 1000 * time.Hour,
	}}}})
	alerts.Observe(p.Observe)
	runCtx, cancel := context.WithCancel(ctx)
	var running sync.WaitGroup
	running.Go(func() { p.Run(runCtx) })
	running.Go(func() { p.RunGroups(runCtx, 10*time.Millisecond) })
	t.Cleanup(func() {
		cancel()
		running.Wait()
	})
	settled := func() []string {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var pending int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notifications WHERE status = 'pending') +
				(SELECT count(*) FROM alert_groups WHERE notify_at IS NOT NULL)`).Scan(&pending); err != nil {
				t.Fatal(err)
			}
			if pending == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("notifications are still pending")
			}
			time.Sleep(10 * time.Millisecond)
		}
		rows, _ := pool.Query(ctx, `SELECT outcome || COALESCE(': ' || error, '') FROM notification_attempts ORDER BY id`)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	a, _, err := alerts.Fire(ctx, alert.Firing{
		SourceID: source, Fingerprint: "fp", Severity: alert.SeverityCritical,
		Labels: map[string]string{"alertname": "DiskFull"}, Annotations: map[string]string{"summary": "Disk is full"},
		Group: alert.Grouping{Key: "fp", EscalationPolicyID: policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"retryable_error: webhook: the receiver answered HTTP status 503", "retryable_error: webhook: the receiver answered HTTP status 500", "success"}
	if got := settled(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("attempts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if _, err := alerts.Apply(ctx, a.ID, alert.ActionAcknowledge, alert.ChatUser("slack", "U1", "alice"), alert.Options{}); err != nil {
		t.Fatal(err)
	}
	settled()
	if _, _, err := alerts.ResolveFromSource(ctx, source, "fp", time.Time{}); err != nil {
		t.Fatal(err)
	}
	settled()

	var seen []string
	ids := map[string]int{}
	for _, d := range r.deliveries() {
		var p webhook.Payload
		if err := json.Unmarshal(d.body, &p); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if p.ID == "" || p.ID != d.header.Get("Idempotency-Key") || p.Group.ID != a.GroupID || p.Version != "1" {
			t.Errorf("payload id %q, header %q, group %q, version %q", p.ID, d.header.Get("Idempotency-Key"), p.Group.ID, p.Version)
		}
		if !webhook.Verify(signingSecret, d.header.Get("X-Beacon-Timestamp"), d.header.Get("X-Beacon-Signature"), d.body, time.Now(), time.Minute) {
			t.Errorf("a request is not signed for the time it was sent at")
		}
		ids[p.ID]++
		seen = append(seen, string(p.Reason)+" "+string(p.Group.Status))
	}
	wantSeen := "escalation_step firing, escalation_step firing, escalation_step firing, group_update acknowledged, resolved resolved"
	if got := strings.Join(seen, ", "); got != wantSeen {
		t.Errorf("requests: %s\nwant:     %s", got, wantSeen)
	}
	// Three notifications, the first of them tried three times under one
	// identifier.
	if len(ids) != 3 {
		t.Errorf("identifiers: %v, want three, one of them three times", ids)
	}
}
