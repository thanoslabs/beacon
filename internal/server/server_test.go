package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/logging"
)

// fakeDB is a Pinger whose answer the test controls.
type fakeDB struct {
	err error
}

func (f *fakeDB) Ping(ctx context.Context) error {
	if f.err != nil {
		return f.err
	}
	return ctx.Err()
}

// syncBuffer is a bytes.Buffer that is safe to write from server goroutines
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logRecords parses JSON log output into one map per record.
func logRecords(t *testing.T, out string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		records = append(records, rec)
	}
	return records
}

func newTestServer(t *testing.T, db Pinger) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	logger, err := logging.New(logs, "debug", logging.FormatJSON)
	if err != nil {
		t.Fatalf("logging.New: %v", err)
	}
	return New(Config{Addr: "127.0.0.1:0"}, logger, db, NewRegistry()), logs
}

func do(t *testing.T, s *Server, method, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) statusResponse {
	t.Helper()
	var body statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestLiveness(t *testing.T) {
	// Liveness must not depend on the database.
	s, _ := newTestServer(t, &fakeDB{err: errors.New("database is down")})

	rec := do(t, s, http.MethodGet, PathLiveness, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if body := decodeStatus(t, rec); body.Status != "ok" {
		t.Errorf("body = %+v, want status ok", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestReadiness(t *testing.T) {
	t.Run("database reachable", func(t *testing.T) {
		s, _ := newTestServer(t, &fakeDB{})
		rec := do(t, s, http.MethodGet, PathReadiness, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if body := decodeStatus(t, rec); body.Status != "ok" || body.Checks["database"] != "ok" {
			t.Errorf("body = %+v, want status ok and database ok", body)
		}
	})

	t.Run("database unreachable", func(t *testing.T) {
		s, logs := newTestServer(t, &fakeDB{err: errors.New("dial tcp 10.0.0.5:5432: connection refused for user secret-user")})
		rec := do(t, s, http.MethodGet, PathReadiness, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if body := decodeStatus(t, rec); body.Status != "unavailable" || body.Checks["database"] != "unreachable" {
			t.Errorf("body = %+v, want unavailable and database unreachable", body)
		}
		if strings.Contains(rec.Body.String(), "secret-user") {
			t.Errorf("response leaks the database error: %q", rec.Body.String())
		}
		if !strings.Contains(logs.String(), "secret-user") {
			t.Errorf("the cause of the failed check was not logged: %q", logs.String())
		}
	})

	t.Run("shutting down", func(t *testing.T) {
		s, _ := newTestServer(t, &fakeDB{})
		s.stopping.Store(true)
		rec := do(t, s, http.MethodGet, PathReadiness, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 while shutting down", rec.Code)
		}
	})
}

func TestMetrics(t *testing.T) {
	s, _ := newTestServer(t, &fakeDB{})

	rec := do(t, s, http.MethodGet, PathMetrics, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	want := []string{"go_goroutines ", "go_memstats_alloc_bytes ", "beacon_build_info{"}
	// The process collector reads /proc, so it only reports on Linux.
	if runtime.GOOS == "linux" {
		want = append(want, "process_cpu_seconds_total ", "process_resident_memory_bytes ")
	}
	for _, metric := range want {
		if !strings.Contains(body, "\n"+metric) {
			t.Errorf("metrics output has no %q", metric)
		}
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	s, _ := newTestServer(t, &fakeDB{})
	if rec := do(t, s, http.MethodGet, "/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, PathLiveness, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %s = %d, want 405", PathLiveness, rec.Code)
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		sent     string
		wantKept bool
	}{
		{name: "generated when absent", sent: "", wantKept: false},
		{name: "kept when safe", sent: "abc-123_DEF.4", wantKept: true},
		{name: "replaced when it has unsafe characters", sent: "abc\"} injected", wantKept: false},
		{name: "replaced when too long", sent: strings.Repeat("a", 65), wantKept: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t, &fakeDB{})
			s.Mux().HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "hello")
			})

			header := http.Header{}
			if tt.sent != "" {
				header.Set(RequestIDHeader, tt.sent)
			}
			rec := do(t, s, http.MethodGet, "/hello", header)

			got := rec.Header().Get(RequestIDHeader)
			if tt.wantKept && got != tt.sent {
				t.Errorf("response identifier = %q, want the one sent, %q", got, tt.sent)
			}
			if !tt.wantKept {
				if got == tt.sent || len(got) != 32 {
					t.Errorf("response identifier = %q, want a generated 32-character one", got)
				}
			}

			// The access log record carries the same identifier.
			var found bool
			for _, rec := range logRecords(t, logs.String()) {
				if rec["msg"] == "http request" {
					found = true
					if rec["request_id"] != got {
						t.Errorf("logged request_id = %v, want %q", rec["request_id"], got)
					}
					if rec["path"] != "/hello" || rec["status"] != float64(200) || rec["bytes"] != float64(5) || rec["level"] != "INFO" {
						t.Errorf("access log record = %v", rec)
					}
				}
			}
			if !found {
				t.Errorf("no access log record in %q", logs.String())
			}
		})
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	s, _ := newTestServer(t, &fakeDB{})
	a := do(t, s, http.MethodGet, PathLiveness, nil).Header().Get(RequestIDHeader)
	b := do(t, s, http.MethodGet, PathLiveness, nil).Header().Get(RequestIDHeader)
	if a == "" || a == b {
		t.Errorf("identifiers %q and %q, want two different ones", a, b)
	}
}

func TestProbesAreLoggedAtDebug(t *testing.T) {
	s, logs := newTestServer(t, &fakeDB{})
	do(t, s, http.MethodGet, PathLiveness, nil)
	for _, rec := range logRecords(t, logs.String()) {
		if rec["msg"] == "http request" && rec["level"] != "DEBUG" {
			t.Errorf("probe request logged at %v, want DEBUG", rec["level"])
		}
	}
}

func TestPanicIsRecovered(t *testing.T) {
	s, logs := newTestServer(t, &fakeDB{})
	s.Mux().HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})

	rec := do(t, s, http.MethodGet, "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("response leaks the panic value: %q", rec.Body.String())
	}

	var sawPanic, sawAccess bool
	for _, r := range logRecords(t, logs.String()) {
		switch r["msg"] {
		case "panic in http handler":
			sawPanic = r["panic"] == "kaboom" && r["request_id"] != nil
		case "http request":
			sawAccess = r["status"] == float64(500)
		}
	}
	if !sawPanic || !sawAccess {
		t.Errorf("want the panic and a 500 access record logged, got %q", logs.String())
	}
}

// startServer runs the server on a free port and returns its base URL, a
// function that triggers shutdown, and a channel with the result of Serve.
func startServer(t *testing.T, s *Server) (baseURL string, stop context.CancelFunc, done <-chan error) {
	t.Helper()
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- s.Serve(ctx) }()
	t.Cleanup(cancel)
	return "http://" + s.Addr().String(), cancel, result
}

func TestGracefulShutdownWaitsForInFlightRequests(t *testing.T) {
	s, logs := newTestServer(t, &fakeDB{})
	started := make(chan struct{})
	release := make(chan struct{})
	s.Mux().HandleFunc("GET /slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	baseURL, stop, done := startServer(t, s)

	type result struct {
		body string
		err  error
	}
	slow := make(chan result, 1)
	go func() {
		resp, err := http.Get(baseURL + "/slow")
		if err != nil {
			slow <- result{err: err}
			return
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		b, err := io.ReadAll(resp.Body)
		slow <- result{body: string(b), err: err}
	}()

	<-started
	stop() // what a termination signal does

	// Serve must not return while the request is still running.
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) before the in-flight request finished", err)
	case <-time.After(200 * time.Millisecond):
	}

	// New connections are refused once shutdown has begun.
	if resp, err := http.Get(baseURL + PathLiveness); err == nil {
		_ = resp.Body.Close()
		t.Error("a new connection was accepted during shutdown")
	}

	close(release)
	if r := <-slow; r.err != nil || r.body != "finished" {
		t.Errorf("in-flight request: body %q, err %v; want it to finish normally", r.body, r.err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the in-flight request finished")
	}
	if !strings.Contains(logs.String(), "http server stopped") {
		t.Errorf("shutdown was not logged: %q", logs.String())
	}
}

func TestShutdownTimeoutCutsOffStuckRequests(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	s := New(Config{Addr: "127.0.0.1:0", ShutdownTimeout: 100 * time.Millisecond}, logger, &fakeDB{}, NewRegistry())
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	s.Mux().HandleFunc("GET /stuck", func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	})
	baseURL, stop, done := startServer(t, s)

	go func() {
		resp, err := http.Get(baseURL + "/stuck")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	stop()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Serve = %v, want a deadline exceeded error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not give up after the shutdown timeout")
	}
}

func TestListenFailsOnBadAddress(t *testing.T) {
	s := New(Config{Addr: "not-an-address"}, slog.New(slog.DiscardHandler), &fakeDB{}, NewRegistry())
	if err := s.Listen(); err == nil || !strings.Contains(err.Error(), "listen on not-an-address") {
		t.Errorf("Listen = %v, want an error naming the address", err)
	}
}
