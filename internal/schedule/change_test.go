package schedule

import (
	"strings"
	"testing"
	"time"
)

// london is where the rotations of these tests hand over.
var london = func() *time.Location {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		panic(err)
	}
	return loc
}()

// jan returns a time in January 2026 in London. The 5th is a Monday.
func jan(day, hour, minute int) time.Time {
	return time.Date(2026, 1, day, hour, minute, 0, 0, london)
}

// spansText writes spans as "who day hh:mm..day hh:mm", in London.
func spansText(spans []Span) string {
	var out []string
	for _, s := range spans {
		who := s.Member
		if who == "" {
			who = "nobody"
		}
		out = append(out, who+" "+s.Start.In(london).Format("Jan 2 15:04")+".."+s.End.In(london).Format("Jan 2 15:04"))
	}
	return strings.Join(out, "\n")
}

func project(t *testing.T, s State, until time.Time) string {
	t.Helper()
	spans, err := s.Project(until, everyone)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	return spansText(spans)
}

// mondays is a weekly rotation that hands over on Monday at 09:00, with
// who on call since Monday 5 January.
func mondays(who string, members ...string) State {
	return State{Member: who, ShiftStart: jan(5, 9, 0), NextHandoff: jan(12, 9, 0), Definition: weekly("Europe/London", "monday", "09:00", members...)}
}

func lines(s ...string) string { return strings.Join(s, "\n") }

// TestChangeTakesOverAtTheNextHandoff changes the members of a rotation in
// the middle of a shift, in every way the list can change. The shift in
// progress always ends when it would have, and from then on the new order
// applies, continued from the person who was on call: everyone else keeps
// their place relative to each other.
func TestChangeTakesOverAtTheNextHandoff(t *testing.T) {
	now := jan(7, 15, 0)
	for _, tc := range []struct {
		name   string
		before State
		after  []string
		// want is who is on call, shift by shift, from the one in progress.
		want string
	}{
		{"unchanged order, for reference", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "linus", "ken"}, "grace linus ken ada grace linus"},
		{"a member is added after the one on call", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "new", "linus", "ken"}, "grace new linus ken ada grace"},
		{"a member is added before the one on call", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "new", "grace", "linus", "ken"}, "grace linus ken ada new grace"},
		{"a member is added at the end", mondays("ken", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "linus", "ken", "new"}, "ken new ada grace linus ken"},
		{"another member is removed", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "ken"}, "grace ken ada grace ken ada"},
		{"the next member is removed", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "ken"}, "grace ken ada grace ken ada"},
		{"the one on call is removed", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "linus", "ken"}, "grace linus ken ada linus ken"},
		{"the last, who is on call, is removed", mondays("ken", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "linus"}, "ken ada grace linus ada grace"},
		{"the one on call and the next are removed", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "ken"}, "grace ken ada ken ada ken"},
		{"two are swapped", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ada", "grace", "ken", "linus"}, "grace ken linus ada grace ken"},
		{"the order is reversed", mondays("grace", "ada", "grace", "linus", "ken"), []string{"ken", "linus", "grace", "ada"}, "grace ada ken linus grace ada"},
		{"everyone is replaced", mondays("grace", "ada", "grace"), []string{"x", "y"}, "grace x y x y x"},
		{"one is added and one removed", mondays("linus", "ada", "grace", "linus", "ken"), []string{"ada", "new", "linus", "ken"}, "linus ken ada new linus ken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.before.WithDefinition(weekly("Europe/London", "monday", "09:00", tc.after...), nil, now)
			spans, err := s.Project(jan(5, 9, 0).AddDate(0, 0, 7*5+1), everyone)
			if err != nil {
				t.Fatal(err)
			}
			var who []string
			for i, span := range spans {
				who = append(who, span.Member)
				// Every shift is a week, Monday 09:00 to Monday 09:00,
				// beginning with the one in progress, which is not touched.
				if want := jan(5+7*i, 9, 0); !span.Start.Equal(want) || !span.End.Equal(want.AddDate(0, 0, 7)) {
					t.Errorf("shift %d is %s..%s, want a week from %s", i, span.Start, span.End, want)
				}
			}
			if got := strings.Join(who, " "); got != tc.want {
				t.Errorf("on call:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestChangeOfHandoffTime moves the handoff of a running rotation. At the
// next handoff, which is still the old one, the next person takes over, and
// their first shift under the new rotation is never short.
func TestChangeOfHandoffTime(t *testing.T) {
	s := mondays("ada", "ada", "grace", "linus").WithDefinition(weekly("Europe/London", "friday", "17:00", "ada", "grace", "linus"), nil, jan(7, 15, 0))
	want := lines(
		"ada Jan 5 09:00..Jan 12 09:00",
		// Friday the 16th would leave Grace four days; the 23rd is the
		// first handoff a full shift away.
		"grace Jan 12 09:00..Jan 23 17:00",
		"linus Jan 23 17:00..Jan 30 17:00",
		"ada Jan 30 17:00..Feb 6 17:00",
	)
	if got := project(t, s, jan(31, 0, 0)); got != want {
		t.Errorf("shifts:\n%s\nwant:\n%s", got, want)
	}

	// From weekly to daily: the same rule, and then a day at a time.
	s = mondays("ada", "ada", "grace").WithDefinition(daily("Europe/London", "09:00", "ada", "grace"), nil, jan(7, 15, 0))
	want = lines("ada Jan 5 09:00..Jan 12 09:00", "grace Jan 12 09:00..Jan 13 09:00", "ada Jan 13 09:00..Jan 14 09:00")
	if got := project(t, s, jan(14, 0, 0)); got != want {
		t.Errorf("weekly to daily:\n%s\nwant:\n%s", got, want)
	}

	// To another time zone: 09:00 in Sydney is 22:00 the evening before in
	// London, in January.
	s = mondays("ada", "ada", "grace").WithDefinition(weekly("Australia/Sydney", "monday", "09:00", "ada", "grace"), nil, jan(7, 15, 0))
	want = lines("ada Jan 5 09:00..Jan 12 09:00", "grace Jan 12 09:00..Jan 25 22:00", "ada Jan 25 22:00..Feb 1 22:00")
	if got := project(t, s, jan(26, 0, 0)); got != want {
		t.Errorf("to another zone:\n%s\nwant:\n%s", got, want)
	}
}

// TestChangeAtAStatedTime gives changes a time at which to take over.
func TestChangeAtAStatedTime(t *testing.T) {
	now := jan(6, 12, 0)
	stated := func(day, hour int) *time.Time {
		at := jan(day, hour, 0)
		return &at
	}
	for _, tc := range []struct {
		name   string
		before State
		def    Definition
		at     *time.Time
		until  time.Time
		want   string
	}{
		{
			// The time is in the middle of Ada's shift. She keeps it; only
			// its end moves, to the first handoff of the new rotation.
			name: "in the middle of a shift, the person on call stays", before: mondays("ada", "ada", "grace", "linus"),
			def: weekly("Europe/London", "friday", "17:00", "ada", "linus", "grace"), at: stated(7, 12), until: jan(24, 0, 0),
			want: lines("ada Jan 5 09:00..Jan 9 17:00", "linus Jan 9 17:00..Jan 16 17:00", "grace Jan 16 17:00..Jan 23 17:00", "ada Jan 23 17:00..Jan 30 17:00"),
		},
		{
			// Only the members change: the shift is not touched at all.
			name: "in the middle of a shift, same handoffs", before: mondays("ada", "ada", "grace", "linus"),
			def: weekly("Europe/London", "monday", "09:00", "ada", "linus"), at: stated(7, 12), until: jan(20, 0, 0),
			want: lines("ada Jan 5 09:00..Jan 12 09:00", "linus Jan 12 09:00..Jan 19 09:00", "ada Jan 19 09:00..Jan 26 09:00"),
		},
		{
			// The new rotation does not have the person on call. This is
			// the one case in which a shift ends early: whoever follows
			// takes the rest of it.
			name: "the person on call is removed", before: mondays("grace", "ada", "grace", "linus"),
			def: weekly("Europe/London", "monday", "09:00", "ada", "linus"), at: stated(7, 12), until: jan(20, 0, 0),
			want: lines("grace Jan 5 09:00..Jan 7 12:00", "linus Jan 7 12:00..Jan 12 09:00", "ada Jan 12 09:00..Jan 19 09:00", "linus Jan 19 09:00..Jan 26 09:00"),
		},
		{
			// Until the time comes, the rotation in force hands over as
			// before.
			name: "after several handoffs", before: mondays("ada", "ada", "grace", "linus"),
			def: weekly("Europe/London", "monday", "09:00", "linus", "grace", "ada"), at: stated(21, 12), until: jan(31, 0, 0),
			want: lines("ada Jan 5 09:00..Jan 12 09:00", "grace Jan 12 09:00..Jan 19 09:00", "linus Jan 19 09:00..Jan 26 09:00", "grace Jan 26 09:00..Feb 2 09:00"),
		},
		{
			// A time that is a handoff is a handoff.
			name: "exactly at a handoff", before: mondays("ada", "ada", "grace", "linus"),
			def: weekly("Europe/London", "monday", "09:00", "ada", "linus", "grace"), at: stated(12, 9), until: jan(20, 0, 0),
			want: lines("ada Jan 5 09:00..Jan 12 09:00", "linus Jan 12 09:00..Jan 19 09:00", "grace Jan 19 09:00..Jan 26 09:00"),
		},
		{
			// A time that has passed means now.
			name: "in the past", before: mondays("ada", "ada", "grace", "linus"),
			def: weekly("Europe/London", "friday", "17:00", "ada", "linus", "grace"), at: stated(2, 12), until: jan(10, 0, 0),
			want: lines("ada Jan 5 09:00..Jan 9 17:00", "linus Jan 9 17:00..Jan 16 17:00"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.before.WithDefinition(tc.def, tc.at, now)
			if got := project(t, s, tc.until); got != tc.want {
				t.Errorf("shifts:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestChangeThatIsTakenBack returns the files to the rotation in force
// before a change took over. Nothing is pending then.
func TestChangeThatIsTakenBack(t *testing.T) {
	now := jan(7, 15, 0)
	original := weekly("Europe/London", "monday", "09:00", "ada", "grace")
	s := mondays("ada", "ada", "grace").WithDefinition(weekly("Europe/London", "monday", "09:00", "grace", "ada", "linus"), nil, now)
	if s.Pending == nil {
		t.Fatal("the change is not pending")
	}
	if s = s.WithDefinition(original, nil, now); s.Pending != nil || s.PendingAt != nil {
		t.Errorf("after taking the change back, %+v is pending", s.Pending)
	}
	// A stated time alone changes nothing.
	at := jan(9, 9, 0)
	if s = s.WithDefinition(original, &at, now); s.Pending != nil || s.PendingAt != nil {
		t.Errorf("a stated time without a change left %+v pending at %v", s.Pending, s.PendingAt)
	}
}
