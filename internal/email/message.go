package email

import (
	"bytes"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/notify"
)

// How much of each part an email shows. An email has no hard limits like a
// chat message, but an alert must not be able to make one of any size.
const (
	maxSubject     = 200
	maxSummary     = 300
	maxDescription = 4000
	maxLabel       = 300
	maxLabels      = 50
	maxURL         = 2000
	maxListed      = 25
	maxPaged       = 50
	maxName        = 100
)

// link is an address an email offers, with what it is.
type link struct{ label, url string }

// content is what an email says about a group, before it is written out as
// text and as HTML. Nothing in it is escaped; each form escapes for itself.
type content struct {
	// state is the word for the group's state, such as "FIRING".
	state    string
	severity string
	title    string
	// description is set for a group of one alert.
	description string
	// count and listed are set for a group of several alerts.
	count  string
	listed []string
	labels []string
	status string
	paged  string
	links  []link
	reason string
}

// words is how each state is written.
var words = map[alert.Status]string{
	alert.StatusFiring:       "FIRING",
	alert.StatusAcknowledged: "ACKNOWLEDGED",
	alert.StatusSnoozed:      "SNOOZED",
	alert.StatusResolved:     "RESOLVED",
}

// why says, at the foot of an email, why it was sent.
var why = map[notify.Reason]string{
	notify.ReasonEscalationStep: "an escalation policy asks for a response to this alert",
	notify.ReasonGroupUpdate:    "this alert changed",
	notify.ReasonResolved:       "this alert is resolved",
}

// describe works out what the email says. publicURL is the address of
// Beacon, for the link to the alert, or empty.
func describe(m notify.Message, publicURL string) content {
	if len(m.Alerts) == 0 {
		return content{state: "ALERT", title: "Alert"}
	}
	status := notify.GroupStatus(m.Alerts)
	a := notify.Representative(m.Alerts, status)
	single := len(m.Alerts) == 1
	c := content{
		state:    words[status],
		severity: string(notify.WorstSeverity(m.Alerts)),
		title:    clip(oneLine(notify.AlertTitle(a)), maxSummary),
		status:   statusLine(a, m.Events),
		reason:   why[m.Notification.Reason],
	}
	labels := a.Labels
	if single {
		c.description = clip(clean(a.Annotations[notify.AnnotationDescription]), maxDescription)
		c.links = alertLinks(a, publicURL)
	} else {
		labels = m.Group.Labels
		c.title = clip(oneLine(notify.GroupTitle(m.Group.Labels)), maxSummary)
		c.count = notify.CountLine(m.Alerts)
		ordered := notify.ByUrgency(m.Alerts)
		for i, a := range ordered {
			if i == maxListed {
				c.listed = append(c.listed, fmt.Sprintf("… and %d more", len(ordered)-maxListed))
				break
			}
			line := string(a.Status) + ": " + clip(oneLine(notify.AlertTitle(a)), maxSummary)
			if own := labelPairs(notify.OwnLabels(a, m.Group.Labels)); len(own) > 0 {
				line += " (" + strings.Join(own, ", ") + ")"
			}
			c.listed = append(c.listed, line)
		}
		if u := beaconLink(publicURL, a.ID); u != "" {
			c.links = append(c.links, link{"Open in Beacon", u})
		}
	}
	c.labels = labelPairs(labels)
	var names []string
	paged := notify.Paged(m)
	for i, p := range paged {
		if i == maxPaged {
			names = append(names, fmt.Sprintf("and %d more", len(paged)-maxPaged))
			break
		}
		p.Name = clip(oneLine(p.Name), maxName)
		names = append(names, p.ShownAs())
	}
	c.paged = strings.Join(names, ", ")
	return c
}

// alertLinks are the addresses an email about one alert offers: the alert
// in Beacon, the runbook, and the links the source sent.
func alertLinks(a alert.Alert, publicURL string) []link {
	var out []link
	add := func(label, raw string) {
		if u := webAddress(raw); u != "" {
			out = append(out, link{label, u})
		}
	}
	add("Open in Beacon", beaconLink(publicURL, a.ID))
	add("Runbook", a.Annotations[notify.AnnotationRunbook])
	add("Source", a.Links[alert.LinkGenerator])
	add("Dashboard", a.Links[alert.LinkDashboard])
	add("Panel", a.Links[alert.LinkPanel])
	add("Silence at the source", a.Links[alert.LinkSilence])
	return out
}

// AlertPath is where Beacon serves an alert, under its public address. An
// email links to it.
const AlertPath = "/api/v1/alerts/"

// beaconLink returns the address of an alert in Beacon, or nothing when
// Beacon does not know its own address.
func beaconLink(publicURL, alertID string) string {
	if publicURL == "" || alertID == "" {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + AlertPath + url.PathEscape(alertID)
}

// webAddress returns raw when it is an http or https address of a bounded
// length without spaces or control characters, and nothing otherwise, so
// that an alert cannot put another kind of link into an email.
func webAddress(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > maxURL ||
		strings.ContainsFunc(raw, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || r == '<' || r == '>' || r == '"'
		}) {
		return ""
	}
	return raw
}

// labelPairs lists labels as "name = value" in name order, bounded.
func labelPairs(labels map[string]string) []string {
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for i, name := range names {
		if i == maxLabels {
			out = append(out, fmt.Sprintf("… and %d more", len(names)-maxLabels))
			break
		}
		out = append(out, clip(oneLine(name+" = "+labels[name]), maxLabel))
	}
	return out
}

// statusLine says what last happened to the alert, who did it and when. An
// email cannot show a time in its reader's zone, so times are in UTC.
func statusLine(a alert.Alert, events []alert.Event) string {
	last := notify.LastChange(a, events)
	actor := clip(oneLine(notify.ActorName(last)), maxName)
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

func when(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// subject is the subject line: the state first, because that is what a list
// of messages shows.
func (c content) subject() string {
	s := "[" + c.state + "] "
	if c.severity != "" {
		s += c.severity + ": "
	}
	s += c.title
	if c.count != "" {
		count, _, _ := strings.Cut(c.count, ":")
		s += " (" + count + ")"
	}
	return clip(oneLine(s), maxSubject)
}

// text writes the email as plain text.
func (c content) text() string {
	var b strings.Builder
	head := c.state
	if c.severity != "" {
		head += " · " + c.severity
	}
	b.WriteString(head + "\n" + c.title + "\n")
	if c.description != "" {
		b.WriteString("\n" + c.description + "\n")
	}
	if c.count != "" {
		b.WriteString("\n" + c.count + "\n")
		for _, line := range c.listed {
			b.WriteString("  - " + line + "\n")
		}
	}
	if len(c.labels) > 0 {
		b.WriteString("\nLabels:\n")
		for _, l := range c.labels {
			b.WriteString("  " + l + "\n")
		}
	}
	b.WriteString("\n")
	if c.status != "" {
		b.WriteString("Status: " + c.status + "\n")
	}
	if c.paged != "" {
		b.WriteString("Paged: " + c.paged + "\n")
	}
	if len(c.links) > 0 {
		b.WriteString("\n")
		for _, l := range c.links {
			b.WriteString(l.label + ": " + l.url + "\n")
		}
	}
	// "-- " on a line of its own begins a signature.
	b.WriteString("\n-- \nSent by Beacon")
	if c.reason != "" {
		b.WriteString(" because " + c.reason)
	}
	b.WriteString(".\n")
	return b.String()
}

// colours of the state in the HTML form.
var colours = map[string]string{"FIRING": "#d92d20", "ACKNOWLEDGED": "#b54708", "SNOOZED": "#475467", "RESOLVED": "#067647"}

// html writes the email as HTML. Everything that is not markup of Beacon's
// own passes through html.EscapeString, and an address becomes a link only
// after webAddress let it through.
func (c content) html() string {
	e := html.EscapeString
	var b strings.Builder
	colour := colours[c.state]
	if colour == "" {
		colour = "#475467"
	}
	b.WriteString(`<!DOCTYPE html><html><body style="font-family: sans-serif; color: #101828;">` + "\n")
	head := e(c.state)
	if c.severity != "" {
		head += " · " + e(c.severity)
	}
	b.WriteString(`<p style="margin: 0; font-weight: bold; color: ` + colour + `;">` + head + "</p>\n")
	b.WriteString(`<h2 style="margin: 4px 0 12px 0;">` + e(c.title) + "</h2>\n")
	if c.description != "" {
		b.WriteString(`<p style="white-space: pre-wrap;">` + e(c.description) + "</p>\n")
	}
	if c.count != "" {
		b.WriteString("<p>" + e(c.count) + "</p>\n<ul>\n")
		for _, line := range c.listed {
			b.WriteString("<li>" + e(line) + "</li>\n")
		}
		b.WriteString("</ul>\n")
	}
	if len(c.labels) > 0 {
		b.WriteString("<p><b>Labels</b></p>\n<ul>\n")
		for _, l := range c.labels {
			b.WriteString("<li><code>" + e(l) + "</code></li>\n")
		}
		b.WriteString("</ul>\n")
	}
	if c.status != "" {
		b.WriteString("<p><b>Status:</b> " + e(c.status) + "</p>\n")
	}
	if c.paged != "" {
		b.WriteString("<p><b>Paged:</b> " + e(c.paged) + "</p>\n")
	}
	if len(c.links) > 0 {
		var parts []string
		for _, l := range c.links {
			parts = append(parts, `<a href="`+e(l.url)+`">`+e(l.label)+"</a>")
		}
		b.WriteString("<p>" + strings.Join(parts, " · ") + "</p>\n")
	}
	foot := "Sent by Beacon"
	if c.reason != "" {
		foot += " because " + e(c.reason)
	}
	b.WriteString(`<p style="color: #667085; font-size: 12px;">` + foot + ".</p>\n</body></html>\n")
	return b.String()
}

// envelope is what the headers of a message are built from.
type envelope struct {
	from, to string
	date     time.Time
	// id is the Message-ID. inReplyTo is the message this one follows, and
	// thread the root that every message about the group refers to, so that
	// a mail client can show them as one conversation.
	id, inReplyTo, thread string
}

// compose writes a whole message: headers, then the content as plain text
// and as HTML.
//
// A header value never holds a line break, which is what would let an alert
// add headers of its own: the addresses and the identifiers are Beacon's and
// are checked by the caller, and the subject is the one value that comes
// from an alert, so it is stripped of control characters and encoded.
func compose(env envelope, c content) ([]byte, error) {
	var body bytes.Buffer
	parts := multipart.NewWriter(&body)
	for _, part := range []struct{ kind, text string }{{"text/plain", c.text()}, {"text/html", c.html()}} {
		w, err := parts.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.kind + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		// Quoted-printable keeps lines short and encodes whatever is not
		// plain text, including a carriage return that is not a line break.
		qp := quotedprintable.NewWriter(w)
		if _, err := qp.Write([]byte(part.text)); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := parts.Close(); err != nil {
		return nil, err
	}

	var msg bytes.Buffer
	header := func(name, value string) {
		msg.WriteString(name + ": " + value + "\r\n")
	}
	header("From", "<"+env.from+">")
	header("To", "<"+env.to+">")
	header("Subject", mime.QEncoding.Encode("utf-8", c.subject()))
	header("Date", env.date.UTC().Format(time.RFC1123Z))
	header("Message-ID", env.id)
	if env.inReplyTo != "" {
		header("In-Reply-To", env.inReplyTo)
	}
	references := env.thread
	if env.inReplyTo != "" && env.inReplyTo != env.thread {
		references += " " + env.inReplyTo
	}
	if references != "" {
		header("References", references)
	}
	// Tells mail systems that no person wrote this, so that they do not
	// answer it with an out-of-office reply.
	header("Auto-Submitted", "auto-generated")
	header("MIME-Version", "1.0")
	header("Content-Type", `multipart/alternative; boundary="`+parts.Boundary()+`"`)
	msg.WriteString("\r\n")
	msg.Write(body.Bytes())
	return msg.Bytes(), nil
}

// clean removes what does not belong in text from an alert: control
// characters other than line breaks and tabs. Line breaks become plain
// newlines.
func clean(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// oneLine is clean, on one line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(clean(s)), " ")
}

// clip shortens text to at most n bytes, ending it with an ellipsis,
// without leaving half a character behind.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n-len("…")]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
