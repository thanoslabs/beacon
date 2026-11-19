package config

import (
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"

	// Time zones are checked against the embedded database, so that the
	// answer does not depend on the machine that runs the check.
	_ "time/tzdata"
)

// minTokenLength is the shortest source token accepted. A token is the only
// thing that stops anyone who can reach Beacon from sending alerts.
const minTokenLength = 16

// maxRepeat bounds how often an escalation policy runs again.
const maxRepeat = 100

// maxServiceLength bounds the name of a service.
const maxServiceLength = 100

const nameRule = "must be lowercase letters, digits and hyphens, start with a letter and be at most 63 characters"

var (
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	handoffRE    = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	headerNameRE = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_|~-]+$`)
	publicKeyRE  = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	snowflakeRE  = regexp.MustCompile(`^[0-9]{1,20}$`)
	// discordChannelRE matches the ID of a Discord channel.
	discordChannelRE = regexp.MustCompile(`^[0-9]{15,22}$`)
	weekdays         = map[string]bool{"monday": true, "tuesday": true, "wednesday": true, "thursday": true, "friday": true, "saturday": true, "sunday": true}
)

// Read loads the configuration at path and validates it. The error is an
// Errors holding every problem found, in the files' structure and in what
// they say.
func Read(path string, opts LoadOptions) (Config, error) {
	cfg, err := Load(path, opts)
	structural, _ := err.(Errors) //nolint:errorlint // Load returns Errors itself, never wrapped
	if err != nil && structural == nil {
		return cfg, err
	}

	v := validator{}
	taken := map[string]bool{}
	for _, e := range structural {
		// A file that could not be parsed contributes no objects, so every
		// reference to one of them would be reported as missing.
		if e.Object == "" {
			v.skipRefs = true
		}
		taken[e.File+"\x00"+e.Object+"\x00"+e.Field] = true
	}
	cfg.validate(&v)
	all := structural
	for _, e := range v.errs {
		// A field that could not be decoded is reported once, not again as
		// missing.
		if !taken[e.File+"\x00"+e.Object+"\x00"+e.Field] {
			all = append(all, e)
		}
	}
	return cfg, all.err()
}

// Validate checks what the configuration says: names, required fields,
// allowed values and the references between objects. The error is an Errors
// holding every problem; each names the object and field at fault, and none
// contains a secret.
func (c Config) Validate() error {
	var v validator
	c.validate(&v)
	return v.errs.err()
}

// validator collects the problems found.
type validator struct {
	errs Errors
	// skipRefs turns off the checks that an object referred to exists.
	skipRefs bool
}

// object is the object being checked: where it is and what to call it.
type object struct {
	v      *validator
	origin Origin
	label  string
}

func (v *validator) object(o Origin, kind, name string) object {
	return object{v: v, origin: o, label: label(kind, name)}
}

// errorf reports a problem with a field of the object.
func (o object) errorf(field, format string, args ...any) {
	o.v.errs = append(o.v.errs, Error{File: o.origin.File, Line: o.origin.lineOf(field), Object: o.label, Field: field, Msg: fmt.Sprintf(format, args...)})
}

// name checks an object's name and that no other object of its kind has it.
func (o object) name(name string, seen map[string]Origin) {
	first, dup := seen[name]
	switch {
	case !ValidName(name):
		o.errorf("name", "%s", nameRule)
	case dup && first.File != "":
		o.errorf("name", "the name is used twice; first at %s:%d", first.File, first.Line)
	case dup:
		o.errorf("name", "the name is used twice")
	default:
		seen[name] = o.origin
	}
}

// ref checks that a named object exists.
func (o object) ref(field, kind, name string, known map[string]Origin) {
	if _, ok := known[name]; !ok && !o.v.skipRefs {
		o.errorf(field, "%s %q does not exist", kind, name)
	}
}

// emails checks a list of user email addresses.
func (o object) emails(field string, list []string) {
	seen := map[string]bool{}
	for i, e := range list {
		at := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case !validEmail(e):
			o.errorf(at, "%q is not an email address", e)
		case seen[NormalizeEmail(e)]:
			o.errorf(at, "%q is listed twice", e)
		}
		seen[NormalizeEmail(e)] = true
	}
}

// groups checks a list of identity provider group names.
func (o object) groups(field string, list []string) {
	seen := map[string]bool{}
	for i, g := range list {
		at := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case strings.TrimSpace(g) == "":
			o.errorf(at, "must not be empty")
		case seen[g]:
			o.errorf(at, "%q is listed twice", g)
		}
		seen[g] = true
	}
}

// labelNames checks a list of label names.
func (o object) labelNames(field string, names []string) {
	seen := map[string]bool{}
	for i, n := range names {
		at := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case !labelNameRE.MatchString(n):
			o.errorf(at, "%q is not a label name", n)
		case seen[n]:
			o.errorf(at, "%q is listed twice", n)
		}
		seen[n] = true
	}
}

// positive checks an optional duration.
func (o object) positive(field string, d *Duration) {
	if d != nil && *d <= 0 {
		o.errorf(field, "must be longer than zero")
	}
}

// ValidLabelName reports whether name can be the name of a label: letters,
// digits and underscores, not starting with a digit, as in Prometheus.
func ValidLabelName(name string) bool {
	return labelNameRE.MatchString(name)
}

// NormalizeEmail returns the form in which email addresses are compared and
// stored: lowercase, without surrounding space.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validEmail reports whether s is a bare address such as ada@example.com.
func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Name == "" && a.Address == s && !strings.ContainsAny(s, " <>")
}

func (c Config) validate(v *validator) {
	sources := map[string]Origin{}
	for _, s := range c.Sources {
		o := v.object(s.Origin, kindSource, s.Name)
		o.name(s.Name, sources)
		switch s.Type {
		case SourceTypeAlertmanager, SourceTypeGrafana, SourceTypeWebhook:
		case "":
			o.errorf("type", "is required: alertmanager, grafana or webhook")
		default:
			o.errorf("type", "%q is not alertmanager, grafana or webhook", s.Type)
		}
		switch {
		case !s.Token.IsReferenced() && !s.Token.IsSet():
			o.errorf("token", "is required")
		case s.Token.IsSet() && len(s.Token.Reveal()) < minTokenLength:
			o.errorf("token", "must be at least %d characters", minTokenLength)
		}
		for name := range s.Labels {
			if !labelNameRE.MatchString(name) {
				o.errorf("labels", "%q is not a label name", name)
			}
		}
		if l := s.Severity.Label; l != "" && !labelNameRE.MatchString(l) {
			o.errorf("severity.label", "%q is not a label name", l)
		}
		for value, level := range s.Severity.Mapping {
			if !level.Valid() {
				o.errorf("severity.mapping."+value, "%q is not critical, warning or info", level)
			}
		}
		if d := s.Severity.Default; d != "" && !d.Valid() {
			o.errorf("severity.default", "%q is not critical, warning or info", d)
		}
	}

	integrations := map[string]Origin{}
	// types maps each integration to its type, which decides what the
	// channel of a target must look like.
	types := map[string]string{}
	for _, in := range c.Integrations {
		o := v.object(in.Origin, kindIntegration, in.Name)
		o.name(in.Name, integrations)
		in.validate(o)
		types[in.Name] = in.Type
	}

	teams := map[string]Origin{}
	for _, t := range c.Teams {
		o := v.object(t.Origin, kindTeam, t.Name)
		o.name(t.Name, teams)
		o.emails("members", t.Members)
		o.groups("groups", t.Groups)
	}

	schedules := map[string]Origin{}
	for _, s := range c.Schedules {
		o := v.object(s.Origin, kindSchedule, s.Name)
		o.name(s.Name, schedules)
		if s.Team == "" {
			o.errorf("team", "is required")
		} else {
			o.ref("team", kindTeam, s.Team, teams)
		}
		if s.TimeZone == "" {
			o.errorf("time_zone", "is required: an IANA name such as Europe/London")
		} else if _, err := time.LoadLocation(s.TimeZone); err != nil || s.TimeZone == "Local" {
			o.errorf("time_zone", "%q is not an IANA time zone such as Europe/London", s.TimeZone)
		}
		s.Rotation.validate(o)
	}

	policies := map[string]Origin{}
	for _, p := range c.EscalationPolicies {
		o := v.object(p.Origin, kindPolicy, p.Name)
		o.name(p.Name, policies)
		if p.Repeat < 0 || p.Repeat > maxRepeat {
			o.errorf("repeat", "must be between 0 and %d", maxRepeat)
		}
		if len(p.Steps) == 0 {
			o.errorf("steps", "at least one step is required")
		}
		for i, step := range p.Steps {
			at := fmt.Sprintf("steps[%d]", i)
			if step.Timeout <= 0 {
				o.errorf(at+".timeout", "is required, and must be longer than zero")
			}
			if len(step.Notify) == 0 {
				o.errorf(at+".notify", "at least one target is required")
			}
			for j, t := range step.Notify {
				t.validate(o, fmt.Sprintf("%s.notify[%d]", at, j), schedules, teams, integrations, types)
			}
		}
	}

	if a := c.Auth; a != nil {
		a.validate(object{v: v, origin: a.Origin, label: kindAuth})
	}

	if r := c.Routing; r != nil {
		o := object{v: v, origin: r.Origin, label: kindRouting}
		o.labelNames("group_by", r.GroupBy)
		// group_wait may be zero: the first notification is then sent at
		// once.
		o.positive("group_interval", r.GroupInterval)
		if r.Default.Team != "" {
			o.ref("default.team", kindTeam, r.Default.Team, teams)
		}
		if r.Default.EscalationPolicy == "" {
			o.errorf("default.escalation_policy", "is required")
		} else {
			o.ref("default.escalation_policy", kindPolicy, r.Default.EscalationPolicy, policies)
		}
		rules := map[string]Origin{}
		for _, rule := range r.Rules {
			o := v.object(rule.Origin, kindRule, rule.Name)
			o.name(rule.Name, rules)
			if len(rule.Match) == 0 && len(rule.MatchRE) == 0 {
				o.errorf("match", "a rule needs match, match_re or both")
			}
			for name, value := range rule.Match {
				if !labelNameRE.MatchString(name) {
					o.errorf("match", "%q is not a label name", name)
				}
				if len(value) > MaxMatcherLength {
					o.errorf("match."+name, "is longer than %d characters", MaxMatcherLength)
				}
				if _, both := rule.MatchRE[name]; both {
					o.errorf("match."+name, "the label is in match and in match_re; use one of them")
				}
			}
			for name, expr := range rule.MatchRE {
				if !labelNameRE.MatchString(name) {
					o.errorf("match_re", "%q is not a label name", name)
				}
				if _, err := CompileMatcher(expr); err != nil {
					o.errorf("match_re."+name, "%s", err)
				}
			}
			if rule.Team != "" {
				o.ref("team", kindTeam, rule.Team, teams)
			}
			if len(rule.Service) > maxServiceLength || strings.ContainsFunc(rule.Service, unicode.IsControl) {
				o.errorf("service", "must be at most %d characters, without control characters", maxServiceLength)
			}
			if rule.EscalationPolicy == "" {
				o.errorf("escalation_policy", "is required")
			} else {
				o.ref("escalation_policy", kindPolicy, rule.EscalationPolicy, policies)
			}
			o.labelNames("group_by", rule.GroupBy)
			o.positive("group_interval", rule.GroupInterval)
		}
	}
}

// MinBcryptCost is the lowest bcrypt cost accepted for the bootstrap admin's
// password hash.
const MinBcryptCost = 10

// The shortest and longest session lifetimes accepted.
const (
	minSessionLifetime = time.Minute
	maxSessionLifetime = 30 * Day
)

func (a Auth) validate(o object) {
	public, err := url.Parse(a.PublicURL)
	switch {
	case a.PublicURL == "":
		o.errorf("public_url", "is required: the address at which browsers reach Beacon, such as https://beacon.example.com")
	case err != nil || public.Host == "" || (public.Scheme != "https" && public.Scheme != "http") ||
		strings.Trim(public.Path, "/") != "" || public.RawQuery != "" || public.Fragment != "" || public.User != nil:
		o.errorf("public_url", "must be an http or https address without a path, such as https://beacon.example.com")
	case public.Scheme == "http" && !loopback(public.Hostname()):
		o.errorf("public_url", "must use https; plain http is accepted only for localhost, where it is useful for trying Beacon out")
	}
	if l := a.SessionLifetime; l != nil && (time.Duration(*l) < minSessionLifetime || time.Duration(*l) > maxSessionLifetime) {
		o.errorf("session_lifetime", "must be between 1m and 30d")
	}
	if r := a.Roles; r != nil {
		o.groups("roles.admin", r.Admin)
		o.groups("roles.member", r.Member)
		if len(r.Admin) == 0 && len(r.Member) == 0 {
			o.errorf("roles", "needs admin, member or both; leave roles out to make everyone a member")
		}
	}
	if a.OIDC == nil && a.BootstrapAdmin == nil {
		o.errorf("", "needs oidc, bootstrap_admin or both: without either, nobody can sign in")
	}
	if b := a.BootstrapAdmin; b != nil {
		o.required("bootstrap_admin.password_hash", b.PasswordHash)
		// The value is checked only once it has been read. The message must
		// not repeat it: someone may have put the password itself there.
		if h := b.PasswordHash; h.IsSet() {
			if cost, err := bcrypt.Cost([]byte(h.Reveal())); err != nil || cost < MinBcryptCost {
				o.errorf("bootstrap_admin.password_hash", "must be a bcrypt hash with a cost of at least %d, as `beacon hash-password` prints it; it must not be the password itself", MinBcryptCost)
			}
		}
	}
	if a.OIDC == nil {
		return
	}
	issuer, err := url.Parse(a.OIDC.Issuer)
	switch {
	case a.OIDC.Issuer == "":
		o.errorf("oidc.issuer", "is required: the issuer URL of the identity provider")
	case err != nil || issuer.Host == "" || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.RawQuery != "" || issuer.Fragment != "":
		o.errorf("oidc.issuer", "must be an http or https URL")
	case issuer.Scheme == "http" && !loopback(issuer.Hostname()):
		// Over plain http anyone on the network path could answer in the
		// provider's place and sign people in as whoever they like.
		o.errorf("oidc.issuer", "must use https; plain http is accepted only for a provider on localhost")
	}
	if a.OIDC.ClientID == "" {
		o.errorf("oidc.client_id", "is required")
	}
	o.required("oidc.client_secret", a.OIDC.ClientSecret)
	if len(a.OIDC.Scopes) > 0 && !slices.Contains(a.OIDC.Scopes, "openid") {
		o.errorf("oidc.scopes", "must include openid")
	}
}

// loopback reports whether host is this machine.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// required reports a secret that is neither referenced nor set.
func (o object) required(field string, s Secret) {
	if !s.IsReferenced() && !s.IsSet() {
		o.errorf(field, "is required")
	}
}

func (in Integration) validate(o object) {
	blocks := map[string]bool{
		IntegrationTypeSlack: in.Slack != nil, IntegrationTypeDiscord: in.Discord != nil,
		IntegrationTypeEmail: in.Email != nil, IntegrationTypeWebhook: in.Webhook != nil,
	}
	present, known := blocks[in.Type]
	switch {
	case in.Type == "":
		o.errorf("type", "is required: slack, discord, email or webhook")
		return
	case !known:
		o.errorf("type", "%q is not slack, discord, email or webhook", in.Type)
		return
	case !present:
		o.errorf(in.Type, "the settings are missing: an integration of type %s needs a %s block", in.Type, in.Type)
	}
	for _, block := range []string{IntegrationTypeSlack, IntegrationTypeDiscord, IntegrationTypeEmail, IntegrationTypeWebhook} {
		if block != in.Type && blocks[block] {
			o.errorf(block, "does not belong in an integration of type %s", in.Type)
		}
	}
	if !present {
		return
	}
	switch in.Type {
	case IntegrationTypeSlack:
		o.required("slack.bot_token", in.Slack.BotToken)
		o.required("slack.signing_secret", in.Slack.SigningSecret)
	case IntegrationTypeDiscord:
		o.required("discord.bot_token", in.Discord.BotToken)
		if !publicKeyRE.MatchString(in.Discord.PublicKey) {
			o.errorf("discord.public_key", "is required: the application's public key, 64 hexadecimal characters")
		}
		if !snowflakeRE.MatchString(in.Discord.ApplicationID) {
			o.errorf("discord.application_id", "is required: the application's numeric ID")
		}
	case IntegrationTypeEmail:
		e := in.Email
		if e.Host == "" {
			o.errorf("email.host", "is required")
		}
		if e.Port < 1 || e.Port > 65535 {
			o.errorf("email.port", "is required: a port between 1 and 65535")
		}
		if !validEmail(e.From) {
			o.errorf("email.from", "is required: an email address")
		}
		switch e.TLS {
		case EmailTLSStartTLS, EmailTLSImplicit, EmailTLSNone:
		default:
			o.errorf("email.tls", "is required: starttls, implicit or none")
		}
		hasPassword := e.Password.IsReferenced() || e.Password.IsSet()
		if (e.Username != "") != hasPassword {
			o.errorf("email.password", "username and password go together: set both or neither")
		}
		// A password must not cross a network in the clear.
		if e.TLS == EmailTLSNone && hasPassword && e.Host != "localhost" && e.Host != "127.0.0.1" && e.Host != "::1" {
			o.errorf("email.tls", "none would send the password unencrypted; use starttls or implicit, or remove username and password")
		}
	case IntegrationTypeWebhook:
		u, err := url.Parse(in.Webhook.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			o.errorf("webhook.url", "is required: an http or https URL")
		}
		for name := range in.Webhook.Headers {
			if !headerNameRE.MatchString(name) {
				o.errorf("webhook.headers", "%q is not a header name", name)
			}
		}
	}
}

func (r Rotation) validate(o object) {
	if len(r.Members) == 0 {
		o.errorf("rotation.members", "at least one member is required")
	}
	o.emails("rotation.members", r.Members)

	length := time.Duration(r.ShiftLength)
	weeks := length > 0 && length%Week == 0
	if length <= 0 || length%Day != 0 {
		o.errorf("rotation.shift_length", "is required: a whole number of days or weeks, such as 1d, 3d or 1w")
	}
	if !handoffRE.MatchString(r.Handoff.Time) {
		o.errorf("rotation.handoff.time", `is required: a time of day such as "09:00"`)
	}
	switch {
	case weeks && !weekdays[r.Handoff.Day]:
		o.errorf("rotation.handoff.day", "is required when shifts are whole weeks: a weekday such as monday")
	case !weeks && length > 0 && r.Handoff.Day != "":
		o.errorf("rotation.handoff.day", "applies only when shifts are whole weeks")
	}
}

func (t Target) validate(o object, at string, schedules, teams, integrations map[string]Origin, types map[string]string) {
	forms := 0
	for _, set := range []bool{t.Schedule != "", t.Team != "", t.User != "", t.Channel != "" || t.Integration != ""} {
		if set {
			forms++
		}
	}
	if forms != 1 {
		o.errorf(at, "a target is exactly one of {schedule}, {team}, {user} and {channel, integration}")
		return
	}
	switch {
	case t.Schedule != "":
		o.ref(at+".schedule", kindSchedule, t.Schedule, schedules)
	case t.Team != "":
		o.ref(at+".team", kindTeam, t.Team, teams)
	case t.User != "":
		if !validEmail(t.User) {
			o.errorf(at+".user", "%q is not an email address", t.User)
		}
	default:
		if t.Channel == "" {
			o.errorf(at+".channel", "is required with integration")
		}
		if t.Integration == "" {
			o.errorf(at+".integration", "is required with channel")
		} else {
			o.ref(at+".integration", kindIntegration, t.Integration, integrations)
		}
		if len(t.Via) > 0 {
			o.errorf(at+".via", "applies to schedule, team and user targets, not to a channel")
		}
		// What a channel is depends on the service.
		switch {
		case t.Channel == "":
		case types[t.Integration] == IntegrationTypeDiscord && !discordChannelRE.MatchString(t.Channel):
			o.errorf(at+".channel", "must be the numeric ID of the Discord channel, in quotes; Discord does not take a channel by name")
		case types[t.Integration] == IntegrationTypeEmail && !validEmail(t.Channel):
			o.errorf(at+".channel", "must be an email address: for an email integration the channel is where the message is sent")
		}
		return
	}
	for i, name := range t.Via {
		o.ref(fmt.Sprintf("%s.via[%d]", at, i), kindIntegration, name, integrations)
	}
}
