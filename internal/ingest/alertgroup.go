package ingest

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/thanoslabs/beacon/internal/alert"
)

// AlertmanagerVersion is the version of Alertmanager's webhook payload that
// Beacon reads: the value of its "version" field. docs/alertmanager.md
// describes the payload.
const AlertmanagerVersion = "4"

// maxLinkLength bounds a web address kept with an alert.
const maxLinkLength = 2048

// alertGroup is what Beacon reads from a payload in the shape Alertmanager
// invented, which Grafana's webhook contact point follows: one notification
// about a group of alerts.
type alertGroup struct {
	// Version is the payload's "version" field, or empty when it has none.
	Version string
	// Truncated is how many alerts the sender left out of the payload.
	Truncated  int
	Deliveries []delivery
}

// alertLinks maps the fields of an alert that hold web addresses to the names
// under which Beacon keeps them.
type alertLinks map[string]string

// alertmanagerLinks are the links of an alert from Alertmanager.
var alertmanagerLinks = alertLinks{"generatorURL": alert.LinkGenerator}

// parseAlertmanager reads the body that Alertmanager's webhook receiver
// sends. Every error is a *fieldError.
func parseAlertmanager(body []byte) (alertGroup, error) {
	return parseAlertGroup(body, alertmanagerLinks)
}

// parseAlertGroup reads a group notification. Only the fields Beacon uses are
// checked, and fields it does not know are ignored, so that a newer sender
// which adds a field keeps working. The fields of the group itself, such as
// commonLabels, repeat what the alerts hold and are not read.
func parseAlertGroup(body []byte, links alertLinks) (alertGroup, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return alertGroup{}, &fieldError{msg: "the body must be a JSON object"}
	}
	var g alertGroup
	if v, ok := raw["version"]; ok {
		if err := stringField("version", v, &g.Version); err != nil {
			return alertGroup{}, err
		}
	}
	if v, ok := raw["truncatedAlerts"]; ok && !isNull(v) {
		if err := json.Unmarshal(v, &g.Truncated); err != nil || g.Truncated < 0 {
			return alertGroup{}, &fieldError{field: "truncatedAlerts", msg: "must be a number, zero or more"}
		}
	}

	var alerts []json.RawMessage
	v, ok := raw["alerts"]
	if !ok {
		return alertGroup{}, &fieldError{field: "alerts", msg: "is required: the list of alerts, as the default webhook payload has it"}
	}
	if err := json.Unmarshal(v, &alerts); err != nil || isNull(v) {
		return alertGroup{}, &fieldError{field: "alerts", msg: "must be a list of alerts"}
	}
	for i, a := range alerts {
		d, err := parseGroupAlert(fmt.Sprintf("alerts[%d]", i), a, links)
		if err != nil {
			return alertGroup{}, err
		}
		g.Deliveries = append(g.Deliveries, d)
	}
	return g, nil
}

// parseGroupAlert reads one alert of a group notification. at is its place
// in the payload, for error messages.
func parseGroupAlert(at string, value json.RawMessage, links alertLinks) (delivery, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(value, &raw); err != nil || raw == nil {
		return delivery{}, &fieldError{field: at, msg: "must be an object"}
	}
	var (
		d      delivery
		status string
		err    error
	)
	if err := stringField(at+".status", raw["status"], &status); err != nil || (status != statusFiring && status != statusResolved) {
		return delivery{}, &fieldError{field: at + ".status", msg: fmt.Sprintf("must be %q or %q", statusFiring, statusResolved)}
	}
	d.Resolved = status == statusResolved

	if d.Labels, err = optionalStringMap(at+".labels", raw["labels"]); err != nil {
		return delivery{}, err
	}
	if d.Annotations, err = optionalStringMap(at+".annotations", raw["annotations"]); err != nil {
		return delivery{}, err
	}
	if v, ok := raw["fingerprint"]; ok && !isNull(v) {
		if err := stringField(at+".fingerprint", v, &d.Fingerprint); err != nil {
			return delivery{}, err
		}
		if len(d.Fingerprint) > maxFingerprintLength {
			return delivery{}, &fieldError{field: at + ".fingerprint", msg: fmt.Sprintf("must be at most %d characters", maxFingerprintLength)}
		}
	}
	if len(d.Labels) == 0 && (!d.Resolved || d.Fingerprint == "") {
		return delivery{}, &fieldError{field: at + ".labels", msg: "an alert needs at least one label"}
	}
	if d.StartsAt, err = optionalTime(at+".startsAt", raw["startsAt"]); err != nil {
		return delivery{}, err
	}
	if d.EndsAt, err = optionalTime(at+".endsAt", raw["endsAt"]); err != nil {
		return delivery{}, err
	}
	if !d.Resolved {
		// A firing alert may carry the time at which it will be taken as
		// resolved if nothing more is heard of it. That is not an end.
		d.EndsAt = time.Time{}
	}

	for field, name := range links {
		var address string
		if v, ok := raw[field]; ok && !isNull(v) {
			if err := stringField(at+"."+field, v, &address); err != nil {
				return delivery{}, err
			}
		}
		if webAddress(address) {
			if d.Links == nil {
				d.Links = map[string]string{}
			}
			d.Links[name] = address
		}
	}
	return d, nil
}

// optionalStringMap is stringMapField for a field that may be absent or null.
func optionalStringMap(name string, value json.RawMessage) (map[string]string, error) {
	if value == nil || isNull(value) {
		return nil, nil
	}
	return stringMapField(name, value)
}

// optionalTime reads a time that may be absent, null, empty or the zero time,
// which is how Go programs write "none".
func optionalTime(name string, value json.RawMessage) (time.Time, error) {
	if value == nil || isNull(value) || string(value) == `""` {
		return time.Time{}, nil
	}
	t, err := timeField(name, value)
	if err != nil || t.Year() <= 1 {
		return time.Time{}, err
	}
	return t, nil
}

// webAddress reports whether raw is an http or https address that is short
// enough to keep. Anything else a source sends as a link is dropped: a link
// is shown to people, so it must not be another kind of URL.
func webAddress(raw string) bool {
	if raw == "" || len(raw) > maxLinkLength {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
