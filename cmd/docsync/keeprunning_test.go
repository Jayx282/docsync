package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Jayx282/docsync/examples/stones"
	"github.com/Jayx282/docsync/internal/config"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestKeepRunningRetriesWithBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var waits []time.Duration
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	attempts, retried := 0, 0
	attempt := func(ctx context.Context) error {
		attempts++
		switch {
		case attempts <= 8:
			return errors.New("cannot reach SQL Server")
		case attempts == 9:
			return nil // returning without being asked to stop counts as a failure too
		}
		cancel() // the service is asked to stop while the pipeline runs
		<-ctx.Done()
		return ctx.Err()
	}
	keepRunning(ctx, attempt, quiet(), func(error, time.Duration) { retried++ }, after)
	want := []time.Duration{5, 10, 20, 40, 80, 120, 120, 120, 120}
	for i := range want {
		want[i] *= time.Second
	}
	if attempts != 10 || retried != 9 || !reflect.DeepEqual(waits, want) {
		t.Fatalf("attempts %d, retries %d, waits %v", attempts, retried, waits)
	}
}

func TestKeepRunningStopsWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	never := func(time.Duration) <-chan time.Time { cancel(); return make(chan time.Time) }
	done := make(chan struct{})
	go func() {
		keepRunning(ctx, func(context.Context) error { return errors.New("down") }, quiet(), nil, never)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stop request during the pause must end the loop")
	}
}

func TestLoadForServiceReadsTheConnectionFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "docsync.yaml")
	if err := os.WriteFile(cfgPath, stones.Config, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "dsn.secret")
	if err := os.WriteFile(secret, []byte("sqlserver://docsync:s3cret@db?database=stones\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCSYNC_DSN", "")
	t.Setenv(dsnFileEnv, secret)
	cfg, err := loadForService(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cfg.DSNValue(); err != nil || got != "sqlserver://docsync:s3cret@db?database=stones" {
		t.Fatalf("connection string from the file: %q %v", got, err)
	}
	t.Setenv(dsnFileEnv, filepath.Join(dir, "missing"))
	if _, err := loadForService(cfgPath); err == nil {
		t.Fatal("a missing connection file is an error")
	}
}

func TestDSNHasPassword(t *testing.T) {
	for dsn, want := range map[string]bool{
		"sqlserver://docsync:pw@host:1433?database=stones":   true,
		"sqlserver://docsync@host?database=stones":           false,
		"sqlserver://localhost?database=stones&password=pw":  true,
		"sqlserver://localhost?database=stones&encrypt=true": false,
		"server=localhost;user id=sa;password=pw;database=x": true,
		"Server=.;Pwd=pw": true,
		"server=localhost;database=x;integrated security=true": false,
	} {
		if got := dsnHasPassword(dsn); got != want {
			t.Errorf("%s: got %v", dsn, got)
		}
	}
}

func TestDefaultServiceName(t *testing.T) {
	cfg, err := config.Parse(stones.Config)
	if err != nil {
		t.Fatal(err)
	}
	if got := defaultServiceName(cfg); got != "docsync-stone" {
		t.Fatalf("service name: %s", got)
	}
}
