package discord

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

// message is the body of Create Message and Edit Message, as far as Beacon
// uses it. Every field is always sent: an edit that left one out would keep
// what the message had before.
type message struct {
	// Content is the text of the message, which is also what a notification
	// on a phone shows.
	Content string  `json:"content"`
	Embeds  []embed `json:"embeds"`
	// Components are the rows of buttons.
	Components []actionRow `json:"components"`
	// AllowedMentions says which mentions in the content notify someone.
	// Beacon allows none: see render.
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// embed is a rich embed: the framed part of a message, with a coloured edge.
type embed struct {
	Description string  `json:"description,omitempty"`
	Color       int     `json:"color"`
	Fields      []field `json:"fields,omitempty"`
}

type field struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// allowedMentions is Discord's allowed mentions object. An empty parse list
// means that nothing in the message notifies anyone.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

// actionRow is a row of buttons, and button one of them.
type actionRow struct {
	Type       int      `json:"type"`
	Components []button `json:"components"`
}

type button struct {
	Type     int    `json:"type"`
	Style    int    `json:"style"`
	Label    string `json:"label"`
	CustomID string `json:"custom_id"`
}

// Limits of Discord, and how much of each part Beacon shows. The content of
// a message may be at most 2000 characters, the description of an embed
// 4096, the value of a field 1024, and everything in an embed together 6000.
// The parts are clipped in bytes, which is never less than characters.
const (
	maxSummary     = 300
	maxDescription = 1200
	maxLinkURL     = 450
	maxLabel       = 200
	maxLabels      = 1000
	maxActor       = 80
	// Ten lines of at most 100 bytes of title and 100 of labels.
	maxListed     = 10
	maxListTitle  = 100
	maxListLabels = 100
	// How many people the line of who was paged names, how much of each
	// name it shows, and how long it may get.
	maxPaged     = 20
	maxPagedName = 80
	maxPagedLine = 1000
)

// appearance is how each state is shown. The emoji are Unicode characters,
// which is how Discord takes standard emoji.
var appearance = map[alert.Status]struct {
	emoji, word string
	color       int
}{
	alert.StatusFiring:       {"🔴", "Firing", 0xD92D20},
	alert.StatusAcknowledged: {"🟡", "Acknowledged", 0xF79009},
	alert.StatusSnoozed:      {"💤", "Snoozed", 0x667085},
	alert.StatusResolved:     {"🟢", "Resolved", 0x12B76A},
}

// render builds the message for a group as it is now. A group of one alert
// shows that alert in full. A larger group shows what its alerts share and
// lists them.
//
// Nothing in the message notifies anyone by being mentioned: a person is
// paged by a direct message, which notifies them by itself, and an alert
// must not be able to ping a server with "@everyone". So the allowed
// mentions are empty, and the people who were paged appear inside the embed,
// where Discord shows a mention as a name and notifies nobody.
func render(m notify.Message) message {
	out := message{Embeds: []embed{}, Components: []actionRow{}, AllowedMentions: allowedMentions{Parse: []string{}}}
	if len(m.Alerts) == 0 {
		out.Content = "Alert"
		return out
	}
	status := notify.GroupStatus(m.Alerts)
	a := notify.Representative(m.Alerts, status)
	look := appearance[status]
	severity := notify.WorstSeverity(m.Alerts)
	single := len(m.Alerts) == 1

	title := clip(escape(oneLine(notify.AlertTitle(a))), maxSummary)
	if !single {
		title = clip(escape(oneLine(notify.GroupTitle(m.Group.Labels))), maxSummary)
	}
	out.Content = fmt.Sprintf("%s **%s** · %s\n**%s**", look.emoji, look.word, severity, title)
	if !single {
		out.Content += fmt.Sprintf(" (%d alerts)", len(m.Alerts))
	}

	e := embed{Color: look.color}
	var body []string
	labels := a.Labels
	if single {
		if d := a.Annotations[notify.AnnotationDescription]; d != "" {
			body = append(body, clip(escape(d), maxDescription))
		}
		links := linkLine(a)
		if links != "" {
			body = append(body, links)
		}
	} else {
		labels = m.Group.Labels
		body = append(body, notify.CountLine(m.Alerts), alertList(m.Alerts, m.Group.Labels))
	}
	// Each part is bounded, so together they fit in a description.
	e.Description = strings.Join(body, "\n")
	if line := labelLine(labels, maxLabels); line != "" {
		e.Fields = append(e.Fields, field{Name: "Labels", Value: line})
	}
	e.Fields = append(e.Fields, field{Name: "Status", Value: statusLine(a, m.Events)})
	if paged := pagedLine(m); paged != "" {
		e.Fields = append(e.Fields, field{Name: "Paged", Value: paged})
	}
	out.Embeds = append(out.Embeds, e)
	if row, ok := actionRowFor(m.Alerts); ok {
		out.Components = append(out.Components, row)
	}
	return out
}

// The identifiers of the buttons. Discord sends the custom ID of a button
// back when it is pressed; Beacon's is the identifier, a colon, and the
// group to act on.
const (
	actionAcknowledge = "beacon_acknowledge"
	actionSnooze      = "beacon_snooze"
	actionResolve     = "beacon_resolve"
)

// The styles of a button, by Discord's numbers.
const (
	stylePrimary   = 1
	styleSecondary = 2
)

// buttons lists the buttons in the order they are shown, with the action each
// one takes.
var buttons = []struct {
	id, label string
	style     int
	action    alert.Action
}{
	{actionAcknowledge, "Acknowledge", stylePrimary, alert.ActionAcknowledge},
	{actionSnooze, "Snooze 1h", styleSecondary, alert.ActionSnooze},
	{actionResolve, "Resolve", styleSecondary, alert.ActionResolve},
}

// actionRowFor returns the buttons for the actions that the state of at
// least one alert allows, or false when there are none. A press applies to
// every alert of the group that allows the action.
func actionRowFor(alerts []alert.Alert) (actionRow, bool) {
	row := actionRow{Type: componentActionRow}
	allowed := notify.Actions(alerts)
	for _, b := range buttons {
		if slices.Contains(allowed, b.action) {
			row.Components = append(row.Components, button{Type: componentButton, Style: b.style, Label: b.label, CustomID: b.id + ":" + alerts[0].GroupID})
		}
	}
	return row, len(row.Components) > 0
}

// The types of component, by Discord's numbers.
const (
	componentActionRow = 1
	componentButton    = 2
)

// superseded is what a message is turned into when Beacon has said the same
// alert again further down: its title, without state or buttons, so that
// nobody acts on a message that is no longer kept up to date.
func superseded(current message) message {
	// The second line of a message's content is its title, already escaped
	// and clipped.
	_, title, _ := strings.Cut(current.Content, "\n")
	return message{
		Content:         "⬇️ Beacon sent this alert again; the newer message below has its current state.\n" + title,
		Embeds:          []embed{},
		Components:      []actionRow{},
		AllowedMentions: allowedMentions{Parse: []string{}},
	}
}

// snowflakeRE matches a Discord ID, the only thing that goes into a mention.
var snowflakeRE = regexp.MustCompile(`^[0-9]{15,22}$`)

// pagedLine says who the escalation of the group has paged so far, or
// returns "" when it has paged no person.
//
//   - A person who was paged through the integration this message is sent
//     with is mentioned, so the name is the one they have in this server and
//     leads to them. Inside an embed a mention notifies nobody.
//   - A person who was to be paged and has no linked account is named in
//     plain text, with "(Discord not connected)". They were not notified,
//     and the people who read the message should know that.
//   - Anyone else, such as a person paged through another integration, is
//     named in plain text.
func pagedLine(m notify.Message) string {
	paged := notify.Paged(m)
	if len(paged) == 0 {
		return ""
	}
	var names []string
	length := 0
	for i, p := range paged {
		name := escape(notify.PagedPerson{Name: clipPlain(oneLine(p.Name), maxPagedName), Missing: p.Missing}.ShownAs())
		if snowflakeRE.MatchString(p.Destination) {
			name = "<@" + p.Destination + ">"
		}
		if i == maxPaged || length+len(name) > maxPagedLine {
			names = append(names, fmt.Sprintf("and %d more", len(paged)-i))
			break
		}
		length += len(name) + len(", ")
		names = append(names, name)
	}
	return strings.Join(names, ", ")
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
			line += " " + clip(escape(oneLine(summary)), maxListTitle)
		} else if labels == "" {
			line += " " + clip(escape(oneLine(notify.AlertTitle(a))), maxListTitle)
		}
		if labels != "" {
			line += " " + labels
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// shownLinks are the links of an alert that a message offers, in order, with
// their text.
var shownLinks = []struct{ name, label string }{
	{alert.LinkGenerator, "🔍 Source"},
	{alert.LinkDashboard, "📊 Dashboard"},
	{alert.LinkPanel, "📈 Panel"},
	{alert.LinkSilence, "🔕 Silence at the source"},
}

// linkLine shows the runbook and the links the source sent with the alert.
// There are at most five, each bounded.
func linkLine(a alert.Alert) string {
	var parts []string
	if s := link(a.Annotations[notify.AnnotationRunbook], "📖 Runbook"); s != "" {
		parts = append(parts, s)
	}
	for _, l := range shownLinks {
		if s := link(a.Links[l.name], l.label); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " · ")
}

// link returns a Discord link to raw with the given text, or nothing: only
// http and https links of a bounded length are made, so an alert cannot put
// another kind of link in the message or make it too long to send.
func link(raw, label string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		len(raw) > maxLinkURL || strings.ContainsAny(raw, "<> \t\r\n\\") {
		return ""
	}
	// A parenthesis would end the link early.
	raw = strings.NewReplacer("(", "%28", ")", "%29").Replace(raw)
	return "[" + label + "](" + raw + ")"
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
		// Inside code formatting nothing is markup except the backtick, which
		// would end it early.
		pair := strings.ReplaceAll(oneLine(name+"="+labels[name]), "`", "'")
		parts = append(parts, "`"+clipPlain(pair, min(maxLabel, maxLen-len("`` …")))+"`")
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
	// A person is named in plain text, never mentioned.
	actor := escape(clipPlain(oneLine(notify.ActorName(last)), maxActor))
	switch a.Status {
	case alert.StatusAcknowledged:
		return "Acknowledged by " + actor + " " + when(a.AcknowledgedAt)
	case alert.StatusSnoozed:
		return "Snoozed by " + actor + " until " + when(a.SnoozedUntil)
	case alert.StatusResolved:
		return "Resolved by " + actor + " " + when(a.ResolvedAt)
	default:
		if last != nil && last.Type == alert.EventSnoozeEnded {
			return "Firing again: the snooze ended " + when(&last.OccurredAt)
		}
		return "Firing since " + when(&a.StartedAt)
	}
}

// when formats a time so that Discord shows it in each reader's time zone
// and locale.
func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmt.Sprintf("<t:%d:f>", t.Unix())
}

// oneLine replaces line breaks, so that text from an alert cannot start a
// line of its own, where Discord reads headings, lists and quotes.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// escape puts a backslash before every character that Discord reads as
// markup. Everything that comes from an alert goes through it, so an alert
// cannot format the message, add links, or write a mention such as <@id>:
// without its angle bracket a mention is plain text. "@everyone" needs no
// escape, because the allowed mentions of every message are empty.
func escape(s string) string {
	return escaper.Replace(s)
}

var escaper = strings.NewReplacer(
	`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`, "<", `\<`,
	"#", `\#`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`,
)

// clip shortens escaped text to at most n bytes, ending it with an ellipsis,
// without leaving half a character or half an escape behind.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = clipPlain(s, n)
	s = strings.TrimSuffix(s, "…")
	// A backslash that lost the character it escaped would escape whatever
	// follows the text.
	trailing := len(s) - len(strings.TrimRight(s, `\`))
	if trailing%2 == 1 {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// clipPlain shortens text to at most n bytes, ending it with an ellipsis,
// without leaving half a character behind.
func clipPlain(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
