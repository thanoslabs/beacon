// Package config holds the typed form of Beacon's provisioned objects: the
// sources, integrations and so on that docs/configuration-and-data-model.md
// describes.
//
// Nothing here reads files. Until the YAML loader exists, beacon serve fills
// these types from a few flags and environment variables; the loader will fill
// the same types.
package config

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/thanoslabs/beacon/internal/alert"
)

// Config is everything provisioned.
type Config struct {
	Sources      []Source
	Integrations []Integration

	// Notify lists the shared channels that are told about every alert.
	//
	// Temporary: it stands in for routing and escalation policies, whose
	// steps will name their targets. It is not part of the file schema.
	Notify []ChannelTarget
}

// SourceTypeWebhook is the generic JSON webhook source type.
const SourceTypeWebhook = "webhook"

// Source is an authenticated endpoint that receives alerts.
type Source struct {
	// Name is the identity, and the last segment of the ingest URL.
	Name string `json:"name"`
	Type string `json:"type"`
	// Token is what the sender presents as a bearer token.
	Token Secret `json:"token"`
	// Labels are added to every alert from this source. They do not replace
	// a label the alert already has.
	Labels   map[string]string `json:"labels,omitempty"`
	Severity SeverityMapping   `json:"severity"`
}

// SeverityMapping says how a source's alerts get their severity.
type SeverityMapping struct {
	// Label names the label that carries severity. Empty means "severity".
	Label string `json:"label,omitempty"`
	// Mapping maps label values to Beacon's levels. When empty, the names of
	// the levels map to themselves.
	Mapping map[string]alert.Severity `json:"mapping,omitempty"`
	// Default applies when the label is missing or unmapped. Empty means
	// warning.
	Default alert.Severity `json:"default,omitempty"`
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

// IntegrationTypeSlack is the Slack integration type.
const IntegrationTypeSlack = "slack"

// Integration is a connection to a notification service.
type Integration struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Slack holds the settings when Type is IntegrationTypeSlack.
	Slack *SlackIntegration `json:"slack,omitempty"`
}

// SlackIntegration holds the credentials of a Slack app.
type SlackIntegration struct {
	// BotToken is the app's bot user OAuth token.
	BotToken Secret `json:"bot_token"`
	// SigningSecret verifies that requests come from Slack.
	SigningSecret Secret `json:"signing_secret"`
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

// minTokenLength is the shortest source token accepted. A token is the only
// thing that stops anyone who can reach Beacon from sending alerts.
const minTokenLength = 16

// Validate checks the configuration. Its errors name the object and field at
// fault, and never contain a secret.
func (c Config) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for _, s := range c.Sources {
		at := fmt.Sprintf("source %q", s.Name)
		switch {
		case !ValidName(s.Name):
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits and hyphens, start with a letter and be at most 63 characters", at))
		case seen[s.Name]:
			errs = append(errs, fmt.Errorf("%s: name is used twice", at))
		}
		seen[s.Name] = true
		if s.Type != SourceTypeWebhook {
			errs = append(errs, fmt.Errorf("%s: type %q is not supported; use %q", at, s.Type, SourceTypeWebhook))
		}
		if len(s.Token.Reveal()) < minTokenLength {
			errs = append(errs, fmt.Errorf("%s: token must be at least %d characters", at, minTokenLength))
		}
		for value, level := range s.Severity.Mapping {
			if !level.Valid() {
				errs = append(errs, fmt.Errorf("%s: severity.mapping.%s: %q is not critical, warning or info", at, value, level))
			}
		}
		if d := s.Severity.Default; d != "" && !d.Valid() {
			errs = append(errs, fmt.Errorf("%s: severity.default: %q is not critical, warning or info", at, d))
		}
	}

	integrations := map[string]bool{}
	for _, i := range c.Integrations {
		at := fmt.Sprintf("integration %q", i.Name)
		switch {
		case !ValidName(i.Name):
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits and hyphens, start with a letter and be at most 63 characters", at))
		case integrations[i.Name]:
			errs = append(errs, fmt.Errorf("%s: name is used twice", at))
		}
		integrations[i.Name] = true
		switch {
		case i.Type != IntegrationTypeSlack:
			errs = append(errs, fmt.Errorf("%s: type %q is not supported; use %q", at, i.Type, IntegrationTypeSlack))
		case i.Slack == nil:
			errs = append(errs, fmt.Errorf("%s: slack: the settings are missing", at))
		case !i.Slack.BotToken.IsSet():
			errs = append(errs, fmt.Errorf("%s: slack.bot_token is required", at))
		}
	}
	for n, t := range c.Notify {
		at := fmt.Sprintf("notify target %d", n+1)
		if t.Channel == "" {
			errs = append(errs, fmt.Errorf("%s: channel is required", at))
		}
		if !integrations[t.Integration] {
			errs = append(errs, fmt.Errorf("%s: integration %q does not exist", at, t.Integration))
		}
	}
	return errors.Join(errs...)
}
