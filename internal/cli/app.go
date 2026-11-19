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
	"github.com/thanoslabs/beacon/internal/identity"
	"github.com/thanoslabs/beacon/internal/ingest"
	"github.com/thanoslabs/beacon/internal/notify"
	"github.com/thanoslabs/beacon/internal/provision"
	"github.com/thanoslabs/beacon/internal/routing"
	"github.com/thanoslabs/beacon/internal/schedule"
	"github.com/thanoslabs/beacon/internal/silence"
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
	silences     *silence.Handler
	schedules    *schedule.Store
	scheduleAPI  *schedule.Handler
	identities   *identity.Store
	identityAPI  *identity.Handler
	linker       *identity.Linker

	// mu makes applications take turns, so that the configuration that was
	// reconciled last is also the one that is live.
	mu sync.Mutex

	reloads    *prometheus.CounterVec
	successful prometheus.Gauge
	lastOK     prometheus.Gauge
	skipped    *prometheus.GaugeVec
	// rotationSkipped counts the rotation members that are not users.
	rotationSkipped *prometheus.GaugeVec

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
	routes = append(routes, p.auth.Routes()...)
	// Silences are runtime state that members create and end.
	routes = append(routes, p.silences.Routes()...)
	// So are the overrides of schedules.
	routes = append(routes, p.scheduleAPI.Routes()...)
	// A person sees their own chat accounts.
	return append(routes, p.identityAPI.Routes()...)
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
	p.rotationSkipped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "beacon_schedule_members_skipped",
		Help: "Email addresses in a schedule's rotation that belong to no user, by schedule, as of the last time this process applied the configuration. Such a member is passed over until the person has signed in.",
	}, []string{"schedule"})
	reg.MustRegister(p.reloads, p.successful, p.lastOK, p.skipped, p.rotationSkipped)
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
	// The router is built before anything is swapped, like the rows: if it
	// cannot be, the configuration that was active stays active.
	router, err := routing.New(cfg.Routing, routing.Refs{
		Rules: rows.IDs[provision.RoutingRules], Teams: rows.IDs[provision.Teams], Policies: rows.IDs[provision.EscalationPolicies],
	})
	if err != nil {
		return err
	}
	// Positions are runtime state that follows the rows: a new schedule
	// starts its rotation here, and one that left the files ends.
	rotations, err := p.applySchedules(ctx, cfg, rows.IDs[provision.Schedules])
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

	p.reportSchedules(ctx, cfg, rotations)
	p.applySources(ctx, cfg, rows.IDs[provision.Sources])
	p.applyRouting(ctx, cfg, router)
	p.applyNotifications(ctx, cfg, rows.IDs)
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

// applySchedules gives every schedule of the configuration a position in its
// rotation. ids maps each schedule to its row.
func (p *provisioner) applySchedules(ctx context.Context, cfg config.Config, ids map[string]string) (schedule.Applied, error) {
	provisioned := make([]schedule.Provisioned, 0, len(cfg.Schedules))
	for _, s := range cfg.Schedules {
		provisioned = append(provisioned, schedule.Provisioned{ID: ids[s.Name], Schedule: s})
	}
	return p.schedules.Apply(ctx, provisioned)
}

// reportSchedules logs what applying the schedules did, and counts the
// rotation members that were passed over.
func (p *provisioner) reportSchedules(ctx context.Context, cfg config.Config, applied schedule.Applied) {
	for _, name := range applied.Started {
		p.logger.InfoContext(ctx, "schedule started its rotation", slog.String("schedule", name))
	}
	for _, name := range applied.Ended {
		p.logger.InfoContext(ctx, "schedule ended: it left the configuration; its recorded shifts are kept", slog.String("schedule", name))
	}
	// A changed definition never moves the shift in progress. It is said
	// once when it is stored, with when it takes over.
	for _, c := range applied.Changed {
		if !c.New {
			continue
		}
		when := "at the next handoff"
		if c.Stated {
			when = "at the time that effective_from states"
		}
		p.logger.InfoContext(ctx, "schedule definition changed: the rotation in force runs until the change takes over, "+when,
			slog.String("schedule", c.Schedule), slog.Time("takes_over_at", c.At))
	}
	// Nobody on call is a state some teams choose, so it is a warning.
	for _, name := range applied.Uncovered {
		p.logger.WarnContext(ctx, schedule.GapInCoverage, slog.String("schedule", name))
	}
	// Reset first, so that a schedule that left the configuration leaves
	// the metric too.
	p.rotationSkipped.Reset()
	for _, s := range cfg.Schedules {
		p.rotationSkipped.WithLabelValues(s.Name).Set(0)
	}
	// As for a team: an error in the log, as the requirements ask, and not
	// a failure.
	for _, s := range applied.Skipped {
		p.rotationSkipped.WithLabelValues(s.Schedule).Inc()
		p.logger.ErrorContext(ctx, "rotation member skipped: no user has this email address; they take their place in the rotation once they have signed in",
			slog.String("schedule", s.Schedule), slog.String("email", s.Email))
	}
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
	p.ingest.SetSources(sources)
}

// applyRouting makes new alerts follow the routing block.
func (p *provisioner) applyRouting(ctx context.Context, cfg config.Config, router *routing.Router) {
	grouping := cfg.Routing.Grouping()
	rules := 0
	if cfg.Routing != nil {
		rules = len(cfg.Routing.Rules)
	}
	p.logger.InfoContext(ctx, "alerts are routed and grouped", slog.Int("rules", rules), slog.Any("group_by", grouping.By),
		slog.String("group_wait", config.Duration(grouping.Wait).String()), slog.String("group_interval", config.Duration(grouping.Interval).String()))
	p.ingest.SetRouter(router)
}

// applyNotifications gives the pipeline a channel for each configured
// integration and the escalation policies, with their targets resolved to
// rows, and tells the interactions endpoint the signing secrets. rows maps
// each object to its row, by kind.
func (p *provisioner) applyNotifications(ctx context.Context, cfg config.Config, rows map[provision.Kind]map[string]string) {
	integrations, policies := rows[provision.Integrations], rows[provision.EscalationPolicies]
	schedules, teams := rows[provision.Schedules], rows[provision.Teams]
	ids := make(map[string]string, len(cfg.Integrations))
	channels := make(map[string]notify.Channel, len(cfg.Integrations))
	signingSecrets := make(map[string]config.Secret, len(cfg.Integrations))
	// The chat services in which people have accounts. Slack can be asked
	// for an account by email address; in Discord people link theirs.
	var chat []identity.Integration
	for _, i := range cfg.Integrations {
		if i.Type == config.IntegrationTypeDiscord {
			chat = append(chat, identity.Integration{ID: integrations[i.Name], Name: i.Name, Provider: identity.ProviderDiscord})
		}
		if i.Type != config.IntegrationTypeSlack {
			p.logger.WarnContext(ctx, "this type of integration does not deliver yet, so nothing is sent through it",
				slog.String("integration", i.Name), slog.String("type", i.Type))
			continue
		}
		ids[i.Name] = integrations[i.Name]
		client := slack.NewClient(p.slackAPIURL, i.Slack.BotToken)
		channels[integrations[i.Name]] = slack.NewChannel(client)
		signingSecrets[i.Name] = i.Slack.SigningSecret
		chat = append(chat, identity.Integration{ID: integrations[i.Name], Name: i.Name, Provider: identity.ProviderSlack, Lookup: client})
		p.logger.InfoContext(ctx, "integration configured", slog.String("integration", i.Name), slog.String("type", i.Type),
			slog.String("interactions_path", strings.Replace(slack.InteractionsPath, "{name}", i.Name, 1)))
	}

	// A person is reached through every integration that can reach one
	// directly, unless the target narrows that with via. Today that is a
	// direct message in Slack.
	var direct []notify.Via
	for _, i := range cfg.Integrations {
		if _, delivers := ids[i.Name]; delivers && i.Type == config.IntegrationTypeSlack {
			direct = append(direct, notify.Via{IntegrationID: ids[i.Name], Name: i.Name, Provider: i.Type})
		}
	}
	// via returns the integration with a name as a step uses it. One that
	// does not deliver yet is kept: the step then records that it skipped
	// it, which is better than leaving it out without a trace.
	types := map[string]string{}
	for _, i := range cfg.Integrations {
		types[i.Name] = i.Type
	}
	via := func(name string) notify.Via {
		return notify.Via{IntegrationID: integrations[name], Name: name, Provider: types[name]}
	}

	byID := make(map[string]notify.Policy, len(cfg.EscalationPolicies))
	notifies := map[string]bool{}
	for _, policy := range cfg.EscalationPolicies {
		out := notify.Policy{Name: policy.Name, Repeat: policy.Repeat}
		for _, step := range policy.Steps {
			next := notify.Step{Timeout: time.Duration(step.Timeout)}
			for _, t := range step.Notify {
				var target notify.StepTarget
				switch {
				case t.Channel != "":
					target = notify.ChannelTarget(integrations[t.Integration], t.Integration, t.Channel)
				case t.Schedule != "":
					target = notify.StepTarget{Kind: notify.TargetSchedule, Name: t.Schedule, ID: schedules[t.Schedule], Via: direct}
				case t.Team != "":
					target = notify.StepTarget{Kind: notify.TargetTeam, Name: t.Team, ID: teams[t.Team], Via: direct}
				default:
					target = notify.StepTarget{Kind: notify.TargetUser, Name: config.NormalizeEmail(t.User), Via: direct}
				}
				if len(t.Via) > 0 {
					target.Via = nil
					for _, name := range t.Via {
						target.Via = append(target.Via, via(name))
					}
				}
				for _, v := range target.Via {
					if _, delivers := channels[v.IntegrationID]; delivers {
						notifies[policy.Name] = true
					}
				}
				next.Targets = append(next.Targets, target)
			}
			out.Steps = append(out.Steps, next)
		}
		byID[policies[policy.Name]] = out
		p.logger.InfoContext(ctx, "escalation policy configured", slog.String("policy", policy.Name), slog.Int("steps", len(out.Steps)), slog.Int("repeat", out.Repeat))
		if !notifies[policy.Name] {
			p.logger.WarnContext(ctx, "this escalation policy can notify nobody: none of its targets is reached through an integration that delivers; today that is a Slack integration, for channels and for people",
				slog.String("policy", policy.Name))
		}
	}
	if cfg.Routing == nil {
		p.logger.WarnContext(ctx, "alerts are stored but nobody is notified: the configuration has no routing block, so alerts have no escalation policy")
	}
	p.pipeline.SetChannels(channels)
	p.pipeline.SetPolicies(byID)
	p.interactions.SetSecrets(signingSecrets)
	p.linker.SetIntegrations(chat)
	// The new integrations are asked about their users at once.
	p.linker.Kick()
}

// UserFor tells the Slack handler which Beacon user an account of the
// integration with this name belongs to.
func (p *provisioner) UserFor(ctx context.Context, integration, externalID string) (userID, name string, found bool, err error) {
	in, ok := p.linker.Integrations()[integration]
	if !ok {
		return "", "", false, nil
	}
	u, found, err := p.identities.UserFor(ctx, p.pool, in.ID, externalID)
	return u.ID, u.Name, found, err
}
