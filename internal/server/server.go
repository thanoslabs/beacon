// Package server is Beacon's HTTP server: the listener, the middleware every
// request passes through, and the health, readiness and metrics endpoints.
// Features attach their routes with Handle, which puts every route that is
// not explicitly public behind authentication.
//
// The server keeps no state of its own. Everything that must survive a
// restart, or be seen by another replica, lives in Postgres.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/thanoslabs/beacon/internal/access"
)

// Paths of the operational endpoints.
const (
	PathLiveness  = "/healthz"
	PathReadiness = "/readyz"
	PathMetrics   = "/metrics"
)

// Pinger checks that a dependency is reachable. *pgxpool.Pool implements it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Config holds the server settings.
type Config struct {
	// Addr is the address to listen on, for example ":8080".
	Addr string
	// ShutdownTimeout is how long in-flight requests get to finish after a
	// termination signal.
	ShutdownTimeout time.Duration
	// ReadinessTimeout bounds the database check made by the readiness
	// endpoint.
	ReadinessTimeout time.Duration
	// Authenticator tells who is behind a request to a route that is not
	// public. Without one, every such route answers 401.
	Authenticator access.Authenticator
}

// Defaults for Config.
const (
	DefaultAddr             = ":8080"
	DefaultShutdownTimeout  = 30 * time.Second
	DefaultReadinessTimeout = 2 * time.Second
)

// Server is the HTTP server.
type Server struct {
	cfg      Config
	logger   *slog.Logger
	db       Pinger
	mux      *http.ServeMux
	guard    *access.Guard
	routes   []access.Route
	http     *http.Server
	listener net.Listener
	metrics  *httpMetrics
	// stopping is set when shutdown begins, so that readiness fails and load
	// balancers stop sending new requests while in-flight ones finish.
	stopping atomic.Bool
}

// New builds a server. db is checked by the readiness endpoint, and registry
// is served on the metrics endpoint.
func New(cfg Config, logger *slog.Logger, db Pinger, registry *prometheus.Registry) *Server {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.ReadinessTimeout <= 0 {
		cfg.ReadinessTimeout = DefaultReadinessTimeout
	}

	s := &Server{
		cfg: cfg, logger: logger, db: db, mux: http.NewServeMux(), guard: access.NewGuard(logger, cfg.Authenticator),
		metrics: newHTTPMetrics(registry),
	}

	// Probes and scrapes come from machines that have no session.
	s.Handle(access.Route{Pattern: "GET " + PathLiveness, Level: access.Public, Handler: http.HandlerFunc(s.handleLiveness)})
	s.Handle(access.Route{Pattern: "GET " + PathReadiness, Level: access.Public, Handler: http.HandlerFunc(s.handleReadiness)})
	s.Handle(access.Route{Pattern: "GET " + PathMetrics, Level: access.Public, Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
	})})
	// Everything else is denied by default: a path that no route matches
	// answers 401 to a stranger, like a route that exists, and 404 only to
	// someone who is signed in.
	s.mux.Handle("/", s.metrics.instrument("", s.NotFound()))

	quiet := map[string]bool{PathLiveness: true, PathReadiness: true, PathMetrics: true}
	// Outermost first: the identifier exists before anything is logged, and
	// a recovered panic is still written to the access log as a 500.
	handler := requestID(accessLog(logger, quiet, recoverPanic(logger, s.mux)))

	s.http = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	return s
}

// Handle attaches a route. This is the only way in: there is no access to
// the router itself, so no route can skip the check of its level. A route
// that is not access.Public needs a valid session or API token, and
// access.Admin needs the admin role as well. Routes must be added before
// Serve is called.
func (s *Server) Handle(route access.Route) {
	s.routes = append(s.routes, route)
	s.mux.Handle(route.Pattern, s.metrics.instrument(route.Pattern, s.guard.Wrap(route)))
}

// NotFound answers a request that no route takes: 401 to a stranger, and 404
// only to someone who is signed in. It is what the router falls back to, and
// what a route that takes many addresses, as the web interface does, must
// hand the addresses of the API to.
func (s *Server) NotFound() http.Handler {
	return s.guard.Wrap(access.Route{Level: access.Member, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "not found"})
	})})
}

// Routes lists every route that was attached, with its level.
func (s *Server) Routes() []access.Route {
	return slices.Clone(s.routes)
}

// Listen opens the listening socket. Calling it before Serve lets the caller
// fail fast on a bad address and learn the port that was bound.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	s.listener = ln
	return nil
}

// Addr returns the address the server is listening on. It is only valid
// after Listen.
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// Serve handles requests until ctx is cancelled, then shuts down gracefully:
// readiness starts failing, the listener closes, and in-flight requests get
// up to the shutdown timeout to finish. It returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Listen(); err != nil {
			return err
		}
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.http.Serve(s.listener)
	}()
	s.logger.InfoContext(ctx, "http server listening", slog.String("addr", s.listener.Addr().String()))

	select {
	case err := <-serveErr:
		// Serve only returns on its own when the listener fails.
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	s.stopping.Store(true)
	s.logger.Info("shutting down http server", slog.Duration("timeout", s.cfg.ShutdownTimeout))

	// ctx is already cancelled, so the shutdown deadline needs a fresh one.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Requests are still running after the timeout: cut them off.
		_ = s.http.Close()
		return fmt.Errorf("shut down http server: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	s.logger.Info("http server stopped")
	return nil
}

// handleLiveness reports that the process is running and able to answer. It
// checks nothing else, so a database outage does not get the process killed.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, statusResponse{Status: "ok"})
}

// handleReadiness reports whether the process can do useful work: it is not
// shutting down and the database answers.
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.stopping.Load() {
		s.writeJSON(w, r, http.StatusServiceUnavailable, statusResponse{
			Status: "unavailable",
			Checks: map[string]string{"server": "shutting down"},
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.ReadinessTimeout)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		// The cause goes to the log, not the response: the endpoint is not
		// authenticated and driver errors can name hosts and users.
		s.logger.WarnContext(r.Context(), "readiness check failed", slog.String("check", "database"), slog.Any("error", err))
		s.writeJSON(w, r, http.StatusServiceUnavailable, statusResponse{
			Status: "unavailable",
			Checks: map[string]string{"database": "unreachable"},
		})
		return
	}
	s.writeJSON(w, r, http.StatusOK, statusResponse{
		Status: "ok",
		Checks: map[string]string{"database": "ok"},
	})
}

// statusResponse is the body of the liveness and readiness endpoints.
type statusResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logger.DebugContext(r.Context(), "write response", slog.Any("error", err))
	}
}
