package cli

import (
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
	"github.com/thanoslabs/beacon/internal/slack/slacktest"
)

// TestTestAlert runs beacon test-alert against a running server: the alert
// goes in like any other, is paged through the policy that was named, and
// is ended by the same command.
func TestTestAlert(t *testing.T) {
	const (
		sourceToken = "source-token-marker-7c2e41d0"
		botToken    = "xoxb-bot-token-marker-55d1"
	)
	fake := slacktest.New(t, botToken)
	srv := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t), "BEACON_CONFIG": writeConfig(t, openAPIConfig), "BEACON_SOURCE_TOKEN": sourceToken,
		"BEACON_SLACK_BOT_TOKEN": botToken, "BEACON_SLACK_SIGNING_SECRET": "signing-secret-marker-e07b", "BEACON_SLACK_API_URL": fake.URL,
		"BEACON_HOOK_SIGNING_SECRET": "hook-signing-secret-marker-4d1a", "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": "$2a$10$2b2cU8CPhOTaGrs1HRQuAueS7JTT5ZHsHSzYiFPm1leZck7Mc8T4W",
	})
	environ := map[string]string{"BEACON_SOURCE_TOKEN": sourceToken, "BEACON_URL": srv.baseURL}
	run := func(args ...string) (int, string, string) {
		t.Helper()
		code, stdout, stderr := runCLIEnv(t, environ, append([]string{"test-alert"}, args...)...)
		assertNoSecrets(t, "the output of test-alert", stdout+stderr, sourceToken)
		return code, stdout, stderr
	}

	code, stdout, stderr := run("--policy", "default")
	if code != exitOK || !strings.HasPrefix(stdout, "The test alert is firing: "+srv.baseURL+"/alerts/") ||
		!strings.Contains(stdout, `paged through the escalation policy "default"`) || !strings.Contains(stdout, "beacon test-alert --resolve --policy default") {
		t.Fatalf("test-alert = %d\n%s%s", code, stdout, stderr)
	}
	// It is a real alert: the policy pages its channel.
	waitFor(t, "the test alert in Slack", func() bool {
		for _, m := range fake.Messages() {
			if strings.Contains(m.JSON(), "BeaconTestAlert") && strings.Contains(m.JSON(), "Test alert from beacon test-alert") {
				return true
			}
		}
		return false
	})
	// Sent again while it is open, it says why nobody is paged twice.
	if code, stdout, _ := run("--policy", "default"); code != exitOK || !strings.Contains(stdout, "was already firing") {
		t.Errorf("a second test-alert = %d %s", code, stdout)
	}
	if code, stdout, _ := run("--policy", "default", "--resolve"); code != exitOK || !strings.HasPrefix(stdout, "The test alert is resolved: ") {
		t.Errorf("--resolve = %d %s", code, stdout)
	}
	if code, stdout, _ := run("--policy", "default", "--resolve"); code != exitOK || !strings.Contains(stdout, "no open test alert") {
		t.Errorf("--resolve again = %d %s", code, stdout)
	}
	// Without a policy, routing decides, as for any alert.
	if code, stdout, _ := run(); code != exitOK || !strings.Contains(stdout, "the escalation policy that routing gives it") {
		t.Errorf("test-alert without a policy = %d %s", code, stdout)
	}

	// What a newcomer gets wrong first, and what they are told.
	for _, tt := range []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"a policy that does not exist", []string{"--policy", "nope"}, environ, `the configuration has no escalation policy named "nope"`},
		{"a source that does not exist", []string{"--source", "nope"}, environ, `it has no source named "nope", or the token is not that source's`},
		{"a wrong token", nil, map[string]string{"BEACON_SOURCE_TOKEN": "wrong-token-marker-1a2b3c", "BEACON_URL": srv.baseURL}, "set --token or BEACON_SOURCE_TOKEN"},
		{"no token", nil, map[string]string{"BEACON_URL": srv.baseURL}, "the token of the source is required"},
		{"nothing listening", nil, map[string]string{"BEACON_SOURCE_TOKEN": sourceToken, "BEACON_URL": "http://127.0.0.1:1"}, "could not reach Beacon at http://127.0.0.1:1"},
		{"an address that is none", []string{"--url", "localhost"}, environ, "--url must be an http or https address"},
	} {
		code, stdout, stderr := runCLIEnv(t, tt.env, append([]string{"test-alert"}, tt.args...)...)
		if code != exitError || !strings.Contains(stderr, tt.want) {
			t.Errorf("%s: exit %d\n%s%s\nwant an error with %q", tt.name, code, stdout, stderr, tt.want)
		}
		assertNoSecrets(t, tt.name, stdout+stderr, sourceToken, "wrong-token-marker-1a2b3c")
	}
}
