package routing_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/routing"
)

func duration(d time.Duration) *config.Duration {
	c := config.Duration(d)
	return &c
}

var refs = routing.Refs{
	Rules:    map[string]string{"database": "rule-1", "pages": "rule-2", "europe": "rule-3", "catch-http": "rule-4"},
	Teams:    map[string]string{"dba": "team-1", "platform": "team-2"},
	Policies: map[string]string{"dba": "policy-1", "urgent": "policy-2", "default": "policy-3"},
}

func block() *config.Routing {
	return &config.Routing{
		GroupBy:       []string{"alertname", "cluster"},
		GroupInterval: duration(10 * time.Minute),
		Default:       config.RouteDefault{Team: "platform", EscalationPolicy: "default"},
		Rules: []config.RoutingRule{
			{Name: "database", Match: map[string]string{"team": "dba"}, Team: "dba", Service: "postgres", EscalationPolicy: "dba",
				GroupBy: []string{"alertname"}, GroupWait: duration(0)},
			{Name: "pages", MatchRE: map[string]string{"severity": "critical|page"}, EscalationPolicy: "urgent"},
			{Name: "europe", Match: map[string]string{"env": "prod"}, MatchRE: map[string]string{"region": "eu-.*"}, Team: "platform", EscalationPolicy: "default"},
			{Name: "catch-http", MatchRE: map[string]string{"code": "5.."}, EscalationPolicy: "default"},
		},
	}
}

func TestRoute(t *testing.T) {
	rt, err := routing.New(block(), refs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base := config.Grouping{By: []string{"alertname", "cluster"}, Wait: 30 * time.Second, Interval: 10 * time.Minute}
	tests := []struct {
		name   string
		labels map[string]string
		want   routing.Route
	}{
		{
			name:   "the first rule, by an exact label",
			labels: map[string]string{"team": "dba", "severity": "critical", "service": "from-the-alert"},
			want: routing.Route{Rule: "database", RuleID: "rule-1", Team: "dba", TeamID: "team-1", Service: "postgres", Policy: "dba", PolicyID: "policy-1",
				Grouping: config.Grouping{By: []string{"alertname"}, Wait: 0, Interval: 10 * time.Minute}},
		},
		{
			name:   "a later rule, by a regular expression; the service comes from the alert",
			labels: map[string]string{"team": "web", "severity": "page", "service": "checkout"},
			want:   routing.Route{Rule: "pages", RuleID: "rule-2", Service: "checkout", Policy: "urgent", PolicyID: "policy-2", Grouping: base},
		},
		{
			name:   "match and match_re must both hold",
			labels: map[string]string{"env": "prod", "region": "eu-west-1"},
			want:   routing.Route{Rule: "europe", RuleID: "rule-3", Team: "platform", TeamID: "team-2", Policy: "default", PolicyID: "policy-3", Grouping: base},
		},
		{
			name:   "no rule: the default",
			labels: map[string]string{"env": "prod", "region": "us-east-1", "service": "billing"},
			want:   routing.Route{Team: "platform", TeamID: "team-2", Service: "billing", Policy: "default", PolicyID: "policy-3", Grouping: base},
		},
		{
			name:   "no labels at all: the default",
			labels: nil,
			want:   routing.Route{Team: "platform", TeamID: "team-2", Policy: "default", PolicyID: "policy-3", Grouping: base},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rt.Route(tt.labels)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Route(%v) =\n%+v\nwant\n%+v", tt.labels, got, tt.want)
			}
			if got.Matched() != (tt.want.Rule != "") {
				t.Errorf("Matched() = %v", got.Matched())
			}
		})
	}
}

// TestRegularExpressionsAreAnchored: a pattern must match the whole value,
// as in Prometheus and Alertmanager.
func TestRegularExpressionsAreAnchored(t *testing.T) {
	rt, err := routing.New(block(), refs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for value, want := range map[string]string{
		"500": "catch-http", "503": "catch-http",
		"1500": "", "5000": "", "50": "", "x503": "", "503x": "", "": "",
	} {
		if got := rt.Route(map[string]string{"code": value}).Rule; got != want {
			t.Errorf("code=%q matched rule %q, want %q", value, got, want)
		}
	}
	// An alternation is anchored as a whole, not only its first branch.
	for value, want := range map[string]string{"critical": "pages", "page": "pages", "pager": "", "non-critical": "", "criticalpage": ""} {
		if got := rt.Route(map[string]string{"severity": value}).Rule; got != want {
			t.Errorf("severity=%q matched rule %q, want %q", value, got, want)
		}
	}
}

// TestAMissingLabelCountsAsEmpty: a rule can ask for a label to be absent,
// and a pattern that matches the empty string matches an alert without the
// label.
func TestAMissingLabelCountsAsEmpty(t *testing.T) {
	r := &config.Routing{
		Default: config.RouteDefault{EscalationPolicy: "default"},
		Rules: []config.RoutingRule{
			{Name: "database", Match: map[string]string{"team": ""}, MatchRE: map[string]string{"env": "prod|"}, EscalationPolicy: "dba"},
		},
	}
	rt, err := routing.New(r, refs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, tt := range []struct {
		labels map[string]string
		want   bool
	}{
		{map[string]string{"alertname": "A"}, true},
		{map[string]string{"env": "prod"}, true},
		{map[string]string{"env": "dev"}, false},
		{map[string]string{"team": "dba"}, false},
	} {
		if got := rt.Route(tt.labels).Matched(); got != tt.want {
			t.Errorf("Route(%v) matched = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func TestNoRoutingBlock(t *testing.T) {
	rt, err := routing.New(nil, routing.Refs{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := rt.Route(map[string]string{"alertname": "A", "service": "api"})
	want := routing.Route{Service: "api", Grouping: config.Grouping{By: []string{"alertname"}, Wait: 30 * time.Second, Interval: 5 * time.Minute}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Route = %+v, want %+v", got, want)
	}
}

func TestNewRefusesWhatItCannotResolve(t *testing.T) {
	for name, change := range map[string]func(r *config.Routing, refs *routing.Refs){
		`escalation policy "default" has no row`: func(_ *config.Routing, refs *routing.Refs) {
			refs.Policies = map[string]string{"dba": "p", "urgent": "p"}
		},
		`team "dba" has no row`: func(_ *config.Routing, refs *routing.Refs) { refs.Teams = map[string]string{"platform": "t"} },
		`routing rule "pages": the rule has no row`: func(_ *config.Routing, refs *routing.Refs) {
			refs.Rules = map[string]string{"database": "r", "europe": "r", "catch-http": "r"}
		},
		`match_re.severity: is not a regular expression`: func(r *config.Routing, _ *routing.Refs) {
			r.Rules[1].MatchRE = map[string]string{"severity": "(critical"}
		},
		`match_re.severity: is longer than 1024`: func(r *config.Routing, _ *routing.Refs) {
			r.Rules[1].MatchRE = map[string]string{"severity": strings.Repeat("a", 1025)}
		},
	} {
		r, local := block(), refs
		change(r, &local)
		if _, err := routing.New(r, local); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("New = %v, want an error containing %q", err, name)
		}
	}
}

// TestHostilePatterns: patterns that make backtracking engines take
// exponential time are matched in time linear in the input, because Go's
// regexp package does not backtrack. The test would hang otherwise.
func TestHostilePatterns(t *testing.T) {
	input := strings.Repeat("a", 5000) + "!"
	for _, expr := range []string{"(a+)+", "(a|aa)+", "(a*)*b", "([a-z]+)*", "(.*a){20}"} {
		r := &config.Routing{
			Default: config.RouteDefault{EscalationPolicy: "default"},
			Rules:   []config.RoutingRule{{Name: "database", MatchRE: map[string]string{"x": expr}, EscalationPolicy: "dba"}},
		}
		rt, err := routing.New(r, refs)
		if err != nil {
			t.Fatalf("New(%q): %v", expr, err)
		}
		start := time.Now()
		for range 5 {
			if rt.Route(map[string]string{"x": input}).Matched() {
				t.Errorf("%q matched an input it cannot match", expr)
			}
		}
		if took := time.Since(start); took > 20*time.Second {
			t.Errorf("%q took %v for 5 matches of a 5 kB value", expr, took)
		}
	}
}
