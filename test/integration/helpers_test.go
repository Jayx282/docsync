//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/engine"
	"github.com/Jayx282/docsync/internal/testkit"
)

var errInjected = errors.New("injected fault")

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func serverDSN(t *testing.T) string {
	dsn := os.Getenv("DOCSYNC_TEST_DSN")
	if dsn == "" {
		t.Skip("DOCSYNC_TEST_DSN is not set")
	}
	return dsn
}

func seed(t *testing.T) int64 {
	if s := os.Getenv("DOCSYNC_TEST_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	return time.Now().UnixNano()
}

func rounds(def int) int {
	if v := os.Getenv("DOCSYNC_TEST_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

type fixture struct {
	name string // database name
	dsn  string // DSN of the scratch database
	db   *sql.DB
	gen  *testkit.Stones
}

// newFixture creates a scratch database with the example schema and seed data.
func newFixture(t *testing.T, seedValue int64, stones int) *fixture {
	t.Helper()
	name, dsn := testkit.CreateDatabase(t, serverDSN(t))
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testkit.ApplySQLFile(t, db, filepath.Join(repoRoot(), "examples", "stones", "schema.sql"))
	gen := testkit.NewStones(db, seedValue)
	if err := gen.Seed(context.Background(), stones, 6, stones/4); err != nil {
		t.Fatal(err)
	}
	return &fixture{name: name, dsn: dsn, db: db, gen: gen}
}

// testConfig loads the example config and tunes it for tests (see tuneForTest).
func testConfig(t *testing.T, dsn, dir string, budget int, mutate func(*config.Config)) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(repoRoot(), "examples", "stones", "docsync.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tuneForTest(cfg, dsn, dir, budget)
	if mutate != nil {
		mutate(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// tuneForTest points a config at a scratch database and directory, with tiny budgets
// and fan-out limits so the pending set is exercised, and NDJSON plus Parquet sinks.
func tuneForTest(cfg *config.Config, dsn, dir string, budget int) {
	cfg.Source.DSN, cfg.Source.DSNEnv = dsn, ""
	cfg.Source.AssembleBudget = budget
	cfg.Source.PollInterval.Duration = 50 * time.Millisecond
	for i := range cfg.Routing {
		if cfg.Routing[i].HotFanoutLimit > 0 {
			cfg.Routing[i].HotFanoutLimit = 15
		}
	}
	cfg.Sinks = []config.Sink{
		{Name: "ndjson", Type: config.SinkNDJSON, Dir: filepath.Join(dir, "out", "ndjson")},
		{Name: "parquet", Type: config.SinkParquetLog, Dir: filepath.Join(dir, "out", "parquet"), IncludeJSON: true},
	}
	for i := range cfg.Sinks {
		s := &cfg.Sinks[i]
		s.SegmentInterval.Duration = time.Millisecond
		s.BaseEvery.Duration = time.Hour
		s.RetainSegments.Duration = time.Hour
		s.BaseFileRows = 97
	}
	cfg.State.Dir = filepath.Join(dir, "state")
	cfg.State.OutboxRetention.Duration = time.Hour
}

func openEngine(t *testing.T, cfg *config.Config, faults engine.FaultHook) *engine.Engine {
	t.Helper()
	e, err := engine.Open(context.Background(), engine.Options{Config: cfg, Faults: faults})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	return e
}

// catchUp steps the engine until everything is captured, assembled and published.
func catchUp(t *testing.T, ctx context.Context, e *engine.Engine) {
	t.Helper()
	for i := 0; i < 20000; i++ {
		res, err := e.Step(ctx)
		if err != nil {
			t.Fatalf("catch-up step: %v", err)
		}
		v := e.Status()
		if v.Paused {
			t.Fatalf("engine paused: %s", v.PauseReason)
		}
		if res.Changes > 0 || res.MoreWork || v.Pending > 0 || v.Checkpoint.Snapshotting {
			continue
		}
		done := true
		for _, s := range v.Sinks {
			if !s.HasBase || s.Lag > 0 || s.BaseInFlight {
				done = false
			}
		}
		if done {
			return
		}
		e.WaitForBases()
	}
	t.Fatal("engine did not catch up")
}

// fullRebuild runs a fresh engine (empty state and sinks) on the example config.
func fullRebuild(t *testing.T, ctx context.Context, dsn string, mutate func(*config.Config)) *testkit.Folded {
	t.Helper()
	return rebuildWith(t, ctx, func(dir string) *config.Config { return testConfig(t, dsn, dir, 500, mutate) })
}

// rebuildWith runs a fresh engine (empty state and sinks) on the config mk builds for
// a new directory, and returns its output.
func rebuildWith(t *testing.T, ctx context.Context, mk func(dir string) *config.Config) *testkit.Folded {
	t.Helper()
	dir := t.TempDir()
	e := openEngine(t, mk(dir), nil)
	defer e.Close()
	catchUp(t, ctx, e)
	f, err := testkit.FoldNDJSON(filepath.Join(dir, "out", "ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// check asserts the consumer view of both sinks equals a full rebuild of the example config.
func check(t *testing.T, ctx context.Context, dsn, dir string, e *engine.Engine, label string, mutate func(*config.Config)) *testkit.Folded {
	t.Helper()
	return checkWith(t, ctx, dir, e, label, func(d string) *config.Config { return testConfig(t, dsn, d, 500, mutate) })
}

// checkWith asserts the consumer view of both sinks equals a full rebuild with the
// config mk builds.
func checkWith(t *testing.T, ctx context.Context, dir string, e *engine.Engine, label string, mk func(dir string) *config.Config) *testkit.Folded {
	t.Helper()
	catchUp(t, ctx, e)
	inc, err := testkit.FoldNDJSON(filepath.Join(dir, "out", "ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	pq, err := testkit.FoldParquet(filepath.Join(dir, "out", "parquet"))
	if err != nil {
		t.Fatal(err)
	}
	full := rebuildWith(t, ctx, mk)
	if d := testkit.Diff(full.Docs, inc.Docs, 10); len(d) > 0 {
		t.Fatalf("%s: incremental output differs from a full rebuild (%d docs vs %d):\n%s", label, len(inc.Docs), len(full.Docs), strings.Join(d, "\n"))
	}
	if d := testkit.Diff(inc.Docs, pq.Docs, 10); len(d) > 0 {
		t.Fatalf("%s: Parquet output differs from NDJSON output:\n%s", label, strings.Join(d, "\n"))
	}
	t.Logf("%s: %d documents match a full rebuild (generation %d, base %d, epoch %d)", label, len(inc.Docs), inc.Latest, inc.Base, inc.Epoch)
	return inc
}
