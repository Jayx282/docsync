//go:build integration

package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/studio"
)

// stonesDesign builds the example document as a Studio user would: from the
// suggester's output for dbo.stone (the example schema declares no foreign keys, so
// every relation comes from column names).
func stonesDesign(t *testing.T, snap *studio.Snapshot) *studio.Design {
	t.Helper()
	sugg, err := studio.Suggest(snap, "dbo.stone")
	if err != nil {
		t.Fatal(err)
	}
	find := func(table string, via bool) studio.Suggestion {
		for _, s := range sugg {
			if strings.EqualFold(s.Table, table) && (s.Via != nil) == via {
				return s
			}
		}
		t.Fatalf("no suggestion for %s (via=%v) in %+v", table, via, sugg)
		return studio.Suggestion{}
	}
	f := func(cols ...string) []studio.Field {
		out := make([]studio.Field, len(cols))
		for i, c := range cols {
			out[i] = studio.Field{Column: c}
		}
		return out
	}
	rel := func(s studio.Suggestion, fields []studio.Field, sortBy ...string) studio.Relation {
		return studio.Relation{ID: s.ID, Table: s.Table, Shape: s.Shape, Join: s.Join, Via: s.Via, Path: s.Path, Fields: fields, SortBy: sortBy}
	}
	lab := find("dbo.lab", false)
	if lab.Shape != studio.ShapeFields || lab.Join[0].From != "lab_code" || lab.Join[0].To != "code" {
		t.Fatalf("lab lookup suggestion: %+v", lab)
	}
	orders := find("dbo.order_header", true)
	if orders.Via.Table != "dbo.order_stone" || orders.Shape != studio.ShapeList {
		t.Fatalf("bridge suggestion: %+v", orders)
	}
	return &studio.Design{
		Version: 1,
		Name:    "stone",
		Root:    studio.Root{Table: "dbo.stone", Key: "stoneid", Fields: f("stoneid", "packet_no", "carat", "shape", "color", "clarity", "lab_code", "status", "updated_at")},
		Related: []studio.Relation{
			rel(lab, []studio.Field{{Column: "name", As: "lab_name"}, {Column: "country", As: "lab_country"}}),
			rel(find("dbo.inclusion", false), f("inclusion_id", "kind", "position", "severity"), "position", "inclusion_id"),
			rel(find("dbo.rate", false), f("rate_type", "amount", "currency")), // sorted by the primary key
			rel(orders, f("order_id", "party", "order_date", "status")),
		},
		Output: studio.Output{IncludeJSON: true},
	}
}

// TestStudio designs the stones document from suggestions, previews and validates it
// on a real database, checks that the compiled config keeps incremental output equal
// to a full rebuild under random changes, and deploys it through the HTTP API.
func TestStudio(t *testing.T) {
	ctx := context.Background()
	s := seed(t)
	t.Logf("seed %d (replay with DOCSYNC_TEST_SEED=%d)", s, s)
	rng := rand.New(rand.NewSource(s))
	fx := newFixture(t, s, 120)

	snap, err := studio.LoadSnapshot(ctx, fx.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tables) != 7 || !snap.CT || !snap.Snapshot {
		t.Fatalf("schema snapshot: %d tables, change tracking %v, snapshot isolation %v", len(snap.Tables), snap.CT, snap.Snapshot)
	}
	design := stonesDesign(t, snap)
	t.Setenv("DOCSYNC_STUDIO_IT_DSN", fx.dsn)
	svc := &studio.Service{DB: fx.db, DSNEnv: "DOCSYNC_STUDIO_IT_DSN", BaseDir: t.TempDir()}

	// Preview builds real documents with every part of the design.
	prev, err := svc.Preview(ctx, design, nil, 50)
	if err != nil || len(prev.Errors) > 0 {
		t.Fatalf("preview: %v %v", err, prev.Errors)
	}
	seen := map[string]bool{}
	for _, d := range prev.Documents {
		if d.Error != "" {
			t.Fatalf("preview document %s: %s", d.Root, d.Error)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(d.JSON), &m); err != nil {
			t.Fatal(err)
		}
		for k, v := range m {
			if v != nil {
				seen[k] = true
			}
		}
	}
	for _, k := range []string{"stoneid", "lab_name", "inclusions", "rates", "order_headers"} {
		if !seen[k] {
			t.Fatalf("no previewed document has %s: %v", k, seen)
		}
	}
	if one, err := svc.Preview(ctx, design, []string{"999999999"}, 0); err != nil || len(one.Documents) != 1 || one.Documents[0].Error == "" {
		t.Fatalf("previewing a missing id reports it: %+v %v", one, err)
	}

	// Validation runs the doctor checks; the example database is fully prepared.
	val, err := svc.Validate(ctx, design)
	if err != nil {
		t.Fatal(err)
	}
	if !val.OK {
		t.Fatalf("validation failed: %v %+v", val.Errors, val.Findings)
	}
	if !strings.Contains(val.Script, "GRANT SELECT, VIEW CHANGE TRACKING ON [dbo].[order_stone]") {
		t.Fatalf("DBA script misses the bridge table:\n%s", val.Script)
	}

	// The compiled config obeys the invariant: incremental output == full rebuild.
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
	dir := t.TempDir()
	e := openEngine(t, mk(dir, 29), nil)
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
			checkWith(t, ctx, dir, e, fmt.Sprintf("round %d", round), func(d string) *config.Config { return mk(d, 500) })
		}
	}
	e.Close()

	// Deploy through the HTTP API; the pipeline runs inside the studio.
	store, err := studio.NewStore(filepath.Join(svc.BaseDir, "designs"))
	if err != nil {
		t.Fatal(err)
	}
	rt := &studio.Runtime{}
	defer rt.Stop()
	srv := &studio.Server{Svc: svc, Store: store, Runtime: rt, Token: "it-token"}
	h, err := srv.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	call := func(method, path string, body, out any) int {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer it-token")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		if out != nil && res.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("%s %s: %v: %s", method, path, err, raw)
			}
		}
		return res.StatusCode
	}

	var tables []map[string]any
	if code := call("GET", "/api/studio/tables", nil, &tables); code != 200 || len(tables) != 7 {
		t.Fatalf("tables: %d %v", code, tables)
	}
	var sugg []studio.Suggestion
	if code := call("GET", "/api/studio/suggestions?root=dbo.stone", nil, &sugg); code != 200 || len(sugg) < 5 {
		t.Fatalf("suggestions: %d %+v", code, sugg)
	}
	var detail struct {
		Table   studio.Table        `json:"table"`
		Related []studio.Suggestion `json:"related"`
	}
	if code := call("GET", "/api/studio/tables/dbo.stone", nil, &detail); code != 200 || detail.Table.PK[0] != "stoneid" || len(detail.Related) != len(sugg) {
		t.Fatalf("table detail: %d %+v", code, detail.Table)
	}
	var prof studio.Profile
	if code := call("POST", "/api/studio/profile", map[string]any{"root": "dbo.stone", "relation": design.Related[0]}, &prof); code != 200 || prof.FanoutMax < 1 {
		t.Fatalf("profile of the lab lookup: %d %+v", code, prof)
	}
	var st studio.RuntimeStatus
	if code := call("POST", "/api/studio/deploy", map[string]any{"design": design}, &st); code != 200 {
		t.Fatalf("deploy: %d", code)
	}
	var stones int64
	if err := fx.db.QueryRowContext(ctx, "SELECT COUNT_BIG(*) FROM dbo.stone").Scan(&stones); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		var status struct {
			Upserts uint64 `json:"upserts"`
			Sinks   []struct {
				HasBase bool `json:"has_base"`
			} `json:"sinks"`
		}
		code := call("GET", "/pipeline/api/status", nil, &status)
		if code == 200 && status.Upserts >= uint64(stones) && len(status.Sinks) == 1 && status.Sinks[0].HasBase {
			t.Logf("deployed pipeline loaded %d documents and wrote a base snapshot", status.Upserts)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployed pipeline did not finish its first load: %+v, status %d", rt.Status(), code)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, p := range []string{"pipelines/stone/docsync.yaml", "pipelines/stone/design.json", "active.json", "designs/stone.json", "pipelines/stone/out/manifest.json"} {
		if _, err := os.Stat(filepath.Join(svc.BaseDir, p)); err != nil {
			t.Fatalf("deploy should write %s: %v", p, err)
		}
	}
	// A studio connected to another database must not restart this deployment: the
	// config names only the environment variable, which may now point elsewhere.
	master, err := sql.Open("sqlserver", serverDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	elsewhere := &studio.Service{DB: master, DSNEnv: "DOCSYNC_STUDIO_IT_DSN", BaseDir: svc.BaseDir}
	if err := elsewhere.StartActive(ctx, &studio.Runtime{}); err == nil || !strings.Contains(err.Error(), "not restarting") {
		t.Fatalf("restarting against another database should be refused, got %v", err)
	}
	if code := call("POST", "/api/studio/runtime/stop", map[string]any{}, &st); code != 200 || st.State != "stopped" {
		t.Fatalf("stop: %d %+v", code, st)
	}
	if _, err := os.Stat(filepath.Join(svc.BaseDir, "active.json")); !os.IsNotExist(err) {
		t.Fatal("stopping forgets the deployment so it does not restart with the studio")
	}
}
