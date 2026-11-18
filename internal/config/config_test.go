package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
)

const secretValue = "s3cr3t-token-value-0123456789"

func testSource() Source {
	return Source{
		Name:  "prod",
		Type:  SourceTypeWebhook,
		Token: NewSecret(SecretRef{Env: "BEACON_SOURCE_TOKEN"}, secretValue),
	}
}

// TestSecretIsNeverPrinted guards against a credential reaching output by
// accident: every common way of printing, logging or encoding a value that
// holds a Secret must leave the secret out.
func TestSecretIsNeverPrinted(t *testing.T) {
	src := testSource()
	cfg := Config{Sources: []Source{src}}
	ptr := &src

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.Info("secret", slog.Any("token", src.Token), slog.Any("source", src), slog.Any("config", cfg))
	text := slog.New(slog.NewTextHandler(&logs, nil))
	text.Info("secret", slog.Any("token", src.Token), slog.Any("source", src), slog.Any("pointer", ptr))

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	outputs := map[string]string{
		"%v secret":   fmt.Sprintf("%v", src.Token),
		"%q secret":   fmt.Sprintf("%q", src.Token),
		"%+v secret":  fmt.Sprintf("%+v", src.Token),
		"%#v secret":  fmt.Sprintf("%#v", src.Token),
		"%v source":   fmt.Sprintf("%v", src),
		"%+v source":  fmt.Sprintf("%+v", src),
		"%#v source":  fmt.Sprintf("%#v", src),
		"%+v pointer": fmt.Sprintf("%+v", ptr),
		"%+v config":  fmt.Sprintf("%+v", cfg),
		"%#v config":  fmt.Sprintf("%#v", cfg),
		"Sprint":      fmt.Sprint(src.Token, src, cfg),
		"error":       fmt.Errorf("bad source %v: %w", src, fmt.Errorf("token %s", src.Token)).Error(),
		"slog":        logs.String(),
		"json":        string(encoded),
	}
	for name, out := range outputs {
		if strings.Contains(out, secretValue) {
			t.Errorf("%s prints the secret: %s", name, out)
		}
	}
	if !strings.Contains(outputs["%v secret"], "[redacted]") {
		t.Errorf("%%v of a secret = %q, want [redacted]", outputs["%v secret"])
	}
	// The stored form names where the secret comes from.
	if want := `"token":{"env":"BEACON_SOURCE_TOKEN"}`; !strings.Contains(string(encoded), want) {
		t.Errorf("json = %s, want it to contain %s", encoded, want)
	}
	if src.Token.Reveal() != secretValue || !src.Token.IsSet() || (Secret{}).IsSet() {
		t.Error("Reveal or IsSet is wrong")
	}
}

func TestSeverityMappingResolve(t *testing.T) {
	custom := SeverityMapping{
		Label:   "priority",
		Mapping: map[string]alert.Severity{"page": alert.SeverityCritical, "ticket": alert.SeverityInfo},
		Default: alert.SeverityInfo,
	}
	tests := []struct {
		name    string
		mapping SeverityMapping
		labels  map[string]string
		want    alert.Severity
	}{
		{name: "level name maps to itself", labels: map[string]string{"severity": "critical"}, want: alert.SeverityCritical},
		{name: "info", labels: map[string]string{"severity": "info"}, want: alert.SeverityInfo},
		{name: "missing label defaults to warning", labels: map[string]string{"alertname": "x"}, want: alert.SeverityWarning},
		{name: "unknown value defaults to warning", labels: map[string]string{"severity": "page"}, want: alert.SeverityWarning},
		{name: "custom label and mapping", mapping: custom, labels: map[string]string{"priority": "page", "severity": "info"}, want: alert.SeverityCritical},
		{name: "custom mapping does not pass level names through", mapping: custom, labels: map[string]string{"priority": "critical"}, want: alert.SeverityInfo},
		{name: "custom default", mapping: custom, labels: nil, want: alert.SeverityInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.mapping.Resolve(tt.labels); got != tt.want {
				t.Errorf("Resolve = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	if err := (Config{Sources: []Source{testSource()}}).Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("empty config: %v", err)
	}

	tests := []struct {
		name   string
		change func(*Source)
		want   string
	}{
		{name: "uppercase name", change: func(s *Source) { s.Name = "Prod" }, want: `source "Prod": name: must be`},
		{name: "empty name", change: func(s *Source) { s.Name = "" }, want: `source "": name: must be`},
		{name: "name with slash", change: func(s *Source) { s.Name = "a/b" }, want: "name: must be"},
		{name: "long name", change: func(s *Source) { s.Name = "a" + strings.Repeat("b", 63) }, want: "name: must be"},
		{name: "unknown type", change: func(s *Source) { s.Type = "pager" }, want: `type: "pager" is not alertmanager, grafana or webhook`},
		{name: "missing token", change: func(s *Source) { s.Token = Secret{} }, want: "token: is required"},
		{name: "short token", change: func(s *Source) { s.Token = NewSecret(SecretRef{}, "hunter2") }, want: "token: must be at least 16 characters"},
		{name: "bad mapping", change: func(s *Source) { s.Severity.Mapping = map[string]alert.Severity{"page": "urgent"} }, want: `severity.mapping.page: "urgent" is not`},
		{name: "bad default", change: func(s *Source) { s.Severity.Default = "urgent" }, want: `severity.default: "urgent" is not`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := testSource()
			tt.change(&src)
			err := Config{Sources: []Source{src}}.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), secretValue) {
				t.Errorf("the error contains the token: %v", err)
			}
		})
	}

	err := Config{Sources: []Source{testSource(), testSource()}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "the name is used twice") {
		t.Errorf("duplicate names: Validate = %v", err)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"a", "prod-alertmanager", "x1", "a" + strings.Repeat("b", 62)} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "1a", "-a", "a_b", "A", "a b", "a.b"} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) = true", name)
		}
	}
}

func testSlack() Integration {
	return Integration{Name: "slack", Type: IntegrationTypeSlack, Slack: &SlackIntegration{
		BotToken:      NewSecret(SecretRef{Env: "BEACON_SLACK_BOT_TOKEN"}, "xoxb-"+secretValue),
		SigningSecret: NewSecret(SecretRef{Env: "BEACON_SLACK_SIGNING_SECRET"}, "signing-"+secretValue),
	}}
}

func TestValidateIntegrations(t *testing.T) {
	valid := Config{Integrations: []Integration{testSlack()}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}

	tests := []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{name: "bad name", change: func(c *Config) { c.Integrations[0].Name = "Slack!" }, want: `integration "Slack!": name: must be`},
		{name: "duplicate", change: func(c *Config) { c.Integrations = append(c.Integrations, testSlack()) }, want: `integration "slack": name: the name is used twice`},
		{name: "unknown type", change: func(c *Config) { c.Integrations[0].Type = "pager" }, want: `type: "pager" is not slack, discord, email or webhook`},
		{name: "missing settings", change: func(c *Config) { c.Integrations[0].Slack = nil }, want: "slack: the settings are missing"},
		{name: "missing bot token", change: func(c *Config) { c.Integrations[0].Slack.BotToken = Secret{} }, want: "slack.bot_token: is required"},
		{name: "missing signing secret", change: func(c *Config) { c.Integrations[0].Slack.SigningSecret = Secret{} }, want: "slack.signing_secret: is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Integrations: []Integration{testSlack()}}
			tt.change(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secretValue) {
				t.Errorf("the error contains a secret: %v", err)
			}
		})
	}
}

func TestIntegrationSecretsAreNeverPrinted(t *testing.T) {
	cfg := Config{Integrations: []Integration{testSlack()}}
	encoded, err := json.Marshal(cfg.Integrations[0])
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("integration", slog.Any("integration", cfg.Integrations[0]), slog.Any("config", cfg))
	for name, out := range map[string]string{
		"%+v":  fmt.Sprintf("%+v %+v", cfg, cfg.Integrations[0].Slack),
		"%#v":  fmt.Sprintf("%#v %#v", cfg, *cfg.Integrations[0].Slack),
		"json": string(encoded),
		"slog": logs.String(),
	} {
		if strings.Contains(out, secretValue) {
			t.Errorf("%s prints a secret: %s", name, out)
		}
	}
	want := `{"name":"slack","type":"slack","slack":{"bot_token":{"env":"BEACON_SLACK_BOT_TOKEN"},"signing_secret":{"env":"BEACON_SLACK_SIGNING_SECRET"}}}`
	if string(encoded) != want {
		t.Errorf("stored form = %s, want %s", encoded, want)
	}
}

func TestGrouping(t *testing.T) {
	// Without a routing block, and with one that says nothing: the defaults.
	want := Grouping{By: []string{"alertname"}, Wait: 30 * time.Second, Interval: 5 * time.Minute}
	for name, r := range map[string]*Routing{"no block": nil, "an empty block": {}} {
		if got := r.Grouping(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Grouping = %+v, want %+v", name, got, want)
		}
	}

	cfg, err := Read("testdata/example", LoadOptions{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	block := Grouping{By: []string{"alertname", "cluster"}, Wait: 30 * time.Second, Interval: 5 * time.Minute}
	if got := cfg.Routing.Grouping(); !reflect.DeepEqual(got, block) {
		t.Errorf("Grouping = %+v, want %+v", got, block)
	}
	// A rule replaces what it sets and keeps the rest.
	rule := cfg.Routing.Rules[0]
	if got, want := cfg.Routing.GroupingFor(rule), (Grouping{By: block.By, Wait: 10 * time.Second, Interval: block.Interval}); !reflect.DeepEqual(got, want) {
		t.Errorf("GroupingFor(%s) = %+v, want %+v", rule.Name, got, want)
	}
	zero := Duration(0)
	rule.GroupBy, rule.GroupWait = []string{}, &zero
	if got := cfg.Routing.GroupingFor(rule); len(got.By) != 0 || got.Wait != 0 {
		t.Errorf("a rule that groups by nothing and does not wait: %+v", got)
	}
}

func TestCompileMatcher(t *testing.T) {
	re, err := CompileMatcher("critical|page")
	if err != nil {
		t.Fatalf("CompileMatcher: %v", err)
	}
	for value, want := range map[string]bool{"critical": true, "page": true, "pager": false, "non-critical": false, "": false} {
		if got := re.MatchString(value); got != want {
			t.Errorf("critical|page matches %q = %v, want %v", value, got, want)
		}
	}
	if _, err := CompileMatcher("(critical"); err == nil || !strings.Contains(err.Error(), "is not a regular expression: missing closing )") {
		t.Errorf("an unbalanced pattern: %v", err)
	}
	if _, err := CompileMatcher(strings.Repeat("a", MaxMatcherLength)); err != nil {
		t.Errorf("a pattern of the greatest length: %v", err)
	}
	if _, err := CompileMatcher(strings.Repeat("a", MaxMatcherLength+1)); err == nil || !strings.Contains(err.Error(), "is longer than 1024 characters") {
		t.Errorf("a pattern that is too long: %v", err)
	}
	// Go refuses a pattern whose repetitions would make a huge program.
	if _, err := CompileMatcher("((a{1000}){1000}){1000}"); err == nil {
		t.Error("a pattern with nested large repetitions compiled")
	}
}
