package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "beacon.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validConfig = `sources:
  - name: prod
    type: webhook
    token: {env: BEACON_SOURCE_TOKEN}
`

// TestValidateExamples keeps the shipped examples valid. No database and no
// secrets are involved.
func TestValidateExamples(t *testing.T) {
	examples, err := filepath.Glob("../../examples/*")
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, dir := range examples {
		code, stdout, stderr := runCLI(t, "validate", "--config", dir)
		if code != exitOK || !strings.Contains(stdout, "is valid") {
			t.Errorf("validate %s: exit %d\n%s%s", dir, code, stdout, stderr)
		}
	}
}

func TestValidate(t *testing.T) {
	path := writeConfig(t, validConfig)
	code, stdout, stderr := runCLI(t, "validate", "--config", path)
	want := "the configuration at " + path + " is valid: 1 source, 0 integrations, 0 teams, 0 schedules, 0 escalation policies, 0 routing rules\n"
	if code != exitOK || stdout != want || stderr != "" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	// The path can come from the environment.
	if code, _, _ := runCLIEnv(t, map[string]string{"BEACON_CONFIG": path}, "validate"); code != exitOK {
		t.Errorf("BEACON_CONFIG: exit %d", code)
	}
	if code, _, stderr := runCLI(t, "validate"); code != exitError || !strings.Contains(stderr, "set --config or BEACON_CONFIG") {
		t.Errorf("no path: exit %d, stderr %q", code, stderr)
	}
}

func TestValidateReportsEveryProblemAndFails(t *testing.T) {
	path := writeConfig(t, `sources:
  - name: prod
    type: pager
    token: inline-token-marker-91c4
teams:
  - name: Platform
routing:
  default: {escalation_policy: missing}
`)
	code, stdout, stderr := runCLI(t, "validate", "--config", path)
	if code != exitError || stdout != "" {
		t.Errorf("exit %d, stdout %q", code, stdout)
	}
	for _, want := range []string{
		path + `:3: source "prod": type: "pager" is not alertmanager, grafana or webhook`,
		path + `:4: source "prod": token: secrets are never written inline`,
		path + `:6: team "Platform": name: must be lowercase`,
		path + `:8: routing: default.escalation_policy: escalation policy "missing" does not exist`,
		"is not valid: 4 problems",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	assertNoSecrets(t, "stderr", stderr, "inline-token-marker-91c4")
}

func TestValidateCheckSecrets(t *testing.T) {
	path := writeConfig(t, validConfig)
	code, _, stderr := runCLI(t, "validate", "--config", path, "--check-secrets")
	if code != exitError || !strings.Contains(stderr, `source "prod": token: the environment variable BEACON_SOURCE_TOKEN is not set`) {
		t.Errorf("missing secret: exit %d, stderr %q", code, stderr)
	}
	environ := map[string]string{"BEACON_SOURCE_TOKEN": "hunter2"}
	code, _, stderr = runCLIEnv(t, environ, "validate", "--config", path, "--check-secrets")
	if code != exitError || !strings.Contains(stderr, "token: must be at least 16 characters") {
		t.Errorf("short secret: exit %d, stderr %q", code, stderr)
	}
	assertNoSecrets(t, "stderr", stderr, "hunter2")
	environ["BEACON_SOURCE_TOKEN"] = "source-token-marker-7c2e41d0"
	code, stdout, stderr := runCLIEnv(t, environ, "validate", "--config", path, "--check-secrets")
	if code != exitOK {
		t.Errorf("good secret: exit %d, stderr %q", code, stderr)
	}
	assertNoSecrets(t, "output", stdout+stderr, "source-token-marker-7c2e41d0")
}
