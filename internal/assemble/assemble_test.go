package assemble

import (
	"errors"
	"testing"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
)

var (
	fDetails = config.Fragment{Name: "details", Cardinality: config.CardinalityOne, SQL: "x"}
	fIncl    = config.Fragment{Name: "incl", Cardinality: config.CardinalityMany, Path: "inclusions", SortKey: []string{"pos"}, SQL: "x"}
	fLab     = config.Fragment{Name: "lab", Cardinality: config.CardinalityOne, Path: "lab", SQL: "x"}
)

func cols(names ...string) []schema.Column {
	var out []schema.Column
	for i := 0; i < len(names); i += 2 {
		out = append(out, schema.Column{Name: names[i], DBType: names[i+1], Precision: 10, Scale: 3})
	}
	return out
}

func buildSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Build("stone", "stoneid", schema.Column{Name: "stoneid", DBType: "BIGINT"}, []schema.FragmentColumns{
		{Fragment: fDetails, Columns: cols("_root", "BIGINT", "carat", "DECIMAL", "color", "VARCHAR")},
		{Fragment: fIncl, Columns: cols("_root", "BIGINT", "pos", "INT", "kind", "VARCHAR")},
		{Fragment: fLab, Columns: cols("_root", "BIGINT", "name", "NVARCHAR")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func roots(t *testing.T, ids ...int64) []Root {
	var out []Root
	for _, id := range ids {
		r, err := NewRoot(id)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestAssemble(t *testing.T) {
	s := buildSchema(t)
	inputs := []Input{
		{Fragment: fDetails, Columns: cols("_root", "BIGINT", "carat", "DECIMAL", "color", "VARCHAR"), Rows: [][]any{
			{int64(1), []byte("1.5"), "D"}, {int64(2), []byte("0.3"), nil},
		}},
		{Fragment: fIncl, Columns: cols("_root", "BIGINT", "pos", "INT", "kind", "VARCHAR"), Rows: [][]any{
			{int64(1), int64(5), "cloud"}, {int64(1), int64(2), "feather"}, {int64(1), int64(2), "crystal"},
		}},
		{Fragment: fLab, Columns: cols("_root", "BIGINT", "name", "NVARCHAR"), Rows: [][]any{{int64(2), "GIA"}}},
	}
	out, err := Assemble(s, roots(t, 1, 2), inputs)
	if err != nil {
		t.Fatal(err)
	}
	got1 := doc.MustJSON(out.Docs[roots(t, 1)[0].Key])
	want1 := `{"carat":1.500,"color":"D","inclusions":[{"kind":"crystal","pos":2},{"kind":"feather","pos":2},{"kind":"cloud","pos":5}],"lab":null}`
	if got1 != want1 {
		t.Fatalf("doc 1:\n got %s\nwant %s", got1, want1)
	}
	got2 := doc.MustJSON(out.Docs[roots(t, 2)[0].Key])
	want2 := `{"carat":0.300,"color":null,"inclusions":[],"lab":{"name":"GIA"}}`
	if got2 != want2 {
		t.Fatalf("doc 2:\n got %s\nwant %s", got2, want2)
	}
}

func TestOrderIndependent(t *testing.T) {
	s := buildSchema(t)
	rows := [][]any{{int64(1), int64(3), "a"}, {int64(1), int64(1), "b"}, {int64(1), int64(3), "c"}}
	rev := [][]any{rows[2], rows[1], rows[0]}
	mk := func(r [][]any) string {
		out, err := Assemble(s, roots(t, 1), []Input{{Fragment: fIncl, Columns: cols("_root", "BIGINT", "pos", "INT", "kind", "VARCHAR"), Rows: r}})
		if err != nil {
			t.Fatal(err)
		}
		return doc.MustJSON(out.Docs[roots(t, 1)[0].Key])
	}
	if mk(rows) != mk(rev) {
		t.Fatal("list order must not depend on query row order")
	}
}

func TestDuplicateOneRowIsARootError(t *testing.T) {
	s := buildSchema(t)
	out, err := Assemble(s, roots(t, 1, 2), []Input{{Fragment: fDetails, Columns: cols("_root", "BIGINT", "carat", "DECIMAL", "color", "VARCHAR"), Rows: [][]any{
		{int64(1), []byte("1"), "D"}, {int64(1), []byte("2"), "E"}, {int64(2), []byte("3"), "F"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := out.Errors[roots(t, 1)[0].Key]; !bad {
		t.Fatal("root 1 should fail: two rows from a cardinality-one fragment")
	}
	if _, ok := out.Docs[roots(t, 2)[0].Key]; !ok {
		t.Fatal("root 2 is unaffected")
	}
}

func TestDrift(t *testing.T) {
	s := buildSchema(t)
	_, err := Assemble(s, roots(t, 1), []Input{{Fragment: fDetails, Columns: cols("_root", "BIGINT", "carat", "DECIMAL", "colour", "VARCHAR")}})
	if !errors.Is(err, ErrSchemaDrift) {
		t.Fatalf("renamed column must be drift, got %v", err)
	}
	_, err = Assemble(s, roots(t, 1), []Input{{Fragment: fDetails, Columns: cols("_root", "BIGINT", "carat", "FLOAT", "color", "VARCHAR")}})
	if !errors.Is(err, ErrSchemaDrift) {
		t.Fatalf("changed type must be drift, got %v", err)
	}
}
