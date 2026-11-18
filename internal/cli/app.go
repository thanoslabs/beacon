package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/auth"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/slack"
)

// provisioner applies a configuration to the running server: it reconciles
// the database, then swaps what the handlers and the pipeline work from. It
// does so at start and again on every reload.
type provisioner struct {
	logger *slog.Logger
	pool   *pgxpool.Pool
	// path is where the configuration is read from; empty means none.
	path   string
	getenv func(string) string
	// slackAPIURL is a process setting, not part of an integration.
	slackAPIURL string

	ingest       *ingest.Handler
	interactions *slack.Interactions
	pipeline     *notify.Pipeline
	auth         *auth.Handler

	// mu makes applications take turns, so that the configuration that was
	// reconciled last is also the one that is live.
	mu sync.Mutex

	reloads    *prometheus.CounterVec
	successful prometheus.Gauge
	lastOK     prometheus.Gauge
	skipped    *prometheus.GaugeVec

	// teams are the teams of the active configuration.
	teams atomic.Pointer[[]config.Team]
}

// ReloadPath is the route that reloads the configuration.
const ReloadPath = "/api/v1/config/reload"

// routes lists every route that beacon serve adds to the server, with who
// may use it. The server refuses anything that is not listed, and puts every
// route that is not public behind a session.
//
// The public routes are the explicit allow-list of what is reachable without
// a session. Each must either be harmless to a stranger or check a
// credential of its own. TestRoutes holds the same list; a route added here
// must be added there, which is a second look at its level.
func (p *provisioner) routes() []access.Route {
	routes := []access.Route{
		// The source's token is checked by the handler.
		{Pattern: "POST " + ingest.Path, Level: access.Public, Handler: p.ingest},
		// Slack's signature over the body is checked by the handler.
		{Pattern: "POST " + slack.InteractionsPath, Level: access.Public, Handler: p.interactions},
		{Pattern: "POST " + ReloadPath, Level: access.Admin, Handler: http.HandlerFunc(p.handleReload)},
	}
	return append(routes, p.auth.Routes()...)
}

// handleReload reloads the configuration for an admin. It does what SIGHUP
// does, and answers with the outcome.
func (p *provisioner) handleReload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	err := p.reload(r.Context(), "api")
	if err == nil {
		_ = json.NewEncoder(w).Encode(map[string]string{"result": "reloaded"})
		return
	}
	// The problems name files, objects and fields, never the value of a
	// secret, and the caller is an admin.
	body := struct {
		Error    string          `json:"error"`
		Problems []reloadProblem `json:"problems,omitempty"`
	}{Error: err.Error() + "; the previous configuration stays active"}
	var problems config.Errors
	status := http.StatusInternalServerError
	if errors.As(err, &problems) {
		status = http.StatusUnprocessableEntity
		for _, problem := range problems {
			body.Problems = append(body.Problems, reloadProblem{File: problem.File, Line: problem.Line, Object: problem.Object, Field: problem.Field, Problem: problem.Msg})
		}
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// reloadProblem is one problem of a rejected configuration, as the reload
// endpoint reports it.
type reloadProblem struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Object  string `json:"object,omitempty"`
	Field   string `json:"field,omitempty"`
	Problem string `json:"problem"`
}

// invalidConfig is a configuration that was rejected, with its problems.
type invalidConfig struct{ problems config.Errors }

func (e *invalidConfig) Error() string {
	return "the configuration is not valid: " + plural(len(e.problems), "problem")
}

// As lets errors.As find the problems.
func (e *invalidConfig) As(target any) bool {
	if t, ok := target.(*config.Errors); ok {
		*t = e.problems
		return true
	}
	return false
}

// Values of the result label of the reload counter.
const (
	resultSuccess = "success"
	resultFailure = "failure"
)

// registerMetrics creates the reload metrics on reg.
func (p *provisioner) registerMetrics(reg prometheus.Registerer) {
	p.reloads = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "beacon_config_reloads_total",
		Help: "Configuration reloads since the process started, by result. Loading the configuration at start is not counted.",
	}, []string{"result"})
	p.successful = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "beacon_config_last_reload_successful",
		Help: "Whether the last attempt to load the configuration succeeded: 1 or 0. When 0, the configuration loaded before is still active.",
	})
	p.lastOK = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "beacon_config_last_reload_success_timestamp_seconds",
		Help: "Unix time at which the active configuration was loaded.",
	})
	p.skipped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "beacon_team_members_skipped",
		Help: "Email addresses in a team's members list that belong to no user, by team, as of the last time this process applied the configuration or completed a sign-in. Such a person joins the team when they first sign in.",
	}, []string{"team"})
	reg.MustRegister(p.reloads, p.successful, p.lastOK, p.skipped)
	// Both results exist from the start, so a rate over them is never empty.
	p.reloads.WithLabelValues(resultSuccess)
	p.reloads.WithLabelValues(resultFailure)
}

// apply makes cfg the active configuration. The database is reconciled
// first; if that fails nothing is swapped, and the configuration that was
// active stays active.
func (p *provisioner) apply(ctx context.Context, cfg config.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	rows, err := provision.Reconcile(ctx, p.pool, cfg)
	if err != nil {
		return err
	}
	for _, c := range rows.Changes {
		p.logger.InfoContext(ctx, "provisioned object "+string(c.Action), slog.String("kind", string(c.Kind)), slog.String("name", c.Name))
	}
	p.logger.InfoContext(ctx, "configuration reconciled", slog.Int("changes", len(rows.Changes)),
		slog.Int("team_members_added", rows.MembersAdded), slog.Int("team_members_removed", rows.MembersRemoved))
	// A listed person who has never signed in is an error in the log, as
	// the requirements ask, but not a failure: everything else is applied.
	for _, s := range rows.Skipped {
		p.logger.ErrorContext(ctx, "team member skipped: no user has this email address; they join the team when they first sign in",
			slog.String("team", s.Team), slog.String("email", s.Email))
	}
	p.teams.Store(&cfg.Teams)
	p.countSkipped(cfg.Teams, rows.Skipped)

	p.applySources(ctx, cfg, rows.IDs[provision.Sources])
	p.applyNotifications(ctx, cfg, rows.IDs[provision.Integrations])
	p.auth.Configure(ctx, cfg.Auth, cfg.Teams)
	p.successful.Set(1)
	p.lastOK.SetToCurrentTime()
	return nil
}

// countSkipped sets the metric of skipped team members.
func (p *provisioner) countSkipped(teams []config.Team, skipped []provision.SkippedMember) {
	// Reset first, so that a team that left the configuration leaves the
	// metric too.
	p.skipped.Reset()
	for _, t := range teams {
		p.skipped.WithLabelValues(t.Name).Set(0)
	}
	for _, s := range skipped {
		p.skipped.WithLabelValues(s.Team).Inc()
	}
}

// refreshSkipped counts the skipped team members again. It runs after each
// sign-in, which is when a skipped person may have become a user.
func (p *provisioner) refreshSkipped(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded := p.teams.Load()
	if loaded == nil {
		return
	}
	teams := *loaded
	skipped, err := provision.SkippedMembers(ctx, p.pool, teams)
	if err != nil {
		p.logger.ErrorContext(ctx, "count skipped team members", slog.Any("error", err))
		return
	}
	p.countSkipped(teams, skipped)
}

// reload reads the configuration again and applies it. A configuration that
// is not valid, or cannot be applied, is rejected as a whole and the one
// that was active stays active. The outcome is logged and counted.
func (p *provisioner) reload(ctx context.Context, trigger string) error {
	start := time.Now()
	err := p.reloadFrom(ctx)
	if err != nil {
		p.reloads.WithLabelValues(resultFailure).Inc()
		p.successful.Set(0)
		p.logger.ErrorContext(ctx, "configuration reload failed; the previous configuration stays active",
			slog.String("trigger", trigger), slog.String("path", p.path), slog.Any("error", err))
		return err
	}
	p.reloads.WithLabelValues(resultSuccess).Inc()
	p.logger.InfoContext(ctx, "configuration reloaded",
		slog.String("trigger", trigger), slog.String("path", p.path), slog.Duration("duration", time.Since(start)))
	return nil
}

func (p *provisioner) reloadFrom(ctx context.Context) error {
	if p.path == "" {
		return errors.New("there is no configuration to reload: beacon serve was started without --config")
	}
	cfg, err := config.Read(p.path, config.LoadOptions{Getenv: p.getenv})
	var problems config.Errors
	if errors.As(err, &problems) {
		// Each problem is its own record, so that none is lost in a long
		// message. They hold names and positions, never values of secrets.
		for _, problem := range problems {
			p.logger.ErrorContext(ctx, "configuration problem",
				slog.String("file", problem.File), slog.Int("line", problem.Line),
				slog.String("object", problem.Object), slog.String("field", problem.Field), slog.String("problem", problem.Msg))
		}
		return &invalidConfig{problems: problems}
	}
	if err != nil {
		return err
	}
	return p.apply(ctx, cfg)
}

// applySources makes the ingest endpoint serve the configured sources. ids
// maps each source to its row.
func (p *provisioner) applySources(ctx context.Context, cfg config.Config, ids map[string]string) {
	sources := make([]ingest.Source, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		sources = append(sources, ingest.Source{ID: ids[s.Name], Source: s})
		p.logger.InfoContext(ctx, "source configured",
			slog.String("source", s.Name), slog.String("type", s.Type),
			slog.String("path", strings.Replace(ingest.Path, "{source}", s.Name, 1)))
	}
	if len(sources) == 0 {
		p.logger.WarnContext(ctx, "no source is configured, so every request to the ingest endpoint is refused; add a source to the configuration")
	}
	grouping := cfg.Routing.Grouping()
	p.logger.InfoContext(ctx, "alerts are grouped", slog.Any("group_by", grouping.By),
		slog.String("group_wait", config.Duration(grouping.Wait).String()), slog.String("group_interval", config.Duration(grouping.Interval).String()))
	p.ingest.SetSources(sources)
	p.ingest.SetGrouping(grouping)
}

// applyNotifications gives the pipeline a channel for each configured
// integration, tells the interactions endpoint their signing secrets, and
// sets the targets that every alert notifies. rows maps each integration to
// its row.
func (p *provisioner) applyNotifications(ctx context.Context, cfg config.Config, rows map[string]string) {
	ids := make(map[string]string, len(cfg.Integrations))
	channels := make(map[string]notify.Channel, len(cfg.Integrations))
	signingSecrets := make(map[string]config.Secret, len(cfg.Integrations))
	for _, i := range cfg.Integrations {
		if i.Type != config.IntegrationTypeSlack {
			p.logger.WarnContext(ctx, "this type of integration does not deliver yet, so nothing is sent through it",
				slog.String("integration", i.Name), slog.String("type", i.Type))
			continue
		}
		ids[i.Name] = rows[i.Name]
		channels[rows[i.Name]] = slack.NewChannel(slack.NewClient(p.slackAPIURL, i.Slack.BotToken))
		signingSecrets[i.Name] = i.Slack.SigningSecret
		p.logger.InfoContext(ctx, "integration configured", slog.String("integration", i.Name), slog.String("type", i.Type),
			slog.String("interactions_path", strings.Replace(slack.InteractionsPath, "{name}", i.Name, 1)))
	}

	var targets []notify.Target
	for _, t := range cfg.EveryAlertTargets() {
		id, ok := ids[t.Integration]
		if !ok {
			continue // warned about above
		}
		targets = append(targets, notify.Target{IntegrationID: id, Destination: t.Channel})
		p.logger.InfoContext(ctx, "every alert notifies", slog.String("integration", t.Integration), slog.String("channel", t.Channel))
	}
	if len(targets) == 0 {
		p.logger.WarnContext(ctx, "alerts are stored but nobody is notified: the first step of the default escalation policy names no channel of a Slack integration")
	}
	p.pipeline.SetChannels(channels)
	p.interactions.SetSecrets(signingSecrets)
	p.pipeline.SetTargets(targets)
}
