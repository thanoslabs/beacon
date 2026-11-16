package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewRejectsUnknownSettings(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "loud", FormatJSON); err == nil || !strings.Contains(err.Error(), `unknown log level "loud"`) {
		t.Errorf("unknown level: err = %v", err)
	}
	if _, err := New(&bytes.Buffer{}, "info", "xml"); err == nil || !strings.Contains(err.Error(), `unknown log format "xml"`) {
		t.Errorf("unknown format: err = %v", err)
	}
}

func TestRequestIDIsLogged(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, "info", FormatJSON)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := WithRequestID(context.Background(), "req-123")
	// Attributes and groups added to the logger must not lose the identifier.
	logger.With("component", "test").InfoContext(ctx, "with id")
	logger.InfoContext(context.Background(), "without id")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2: %q", len(lines), buf.String())
	}

	var with, without map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &with); err != nil {
		t.Fatalf("first line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &without); err != nil {
		t.Fatalf("second line is not JSON: %v", err)
	}
	if with["request_id"] != "req-123" || with["component"] != "test" {
		t.Errorf("first record = %v, want request_id and component", with)
	}
	if _, ok := without["request_id"]; ok {
		t.Errorf("second record = %v, want no request_id", without)
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(&buf, "warn", FormatText)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("hidden")
	logger.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Errorf("output = %q, want only the warning", out)
	}
}
