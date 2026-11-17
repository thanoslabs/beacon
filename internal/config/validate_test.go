package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func TestReadValidExample(t *testing.T) {
	if _, err := Read("testdata/example", LoadOptions{Getenv: getenv(exampleEnv)}); err != nil {
		t.Errorf("with secrets: %v", err)
	}
	// Without the secrets, as in CI.
	if _, err := Read("testdata/example", LoadOptions{}); err != nil {
		t.Errorf("without secrets: %v", err)
	}
}

// TestReadReportsEveryProblem checks one configuration with a mistake of
// every kind, in testdata/invalid: all are reported in one run, each with its
// file, line, object and field.
func TestReadReportsEveryProblem(t *testing.T) {
	env := map[string]string{"SHORT_TOKEN": "hunter2", "GOOD_TOKEN": secretValue}
	_, err := Read("testdata/invalid", LoadOptions{Getenv: getenv(env)})
	if err == nil {
		t.Fatal("Read succeeded")
	}
	got := strings.Split(filepath.ToSlash(err.Error()), "\n")

	// The expected report is testdata/invalid.golden. After changing a
	// message, run the test with -update and read the difference.
	const golden = "testdata/invalid.golden"
	if *update {
		if err := os.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n")+"\n" != string(want) {
		t.Errorf("errors:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), want)
	}
	for _, e := range got {
		if strings.Contains(e, "hunter2") || strings.Contains(e, secretValue) {
			t.Errorf("an error contains a secret: %s", e)
		}
	}
}

// TestReadCombinesStructuralAndSemanticErrors checks that one run reports
// both kinds, without repeating a field that could not be decoded and
// without blaming references on a file that could not be parsed.
func TestReadCombinesStructuralAndSemanticErrors(t *testing.T) {
	dir := write(t, map[string]string{
		"a.yaml": `sources:
  - name: prod
    type: webhook
    token: written-inline
    colour: blue
escalation_policies:
  - name: p
    steps:
      - notify: [{team: platform}]
        timeout: soon
`,
	})
	got := readErrors(t, dir, LoadOptions{})
	want := []string{
		`a.yaml:4: source "prod": token: secrets are never written inline; use {env: NAME} or {file: /path}`,
		`a.yaml:5: source "prod": colour: unknown field`,
		`a.yaml:9: escalation policy "p": steps[0].notify[0].team: team "platform" does not exist`,
		`a.yaml:10: escalation policy "p": steps[0].timeout: "soon" is not a duration such as 30s, 5m, 12h, 1d or 1w`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("errors:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// teams.yaml cannot be parsed, so the team may well exist: only the
	// syntax error is reported.
	dir = write(t, map[string]string{
		"policies.yaml": "escalation_policies:\n  - name: p\n    steps:\n      - notify: [{team: platform}]\n        timeout: 5m\n",
		"teams.yaml":    "teams: [\n",
	})
	if got := readErrors(t, dir, LoadOptions{}); len(got) != 1 || !strings.Contains(got[0], "teams.yaml:1: invalid YAML") {
		t.Errorf("errors: %v", got)
	}
}

// readErrors is loadErrors for Read.
func readErrors(t *testing.T, dir string, opts LoadOptions) []string {
	t.Helper()
	_, err := Read(dir, opts)
	errs, _ := err.(Errors) //nolint:errorlint // Read returns Errors itself
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = strings.TrimPrefix(e.Error(), dir+string(filepath.Separator))
	}
	return out
}
