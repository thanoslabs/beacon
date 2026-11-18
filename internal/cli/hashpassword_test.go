package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// runWithInput runs the command line with text on standard input.
func runWithInput(t *testing.T, input string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, func(string) string { return "" }, strings.NewReader(input), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestHashPassword(t *testing.T) {
	const password = "correct horse battery staple"
	code, stdout, stderr := runWithInput(t, password+"\n", "hash-password")
	hash := strings.TrimSpace(stdout)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		t.Errorf("the hash does not match the password: %v", err)
	}
	if cost, _ := bcrypt.Cost([]byte(hash)); cost != bcryptCost {
		t.Errorf("cost = %d, want %d", cost, bcryptCost)
	}
	// The password is never repeated.
	if strings.Contains(stdout+stderr, password) {
		t.Errorf("the output contains the password: %s%s", stdout, stderr)
	}

	// The hash is what the configuration takes.
	path := writeConfig(t, "auth:\n  public_url: https://beacon.example.com\n  bootstrap_admin:\n    password_hash: {env: BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH}\n")
	environ := map[string]string{"BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": hash}
	if code, _, stderr := runCLIEnv(t, environ, "validate", "--config", path, "--check-secrets"); code != exitOK {
		t.Errorf("validate with the hash: exit %d, %s", code, stderr)
	}
	// The password in place of its hash is refused, and not repeated.
	environ["BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH"] = password
	code, _, stderr = runCLIEnv(t, environ, "validate", "--config", path, "--check-secrets")
	if code != exitError || !strings.Contains(stderr, "auth: bootstrap_admin.password_hash: must be a bcrypt hash") || strings.Contains(stderr, password) {
		t.Errorf("validate with the password itself: exit %d, %s", code, stderr)
	}

	for input, want := range map[string]string{
		"short\n":                      "the password must be at least 12 characters",
		"":                             "the password must be at least 12 characters",
		strings.Repeat("x", 73) + "\n": "the password must be at most 72 bytes",
	} {
		code, stdout, stderr := runWithInput(t, input, "hash-password")
		if code != exitError || stdout != "" || !strings.Contains(stderr, want) {
			t.Errorf("hash-password with %d bytes: exit %d, stdout %q, stderr %q", len(input), code, stdout, stderr)
		}
		if input != "" && strings.Contains(stderr, strings.TrimSpace(input)) {
			t.Errorf("the error repeats the password: %s", stderr)
		}
	}
}
