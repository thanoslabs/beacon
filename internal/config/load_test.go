package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// exampleEnv holds the secrets that testdata/example refers to.
var exampleEnv = map[string]string{
	"BEACON_SOURCE_PROD_ALERTMANAGER_TOKEN": "alertmanager-" + secretValue,
	"BEACON_SLACK_BOT_TOKEN":                "xoxb-" + secretValue,
	"BEACON_SLACK_SIGNING_SECRET":           "signing-" + secretValue,
	"BEACON_SMTP_PASSWORD":                  "smtp-" + secretValue,
	"BEACON_DISCORD_BOT_TOKEN":              "discord-" + secretValue,
	"BEACON_TICKETING_AUTHORIZATION":        "Bearer " + secretValue,
	"BEACON_OIDC_CLIENT_SECRET":             "oidc-" + secretValue,
}

func getenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

func TestLoadDirectory(t *testing.T) {
	cfg, err := Load("testdata/example", LoadOptions{Getenv: getenv(exampleEnv)})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Every kind is loaded, from both files, and the hidden files are not.
	if len(cfg.Sources) != 2 || len(cfg.Integrations) != 4 || len(cfg.Teams) != 1 ||
		len(cfg.Schedules) != 1 || len(cfg.EscalationPolicies) != 1 || cfg.Routing == nil || len(cfg.Routing.Rules) != 1 {
		t.Fatalf("loaded %d sources, %d integrations, %d teams, %d schedules, %d policies, routing %v",
			len(cfg.Sources), len(cfg.Integrations), len(cfg.Teams), len(cfg.Schedules), len(cfg.EscalationPolicies), cfg.Routing)
	}

	src := cfg.Sources[0]
	if src.Name != "prod-alertmanager" || src.Type != SourceTypeAlertmanager || src.Labels["environment"] != "production" ||
		src.Severity.Mapping["page"] != "critical" || src.Severity.Default != "warning" {
		t.Errorf("source = %+v", src)
	}
	if got := src.Token.Reveal(); got != exampleEnv["BEACON_SOURCE_PROD_ALERTMANAGER_TOKEN"] {
		t.Errorf("token from the environment was not read")
	}
	if o := src.Origin; o.File != filepath.Join("testdata", "example", "base.yaml") || o.Line != 4 || o.lineOf("severity.default") != 15 {
		t.Errorf("origin = %+v", o)
	}
	// A secret file is read relative to the file that names it, without the
	// trailing newline.
	if got := cfg.Sources[1].Token.Reveal(); got != "grafana-token-from-a-file-0123456789" {
		t.Errorf("token from a file = %q", got)
	}

	hook := cfg.Integrations[3].Webhook
	if hook == nil || hook.Headers["X-Team"].Reveal() != "platform" || hook.Headers["Authorization"].Reveal() != exampleEnv["BEACON_TICKETING_AUTHORIZATION"] {
		t.Errorf("webhook integration = %+v", hook)
	}
	if email := cfg.Integrations[1].Email; email == nil || email.Port != 587 || email.TLS != EmailTLSStartTLS {
		t.Errorf("email integration = %+v", email)
	}
	if d := cfg.Integrations[2].Discord; d == nil || d.ApplicationID != "1234567890123456789" {
		t.Errorf("discord integration = %+v", d)
	}

	sched := cfg.Schedules[0]
	if sched.Rotation.ShiftLength != Duration(Week) || sched.Rotation.Handoff.Time != "09:00" || sched.Rotation.Handoff.Day != "monday" ||
		len(sched.Rotation.Members) != 3 || sched.EffectiveFrom == nil || !time.Time(*sched.EffectiveFrom).Equal(time.Date(2026, 12, 7, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("schedule = %+v", sched)
	}
	policy := cfg.EscalationPolicies[0]
	if policy.Repeat != 2 || len(policy.Steps) != 3 || policy.Steps[0].Timeout != Duration(5*time.Minute) ||
		policy.Steps[0].Notify[1].Channel != "#platform-alerts" || policy.Steps[0].Notify[1].Integration != "slack" || policy.Steps[2].Notify[0].Via[0] != "email" {
		t.Errorf("policy = %+v", policy)
	}
	rule := cfg.Routing.Rules[0]
	if rule.Match["team"] != "platform" || rule.MatchRE["severity"] != "critical|page" || *rule.GroupWait != Duration(10*time.Second) ||
		cfg.Routing.Default.EscalationPolicy != "platform-default" || rule.Origin.Line != 62 {
		t.Errorf("routing = %+v", cfg.Routing)
	}

	a := cfg.Auth
	if a == nil || a.PublicURL != "https://beacon.example.com" || a.Lifetime() != 8*time.Hour || a.OIDC == nil || a.OIDC.Issuer != "https://idp.example.com" ||
		a.OIDC.ClientSecret.Reveal() != exampleEnv["BEACON_OIDC_CLIENT_SECRET"] || len(a.OIDC.RequestedScopes()) != 4 {
		t.Errorf("auth = %+v", a)
	}
	if (Auth{}).Lifetime() != 12*time.Hour || len((OIDC{}).RequestedScopes()) != 3 {
		t.Error("the defaults of auth are wrong")
	}

	// What is stored and logged holds references, never values.
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), secretValue) || strings.Contains(string(encoded), "grafana-token-from-a-file") {
		t.Errorf("the stored form contains a secret: %s", encoded)
	}
	for _, want := range []string{`"Authorization":{"env":"BEACON_TICKETING_AUTHORIZATION"}`, `"X-Team":"platform"`, `"token":{"file":"secrets/grafana-source-token"}`, `"shift_length":"1w"`, `"effective_from":"2026-12-07T09:00:00Z"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("the stored form lacks %s: %s", want, encoded)
		}
	}
}

func TestLoadWithoutSecrets(t *testing.T) {
	cfg, err := Load("testdata/example", LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tok := cfg.Sources[0].Token
	if tok.IsSet() || !tok.IsReferenced() || tok.Ref().Env != "BEACON_SOURCE_PROD_ALERTMANAGER_TOKEN" {
		t.Errorf("token = %#v, ref %+v", tok, tok.Ref())
	}
}

// write puts files into a new directory and returns it.
func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// loadErrors loads dir and returns its errors with the directory removed
// from the file names.
func loadErrors(t *testing.T, dir string, opts LoadOptions) []string {
	t.Helper()
	_, err := Load(dir, opts)
	var errs Errors
	if err != nil && !errors.As(err, &errs) {
		t.Fatalf("Load returned %T, want Errors: %v", err, err)
	}
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = strings.TrimPrefix(e.Error(), dir+string(filepath.Separator))
	}
	return out
}

func TestLoadSingleFile(t *testing.T) {
	dir := write(t, map[string]string{"beacon.yaml": "teams:\n  - name: platform\n", "other.yaml": "teams: 3\n"})
	cfg, err := Load(filepath.Join(dir, "beacon.yaml"), LoadOptions{})
	if err != nil || len(cfg.Teams) != 1 {
		t.Fatalf("Load = %+v, %v", cfg, err)
	}
	// Empty files and files of comments are fine.
	dir = write(t, map[string]string{"a.yaml": "", "b.yaml": "# nothing yet\n"})
	if _, err := Load(dir, LoadOptions{}); err != nil {
		t.Errorf("empty files: %v", err)
	}
}

func TestLoadReportsEveryStructuralError(t *testing.T) {
	const inline = "inline-secret-marker-3e9a"
	dir := write(t, map[string]string{
		"a.yaml": `sources:
  - name: prod
    type: webhook
    token: ` + inline + `
    colour: blue
    severity:
      mapping: [` + inline + `]
  - name: other
    token: {env: A, file: /b}
  - name: third
    token: {vault: x}
  - token:
      env: ""
integrations:
  - name: slack
    type: slack
    slack:
      bot_token: [` + inline + `]
      signing_secret: {}
      workspace: T1
  - name: email
    type: email
    email:
      port: many
routing:
  group_wait: soon
  default: {escalation_policy: p}
  rules:
    - name: r1
      escalation_policy: p
      match: 5
`,
		"b.yaml": `teams:
  - name: platform
    members: ada@example.com
  - name: twice
    name: again
escalation_policies:
  - name: p
    repeat: twice
    steps:
      - notify: [{user: ada@example.com, sms: true}]
        timeout: 5 minutes
schedules:
  - name: s
    effective_from: tomorrow
dashboards: []
routing:
  default: {escalation_policy: p}
auth:
  public_url: https://beacon.example.com
  oidc: {client_secret: inline-secret-marker-3e9a, tenant: x}
`,
		"e.yaml": "auth: {public_url: \"https://beacon.example.com\"}\n",
		"c.yaml": "sources: [\n",
		"d.yaml": "teams: []\n---\nteams: []\n",
	})
	got := loadErrors(t, dir, LoadOptions{})
	want := []string{
		`a.yaml:4: source "prod": token: secrets are never written inline; use {env: NAME} or {file: /path}`,
		`a.yaml:5: source "prod": colour: unknown field`,
		`a.yaml:7: source "prod": severity.mapping: must be a mapping of names to values, not a list`,
		`a.yaml:9: source "other": token: a secret takes exactly one of env and file`,
		`a.yaml:11: source "third": token: unknown field "vault"; a secret takes env or file`,
		`a.yaml:13: source 4: token: must be the name of an environment variable or the path of a file`,
		`a.yaml:18: integration "slack": slack.bot_token: secrets are never written inline; use {env: NAME} or {file: /path}`,
		`a.yaml:19: integration "slack": slack.signing_secret: a secret takes exactly one of env and file`,
		`a.yaml:20: integration "slack": slack.workspace: unknown field`,
		`a.yaml:24: integration "email": email.port: must be a whole number, not a string`,
		`a.yaml:26: routing: group_wait: "soon" is not a duration such as 30s, 5m, 12h, 1d or 1w`,
		`a.yaml:31: routing rule "r1": match: must be a mapping of names to values, not a number`,
		`b.yaml:3: team "platform": members: must be a list, not a string`,
		`b.yaml:5: team "twice": name: the field is set twice; first at line 4`,
		`b.yaml:8: escalation policy "p": repeat: must be a whole number, not a string`,
		`b.yaml:10: escalation policy "p": steps[0].notify[0].sms: unknown field`,
		`b.yaml:11: escalation policy "p": steps[0].timeout: "5 minutes" is not a duration such as 30s, 5m, 12h, 1d or 1w`,
		`b.yaml:14: schedule "s": effective_from: must be a date and time with a zone, such as 2026-01-05T09:00:00Z`,
		`b.yaml:15: dashboards: unknown field`,
		`b.yaml:17: routing: routing is already defined in ` + filepath.Join(dir, "a.yaml") + `; its rules are ordered, so the whole block must be in one file`,
		`b.yaml:20: auth: oidc.client_secret: secrets are never written inline; use {env: NAME} or {file: /path}`,
		`b.yaml:20: auth: oidc.tenant: unknown field`,
		`c.yaml:1: invalid YAML: did not find expected node content`,
		`d.yaml: the file holds more than one YAML document; use one document per file`,
		`e.yaml:1: auth: auth is already defined in ` + filepath.Join(dir, "b.yaml") + `; it is one group of settings, so it must be in one file`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("errors:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A secret typed into a file must not be repeated by the error.
	if strings.Contains(strings.Join(got, "\n"), inline) {
		t.Errorf("an error repeats an inline secret:\n%s", strings.Join(got, "\n"))
	}
}

func TestLoadSecretErrors(t *testing.T) {
	dir := write(t, map[string]string{
		"a.yaml": `sources:
  - name: prod
    type: webhook
    token: {env: BEACON_MISSING}
  - name: filed
    type: webhook
    token: {file: missing-token}
  - name: empty
    type: webhook
    token: {file: empty-token}
`,
		"empty-token": "\n",
	})
	got := loadErrors(t, dir, LoadOptions{Getenv: getenv(nil)})
	want := []string{
		`a.yaml:4: source "prod": token: the environment variable BEACON_MISSING is not set`,
		`a.yaml:7: source "filed": token: read the secret file: open ` + filepath.Join(dir, "missing-token") + `: no such file or directory`,
		`a.yaml:10: source "empty": token: the secret file ` + filepath.Join(dir, "empty-token") + ` is empty`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("errors:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without Getenv nothing is read, so nothing is missing.
	if got := loadErrors(t, dir, LoadOptions{}); len(got) != 0 {
		t.Errorf("errors without secrets: %v", got)
	}
}

func TestLoadPathErrors(t *testing.T) {
	empty := write(t, map[string]string{"notes.txt": "x"})
	for path, want := range map[string]string{
		filepath.Join(empty, "nope.yaml"): "cannot read the configuration: no such file or directory",
		empty:                             "the directory holds no *.yaml or *.yml file",
	} {
		_, err := Load(path, LoadOptions{})
		if err == nil || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), path+": ") {
			t.Errorf("Load(%s) = %v, want %q", path, err, want)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30s": 30 * time.Second, "5m": 5 * time.Minute, "12h": 12 * time.Hour, "1d": Day, "2w": 2 * Week, "1h30m": 90 * time.Minute, "0s": 0,
	} {
		got, err := ParseDuration(in)
		if err != nil || time.Duration(got) != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "5", "m", "5 m", "-5m", "1.5h", "5m1h", "10ms", "99999999999999999999w", "9999999999w"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", in)
		}
	}
	for d, want := range map[time.Duration]string{Week: "1w", 36 * time.Hour: "1d12h", 90 * time.Second: "1m30s", 0: "0s"} {
		if got := Duration(d).String(); got != want {
			t.Errorf("Duration(%v).String() = %q, want %q", d, got, want)
		}
	}
}
