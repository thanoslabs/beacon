// Package metricstest reads metrics in tests the way a scrape shows them.
package metricstest

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// Values gathers from g and returns the value of every series, by the
// series as PromQL writes it: `name{a="b",c="d"}`, with the labels in
// alphabetical order, or `name` alone. A histogram is there with its count
// and its sum, as `name_count{...}` and `name_sum{...}`.
func Values(t testing.TB, g prometheus.Gatherer) map[string]float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	return FromFamilies(families)
}

// FromFamilies is Values for families that were gathered or parsed already.
func FromFamilies(families []*dto.MetricFamily) map[string]float64 {
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			labels := Series("", m)
			switch {
			case m.GetCounter() != nil:
				out[f.GetName()+labels] = m.GetCounter().GetValue()
			case m.GetGauge() != nil:
				out[f.GetName()+labels] = m.GetGauge().GetValue()
			case m.GetHistogram() != nil:
				out[f.GetName()+"_count"+labels] = float64(m.GetHistogram().GetSampleCount())
				out[f.GetName()+"_sum"+labels] = m.GetHistogram().GetSampleSum()
			case m.GetUntyped() != nil:
				out[f.GetName()+labels] = m.GetUntyped().GetValue()
			}
		}
	}
	return out
}

// Series writes the labels of m after name, as PromQL does.
func Series(name string, m *dto.Metric) string {
	pairs := make([]string, 0, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		pairs = append(pairs, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
	}
	if len(pairs) == 0 {
		return name
	}
	sort.Strings(pairs)
	return name + "{" + strings.Join(pairs, ",") + "}"
}

// Expect fails the test for every series of want that got does not have
// with that value.
func Expect(t testing.TB, got map[string]float64, want map[string]float64) {
	t.Helper()
	for series, value := range want {
		if v, ok := got[series]; !ok {
			t.Errorf("%s is not exposed, want %v", series, value)
		} else if v != value {
			t.Errorf("%s = %v, want %v", series, v, value)
		}
	}
}

// WithPrefix returns the series of values whose name starts with prefix.
func WithPrefix(values map[string]float64, prefix string) []string {
	var out []string
	for series := range values {
		if strings.HasPrefix(series, prefix) {
			out = append(out, series)
		}
	}
	sort.Strings(out)
	return out
}
