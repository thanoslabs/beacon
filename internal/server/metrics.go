package server

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/thanoslabs/beacon/internal/version"
)

// NewRegistry returns a metrics registry with the Go runtime, process and
// build information collectors. Later features register their own metrics on
// it. A registry per server, not the global default, keeps tests independent.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	info := version.Get()
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "beacon_build_info",
		Help: "Build information for the running Beacon binary. The value is always 1.",
		ConstLabels: prometheus.Labels{
			"version":   info.Version,
			"commit":    info.Commit,
			"goversion": info.GoVersion,
		},
	})
	buildInfo.Set(1)
	reg.MustRegister(buildInfo)
	return reg
}
