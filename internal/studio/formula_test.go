package studio

import (
	"strings"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
)

func typed(name string, pk []string, cols ...string) *Table {
	t := &Table{Name: name, Rows: 10, CTEnabled: true, PK: pk}
	for _, c := range cols {
		n, ty, _ := strings.Cut(c, " ")
		t.Columns = append(t.Columns, Column{Name: n, Type: ty})
	}
	t.Indexes = []Index{{Name: "pk_" + name, Columns: pk, Unique: true, Primary: true}}
	return t
}

func typedSnapshot() *Snapshot {
	lab := typed("dbo.lab", []string{"lab_id"}, "lab_id int", "code varchar(10)", "name nvarchar(100)", "country nvarchar(50)")
	lab.Indexes = append(lab.Indexes, Index{Name: "ux_lab_code", Columns: []string{"code"}, Unique: true})
	return NewSnapshot([]*Table{
		typed("dbo.stone", []string{"stoneid"}, "stoneid bigint", "packet_no varchar(20)", "carat decimal(10,3)", "status varchar(20)",
			"lab_code varchar(10)", "certified_on date", "is_fancy bit"),
		lab,
		typed("dbo.inclusion", []string{"inclusion_id"}, "inclusion_id bigint", "stoneid bigint", "kind varchar(20)", "position int", "severity tinyint"),
		typed("dbo.rate", []string{"rate_id"}, "rate_id bigint", "stoneid bigint", "rate_type varchar(10)", "amount decimal(18,2)", "currency char(3)"),
		typed("dbo.order_header", []string{"order_id"}, "order_id bigint", "party nvarchar(100)", "order_date date", "status varchar(20)"),
		typed("dbo.order_stone", []string{"order_id", "stoneid"}, "order_id bigint", "stoneid bigint"),
	}, nil)
}

func formulaDesign() *Design {
	return &Design{
		Version: 1, Name: "stone",
		Root: Root{Table: "dbo.stone", Key: "stoneid", Fields: []Field{{Column: "stoneid"}, {Column: "carat"}, {Column: "status", As: "stock_status"}}},
		Related: []Relation{
			{ID: "lab", Table: "dbo.lab", Shape: ShapeFields, Join: []JoinPair{{From: "lab_code", To: "code"}}, Fields: []Field{{Column: "name", As: "lab_name"}}},
			{ID: "inclusions", Table: "dbo.inclusion", Shape: ShapeList, Path: "inclusions", Join: []JoinPair{{From: "stoneid", To: "stoneid"}}, Fields: []Field{{Column: "kind"}}},
			{ID: "rates", Table: "dbo.rate", Shape: ShapeList, Join: []JoinPair{{From: "stoneid", To: "stoneid"}}, Hidden: true},
			{ID: "orders", Table: "dbo.order_header", Shape: ShapeList, Path: "orders", Via: &Via{Table: "dbo.order_stone", Join: []JoinPair{{From: "stoneid", To: "stoneid"}}},
				Join: []JoinPair{{From: "order_id", To: "order_id"}}, Fields: []Field{{Column: "order_id"}}},
		},
		Output: Output{IncludeJSON: true},
	}
}

func compileOne(t *testing.T, d *Design, expr string) (string, error) {
	t.Helper()
	d.Calculated = []Calc{{Name: "x", Expr: expr}}
	sc, err := newFormulaScope(d, typedSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	ce, err := sc.compileCalc(d.Calculated[0])
	return ce.sql, err
}

func TestFormulaSQL(t *testing.T) {
	for _, c := range []struct{ expr, want string }{
		{"1 + 2 * 3", "(1 + (2 * 3))"},
		{"(1 + 2) * 3", "((1 + 2) * 3)"},
		{"-carat", "(-t.[carat])"},
		{"carat * 2", "(t.[carat] * 2)"},
		{"stone.carat", "t.[carat]"},
		{"stock_status", "t.[status]"}, // a renamed root field
		{"packet_no", "t.[packet_no]"}, // a root column that is not a field
		{"lab.country", "f_lab.[country]"},
		{"lab_name", "f_lab.[name]"}, // a lookup's field
		{"carat / 0", "(CAST(t.[carat] AS float) / NULLIF(CAST(0 AS float), 0))"},
		{"stoneid % 7", "(t.[stoneid] % NULLIF(7, 0))"},
		{`"it's " & lab_name`, "CONCAT(N'it''s ', f_lab.[name])"},
		{`'single'`, "N'single'"},
		{"carat >= 2", "(CASE WHEN (t.[carat] >= 2) THEN CAST(1 AS bit) WHEN NOT (t.[carat] >= 2) THEN CAST(0 AS bit) END)"},
		{"IF(is_fancy AND carat > 1, \"big\", \"small\")", "(CASE WHEN ((t.[is_fancy] = 1) AND (t.[carat] > 1)) THEN N'big' ELSE N'small' END)"},
		{"IF(lab_name = NULL, 0, 1)", "(CASE WHEN (f_lab.[name] IS NULL) THEN 0 ELSE 1 END)"},
		{`IF(stock_status IN ("stock", 'memo'), 1)`, "(CASE WHEN (t.[status] IN (N'stock', N'memo')) THEN 1 ELSE NULL END)"},
		{`IF(certified_on > "2024-01-31", 1, 0)`, "(CASE WHEN (t.[certified_on] > N'2024-01-31') THEN 1 ELSE 0 END)"},
		{"ROUND(carat, 1)", "ROUND(t.[carat], 1)"},
		{"LEFT(packet_no, 2)", "LEFT(t.[packet_no], 2)"},
		{"MID(packet_no, 3, 4)", "SUBSTRING(t.[packet_no], 3, 4)"},
		{"TRIM(packet_no)", "LTRIM(RTRIM(t.[packet_no]))"},
		{"TEXT(certified_on)", "CONVERT(nvarchar(33), t.[certified_on], 126)"},
		{`DATEDIFF("day", certified_on, "2026-01-01")`, "DATEDIFF_BIG(day, t.[certified_on], N'2026-01-01')"},
		{"SQRT(carat)", "(CASE WHEN t.[carat] >= 0 THEN SQRT(CAST(t.[carat] AS float)) END)"},
		{"IF(ISBLANK(packet_no), 1, 0)", "(CASE WHEN (NULLIF(LTRIM(RTRIM(t.[packet_no])), N'') IS NULL) THEN 1 ELSE 0 END)"},
		{"COUNT(rates)", "(SELECT COUNT_BIG(*) FROM [dbo].[rate] s1 WHERE s1.[stoneid] = r.id)"},
		{"SUM(rates.amount)", "(SELECT SUM(s1.[amount]) FROM [dbo].[rate] s1 WHERE s1.[stoneid] = r.id)"},
		{"MAX(inclusions.severity)", "(SELECT MAX(s1.[severity]) FROM [dbo].[inclusion] s1 WHERE s1.[stoneid] = r.id)"},
		{"SUM(inclusions.position)", "(SELECT SUM(CAST(s1.[position] AS bigint)) FROM [dbo].[inclusion] s1 WHERE s1.[stoneid] = r.id)"},
		{"AVG(inclusions.severity)", "(SELECT AVG(CAST(s1.[severity] AS decimal(38, 6))) FROM [dbo].[inclusion] s1 WHERE s1.[stoneid] = r.id)"},
		{`COUNTIF(inclusions.severity >= 3 AND inclusions.kind <> "cloud")`,
			"(SELECT COUNT_BIG(*) FROM [dbo].[inclusion] s1 WHERE s1.[stoneid] = r.id AND ((s1.[severity] >= 3) AND (s1.[kind] <> N'cloud')))"},
		{"SUM(rates.amount / carat)", // root columns are visible inside a summary
			"(SELECT SUM(CAST((CAST(s1.[amount] AS float) / NULLIF(CAST(t.[carat] AS float), 0)) AS decimal(38, 6))) FROM [dbo].[rate] s1 WHERE s1.[stoneid] = r.id)"},
		{"COUNT(orders)", "(SELECT COUNT_BIG(*) FROM [dbo].[order_stone] s1b JOIN [dbo].[order_header] s1 ON s1.[order_id] = s1b.[order_id] WHERE s1b.[stoneid] = r.id)"},
		{"MIN(orders.order_date)", "(SELECT MIN(s1.[order_date]) FROM [dbo].[order_stone] s1b JOIN [dbo].[order_header] s1 ON s1.[order_id] = s1b.[order_id] WHERE s1b.[stoneid] = r.id)"},
	} {
		got, err := compileOne(t, formulaDesign(), c.expr)
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.expr, got, c.want)
		}
	}
}

func TestFormulaErrors(t *testing.T) {
	for _, c := range []struct{ expr, want string }{
		{"", "empty"},
		{"carat *", "ends too early"},
		{`"open`, "no closing"},
		{"carat $ 2", "unexpected character"},
		{"carat 2", "unexpected number 2"},
		{"carrat * 2", "unknown field carrat (did you mean carat?)"},
		{"SUM(amount)", "amount is a column of rates; write rates.amount"},
		{"amount * 2", "for the column of rates, write rates.amount"},
		{"SUM(carat)", "for example SUM(inclusions.position)"}, // never a key or join column
		{"UPPR(lab_name)", "unknown function UPPR (did you mean UPPER?)"},
		{"labb.name", "unknown table labb"},
		{"lab.nmae", "lab has no column nmae (did you mean name?)"},
		{"inclusions.severity", "is a list (many rows per document); summarise it"},
		{"inclusions", "summarise it, for example COUNT(inclusions)"},
		{"lab", "pick one of its columns"},
		{"SUM(rates.amount + inclusions.severity)", "one list at a time"},
		{"SUM(rates.amount * COUNT(rates))", "cannot contain another summary"},
		{"SUM(carat)", "needs a list to summarise"},
		{"MAX(carat, 2)", "use IF(a > b, a, b)"},
		{"SUM(rates.currency)", "SUM needs numbers, but this is text"},
		{"COUNT(lab)", "lab is not a list"},
		{"packet_no = 5", "compares text with a number"},
		{`certified_on > "last year"`, "write dates as"},
		{`stock_status + "x"`, "use & to join text"},
		{"certified_on + 1", "arithmetic on dates"},
		{"IF(carat > 1, 1, \"no\")", "IF gives a number in one case and text in the other"},
		{"IF(carat, 1, 0)", "expected a condition"},
		{"carat > NOW()", "NOW() is not allowed"},
		{"LEFT(packet_no, carat)", "the count must be a whole number written in the formula"},
		{`DATEDIFF("fortnight", certified_on, certified_on)`, "the unit must be one of"},
		{"ROUND(packet_no)", "ROUND needs a number"},
		{"carat < NULL", "cannot compare with NULL"},
		{"99999999999999999999", "too large"},
	} {
		_, err := compileOne(t, formulaDesign(), c.expr)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want error containing %q, got %v", c.expr, c.want, err)
		}
	}
}

func TestFormulaChainsAndLoops(t *testing.T) {
	d := formulaDesign()
	d.Calculated = []Calc{
		{Name: "price_per_carat", Expr: "total / carat"},
		{Name: "total", Expr: "SUM(rates.amount)"},
		{Name: "carat", Expr: "ROUND(carat, 1)"}, // its own name means the column
	}
	sc, err := newFormulaScope(d, typedSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	ce, err := sc.compileCalc(d.Calculated[0])
	if err != nil || !strings.Contains(ce.sql, "SUM(s1.[amount])") || !strings.Contains(ce.sql, "ROUND(t.[carat], 1)") {
		t.Fatalf("calculated fields can use each other: %v\n%s", err, ce.sql)
	}
	d.Calculated = []Calc{{Name: "a", Expr: "b + 1"}, {Name: "b", Expr: "a + 1"}}
	sc, _ = newFormulaScope(d, typedSnapshot())
	if _, err := sc.compileCalc(d.Calculated[0]); err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("a loop must be reported: %v", err)
	}
	d.Calculated = []Calc{{Name: "n", Expr: "COUNT(rates)"}, {Name: "bad", Expr: "SUM(rates.amount * n)"}}
	sc, _ = newFormulaScope(d, typedSnapshot())
	if _, err := sc.compileCalc(d.Calculated[1]); err == nil || !strings.Contains(err.Error(), "cannot contain another summary") {
		t.Fatalf("a summary over a summary must be refused: %v", err)
	}
}

func TestFilterFormula(t *testing.T) {
	sc, _ := newFormulaScope(formulaDesign(), typedSnapshot())
	got, err := sc.compileFilter(`stock_status <> "sold" AND NOT is_fancy`)
	if err != nil || got != "((t.[status] <> N'sold') AND (NOT (t.[is_fancy] = 1)))" {
		t.Fatalf("filter: %q %v", got, err)
	}
	for expr, want := range map[string]string{
		`lab.country = "IN"`: "a filter can use only columns of stone",
		`lab_name = "x"`:     "unknown field lab_name",
		`COUNT(rates) > 0`:   "a filter can use only columns of stone",
		`carat`:              "expected a condition",
	} {
		if _, err := sc.compileFilter(expr); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("filter %q: want %q, got %v", expr, want, err)
		}
	}
}

func TestCompileCalculated(t *testing.T) {
	d := formulaDesign()
	d.Root.Filter = `stock_status <> "sold"`
	d.Calculated = []Calc{
		{Name: "rate_count", Expr: "COUNT(rates)"},
		{Name: "lab_label", Expr: `UPPER(lab_name) & " (" & lab.country & ")"`},
		{Name: "severe", Expr: "COUNTIF(inclusions.severity >= 3)"},
	}
	comp, err := Compile(d, typedSnapshot(), CompileOptions{BaseDir: "/tmp/s"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := comp.Config
	frags := map[string]config.Fragment{}
	for _, f := range cfg.Document.Fragments {
		frags[f.Name] = f
	}
	if _, ok := frags["rates"]; ok {
		t.Fatal("a hidden relation has no fragment")
	}
	calc := frags[calculatedFragment].SQL
	for _, want := range []string{"AS [rate_count]", "AS [lab_label]", "AS [severe]", "LEFT JOIN [dbo].[lab] f_lab ON f_lab.[code] = t.[lab_code]", "JOIN [dbo].[stone] t ON t.[stoneid] = r.id"} {
		if !strings.Contains(calc, want) {
			t.Fatalf("calculated fragment misses %q:\n%s", want, calc)
		}
	}
	hasRoute := false
	for _, r := range cfg.Routing {
		hasRoute = hasRoute || r.Table == "dbo.rate"
	}
	if !hasRoute {
		t.Fatal("a hidden relation still needs routing, or rate changes would be missed")
	}
	if cfg.Document.Root.Filter != "(t.[status] <> N'sold')" {
		t.Fatalf("root filter: %q", cfg.Document.Root.Filter)
	}

	// Errors name the field they belong to, so the UI can show them in place.
	d.Calculated = append(d.Calculated, Calc{Name: "oops", Expr: "carrat"}, Calc{Name: "lab_name", Expr: "1"})
	err = d.Validate(typedSnapshot())
	if err == nil || !strings.Contains(err.Error(), `field "lab_name" is produced by both related[0] (dbo.lab) and calculated field "lab_name"`) {
		t.Fatalf("a calculated field cannot reuse a field name: %v", err)
	}
	d.Calculated = d.Calculated[:len(d.Calculated)-1]
	err = d.Validate(typedSnapshot())
	if err == nil || !strings.Contains(err.Error(), `calculated field "oops": unknown field carrat`) {
		t.Fatalf("formula errors are reported per field: %v", err)
	}
}
