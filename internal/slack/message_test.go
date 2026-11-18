package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func firingAlert() alert.Alert {
	return alert.Alert{
		ID:          "alert-1",
		Fingerprint: "fp",
		Status:      alert.StatusFiring,
		Severity:    alert.SeverityCritical,
		Labels:      map[string]string{"alertname": "DiskFull", "host": "db1", "severity": "critical"},
		Annotations: map[string]string{
			"summary":     "Disk is full on db1",
			"description": "/var/lib/postgresql has 2% free",
			"runbook_url": "https://runbooks.example.com/disk?a=1&b=2",
		},
		StartedAt: t0,
	}
}

func event(typ alert.EventType, actor alert.Actor) alert.Event {
	return alert.Event{AlertID: "alert-1", Type: typ, Actor: actor, OccurredAt: t0.Add(time.Minute)}
}

// rendered returns the message as the JSON that would be sent.
func rendered(t *testing.T, m notify.Message) (message, string) {
	t.Helper()
	msg := render(m)
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	for _, blk := range msg.Blocks {
		if blk.Text != nil && len(blk.Text.Text) > 3000 {
			t.Errorf("a section has %d characters, more than Slack's 3000", len(blk.Text.Text))
		}
	}
	// encoding/json writes < > & as escapes; undo that to read the text.
	return msg, strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(b))
}

func TestRenderFiring(t *testing.T) {
	msg, out := rendered(t, notify.Message{Alerts: []alert.Alert{firingAlert()}, Events: []alert.Event{event(alert.EventFired, alert.Source("s"))}})

	if msg.Text != "[FIRING] critical: Disk is full on db1" {
		t.Errorf("fallback text = %q", msg.Text)
	}
	for _, want := range []string{
		":red_circle: *Firing* · critical",
		"*Disk is full on db1*",
		"/var/lib/postgresql has 2% free",
		"<https://runbooks.example.com/disk?a=1&amp;b=2|:book: Runbook>",
		"`alertname=DiskFull` `host=db1` `severity=critical`",
		"Firing since <!date^1772366400^{date_short_pretty} {time}|2026-03-01 12:00 UTC>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("message does not contain %q:\n%s", want, out)
		}
	}
	if len(msg.Blocks) != 4 || msg.Blocks[0].Type != "section" || msg.Blocks[1].Type != "section" || msg.Blocks[2].Type != "context" || msg.Blocks[3].Type != "actions" {
		t.Errorf("blocks = %+v", msg.Blocks)
	}
}

func TestRenderStates(t *testing.T) {
	alice := alert.ChatUser("slack", "U1", "alice")
	ack, snoozed, resolved := t0.Add(time.Minute), t0.Add(time.Hour), t0.Add(2*time.Minute)
	tests := []struct {
		name   string
		change func(*alert.Alert)
		events []alert.Event
		want   []string
	}{
		{
			name:   "acknowledged",
			change: func(a *alert.Alert) { a.Status, a.AcknowledgedAt = alert.StatusAcknowledged, &ack },
			events: []alert.Event{event(alert.EventFired, alert.Source("s")), event(alert.EventAcknowledged, alice)},
			want:   []string{":large_yellow_circle: *Acknowledged* · critical", "Acknowledged by alice <!date^1772366460^", "[ACKNOWLEDGED] critical: Disk is full on db1"},
		},
		{
			name:   "snoozed",
			change: func(a *alert.Alert) { a.Status, a.SnoozedUntil = alert.StatusSnoozed, &snoozed },
			events: []alert.Event{event(alert.EventFired, alert.Source("s")), event(alert.EventSnoozed, alice)},
			want:   []string{":zzz: *Snoozed* · critical", "Snoozed by alice until <!date^1772370000^"},
		},
		{
			name:   "firing again after a snooze",
			change: func(*alert.Alert) {},
			events: []alert.Event{event(alert.EventSnoozed, alice), event(alert.EventSnoozeEnded, alert.System())},
			want:   []string{":red_circle: *Firing*", "Firing again: the snooze ended <!date^1772366460^"},
		},
		{
			name:   "resolved by the source",
			change: func(a *alert.Alert) { a.Status, a.ResolvedAt = alert.StatusResolved, &resolved },
			events: []alert.Event{event(alert.EventFired, alert.Source("s")), event(alert.EventResolved, alert.Source("s"))},
			want:   []string{":large_green_circle: *Resolved* · critical", "Resolved by the source <!date^1772366520^"},
		},
		{
			name:   "resolved by a person",
			change: func(a *alert.Alert) { a.Status, a.ResolvedAt = alert.StatusResolved, &resolved },
			events: []alert.Event{event(alert.EventAcknowledged, alert.ChatUser("slack", "U2", "bob")), event(alert.EventResolved, alice)},
			want:   []string{"Resolved by alice <!date"},
		},
		{
			name:   "actor without a name",
			change: func(a *alert.Alert) { a.Status, a.AcknowledgedAt = alert.StatusAcknowledged, &ack },
			events: []alert.Event{event(alert.EventAcknowledged, alert.ChatUser("slack", "U1", ""))},
			want:   []string{"Acknowledged by someone <!date"},
		},
		{
			name:   "events of other alerts and notifications are ignored",
			change: func(a *alert.Alert) { a.Status, a.AcknowledgedAt = alert.StatusAcknowledged, &ack },
			events: []alert.Event{
				event(alert.EventAcknowledged, alice),
				{AlertID: "another", Type: alert.EventResolved, Actor: alert.ChatUser("slack", "U9", "mallory")},
				{Type: notify.EventSent, Actor: alert.System()},
			},
			want: []string{"Acknowledged by alice"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := firingAlert()
			tt.change(&a)
			_, out := rendered(t, notify.Message{Alerts: []alert.Alert{a}, Events: tt.events})
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("message does not contain %q:\n%s", want, out)
				}
			}
		})
	}
}

func TestRenderWithoutOptionalParts(t *testing.T) {
	a := firingAlert()
	a.Annotations = nil
	a.Severity = alert.SeverityWarning
	msg, out := rendered(t, notify.Message{Alerts: []alert.Alert{a}})
	if msg.Text != "[FIRING] warning: DiskFull" {
		t.Errorf("fallback text = %q, want the alert name when there is no summary", msg.Text)
	}
	if strings.Contains(out, "Runbook") {
		t.Errorf("message has a runbook link without the annotation:\n%s", out)
	}

	a.Labels = map[string]string{"host": "db1"}
	if msg, _ := rendered(t, notify.Message{Alerts: []alert.Alert{a}}); msg.Text != "[FIRING] warning: Alert fp" {
		t.Errorf("fallback text = %q, want the fingerprint as a last resort", msg.Text)
	}

	a.Labels = nil
	if msg, _ := rendered(t, notify.Message{Alerts: []alert.Alert{a}}); len(msg.Blocks) != 3 {
		t.Errorf("an alert without labels has %d blocks, want no empty labels block", len(msg.Blocks))
	}

	if msg, _ := rendered(t, notify.Message{}); msg.Text == "" || len(msg.Blocks) == 0 {
		t.Errorf("a message without alerts is empty: %+v", msg)
	}

}

// TestRenderEscapesAlertContent checks that nothing from an alert can add
// Slack markup: links, mentions, or an early end of the label formatting.
func TestRenderEscapesAlertContent(t *testing.T) {
	a := firingAlert()
	a.Annotations["summary"] = "<!channel> disk & <https://evil.example|click>"
	a.Annotations["description"] = "<@U123> look > here"
	a.Labels = map[string]string{"alertname": "<!here>", "note": "a`b <x>"}
	a.Status = alert.StatusAcknowledged
	events := []alert.Event{event(alert.EventAcknowledged, alert.ChatUser("slack", "U1", "<!everyone>"))}
	msg, out := rendered(t, notify.Message{Alerts: []alert.Alert{a}, Events: events})

	for _, bad := range []string{"<!channel>", "<!here>", "<!everyone>", "<@U123>", "<https://evil.example", "<x>", "a`b"} {
		if strings.Contains(out, bad) || strings.Contains(msg.Text, bad) {
			t.Errorf("message contains unescaped %q:\n%s", bad, out)
		}
	}
	for _, want := range []string{"&lt;!channel&gt; disk &amp; &lt;https://evil.example|click&gt;", "&lt;@U123&gt; look &gt; here", "`alertname=&lt;!here&gt;`", "`note=a'b &lt;x&gt;`", "by &lt;!everyone&gt;"} {
		if !strings.Contains(out, want) {
			t.Errorf("message does not contain %q:\n%s", want, out)
		}
	}
}

func TestRunbookLink(t *testing.T) {
	for _, raw := range []string{
		"", "runbook", "javascript:alert(1)", "ftp://example.com/x", "mailto:a@example.com", "https://",
		"https://example.com/a|b", "https://example.com/<x>", "https://example.com/a b", "http://example.com/\nx",
		"https://example.com/" + strings.Repeat("x", 500),
	} {
		if got := runbookLink(raw); got != "" {
			t.Errorf("runbookLink(%q) = %q, want no link", raw, got)
		}
	}
	if got, want := runbookLink("http://wiki.internal/runbooks/disk"), "<http://wiki.internal/runbooks/disk|:book: Runbook>"; got != want {
		t.Errorf("runbookLink = %q, want %q", got, want)
	}
}

func TestRenderStaysWithinSlackLimits(t *testing.T) {
	a := firingAlert()
	a.Annotations["summary"] = strings.Repeat("&", 1000)
	a.Annotations["description"] = strings.Repeat("é<", 4000)
	a.Annotations["runbook_url"] = "https://example.com/" + strings.Repeat("r", 470)
	a.Labels = map[string]string{"huge": strings.Repeat(">", 5000)}
	for i := range 200 {
		a.Labels[fmt.Sprintf("label_%03d", i)] = strings.Repeat("v", 30)
	}
	msg, out := rendered(t, notify.Message{Alerts: []alert.Alert{a}})

	if n := len(msg.Blocks[0].Text.Text); n > 3000 || n < 1500 {
		t.Errorf("the first section has %d characters", n)
	}
	labels := msg.Blocks[1].Text.Text
	if len(labels) > maxLabels || !strings.HasSuffix(labels, " …") || strings.Count(labels, "`")%2 != 0 {
		t.Errorf("labels line has %d characters and %d backticks: …%s", len(labels), strings.Count(labels, "`"), labels[len(labels)-40:])
	}
	// Clipping never leaves half an escape sequence or half a character.
	for _, bad := range []string{"&am…", "&a…", "&…", "&l…", "&g…", "\ufffd"} {
		if strings.Contains(out, bad) {
			t.Errorf("message contains a cut escape sequence %q", bad)
		}
	}
	if !strings.Contains(out, "|:book: Runbook>") {
		t.Errorf("the runbook link was lost")
	}
}

func TestClip(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"this is too long", 10, "this is…"},
		{"aaaa&amp;bbbb", 10, "aaaa…"},
		{"ééééééé", 10, "ééé…"},
	}
	for _, tt := range tests {
		if got := clip(tt.in, tt.n); got != tt.want || len(got) > tt.n {
			t.Errorf("clip(%q, %d) = %q (%d bytes), want %q", tt.in, tt.n, got, len(got), tt.want)
		}
	}
}

// TestButtonsFollowTheState checks that a message offers exactly the actions
// its alert's state allows, each carrying the group to act on.
func TestButtonsFollowTheState(t *testing.T) {
	tests := []struct {
		status alert.Status
		want   []string
	}{
		{alert.StatusFiring, []string{"beacon_acknowledge", "beacon_snooze", "beacon_resolve"}},
		{alert.StatusAcknowledged, []string{"beacon_snooze", "beacon_resolve"}},
		{alert.StatusSnoozed, []string{"beacon_acknowledge", "beacon_resolve"}},
		{alert.StatusResolved, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			a := firingAlert()
			a.Status, a.GroupID = tt.status, "0f8b1c1e-6a0e-4b0e-9a57-0d7f6d4f3c11"
			msg, _ := rendered(t, notify.Message{Alerts: []alert.Alert{a}})
			last := msg.Blocks[len(msg.Blocks)-1]
			if tt.want == nil {
				if last.Type == "actions" {
					t.Fatalf("a %s alert has buttons: %+v", tt.status, last.Elements)
				}
				return
			}
			if last.Type != "actions" {
				t.Fatalf("the last block is %q, want the buttons", last.Type)
			}
			var got []string
			for _, e := range last.Elements {
				b := e.(button)
				got = append(got, b.ActionID)
				if b.Type != "button" || b.Text.Type != "plain_text" || b.Text.Text == "" || b.Value != a.GroupID {
					t.Errorf("button = %+v", b)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("buttons = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRenderLinks(t *testing.T) {
	a := firingAlert()
	a.Links = map[string]string{
		alert.LinkGenerator: "http://prometheus.example.com:9090/graph?g0.expr=up+%3C+1&g0.tab=1",
		alert.LinkDashboard: "https://grafana.example.com/d/abc",
		alert.LinkPanel:     "https://grafana.example.com/d/abc?viewPanel=2",
		alert.LinkSilence:   "javascript:alert(1)",
		"other":             "https://example.com/not-shown",
	}
	msg, out := rendered(t, notify.Message{Alerts: []alert.Alert{a}})
	want := "<http://prometheus.example.com:9090/graph?g0.expr=up+%3C+1&amp;g0.tab=1|:mag: Source> · " +
		"<https://grafana.example.com/d/abc|:bar_chart: Dashboard> · " +
		"<https://grafana.example.com/d/abc?viewPanel=2|:chart_with_upwards_trend: Panel>"
	if len(msg.Blocks) != 5 || msg.Blocks[2].Type != "section" || msg.Blocks[2].Text.Text != want {
		t.Errorf("links block = %+v, want %s", msg.Blocks[2], want)
	}
	if strings.Contains(out, "javascript") || strings.Contains(out, "not-shown") {
		t.Errorf("the message has a link it should not:\n%s", out)
	}

	// Four links of the greatest length shown still fit in a section, and a
	// longer one is left out rather than cut.
	long := "https://grafana.example.com/" + strings.Repeat("d", maxLinkURL-len("https://grafana.example.com/"))
	a.Links = map[string]string{alert.LinkGenerator: long, alert.LinkDashboard: long, alert.LinkPanel: long, alert.LinkSilence: long + "x"}
	msg, _ = rendered(t, notify.Message{Alerts: []alert.Alert{a}})
	if got := strings.Count(msg.Blocks[2].Text.Text, long); got != 3 || strings.Contains(msg.Blocks[2].Text.Text, "Silence") {
		t.Errorf("links block has %d long links, want the three that fit: %s", got, msg.Blocks[2].Text.Text)
	}
}

// groupOf builds a group message from alerts given as "host status severity".
func groupOf(specs ...string) notify.Message {
	m := notify.Message{Group: alert.Group{ID: "group-1", Labels: map[string]string{"alertname": "DiskFull", "cluster": "eu"}}}
	for i, spec := range specs {
		f := strings.Fields(spec)
		a := alert.Alert{
			ID: fmt.Sprintf("alert-%d", i), GroupID: "group-1", Fingerprint: f[0],
			Status: alert.Status(f[1]), Severity: alert.Severity(f[2]),
			Labels:      map[string]string{"alertname": "DiskFull", "cluster": "eu", "host": f[0]},
			Annotations: map[string]string{"summary": "Disk is full on " + f[0], "description": "only shown for one alert"},
			StartedAt:   t0.Add(time.Duration(i) * time.Minute),
		}
		at := t0.Add(time.Hour + time.Duration(i)*time.Minute)
		switch a.Status {
		case alert.StatusAcknowledged:
			a.AcknowledgedAt = &at
		case alert.StatusSnoozed:
			a.SnoozedUntil = &at
		case alert.StatusResolved:
			a.ResolvedAt = &at
		}
		m.Alerts = append(m.Alerts, a)
	}
	return m
}

func TestRenderGroup(t *testing.T) {
	m := groupOf("db1 resolved critical", "db2 firing warning", "db3 acknowledged critical", "db4 firing info")
	m.Events = []alert.Event{{AlertID: "alert-2", Type: alert.EventAcknowledged, Actor: alert.ChatUser("slack", "U1", "alice")}}
	msg, out := rendered(t, m)

	if msg.Text != "[FIRING] critical: DiskFull · eu (4 alerts)" {
		t.Errorf("fallback text = %q", msg.Text)
	}
	for _, want := range []string{
		// Firing while any alert fires, with the worst severity that is open.
		":red_circle: *Firing* · critical\\n*DiskFull · eu*\\n4 alerts: 2 firing, 1 acknowledged, 1 resolved",
		// What the alerts share.
		`"text":"` + "`alertname=DiskFull` `cluster=eu`" + `"`,
		// Each alert, those that fire first, with what tells it apart.
		":red_circle: Disk is full on db2 `host=db2`\\n:red_circle: Disk is full on db4 `host=db4`\\n" +
			":large_yellow_circle: Disk is full on db3 `host=db3`\\n:large_green_circle: Disk is full on db1 `host=db1`",
		// The alert that has fired longest.
		"Firing since <!date^1772366460^",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("message does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "only shown for one alert") {
		t.Errorf("a group shows the description of one of its alerts:\n%s", out)
	}
	if len(msg.Blocks) != 5 || msg.Blocks[4].Type != "actions" || len(msg.Blocks[4].Elements) != 3 {
		t.Fatalf("blocks = %+v, want head, labels, list, context and the three buttons", msg.Blocks)
	}
	for _, e := range msg.Blocks[4].Elements {
		if b := e.(button); b.Value != "group-1" {
			t.Errorf("button %s acts on %q, want the group", b.ActionID, b.Value)
		}
	}
}

func TestRenderGroupStates(t *testing.T) {
	alice := alert.ChatUser("slack", "U1", "alice")
	tests := []struct {
		name    string
		alerts  []string
		event   alert.Event
		want    []string
		buttons []string
	}{
		{
			name:    "acknowledged once nothing fires",
			alerts:  []string{"db1 acknowledged warning", "db2 acknowledged critical", "db3 resolved critical", "db4 snoozed info"},
			event:   alert.Event{AlertID: "alert-1", Type: alert.EventAcknowledged, Actor: alice},
			want:    []string{":large_yellow_circle: *Acknowledged* · critical", "4 alerts: 2 acknowledged, 1 snoozed, 1 resolved", "Acknowledged by alice"},
			buttons: []string{actionAcknowledge, actionSnooze, actionResolve},
		},
		{
			name:    "snoozed when the rest is resolved",
			alerts:  []string{"db1 snoozed warning", "db2 resolved critical"},
			event:   alert.Event{AlertID: "alert-0", Type: alert.EventSnoozed, Actor: alice},
			want:    []string{":zzz: *Snoozed* · warning", "2 alerts: 1 snoozed, 1 resolved", "Snoozed by alice until"},
			buttons: []string{actionAcknowledge, actionResolve},
		},
		{
			name:   "resolved when all are",
			alerts: []string{"db1 resolved warning", "db2 resolved critical"},
			event:  alert.Event{AlertID: "alert-1", Type: alert.EventResolved, Actor: alert.Source("s")},
			want:   []string{":large_green_circle: *Resolved* · critical", "2 alerts: 2 resolved", "Resolved by the source"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := groupOf(tt.alerts...)
			m.Events = []alert.Event{tt.event}
			msg, out := rendered(t, m)
			for _, want := range tt.want {
				if !strings.Contains(out, want) {
					t.Errorf("message does not contain %q:\n%s", want, out)
				}
			}
			var got []string
			if last := msg.Blocks[len(msg.Blocks)-1]; last.Type == "actions" {
				for _, e := range last.Elements {
					got = append(got, e.(button).ActionID)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.buttons) {
				t.Errorf("buttons = %v, want %v", got, tt.buttons)
			}
		})
	}
}

// TestRenderLargeGroup checks the bounds: a group of many alerts with long
// and hostile content still fits in Slack's limits, and says what it left
// out.
func TestRenderLargeGroup(t *testing.T) {
	var specs []string
	for i := range 40 {
		specs = append(specs, fmt.Sprintf("db%d firing warning", i))
	}
	m := groupOf(specs...)
	for i := range m.Alerts {
		m.Alerts[i].Annotations["summary"] = "<!channel> " + strings.Repeat("é", 300)
		m.Alerts[i].Labels["note"] = strings.Repeat("`<x>", 200)
	}
	m.Group.Labels = map[string]string{"alertname": strings.Repeat("N", 500), "cluster": "<!here>"}
	msg, out := rendered(t, m) // rendered fails the test if a section is too long
	if !strings.Contains(out, "… and 30 more") {
		t.Errorf("the message does not count the alerts it left out:\n%s", out)
	}
	if got := strings.Count(msg.Blocks[2].Text.Text, ":red_circle:"); got != 10 {
		t.Errorf("%d alerts are listed, want 10", got)
	}
	for _, bad := range []string{"<!channel>", "<!here>", "<x>"} {
		if strings.Contains(out, bad) {
			t.Errorf("message contains unescaped %q", bad)
		}
	}
	if len(msg.Text) > 400 {
		t.Errorf("fallback text has %d bytes", len(msg.Text))
	}

	// A group whose alerts have no summary and no label of their own still
	// lists something for each.
	bare := groupOf("db1 firing info", "db2 firing info")
	for i := range bare.Alerts {
		bare.Alerts[i].Annotations = nil
		bare.Alerts[i].Labels = map[string]string{"alertname": "DiskFull", "cluster": "eu"}
	}
	bare.Group.Labels = nil
	msg, _ = rendered(t, bare)
	if msg.Text != "[FIRING] info: Alerts (2 alerts)" || !strings.Contains(msg.Blocks[1].Text.Text, "`alertname=DiskFull` `cluster=eu`") {
		t.Errorf("bare group: text %q, blocks %+v", msg.Text, msg.Blocks)
	}
}
