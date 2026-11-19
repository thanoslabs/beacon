package schedule

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func everyone(string) bool { return true }

func daily(zone, at string, members ...string) Definition {
	return Definition{TimeZone: zone, Members: members, ShiftDays: 1, HandoffTime: at}
}

func weekly(zone, day, at string, members ...string) Definition {
	return Definition{TimeZone: zone, Members: members, ShiftDays: 7, HandoffTime: at, HandoffDay: day}
}

func mustLoad(t *testing.T, zone string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// walk starts a rotation at start and returns its first n handoffs, each in
// the rotation's zone.
func walk(t *testing.T, d Definition, start time.Time, n int) []time.Time {
	t.Helper()
	s, err := Start(d, start, everyone)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	loc := mustLoad(t, d.TimeZone)
	var out []time.Time
	for range n {
		out = append(out, s.NextHandoff.In(loc))
		if s, _, err = s.Next(everyone); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	return out
}

func format(times []time.Time) string {
	var out []string
	for _, t := range times {
		out = append(out, t.Format("Jan 2 15:04 MST"))
	}
	return strings.Join(out, ", ")
}

// TestDaylightSaving walks rotations across the days on which clocks change,
// in both directions and in both hemispheres. The handoff keeps its time on
// the local clock, so the shift around the change is an hour shorter or
// longer; a handoff in the hour that is skipped happens after the gap, and
// one in the hour that comes twice happens the first time.
func TestDaylightSaving(t *testing.T) {
	utc := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name  string
		def   Definition
		start time.Time
		want  string
		// hours are the lengths of the shifts between the handoffs.
		hours string
	}{
		{
			// Clocks go forward on 8 March 2026 at 02:00.
			name: "New York, forward, 09:00", def: daily("America/New_York", "09:00", "a"), start: utc(2026, 3, 5, 14, 0),
			want: "Mar 6 09:00 EST, Mar 7 09:00 EST, Mar 8 09:00 EDT, Mar 9 09:00 EDT", hours: "24 23 24",
		},
		{
			// 02:30 does not exist on 8 March: the handoff is at 03:30.
			name: "New York, forward, in the skipped hour", def: daily("America/New_York", "02:30", "a"), start: utc(2026, 3, 5, 7, 30),
			want: "Mar 6 02:30 EST, Mar 7 02:30 EST, Mar 8 03:30 EDT, Mar 9 02:30 EDT, Mar 10 02:30 EDT", hours: "24 24 23 24",
		},
		{
			// Clocks go back on 1 November 2026 at 02:00.
			name: "New York, back, 09:00", def: daily("America/New_York", "09:00", "a"), start: utc(2026, 10, 30, 13, 0),
			want: "Oct 31 09:00 EDT, Nov 1 09:00 EST, Nov 2 09:00 EST", hours: "25 24",
		},
		{
			// 01:30 comes twice on 1 November: the handoff is the first.
			name: "New York, back, in the repeated hour", def: daily("America/New_York", "01:30", "a"), start: utc(2026, 10, 30, 5, 30),
			want: "Oct 31 01:30 EDT, Nov 1 01:30 EDT, Nov 2 01:30 EST, Nov 3 01:30 EST", hours: "24 25 24",
		},
		{
			// In Sydney clocks go forward on 4 October 2026 at 02:00.
			name: "Sydney, forward, 09:00", def: daily("Australia/Sydney", "09:00", "a"), start: utc(2026, 10, 1, 23, 0),
			want: "Oct 3 09:00 AEST, Oct 4 09:00 AEDT, Oct 5 09:00 AEDT", hours: "23 24",
		},
		{
			name: "Sydney, forward, in the skipped hour", def: daily("Australia/Sydney", "02:30", "a"), start: utc(2026, 10, 1, 16, 30),
			want: "Oct 3 02:30 AEST, Oct 4 03:30 AEDT, Oct 5 02:30 AEDT", hours: "24 23",
		},
		{
			// And back on 5 April 2026 at 03:00.
			name: "Sydney, back, 09:00", def: daily("Australia/Sydney", "09:00", "a"), start: utc(2026, 4, 2, 22, 0),
			want: "Apr 4 09:00 AEDT, Apr 5 09:00 AEST, Apr 6 09:00 AEST", hours: "25 24",
		},
		{
			name: "Sydney, back, in the repeated hour", def: daily("Australia/Sydney", "02:30", "a"), start: utc(2026, 4, 2, 15, 30),
			want: "Apr 4 02:30 AEDT, Apr 5 02:30 AEDT, Apr 6 02:30 AEST", hours: "24 25",
		},
		{
			// Lord Howe Island moves its clocks by half an hour, at 02:00.
			name: "Lord Howe, forward, in the skipped half hour", def: daily("Australia/Lord_Howe", "02:15", "a"), start: utc(2026, 10, 1, 15, 45),
			want: "Oct 3 02:15 +1030, Oct 4 02:45 +11, Oct 5 02:15 +11", hours: "24 23.5",
		},
		{
			// A weekly shift across the change in London, 29 March 2026.
			name: "London, forward, weekly", def: weekly("Europe/London", "monday", "09:00", "a"), start: utc(2026, 3, 16, 9, 0),
			want: "Mar 23 09:00 GMT, Mar 30 09:00 BST, Apr 6 09:00 BST", hours: "167 168",
		},
		{
			name: "London, back, weekly", def: weekly("Europe/London", "monday", "09:00", "a"), start: utc(2026, 10, 12, 8, 0),
			want: "Oct 19 09:00 BST, Oct 26 09:00 GMT, Nov 2 09:00 GMT", hours: "169 168",
		},
		{
			// Three days at a time, across the change.
			name: "New York, forward, three days", def: Definition{TimeZone: "America/New_York", Members: []string{"a"}, ShiftDays: 3, HandoffTime: "17:00"}, start: utc(2026, 3, 3, 22, 0),
			want: "Mar 6 17:00 EST, Mar 9 17:00 EDT, Mar 12 17:00 EDT", hours: "71 72",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := walk(t, tc.def, tc.start, strings.Count(tc.want, ",")+1)
			if format(got) != tc.want {
				t.Errorf("handoffs:\n got %s\nwant %s", format(got), tc.want)
			}
			var hours []string
			for i := 1; i < len(got); i++ {
				hours = append(hours, fmt.Sprint(got[i].Sub(got[i-1]).Hours()))
			}
			if strings.Join(hours, " ") != tc.hours {
				t.Errorf("shift lengths in hours: got %s, want %s", strings.Join(hours, " "), tc.hours)
			}
		})
	}
}

// TestHandoffsNeverDriftOrOverlap walks rotations through several years in
// zones with every kind of clock change. Every handoff comes after the one
// before, a shift is its length give or take the clock change, and the
// handoff reads its time on the local clock on every day but the ones that
// skip it.
func TestHandoffsNeverDriftOrOverlap(t *testing.T) {
	zones := []string{"America/New_York", "Australia/Sydney", "Europe/London", "Australia/Lord_Howe", "America/Santiago", "Asia/Kolkata", "Pacific/Chatham", "UTC"}
	rnd := rand.New(rand.NewPCG(1, 2))
	for _, zone := range zones {
		loc := mustLoad(t, zone)
		for range 20 {
			at := fmt.Sprintf("%02d:%02d", rnd.IntN(24), rnd.IntN(60))
			def := Definition{TimeZone: zone, Members: []string{"a", "b"}, ShiftDays: 1 + rnd.IntN(4), HandoffTime: at}
			if rnd.IntN(2) == 0 {
				def.ShiftDays, def.HandoffDay = 7*(1+rnd.IntN(2)), "wednesday"
			}
			start := time.Date(2024, time.Month(1+rnd.IntN(12)), 1+rnd.IntN(28), rnd.IntN(24), rnd.IntN(60), 0, 0, time.UTC)
			s, err := Start(def, start, everyone)
			if err != nil {
				t.Fatal(err)
			}
			if first := s.NextHandoff.Sub(start); first < time.Duration(def.ShiftDays)*24*time.Hour-time.Hour {
				t.Fatalf("%s %+v from %s: the first shift lasts only %s", zone, def, start, first)
			}
			skipped := 0
			for range 3 * 365 / def.ShiftDays {
				next, _, err := s.Next(everyone)
				if err != nil {
					t.Fatal(err)
				}
				length, want := next.NextHandoff.Sub(s.NextHandoff), time.Duration(def.ShiftDays)*24*time.Hour
				if length < want-time.Hour || length > want+time.Hour {
					t.Fatalf("%s %+v: the shift from %s lasts %s", zone, def, s.NextHandoff.In(loc), length)
				}
				if next.ShiftStart != s.NextHandoff {
					t.Fatalf("%s %+v: a shift begins at %s, and the one before ended at %s", zone, def, next.ShiftStart, s.NextHandoff)
				}
				local := next.NextHandoff.In(loc)
				if local.Format("15:04") != at {
					skipped++
				}
				if def.HandoffDay != "" && local.Weekday() != time.Wednesday {
					t.Fatalf("%s %+v: handoff on a %s", zone, def, local.Weekday())
				}
				s = next
			}
			// At most one day a year skips the time.
			if skipped > 3 {
				t.Fatalf("%s %+v: %d handoffs were not at %s", zone, def, skipped, at)
			}
		}
	}
}

// TestFirstShiftIsNeverShort checks the first handoff of a rotation that
// starts at an arbitrary moment: the first one at least a full shift away.
func TestFirstShiftIsNeverShort(t *testing.T) {
	london := mustLoad(t, "Europe/London")
	at := func(d, h, m int) time.Time { return time.Date(2026, 1, d, h, m, 0, 0, london) }
	for _, tc := range []struct {
		name  string
		def   Definition
		start time.Time
		want  time.Time
	}{
		// 6 January 2026 is a Tuesday.
		{"daily, after the time", daily("Europe/London", "09:00", "a"), at(6, 10, 0), at(8, 9, 0)},
		{"daily, before the time", daily("Europe/London", "09:00", "a"), at(6, 8, 0), at(7, 9, 0)},
		{"daily, at the time", daily("Europe/London", "09:00", "a"), at(6, 9, 0), at(7, 9, 0)},
		{"weekly, the day after", weekly("Europe/London", "monday", "09:00", "a"), at(6, 10, 0), at(19, 9, 0)},
		{"weekly, on the day before the time", weekly("Europe/London", "monday", "09:00", "a"), at(5, 8, 59), at(12, 9, 0)},
		{"weekly, at the handoff", weekly("Europe/London", "monday", "09:00", "a"), at(5, 9, 0), at(12, 9, 0)},
		{"weekly, a minute late", weekly("Europe/London", "monday", "09:00", "a"), at(5, 9, 1), at(19, 9, 0)},
		{"two weeks", Definition{TimeZone: "Europe/London", Members: []string{"a"}, ShiftDays: 14, HandoffTime: "09:00", HandoffDay: "friday"}, at(6, 10, 0), at(23, 9, 0)},
		{"three days", Definition{TimeZone: "Europe/London", Members: []string{"a"}, ShiftDays: 3, HandoffTime: "18:00"}, at(6, 10, 0), at(9, 18, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Start(tc.def, tc.start, everyone)
			if err != nil {
				t.Fatal(err)
			}
			if !s.NextHandoff.Equal(tc.want) || !s.ShiftStart.Equal(tc.start) || s.Member != "a" {
				t.Errorf("Start = %s on call from %s to %s, want a until %s", s.Member, s.ShiftStart, s.NextHandoff.In(london), tc.want)
			}
		})
	}
}

// TestRotationOrder checks who takes over: the next member in order, round
// and round, passing over the ones who are not users.
func TestRotationOrder(t *testing.T) {
	def := daily("UTC", "09:00", "ada", "grace", "linus", "ken")
	order := func(known func(string) bool, n int) string {
		s, err := Start(def, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), known)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{s.Member}
		for range n {
			if s, _, err = s.Next(known); err != nil {
				t.Fatal(err)
			}
			out = append(out, s.Member)
		}
		return strings.Join(out, " ")
	}
	if got, want := order(everyone, 5), "ada grace linus ken ada grace"; got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
	without := func(missing ...string) func(string) bool {
		return func(m string) bool { return !slices.Contains(missing, m) }
	}
	if got, want := order(without("ada", "linus"), 3), "grace ken grace ken"; got != want {
		t.Errorf("order without ada and linus = %s, want %s", got, want)
	}
	if got, want := order(without("ada", "grace", "linus"), 2), "ken ken ken"; got != want {
		t.Errorf("order with one user = %s, want %s", got, want)
	}
	if got, want := order(func(string) bool { return false }, 1), " "; got != want {
		t.Errorf("order with no users = %q, want nobody", got)
	}
}

// TestCompileRefusesWhatWouldNeverEnd checks that a definition read back
// from the database is not trusted.
func TestCompileRefusesWhatWouldNeverEnd(t *testing.T) {
	for _, d := range []Definition{
		{TimeZone: "Nowhere/Land", ShiftDays: 1, HandoffTime: "09:00"},
		{TimeZone: "", ShiftDays: 1, HandoffTime: "09:00"},
		{TimeZone: "UTC", ShiftDays: 0, HandoffTime: "09:00"},
		{TimeZone: "UTC", ShiftDays: 1, HandoffTime: "25:00"},
		{TimeZone: "UTC", ShiftDays: 1, HandoffTime: "0900"},
		{TimeZone: "UTC", ShiftDays: 7, HandoffTime: "09:00", HandoffDay: "someday"},
		{TimeZone: "UTC", ShiftDays: 3, HandoffTime: "09:00", HandoffDay: "monday"},
	} {
		if _, err := Start(d, time.Now(), everyone); err == nil {
			t.Errorf("Start(%+v) succeeded", d)
		}
	}
}
