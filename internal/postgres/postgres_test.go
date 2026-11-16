package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thanoslabs/beacon/internal/postgres"
	"github.com/thanoslabs/beacon/internal/postgres/postgrestest"
)

func TestOpenRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     postgres.Config
		wantErr string
	}{
		{name: "empty URL", cfg: postgres.Config{}, wantErr: "database URL is not set"},
		{name: "negative pool size", cfg: postgres.Config{URL: "postgres://localhost/beacon", MaxConns: -1}, wantErr: "must not be negative"},
		{name: "malformed URL", cfg: postgres.Config{URL: "postgres://beacon:hunter2@localhost:notaport/beacon"}, wantErr: "parse database URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := postgres.Open(context.Background(), tt.cfg)
			if err == nil {
				pool.Close()
				t.Fatal("Open succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error leaks the password: %q", err)
			}
		})
	}
}

func TestOpenFailsWhenDatabaseIsUnreachable(t *testing.T) {
	// Port 1 on localhost refuses connections.
	_, err := postgres.Open(context.Background(), postgres.Config{
		URL:            "postgres://beacon:hunter2@127.0.0.1:1/beacon?sslmode=disable",
		ConnectTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("Open succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "connect to database") {
		t.Errorf("error = %q, want it to say the connection failed", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the password: %q", err)
	}
}

func TestOpenAppliesMaxConns(t *testing.T) {
	dbURL := postgrestest.NewURL(t)

	pool, err := postgres.Open(context.Background(), postgres.Config{URL: dbURL, MaxConns: 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	if got := pool.Config().MaxConns; got != 3 {
		t.Errorf("MaxConns = %d, want 3", got)
	}
	var one int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Errorf("SELECT 1 = %d, %v", one, err)
	}
}
