package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "examples", "stones", "docsync.yaml")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Document.Name != "stone" || len(cfg.Document.Fragments) != 5 || len(cfg.Routing) != 6 {
		t.Fatalf("unexpected example config: %+v", cfg.Document)
	}
	if got := cfg.ResolvePath("./state"); got != filepath.Join(filepath.Dir(path), "state") {
		t.Fatalf("relative paths resolve against the config file, got %s", got)
	}
	if cfg.ContentHash() == "" {
		t.Fatal("empty content hash")
	}
}

const base = `
source: { type: sqlserver, dsn_env: X }
document:
  name: d
  root: { table: dbo.r, key: [id] }
  fragments:
    - name: f
      sql: SELECT r.id AS _root, t.a FROM {{roots}} r JOIN dbo.t t ON t.id = r.id
routing:
  - { table: dbo.t, key: [tid], next: { table: dbo.r, join: { id: id }, kind: references } }
sinks:
  - { type: ndjson, dir: ./out }
`

func TestValidConfig(t *testing.T) {
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.AssembleBudget != 5000 || cfg.Sinks[0].Name != "ndjson" || cfg.State.Dir != "./state" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"select star":       {"t.a FROM", "* FROM", "SELECT * is not allowed"},
		"dotted star":       {"t.a FROM", "t.* FROM", "SELECT * is not allowed"},
		"no roots":          {"{{roots}} r", "dbo.r r", "must join {{roots}}"},
		"unknown field":     {"name: d", "name: d\n  bogus: 1", "unknown field"},
		"many without sort": {"name: f\n", "name: f\n      cardinality: many\n      path: p\n", "requires sort_key"},
		"broken route":      {"table: dbo.r, join", "table: dbo.zzz, join", "has no routing rule"},
		"root routed":       {"table: dbo.t, key: [tid]", "table: dbo.r, key: [tid]", "routes to itself"},
		"bad kind":          {"kind: references", "kind: sideways", "next.kind must be"},
		"bad sink":          {"type: ndjson", "type: kafka", "type must be"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(base, c.from, c.to, 1)
			if src == base {
				t.Fatalf("replacement %q not found", c.from)
			}
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestStarDetection(t *testing.T) {
	for sql, star := range map[string]bool{
		"SELECT * FROM t":                             true,
		"SELECT TOP 5 * FROM t":                       true,
		"SELECT r.id AS _root, * FROM t":              true,
		"SELECT t.* FROM t":                           true,
		"SELECT COUNT(*) FROM t":                      false,
		"SELECT a * b AS c FROM t":                    false,
		"SELECT a, b*2 AS c FROM t":                   false,
		"SELECT (SELECT COUNT(*) FROM x) AS c FROM t": false,
	} {
		got := selectStar.MatchString(sql) || dottedStar.MatchString(sql)
		if got != star {
			t.Errorf("%q: star detected = %v, want %v", sql, got, star)
		}
	}
}

func TestCycleDetected(t *testing.T) {
	src := strings.Replace(base, "routing:\n", `routing:
  - { table: dbo.a, key: [id], next: { table: dbo.b, join: { x: x }, kind: references } }
  - { table: dbo.b, key: [id], next: { table: dbo.a, join: { x: x }, kind: references } }
`, 1)
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestTableNames(t *testing.T) {
	for in, want := range map[string]string{"stone": "dbo.stone", "sales.stone": "sales.stone", "[my schema].[my.table]": "my schema.my.table"} {
		tn, err := ParseTableName(in)
		if err != nil || tn.String() != want {
			t.Fatalf("ParseTableName(%q) = %v, %v", in, tn, err)
		}
	}
	if QuoteIdent("a]b") != "[a]]b]" {
		t.Fatal("closing brackets must be escaped")
	}
	if _, err := ParseTableName("db.dbo.t"); err == nil {
		t.Fatal("three-part names are rejected")
	}
}

func TestDSNValue(t *testing.T) {
	cfg, _ := Parse([]byte(base))
	os.Setenv("X", "")
	if _, err := cfg.DSNValue(); err == nil {
		t.Fatal("empty env var must fail")
	}
	os.Setenv("X", "sqlserver://x")
	if v, err := cfg.DSNValue(); err != nil || v != "sqlserver://x" {
		t.Fatalf("DSNValue = %q, %v", v, err)
	}
}
