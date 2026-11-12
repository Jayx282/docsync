package routing

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
)

const cfgYAML = `
source: { type: sqlserver, dsn_env: X }
document:
  name: stone
  root: { table: dbo.stone, key: [stoneid] }
  fragments:
    - { name: f, sql: "SELECT r.id AS _root, s.color FROM {{roots}} r JOIN dbo.stone s ON s.stoneid = r.id" }
routing:
  - { table: dbo.inclusion,    key: [inclusion_id],      next: { table: dbo.stone,        join: { stoneid: stoneid },   kind: references } }
  - { table: dbo.lab,          key: [lab_id],            next: { table: dbo.stone,        join: { code: lab_code },     kind: referenced_by } }
  - { table: dbo.order_stone,  key: [order_id, stoneid], next: { table: dbo.stone,        join: { stoneid: stoneid },   kind: references } }
  - { table: dbo.order_header, key: [order_id],          next: { table: dbo.order_stone,  join: { order_id: order_id }, kind: referenced_by } }
  - { table: dbo.order_line,   key: [line_id],           next: { table: dbo.order_header, join: { order_id: order_id }, kind: references } }
sinks: [ { type: ndjson, dir: ./out } ]
`

// fakeDB answers lookups from in-memory rows.
type fakeDB map[string][]map[string]any

func (f fakeDB) Lookup(_ context.Context, table config.TableName, match []string, tuples [][]any, want []string) ([][]any, error) {
	set := NewTupleSet()
	for _, row := range f[table.Key()] {
		for _, tu := range tuples {
			ok := true
			for i, m := range match {
				if row[strings.ToLower(m)] != tu[i] {
					ok = false
				}
			}
			if ok {
				var out []any
				for _, w := range want {
					out = append(out, row[strings.ToLower(w)])
				}
				_ = set.Add(out)
			}
		}
	}
	return set.Rows(), nil
}

func plan(t *testing.T) *Plan {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ints(vs []any) []int64 {
	var out []int64
	for _, v := range vs {
		out = append(out, v.(int64))
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestPlan(t *testing.T) {
	p := plan(t)
	tp, _ := p.Table(config.MustParseTableName("dbo.order_line"))
	if len(tp.Hops) != 3 || !tp.NeedsKeyMap {
		t.Fatalf("order_line: want 3 hops and a key map, got %d hops, key map %v", len(tp.Hops), tp.NeedsKeyMap)
	}
	os, _ := p.Table(config.MustParseTableName("dbo.order_stone"))
	if os.NeedsKeyMap {
		t.Fatal("order_stone routes by primary-key columns and needs no key map")
	}
	lab, _ := p.Table(config.MustParseTableName("dbo.lab"))
	if !lab.NeedsKeyMap {
		t.Fatal("lab routes by code, which is not its primary key")
	}
}

func TestPropagate(t *testing.T) {
	p := plan(t)
	db := fakeDB{
		"dbo.stone": {
			{"stoneid": int64(1), "lab_code": "GIA"}, {"stoneid": int64(2), "lab_code": "GIA"}, {"stoneid": int64(3), "lab_code": "IGI"},
		},
		"dbo.order_stone": {
			{"order_id": int64(10), "stoneid": int64(1)}, {"order_id": int64(10), "stoneid": int64(3)}, {"order_id": int64(11), "stoneid": int64(2)},
		},
	}
	ctx := context.Background()
	get := func(table string) *TablePlan {
		tp, ok := p.Table(config.MustParseTableName(table))
		if !ok {
			t.Fatalf("no plan for %s", table)
		}
		return tp
	}
	cases := []struct {
		table string
		start [][]any
		want  []int64
	}{
		{"dbo.inclusion", [][]any{{int64(1)}, {int64(3)}}, []int64{1, 3}},       // old and new parent of a moved inclusion
		{"dbo.lab", [][]any{{"GIA"}}, []int64{1, 2}},                            // lookup fan-out
		{"dbo.lab", [][]any{{"NONE"}}, nil},                                     // no referencing stones
		{"dbo.order_line", [][]any{{int64(10)}, {int64(11)}}, []int64{1, 2, 3}}, // three hops
		{"dbo.order_header", [][]any{{int64(11)}}, []int64{2}},                  // two hops
		{"dbo.order_stone", [][]any{{int64(3)}}, []int64{3}},                    // bridge row's own stoneid
		{"dbo.inclusion", [][]any{{nil}}, nil},                                  // NULL never routes (TupleSet drops it)
	}
	for _, c := range cases {
		set := NewTupleSet()
		for _, s := range c.start {
			_ = set.Add(s)
		}
		got, err := Propagate(ctx, db, p, get(c.table), set.Rows())
		if err != nil {
			t.Fatal(err)
		}
		g := ints(got)
		if len(g) != len(c.want) {
			t.Fatalf("%s %v: got %v, want %v", c.table, c.start, g, c.want)
		}
		for i := range g {
			if g[i] != c.want[i] {
				t.Fatalf("%s %v: got %v, want %v", c.table, c.start, g, c.want)
			}
		}
	}
}
