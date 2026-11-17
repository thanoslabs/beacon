// Package config holds the typed form of Beacon's provisioned objects: the
// sources, integrations and so on that docs/configuration-and-data-model.md
// describes.
//
// Load reads the objects from YAML files. Until beacon serve takes --config,
// it still fills these types from a few flags and environment variables.
package config

import (
	"regexp"
	"strings"

	"github.com/thanoslabs/beacon/internal/alert"
)

// Config is everything provisioned.
type Config struct {
	Sources            []Source
	Integrations       []Integration
	Teams              []Team
	Schedules          []Schedule
	EscalationPolicies []EscalationPolicy
	// Routing is nil when no file has a routing block.
	Routing *Routing

	// Notify lists the shared channels that are told about every alert.
	//
	// Temporary: it stands in for routing and escalation policies, whose
	// steps will name their targets. It is not part of the file schema.
	Notify []ChannelTarget
}

// Origin says where an object was defined. It is empty for objects that did
// not come from a file.
type Origin struct {
	File string
	Line int
	// fields maps the path of each field that the file sets, such as
	// severity.default, to its line.
	fields map[string]int
}

// lineOf returns the line of a field, or of the nearest field around it that
// the file sets, or of the object.
func (o Origin) lineOf(field string) int {
	for field != "" {
		if line, ok := o.fields[field]; ok {
			return line
		}
		field = field[:max(strings.LastIndexAny(field, ".["), 0)]
	}
	return o.Line
}

// The source types.
const (
	SourceTypeAlertmanager = "alertmanager"
	SourceTypeGrafana      = "grafana"
	// SourceTypeWebhook is the generic JSON webhook.
	SourceTypeWebhook = "webhook"
)

// Source is an authenticated endpoint that receives alerts.
type Source struct {
	// Name is the identity, and the last segment of the ingest URL.
	Name string `yaml:"name" json:"name"`
	Type string `yaml:"type" json:"type"`
	// Token is what the sender presents as a bearer token.
	Token Secret `yaml:"token" json:"token"`
	// Labels are added to every alert from this source. They do not replace
	// a label the alert already has.
	Labels   map[string]string `yaml:"labels" json:"labels,omitempty"`
	Severity SeverityMapping   `yaml:"severity" json:"severity"`

	Origin Origin `yaml:"-" json:"-"`
}

// SeverityMapping says how a source's alerts get their severity.
type SeverityMapping struct {
	// Label names the label that carries severity. Empty means "severity".
	Label string `yaml:"label" json:"label,omitempty"`
	// Mapping maps label values to Beacon's levels. When empty, the names of
	// the levels map to themselves.
	Mapping map[string]alert.Severity `yaml:"mapping" json:"mapping,omitempty"`
	// Default applies when the label is missing or unmapped. Empty means
	// warning.
	Default alert.Severity `yaml:"default" json:"default,omitempty"`
}

// Resolve returns the severity of an alert with these labels.
func (m SeverityMapping) Resolve(labels map[string]string) alert.Severity {
	label := m.Label
	if label == "" {
		label = "severity"
	}
	value := labels[label]
	if len(m.Mapping) == 0 {
		if s := alert.Severity(value); s.Valid() {
			return s
		}
	} else if s, ok := m.Mapping[value]; ok {
		return s
	}
	if m.Default != "" {
		return m.Default
	}
	return alert.SeverityWarning
}

// The integration types.
const (
	IntegrationTypeSlack   = "slack"
	IntegrationTypeDiscord = "discord"
	IntegrationTypeEmail   = "email"
	IntegrationTypeWebhook = "webhook"
)

// Integration is a connection to a notification service. Exactly the block
// matching Type is set.
type Integration struct {
	Name    string              `yaml:"name" json:"name"`
	Type    string              `yaml:"type" json:"type"`
	Slack   *SlackIntegration   `yaml:"slack" json:"slack,omitempty"`
	Discord *DiscordIntegration `yaml:"discord" json:"discord,omitempty"`
	Email   *EmailIntegration   `yaml:"email" json:"email,omitempty"`
	Webhook *WebhookIntegration `yaml:"webhook" json:"webhook,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// SlackIntegration holds the credentials of a Slack app.
type SlackIntegration struct {
	// BotToken is the app's bot user OAuth token.
	BotToken Secret `yaml:"bot_token" json:"bot_token"`
	// SigningSecret verifies that requests come from Slack.
	SigningSecret Secret `yaml:"signing_secret" json:"signing_secret"`
}

// DiscordIntegration holds the settings of a Discord application.
type DiscordIntegration struct {
	BotToken Secret `yaml:"bot_token" json:"bot_token"`
	// PublicKey verifies that interactions come from Discord. It is not a
	// secret.
	PublicKey     string `yaml:"public_key" json:"public_key"`
	ApplicationID string `yaml:"application_id" json:"application_id"`
}

// The ways an email integration secures its connection.
const (
	EmailTLSStartTLS = "starttls"
	EmailTLSImplicit = "implicit"
	EmailTLSNone     = "none"
)

// EmailIntegration holds the settings of an SMTP server.
type EmailIntegration struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	Username string `yaml:"username" json:"username,omitempty"`
	Password Secret `yaml:"password" json:"password"`
	From     string `yaml:"from" json:"from"`
	// TLS is starttls, implicit or none.
	TLS string `yaml:"tls" json:"tls"`
}

// WebhookIntegration is an outbound webhook.
type WebhookIntegration struct {
	URL string `yaml:"url" json:"url"`
	// Headers are sent with every request. A value is a plain string or a
	// secret reference.
	Headers map[string]HeaderValue `yaml:"headers" json:"headers,omitempty"`
}

// Team is a group of users.
type Team struct {
	Name        string `yaml:"name" json:"name"`
	DisplayName string `yaml:"display_name" json:"display_name,omitempty"`
	// Members are user email addresses.
	Members []string `yaml:"members" json:"members,omitempty"`
	// Groups are identity provider group names whose members belong to the
	// team.
	Groups []string `yaml:"groups" json:"groups,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// Schedule is one on-call rotation.
type Schedule struct {
	Name string `yaml:"name" json:"name"`
	// Team is the name of the owning team.
	Team string `yaml:"team" json:"team"`
	// TimeZone is the IANA zone in which the handoff time is read.
	TimeZone string   `yaml:"time_zone" json:"time_zone"`
	Rotation Rotation `yaml:"rotation" json:"rotation"`
	// EffectiveFrom is when a changed definition takes over. Without it the
	// change applies at the next handoff.
	EffectiveFrom *Timestamp `yaml:"effective_from" json:"effective_from,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// Rotation says who takes turns, for how long, and when they hand over.
type Rotation struct {
	// Members are user email addresses, in rotation order.
	Members []string `yaml:"members" json:"members"`
	// ShiftLength is a whole number of days or weeks.
	ShiftLength Duration `yaml:"shift_length" json:"shift_length"`
	Handoff     Handoff  `yaml:"handoff" json:"handoff"`
}

// Handoff is the local time, and for shifts in weeks the weekday, at which
// one shift ends and the next begins.
type Handoff struct {
	Day  string `yaml:"day" json:"day,omitempty"`
	Time string `yaml:"time" json:"time"`
}

// EscalationPolicy says who is notified about an alert, in which order.
type EscalationPolicy struct {
	Name string `yaml:"name" json:"name"`
	// Repeat is how many more times all steps run after the first pass.
	Repeat int              `yaml:"repeat" json:"repeat,omitempty"`
	Steps  []EscalationStep `yaml:"steps" json:"steps"`

	Origin Origin `yaml:"-" json:"-"`
}

// EscalationStep notifies its targets, then waits Timeout for an
// acknowledgement before the next step runs.
type EscalationStep struct {
	Notify  []Target `yaml:"notify" json:"notify"`
	Timeout Duration `yaml:"timeout" json:"timeout"`
}

// Target is who an escalation step notifies. It has exactly one of four
// forms: a schedule, a team, a user, or a channel with its integration.
type Target struct {
	Schedule string `yaml:"schedule" json:"schedule,omitempty"`
	Team     string `yaml:"team" json:"team,omitempty"`
	// User is an email address.
	User        string `yaml:"user" json:"user,omitempty"`
	Channel     string `yaml:"channel" json:"channel,omitempty"`
	Integration string `yaml:"integration" json:"integration,omitempty"`
	// Via narrows the integrations a person is reached through. It applies
	// to schedule, team and user targets.
	Via []string `yaml:"via" json:"via,omitempty"`
}

// Routing decides which team and escalation policy an alert gets.
type Routing struct {
	GroupBy       []string     `yaml:"group_by" json:"group_by,omitempty"`
	GroupWait     *Duration    `yaml:"group_wait" json:"group_wait,omitempty"`
	GroupInterval *Duration    `yaml:"group_interval" json:"group_interval,omitempty"`
	Default       RouteDefault `yaml:"default" json:"default"`
	// Rules are tried in order; the first match wins.
	Rules []RoutingRule `yaml:"rules" json:"rules,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// RouteDefault applies to alerts that match no rule.
type RouteDefault struct {
	Team             string `yaml:"team" json:"team,omitempty"`
	EscalationPolicy string `yaml:"escalation_policy" json:"escalation_policy"`
}

// RoutingRule matches alerts by label.
type RoutingRule struct {
	Name string `yaml:"name" json:"name"`
	// Match maps a label to the exact value it must have.
	Match map[string]string `yaml:"match" json:"match,omitempty"`
	// MatchRE maps a label to a regular expression, anchored at both ends.
	MatchRE          map[string]string `yaml:"match_re" json:"match_re,omitempty"`
	Team             string            `yaml:"team" json:"team,omitempty"`
	Service          string            `yaml:"service" json:"service,omitempty"`
	EscalationPolicy string            `yaml:"escalation_policy" json:"escalation_policy"`
	GroupBy          []string          `yaml:"group_by" json:"group_by,omitempty"`
	GroupWait        *Duration         `yaml:"group_wait" json:"group_wait,omitempty"`
	GroupInterval    *Duration         `yaml:"group_interval" json:"group_interval,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// ChannelTarget is a shared channel reached through an integration, in the
// form escalation steps use: {channel: <channel>, integration: <name>}.
type ChannelTarget struct {
	Channel     string `json:"channel"`
	Integration string `json:"integration"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidName reports whether name can identify a provisioned object: lowercase
// letters, digits and hyphens, starting with a letter, at most 63 characters.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}
