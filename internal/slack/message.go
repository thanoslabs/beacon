package slack

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

// message is the content of a Slack message: Block Kit blocks, and a plain
// text that Slack shows in notifications and where blocks cannot be drawn.
type message struct {
	Text   string  `json:"text"`
	Blocks []block `json:"blocks"`
}

// block is a section or a context block.
type block struct {
	Type     string `json:"type"`
	Text     *text  `json:"text,omitempty"`
	Elements []text `json:"elements,omitempty"`
}

// text is a Block Kit text object.
type text struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func mrkdwn(s string) *text { return &text{Type: "mrkdwn", Text: s} }

// Limits of Block Kit, and how much of each part Beacon shows.
// A section's text may be at most 3000 characters.
const (
	maxSummary     = 300
	maxDescription = 1500
	maxRunbookURL  = 500
	maxLabel       = 200
	maxLabels      = 2000
)

// The annotations a message looks for.
const (
	annotationSummary     = "summary"
	annotationDescription = "description"
	annotationRunbook     = "runbook_url"
)

// appearance is how each state is shown.
var appearance = map[alert.Status]struct{ emoji, word string }{
	alert.StatusFiring:       {":red_circle:", "Firing"},
	alert.StatusAcknowledged: {":large_yellow_circle:", "Acknowledged"},
	alert.StatusSnoozed:      {":zzz:", "Snoozed"},
	alert.StatusResolved:     {":large_green_circle:", "Resolved"},
}

// render builds the message for a group as it is now. Until grouping exists a
// group holds one alert; a larger group shows its first alert and a count.
func render(m notify.Message) message {
	if len(m.Alerts) == 0 {
		return message{Text: "Alert", Blocks: []block{{Type: "section", Text: mrkdwn("Alert")}}}
	}
	a := m.Alerts[0]
	look := appearance[a.Status]
	title := clip(escape(alertTitle(a)), maxSummary)

	var head strings.Builder
	fmt.Fprintf(&head, "%s *%s* · %s\n*%s*", look.emoji, look.word, a.Severity, title)
	if d := a.Annotations[annotationDescription]; d != "" {
		head.WriteString("\n" + clip(escape(d), maxDescription))
	}
	if link := runbookLink(a.Annotations[annotationRunbook]); link != "" {
		head.WriteString("\n" + link)
	}
	// The parts are each bounded, so together they fit in a section.
	blocks := []block{{Type: "section", Text: mrkdwn(head.String())}}

	if labels := labelLine(a.Labels); labels != "" {
		blocks = append(blocks, block{Type: "section", Text: mrkdwn(labels)})
	}

	detail := statusLine(a, m.Events)
	if more := len(m.Alerts) - 1; more > 0 {
		detail += fmt.Sprintf(" · and %d more in this group", more)
	}
	blocks = append(blocks, block{Type: "context", Elements: []text{*mrkdwn(detail)}})

	return message{
		Text:   fmt.Sprintf("[%s] %s: %s", strings.ToUpper(look.word), a.Severity, title),
		Blocks: blocks,
	}
}

// alertTitle is the one line that says what the alert is.
func alertTitle(a alert.Alert) string {
	for _, s := range []string{a.Annotations[annotationSummary], a.Labels["alertname"]} {
		if s != "" {
			return s
		}
	}
	return "Alert " + a.Fingerprint
}

// runbookLink returns a link to the runbook when the annotation holds a web
// address, and nothing otherwise: only http and https links are made, so an
// alert cannot put another kind of link in the message.
func runbookLink(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		len(raw) > maxRunbookURL || strings.ContainsAny(raw, "|<> \n") {
		return ""
	}
	return "<" + escape(raw) + "|:book: Runbook>"
}

// labelLine shows the labels in name order as `name=value`.
func labelLine(labels map[string]string) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		// A backtick would end the code formatting early.
		pair := strings.ReplaceAll(name+"="+labels[name], "`", "'")
		parts = append(parts, "`"+clip(escape(pair), maxLabel)+"`")
	}
	line := strings.Join(parts, " ")
	if len(line) <= maxLabels {
		return line
	}
	// Drop whole labels rather than cut one in the middle of its formatting.
	for len(parts) > 0 && len(strings.Join(parts, " ")) > maxLabels-2 {
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, " ") + " …"
}

// statusLine says what last happened to the alert, who did it and when.
func statusLine(a alert.Alert, events []alert.Event) string {
	last := lastChange(a, events)
	switch a.Status {
	case alert.StatusAcknowledged:
		return "Acknowledged by " + actorName(last) + " " + when(a.AcknowledgedAt)
	case alert.StatusSnoozed:
		return "Snoozed by " + actorName(last) + " until " + when(a.SnoozedUntil)
	case alert.StatusResolved:
		return "Resolved by " + actorName(last) + " " + when(a.ResolvedAt)
	default:
		if last != nil && last.Type == alert.EventSnoozeEnded {
			return "Firing again: the snooze ended " + when(&last.OccurredAt)
		}
		return "Firing since " + when(&a.StartedAt)
	}
}

// lastChange returns the latest state change of the alert on the timeline.
func lastChange(a alert.Alert, events []alert.Event) *alert.Event {
	for i := len(events) - 1; i >= 0; i-- {
		e := &events[i]
		if e.AlertID != a.ID {
			continue
		}
		switch e.Type {
		case alert.EventFired, alert.EventAcknowledged, alert.EventSnoozed, alert.EventSnoozeEnded, alert.EventResolved:
			return e
		}
	}
	return nil
}

// actorName names who caused an event. A person is named in plain text, not
// mentioned, so that updating a message does not notify them again.
func actorName(e *alert.Event) string {
	switch {
	case e == nil:
		return "someone"
	case e.Actor.Type == alert.ActorSource:
		return "the source"
	case e.Actor.Type == alert.ActorSystem:
		return "Beacon"
	case e.Actor.Name != "":
		return escape(e.Actor.Name)
	default:
		return "someone"
	}
}

// when formats a time so that Slack shows it in each reader's time zone, with
// UTC as the fallback text.
func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format("2006-01-02 15:04 UTC"))
}

// escape replaces the three characters that Slack treats as control
// characters in message text. Everything that comes from an alert goes
// through it, so an alert cannot add links or mentions such as <!channel>.
func escape(s string) string {
	return escaper.Replace(s)
}

var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// partialEntity matches an escape sequence that was cut short.
var partialEntity = regexp.MustCompile(`&[a-z]{0,3}$`)

// clip shortens escaped text to at most n bytes, ending it with an ellipsis,
// without leaving half a character or half an escape sequence behind.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return partialEntity.ReplaceAllString(s, "") + "…"
}
