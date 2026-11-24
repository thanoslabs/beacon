package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// TestQuickstartOverlays keeps the two files that turn the quickstart into
// one with pages in chat valid: each is renamed into a copy of the
// directory, as the guide says to, and the result is a configuration.
func TestQuickstartOverlays(t *testing.T) {
	quickstart := filepath.Join("..", "..", "examples", "quickstart")
	base, err := os.ReadFile(filepath.Join(quickstart, "beacon.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlays, err := filepath.Glob(filepath.Join(quickstart, "*.yaml.example"))
	if err != nil || len(overlays) != 2 {
		t.Fatalf("overlays = %v, %v; want one for Slack and one for Discord", overlays, err)
	}
	for _, overlay := range overlays {
		dir := t.TempDir()
		raw, err := os.ReadFile(overlay)
		if err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(overlay), ".example")
		for file, content := range map[string][]byte{"beacon.yaml": base, name: raw} {
			if err := os.WriteFile(filepath.Join(dir, file), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		code, stdout, stderr := runCLI(t, "validate", "--config", dir)
		want := "is valid: 1 source, 1 integration, 0 teams, 0 schedules, 1 escalation policy, 0 routing rules"
		if code != exitOK || !strings.Contains(stdout, want) {
			t.Errorf("the quickstart with %s: exit %d\n%s%s\nwant %q", name, code, stdout, stderr, want)
		}
	}
}

// TestLocalCredentialsAreWhatComposeGives holds the three places that know
// the credentials of the quickstart together: compose.yaml, the warning of
// beacon serve, and the guide.
func TestLocalCredentialsAreWhatComposeGives(t *testing.T) {
	if err := bcrypt.CompareHashAndPassword([]byte(localAdminHash), []byte(localAdminPassword)); err != nil {
		t.Errorf("localAdminHash is not the hash of localAdminPassword: %v", err)
	}
	root := filepath.Join("..", "..")
	compose, err := os.ReadFile(filepath.Join(root, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Compose reads a $ in a value unless it is doubled.
	for _, want := range []string{
		"BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH: ${BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH:-" + strings.ReplaceAll(localAdminHash, "$", "$$") + "}",
		"BEACON_SOURCE_TOKEN: ${BEACON_SOURCE_TOKEN:-" + localSourceToken + "}",
	} {
		if !strings.Contains(string(compose), want) {
			t.Errorf("compose.yaml does not have %s", want)
		}
	}
	guide, err := os.ReadFile(filepath.Join(root, "docs", "quickstart.md"))
	if err != nil {
		t.Fatal(err)
	}
	// On a line of its own, in the block a reader copies it from.
	if !strings.Contains(string(guide), "\n"+localAdminPassword+"\n") {
		t.Errorf("docs/quickstart.md does not give the password %s", localAdminPassword)
	}
}

// TestServeWarnsAboutTheCredentialsOfTheQuickstart: an install that runs on
// the published credentials says so in its log, and one with its own does
// not.
func TestServeWarnsAboutTheCredentialsOfTheQuickstart(t *testing.T) {
	quickstart := filepath.Join("..", "..", "examples", "quickstart")
	local := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t), "BEACON_CONFIG": quickstart,
		"BEACON_SOURCE_TOKEN": localSourceToken, "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": localAdminHash,
	})
	for _, want := range []string{`"msg":"the source has the token of the quickstart`, `"msg":"the bootstrap admin has the password of the quickstart`} {
		if !strings.Contains(local.logs.String(), want) {
			t.Errorf("the log of an install on the quickstart's credentials has no %s", want)
		}
	}
	own := startServe(t, map[string]string{
		"BEACON_DATABASE_URL": postgrestest.NewURL(t), "BEACON_CONFIG": quickstart,
		"BEACON_SOURCE_TOKEN": secretSettings["BEACON_SOURCE_TOKEN"], "BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH": secretSettings["BEACON_BOOTSTRAP_ADMIN_PASSWORD_HASH"],
	})
	if strings.Contains(own.logs.String(), "of the quickstart") {
		t.Errorf("an install with credentials of its own is warned:\n%s", own.logs.String())
	}
}
