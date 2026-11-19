package discord

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func firingAlert() alert.Alert {
	return alert.Alert{
		ID: "alert-1", GroupID: "3f0e9a0c-6f6e-4a53-9a5e-0d6a4b0c2a11", Fingerprint: "fp",
		Status: alert.StatusFiring, Severity: alert.SeverityCritical,
		Labels: map[string]string{"alertname": "DiskFull", "host": "db1", "severity": "critical"},
		Annotations: map[string]string{
			"summary":     "Disk is full on db1",
			"description": "/var/lib/postgresql has 2% free",
			"runbook_url": "https://runbooks.example.com/disk?a=1&b=(2)",
		},
		Links:     map[string]string{alert.LinkGenerator: "https://prometheus.example.com/graph?g0.expr=up"},
		StartedAt: t0,
	}
}

func event(typ alert.EventType, actor alert.Actor) alert.Event {
	return alert.Event{AlertID: "alert-1", Type: typ, Actor: actor, OccurredAt: t0.Add(time.Minute)}
}

// rendered returns the message and the JSON that would be sent, and checks
// the message against Discord's limits.
func rendered(t *testing.T, m notify.Message) (message, string) {
	t.Helper()
	msg := render(m)
	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	if n := utf8.RuneCountInString(msg.Content); n > 2000 {
		t.Errorf("the content has %d characters, more than Discord's 2000", n)
	}
	for _, e := range msg.Embeds {
		total := len(e.Description)
		if len(e.Description) > 4096 {
			t.Errorf("a description has %d bytes, more than Discord's 4096", len(e.Description))
		}
		for _, f := range e.Fields {
			total += len(f.Name) + len(f.Value)
			if f.Value == "" || len(f.Value) > 1024 {
				t.Errorf("the field %s has %d bytes, want between 1 and 1024", f.Name, len(f.Value))
			}
		}
		if total > 6000 {
			t.Errorf("the embed has %d bytes, more than Discord's 6000", total)
		}
	}
	// encoding/json writes < > & as escapes; undo that to read the text.
	return msg, strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(string(b))
}

func TestRenderFiring(t *testing.T) {
	msg, body := rendered(t, notify.Message{Alerts: []alert.Alert{firingAlert()}})
	if want := "🔴 **Firing** · critical\n**Disk is full on db1**"; msg.Content != want {
		t.Errorf("content = %q, want %q", msg.Content, want)
	}
	for _, want := range []string{
		"/var/lib/postgresql has 2% free",
		// A parenthesis in an address would end the link early.
		"[📖 Runbook](https://runbooks.example.com/disk?a=1&b=%282%29)",
		"[🔍 Source](https://prometheus.example.com/graph?g0.expr=up)",
		"`alertname=DiskFull` `host=db1` `severity=critical`",
		fmt.Sprintf("Firing since <t:%d:f>", t0.Unix()),
		// Nothing in a message may notify anyone by a mention.
		`"allowed_mentions":{"parse":[]}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the message does not contain %q:\n%s", want, body)
		}
	}
	if msg.Embeds[0].Color != 0xD92D20 {
		t.Errorf("color = %#x", msg.Embeds[0].Color)
	}
}

func TestRenderStates(t *testing.T) {
	ada := alert.LinkedChatUser("u1", "discord", "300000000000000001", "Ada Lovelace")
	at := t0.Add(time.Minute)
	tests := []struct {
		status alert.Status
		set    func(*alert.Alert)
		events []alert.Event
		want   []string
	}{
		{alert.StatusAcknowledged, func(a *alert.Alert) { a.AcknowledgedAt = &at }, []alert.Event{event(alert.EventAcknowledged, ada)},
			[]string{"🟡 **Acknowledged**", fmt.Sprintf("Acknowledged by Ada Lovelace <t:%d:f>", at.Unix())}},
		{alert.StatusSnoozed, func(a *alert.Alert) { a.SnoozedUntil = &at }, []alert.Event{event(alert.EventSnoozed, ada)},
			[]string{"💤 **Snoozed**", fmt.Sprintf("Snoozed by Ada Lovelace until <t:%d:f>", at.Unix())}},
		{alert.StatusResolved, func(a *alert.Alert) { a.ResolvedAt = &at }, []alert.Event{event(alert.EventResolved, alert.Source("s"))},
			[]string{"🟢 **Resolved**", "Resolved by the source"}},
		{alert.StatusFiring, func(*alert.Alert) {}, []alert.Event{event(alert.EventSnoozeEnded, alert.System())},
			[]string{"🔴 **Firing**", "Firing again: the snooze ended"}},
	}
	for _, tt := range tests {
		a := firingAlert()
		a.Status = tt.status
		tt.set(&a)
		_, body := rendered(t, notify.Message{Alerts: []alert.Alert{a}, Events: tt.events})
		for _, want := range tt.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the message does not contain %q:\n%s", tt.status, want, body)
			}
		}
	}
}

// TestRenderEscapesAlertContent: nothing an alert says is read as markup, a
// link or a mention.
func TestRenderEscapesAlertContent(t *testing.T) {
	a := firingAlert()
	a.Annotations["summary"] = "**bold** <@300000000000000001> @everyone [click](https://evil.example.com)\n# heading"
	a.Annotations["description"] = "~~gone~~ ||spoiler|| `code` <#100000000000000001> <t:0:R> > quote"
	a.Annotations["runbook_url"] = "javascript:alert(1)"
	a.Links = map[string]string{alert.LinkGenerator: "https://ok.example.com/a b", alert.LinkDashboard: "ftp://files.example.com/x"}
	a.Labels = map[string]string{"alertname": "Disk`Full`", "note": "line one\nline two"}
	at := t0.Add(time.Minute)
	a.Status, a.ResolvedAt = alert.StatusResolved, &at
	msg, body := rendered(t, notify.Message{Alerts: []alert.Alert{a}, Events: []alert.Event{
		event(alert.EventResolved, alert.ChatUser("discord", "300000000000000009", "<@300000000000000001> **Gus**\n# big")),
	}})
	if want := `**\*\*bold\*\* \<@300000000000000001\> @everyone \[click\]\(https://evil.example.com\) \# heading**`; !strings.Contains(msg.Content, want) {
		t.Errorf("content = %q, want it to contain %q", msg.Content, want)
	}
	for _, want := range []string{
		`\~\~gone\~\~ \|\|spoiler\|\| \` + "`code\\`" + ` \<\#100000000000000001\> \<t:0:R\> \> quote`,
		"`alertname=Disk'Full'` `note=line one line two`",
		`Resolved by \<@300000000000000001\> \*\*Gus\*\* \# big`,
	} {
		if !strings.Contains(msg.Embeds[0].Description+" "+fieldsOf(msg), want) {
			t.Errorf("the embed does not contain %q:\n%s", want, body)
		}
	}
	// No link is made from an address that is not a plain http or https one.
	for _, bad := range []string{"](", "javascript", "ftp://", "a b"} {
		if strings.Contains(msg.Embeds[0].Description, bad) {
			t.Errorf("the description contains %q:\n%s", bad, msg.Embeds[0].Description)
		}
	}
}

func fieldsOf(m message) string {
	var parts []string
	for _, e := range m.Embeds {
		for _, f := range e.Fields {
			parts = append(parts, f.Name+": "+f.Value)
		}
	}
	return strings.Join(parts, "\n")
}

// TestRenderStaysWithinDiscordLimits: an alert with very long values, and a
// very large group, still make a message that Discord accepts.
func TestRenderStaysWithinDiscordLimits(t *testing.T) {
	long := strings.Repeat("é*<", 3000)
	a := firingAlert()
	a.Annotations = map[string]string{"summary": long, "description": long, "runbook_url": "https://example.com/" + strings.Repeat("a", 5000)}
	a.Labels = map[string]string{}
	for i := range 60 {
		a.Labels[fmt.Sprintf("label%02d", i)] = long
	}
	a.Links = map[string]string{alert.LinkGenerator: "https://example.com/" + strings.Repeat("b", 440), alert.LinkDashboard: "https://example.com/" + strings.Repeat("c", 430),
		alert.LinkPanel: "https://example.com/" + strings.Repeat("d", 430), alert.LinkSilence: "https://example.com/" + strings.Repeat("e", 430)}
	var steps []any
	for i := range 40 {
		steps = append(steps, map[string]any{"user": fmt.Sprintf("p%d@example.com", i), "name": long, "not_connected": "Discord"})
	}
	events := []alert.Event{{Type: notify.EventEscalated, Data: map[string]any{"skipped": steps}},
		event(alert.EventAcknowledged, alert.ChatUser("discord", "1", long))}
	at := t0
	a.Status, a.AcknowledgedAt = alert.StatusAcknowledged, &at
	msg, _ := rendered(t, notify.Message{Alerts: []alert.Alert{a}, Events: events})
	if !strings.HasSuffix(msg.Content, "…**") || strings.Contains(msg.Content, `\…`) {
		t.Errorf("the title is not clipped cleanly: %q", msg.Content[len(msg.Content)-20:])
	}
	if paged := msg.Embeds[0].Fields[len(msg.Embeds[0].Fields)-1]; paged.Name != "Paged" || !strings.Contains(paged.Value, "more") {
		t.Errorf("the paged line does not count who did not fit: %+v", paged)
	}

	var many []alert.Alert
	for i := range 200 {
		b := firingAlert()
		b.ID = fmt.Sprintf("a%d", i)
		b.Annotations = map[string]string{"summary": long}
		b.Labels = map[string]string{"alertname": long, "host": long + fmt.Sprint(i)}
		many = append(many, b)
	}
	msg, body := rendered(t, notify.Message{Alerts: many, Group: alert.Group{Labels: map[string]string{"alertname": long}}})
	if !strings.Contains(body, "200 alerts: 200 firing") || !strings.Contains(body, "… and 190 more") || !strings.Contains(msg.Content, "(200 alerts)") {
		t.Errorf("the group message does not count its alerts:\n%.600s", body)
	}
}

func TestClip(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"a long sentence", 10, "a long …"},
		// Half a character is not left behind.
		{"ééééé", 6, "é…"},
		// Nor is the backslash of an escape whose character was cut off.
		{`aaaa\*bbbb`, 8, `aaaa…`},
		{`aaa\\\*bb`, 8, `aaa\\…`},
	}
	for _, tt := range tests {
		if got := clip(tt.in, tt.n); got != tt.want || len(got) > tt.n {
			t.Errorf("clip(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

// TestRenderGroup: a group of several alerts shows what they share, counts
// them and lists them.
func TestRenderGroup(t *testing.T) {
	a, b, c := firingAlert(), firingAlert(), firingAlert()
	at := t0.Add(time.Minute)
	b.ID, b.Labels, b.Annotations = "alert-2", map[string]string{"alertname": "DiskFull", "host": "db2"}, map[string]string{"summary": "Disk is full on db2"}
	b.Status, b.AcknowledgedAt = alert.StatusAcknowledged, &at
	c.ID, c.Labels, c.Annotations, c.Severity = "alert-3", map[string]string{"alertname": "DiskFull", "host": "db3"}, nil, alert.SeverityWarning
	c.Status, c.ResolvedAt = alert.StatusResolved, &at
	msg, body := rendered(t, notify.Message{Alerts: []alert.Alert{c, b, a}, Group: alert.Group{Labels: map[string]string{"alertname": "DiskFull"}}})
	if want := "🔴 **Firing** · critical\n**DiskFull** (3 alerts)"; msg.Content != want {
		t.Errorf("content = %q, want %q", msg.Content, want)
	}
	want := "3 alerts: 1 firing, 1 acknowledged, 1 resolved\n" +
		"🔴 Disk is full on db1 `host=db1` `severity=critical`\n" +
		"🟡 Disk is full on db2 `host=db2`\n" +
		"🟢 `host=db3`"
	if msg.Embeds[0].Description != want {
		t.Errorf("description:\n%s\nwant:\n%s\n%s", msg.Embeds[0].Description, want, body)
	}
	if got := fieldsOf(msg); !strings.Contains(got, "Labels: `alertname=DiskFull`") {
		t.Errorf("fields = %s", got)
	}
}

// TestPagedLine: people who were paged through this integration are named
// by a mention, which inside an embed notifies nobody; people without a
// linked account stay visible by name.
func TestPagedLine(t *testing.T) {
	m := notify.Message{
		Notification: notify.Notification{IntegrationID: "int-discord"},
		Alerts:       []alert.Alert{firingAlert()},
		Events: []alert.Event{{Type: notify.EventEscalated, Data: map[string]any{
			"notified": []any{
				map[string]any{"user": "ada@example.com", "name": "Ada Lovelace", "integration_id": "int-discord", "destination": "300000000000000001"},
				map[string]any{"user": "ken@example.com", "name": "Ken", "integration_id": "int-slack", "destination": "U0KEN"},
				map[string]any{"target": "channel 100000000000000001", "integration_id": "int-discord", "destination": "100000000000000001"},
			},
			"skipped": []any{
				map[string]any{"user": "grace@example.com", "name": "Grace *Hopper*", "not_connected": "Discord", "shown_as": "Grace *Hopper* (Discord not connected)"},
			},
		}}},
	}
	msg, _ := rendered(t, m)
	if got, want := fieldsOf(msg), `Paged: <@300000000000000001>, Ken, Grace \*Hopper\* \(Discord not connected\)`; !strings.Contains(got, want) {
		t.Errorf("fields = %s\nwant %s", got, want)
	}
	// A group that has only told channels has no such line.
	msg, _ = rendered(t, notify.Message{Alerts: []alert.Alert{firingAlert()}})
	if strings.Contains(fieldsOf(msg), "Paged") {
		t.Errorf("fields = %s", fieldsOf(msg))
	}
}

// TestButtonsFollowTheState: a message offers what the state machine allows
// for at least one alert of the group, and nothing once all are resolved.
func TestButtonsFollowTheState(t *testing.T) {
	ids := func(m message) string {
		var out []string
		for _, row := range m.Components {
			for _, b := range row.Components {
				if b.Type != 2 || b.Label == "" {
					t.Errorf("button = %+v", b)
				}
				out = append(out, b.CustomID)
			}
		}
		return strings.Join(out, " ")
	}
	group := firingAlert().GroupID
	at := t0
	a := firingAlert()
	tests := []struct {
		status alert.Status
		want   string
	}{
		{alert.StatusFiring, "beacon_acknowledge:" + group + " beacon_snooze:" + group + " beacon_resolve:" + group},
		{alert.StatusAcknowledged, "beacon_snooze:" + group + " beacon_resolve:" + group},
		{alert.StatusSnoozed, "beacon_acknowledge:" + group + " beacon_resolve:" + group},
		{alert.StatusResolved, ""},
	}
	for _, tt := range tests {
		a.Status, a.AcknowledgedAt, a.SnoozedUntil, a.ResolvedAt = tt.status, &at, &at, &at
		msg, body := rendered(t, notify.Message{Alerts: []alert.Alert{a}})
		if got := ids(msg); got != tt.want {
			t.Errorf("%s: buttons = %q, want %q", tt.status, got, tt.want)
		}
		// An edit must be able to take the buttons away.
		if tt.want == "" && !strings.Contains(body, `"components":[]`) {
			t.Errorf("a message without buttons does not send an empty list: %s", body)
		}
	}
	// A group offers an action when one of its alerts allows it.
	resolved, firing := firingAlert(), firingAlert()
	resolved.Status, resolved.ResolvedAt = alert.StatusResolved, &at
	firing.ID = "alert-2"
	if msg, _ := rendered(t, notify.Message{Alerts: []alert.Alert{resolved, firing}}); !strings.Contains(ids(msg), "beacon_acknowledge:") {
		t.Errorf("buttons of a group with one firing alert = %q", ids(msg))
	}
}
