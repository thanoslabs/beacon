// Package schedule works out who is on call. A schedule's rotation is defined
// in the configuration; where the rotation is, and every shift that has
// happened, is kept in Postgres.
//
// This file is the arithmetic, and it is pure: given where a rotation is, it
// says what happens next. Store applies it to Postgres. docs/schedules.md
// describes the rules.
package schedule

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	// Handoff times are read against the embedded zone database, so that the
	// answer does not depend on the machine Beacon runs on.
	_ "time/tzdata"

	"github.com/thanoslabs/beacon/internal/config"
)

// Definition is a rotation as it is kept beside a schedule's position: who
// takes turns, for how long, and when they hand over.
type Definition struct {
	// TimeZone is the IANA zone in which the handoff time is read.
	TimeZone string `json:"time_zone"`
	// Members are user email addresses in rotation order, lowercase.
	Members []string `json:"members"`
	// ShiftDays is the length of a shift in days.
	ShiftDays int `json:"shift_days"`
	// HandoffTime is the local time of the handoff, "HH:MM".
	HandoffTime string `json:"handoff_time"`
	// HandoffDay is the weekday of the handoff, for shifts of whole weeks.
	HandoffDay string `json:"handoff_day,omitempty"`
}

// DefinitionOf returns the rotation that a valid schedule defines.
func DefinitionOf(s config.Schedule) Definition {
	d := Definition{
		TimeZone:    s.TimeZone,
		Members:     make([]string, 0, len(s.Rotation.Members)),
		ShiftDays:   int(time.Duration(s.Rotation.ShiftLength) / config.Day),
		HandoffTime: s.Rotation.Handoff.Time,
		HandoffDay:  s.Rotation.Handoff.Day,
	}
	for _, m := range s.Rotation.Members {
		d.Members = append(d.Members, config.NormalizeEmail(m))
	}
	return d
}

// Equal reports whether two definitions describe the same rotation.
func (d Definition) Equal(o Definition) bool {
	return d.TimeZone == o.TimeZone && d.ShiftDays == o.ShiftDays && d.HandoffTime == o.HandoffTime &&
		d.HandoffDay == o.HandoffDay && slices.Equal(d.Members, o.Members)
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

// rota is a definition made ready for arithmetic.
type rota struct {
	loc          *time.Location
	days         int
	hour, minute int
	// weekly says that handoffs fall on weekday only.
	weekly  bool
	weekday time.Weekday
}

// compile checks a definition and prepares it. A definition that came from a
// valid configuration always compiles; one read back from the database is
// checked again, because arithmetic on a bad one would never end.
func (d Definition) compile() (rota, error) {
	loc, err := time.LoadLocation(d.TimeZone)
	if err != nil || d.TimeZone == "" {
		return rota{}, fmt.Errorf("time zone %q is not known", d.TimeZone)
	}
	hh, mm, ok := strings.Cut(d.HandoffTime, ":")
	hour, errH := strconv.Atoi(hh)
	minute, errM := strconv.Atoi(mm)
	if !ok || errH != nil || errM != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return rota{}, fmt.Errorf("handoff time %q is not a time of day", d.HandoffTime)
	}
	if d.ShiftDays < 1 {
		return rota{}, errors.New("a shift must be at least one day long")
	}
	r := rota{loc: loc, days: d.ShiftDays, hour: hour, minute: minute}
	if d.HandoffDay != "" {
		if r.weekday, r.weekly = weekdays[d.HandoffDay]; !r.weekly {
			return rota{}, fmt.Errorf("handoff day %q is not a weekday", d.HandoffDay)
		}
		if d.ShiftDays%7 != 0 {
			return rota{}, errors.New("a handoff day needs shifts of whole weeks")
		}
	}
	return r, nil
}

// instant returns the moment at which clocks in loc read the given date and
// time of day. The day may be out of range, as for time.Date.
//
// Twice a year, in zones with daylight saving, that is not one moment:
//
//   - A time that the clocks skip, when they are put forward, never comes.
//     The handoff then happens as late as the clocks were moved: 02:30 on
//     such a day is 03:30. No shift is lost and none overlaps.
//   - A time that comes twice, when the clocks are put back, means its first
//     occurrence.
//
// time.Date leaves both cases to the implementation, so they are decided
// here.
func instant(loc *time.Location, year int, month time.Month, day, hour, minute int) time.Time {
	naive := time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
	reads := func(t time.Time) bool {
		l := t.In(loc)
		return l.Year() == naive.Year() && l.Month() == naive.Month() && l.Day() == naive.Day() &&
			l.Hour() == naive.Hour() && l.Minute() == naive.Minute()
	}
	// The offsets on either side of any change near the time. Offsets change
	// far less often than once in two days.
	_, before := naive.Add(-26 * time.Hour).In(loc).Zone()
	_, after := naive.Add(26 * time.Hour).In(loc).Zone()
	early := naive.Add(-time.Duration(before) * time.Second)
	late := naive.Add(-time.Duration(after) * time.Second)
	if late.Before(early) {
		early, late = late, early
	}
	switch {
	case reads(early):
		return early
	case reads(late):
		return late
	}
	// Skipped. Read with the offset in force before the change, the time
	// lands after the gap, by the length of the gap.
	return naive.Add(-time.Duration(before) * time.Second)
}

// on returns the handoff on a calendar day, and whether the rotation has one
// that day at all.
func (r rota) on(year int, month time.Month, day int) (time.Time, bool) {
	if r.weekly && time.Date(year, month, day, 12, 0, 0, 0, time.UTC).Weekday() != r.weekday {
		return time.Time{}, false
	}
	return instant(r.loc, year, month, day, r.hour, r.minute), true
}

// isHandoff reports whether t is a moment at which the rotation hands over.
func (r rota) isHandoff(t time.Time) bool {
	y, m, d := t.In(r.loc).Date()
	h, ok := r.on(y, m, d)
	return ok && h.Equal(t)
}

// next returns the handoff that ends a shift which began at start.
//
// When start is itself a handoff, the next one is a shift length of calendar
// days later, at the handoff time. It is found from the date and not by
// adding hours, so a daylight saving change moves the moment and not the
// time on the clock.
//
// When it is not, as for a schedule that is new, it is the first handoff at
// least one full shift length away, so that shift is never shorter than a
// normal one.
func (r rota) next(start time.Time) time.Time {
	local := start.In(r.loc)
	y, m, d := local.Date()
	if r.isHandoff(start) {
		h, _ := r.on(y, m, d+r.days)
		return h
	}
	full := local.AddDate(0, 0, r.days)
	for day := d + r.days; ; day++ {
		if h, ok := r.on(y, m, day); ok && !h.Before(full) {
			return h
		}
	}
}

// after returns the first handoff later than t, however soon that is.
func (r rota) after(t time.Time) time.Time {
	y, m, d := t.In(r.loc).Date()
	for day := d; ; day++ {
		if h, ok := r.on(y, m, day); ok && h.After(t) {
			return h
		}
	}
}

// State is where a rotation is: who is on call under it, since when and
// until when, and the change of definition that is waiting, if any.
type State struct {
	// Member is the email address of the person on call, or empty when no
	// member of the rotation is a user.
	Member string
	// ShiftStart is when the shift in progress began.
	ShiftStart time.Time
	// NextHandoff is when it ends.
	NextHandoff time.Time
	// Definition is the rotation in force.
	Definition Definition
	// Pending is a changed rotation that has not taken over yet, or nil.
	Pending *Definition
	// PendingAt is when Pending takes over. Nil means at the next handoff.
	PendingAt *time.Time
}

// Start returns the state of a rotation that begins at now: the first member
// who is a user is on call, and the first handoff is at least one full shift
// away. known reports whether an email address belongs to a user.
func Start(d Definition, now time.Time, known func(email string) bool) (State, error) {
	r, err := d.compile()
	if err != nil {
		return State{}, err
	}
	return State{Member: successor(d.Members, d.Members, "", known), ShiftStart: now, NextHandoff: r.next(now), Definition: d}, nil
}

// WithDefinition returns the state once the files define the rotation as d,
// read at now. This is the whole of what a change in the files does at the
// moment it is applied: it never moves the shift in progress.
//
//   - When d is the rotation in force, nothing is pending. That also takes
//     back a change that was waiting.
//   - Otherwise d is pending. It takes over at the next handoff, or at
//     effectiveFrom when that is given. A date that has passed means now.
func (s State) WithDefinition(d Definition, effectiveFrom *time.Time, now time.Time) State {
	s.Pending, s.PendingAt = nil, nil
	if d.Equal(s.Definition) {
		return s
	}
	s.Pending = &d
	if effectiveFrom != nil {
		at := *effectiveFrom
		if at.Before(now) {
			at = now
		}
		s.PendingAt = &at
	}
	return s
}

// Due returns when the state next changes: at the next handoff, or sooner
// when a pending rotation takes over at a stated time before it.
func (s State) Due() time.Time {
	if s.Pending != nil && s.PendingAt != nil && s.PendingAt.Before(s.NextHandoff) {
		return *s.PendingAt
	}
	return s.NextHandoff
}

// takeover returns when the pending rotation takes over.
func (s State) takeover() time.Time {
	if s.PendingAt != nil {
		return *s.PendingAt
	}
	return s.NextHandoff
}

// Next returns the state after the change that is due at s.Due(), and
// whether that change ended the shift in progress.
//
// With nothing pending, or a pending rotation whose time has not come, it is
// an ordinary handoff: the member after the one on call takes over, and
// members who are not users are passed over.
//
// A pending rotation takes over in one of two ways, and neither takes a
// shift away from the person who has it:
//
//   - At a handoff, which is the rule unless a time is stated. The handoff
//     happens as it would have, and the person who takes over is the one who
//     follows in the new order. Their shift ends at the first handoff of the
//     new rotation that leaves them a full shift.
//   - At a stated time in the middle of a shift. Whoever is on call stays on
//     call, and their shift ends at the first handoff of the new rotation
//     after that time. Only when the new rotation no longer has them does a
//     shift end then, and the person who follows takes the rest of it.
func (s State) Next(known func(email string) bool) (next State, shiftEnded bool, err error) {
	at := s.Due()
	if s.Pending == nil || (s.PendingAt != nil && s.PendingAt.After(at)) {
		r, err := s.Definition.compile()
		if err != nil {
			return State{}, false, err
		}
		s.Member = successor(s.Definition.Members, s.Definition.Members, s.Member, known)
		s.ShiftStart, s.NextHandoff = at, r.next(at)
		return s, true, nil
	}

	old, d := s.Definition, *s.Pending
	r, err := d.compile()
	if err != nil {
		return State{}, false, err
	}
	next = State{Member: s.Member, ShiftStart: s.ShiftStart, Definition: d}
	switch {
	case at.Equal(s.NextHandoff):
		next.Member = successor(old.Members, d.Members, s.Member, known)
		next.ShiftStart, next.NextHandoff, shiftEnded = at, r.next(at), true
	case s.Member != "" && slices.Contains(d.Members, s.Member):
		next.NextHandoff = r.after(at)
	default:
		next.Member = successor(old.Members, d.Members, s.Member, known)
		next.ShiftStart, next.NextHandoff, shiftEnded = at, r.after(at), true
	}
	return next, shiftEnded, nil
}

// Span is a stretch of time during which one person is on call.
type Span struct {
	// Member is an email address, or empty when nobody is on call.
	Member string
	Start  time.Time
	End    time.Time
}

// maxProjected bounds how many shifts Project works out.
const maxProjected = 5000

// Project returns the shifts the rotation will have from the one in
// progress until the given time, if nothing but time changes: the same
// arithmetic that the store carries out, run ahead. The last shift may end
// after until.
func (s State) Project(until time.Time, known func(email string) bool) ([]Span, error) {
	var out []Span
	for range maxProjected {
		next, ended, err := s.Next(known)
		if err != nil {
			return nil, err
		}
		if ended {
			out = append(out, Span{Member: s.Member, Start: s.ShiftStart, End: next.ShiftStart})
		}
		if !next.ShiftStart.Before(until) && ended {
			return out, nil
		}
		s = next
	}
	return nil, fmt.Errorf("more than %d shifts lie before %s", maxProjected, until.Format(time.RFC3339))
}

// successor returns who follows current in the order of next, among the
// members that are known, or "" when none is.
//
// The position of a rotation is a person, not an index, which is what keeps
// the order of everyone else when a member is added or removed. When current
// is not in next, the successor is the first member of next who came after
// current in previous, the order that was in force until now. With no
// current, or one that neither list has, the order starts at its beginning.
func successor(previous, next []string, current string, known func(string) bool) string {
	start := 0
	if i := slices.Index(next, current); i >= 0 {
		start = i + 1
	} else if i := slices.Index(previous, current); i >= 0 {
		for step := 1; step <= len(previous); step++ {
			if j := slices.Index(next, previous[(i+step)%len(previous)]); j >= 0 {
				start = j
				break
			}
		}
	}
	for step := range next {
		if m := next[(start+step)%len(next)]; known(m) {
			return m
		}
	}
	return ""
}
