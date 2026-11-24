// Package metrics is the catalogue of Beacon's own metrics: every metric the
// process exposes is described here, once, with its labels, and is created
// from that description and from nowhere else.
//
// That one list is what keeps three promises. Labels stay bounded, because a
// metric can only have labels from Labels, the allow-list, and a test fails
// when the catalogue names another. The reference in docs/metrics.md is
// written from the list, so it cannot drift. And a metric that is created
// outside this package is found by a test of the source tree.
package metrics

import (
	"fmt"
	"slices"

	"github.com/prometheus/client_golang/prometheus"
)

// Type is the kind of a metric.
type Type string

// The kinds Beacon uses.
const (
	Counter   Type = "counter"
	Gauge     Type = "gauge"
	Histogram Type = "histogram"
)

// Scope says what a value describes, which decides how it is added up when
// several replicas run.
type Scope string

const (
	// Replica is what one process did or holds. Counters and histograms of
	// several replicas are summed.
	Replica Scope = "replica"
	// Global is a fact about the whole install, read from Postgres. Every
	// replica reports the same value, so it is read with max, never sum.
	Global Scope = "global"
)

// Def describes one metric.
type Def struct {
	Name string
	Type Type
	// Help is the text of the HELP line, and of the reference.
	Help string
	// Labels are the names of the variable labels, each a key of Labels.
	Labels []string
	// ConstLabels are labels with one value for the life of the process.
	ConstLabels []string
	// Buckets are the upper bounds of a histogram, in the metric's unit.
	Buckets []float64
	Scope   Scope
	// Group is the heading the metric is listed under in the reference.
	Group string
}

// Label values that stand for everything outside the bounded set.
const (
	// Other is the value of a label for something that the active
	// configuration does not name: the value an alert brought itself, or an
	// object that has left the configuration.
	Other = "other"
	// None is the value of a label for nothing at all: an alert without a
	// team, for example.
	None = "none"
)

// Labels is the allow-list of label names, each with where its values come
// from. A label is allowed only when its values are a bounded set: fixed in
// the code, or named in the configuration files. What an alert brings (its
// name, its fingerprint, its labels), an identifier, a person's name or
// address and a destination are never a label.
var Labels = map[string]string{
	"source":      "Name of a source in the configuration.",
	"integration": "Name of an integration in the configuration.",
	"team":        "Name of a team in the configuration.",
	"schedule":    "Name of a schedule in the configuration.",
	"result":      "A fixed set of outcomes, listed with the metric.",
	"route":       "Pattern of the route that took the request, such as `/api/v1/alerts/{id}`, never the address that was asked for; `unmatched` when no route did.",
	"method":      "HTTP method of the route.",
	"code":        "HTTP status code of the answer.",
	"version":     "Version of the binary.",
	"commit":      "Commit the binary was built from.",
	"goversion":   "Go version the binary was built with.",
}

// The headings of the reference, in order.
const (
	GroupProcess = "Process and configuration"
	GroupHTTP    = "HTTP and API"
	GroupIngest  = "Ingestion"
	GroupPeople  = "People"
)

// Groups are the headings of the reference, in the order they are shown.
var Groups = []string{GroupProcess, GroupHTTP, GroupIngest, GroupPeople}

// DurationBuckets are the buckets of a request: five milliseconds to ten
// seconds.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// The names of the metrics, for the code that creates them.
const (
	BuildInfo                 = "beacon_build_info"
	ConfigReloads             = "beacon_config_reloads_total"
	ConfigLastReloadOK        = "beacon_config_last_reload_successful"
	ConfigLastReloadTimestamp = "beacon_config_last_reload_success_timestamp_seconds"

	HTTPRequests        = "beacon_http_requests_total"
	HTTPRequestDuration = "beacon_http_request_duration_seconds"

	RoutingUnmatched = "beacon_routing_unmatched_alerts_total"

	TeamMembersSkipped     = "beacon_team_members_skipped"
	ScheduleMembersSkipped = "beacon_schedule_members_skipped"
	UsersWithoutIdentity   = "beacon_users_without_identity"
)

var catalog = []Def{
	{
		Name: BuildInfo, Type: Gauge, Scope: Replica, Group: GroupProcess, ConstLabels: []string{"version", "commit", "goversion"},
		Help: "Build information for the running Beacon binary. The value is always 1.",
	},
	{
		Name: ConfigReloads, Type: Counter, Scope: Replica, Group: GroupProcess, Labels: []string{"result"},
		Help: "Configuration reloads since the process started, by result: success or failure. Loading the configuration at start is not counted.",
	},
	{
		Name: ConfigLastReloadOK, Type: Gauge, Scope: Replica, Group: GroupProcess,
		Help: "Whether the last attempt to load the configuration succeeded: 1 or 0. When 0, the configuration loaded before is still active.",
	},
	{
		Name: ConfigLastReloadTimestamp, Type: Gauge, Scope: Replica, Group: GroupProcess,
		Help: "Unix time at which the active configuration was loaded.",
	},

	{
		Name: HTTPRequests, Type: Counter, Scope: Replica, Group: GroupHTTP, Labels: []string{"route", "method", "code"},
		Help: "HTTP requests answered, by the route that took them, its method and the status code.",
	},
	{
		Name: HTTPRequestDuration, Type: Histogram, Scope: Replica, Group: GroupHTTP, Labels: []string{"route", "method"}, Buckets: DurationBuckets,
		Help: "Time from the arrival of an HTTP request to the end of its answer, in seconds, by the route that took it and its method.",
	},

	{
		Name: RoutingUnmatched, Type: Counter, Scope: Replica, Group: GroupIngest, Labels: []string{"source"},
		Help: "Alerts opened that matched no routing rule and took the routing default, by source. Repeated deliveries of an open alert are not counted.",
	},

	{
		Name: TeamMembersSkipped, Type: Gauge, Scope: Replica, Group: GroupPeople, Labels: []string{"team"},
		Help: "Email addresses in a team's members list that belong to no user, by team, as of the last time this process applied the configuration or completed a sign-in. Such a person joins the team when they first sign in.",
	},
	{
		Name: ScheduleMembersSkipped, Type: Gauge, Scope: Replica, Group: GroupPeople, Labels: []string{"schedule"},
		Help: "Email addresses in a schedule's rotation that belong to no user, by schedule, as of the last time this process applied the configuration. Such a member is passed over until the person has signed in.",
	},
	{
		Name: UsersWithoutIdentity, Type: Gauge, Scope: Replica, Group: GroupPeople, Labels: []string{"integration"},
		Help: "Users who have no linked account in the chat service of an integration, by integration, as of the last time this process looked. Such a person cannot be reached directly through that integration.",
	},
}

// Catalog returns every metric of Beacon, in the order of the reference.
func Catalog() []Def { return slices.Clone(catalog) }

// Lookup returns the description of the metric with a name.
func Lookup(name string) (Def, bool) {
	for _, d := range catalog {
		if d.Name == name {
			return d, true
		}
	}
	return Def{}, false
}

// def returns the description of a metric that the code is about to create.
// A metric that is not in the catalogue, or is of another kind than the
// caller thinks, is a mistake in the program, found the first time it runs.
func def(name string, t Type) Def {
	d, ok := Lookup(name)
	if !ok {
		panic(fmt.Sprintf("metrics: %s is not in the catalogue; describe it in internal/metrics first", name))
	}
	if d.Type != t {
		panic(fmt.Sprintf("metrics: %s is a %s in the catalogue, not a %s", name, d.Type, t))
	}
	return d
}

// Desc returns the descriptor of a metric of the catalogue, for a collector
// that makes its own values. constLabels gives the values of the metric's
// constant labels, in the order of the catalogue.
func Desc(name string, constLabels ...string) *prometheus.Desc {
	d, ok := Lookup(name)
	if !ok {
		panic(fmt.Sprintf("metrics: %s is not in the catalogue; describe it in internal/metrics first", name))
	}
	return prometheus.NewDesc(d.Name, d.Help, d.Labels, constant(d, constLabels))
}

func constant(d Def, values []string) prometheus.Labels {
	if len(values) != len(d.ConstLabels) {
		panic(fmt.Sprintf("metrics: %s has %d constant labels, got %d values", d.Name, len(d.ConstLabels), len(values)))
	}
	if len(values) == 0 {
		return nil
	}
	labels := prometheus.Labels{}
	for i, name := range d.ConstLabels {
		labels[name] = values[i]
	}
	return labels
}

// NewCounterVec creates the counter with a name from the catalogue.
func NewCounterVec(name string) *prometheus.CounterVec {
	d := def(name, Counter)
	return prometheus.NewCounterVec(prometheus.CounterOpts{Name: d.Name, Help: d.Help}, d.Labels)
}

// NewCounter creates the counter with a name from the catalogue, which has
// no labels.
func NewCounter(name string) prometheus.Counter {
	d := def(name, Counter)
	if len(d.Labels) != 0 {
		panic(fmt.Sprintf("metrics: %s has labels; use NewCounterVec", name))
	}
	return prometheus.NewCounter(prometheus.CounterOpts{Name: d.Name, Help: d.Help})
}

// NewGaugeVec creates the gauge with a name from the catalogue.
func NewGaugeVec(name string) *prometheus.GaugeVec {
	d := def(name, Gauge)
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: d.Name, Help: d.Help}, d.Labels)
}

// NewGauge creates the gauge with a name from the catalogue, which has no
// variable labels. constLabels gives the values of its constant labels.
func NewGauge(name string, constLabels ...string) prometheus.Gauge {
	d := def(name, Gauge)
	if len(d.Labels) != 0 {
		panic(fmt.Sprintf("metrics: %s has labels; use NewGaugeVec", name))
	}
	return prometheus.NewGauge(prometheus.GaugeOpts{Name: d.Name, Help: d.Help, ConstLabels: constant(d, constLabels)})
}

// NewHistogramVec creates the histogram with a name from the catalogue.
func NewHistogramVec(name string) *prometheus.HistogramVec {
	d := def(name, Histogram)
	return prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: d.Name, Help: d.Help, Buckets: d.Buckets}, d.Labels)
}
