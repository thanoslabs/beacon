package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

// lockedBuffer is a buffer that the server can write while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestServeSettingsErrors(t *testing.T) {
	const dbURL = "postgres://localhost/beacon"
	tests := []struct {
		name       string
		environ    map[string]string
		args       []string
		wantStderr string
	}{
		{name: "missing database URL", args: []string{"serve"}, wantStderr: "the database URL is required"},
		{name: "unknown log level", args: []string{"serve", "--database-url", dbURL, "--log-level", "loud"}, wantStderr: `unknown log level "loud"`},
		{name: "unknown log format", environ: map[string]string{"BEACON_LOG_FORMAT": "xml"}, args: []string{"serve", "--database-url", dbURL}, wantStderr: `unknown log format "xml"`},
		{name: "bad duration in environment", environ: map[string]string{"BEACON_SHUTDOWN_TIMEOUT": "soon"}, args: []string{"serve", "--database-url", dbURL}, wantStderr: `BEACON_SHUTDOWN_TIMEOUT: "soon" is not a duration`},
		{name: "bad bool in environment", environ: map[string]string{"BEACON_MIGRATE": "maybe"}, args: []string{"serve", "--database-url", dbURL}, wantStderr: `BEACON_MIGRATE: "maybe" is not true or false`},
		{name: "zero shutdown timeout", args: []string{"serve", "--database-url", dbURL, "--shutdown-timeout", "0s"}, wantStderr: "--shutdown-timeout must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLIEnv(t, tt.environ, tt.args...)
			if code != exitError {
				t.Errorf("exit code = %d, want %d", code, exitError)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

// TestServe runs the whole server against a real database: it migrates on
// start, answers the operational endpoints, and stops cleanly when its
// context is cancelled, which is what SIGTERM does in the real binary.
func TestServe(t *testing.T) {
	dbURL := postgrestest.NewURL(t)
	environ := map[string]string{
		"BEACON_DATABASE_URL":   dbURL,
		"BEACON_LISTEN_ADDRESS": "127.0.0.1:0",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &lockedBuffer{}
	exit := make(chan int, 1)
	go func() {
		exit <- Run(ctx, []string{"serve"}, func(k string) string { return environ[k] }, io.Discard, logs)
	}()

	// The bound address is in the "http server listening" log record.
	var baseURL string
	deadline := time.Now().Add(15 * time.Second)
	for baseURL == "" {
		select {
		case code := <-exit:
			t.Fatalf("serve exited early with code %d: %s", code, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start listening: %s", logs.String())
		}
		for line := range strings.SplitSeq(logs.String(), "\n") {
			var rec struct {
				Msg  string `json:"msg"`
				Addr string `json:"addr"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "http server listening" {
				baseURL = "http://" + rec.Addr
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close() //nolint:errcheck // test cleanup
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.Header.Get("X-Request-Id") == "" {
			t.Errorf("GET %s: no X-Request-Id response header", path)
		}
		return resp.StatusCode, string(body)
	}

	if code, body := get("/healthz"); code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("GET /healthz = %d %q", code, body)
	}
	if code, body := get("/readyz"); code != http.StatusOK || !strings.Contains(body, `"database":"ok"`) {
		t.Errorf("GET /readyz = %d %q", code, body)
	}
	if code, body := get("/metrics"); code != http.StatusOK || !strings.Contains(body, "go_goroutines") || !strings.Contains(body, "beacon_build_info") {
		t.Errorf("GET /metrics = %d, missing expected metrics", code)
	}

	cancel()
	select {
	case code := <-exit:
		if code != exitOK {
			t.Errorf("exit code after shutdown = %d, want %d: %s", code, exitOK, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("serve did not stop after its context was cancelled: %s", logs.String())
	}

	out := logs.String()
	for _, want := range []string{"applied migration", "00001_create_users.sql", "database schema is up to date", "beacon stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("logs do not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "beacon:beacon") {
		t.Errorf("logs contain database credentials:\n%s", out)
	}
}

func TestServeFailsWhenDatabaseIsUnreachable(t *testing.T) {
	code, _, stderr := runCLIEnv(t, nil, "serve",
		"--database-url", "postgres://beacon:hunter2@127.0.0.1:1/beacon?sslmode=disable",
		"--database-connect-timeout", "2s",
		"--listen-address", "127.0.0.1:0",
	)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "connect to database") {
		t.Errorf("stderr = %q, want it to say the connection failed", stderr)
	}
	if strings.Contains(stderr, "hunter2") {
		t.Errorf("stderr leaks the database password: %q", stderr)
	}
}
