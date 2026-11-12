//go:build integration

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/engine"
	"github.com/Jayx282/docsync/internal/testkit"
)

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func churn(t *testing.T, ctx context.Context, fx *fixture, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := fx.gen.RandomTransaction(ctx, 5); err != nil {
			t.Fatal(err)
		}
	}
}

// TRUNCATE resets a table's change-tracking history; the engine must resnapshot.
func TestTruncateTriggersResnapshot(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	e := openEngine(t, testConfig(t, fx.dsn, dir, 37, nil), nil)
	defer e.Close()
	catchUp(t, ctx, e)
	churn(t, ctx, fx, 20)
	exec(t, fx.db, "TRUNCATE TABLE dbo.inclusion")
	churn(t, ctx, fx, 10)
	check(t, ctx, fx.dsn, dir, e, "after TRUNCATE", nil)
}

// Turning change tracking off makes the engine wait; turning it back on resnapshots.
func TestTrackingOffAndOn(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	e := openEngine(t, testConfig(t, fx.dsn, dir, 37, nil), nil)
	defer e.Close()
	catchUp(t, ctx, e)

	exec(t, fx.db, "ALTER TABLE dbo.rate DISABLE CHANGE_TRACKING")
	exec(t, fx.db, "UPDATE dbo.rate SET amount = amount + 1")
	if _, err := e.Step(ctx); err == nil || !strings.Contains(err.Error(), "change tracking is not enabled") {
		t.Fatalf("want a 'change tracking is not enabled' error, got %v", err)
	}
	exec(t, fx.db, "ALTER TABLE dbo.rate ENABLE CHANGE_TRACKING")
	churn(t, ctx, fx, 10)
	check(t, ctx, fx.dsn, dir, e, "after change tracking was re-enabled", nil)
}

// Restoring an older backup moves the database back in time; the engine must notice.
func TestRestoreTriggersResnapshot(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	cfg := testConfig(t, fx.dsn, dir, 37, nil)
	e := openEngine(t, cfg, nil)
	catchUp(t, ctx, e)

	master, err := sql.Open("sqlserver", serverDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	bak := fmt.Sprintf("/var/opt/mssql/data/%s.bak", fx.name)
	exec(t, master, fmt.Sprintf("BACKUP DATABASE [%s] TO DISK = N'%s' WITH INIT", fx.name, bak))
	churn(t, ctx, fx, 30) // changes the restore will undo
	catchUp(t, ctx, e)
	e.Close()

	fx.db.Close()
	exec(t, master, fmt.Sprintf("ALTER DATABASE [%s] SET SINGLE_USER WITH ROLLBACK IMMEDIATE", fx.name))
	exec(t, master, fmt.Sprintf("RESTORE DATABASE [%s] FROM DISK = N'%s' WITH REPLACE", fx.name, bak))
	exec(t, master, fmt.Sprintf("ALTER DATABASE [%s] SET MULTI_USER", fx.name))

	e = openEngine(t, cfg, nil)
	defer e.Close()
	check(t, ctx, fx.dsn, dir, e, "after restoring an older backup", nil)
}

// Changing the document definition starts a new epoch with a fresh base.
func TestConfigChangeStartsNewEpoch(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	e := openEngine(t, testConfig(t, fx.dsn, dir, 37, nil), nil)
	before := check(t, ctx, fx.dsn, dir, e, "before the config change", nil)
	e.Close()

	addField := func(c *config.Config) {
		f := &c.Document.Fragments[0]
		f.SQL = strings.Replace(f.SQL, "s.stoneid, s.packet_no,", "s.stoneid, s.packet_no, UPPER(s.shape) AS shape_code,", 1)
	}
	e = openEngine(t, testConfig(t, fx.dsn, dir, 37, addField), nil)
	defer e.Close()
	churn(t, ctx, fx, 10)
	after := check(t, ctx, fx.dsn, dir, e, "after the config change", addField)
	if after.Epoch != before.Epoch+1 {
		t.Fatalf("epoch = %d, want %d", after.Epoch, before.Epoch+1)
	}
	for _, d := range after.Docs {
		if !strings.Contains(d, `"shape_code":`) {
			t.Fatalf("document missing the new field: %s", d)
		}
		break
	}
}

// A table read by fragments but marked static is not tracked, so its changes are
// missed; the sweeper must find and fix the stale documents.
func TestSweeperRepairsMissedChanges(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	labStatic := func(c *config.Config) {
		var rules []config.RoutingRule
		for _, r := range c.Routing {
			if r.Table == "dbo.lab" {
				r = config.RoutingRule{Table: "dbo.lab", Static: true}
			}
			rules = append(rules, r)
		}
		c.Routing = rules
	}
	withSweep := func(c *config.Config) {
		labStatic(c)
		c.Source.SweepPerMinute = 6_000_000 // a full pass every step, for the test
	}
	e := openEngine(t, testConfig(t, fx.dsn, dir, 500, withSweep), nil)
	defer e.Close()
	settle := func(steps int) {
		for i := 0; i < steps; i++ {
			if _, err := e.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for i := 0; i < 500; i++ {
			e.WaitForBases()
			v := e.Status()
			done := true
			for _, s := range v.Sinks {
				if !s.HasBase || s.Lag > 0 || s.BaseInFlight {
					done = false
				}
			}
			if done {
				return
			}
			if _, err := e.Step(ctx); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("sinks did not catch up")
	}
	settle(50)
	if c := e.Status().SweepCorrections; c != 0 {
		t.Fatalf("no corrections expected before any missed change, got %d", c)
	}

	exec(t, fx.db, "UPDATE dbo.lab SET name = name + N' (renamed)'") // invisible to routing: lab is static
	settle(50)
	if c := e.Status().SweepCorrections; c == 0 {
		t.Fatal("the sweeper should have found documents left stale by the untracked lab change")
	} else {
		t.Logf("sweeper corrected %d stale documents", c)
	}
	inc, err := testkit.FoldNDJSON(filepath.Join(dir, "out", "ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	full := fullRebuild(t, ctx, fx.dsn, labStatic)
	if d := testkit.Diff(full.Docs, inc.Docs, 5); len(d) > 0 {
		t.Fatalf("after sweeping, output still differs from a full rebuild:\n%s", strings.Join(d, "\n"))
	}
}

// A column type change under an unchanged config pauses the pipeline until an
// operator starts a new epoch.
func TestSchemaDriftPausesUntilNewEpoch(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, seed(t), 120)
	dir := t.TempDir()
	e := openEngine(t, testConfig(t, fx.dsn, dir, 37, nil), nil)
	defer e.Close()
	before := check(t, ctx, fx.dsn, dir, e, "before the drift", nil)

	exec(t, fx.db, "ALTER TABLE dbo.stone ALTER COLUMN carat decimal(12,4) NOT NULL")
	exec(t, fx.db, "UPDATE TOP (5) dbo.stone SET carat = carat + 1")
	for i := 0; i < 5 && !e.Status().Paused; i++ {
		_, _ = e.Step(ctx)
	}
	v := e.Status()
	if !v.Paused || !strings.Contains(v.PauseReason, "schema drift") {
		t.Fatalf("want a schema-drift pause, got paused=%v reason=%q", v.Paused, v.PauseReason)
	}
	if err := e.DoNow(ctx, engine.CmdResume, nil, false); err == nil {
		t.Fatal("resume must be refused while paused for drift")
	}
	if err := e.DoNow(ctx, engine.CmdNewEpoch, nil, false); err != nil {
		t.Fatal(err)
	}
	after := check(t, ctx, fx.dsn, dir, e, "after the new epoch", nil)
	if after.Epoch != before.Epoch+1 {
		t.Fatalf("epoch = %d, want %d", after.Epoch, before.Epoch+1)
	}
}
