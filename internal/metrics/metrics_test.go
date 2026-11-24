package metrics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil/promlint"

	"github.com/thanoslabs/beacon/internal/metrics"
)

// TestLabelsAreOnTheAllowList is the guard against cardinality that creeps:
// a metric may only have labels that the allow-list names, and the
// allow-list is short enough to read. To add a label, add it to
// metrics.Labels with where its values come from, and be able to say why
// they are a bounded set.
func TestLabelsAreOnTheAllowList(t *testing.T) {
	for _, d := range metrics.Catalog() {
		for _, label := range slices.Concat(d.Labels, d.ConstLabels) {
			if _, ok := metrics.Labels[label]; !ok {
				t.Errorf("%s has the label %q, which is not on the allow-list metrics.Labels", d.Name, label)
			}
		}
	}
	// What must never be a label, under the names it would likely get.
	for _, never := range []string{"alertname", "alert", "alert_id", "fingerprint", "group", "group_id", "id", "rule", "user", "user_id",
		"email", "name", "destination", "channel", "path", "url", "instance_id", "token", "error", "le"} {
		if _, ok := metrics.Labels[never]; ok {
			t.Errorf("the allow-list has %q, whose values are not a bounded set", never)
		}
	}
	for label, doc := range metrics.Labels {
		if doc == "" {
			t.Errorf("the label %q does not say where its values come from", label)
		}
		used := false
		for _, d := range metrics.Catalog() {
			used = used || slices.Contains(d.Labels, label) || slices.Contains(d.ConstLabels, label)
		}
		if !used {
			t.Errorf("the allow-list has %q, which no metric uses", label)
		}
	}
}

// TestCatalogFollowsTheConventions holds every metric to the naming
// conventions of Prometheus and to the form the reference needs.
func TestCatalogFollowsTheConventions(t *testing.T) {
	name := regexp.MustCompile(`^beacon_[a-z][a-z0-9_]*$`)
	seen := map[string]bool{}
	for _, d := range metrics.Catalog() {
		if !name.MatchString(d.Name) {
			t.Errorf("%s: a name starts with beacon_ and is lower case with underscores", d.Name)
		}
		if seen[d.Name] {
			t.Errorf("%s is in the catalogue twice", d.Name)
		}
		seen[d.Name] = true
		if !strings.HasSuffix(d.Help, ".") || len(d.Help) < 20 {
			t.Errorf("%s: the help is a sentence", d.Name)
		}
		if !slices.Contains(metrics.Groups, d.Group) {
			t.Errorf("%s: the group %q is not one of metrics.Groups", d.Name, d.Group)
		}
		if d.Scope != metrics.Replica && d.Scope != metrics.Global {
			t.Errorf("%s: the scope is %q", d.Name, d.Scope)
		}
		switch d.Type {
		case metrics.Counter:
			if !strings.HasSuffix(d.Name, "_total") {
				t.Errorf("%s: a counter ends in _total", d.Name)
			}
			if d.Scope != metrics.Replica {
				t.Errorf("%s: a counter is what one process counted", d.Name)
			}
		case metrics.Gauge:
			if strings.HasSuffix(d.Name, "_total") {
				t.Errorf("%s: a gauge does not end in _total", d.Name)
			}
		case metrics.Histogram:
			if !strings.HasSuffix(d.Name, "_seconds") {
				t.Errorf("%s: a histogram of time ends in _seconds", d.Name)
			}
			if len(d.Buckets) < 5 || !slices.IsSorted(d.Buckets) {
				t.Errorf("%s: buckets = %v", d.Name, d.Buckets)
			}
		default:
			t.Errorf("%s: the type is %q", d.Name, d.Type)
		}
		for _, unit := range []string{"_milliseconds", "_ms", "_minutes", "_hours", "_duration"} {
			if strings.HasSuffix(d.Name, unit) {
				t.Errorf("%s: times are in seconds and the name ends in _seconds", d.Name)
			}
		}
	}
}

// build creates every metric of the catalogue on a registry, with one
// series each, as the process would.
func build(t *testing.T) *prometheus.Registry {
	t.Helper()
	reg := prometheus.NewRegistry()
	for _, d := range metrics.Catalog() {
		values := make([]string, len(d.Labels))
		for i := range values {
			values[i] = "x"
		}
		constant := make([]string, len(d.ConstLabels))
		switch {
		case d.Scope == metrics.Global || d.Name == metrics.StateUp || d.Name == metrics.StateReadDuration:
			// Made by the state collector, which TestStateCollector covers.
			continue
		case d.Type == metrics.Counter && len(d.Labels) == 0:
			reg.MustRegister(metrics.NewCounter(d.Name))
		case d.Type == metrics.Counter:
			c := metrics.NewCounterVec(d.Name)
			c.WithLabelValues(values...).Inc()
			reg.MustRegister(c)
		case d.Type == metrics.Gauge && len(d.Labels) == 0:
			reg.MustRegister(metrics.NewGauge(d.Name, constant...))
		case d.Type == metrics.Gauge:
			g := metrics.NewGaugeVec(d.Name)
			g.WithLabelValues(values...).Set(1)
			reg.MustRegister(g)
		case d.Type == metrics.Histogram:
			h := metrics.NewHistogramVec(d.Name)
			h.WithLabelValues(values...).Observe(1)
			reg.MustRegister(h)
		}
	}
	return reg
}

// TestCatalogPassesTheLinter runs the linter of the Prometheus client
// library, the one behind `promtool check metrics`, over every metric.
func TestCatalogPassesTheLinter(t *testing.T) {
	families, err := build(t).Gather()
	if err != nil {
		t.Fatal(err)
	}
	problems, err := promlint.NewWithMetricFamilies(families).Lint()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
}

func TestConstructorsRefuseWhatIsNotInTheCatalogue(t *testing.T) {
	for name, create := range map[string]func(){
		"a metric that is not described": func() { metrics.NewCounterVec("beacon_surprises_total") },
		"a counter made as a gauge":      func() { metrics.NewGaugeVec(metrics.ConfigReloads) },
		"a vector made without labels":   func() { metrics.NewCounter(metrics.ConfigReloads) },
		"a constant label left out":      func() { metrics.NewGauge(metrics.BuildInfo) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was created", name)
				}
			}()
			create()
		}()
	}
}

// TestMetricsAreCreatedFromTheCatalogue reads the source tree: outside this
// package, no code may create a metric with the client library directly,
// because such a metric would have no entry in the catalogue, no line in
// the reference and no check of its labels.
func TestMetricsAreCreatedFromTheCatalogue(t *testing.T) {
	forbidden := regexp.MustCompile(`^New(Counter|Gauge|Histogram|Summary|Untyped)(Vec|Func)?$|^(MustNew|New)Const(Metric|Histogram|Summary|NativeHistogram)(WithCreatedTimestamp)?$|^NewDesc$`)
	root := filepath.Join("..", "..")
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if rel == "web" || rel == "bin" || strings.HasPrefix(d.Name(), ".") && rel != "." || rel == filepath.Join("internal", "metrics") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		checked++
		// The names the file gives the client library's packages.
		client := map[string]bool{}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, "github.com/prometheus/client_golang/prometheus") {
				continue
			}
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			client[name] = true
			if strings.HasSuffix(p, "/promauto") {
				t.Errorf("%s imports promauto, which registers on the global registry", rel)
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && client[pkg.Name] && forbidden.MatchString(sel.Sel.Name) {
				t.Errorf("%s creates a metric with %s.%s; describe it in internal/metrics and create it with the functions there", rel, pkg.Name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d source files were read; the test is looking in the wrong place", checked)
	}
}
