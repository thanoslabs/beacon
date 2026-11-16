package alert

import (
	"errors"
	"testing"
)

func TestNext(t *testing.T) {
	// Every pair of state and action is listed, so adding a state or an
	// action without deciding its transitions fails this test.
	want := map[Status]map[Action]Status{
		StatusFiring: {
			ActionAcknowledge: StatusAcknowledged,
			ActionSnooze:      StatusSnoozed,
			ActionResolve:     StatusResolved,
			ActionEndSnooze:   "",
		},
		StatusAcknowledged: {
			ActionAcknowledge: "",
			ActionSnooze:      StatusSnoozed,
			ActionResolve:     StatusResolved,
			ActionEndSnooze:   "",
		},
		StatusSnoozed: {
			ActionAcknowledge: StatusAcknowledged,
			ActionSnooze:      "",
			ActionResolve:     StatusResolved,
			ActionEndSnooze:   StatusFiring,
		},
		StatusResolved: {
			ActionAcknowledge: "",
			ActionSnooze:      "",
			ActionResolve:     "",
			ActionEndSnooze:   "",
		},
	}
	if len(want) != len(transitions) {
		t.Fatalf("test covers %d states, the state machine has %d", len(want), len(transitions))
	}
	for from, actions := range want {
		if len(actions) != len(eventFor) {
			t.Fatalf("%s: test covers %d actions, there are %d", from, len(actions), len(eventFor))
		}
		for action, wantTo := range actions {
			t.Run(string(from)+"/"+string(action), func(t *testing.T) {
				to, err := Next(from, action)
				if wantTo == "" {
					var te *TransitionError
					if !errors.As(err, &te) {
						t.Fatalf("Next = %q, %v; want a TransitionError", to, err)
					}
					if te.From != from || te.Action != action {
						t.Errorf("TransitionError = %+v, want from %s action %s", te, from, action)
					}
					return
				}
				if err != nil || to != wantTo {
					t.Errorf("Next = %q, %v; want %q", to, err, wantTo)
				}
			})
		}
	}
}

func TestNextUnknown(t *testing.T) {
	if _, err := Next("paused", ActionResolve); err == nil {
		t.Error("Next from an unknown state returned no error")
	}
	if _, err := Next(StatusFiring, "delete"); err == nil {
		t.Error("Next with an unknown action returned no error")
	}
}

func TestTransitionErrorMessage(t *testing.T) {
	_, err := Next(StatusResolved, ActionAcknowledge)
	if got, want := err.Error(), "cannot acknowledge an alert that is resolved"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestSeverityValid(t *testing.T) {
	for _, s := range []Severity{SeverityCritical, SeverityWarning, SeverityInfo} {
		if !s.Valid() {
			t.Errorf("%q is not valid", s)
		}
	}
	for _, s := range []Severity{"", "page", "Critical"} {
		if s.Valid() {
			t.Errorf("%q is valid", s)
		}
	}
}
