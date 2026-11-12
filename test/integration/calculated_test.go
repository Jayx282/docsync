//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/studio"
)

// TestStudioCalculated runs a design with calculated fields, a list used only in
// calculations and a root filter under random writes. Summaries must follow every
// change to the rows they summarise, and incremental output must equal a full rebuild.
func TestStudioCalculated(t *testing.T) {
	ctx := context.Background()
	s := seed(t)
	t.Logf("seed %d (replay with DOCSYNC_TEST_SEED=%d)", s, s)
	rng := rand.New(rand.NewSource(s))
	fx := newFixture(t, s, 120)

	f := func(cols ...string) []studio.Field {
		out := make([]studio.Field, len(cols))
		for i, c := range cols {
			out[i] = studio.Field{Column: c}
		}
		return out
	}
	design := &studio.Design{
		Version: 1, Name: "stone",
		Root: studio.Root{Table: "dbo.stone", Key: "stoneid", Fields: f("stoneid", "carat", "status", "lab_code"), Filter: `status <> "sold"`},
		Related: []studio.Relation{
			{ID: "lab", Table: "dbo.lab", Shape: studio.ShapeFields, Join: []studio.JoinPair{{From: "lab_code", To: "code"}},
				Fields: []studio.Field{{Column: "name", As: "lab_name"}}},
			{ID: "inclusions", Table: "dbo.inclusion", Shape: studio.ShapeList, Path: "inclusions", Join: []studio.JoinPair{{From: "stoneid", To: "stoneid"}},
				Fields: f("inclusion_id", "severity")},
			{ID: "rates", Table: "dbo.rate", Shape: studio.ShapeList, Join: []studio.JoinPair{{From: "stoneid", To: "stoneid"}}, Hidden: true},
			{ID: "orders", Table: "dbo.order_header", Shape: studio.ShapeList, Path: "orders", Via: &studio.Via{Table: "dbo.order_stone", Join: []studio.JoinPair{{From: "stoneid", To: "stoneid"}}},
				Join: []studio.JoinPair{{From: "order_id", To: "order_id"}}, Fields: f("order_id", "status")},
		},
		Calculated: []studio.Calc{
			{Name: "rate_count", Expr: "COUNT(rates)"},
			{Name: "rate_total", Expr: "SUM(rates.amount)"},
			{Name: "price_per_carat", Expr: "ROUND(rate_total / carat, 2)"},
			{Name: "max_severity", Expr: "MAX(inclusions.severity)"},
			{Name: "severe_inclusions", Expr: "COUNTIF(inclusions.severity >= 3)"},
			{Name: "lab_label", Expr: `UPPER(lab_name) & " / " & COALESCE(lab.country, "?")`},
			{Name: "open_orders", Expr: `COUNTIF(orders.status = "open")`},
			{Name: "big", Expr: "carat >= 2"},
		},
	}
	svc := &studio.Service{DB: fx.db, DSNEnv: "UNUSED", BaseDir: t.TempDir()}

	prev, err := svc.Preview(ctx, design, nil, 20)
	if err != nil || len(prev.Errors) > 0 || len(prev.Documents) == 0 {
		t.Fatalf("preview: %v %v", err, prev.Errors)
	}
	val, err := svc.Validate(ctx, design)
	if err != nil || !val.OK {
		t.Fatalf("validate: %v %v %+v", err, val.Errors, val.Findings)
	}
	checked := false
	for _, fd := range val.Findings {
		checked = checked || (fd.Check == "calculated fields" && strings.Contains(fd.Detail, "without errors"))
	}
	if !checked {
		t.Fatalf("validation computes every calculated field once: %+v", val.Findings)
	}

	comp, err := svc.Compile(ctx, design)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(dir string, budget int) *config.Config {
		cfg, err := config.Parse([]byte(comp.YAML))
		if err != nil {
			t.Fatal(err)
		}
		tuneForTest(cfg, fx.dsn, dir, budget)
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	// Summaries match the database, recomputed directly.
	verify := func(docs map[string]string, label string) {
		t.Helper()
		var unsold int
		if err := fx.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dbo.stone WHERE status <> 'sold'").Scan(&unsold); err != nil {
			t.Fatal(err)
		}
		if len(docs) != unsold {
			t.Fatalf("%s: %d documents for %d unsold stones", label, len(docs), unsold)
		}
		n := 0
		for _, js := range docs {
			if n++; n > 25 {
				break
			}
			var d map[string]any
			if err := json.Unmarshal([]byte(js), &d); err != nil {
				t.Fatal(err)
			}
			id := int64(d["stoneid"].(float64))
			var cnt, severe int64
			var total, maxSev *float64
			if err := fx.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM dbo.rate WHERE stoneid = @p1), (SELECT CAST(SUM(amount) AS float) FROM dbo.rate WHERE stoneid = @p1),
				(SELECT CAST(MAX(severity) AS float) FROM dbo.inclusion WHERE stoneid = @p1), (SELECT COUNT(*) FROM dbo.inclusion WHERE stoneid = @p1 AND severity >= 3)`, id).
				Scan(&cnt, &total, &maxSev, &severe); err != nil {
				t.Fatal(err)
			}
			num := func(k string) *float64 {
				if v, ok := d[k].(float64); ok {
					return &v
				}
				return nil
			}
			same := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 0.005) }
			if got := num("rate_count"); got == nil || int64(*got) != cnt || !same(num("rate_total"), total) || !same(num("max_severity"), maxSev) || int64(*num("severe_inclusions")) != severe {
				t.Fatalf("%s: stone %d: document %s, but the database has %d rates totalling %v, max severity %v, %d severe", label, id, js, cnt, total, maxSev, severe)
			}
		}
	}

	dir := t.TempDir()
	e := openEngine(t, mk(dir, 29), nil)
	defer e.Close()
	n := rounds(80)
	for round := 1; round <= n; round++ {
		if _, err := fx.gen.RandomTransaction(ctx, 6); err != nil {
			t.Fatalf("round %d writer: %v", round, err)
		}
		for steps := rng.Intn(3); steps > 0; steps-- {
			if _, err := e.Step(ctx); err != nil {
				t.Fatalf("round %d step: %v", round, err)
			}
		}
		if round%40 == 0 || round == n {
			label := fmt.Sprintf("round %d", round)
			inc := checkWith(t, ctx, dir, e, label, func(d string) *config.Config { return mk(d, 500) })
			verify(inc.Docs, label)
		}
	}
}
