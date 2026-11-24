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
	"type":        "Type of the integration: `slack`, `discord`, `email` or `webhook`.",
	"team":        "Name of a team in the configuration; `none` for an alert that no team was assigned, and `other` for a team that has left the configuration.",
	"schedule":    "Name of a schedule in the configuration.",
	"policy":      "Name of an escalation policy in the configuration.",
	"step":        "Number of a step in its escalation policy, from 1. The configuration says how many steps a policy has.",
	"service":     "Name of a service that a routing rule of the configuration names. A service that an alert brought in its own `service` label is `other`, and an alert without a service is `none`.",
	"severity":    "`critical`, `warning` or `info`.",
	"state":       "State of an alert that is not resolved: `firing`, `acknowledged` or `snoozed`.",
	"result":      "A fixed set of outcomes, listed with the metric.",
	"outcome":     "Outcome of a delivery attempt: `success`, `retryable_error` or `permanent_error`.",
	"route":       "Pattern of the route that took the request, such as `/api/v1/alerts/{id}`, never the address that was asked for; `unmatched` when no route did.",
	"method":      "HTTP method of the route.",
	"code":        "HTTP status code of the answer.",
	"version":     "Version of the binary.",
	"commit":      "Commit the binary was built from.",
	"goversion":   "Go version the binary was built with.",
}

// The headings of the reference, in order.
const (
	GroupProcess       = "Process and configuration"
	GroupHTTP          = "HTTP and API"
	GroupIngest        = "Ingestion"
	GroupNotifications = "Notifications"
	GroupAlerts        = "Alert handling"
	GroupEscalation    = "Escalation"
	GroupPeople        = "People"
)

// Groups are the headings of the reference, in the order they are shown.
var Groups = []string{GroupProcess, GroupHTTP, GroupIngest, GroupAlerts, GroupEscalation, GroupNotifications, GroupPeople}

// DurationBuckets are the buckets of a request or a delivery: five
// milliseconds to ten seconds.
var DurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// HandlingBuckets are the buckets of the time people take over an alert:
// fifteen seconds to a day. On-call work is judged in minutes, so that is
// where they are close together.
var HandlingBuckets = []float64{15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 86400}

// The names of the metrics, for the code that creates them.
const (
	BuildInfo                 = "beacon_build_info"
	ConfigReloads             = "beacon_config_reloads_total"
	ConfigLastReloadOK        = "beacon_config_last_reload_successful"
	ConfigLastReloadTimestamp = "beacon_config_last_reload_success_timestamp_seconds"
	StateUp                   = "beacon_state_metrics_up"
	StateReadDuration         = "beacon_state_metrics_read_duration_seconds"

	HTTPRequests        = "beacon_http_requests_total"
	HTTPRequestDuration = "beacon_http_request_duration_seconds"

	IngestRequests     = "beacon_ingest_requests_total"
	IngestUnauthorized = "beacon_ingest_unauthorized_requests_total"
	IngestAlerts       = "beacon_ingest_alerts_total"
	RoutingUnmatched   = "beacon_routing_unmatched_alerts_total"

	NotificationAttempts     = "beacon_notification_attempts_total"
	NotificationsFailed      = "beacon_notifications_failed_total"
	NotificationSendDuration = "beacon_notification_send_duration_seconds"
	NotificationsPending     = "beacon_notifications_pending"
	NotificationsOldest      = "beacon_notifications_oldest_pending_age_seconds"

	AlertsOpened           = "beacon_alerts_opened_total"
	AlertTimeToAcknowledge = "beacon_alert_time_to_acknowledge_seconds"
	AlertTimeToResolve     = "beacon_alert_time_to_resolve_seconds"
	AlertsOpen             = "beacon_alerts_open"

	EscalationSteps      = "beacon_escalation_steps_total"
	EscalationsExhausted = "beacon_escalations_exhausted_total"

	ScheduleCovered        = "beacon_schedule_covered"
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
		Name: StateUp, Type: Gauge, Scope: Replica, Group: GroupProcess,
		Help: "Whether this process could read the global metrics from Postgres for this scrape: 1 or 0. When 0, the metrics that are read from Postgres are absent from the scrape rather than stale.",
	},
	{
		Name: StateReadDuration, Type: Gauge, Scope: Replica, Group: GroupProcess,
		Help: "How long the last read of the global metrics from Postgres took, in seconds. The read is abandoned after two seconds.",
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
		Name: IngestRequests, Type: Counter, Scope: Replica, Group: GroupIngest, Labels: []string{"source", "result"},
		Help: "Requests to the ingest endpoint that carried the token of a source, by source and result: accepted, invalid (the payload was refused) or error (an alert could not be stored, and the sender is asked to send again).",
	},
	{
		Name: IngestUnauthorized, Type: Counter, Scope: Replica, Group: GroupIngest,
		Help: "Requests to the ingest endpoint that named no source of the configuration or carried a wrong token. The name that was asked for is not a label: anyone can ask for any name.",
	},
	{
		Name: IngestAlerts, Type: Counter, Scope: Replica, Group: GroupIngest, Labels: []string{"source", "result"},
		Help: "Alerts in accepted deliveries, by source and by what became of them: created (a new alert), updated (a repeat of an open alert), resolved, or not_found (a resolution of an alert Beacon does not have open).",
	},
	{
		Name: RoutingUnmatched, Type: Counter, Scope: Replica, Group: GroupIngest, Labels: []string{"source"},
		Help: "Alerts opened that matched no routing rule and took the routing default, by source. Repeated deliveries of an open alert are not counted.",
	},

	{
		Name: AlertsOpened, Type: Counter, Scope: Replica, Group: GroupAlerts, Labels: []string{"team", "service", "severity"},
		Help: "Alerts opened, by the team and the service that routing assigned and by severity. A repeated delivery of an open alert opens nothing and is not counted.",
	},
	{
		Name: AlertTimeToAcknowledge, Type: Histogram, Scope: Replica, Group: GroupAlerts, Labels: []string{"team", "severity"}, Buckets: HandlingBuckets,
		Help: "Time from the moment Beacon opened an alert to the first time a person acknowledged or snoozed it, in seconds. Observed once per alert; an alert that is resolved before anyone responds is not in it.",
	},
	{
		Name: AlertTimeToResolve, Type: Histogram, Scope: Replica, Group: GroupAlerts, Labels: []string{"team", "severity"}, Buckets: HandlingBuckets,
		Help: "Time from the moment Beacon opened an alert to its resolution, by its source or by a person, in seconds. Observed once per alert.",
	},
	{
		Name: AlertsOpen, Type: Gauge, Scope: Global, Group: GroupAlerts, Labels: []string{"team", "severity", "state"},
		Help: "Alerts that are not resolved, by team, severity and state.",
	},

	{
		Name: EscalationSteps, Type: Counter, Scope: Replica, Group: GroupEscalation, Labels: []string{"policy", "step", "result"},
		Help: "Escalation steps run, by policy, by the number of the step and by result: notified (at least one target was told) or nobody (every target was skipped, so the step paged no one).",
	},
	{
		Name: EscalationsExhausted, Type: Counter, Scope: Replica, Group: GroupEscalation, Labels: []string{"policy"},
		Help: "Escalations that ended because every step had run, as often as the policy repeats, while an alert was still firing: nobody responded. An escalation whose policy left the configuration while it ran ends the same way and is counted under the policy `other`.",
	},

	{
		Name: NotificationAttempts, Type: Counter, Scope: Replica, Group: GroupNotifications, Labels: []string{"integration", "type", "outcome"},
		Help: "Delivery attempts, by integration, its type and the outcome. A retryable_error is tried again; a permanent_error is not.",
	},
	{
		Name: NotificationsFailed, Type: Counter, Scope: Replica, Group: GroupNotifications, Labels: []string{"integration", "type"},
		Help: "Notifications that were given up: the failure was permanent, or every attempt failed. Each is a message somebody did not get.",
	},
	{
		Name: NotificationSendDuration, Type: Histogram, Scope: Replica, Group: GroupNotifications, Labels: []string{"type"}, Buckets: DurationBuckets,
		Help: "Time one delivery attempt took, in seconds, by the type of the integration, whatever its outcome.",
	},
	{
		Name: NotificationsPending, Type: Gauge, Scope: Global, Group: GroupNotifications, Labels: []string{"integration"},
		Help: "Notifications in the queue that are neither delivered nor given up, by integration: the queue depth. It includes those that wait for their next attempt.",
	},
	{
		Name: NotificationsOldest, Type: Gauge, Scope: Global, Group: GroupNotifications,
		Help: "Age in seconds of the oldest notification in the queue that is neither delivered nor given up; 0 when the queue is empty.",
	},

	{
		Name: ScheduleCovered, Type: Gauge, Scope: Global, Group: GroupPeople, Labels: []string{"schedule"},
		Help: "Whether somebody is on call for the schedule now, by the rotation or by an override: 1, or 0 during a gap in coverage. It says that someone is on call, never who.",
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
