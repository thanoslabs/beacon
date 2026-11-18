// Package routing decides what happens to an alert by its labels: which
// routing rule it matches, and with that its team, its service, its
// escalation policy and how it is grouped. docs/configuration.md describes
// the rules.
package routing

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/thanoslabs/beacon/internal/config"
)

// LabelService is the label that names the service of an alert when the
// rule it matched does not.
const LabelService = "service"

// Refs maps the names of provisioned objects to the identifiers of their
// rows, so that a route can say which rows an alert refers to.
type Refs struct {
	Rules    map[string]string
	Teams    map[string]string
	Policies map[string]string
}

// Route is what routing decided for one alert.
type Route struct {
	// Rule is the name of the rule the alert matched. It is empty, like
	// RuleID, when no rule matched and the default applied.
	Rule   string
	RuleID string
	// Team is the team the alert is assigned to, if any.
	Team   string
	TeamID string
	// Service is the service the alert is assigned to, if any.
	Service string
	// Policy is the escalation policy to run. It is empty only when there
	// is no routing block at all.
	Policy   string
	PolicyID string
	// Grouping says how the alert is grouped.
	Grouping config.Grouping
}

// Matched reports whether a rule matched, as opposed to the default.
func (r Route) Matched() bool { return r.Rule != "" }

// rule is a routing rule ready to be tried.
type rule struct {
	// equal and matching are sorted by label, so a rule is always tried the
	// same way.
	equal    []equalMatcher
	matching []regexpMatcher
	service  string
	route    Route
}

type equalMatcher struct{ label, value string }

type regexpMatcher struct {
	label string
	re    *regexp.Regexp
}

func (r rule) matches(labels map[string]string) bool {
	// A label the alert does not have counts as empty, as in Prometheus.
	for _, m := range r.equal {
		if labels[m.label] != m.value {
			return false
		}
	}
	for _, m := range r.matching {
		if !m.re.MatchString(labels[m.label]) {
			return false
		}
	}
	return true
}

// Router routes alerts by a fixed set of rules. It is safe for concurrent
// use; a changed configuration makes a new Router.
type Router struct {
	rules    []rule
	fallback Route
}

// New builds the router for a routing block. A nil block gives a router
// that matches nothing and whose default has no escalation policy: alerts
// are stored, grouped with the default grouping, and nobody is notified.
//
// The block must be valid. An error means it is not, or that refs lacks the
// row of an object the block names.
func New(r *config.Routing, refs Refs) (*Router, error) {
	rt := &Router{fallback: Route{Grouping: r.Grouping()}}
	if r == nil {
		return rt, nil
	}
	var err error
	rt.fallback, err = route("", r.Default.Team, r.Default.EscalationPolicy, r.Grouping(), refs)
	if err != nil {
		return nil, fmt.Errorf("routing default: %w", err)
	}
	for _, c := range r.Rules {
		compiled := rule{service: c.Service}
		if compiled.route, err = route(c.Name, c.Team, c.EscalationPolicy, r.GroupingFor(c), refs); err != nil {
			return nil, fmt.Errorf("routing rule %q: %w", c.Name, err)
		}
		for label, value := range c.Match {
			compiled.equal = append(compiled.equal, equalMatcher{label, value})
		}
		for label, expr := range c.MatchRE {
			re, err := config.CompileMatcher(expr)
			if err != nil {
				return nil, fmt.Errorf("routing rule %q: match_re.%s: %w", c.Name, label, err)
			}
			compiled.matching = append(compiled.matching, regexpMatcher{label, re})
		}
		slices.SortFunc(compiled.equal, func(a, b equalMatcher) int { return strings.Compare(a.label, b.label) })
		slices.SortFunc(compiled.matching, func(a, b regexpMatcher) int { return strings.Compare(a.label, b.label) })
		rt.rules = append(rt.rules, compiled)
	}
	return rt, nil
}

// route resolves the names of a rule, or of the default, to rows.
func route(ruleName, team, policy string, grouping config.Grouping, refs Refs) (Route, error) {
	out := Route{Rule: ruleName, Team: team, Policy: policy, Grouping: grouping}
	var ok bool
	if ruleName != "" {
		if out.RuleID, ok = refs.Rules[ruleName]; !ok {
			return Route{}, fmt.Errorf("the rule has no row")
		}
	}
	if team != "" {
		if out.TeamID, ok = refs.Teams[team]; !ok {
			return Route{}, fmt.Errorf("team %q has no row", team)
		}
	}
	if out.PolicyID, ok = refs.Policies[policy]; !ok {
		return Route{}, fmt.Errorf("escalation policy %q has no row", policy)
	}
	return out, nil
}

// Route returns what applies to an alert with these labels: the first rule,
// in the order of the configuration, whose matchers all match, or the
// default when none does. The service is the one the rule names, or else
// the alert's "service" label.
func (rt *Router) Route(labels map[string]string) Route {
	for _, r := range rt.rules {
		if r.matches(labels) {
			out := r.route
			if out.Service = r.service; out.Service == "" {
				out.Service = labels[LabelService]
			}
			return out
		}
	}
	out := rt.fallback
	out.Service = labels[LabelService]
	return out
}
