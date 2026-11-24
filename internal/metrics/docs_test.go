package metrics_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thanoslabs/beacon/internal/metrics"
)

var update = flag.Bool("update", false, "rewrite the reference in docs/metrics.md")

// referenceHeading starts the part of docs/metrics.md that is written from
// the catalogue. Everything from it to the end of the page is generated.
const referenceHeading = "## Reference\n"

// reference writes the catalogue as the reference of the page.
func reference() string {
	var b strings.Builder
	b.WriteString(referenceHeading + `
Every metric Beacon exposes under the prefix ` + "`beacon_`" + `. This part of the page is written from the catalogue in ` + "`internal/metrics`" + `, and a test fails when the two differ: do not edit it, run ` + "`go test ./internal/metrics -run TestReferenceIsTheCatalogue -update`" + `.

"Replica" is what one process did or holds; "global" is a fact about the whole install, read from Postgres, which every replica reports alike ([Several replicas](#several-replicas)).
`)
	for _, group := range metrics.Groups {
		fmt.Fprintf(&b, "\n### %s\n\n| Metric | Type | Labels | Scope | Meaning |\n|---|---|---|---|---|\n", group)
		for _, d := range metrics.Catalog() {
			if d.Group != group {
				continue
			}
			labels := make([]string, 0, len(d.Labels)+len(d.ConstLabels))
			for _, l := range slices.Concat(d.Labels, d.ConstLabels) {
				labels = append(labels, "`"+l+"`")
			}
			if len(labels) == 0 {
				labels = []string{"none"}
			}
			help := d.Help
			if d.Type == metrics.Histogram {
				help += fmt.Sprintf(" Buckets from %s to %s.", seconds(d.Buckets[0]), seconds(d.Buckets[len(d.Buckets)-1]))
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", d.Name, d.Type, strings.Join(labels, ", "), d.Scope, help)
		}
	}
	b.WriteString("\n### Labels\n\nThe only labels a Beacon metric can have. A test fails when a metric gains one that is not in this list.\n\n| Label | Where its values come from |\n|---|---|\n")
	names := make([]string, 0, len(metrics.Labels))
	for name := range metrics.Labels {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fmt.Fprintf(&b, "| `%s` | %s |\n", name, metrics.Labels[name])
	}
	return b.String()
}

// seconds writes a bucket bound the way a person says it.
func seconds(s float64) string {
	switch {
	case s < 1:
		return fmt.Sprintf("%g ms", s*1000)
	case s < 120:
		return fmt.Sprintf("%g s", s)
	case s < 7200:
		return fmt.Sprintf("%g min", s/60)
	default:
		return fmt.Sprintf("%g h", s/3600)
	}
}

// TestReferenceIsTheCatalogue keeps the reference of docs/metrics.md equal
// to the catalogue, so that the page cannot describe a metric that does not
// exist or miss one that does.
func TestReferenceIsTheCatalogue(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "metrics.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	guide, _, found := strings.Cut(page, referenceHeading)
	if !found {
		t.Fatalf("%s has no %q", path, strings.TrimSpace(referenceHeading))
	}
	want := guide + reference()
	if *update {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if page != want {
		t.Errorf("the reference in %s is not what the catalogue makes it; run go test ./internal/metrics -run TestReferenceIsTheCatalogue -update", path)
	}
}

// TestDocsNameOnlyMetricsThatExist reads everything a person might copy a
// metric name from (the pages and the examples) and fails on a name that
// the catalogue does not have: a renamed metric must not live on in a
// query somebody pastes into an alert rule.
func TestDocsNameOnlyMetricsThatExist(t *testing.T) {
	name := regexp.MustCompile(`\bbeacon_[a-z0-9_]+`)
	known := map[string]bool{}
	for _, d := range metrics.Catalog() {
		known[d.Name] = true
		if d.Type == metrics.Histogram {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				known[d.Name+suffix] = true
			}
		}
	}
	// Words that begin like a metric and are something else.
	other := regexp.MustCompile(`^beacon_(acknowledge|resolve|snooze|link|test|demo|source_token|[a-z_]*(token|secret|password|url|dir|port)[a-z_]*)$`)
	root := filepath.Join("..", "..")
	read := 0
	for _, pattern := range []string{"README.md", "docs/*.md", "examples/*/*", "dev/*/*", "dev/*/*/*", "dev/*/*/*/*"} {
		files, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if info, err := os.Stat(file); err != nil || info.IsDir() {
				continue
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			read++
			for _, found := range name.FindAllString(string(raw), -1) {
				if !known[found] && !other.MatchString(found) {
					rel, _ := filepath.Rel(root, file)
					t.Errorf("%s names %s, which is not a metric of Beacon", rel, found)
				}
			}
		}
	}
	if read < 20 {
		t.Fatalf("only %d files were read; the test is looking in the wrong place", read)
	}
}
