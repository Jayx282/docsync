package studio

import (
	"strings"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
)

func intCols(names ...string) []Column {
	var out []Column
	for _, n := range names {
		out = append(out, Column{Name: n, Type: "int"})
	}
	return out
}

func tbl(name string, pk []string, columns []string, idx ...Index) *Table {
	t := &Table{Name: name, Rows: 10, CTEnabled: true, Columns: intCols(columns...), PK: pk}
	t.Indexes = append(t.Indexes, Index{Name: "pk_" + name, Columns: pk, Unique: true, Primary: true})
	t.Indexes = append(t.Indexes, idx...)
	return t
}

// exampleSnapshot mirrors examples/stones/schema.sql (which declares no foreign keys).
func exampleSnapshot() *Snapshot {
	return NewSnapshot([]*Table{
		tbl("dbo.stone", []string{"stoneid"}, []string{"stoneid", "packet_no", "carat", "lab_code", "status"}),
		tbl("dbo.lab", []string{"lab_id"}, []string{"lab_id", "code", "name", "country"}, Index{Name: "ux_lab_code", Columns: []string{"code"}, Unique: true}),
		tbl("dbo.inclusion", []string{"inclusion_id"}, []string{"inclusion_id", "stoneid", "kind", "position", "severity"}),
		tbl("dbo.rate", []string{"rate_id"}, []string{"rate_id", "stoneid", "rate_type", "amount", "currency"}),
		tbl("dbo.order_header", []string{"order_id"}, []string{"order_id", "party", "order_date", "status"}),
		tbl("dbo.order_stone", []string{"order_id", "stoneid"}, []string{"order_id", "stoneid"}),
		tbl("dbo.order_line", []string{"line_id"}, []string{"line_id", "order_id", "note", "amount"}),
	}, nil)
}

func find(sugg []Suggestion, table string, via bool) *Suggestion {
	for i := range sugg {
		if strings.EqualFold(sugg[i].Table, table) && (sugg[i].Via != nil) == via {
			return &sugg[i]
		}
	}
	return nil
}

func TestSuggestByNames(t *testing.T) {
	sugg, err := Suggest(exampleSnapshot(), "dbo.stone")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		table string
		via   bool
		shape Shape
		join  string
	}{
		{"dbo.inclusion", false, ShapeList, "stoneid=stoneid"},
		{"dbo.rate", false, ShapeList, "stoneid=stoneid"},
		{"dbo.order_stone", false, ShapeList, "stoneid=stoneid"},
		{"dbo.lab", false, ShapeFields, "lab_code=code"},
		{"dbo.order_header", true, ShapeList, "order_id=order_id"},
	} {
		s := find(sugg, c.table, c.via)
		if s == nil {
			t.Fatalf("no suggestion for %s (via=%v); got %+v", c.table, c.via, sugg)
		}
		if s.Shape != c.shape || s.Join[0].From+"="+s.Join[0].To != c.join || !s.Guessed {
			t.Fatalf("%s: got shape %s join %+v guessed %v", c.table, s.Shape, s.Join, s.Guessed)
		}
	}
	if s := find(sugg, "dbo.order_header", true); s.Via.Table != "dbo.order_stone" || s.Path != "order_headers" {
		t.Fatalf("bridge suggestion: %+v", s)
	}
	if find(sugg, "dbo.order_line", false) != nil {
		t.Fatal("order_line is two hops away and must not be suggested directly")
	}
}

func TestSuggestByForeignKeys(t *testing.T) {
	s := exampleSnapshot()
	s.FKs = []ForeignKey{
		{Name: "fk_inc_stone", FromTable: "dbo.inclusion", FromCols: []string{"stoneid"}, ToTable: "dbo.stone", ToCols: []string{"stoneid"}},
		{Name: "fk_os_order", FromTable: "dbo.order_stone", FromCols: []string{"order_id"}, ToTable: "dbo.order_header", ToCols: []string{"order_id"}},
		{Name: "fk_os_stone", FromTable: "dbo.order_stone", FromCols: []string{"stoneid"}, ToTable: "dbo.stone", ToCols: []string{"stoneid"}},
	}
	sugg, err := Suggest(s, "dbo.stone")
	if err != nil {
		t.Fatal(err)
	}
	inc := find(sugg, "dbo.inclusion", false)
	if inc == nil || inc.Guessed || inc.Source != "foreign key fk_inc_stone" {
		t.Fatalf("declared keys win over name guesses: %+v", inc)
	}
	if b := find(sugg, "dbo.order_header", true); b == nil || b.Guessed {
		t.Fatalf("bridge through declared keys: %+v", b)
	}
	if sugg[0].Guessed {
		t.Fatal("declared relations are listed before guesses")
	}
}

func exampleDesign() *Design {
	return &Design{
		Version: 1, Name: "stone",
		Root: Root{Table: "dbo.stone", Key: "stoneid", Fields: []Field{{Column: "stoneid"}, {Column: "carat"}, {Column: "status", As: "stock_status"}}},
		Related: []Relation{
			{ID: "lab", Table: "dbo.lab", Shape: ShapeFields, Join: []JoinPair{{From: "lab_code", To: "code"}}, Fields: []Field{{Column: "name", As: "lab_name"}}},
			{ID: "inclusions", Table: "dbo.inclusion", Shape: ShapeList, Path: "inclusions", Join: []JoinPair{{From: "stoneid", To: "stoneid"}},
				Fields: []Field{{Column: "kind"}, {Column: "position"}}, SortBy: []string{"position"}},
			{ID: "rates", Table: "dbo.rate", Shape: ShapeList, Path: "rates", Join: []JoinPair{{From: "stoneid", To: "stoneid"}}, Fields: []Field{{Column: "amount"}}},
			{ID: "orders", Table: "dbo.order_header", Shape: ShapeList, Path: "orders", Via: &Via{Table: "dbo.order_stone", Join: []JoinPair{{From: "stoneid", To: "stoneid"}}},
				Join: []JoinPair{{From: "order_id", To: "order_id"}}, Fields: []Field{{Column: "order_id"}, {Column: "party"}}},
		},
		Output: Output{IncludeJSON: true},
	}
}

func TestCompile(t *testing.T) {
	comp, err := Compile(exampleDesign(), exampleSnapshot(), CompileOptions{DSNEnv: "X", BaseDir: "/tmp/studio/pipelines/stone"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := comp.Config
	if len(cfg.Document.Fragments) != 5 {
		t.Fatalf("want 5 fragments, got %d", len(cfg.Document.Fragments))
	}
	frag := map[string]config.Fragment{}
	for _, f := range cfg.Document.Fragments {
		frag[f.Name] = f
	}
	if !strings.Contains(frag["root"].SQL, "t.[status] AS [stock_status]") {
		t.Fatalf("root fragment: %s", frag["root"].SQL)
	}
	if !strings.Contains(frag["lab"].SQL, "JOIN [dbo].[stone] t ON t.[stoneid] = r.id") || !strings.Contains(frag["lab"].SQL, "x.[code] = t.[lab_code]") {
		t.Fatalf("lookup fragment must join through the root: %s", frag["lab"].SQL)
	}
	if strings.Contains(frag["inclusions"].SQL, "[dbo].[stone]") || !strings.Contains(frag["inclusions"].SQL, "x.[stoneid] = r.id") {
		t.Fatalf("a child list joined on the root key needs no root join: %s", frag["inclusions"].SQL)
	}
	if got := frag["rates"].SortKey; len(got) != 1 || got[0] != "rate_id" || !strings.Contains(frag["rates"].SQL, "x.[rate_id] AS [rate_id]") {
		t.Fatalf("list without sort_by sorts by the primary key, which is added to the item: %v / %s", got, frag["rates"].SQL)
	}
	if !strings.Contains(frag["orders"].SQL, "JOIN [dbo].[order_stone] b ON b.[stoneid] = r.id") || !strings.Contains(frag["orders"].SQL, "x.[order_id] = b.[order_id]") {
		t.Fatalf("bridge fragment: %s", frag["orders"].SQL)
	}
	route := map[string]config.RoutingRule{}
	for _, r := range cfg.Routing {
		route[r.Table] = r
	}
	checks := []struct{ table, next, from, to, kind string }{
		{"dbo.lab", "dbo.stone", "code", "lab_code", config.KindReferencedBy},
		{"dbo.inclusion", "dbo.stone", "stoneid", "stoneid", config.KindReferences},
		{"dbo.order_stone", "dbo.stone", "stoneid", "stoneid", config.KindReferences},
		{"dbo.order_header", "dbo.order_stone", "order_id", "order_id", config.KindReferencedBy},
	}
	for _, c := range checks {
		r, ok := route[c.table]
		if !ok || r.Next.Table != c.next || r.Next.Join[c.from] != c.to || r.Next.Kind != c.kind {
			t.Fatalf("routing for %s: %+v", c.table, r.Next)
		}
	}
	if len(route["dbo.order_stone"].Key) != 2 {
		t.Fatal("routing keys come from the primary key")
	}
	if route["dbo.lab"].HotFanoutLimit != DefaultHotFanoutLimit || route["dbo.order_header"].HotFanoutLimit != DefaultHotFanoutLimit || route["dbo.inclusion"].HotFanoutLimit != 0 {
		t.Fatal("shared rows (referenced_by) get a hot fan-out limit; child rows do not")
	}
	if !strings.Contains(comp.YAML, "Generated by docsync studio") || cfg.Sinks[0].Dir != "out" || cfg.State.Dir != "state" || cfg.Dashboard.Disabled || !cfg.Sinks[0].IncludeJSON {
		t.Fatalf("output settings: %+v", cfg.Sinks[0])
	}
	// The YAML round-trips through the normal config parser.
	if _, err := config.Parse([]byte(comp.YAML)); err != nil {
		t.Fatalf("generated YAML does not parse: %v", err)
	}
}

func TestDesignValidation(t *testing.T) {
	snap := exampleSnapshot()
	cases := map[string]struct {
		edit func(*Design)
		want string
	}{
		"duplicate field": {func(d *Design) { d.Related[0].Fields[0].As = "carat" }, `field "carat" is produced by both`},
		"table twice":     {func(d *Design) { d.Related[2].Table = "dbo.inclusion" }, "a table can appear only once"},
		"list needs path": {func(d *Design) { d.Related[1].Path = "" }, "a list needs a name"},
		"bad column":      {func(d *Design) { d.Root.Fields[1].Column = "weight" }, "column weight does not exist"},
		"bad join":        {func(d *Design) { d.Related[0].Join[0].To = "nope" }, "column nope does not exist in dbo.lab"},
		"bridge as field": {func(d *Design) { d.Related[3].Shape = ShapeFields }, "through a bridge table must be a list"},
		"wrong root key":  {func(d *Design) { d.Root.Key = "packet_no" }, "single-column primary key"},
		"bad name":        {func(d *Design) { d.Name = "Stone Index" }, "must start with a lowercase letter"},
		"no fields":       {func(d *Design) { d.Related[0].Fields = nil }, "select at least one field"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := exampleDesign()
			c.edit(d)
			err := d.Validate(snap)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
	if err := exampleDesign().Validate(snap); err != nil {
		t.Fatalf("example design should be valid: %v", err)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := exampleDesign()
	if err := st.Put(d); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get("stone")
	if err != nil || len(got.Related) != 4 || got.UpdatedAt.IsZero() {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if _, err := st.Get("../etc"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	list, _ := st.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
}
