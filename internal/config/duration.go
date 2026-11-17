package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Duration is a length of time written in the Prometheus style: 30s, 5m,
// 12h, 1d, 1w, or several units together such as 1h30m. A day is 24 hours and
// a week is seven days.
type Duration time.Duration

// Day and Week are the units that time.Duration lacks.
const (
	Day  = 24 * time.Hour
	Week = 7 * Day
)

var durationUnits = []struct {
	suffix string
	length time.Duration
}{{"w", Week}, {"d", Day}, {"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}}

var durationRE = regexp.MustCompile(`^(?:(\d+)w)?(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// ParseDuration parses a duration such as 5m or 1w.
func ParseDuration(s string) (Duration, error) {
	m := durationRE.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, fmt.Errorf("%q is not a duration such as 30s, 5m, 12h, 1d or 1w", s)
	}
	var total time.Duration
	for i, u := range durationUnits {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 64)
		if err != nil || n > int64((1<<62)/u.length) {
			return 0, fmt.Errorf("%q is too long a duration", s)
		}
		total += time.Duration(n) * u.length
	}
	return Duration(total), nil
}

// String writes the duration in the form ParseDuration reads.
func (d Duration) String() string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	rest := time.Duration(d)
	for _, u := range durationUnits {
		if n := rest / u.length; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.suffix)
			rest -= n * u.length
		}
	}
	return b.String()
}

// MarshalJSON encodes the duration as its string form.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return typeError(n, "must be a duration such as 30s, 5m, 12h, 1d or 1w")
	}
	parsed, err := ParseDuration(n.Value)
	if err != nil {
		return typeError(n, err.Error())
	}
	*d = parsed
	return nil
}

// typeError reports a problem with one node in the form the decoder collects,
// so that decoding continues and every problem in a file is reported. It
// adds the column, from which Load works out the field.
func typeError(n *yaml.Node, msg string) error {
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: column %d: %s", n.Line, n.Column, msg)}}
}

// Timestamp is a date and time written in RFC 3339, such as
// 2026-01-05T09:00:00Z.
type Timestamp time.Time

// UnmarshalYAML implements yaml.Unmarshaler.
func (t *Timestamp) UnmarshalYAML(n *yaml.Node) error {
	parsed, err := time.Parse(time.RFC3339, n.Value)
	if n.Kind != yaml.ScalarNode || err != nil {
		return typeError(n, "must be a date and time with a zone, such as 2026-01-05T09:00:00Z")
	}
	*t = Timestamp(parsed)
	return nil
}

// MarshalJSON encodes the time in RFC 3339, in UTC.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(t).UTC().Format(time.RFC3339))
}
