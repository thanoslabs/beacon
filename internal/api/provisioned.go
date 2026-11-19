package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thanoslabs/beacon/internal/access"
	"github.com/thanoslabs/beacon/internal/alert"
	"github.com/thanoslabs/beacon/internal/config"
	"github.com/thanoslabs/beacon/internal/provision"
)

// The routes of what the configuration defines, and of users.
const (
	SourcesPath      = "/api/v1/sources"
	IntegrationsPath = "/api/v1/integrations"
	PoliciesPath     = "/api/v1/escalation-policies"
	RoutingPath      = "/api/v1/routing"
	TeamsPath        = "/api/v1/teams"
	UsersPath        = "/api/v1/users"
)

// Provisioned serves what the configuration files define, for reading. It
// answers from the configuration that is active in this process, so that
// what it shows is what Beacon acts on. Nothing here can be changed through
// the API: the files are the only way in.
//
// No credential is ever answered, and no reference to one either. The
// address of an outbound webhook is answered without its query and its user
// information, where a token may be.
type Provisioned struct {
	logger *slog.Logger
	db     *pgxpool.Pool
	// cfg is replaced as a whole when the configuration is reloaded.
	cfg atomic.Pointer[config.Config]
}

// NewProvisioned returns a handler that knows no configuration yet.
func NewProvisioned(logger *slog.Logger, db *pgxpool.Pool) *Provisioned {
	h := &Provisioned{logger: logger, db: db}
	h.SetConfig(config.Config{})
	return h
}

// SetConfig replaces the configuration that is answered. It is safe while
// requests are being handled.
func (h *Provisioned) SetConfig(cfg config.Config) { h.cfg.Store(&cfg) }

// Routes are the routes of the provisioned objects and of users. All are
// open to members and none changes anything: whoever responds to alerts
// needs to see where they come from, where they go and who their colleagues
// are.
func (h *Provisioned) Routes() []access.Route {
	get := func(path string, fn http.HandlerFunc) access.Route {
		return access.Route{Pattern: "GET " + path, Level: access.Member, Handler: fn}
	}
	return []access.Route{
		get(SourcesPath, h.sources), get(SourcesPath+"/{name}", h.sources),
		get(IntegrationsPath, h.integrations), get(IntegrationsPath+"/{name}", h.integrations),
		get(PoliciesPath, h.policies), get(PoliciesPath+"/{name}", h.policies),
		get(RoutingPath, h.routing),
		get(TeamsPath, h.teams), get(TeamsPath+"/{name}", h.teams),
		get(UsersPath, h.users),
	}
}

// answer writes a list under key, or, when the path names one object, that
// object, or 404.
func answer[T any](h *Provisioned, w http.ResponseWriter, r *http.Request, key, what string, items []T, name func(T) string) {
	want := r.PathValue("name")
	if want == "" {
		if items == nil {
			items = []T{}
		}
		Write(h.logger, w, r, http.StatusOK, map[string][]T{key: items})
		return
	}
	for _, item := range items {
		if name(item) == want {
			Write(h.logger, w, r, http.StatusOK, item)
			return
		}
	}
	Fail(h.logger, w, r, http.StatusNotFound, "", "no such "+what)
}

// sourceBody is a source, as the API answers. Its token is not part of it.
type sourceBody struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// IngestPath is where the source's sender posts alerts.
	IngestPath string            `json:"ingest_path"`
	Labels     map[string]string `json:"labels"`
	Severity   severityBody      `json:"severity"`
	// Provisioned says that the object is managed in the configuration
	// files and cannot be changed here.
	Provisioned bool `json:"provisioned"`
}

type severityBody struct {
	Label   string                    `json:"label"`
	Mapping map[string]alert.Severity `json:"mapping"`
	Default alert.Severity            `json:"default"`
}

func (h *Provisioned) sources(w http.ResponseWriter, r *http.Request) {
	var out []sourceBody
	for _, s := range h.cfg.Load().Sources {
		body := sourceBody{Name: s.Name, Type: s.Type, IngestPath: "/api/v1/ingest/" + s.Name, Labels: nonNil(s.Labels), Provisioned: true,
			Severity: severityBody{Label: s.Severity.Label, Mapping: s.Severity.Mapping, Default: s.Severity.Default}}
		// The defaults, said out loud.
		if body.Severity.Label == "" {
			body.Severity.Label = "severity"
		}
		if body.Severity.Mapping == nil {
			body.Severity.Mapping = map[string]alert.Severity{}
		}
		if body.Severity.Default == "" {
			body.Severity.Default = alert.SeverityWarning
		}
		out = append(out, body)
	}
	answer(h, w, r, "sources", "source", out, func(s sourceBody) string { return s.Name })
}

// integrationBody is an integration, as the API answers: what it is and
// where it sends, without any credential.
type integrationBody struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// InteractionsPath is where the chat service sends button presses, for
	// an integration that has buttons.
	InteractionsPath *string `json:"interactions_path"`
	// Settings holds what is not a credential, by the type: the application
	// of a Discord integration, the server and sender of an email one, the
	// address and header names of a webhook.
	Settings    map[string]any `json:"settings"`
	Provisioned bool           `json:"provisioned"`
}

// withoutSecrets returns the address of a webhook without the parts that
// may hold a credential: the user information and the query.
func withoutSecrets(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

func (h *Provisioned) integrations(w http.ResponseWriter, r *http.Request) {
	var out []integrationBody
	for _, i := range h.cfg.Load().Integrations {
		body := integrationBody{Name: i.Name, Type: i.Type, Settings: map[string]any{}, Provisioned: true}
		interactions := "/api/v1/integrations/" + i.Name + "/interactions"
		switch {
		case i.Slack != nil:
			body.InteractionsPath = &interactions
		case i.Discord != nil:
			body.InteractionsPath = &interactions
			body.Settings["application_id"], body.Settings["public_key"] = i.Discord.ApplicationID, i.Discord.PublicKey
		case i.Email != nil:
			body.Settings["host"], body.Settings["port"], body.Settings["tls"], body.Settings["from"] = i.Email.Host, i.Email.Port, i.Email.TLS, i.Email.From
			body.Settings["authenticated"] = i.Email.Username != ""
		case i.Webhook != nil:
			headers := make([]string, 0, len(i.Webhook.Headers))
			for name := range i.Webhook.Headers {
				headers = append(headers, name)
			}
			sort.Strings(headers)
			body.Settings["url"], body.Settings["headers"] = withoutSecrets(i.Webhook.URL), headers
		}
		out = append(out, body)
	}
	answer(h, w, r, "integrations", "integration", out, func(i integrationBody) string { return i.Name })
}

// policyBody is an escalation policy, as the API answers.
type policyBody struct {
	Name string `json:"name"`
	// Repeat is how many more times all steps run after the first pass.
	Repeat      int        `json:"repeat"`
	Steps       []stepBody `json:"steps"`
	Provisioned bool       `json:"provisioned"`
}

type stepBody struct {
	// Timeout is how long the step waits for a response, as a duration.
	Timeout string       `json:"timeout"`
	Notify  []targetBody `json:"notify"`
}

// targetBody is who a step notifies. Kind says which of the names is set.
type targetBody struct {
	// Kind is schedule, team, user, channel or webhook.
	Kind        string   `json:"kind"`
	Schedule    *string  `json:"schedule"`
	Team        *string  `json:"team"`
	User        *string  `json:"user"`
	Channel     *string  `json:"channel"`
	Integration *string  `json:"integration"`
	Via         []string `json:"via"`
}

func (h *Provisioned) policies(w http.ResponseWriter, r *http.Request) {
	var out []policyBody
	for _, p := range h.cfg.Load().EscalationPolicies {
		body := policyBody{Name: p.Name, Repeat: p.Repeat, Steps: []stepBody{}, Provisioned: true}
		for _, s := range p.Steps {
			step := stepBody{Timeout: s.Timeout.String(), Notify: []targetBody{}}
			for _, t := range s.Notify {
				target := targetBody{Schedule: optional(t.Schedule), Team: optional(t.Team), User: optional(t.User),
					Channel: optional(t.Channel), Integration: optional(t.Integration), Via: append([]string{}, t.Via...)}
				switch {
				case t.Schedule != "":
					target.Kind = "schedule"
				case t.Team != "":
					target.Kind = "team"
				case t.User != "":
					target.Kind = "user"
				case t.Channel != "":
					target.Kind = "channel"
				default:
					target.Kind = "webhook"
				}
				step.Notify = append(step.Notify, target)
			}
			body.Steps = append(body.Steps, step)
		}
		out = append(out, body)
	}
	answer(h, w, r, "escalation_policies", "escalation policy", out, func(p policyBody) string { return p.Name })
}

// routingBody is the routing block, as the API answers, with the grouping
// that applies when a rule says nothing else.
type routingBody struct {
	GroupBy       []string   `json:"group_by"`
	GroupWait     string     `json:"group_wait"`
	GroupInterval string     `json:"group_interval"`
	Default       routeBody  `json:"default"`
	Rules         []ruleBody `json:"rules"`
	Provisioned   bool       `json:"provisioned"`
}

type routeBody struct {
	Team             *string `json:"team"`
	EscalationPolicy string  `json:"escalation_policy"`
}

// ruleBody is a routing rule. Rules are tried in the order they are
// answered; the first match wins.
type ruleBody struct {
	Name             string            `json:"name"`
	Match            map[string]string `json:"match"`
	MatchRE          map[string]string `json:"match_re"`
	Team             *string           `json:"team"`
	Service          *string           `json:"service"`
	EscalationPolicy string            `json:"escalation_policy"`
	// The grouping of the alerts the rule matches, with the routing block's
	// where the rule sets none.
	GroupBy       []string `json:"group_by"`
	GroupWait     string   `json:"group_wait"`
	GroupInterval string   `json:"group_interval"`
}

func (h *Provisioned) routing(w http.ResponseWriter, r *http.Request) {
	routing := h.cfg.Load().Routing
	if routing == nil {
		Fail(h.logger, w, r, http.StatusNotFound, "", "the configuration has no routing block, so alerts are stored and nobody is notified")
		return
	}
	g := routing.Grouping()
	body := routingBody{
		GroupBy: append([]string{}, g.By...), GroupWait: config.Duration(g.Wait).String(), GroupInterval: config.Duration(g.Interval).String(),
		Default: routeBody{Team: optional(routing.Default.Team), EscalationPolicy: routing.Default.EscalationPolicy},
		Rules:   []ruleBody{}, Provisioned: true,
	}
	for _, rule := range routing.Rules {
		g := routing.GroupingFor(rule)
		body.Rules = append(body.Rules, ruleBody{
			Name: rule.Name, Match: nonNil(rule.Match), MatchRE: nonNil(rule.MatchRE), Team: optional(rule.Team), Service: optional(rule.Service),
			EscalationPolicy: rule.EscalationPolicy,
			GroupBy:          append([]string{}, g.By...), GroupWait: config.Duration(g.Wait).String(), GroupInterval: config.Duration(g.Interval).String(),
		})
	}
	Write(h.logger, w, r, http.StatusOK, body)
}

// teamBody is a team, as the API answers: what the files say, and who its
// members are at this moment.
type teamBody struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	// Groups are the identity provider groups whose members belong.
	Groups []string `json:"groups"`
	// Members are the users who belong, by either way.
	Members []memberBody `json:"members"`
	// Schedules are the names of the schedules the team owns.
	Schedules []string `json:"schedules"`
	// SkippedMembers are the addresses in the files that no user has. They
	// are answered to admins only, and left out otherwise.
	SkippedMembers *[]string `json:"skipped_members,omitempty"`
	Provisioned    bool      `json:"provisioned"`
}

type memberBody struct {
	Email       string  `json:"email"`
	DisplayName *string `json:"display_name"`
	// Origins says why the person is a member: config for the members list
	// of the files, group for an identity provider group.
	Origins []string `json:"origins"`
}

func (h *Provisioned) teams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := h.cfg.Load()
	p, _ := access.FromContext(ctx)
	members := map[string][]memberBody{}
	rows, err := h.db.Query(ctx, `
		SELECT t.name, u.email, u.display_name, array_agg(m.origin ORDER BY m.origin)
		FROM team_members m JOIN teams t ON t.id = m.team_id AND t.removed_at IS NULL JOIN users u ON u.id = m.user_id
		WHERE u.disabled_at IS NULL GROUP BY t.name, u.email, u.display_name ORDER BY t.name, u.email`)
	if err == nil {
		var team string
		var m memberBody
		var name string
		_, err = pgx.ForEachRow(rows, []any{&team, &m.Email, &name, &m.Origins}, func() error {
			m.DisplayName = optional(name)
			members[team] = append(members[team], m)
			return nil
		})
	}
	if err != nil {
		Internal(h.logger, w, r, "read team members", err)
		return
	}
	skipped := map[string][]string{}
	if p.Role == access.RoleAdmin {
		found, err := provision.SkippedMembers(ctx, h.db, cfg.Teams)
		if err != nil {
			Internal(h.logger, w, r, "read skipped team members", err)
			return
		}
		for _, s := range found {
			skipped[s.Team] = append(skipped[s.Team], s.Email)
		}
	}
	var out []teamBody
	for _, t := range cfg.Teams {
		body := teamBody{Name: t.Name, DisplayName: t.DisplayName, Groups: append([]string{}, t.Groups...), Members: members[t.Name], Schedules: []string{}, Provisioned: true}
		if body.DisplayName == "" {
			body.DisplayName = t.Name
		}
		if body.Members == nil {
			body.Members = []memberBody{}
		}
		for _, s := range cfg.Schedules {
			if s.Team == t.Name {
				body.Schedules = append(body.Schedules, s.Name)
			}
		}
		if p.Role == access.RoleAdmin {
			list := append([]string{}, skipped[t.Name]...)
			body.SkippedMembers = &list
		}
		out = append(out, body)
	}
	answer(h, w, r, "teams", "team", out, func(t teamBody) string { return t.Name })
}

// userBody is a user, as the API answers.
type userBody struct {
	ID          string      `json:"id"`
	Email       string      `json:"email"`
	DisplayName *string     `json:"display_name"`
	Role        access.Role `json:"role"`
	// Disabled says that the account is turned off.
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSignInAt *time.Time `json:"last_sign_in_at"`
	// Teams are the names of the teams the user belongs to.
	Teams []string `json:"teams"`
}

// userCursor is where a list of users continues: after this address.
type userCursor struct {
	Email string `json:"email"`
}

func (h *Provisioned) users(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for name := range q {
		if !slices.Contains([]string{"team", "role", "limit", "cursor"}, name) {
			Fail(h.logger, w, r, http.StatusBadRequest, name, "is not a filter of this list; the filters are team and role")
			return
		}
	}
	role := q.Get("role")
	if role != "" && !access.Role(role).Valid() {
		Fail(h.logger, w, r, http.StatusBadRequest, "role", "must be admin or member")
		return
	}
	limit, problem, ok := Limit(q)
	if !ok {
		Fail(h.logger, w, r, http.StatusBadRequest, "limit", problem)
		return
	}
	var c userCursor
	if _, err := ParseCursor(q.Get("cursor"), &c); err != nil {
		Fail(h.logger, w, r, http.StatusBadRequest, "cursor", ErrCursor.Error())
		return
	}
	// Addresses are unique, so the order is total.
	rows, err := h.db.Query(r.Context(), `
		SELECT u.id::text, u.email, u.display_name, u.role, u.disabled_at IS NOT NULL, u.created_at, u.last_sign_in_at,
			COALESCE((SELECT array_agg(DISTINCT t.name ORDER BY t.name) FROM team_members m JOIN teams t ON t.id = m.team_id AND t.removed_at IS NULL
				WHERE m.user_id = u.id), '{}')
		FROM users u
		WHERE u.email > $1 AND ($2 = '' OR u.role = $2)
			AND ($3 = '' OR EXISTS (SELECT 1 FROM team_members m JOIN teams t ON t.id = m.team_id AND t.removed_at IS NULL WHERE m.user_id = u.id AND t.name = $3))
		ORDER BY u.email LIMIT $4`, c.Email, role, q.Get("team"), limit+1)
	var users []userBody
	if err == nil {
		users, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (userBody, error) {
			var u userBody
			var name string
			err := row.Scan(&u.ID, &u.Email, &name, &u.Role, &u.Disabled, &u.CreatedAt, &u.LastSignInAt, &u.Teams)
			u.DisplayName, u.CreatedAt, u.LastSignInAt = optional(name), u.CreatedAt.UTC(), utc(u.LastSignInAt)
			return u, err
		})
	}
	if err != nil {
		Internal(h.logger, w, r, "list users", fmt.Errorf("list users: %w", err))
		return
	}
	body := struct {
		Users      []userBody `json:"users"`
		NextCursor string     `json:"next_cursor,omitempty"`
	}{Users: users}
	if len(users) > limit {
		body.Users = users[:limit]
		body.NextCursor = Cursor(userCursor{Email: users[limit-1].Email})
	}
	if body.Users == nil {
		body.Users = []userBody{}
	}
	Write(h.logger, w, r, http.StatusOK, body)
}
