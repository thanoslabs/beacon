package slack

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
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

// block is a section, a context block or an actions block. The elements of a
// context block are text objects, and those of an actions block are buttons.
type block struct {
	Type     string `json:"type"`
	Text     *text  `json:"text,omitempty"`
	Elements []any  `json:"elements,omitempty"`
}

// button is a Block Kit button element.
type button struct {
	Type     string `json:"type"`
	Text     text   `json:"text"`
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
	Style    string `json:"style,omitempty"`
}

// The action identifiers of the buttons. Slack sends the identifier back when
// a button is pressed.
const (
	actionAcknowledge = "beacon_acknowledge"
	actionSnooze      = "beacon_snooze"
	actionResolve     = "beacon_resolve"
)

// SnoozeDuration is how long the snooze button snoozes for.
const SnoozeDuration = time.Hour

// buttons lists the buttons in the order they are shown, with the action each
// one takes.
var buttons = []struct {
	id, label, style string
	action           alert.Action
}{
	{actionAcknowledge, "Acknowledge", "primary", alert.ActionAcknowledge},
	{actionSnooze, "Snooze 1h", "", alert.ActionSnooze},
	{actionResolve, "Resolve", "", alert.ActionResolve},
}

// actionsBlock returns the buttons for the actions that the state of at least
// one alert allows, or false when there are none. The value of each button is
// the group to act on: a press applies to every alert of the group that
// allows the action.
func actionsBlock(alerts []alert.Alert) (block, bool) {
	var elements []any
	allowed := notify.Actions(alerts)
	for _, b := range buttons {
		if slices.Contains(allowed, b.action) {
			elements = append(elements, button{
				Type: "button", Text: text{Type: "plain_text", Text: b.label},
				ActionID: b.id, Value: alerts[0].GroupID, Style: b.style,
			})
		}
	}
	return block{Type: "actions", Elements: elements}, len(elements) > 0
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
	maxLinkURL     = 700
	maxLabel       = 200
	maxLabels      = 2000
)

// appearance is how each state is shown.
var appearance = map[alert.Status]struct{ emoji, word string }{
	alert.StatusFiring:       {":red_circle:", "Firing"},
	alert.StatusAcknowledged: {":large_yellow_circle:", "Acknowledged"},
	alert.StatusSnoozed:      {":zzz:", "Snoozed"},
	alert.StatusResolved:     {":large_green_circle:", "Resolved"},
}

// Limits of the list of alerts in the message of a group. Ten lines of at
// most 100 characters of title and 100 of labels fit in a section.
const (
	maxListed     = 10
	maxListTitle  = 100
	maxListLabels = 100
)

// render builds the message for a group as it is now. A group of one alert
// shows that alert in full. A larger group shows what its alerts share and
// lists them.
func render(m notify.Message) message {
	if len(m.Alerts) == 0 {
		return message{Text: "Alert", Blocks: []block{{Type: "section", Text: mrkdwn("Alert")}}}
	}
	status := notify.GroupStatus(m.Alerts)
	a := notify.Representative(m.Alerts, status)
	look := appearance[status]
	severity := notify.WorstSeverity(m.Alerts)
	single := len(m.Alerts) == 1

	title := clip(escape(notify.AlertTitle(a)), maxSummary)
	if !single {
		title = clip(escape(notify.GroupTitle(m.Group.Labels)), maxSummary)
	}
	var head strings.Builder
	fmt.Fprintf(&head, "%s *%s* · %s\n*%s*", look.emoji, look.word, severity, title)
	if single {
		if d := a.Annotations[notify.AnnotationDescription]; d != "" {
			head.WriteString("\n" + clip(escape(d), maxDescription))
		}
		if link := runbookLink(a.Annotations[notify.AnnotationRunbook]); link != "" {
			head.WriteString("\n" + link)
		}
	} else {
		head.WriteString("\n" + notify.CountLine(m.Alerts))
	}
	// The parts are each bounded, so together they fit in a section.
	blocks := []block{{Type: "section", Text: mrkdwn(head.String())}}

	labels := a.Labels
	if !single {
		labels = m.Group.Labels
	}
	if line := labelLine(labels, maxLabels); line != "" {
		blocks = append(blocks, block{Type: "section", Text: mrkdwn(line)})
	}
	if single {
		if links := linkLine(a.Links); links != "" {
			blocks = append(blocks, block{Type: "section", Text: mrkdwn(links)})
		}
	} else {
		blocks = append(blocks, block{Type: "section", Text: mrkdwn(alertList(m.Alerts, m.Group.Labels))})
	}

	blocks = append(blocks, block{Type: "context", Elements: []any{mrkdwn(statusLine(a, m.Events))}})
	if paged := pagedLine(m); paged != "" {
		blocks = append(blocks, block{Type: "context", Elements: []any{mrkdwn(paged)}})
	}
	if actions, ok := actionsBlock(m.Alerts); ok {
		blocks = append(blocks, actions)
	}

	fallback := fmt.Sprintf("[%s] %s: %s", strings.ToUpper(look.word), severity, title)
	if !single {
		fallback += fmt.Sprintf(" (%d alerts)", len(m.Alerts))
	}
	return message{Text: fallback, Blocks: blocks}
}

// maxPaged bounds how many people the line of who was paged names, and
// maxPagedName how much of each name it shows, so that the line fits in a
// context element whatever the names are.
const (
	maxPaged     = 20
	maxPagedName = 80
)

// slackUserRE matches a Slack user ID, the only thing that goes into a
// mention.
var slackUserRE = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)

// pagedLine says who the escalation of the group has paged so far, or
// returns "" when it has paged no person.
//
//   - A person who was paged through the integration this message is sent
//     with is mentioned, so the name is the one they have in this workspace
//     and leads to them.
//   - A person who was to be paged and has no linked account is named in
//     plain text, with "(Slack not connected)". They were not notified,
//     and the people who read the message should know that.
//   - Anyone else, such as a person paged through another integration, is
//     named in plain text.
//
// It is read from the steps on the timeline, so it shows what happened and
// not what the policy says now.
func pagedLine(m notify.Message) string {
	paged := notify.Paged(m)
	if len(paged) == 0 {
		return ""
	}
	var names []string
	for i, p := range paged {
		if i == maxPaged {
			names = append(names, fmt.Sprintf("and %d more", len(paged)-maxPaged))
			break
		}
		p.Name = clip(p.Name, maxPagedName)
		if slackUserRE.MatchString(p.Destination) {
			names = append(names, "<@"+p.Destination+">")
			continue
		}
		// Plain text, never a mention: there is no account to mention.
		names = append(names, escape(p.ShownAs()))
	}
	return ":pager: Paged: " + strings.Join(names, ", ")
}

// superseded is what a message is turned into when Beacon has said the same
// alert again further down: its title, without state or buttons, so that
// nobody acts on a message that is no longer kept up to date.
func superseded(current message) message {
	// The fallback text of a message is built from escaped, clipped parts.
	title := current.Text
	return message{
		Text:   "Sent again below: " + title,
		Blocks: []block{{Type: "context", Elements: []any{mrkdwn(":arrow_down: Beacon sent this alert again; the newer message below has its current state.\n" + title)}}},
	}
}

// alertList lists the alerts of a group, those that ask most of the reader
// first: each with its state, its summary and the labels that tell it apart
// from the others. It is bounded; what does not fit is counted.
func alertList(alerts []alert.Alert, shared map[string]string) string {
	ordered := notify.ByUrgency(alerts)
	var lines []string
	for i, a := range ordered {
		if i == maxListed {
			lines = append(lines, fmt.Sprintf("… and %d more", len(ordered)-maxListed))
			break
		}
		line := appearance[a.Status].emoji
		labels := labelLine(notify.OwnLabels(a, shared), maxListLabels)
		if summary := a.Annotations[notify.AnnotationSummary]; summary != "" {
			line += " " + clip(escape(summary), maxListTitle)
		} else if labels == "" {
			line += " " + clip(escape(notify.AlertTitle(a)), maxListTitle)
		}
		if labels != "" {
			line += " " + labels
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// runbookLink returns a link to the runbook when the annotation holds a web
// address, and nothing otherwise.
func runbookLink(raw string) string {
	return link(raw, ":book: Runbook", maxRunbookURL)
}

// link returns a Slack link to raw with the given text, or nothing: only
// http and https links of a bounded length are made, so an alert cannot put
// another kind of link in the message or make it too long to send.
func link(raw, label string, maxLen int) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		len(raw) > maxLen || strings.ContainsAny(raw, "|<> \n") {
		return ""
	}
	return "<" + escape(raw) + "|" + label + ">"
}

// shownLinks are the links of an alert that a message offers, in order, with
// their text.
var shownLinks = []struct{ name, label string }{
	{alert.LinkGenerator, ":mag: Source"},
	{alert.LinkDashboard, ":bar_chart: Dashboard"},
	{alert.LinkPanel, ":chart_with_upwards_trend: Panel"},
	{alert.LinkSilence, ":no_bell: Silence at the source"},
}

// linkLine shows the links the source sent with the alert. There are at most
// four, each bounded, so together they fit in a section.
func linkLine(links map[string]string) string {
	var parts []string
	for _, l := range shownLinks {
		if s := link(links[l.name], l.label, maxLinkURL); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// labelLine shows the labels in name order as `name=value`, in at most
// maxLen bytes.
func labelLine(labels map[string]string, maxLen int) string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		// A backtick would end the code formatting early.
		pair := strings.ReplaceAll(name+"="+labels[name], "`", "'")
		parts = append(parts, "`"+clip(escape(pair), min(maxLabel, maxLen-len("`` …")))+"`")
	}
	line := strings.Join(parts, " ")
	if len(line) <= maxLen {
		return line
	}
	// Drop whole labels rather than cut one in the middle of its formatting.
	for len(parts) > 0 && len(strings.Join(parts, " ")) > maxLen-len(" …") {
		parts = parts[:len(parts)-1]
	}
	return strings.TrimSpace(strings.Join(parts, " ") + " …")
}

// statusLine says what last happened to the alert, who did it and when.
func statusLine(a alert.Alert, events []alert.Event) string {
	last := notify.LastChange(a, events)
	switch a.Status {
	case alert.StatusAcknowledged:
		return "Acknowledged by " + escape(notify.ActorName(last)) + " " + when(a.AcknowledgedAt)
	case alert.StatusSnoozed:
		return "Snoozed by " + escape(notify.ActorName(last)) + " until " + when(a.SnoozedUntil)
	case alert.StatusResolved:
		return "Resolved by " + escape(notify.ActorName(last)) + " " + when(a.ResolvedAt)
	default:
		if last != nil && last.Type == alert.EventSnoozeEnded {
			return "Firing again: the snooze ended " + when(&last.OccurredAt)
		}
		return "Firing since " + when(&a.StartedAt)
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
