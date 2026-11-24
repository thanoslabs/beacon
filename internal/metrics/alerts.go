package metrics

import (
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Names is what the active configuration calls the things that alerts refer
// to. It is what bounds the labels of the alert metrics: a team or a
// service is a label value only when it is in here.
type Names struct {
	// Teams are the names of the teams, by the identifier of their rows.
	Teams map[string]string
	// Services are the services that routing rules name.
	Services map[string]bool
}

// Severities are the values of the severity label, most severe first.
var Severities = []string{"critical", "warning", "info"}

// States are the values of the state label.
var States = []string{"firing", "acknowledged", "snoozed"}

// Alerts are the metrics of alert handling: how many alerts were opened and
// how long people took over them.
type Alerts struct {
	opened        *prometheus.CounterVec
	toAcknowledge *prometheus.HistogramVec
	toResolve     *prometheus.HistogramVec
	names         atomic.Pointer[Names]
}

// NewAlerts creates the metrics of alert handling.
func NewAlerts() *Alerts {
	a := &Alerts{
		opened:        NewCounterVec(AlertsOpened),
		toAcknowledge: NewHistogramVec(AlertTimeToAcknowledge),
		toResolve:     NewHistogramVec(AlertTimeToResolve),
	}
	a.SetNames(Names{})
	return a
}

// Register registers the metrics on reg.
func (a *Alerts) Register(reg prometheus.Registerer) {
	reg.MustRegister(a.opened, a.toAcknowledge, a.toResolve)
}

// SetNames replaces the names. It is safe while alerts are being counted.
func (a *Alerts) SetNames(n Names) {
	a.names.Store(&n)
	// The histograms of every team exist from the start, so that a quantile
	// over them is never empty.
	for _, team := range n.Teams {
		for _, severity := range Severities {
			a.toAcknowledge.WithLabelValues(team, severity)
			a.toResolve.WithLabelValues(team, severity)
		}
	}
}

// Names returns the names that are in use.
func (a *Alerts) Names() Names { return *a.names.Load() }

// Team returns the label value for the team with an identifier: its name in
// the active configuration, None for no team, and Other for a team that the
// configuration no longer has.
func (a *Alerts) Team(id string) string {
	if id == "" {
		return None
	}
	if name, ok := a.names.Load().Teams[id]; ok {
		return name
	}
	return Other
}

// Service returns the label value for a service: the service itself when a
// routing rule names it, None for no service, and Other for anything else.
// An alert may bring its service in a label of its own, and what an alert
// brings is not a bounded set.
func (a *Alerts) Service(service string) string {
	if service == "" {
		return None
	}
	if a.names.Load().Services[service] {
		return service
	}
	return Other
}

// Severity returns the label value for a severity.
func Severity(severity string) string {
	for _, known := range Severities {
		if severity == known {
			return severity
		}
	}
	return Other
}

// Opened counts an alert that was opened.
func (a *Alerts) Opened(teamID, service, severity string) {
	a.opened.WithLabelValues(a.Team(teamID), a.Service(service), Severity(severity)).Inc()
}

// Acknowledged observes how long it took until a person first responded to
// an alert.
func (a *Alerts) Acknowledged(teamID, severity string, took time.Duration) {
	a.toAcknowledge.WithLabelValues(a.Team(teamID), Severity(severity)).Observe(max(took, 0).Seconds())
}

// Resolved observes how long it took until an alert was resolved.
func (a *Alerts) Resolved(teamID, severity string, took time.Duration) {
	a.toResolve.WithLabelValues(a.Team(teamID), Severity(severity)).Observe(max(took, 0).Seconds())
}
