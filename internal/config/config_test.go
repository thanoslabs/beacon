package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

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
		{name: "uppercase name", change: func(s *Source) { s.Name = "Prod" }, want: `source "Prod": name must be`},
		{name: "empty name", change: func(s *Source) { s.Name = "" }, want: `source "": name must be`},
		{name: "name with slash", change: func(s *Source) { s.Name = "a/b" }, want: "name must be"},
		{name: "long name", change: func(s *Source) { s.Name = "a" + strings.Repeat("b", 63) }, want: "name must be"},
		{name: "unknown type", change: func(s *Source) { s.Type = "alertmanager" }, want: `type "alertmanager" is not supported`},
		{name: "missing token", change: func(s *Source) { s.Token = Secret{} }, want: "token must be at least 16 characters"},
		{name: "short token", change: func(s *Source) { s.Token = NewSecret(SecretRef{}, "hunter2") }, want: "token must be at least 16 characters"},
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
	if err == nil || !strings.Contains(err.Error(), "name is used twice") {
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
