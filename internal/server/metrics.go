package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/thanoslabs/beacon/internal/metrics"
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
	// A build that was not stamped has no commit. An empty label is no
	// label to Prometheus, so it gets a word.
	commit := info.Commit
	if commit == "" {
		commit = "unknown"
	}
	buildInfo := metrics.NewGauge(metrics.BuildInfo, info.Version, commit, info.GoVersion)
	buildInfo.Set(1)
	reg.MustRegister(buildInfo)
	return reg
}

// unmatched is the route label of a request that no route took.
const unmatched = "unmatched"

// httpMetrics counts the requests the server answers and measures how long
// they take.
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newHTTPMetrics(reg prometheus.Registerer) *httpMetrics {
	m := &httpMetrics{requests: metrics.NewCounterVec(metrics.HTTPRequests), duration: metrics.NewHistogramVec(metrics.HTTPRequestDuration)}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// instrument measures the requests that next answers as those of the route
// with a pattern. The labels come from the pattern, which is written in the
// code, and never from the request: the address and the method a client
// asks for are whatever the client likes.
func (m *httpMetrics) instrument(pattern string, next http.Handler) http.Handler {
	method, route, found := strings.Cut(pattern, " ")
	if !found {
		// A pattern without a method takes every method.
		method, route = "any", pattern
	}
	if pattern == "" {
		route = unmatched
	}
	duration := m.duration.WithLabelValues(route, method)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			// A handler that panicked is answered with 500 further out.
			status := rec.status
			if v := recover(); v != nil {
				m.observe(duration, route, method, http.StatusInternalServerError, start)
				panic(v)
			}
			if status == 0 {
				status = http.StatusOK
			}
			m.observe(duration, route, method, status, start)
		}()
		next.ServeHTTP(rec, r)
	})
}

func (m *httpMetrics) observe(duration prometheus.Observer, route, method string, status int, start time.Time) {
	duration.Observe(time.Since(start).Seconds())
	m.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
}
