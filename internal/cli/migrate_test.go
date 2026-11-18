package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// runCLIEnv is like runCLI with environment variables.
func runCLIEnv(t *testing.T, environ map[string]string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, func(k string) string { return environ[k] }, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestMigrateSettingsErrors(t *testing.T) {
	tests := []struct {
		name       string
		environ    map[string]string
		args       []string
		wantStderr string
	}{
		{
			name:       "missing database URL",
			args:       []string{"migrate"},
			wantStderr: "the database URL is required",
		},
		{
			name:       "environment value that is not a number",
			environ:    map[string]string{"BEACON_DATABASE_URL": "postgres://localhost/beacon", "BEACON_DATABASE_MAX_CONNS": "many"},
			args:       []string{"migrate"},
			wantStderr: `BEACON_DATABASE_MAX_CONNS: "many" is not a whole number`,
		},
		{
			name:       "negative pool size",
			args:       []string{"migrate", "--database-url", "postgres://localhost/beacon", "--database-max-conns", "-1"},
			wantStderr: "--database-max-conns must be between 0 and 10000",
		},
		{
			name:       "malformed URL does not leak the password",
			args:       []string{"migrate", "--database-url", "postgres://beacon:hunter2@localhost:notaport/beacon"},
			wantStderr: "parse database URL",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLIEnv(t, tt.environ, tt.args...)
			if code != exitError {
				t.Errorf("exit code = %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			if strings.Contains(stderr, "hunter2") {
				t.Errorf("stderr leaks the database password: %q", stderr)
			}
		})
	}
}

func TestHelpDoesNotShowDatabaseURLFromEnvironment(t *testing.T) {
	environ := map[string]string{"BEACON_DATABASE_URL": "postgres://beacon:hunter2@localhost/beacon"}
	code, stdout, stderr := runCLIEnv(t, environ, "migrate", "-h")
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stderr, "-database-url") {
		t.Errorf("help = %q, want it to list the database URL flag", stderr)
	}
	if strings.Contains(stdout+stderr, "hunter2") {
		t.Errorf("help shows the database password from the environment: %q", stdout+stderr)
	}
}

func TestMigrateCommand(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	environ := map[string]string{"BEACON_DATABASE_URL": dbURL}

	code, stdout, stderr := runCLIEnv(t, environ, "migrate", "--status")
	if code != exitOK {
		t.Fatalf("migrate --status: exit code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "00001_create_users.sql") || !strings.Contains(stdout, "pending") {
		t.Errorf("status before migrating = %q, want the first migration listed as pending", stdout)
	}

	code, stdout, stderr = runCLIEnv(t, environ, "migrate")
	if code != exitOK {
		t.Fatalf("migrate: exit code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "applied 00001_create_users.sql") {
		t.Errorf("migrate output = %q, want it to report the applied migration", stdout)
	}

	// The flag takes precedence over the environment, and a second run has
	// nothing to do.
	code, stdout, stderr = runCLIEnv(t, map[string]string{"BEACON_DATABASE_URL": "postgres://invalid.invalid/none"}, "migrate", "--database-url", dbURL)
	if code != exitOK {
		t.Fatalf("second migrate: exit code %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "database schema is up to date") {
		t.Errorf("second migrate output = %q, want it to report nothing to do", stdout)
	}

	code, stdout, _ = runCLIEnv(t, environ, "migrate", "--status")
	if code != exitOK || !strings.Contains(stdout, "applied 20") {
		t.Errorf("status after migrating = %q (exit code %d), want an applied timestamp", stdout, code)
	}
}
