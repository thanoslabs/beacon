package cli

import (
	"strings"
	"testing"
)

// secretSettings lists every setting of beacon serve that holds a credential,
// by environment variable and flag, with a marker value that is long enough
// to pass validation. Add new secret settings here: the tests below then
// check that their values never reach the command's output.
var secretSettings = []struct{ env, flag, value string }{
	{"BEACON_DATABASE_URL", "--database-url", "postgres://beacon:db-pass-marker-1f9a@127.0.0.1:1/beacon?sslmode=disable"},
	{"BEACON_SOURCE_TOKEN", "--source-token", "source-token-marker-7c2e41d0"},
	{"BEACON_SLACK_BOT_TOKEN", "--slack-bot-token", "xoxb-bot-token-marker-55d1"},
	{"BEACON_SLACK_SIGNING_SECRET", "--slack-signing-secret", "signing-secret-marker-e07b"},
}

// secretMarkers are the substrings that must never be printed.
var secretMarkers = []string{"db-pass-marker-1f9a", "source-token-marker-7c2e41d0", "bot-token-marker-55d1", "signing-secret-marker-e07b"}

func assertNoSecrets(t *testing.T, what, out string, markers ...string) {
	t.Helper()
	for _, m := range markers {
		if strings.Contains(out, m) {
			t.Errorf("%s contains the secret %q:\n%s", what, m, out)
		}
	}
}

// TestSecretsAreNeverPrinted runs beacon serve in the ways that print
// settings back (help, usage errors, validation errors, a failed start) with
// every secret set, from the environment and from flags, and checks that no
// secret value appears in the output.
func TestSecretsAreNeverPrinted(t *testing.T) {
	environ := map[string]string{}
	var flags []string
	for _, s := range secretSettings {
		environ[s.env] = s.value
		flags = append(flags, s.flag, s.value)
	}

	tests := []struct {
		name string
		args []string
	}{
		{"help", []string{"serve", "-h"}},
		{"unknown flag", []string{"serve", "--nope"}},
		{"unexpected argument", []string{"serve", "extra"}},
		{"invalid setting", []string{"serve", "--log-level", "loud"}},
		{"invalid source name", []string{"serve", "--source-name", "Not Valid"}},
		{"bad flag value", []string{"serve", "--shutdown-timeout", "soon"}},
		{"Slack secrets without a channel", []string{"serve"}},
		{"invalid Slack API URL", []string{"serve", "--slack-channel", "C0ALERTS", "--slack-api-url", "not a url"}},
		{"database unreachable", []string{"serve", "--slack-channel", "C0ALERTS", "--database-connect-timeout", "1s", "--listen-address", "127.0.0.1:0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" with secrets in the environment", func(t *testing.T) {
			code, stdout, stderr := runCLIEnv(t, environ, tt.args...)
			if tt.name != "help" && code == exitOK {
				t.Errorf("exit code = 0, want a failure")
			}
			assertNoSecrets(t, "output", stdout+stderr, secretMarkers...)
		})
		t.Run(tt.name+" with secrets in flags", func(t *testing.T) {
			_, stdout, stderr := runCLIEnv(t, nil, append(append([]string{}, tt.args...), flags...)...)
			assertNoSecrets(t, "output", stdout+stderr, secretMarkers...)
		})
	}
}

func TestShortSourceTokenIsRejectedWithoutEchoingIt(t *testing.T) {
	code, _, stderr := runCLIEnv(t, map[string]string{"BEACON_SOURCE_TOKEN": "hunter2"}, "serve", "--database-url", "postgres://localhost/beacon")
	if code != exitError || !strings.Contains(stderr, `source "default": token: must be at least 16 characters`) {
		t.Errorf("exit %d, stderr = %q", code, stderr)
	}
	assertNoSecrets(t, "stderr", stderr, "hunter2")
}
