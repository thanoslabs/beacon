package metrics_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/thanoslabs/beacon/internal/metrics"
)

// usesGlobalWithoutMax reports the global metric that expr reads without
// taking one replica's value. Every replica reports a global metric alike,
// so a sum over it is multiplied by the number of replicas.
func usesGlobalWithoutMax(expr string) string {
	for _, d := range metrics.Catalog() {
		if d.Scope != metrics.Global {
			continue
		}
		// The whole name: beacon_alerts_open is not beacon_alerts_opened_total.
		at := regexp.MustCompile(`\b` + d.Name + `\b`).FindStringIndex(expr)
		if at != nil && !strings.Contains(expr[:at[0]], "max") {
			return d.Name
		}
	}
	return ""
}

// TestDashboard holds the Grafana dashboard that ships with Beacon to what
// a person who imports it needs: it parses, it works with any Prometheus
// data source, every panel says what it shows, and no panel adds a global
// metric up over the replicas. That the queries are valid PromQL is checked
// against a real Prometheus in CI (.github/scripts/check-monitoring.sh).
func TestDashboard(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dev", "grafana", "dashboards", "beacon.json"))
	if err != nil {
		t.Fatal(err)
	}
	type datasource struct{ Type, UID string }
	var dashboard struct {
		UID, Title string
		Templating struct {
			List []struct{ Name, Type, Query string }
		}
		Panels []struct {
			ID          int
			Type, Title string
			Description string
			Datasource  *datasource
			Targets     []struct {
				Expr       string
				Datasource *datasource
			}
		}
	}
	if err := json.Unmarshal(raw, &dashboard); err != nil {
		t.Fatalf("the dashboard is not JSON: %v", err)
	}
	if dashboard.UID != "beacon" || dashboard.Title == "" {
		t.Errorf("uid %q, title %q", dashboard.UID, dashboard.Title)
	}
	if l := dashboard.Templating.List; len(l) != 1 || l[0].Name != "datasource" || l[0].Type != "datasource" || l[0].Query != "prometheus" {
		t.Errorf("the dashboard does not let its reader choose the Prometheus data source: %+v", l)
	}
	portable := func(d *datasource) bool { return d != nil && d.Type == "prometheus" && d.UID == "${datasource}" }
	ids := map[int]bool{}
	queries, shown := 0, map[string]bool{}
	for _, p := range dashboard.Panels {
		if ids[p.ID] || p.ID == 0 {
			t.Errorf("panel %q has the identifier %d, which is missing or used twice", p.Title, p.ID)
		}
		ids[p.ID] = true
		if p.Type == "row" {
			continue
		}
		if len(p.Description) < 20 {
			t.Errorf("panel %q does not say what it shows", p.Title)
		}
		if !portable(p.Datasource) || len(p.Targets) == 0 {
			t.Errorf("panel %q has no query, or a data source of one install", p.Title)
		}
		for _, target := range p.Targets {
			queries++
			if !portable(target.Datasource) || target.Expr == "" {
				t.Errorf("a query of panel %q is empty or has a data source of one install", p.Title)
			}
			if name := usesGlobalWithoutMax(target.Expr); name != "" {
				t.Errorf("panel %q reads the global metric %s without max: with several replicas it would show a multiple", p.Title, name)
			}
			for _, d := range metrics.Catalog() {
				if strings.Contains(target.Expr, d.Name) {
					shown[d.Group] = true
				}
			}
		}
	}
	if queries < 20 {
		t.Errorf("the dashboard has %d queries", queries)
	}
	for _, group := range metrics.Groups {
		if !shown[group] {
			t.Errorf("the dashboard shows no metric of the group %q", group)
		}
	}
}

// TestAlertingRules holds the example rules to a form an operator can use:
// each has a severity, says what is wrong in words, and reads global
// metrics with max. promtool checks and tests the expressions in CI.
func TestAlertingRules(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "dev", "prometheus", "beacon-alerts.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Name  string
			Rules []struct {
				Alert, Expr, For string
				Labels           map[string]string
				Annotations      map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	about := ""
	for _, g := range file.Groups {
		for _, r := range g.Rules {
			if !strings.HasPrefix(r.Alert, "Beacon") || seen[r.Alert] {
				t.Errorf("rule %q: a name starts with Beacon and is used once", r.Alert)
			}
			seen[r.Alert] = true
			about += r.Expr
			if s := r.Labels["severity"]; s != "critical" && s != "warning" {
				t.Errorf("rule %s has the severity %q", r.Alert, s)
			}
			if len(r.Annotations["summary"]) < 15 || len(r.Annotations["description"]) < 60 {
				t.Errorf("rule %s does not say what is wrong and what it means", r.Alert)
			}
			if name := usesGlobalWithoutMax(r.Expr); name != "" {
				t.Errorf("rule %s reads the global metric %s without max: with several replicas it would see a multiple", r.Alert, name)
			}
		}
	}
	// The three things an operator must hear about: notifications that
	// fail, a queue that grows, and a source that has gone quiet.
	for _, needs := range []string{metrics.NotificationsFailed, metrics.NotificationsOldest, metrics.NotificationsPending, metrics.IngestRequests, metrics.EscalationsExhausted, metrics.StateUp} {
		if !strings.Contains(about, needs) {
			t.Errorf("no rule watches %s", needs)
		}
	}
	if len(seen) < 10 {
		t.Errorf("%d rules were read", len(seen))
	}
}
