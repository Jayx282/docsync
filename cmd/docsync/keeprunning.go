package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
)

// dsnFileEnv names a file holding the connection string. Windows services use it so a
// password lives in a file only the service account and administrators can read,
// rather than in the service's settings.
const dsnFileEnv = "DOCSYNC_DSN_FILE"

// keepRunning calls attempt until ctx ends, starting it again after every failure
// with a growing pause (5 s doubling to 2 min; back to 5 s after a long healthy run).
// A service must outlive SQL Server restarts, reboots and network trouble.
func keepRunning(ctx context.Context, attempt func(context.Context) error, log *slog.Logger,
	onRetry func(err error, wait time.Duration), after func(time.Duration) <-chan time.Time) {
	if after == nil {
		after = time.After
	}
	wait := 5 * time.Second
	for {
		start := time.Now()
		err := attempt(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 10*time.Minute {
			wait = 5 * time.Second
		}
		if err == nil {
			err = errors.New("the pipeline stopped unexpectedly")
		}
		log.Error("docsync stopped; starting it again", "error", err, "retry_in", wait.String())
		if onRetry != nil {
			onRetry(err, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-after(wait):
		}
		wait = min(wait*2, 2*time.Minute)
	}
}

// loadForService reads the config and, when the service keeps its connection string
// in a file, puts it where the config expects it.
func loadForService(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if p := os.Getenv(dsnFileEnv); p != "" && cfg.Source.DSN == "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dsnFileEnv, err)
		}
		if err := os.Setenv(dsnEnvName(cfg), strings.TrimSpace(string(b))); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// dsnEnvName is the environment variable the config reads its connection string from.
func dsnEnvName(cfg *config.Config) string {
	if cfg.Source.DSNEnv != "" {
		return cfg.Source.DSNEnv
	}
	return "DOCSYNC_DSN"
}

// dsnHasPassword reports whether a connection string carries a password (URL or
// key=value form).
func dsnHasPassword(dsn string) bool {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if _, ok := u.User.Password(); ok {
			return true
		}
		for k, v := range u.Query() {
			if (strings.EqualFold(k, "password") || strings.EqualFold(k, "pwd")) && len(v) > 0 && v[0] != "" {
				return true
			}
		}
		return false
	}
	for _, part := range strings.Split(dsn, ";") {
		k, v, ok := strings.Cut(part, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if ok && (k == "password" || k == "pwd") && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// defaultServiceName is the Windows service name for a config: docsync-<document>.
func defaultServiceName(cfg *config.Config) string {
	return "docsync-" + cfg.Document.Name
}
