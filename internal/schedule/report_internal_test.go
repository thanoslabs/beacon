package schedule

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestGapInCoverageIsAWarning: a handoff to nobody is a state some teams
// choose. The log names it as a gap in coverage, at warning level, and
// never as an error.
func TestGapInCoverageIsAWarning(t *testing.T) {
	var logs bytes.Buffer
	at := time.Date(2026, 1, 3, 9, 0, 0, 0, time.UTC)
	report(context.Background(), slog.New(slog.NewJSONHandler(&logs, nil)), []Handoff{
		{Schedule: "weekdays", From: "ada@example.com", To: "", At: at, Next: at.Add(24 * time.Hour), ShiftBegan: true},
		{Schedule: "primary", From: "ada@example.com", To: "grace@example.com", At: at, Next: at.Add(24 * time.Hour), ShiftBegan: true},
	})
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN","msg":"gap in coverage: the schedule has nobody on call`) || !strings.Contains(out, `"schedule":"weekdays"}`) {
		t.Errorf("the gap is not logged as a warning:\n%s", out)
	}
	if strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a gap in coverage is logged as an error:\n%s", out)
	}
	if strings.Count(out, "gap in coverage") != 1 {
		t.Errorf("the schedule that has someone on call is reported as a gap:\n%s", out)
	}
}
