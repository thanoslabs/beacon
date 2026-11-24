package ingest_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/metrics/metricstest"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/routing"
)

// routed is a fixture whose handler routes by three rules and a default.
type routed struct {
	*fixture
	rows provision.Result
	reg  *prometheus.Registry
}

func seconds(n int) *config.Duration {
	d := config.Duration(time.Duration(n) * time.Second)
	return &d
}

func newRouted(t *testing.T) *routed {
	t.Helper()
	f := &routed{fixture: newFixture(t), reg: prometheus.NewRegistry()}
	policy := func(name string) config.EscalationPolicy {
		return config.EscalationPolicy{Name: name, Steps: []config.EscalationStep{{Notify: []config.Target{{User: "ada@example.com"}}, Timeout: config.Duration(time.Minute)}}}
	}
	cfg := config.Config{
		Sources: []config.Source{
			{Name: "prod", Type: config.SourceTypeWebhook}, {Name: "staging", Type: config.SourceTypeWebhook},
			{Name: "am", Type: config.SourceTypeAlertmanager}, {Name: "grafana", Type: config.SourceTypeGrafana},
		},
		Teams:              []config.Team{{Name: "dba"}, {Name: "platform"}},
		EscalationPolicies: []config.EscalationPolicy{policy("dba"), policy("urgent"), policy("default")},
		Routing: &config.Routing{
			GroupBy: []string{"alertname"}, GroupWait: seconds(30), GroupInterval: seconds(300),
			Default: config.RouteDefault{Team: "platform", EscalationPolicy: "default"},
			Rules: []config.RoutingRule{
				{Name: "database", Match: map[string]string{"team": "dba"}, Team: "dba", Service: "postgres", EscalationPolicy: "dba", GroupWait: seconds(0)},
				{Name: "pages", MatchRE: map[string]string{"severity": "critical|page"}, EscalationPolicy: "urgent", GroupBy: []string{"alertname", "instance"}},
			},
		},
	}
	var err error
	if f.rows, err = provision.Reconcile(context.Background(), f.pool, cfg); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	router, err := routing.New(cfg.Routing, routing.Refs{
		Rules: f.rows.IDs[provision.RoutingRules], Teams: f.rows.IDs[provision.Teams], Policies: f.rows.IDs[provision.EscalationPolicies],
	})
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}
	f.handler.SetRouter(router)
	f.handler.RegisterMetrics(f.reg)
	return f
}

// stored is what routing left on an alert and on its group, by name.
type stored struct {
	Rule, Team, Service          string
	GroupRule, GroupTeam, Policy string
	GroupLabels                  string
	Wait                         time.Duration
}

func (f *routed) stored(t *testing.T, alertID string) (s stored, groupID string) {
	t.Helper()
	var notifyAt, createdAt time.Time
	err := f.pool.QueryRow(context.Background(), `
		SELECT COALESCE(r.name, ''), COALESCE(t.name, ''), COALESCE(a.service, ''),
			COALESCE(gr.name, ''), COALESCE(gt.name, ''), COALESCE(p.name, ''), g.labels::text, g.notify_at, g.created_at, g.id::text
		FROM alerts a JOIN alert_groups g ON g.id = a.group_id
		LEFT JOIN routing_rules r ON r.id = a.routing_rule_id LEFT JOIN teams t ON t.id = a.team_id
		LEFT JOIN routing_rules gr ON gr.id = g.routing_rule_id LEFT JOIN teams gt ON gt.id = g.team_id
		LEFT JOIN escalation_policies p ON p.id = g.escalation_policy_id
		WHERE a.id = $1`, alertID).Scan(&s.Rule, &s.Team, &s.Service, &s.GroupRule, &s.GroupTeam, &s.Policy, &s.GroupLabels, &notifyAt, &createdAt, &groupID)
	if err != nil {
		t.Fatalf("read alert: %v", err)
	}
	s.Wait = notifyAt.Sub(createdAt)
	return s, groupID
}

// unmatched reads beacon_routing_unmatched_alerts_total for a source, as the
// metrics endpoint would show it.
func (f *routed) unmatched(source string) float64 {
	families, err := f.reg.Gather()
	if err != nil {
		panic(err)
	}
	for _, family := range families {
		for _, m := range family.GetMetric() {
			if family.GetName() == "beacon_routing_unmatched_alerts_total" && m.GetLabel()[0].GetValue() == source {
				return m.GetCounter().GetValue()
			}
		}
	}
	return -1
}

func TestRouting(t *testing.T) {
	f := newRouted(t)
	tests := []struct {
		name, labels string
		want         stored
	}{
		{
			name:   "the first rule: its team, service and policy, and its own wait",
			labels: `{"alertname": "ReplicationLag", "team": "dba", "severity": "critical", "service": "from-the-alert"}`,
			want: stored{Rule: "database", Team: "dba", Service: "postgres", GroupRule: "database", GroupTeam: "dba", Policy: "dba",
				GroupLabels: `{"alertname": "ReplicationLag"}`, Wait: 0},
		},
		{
			name:   "a later rule, by a regular expression: its policy and its own group_by; the service is the alert's label",
			labels: `{"alertname": "HighLatency", "severity": "page", "service": "checkout", "instance": "api1"}`,
			want: stored{Rule: "pages", Service: "checkout", GroupRule: "pages", Policy: "urgent",
				GroupLabels: `{"instance": "api1", "alertname": "HighLatency"}`, Wait: 30 * time.Second},
		},
		{
			name:   "no rule: the default team and policy",
			labels: `{"alertname": "DiskFilling", "severity": "warning", "service": "billing"}`,
			want:   stored{Team: "platform", Service: "billing", GroupTeam: "platform", Policy: "default", GroupLabels: `{"alertname": "DiskFilling"}`, Wait: 30 * time.Second},
		},
		{
			name:   "a pattern must match the whole value",
			labels: `{"alertname": "Almost", "severity": "non-critical"}`,
			want:   stored{Team: "platform", GroupTeam: "platform", Policy: "default", GroupLabels: `{"alertname": "Almost"}`, Wait: 30 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.post(t, "staging", stagingToken, `{"labels": `+tt.labels+`}`)
			if res.code != http.StatusCreated {
				t.Fatalf("fire = %d %v", res.code, res.body)
			}
			if got, _ := f.stored(t, res.body["alert_id"]); got != tt.want {
				t.Errorf("stored:\n%+v\nwant:\n%+v", got, tt.want)
			}
		})
	}
	// Two of the four took the default, and are counted for their source.
	if got := f.unmatched("staging"); got != 2 {
		t.Errorf("beacon_routing_unmatched_alerts_total{source=staging} = %v, want 2", got)
	}
	if got := f.unmatched("prod"); got != 0 {
		t.Errorf("beacon_routing_unmatched_alerts_total{source=prod} = %v, want 0", got)
	}
	// The metric is exposed with a series for every source from the start,
	// and with nothing but the source as a label.
	series := metricstest.WithPrefix(metricstest.Values(t, f.reg), "beacon_routing_unmatched_alerts_total")
	want := []string{`beacon_routing_unmatched_alerts_total{source="am"}`, `beacon_routing_unmatched_alerts_total{source="grafana"}`,
		`beacon_routing_unmatched_alerts_total{source="prod"}`, `beacon_routing_unmatched_alerts_total{source="staging"}`}
	if !slices.Equal(series, want) {
		t.Errorf("series = %v, want %v", series, want)
	}
}

// TestRoutingReadsTheLabelsOfTheSource: a label that the source adds routes
// like one the alert sent, and an alert's own label wins.
func TestRoutingReadsTheLabelsOfTheSource(t *testing.T) {
	f := newRouted(t)
	// The source "prod" adds team=from-source; this alert says team=dba.
	res := f.post(t, "prod", prodToken, `{"labels": {"alertname": "A", "team": "dba"}}`)
	if got, _ := f.stored(t, res.body["alert_id"]); got.Rule != "database" {
		t.Errorf("rule = %q, want database", got.Rule)
	}
	res = f.post(t, "prod", prodToken, `{"labels": {"alertname": "B"}}`)
	if got, _ := f.stored(t, res.body["alert_id"]); got.Rule != "" {
		t.Errorf("rule = %q, want the default: the source's team label is from-source", got.Rule)
	}
}

// TestRepeatedDeliveryIsNotRoutedAgain: an open alert keeps its route, and a
// repeat is not counted as unmatched a second time.
func TestRepeatedDeliveryIsNotRoutedAgain(t *testing.T) {
	f := newRouted(t)
	first := f.post(t, "staging", stagingToken, `{"fingerprint": "x", "labels": {"alertname": "A", "severity": "warning"}}`)
	before, group := f.stored(t, first.body["alert_id"])
	// The same alert again, as it was: still unmatched, and not counted again.
	if again := f.post(t, "staging", stagingToken, `{"fingerprint": "x", "labels": {"alertname": "A", "severity": "warning"}}`); again.code != http.StatusOK {
		t.Fatalf("repeat = %d %v", again.code, again.body)
	}
	for range 3 {
		// The same alert, now with labels that the first rule would match.
		again := f.post(t, "staging", stagingToken, `{"fingerprint": "x", "labels": {"alertname": "A", "team": "dba"}}`)
		if again.code != http.StatusOK || again.body["alert_id"] != first.body["alert_id"] {
			t.Fatalf("repeat = %d %v", again.code, again.body)
		}
	}
	after, sameGroup := f.stored(t, first.body["alert_id"])
	if after != before || sameGroup != group {
		t.Errorf("after the repeats: %+v in %s, want what it had: %+v in %s", after, sameGroup, before, group)
	}
	if got := f.unmatched("staging"); got != 1 {
		t.Errorf("unmatched = %v, want 1: the alert was routed once", got)
	}
}

// TestGroupsFormWithinARule: alerts that agree on the group labels share a
// group only when the same rule matched them, because rules may send them to
// different people.
func TestGroupsFormWithinARule(t *testing.T) {
	f := newRouted(t)
	group := func(labels string) string {
		t.Helper()
		res := f.post(t, "staging", stagingToken, `{"labels": `+labels+`}`)
		if res.code != http.StatusCreated {
			t.Fatalf("fire = %d %v", res.code, res.body)
		}
		_, id := f.stored(t, res.body["alert_id"])
		return id
	}
	dba1 := group(`{"alertname": "Down", "team": "dba", "host": "db1"}`)
	dba2 := group(`{"alertname": "Down", "team": "dba", "host": "db2"}`)
	other := group(`{"alertname": "Down", "host": "web1"}`)
	if dba1 != dba2 {
		t.Error("two alerts of one rule with the same alertname are in different groups")
	}
	if other == dba1 {
		t.Error("an alert that took the default shares a group with alerts of a rule")
	}
	// The rule "pages" groups by alertname and instance.
	p1 := group(`{"alertname": "Down", "severity": "critical", "instance": "a"}`)
	p2 := group(`{"alertname": "Down", "severity": "critical", "instance": "b"}`)
	p3 := group(`{"alertname": "Down", "severity": "page", "instance": "a", "host": "x"}`)
	if p1 == p2 || p1 != p3 {
		t.Errorf("groups of the rule with its own group_by: %s %s %s; want the first and third together", p1, p2, p3)
	}
}

// TestSeverityIsMappedForEverySourceType: the severity comes from the label
// and the mapping of the source, whatever payload the alert arrived in.
func TestSeverityIsMappedForEverySourceType(t *testing.T) {
	f := newFixture(t)
	mapping := config.SeverityMapping{Label: "priority", Mapping: map[string]alert.Severity{"P1": alert.SeverityCritical, "P3": alert.SeverityInfo}, Default: alert.SeverityWarning}
	var sources []ingest.Source
	for _, s := range []struct{ name, typ, token string }{
		{"prod", config.SourceTypeWebhook, prodToken}, {"am", config.SourceTypeAlertmanager, amToken}, {"grafana", config.SourceTypeGrafana, grafanaToken},
	} {
		sources = append(sources, ingest.Source{ID: f.ids[s.name], Source: config.Source{
			Name: s.name, Type: s.typ, Token: config.NewSecret(config.SecretRef{}, s.token), Severity: mapping,
		}})
	}
	f.handler.SetSources(sources)

	for priority, want := range map[string]alert.Severity{"P1": alert.SeverityCritical, "P3": alert.SeverityInfo, "P9": alert.SeverityWarning, "": alert.SeverityWarning} {
		labels := fmt.Sprintf(`{"alertname": "A%s", "priority": %q, "severity": "critical"}`, priority, priority)
		if priority == "" {
			labels = `{"alertname": "NoPriority", "severity": "critical"}`
		}
		for source, body := range map[string]string{
			"prod":    `{"labels": ` + labels + `}`,
			"am":      `{"version": "4", "alerts": [{"status": "firing", "labels": ` + labels + `}]}`,
			"grafana": `{"version": "1", "alerts": [{"status": "firing", "labels": ` + labels + `}]}`,
		} {
			token := map[string]string{"prod": prodToken, "am": amToken, "grafana": grafanaToken}[source]
			var out map[string]any
			if code := f.postInto(t, source, token, body, &out); code != http.StatusOK && code != http.StatusCreated {
				t.Fatalf("%s: %d %v", source, code, out)
			}
			var got string
			name := strings.Trim(strings.Split(strings.Split(labels, `"alertname": `)[1], ",")[0], `"`)
			err := f.pool.QueryRow(context.Background(), `SELECT severity FROM alerts WHERE source_id = $1 AND labels->>'alertname' = $2`, f.ids[source], name).Scan(&got)
			if err != nil || got != string(want) {
				t.Errorf("%s, priority %q: severity = %q, %v; want %s", source, priority, got, err, want)
			}
		}
	}
}
