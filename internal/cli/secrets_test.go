package cli

import (
	"strings"
	"testing"
)

// secretSettings lists every setting that holds a credential, by the
// environment variable that carries it, with a marker value that is long
// enough to pass validation. Add new secrets here and to secretsConfig: the
// test below then checks that their values never reach the command's output.
var secretSettings = map[string]string{
	"BEACON_DATABASE_URL":         "postgres://beacon:db-pass-marker-1f9a@127.0.0.1:1/beacon?sslmode=disable",
	"BEACON_SOURCE_TOKEN":         "source-token-marker-7c2e41d0",
	"BEACON_SLACK_BOT_TOKEN":      "xoxb-bot-token-marker-55d1",
	"BEACON_SLACK_SIGNING_SECRET": "signing-secret-marker-e07b",
	"BEACON_SMTP_PASSWORD":        "smtp-password-marker-6a3d",
	"BEACON_DISCORD_BOT_TOKEN":    "discord-token-marker-0c8f",
	"BEACON_HOOK_AUTHORIZATION":   "Bearer hook-marker-b7e2",
	"BEACON_HOOK_SIGNING_SECRET":  "hook-signing-secret-marker-4d1a",
	"BEACON_OIDC_CLIENT_SECRET":   "client-secret-marker-93fa",
	// A bcrypt hash of a password that is used nowhere. The hash is a
	// credential too: it must not be printed either.
	"BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W",
}

// secretsConfig refers to every secret a configuration can hold.
const secretsConfig = slackConfig + `  - name: email
    type: email
    email: {host: smtp.example.com, port: 587, from: beacon@example.com, tls: starttls, username: beacon, password: {env: BEACON_SMTP_PASSWORD}}
  - name: discord
    type: discord
    discord:
      bot_token: {env: BEACON_DISCORD_BOT_TOKEN}
      public_key: 5f1c0c8e7a9b4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e
      application_id: "1234567890123456789"
  - name: hook
    type: webhook
    webhook: {url: "https://tickets.example.com/hook", signing_secret: {env: BEACON_HOOK_SIGNING_SECRET}, headers: {Authorization: {env: BEACON_HOOK_AUTHORIZATION}}}
`

// secretMarkers are the substrings that must never be printed.
var secretMarkers = []string{"db-pass-marker-1f9a", "source-token-marker-7c2e41d0", "bot-token-marker-55d1", "signing-secret-marker-e07b",
	"smtp-password-marker-6a3d", "discord-token-marker-0c8f", "hook-marker-b7e2", "hook-signing-secret-marker-4d1a", "client-secret-marker-93fa", "2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W"}

func assertNoSecrets(t *testing.T, what, out string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if strings.Contains(out, m) {
			t.Errorf("%s contains the secret %q:\n%s", what, m, out)
		}
	}
}

// TestSecretsAreNeverPrinted runs the commands in the ways that print
// settings back (help, usage errors, validation errors, a failed start) with
// every secret set, and checks that no secret value appears in the output.
func TestSecretsAreNeverPrinted(t *testing.T) {
	// The integrations go before the policies in slackConfig, so the extra
	// ones are spliced in after the Slack integration.
	head, tail, _ := strings.Cut(slackConfig, "escalation_policies:")
	extra := strings.TrimPrefix(secretsConfig, slackConfig)
	// Sign-in is pointed at a provider that does not answer, so the client
	// secret is in use while the start fails.
	tail += `auth:
  public_url: https://beacon.example.com
  oidc: {issuer: "http://127.0.0.1:1/idp", client_id: beacon, client_secret: {env: BEACON_OIDC_CLIENT_SECRET}}
  bootstrap_admin: {password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}}
`
	good := writeConfig(t, head+extra+"escalation_policies:"+tail)
	// A configuration with mistakes next to its secrets.
	bad := writeConfig(t, head+extra+"  - name: slack\n    type: pager\nescalation_policies:"+tail)

	environ := map[string]string{}
	for name, value := range secretSettings {
		environ[name] = value
	}
	tests := []struct {
		name string
		args []string
	}{
		{"help", []string{"serve", "-h"}},
		{"unknown flag", []string{"serve", "--nope"}},
		{"unexpected argument", []string{"serve", "extra"}},
		{"invalid setting", []string{"serve", "--config", good, "--log-level", "loud"}},
		{"bad flag value", []string{"serve", "--shutdown-timeout", "soon"}},
		{"invalid configuration", []string{"serve", "--config", bad}},
		{"invalid Slack API URL", []string{"serve", "--config", good, "--slack-api-url", "not a url"}},
		{"invalid Discord API URL", []string{"serve", "--config", good, "--discord-api-url", "not a url"}},
		{"database unreachable", []string{"serve", "--config", good, "--database-connect-timeout", "1s", "--listen-address", "127.0.0.1:0"}},
		{"database URL in a flag", []string{"serve", "--config", good, "--database-url", secretSettings["BEACON_DATABASE_URL"], "--database-connect-timeout", "1s", "--listen-address", "127.0.0.1:0"}},
		{"validate with secrets", []string{"validate", "--config", bad, "--check-secrets"}},
		{"preview help", []string{"preview", "-h"}},
		{"preview, database unreachable", []string{"preview", "--config", good, "--database-connect-timeout", "1s"}},
		{"preview, invalid configuration", []string{"preview", "--config", bad}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLIEnv(t, environ, tt.args...)
			if !strings.HasSuffix(tt.name, "help") && code == exitOK {
				t.Errorf("exit code = 0, want a failure")
			}
			assertNoSecrets(t, "output", stdout+stderr, secretMarkers...)
		})
	}

	// The same configuration is valid, so the failures above came from what
	// each case broke and the secrets were really read.
	if code, _, stderr := runCLIEnv(t, environ, "validate", "--config", good, "--check-secrets"); code != exitOK {
		t.Fatalf("the configuration with every secret is not valid: %s", stderr)
	}
}

func TestShortSourceTokenIsRejectedWithoutEchoingIt(t *testing.T) {
	code, _, stderr := runCLIEnv(t, map[string]string{"BEACON_SOURCE_TOKEN": "hunter2"}, "serve", "--database-url", "postgres://localhost/beacon", "--config", writeConfig(t, validConfig))
	if code != exitError || !strings.Contains(stderr, `source "prod": token: must be at least 16 characters`) {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
	assertNoSecrets(t, "stderr", stderr, "hunter2")
}
