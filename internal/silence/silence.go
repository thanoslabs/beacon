// Package silence holds silences: runtime state with which people keep
// Beacon quiet about the alerts that match some labels, for a period.
// docs/silences.md describes them and their API.
package silence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thanoslabs/beacon/internal/config"
)

// The limits of what a silence may hold. A silence is created through the
// API, so everything in it is bounded.
const (
	// MaxMatchers bounds the matchers of one silence.
	MaxMatchers = 20
	// MaxLabelNameLength bounds the label a matcher names.
	MaxLabelNameLength = 200
	// MaxValueLength bounds the value or pattern of a matcher. It is the
	// bound of a pattern in a routing rule.
	MaxValueLength = config.MaxMatcherLength
	// MaxCommentLength bounds the comment, in characters.
	MaxCommentLength = 1000
	// MaxDuration bounds how long a silence may last, and how far ahead it
	// may begin. A silence that never ends is a problem nobody will see.
	MaxDuration = 366 * config.Day
)

// Operator says how a matcher compares a label with its value.
type Operator string

// The operators, as in Prometheus and Alertmanager.
const (
	// Equal matches a label that has exactly the value.
	Equal Operator = "="
	// NotEqual matches a label that has any other value.
	NotEqual Operator = "!="
	// Matches matches a label whose whole value matches the pattern.
	Matches Operator = "=~"
	// NotMatches matches a label whose value does not match the pattern.
	NotMatches Operator = "!~"
)

// Matcher is one condition on a label. A label that an alert does not have
// counts as empty.
type Matcher struct {
	Label    string   `json:"label"`
	Operator Operator `json:"operator"`
	Value    string   `json:"value"`

	// re is the compiled pattern of a Matches or NotMatches matcher.
	re *regexp.Regexp
}

// matches reports whether an alert with these labels satisfies the matcher.
func (m Matcher) matches(labels map[string]string) bool {
	value := labels[m.Label]
	switch m.Operator {
	case Equal:
		return value == m.Value
	case NotEqual:
		return value != m.Value
	case Matches:
		return m.re.MatchString(value)
	case NotMatches:
		return !m.re.MatchString(value)
	}
	return false
}

// State says where a silence is in its life.
type State string

// The states. They follow from the period and the time; nothing stores them.
const (
	// Pending is a silence whose period has not begun.
	Pending State = "pending"
	// Active is a silence in force.
	Active State = "active"
	// Expired is a silence whose period is over, by itself or because
	// someone expired it.
	Expired State = "expired"
)

// Silence keeps Beacon quiet about the alerts that match all its matchers,
// from StartsAt until EndsAt.
type Silence struct {
	ID       string    `json:"id"`
	Matchers []Matcher `json:"matchers"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	// CreatedBy is the email address of the user who created the silence.
	// It is empty when the silence was created with an API token.
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedByToken is the name of the API token the silence was created
	// with. It is empty when a signed-in user created it.
	CreatedByToken string    `json:"created_by_token,omitempty"`
	Comment        string    `json:"comment"`
	CreatedAt      time.Time `json:"created_at"`
	// State is where the silence was in its life when it was read.
	State State `json:"state"`
}

// StateAt returns the state of the silence at a time.
func (s Silence) StateAt(at time.Time) State {
	switch {
	case !at.Before(s.EndsAt):
		return Expired
	case at.Before(s.StartsAt):
		return Pending
	}
	return Active
}

// Silences reports whether the silence matches an alert with these labels:
// every matcher must. It does not look at the period.
func (s Silence) Silences(labels map[string]string) bool {
	for _, m := range s.Matchers {
		if !m.matches(labels) {
			return false
		}
	}
	return len(s.Matchers) > 0
}

// Error is something wrong with a silence that someone asked for. Field
// names the part at fault, in the form of the request, such as
// "matchers[1].value".
type Error struct {
	Field string
	Msg   string
}

func (e *Error) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

func invalid(field, format string, args ...any) *Error {
	return &Error{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// compile checks the matchers and prepares them for matching. The error is
// an *Error.
func compile(matchers []Matcher) ([]Matcher, error) {
	if len(matchers) == 0 {
		return nil, invalid("matchers", "at least one matcher is required")
	}
	if len(matchers) > MaxMatchers {
		return nil, invalid("matchers", "at most %d matchers are allowed", MaxMatchers)
	}
	out := make([]Matcher, len(matchers))
	selective := false
	for i, m := range matchers {
		at := fmt.Sprintf("matchers[%d]", i)
		switch {
		case !config.ValidLabelName(m.Label) || len(m.Label) > MaxLabelNameLength:
			return nil, invalid(at+".label", "must be a label name: letters, digits and underscores, not starting with a digit, at most %d characters", MaxLabelNameLength)
		case len(m.Value) > MaxValueLength:
			return nil, invalid(at+".value", "is longer than %d characters", MaxValueLength)
		case !utf8.ValidString(m.Value) || strings.ContainsRune(m.Value, 0):
			return nil, invalid(at+".value", "must be valid text")
		}
		switch m.Operator {
		case Equal, NotEqual:
		case Matches, NotMatches:
			re, err := config.CompileMatcher(m.Value)
			if err != nil {
				return nil, invalid(at+".value", "%s", err)
			}
			m.re = re
		default:
			return nil, invalid(at+".operator", `must be "=", "!=", "=~" or "!~"`)
		}
		// A matcher is selective when an alert without the label does not
		// satisfy it.
		selective = selective || !m.matches(nil)
		out[i] = m
	}
	if !selective {
		// As in Alertmanager: without this, {team!="x"} silences every alert
		// that has no team label, which is most of them by accident.
		return nil, invalid("matchers", "at least one matcher must not match an empty value; otherwise the silence covers alerts that lack every label it names")
	}
	return out, nil
}

// decodeMatchers reads the stored form of the matchers. They were checked
// when the silence was created.
func decodeMatchers(raw []byte) ([]Matcher, error) {
	var matchers []Matcher
	if err := json.Unmarshal(raw, &matchers); err != nil {
		return nil, fmt.Errorf("decode matchers: %w", err)
	}
	compiled, err := compile(matchers)
	if err != nil {
		return nil, fmt.Errorf("stored matchers: %w", err)
	}
	return compiled, nil
}
