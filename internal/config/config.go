// Package config holds the typed form of Beacon's provisioned objects: the
// sources, integrations and so on that docs/configuration-and-data-model.md
// describes.
//
// Load reads the objects from YAML files, Validate checks them, and Read does
// both.
package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

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
	// Auth is nil when no file has an auth block. Nobody can sign in then.
	Auth *Auth
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
	// GroupBy lists the labels that define a group. Nil means
	// DefaultGroupBy; an empty list puts every alert in one group.
	GroupBy []string `yaml:"group_by" json:"group_by,omitempty"`
	// GroupWait is how long a new group waits before its first
	// notification. Nil means DefaultGroupWait; zero means no wait.
	GroupWait *Duration `yaml:"group_wait" json:"group_wait,omitempty"`
	// GroupInterval is the least time between notifications about a group.
	// Nil means DefaultGroupInterval.
	GroupInterval *Duration    `yaml:"group_interval" json:"group_interval,omitempty"`
	Default       RouteDefault `yaml:"default" json:"default"`
	// Rules are tried in order; the first match wins.
	Rules []RoutingRule `yaml:"rules" json:"rules,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// The grouping that applies unless the configuration says otherwise.
const (
	DefaultGroupWait     = Duration(30 * time.Second)
	DefaultGroupInterval = Duration(5 * time.Minute)
)

// DefaultGroupBy are the labels that define a group unless the configuration
// says otherwise.
var DefaultGroupBy = []string{"alertname"}

// Grouping says which alerts are told about together, and how often.
type Grouping struct {
	// By lists the labels that define a group: alerts with the same values
	// for all of them share one. An empty list puts every alert in one group.
	By []string
	// Wait is how long a new group waits before its first notification.
	Wait time.Duration
	// Interval is the least time between two notifications about a group
	// that its sources cause.
	Interval time.Duration
}

// Grouping returns the grouping that the routing block asks for, with the
// defaults for what it leaves out. A nil Routing gives the defaults.
func (r *Routing) Grouping() Grouping {
	g := Grouping{By: DefaultGroupBy, Wait: time.Duration(DefaultGroupWait), Interval: time.Duration(DefaultGroupInterval)}
	if r == nil {
		return g
	}
	if r.GroupBy != nil {
		g.By = r.GroupBy
	}
	if r.GroupWait != nil {
		g.Wait = time.Duration(*r.GroupWait)
	}
	if r.GroupInterval != nil {
		g.Interval = time.Duration(*r.GroupInterval)
	}
	return g
}

// GroupingFor returns the grouping of the alerts that rule matches: the
// routing block's, with what the rule sets in its place.
func (r *Routing) GroupingFor(rule RoutingRule) Grouping {
	g := r.Grouping()
	if rule.GroupBy != nil {
		g.By = rule.GroupBy
	}
	if rule.GroupWait != nil {
		g.Wait = time.Duration(*rule.GroupWait)
	}
	if rule.GroupInterval != nil {
		g.Interval = time.Duration(*rule.GroupInterval)
	}
	return g
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

// Auth says how people sign in. It is settings, not an object: it has no
// name and no row in the database.
type Auth struct {
	// PublicURL is the address at which browsers reach Beacon, such as
	// https://beacon.example.com. The identity provider sends people back
	// to it, and its scheme decides whether cookies are marked Secure.
	PublicURL string `yaml:"public_url" json:"public_url"`
	// SessionLifetime is how long a sign-in lasts. Nil means
	// DefaultSessionLifetime.
	SessionLifetime *Duration `yaml:"session_lifetime" json:"session_lifetime,omitempty"`
	// OIDC is the identity provider. Nil means there is none.
	OIDC *OIDC `yaml:"oidc" json:"oidc,omitempty"`
	// Roles maps the provider's roles or groups to Beacon's roles. Nil
	// means that everyone who can sign in is a member and nobody is an
	// admin.
	Roles *RoleMapping `yaml:"roles" json:"roles,omitempty"`
	// BootstrapAdmin is an admin account that needs no identity provider.
	// Nil means it is off.
	BootstrapAdmin *BootstrapAdmin `yaml:"bootstrap_admin" json:"bootstrap_admin,omitempty"`

	Origin Origin `yaml:"-" json:"-"`
}

// DefaultSessionLifetime is how long a sign-in lasts unless the
// configuration says otherwise.
const DefaultSessionLifetime = Duration(12 * time.Hour)

// Lifetime returns how long a sign-in lasts.
func (a Auth) Lifetime() time.Duration {
	if a.SessionLifetime != nil {
		return time.Duration(*a.SessionLifetime)
	}
	return time.Duration(DefaultSessionLifetime)
}

// OIDC is an OpenID Connect identity provider and Beacon's registration
// with it.
type OIDC struct {
	// Issuer is the provider's issuer URL, under which its discovery
	// document is found.
	Issuer       string `yaml:"issuer" json:"issuer"`
	ClientID     string `yaml:"client_id" json:"client_id"`
	ClientSecret Secret `yaml:"client_secret" json:"client_secret"`
	// Scopes are requested at sign-in. Empty means DefaultScopes.
	Scopes []string `yaml:"scopes" json:"scopes,omitempty"`
	// GroupsClaim names the claim of the ID token that carries the person's
	// roles or groups. Empty means DefaultGroupsClaim.
	GroupsClaim string `yaml:"groups_claim" json:"groups_claim,omitempty"`
}

// Claim returns the name of the claim that carries roles or groups.
func (o OIDC) Claim() string {
	if o.GroupsClaim != "" {
		return o.GroupsClaim
	}
	return DefaultGroupsClaim
}

// BootstrapAdmin is an admin account for an install that has no identity
// provider yet, or whose provider maps nobody to admin yet. Taking it out of
// the configuration turns it off.
type BootstrapAdmin struct {
	// PasswordHash is the bcrypt hash of the account's password, as
	// `beacon hash-password` prints it. Beacon never sees the password
	// except when someone signs in with it.
	PasswordHash Secret `yaml:"password_hash" json:"password_hash"`
}

// DefaultGroupsClaim is the claim that carries a person's roles or groups
// unless the configuration says otherwise.
const DefaultGroupsClaim = "groups"

// RoleMapping maps the identity provider's roles or groups to Beacon's two
// roles. The names are compared with the values of the groups claim.
type RoleMapping struct {
	// Admin lists the groups whose members are admins.
	Admin []string `yaml:"admin" json:"admin,omitempty"`
	// Member lists the groups whose members are members. When it is empty,
	// everyone who can sign in is a member.
	Member []string `yaml:"member" json:"member,omitempty"`
}

// DefaultScopes are the scopes requested at sign-in unless the configuration
// says otherwise.
var DefaultScopes = []string{"openid", "profile", "email"}

// RequestedScopes returns the scopes to request at sign-in.
func (o OIDC) RequestedScopes() []string {
	if len(o.Scopes) > 0 {
		return o.Scopes
	}
	return DefaultScopes
}

// MaxMatcherLength bounds a regular expression that matches label values,
// in a routing rule or in a silence. Go's regular expressions run in time
// linear in their input whatever the pattern, so the bound is on the cost of
// compiling and keeping a pattern, not on a pathological one.
const MaxMatcherLength = 1024

// CompileMatcher compiles a regular expression that matches label values.
// It is anchored at both ends, as in Prometheus: "5.." matches "500" and not
// "1500". The syntax is RE2's, which Go's regexp package implements.
func CompileMatcher(expr string) (*regexp.Regexp, error) {
	if len(expr) > MaxMatcherLength {
		return nil, fmt.Errorf("is longer than %d characters", MaxMatcherLength)
	}
	re, err := regexp.Compile("^(?:" + expr + ")$")
	if err != nil {
		return nil, fmt.Errorf("is not a regular expression: %s", strings.TrimPrefix(err.Error(), "error parsing regexp: "))
	}
	return re, nil
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ValidName reports whether name can identify a provisioned object: lowercase
// letters, digits and hyphens, starting with a letter, at most 63 characters.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}
