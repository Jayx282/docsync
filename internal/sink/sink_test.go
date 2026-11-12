package sink_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/sink"
	"github.com/Jayx282/docsync/internal/sink/parquet"
	"github.com/Jayx282/docsync/internal/testkit"
)

func testSchema(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Build("stone", "stoneid", schema.Column{Name: "stoneid", DBType: "BIGINT"}, []schema.FragmentColumns{
		{Fragment: config.Fragment{Name: "d", Cardinality: "one", SQL: "x"}, Columns: []schema.Column{
			{Name: "_root", DBType: "BIGINT"}, {Name: "carat", DBType: "DECIMAL", Precision: 10, Scale: 3},
			{Name: "seen", DBType: "DATETIME2"}, {Name: "guid", DBType: "UNIQUEIDENTIFIER"},
		}},
		{Fragment: config.Fragment{Name: "i", Cardinality: "many", Path: "inclusions", SortKey: []string{"pos"}, SQL: "x"}, Columns: []schema.Column{
			{Name: "_root", DBType: "BIGINT"}, {Name: "pos", DBType: "INT"}, {Name: "kind", DBType: "VARCHAR"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mkDoc(id int64) doc.Object {
	return doc.Object{
		"carat": doc.Decimal("1.250"), "seen": doc.TimestampOf(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
		"guid":       doc.UUID("6F9619FF-8B86-D011-B42D-00C04FC964FF"),
		"inclusions": doc.List{doc.Object{"pos": id, "kind": "cloud"}},
	}
}

func run(t *testing.T, s sink.Sink, sch *schema.Schema, dir string, fold func(string) (*testkit.Folded, error)) {
	ctx := context.Background()
	// No base yet for epoch 1: writing segments must be refused.
	if err := s.Write(ctx, sch, []sink.Generation{{Gen: 1, Epoch: 1}}); err == nil {
		t.Fatal("segments before the epoch's base must be refused")
	}
	docs := []sink.BaseDoc{{Root: int64(1), Gen: 3, Doc: mkDoc(1)}, {Root: int64(2), Gen: 3, Doc: mkDoc(2)}}
	i := 0
	next := func() (sink.BaseDoc, bool, error) {
		if i >= len(docs) {
			return sink.BaseDoc{}, false, nil
		}
		i++
		return docs[i-1], true, nil
	}
	if err := s.WriteBase(ctx, sch, 1, 3, next); err != nil {
		t.Fatal(err)
	}
	gens := []sink.Generation{
		{Gen: 4, Epoch: 1, Changes: []sink.Change{{Root: int64(3), Doc: mkDoc(3)}}},
		{Gen: 5, Epoch: 1, Changes: []sink.Change{{Root: int64(1), Delete: true}}},
	}
	if err := s.Write(ctx, sch, gens); err != nil {
		t.Fatal(err)
	}
	// Replaying already-written generations is a no-op; a gap is an error.
	if err := s.Write(ctx, sch, gens[1:]); err != nil {
		t.Fatalf("idempotent replay failed: %v", err)
	}
	if err := s.Write(ctx, sch, []sink.Generation{{Gen: 9, Epoch: 1}}); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("want gap error, got %v", err)
	}
	if gen, epoch := s.Position(); gen != 5 || epoch != 1 {
		t.Fatalf("position = %d/%d, want 5/1", gen, epoch)
	}
	f, err := fold(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Docs) != 2 || f.Docs["1"] != "" || f.Docs["3"] == "" {
		t.Fatalf("folded docs = %v", f.Docs)
	}
	if want := doc.MustJSON(mkDoc(2)); f.Docs["2"] != want {
		t.Fatalf("doc 2 = %s, want %s", f.Docs["2"], want)
	}
	// No temp files are left behind.
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if strings.HasSuffix(p, ".tmp") {
			t.Fatalf("leftover temp file %s", p)
		}
		return nil
	})
}

func TestNDJSONSink(t *testing.T) {
	dir := t.TempDir()
	s, err := sink.NewLogSink(sink.LogOptions{Name: "n", Dir: dir, Document: "stone", RootKey: "stoneid", BaseFileRows: 1, RetainSegments: time.Hour}, sink.NDJSON{})
	if err != nil {
		t.Fatal(err)
	}
	run(t, s, testSchema(t), dir, testkit.FoldNDJSON)
}

func TestParquetSink(t *testing.T) {
	dir := t.TempDir()
	s, err := sink.NewLogSink(sink.LogOptions{Name: "p", Dir: dir, Document: "stone", RootKey: "stoneid", BaseFileRows: 1, RetainSegments: time.Hour}, parquet.Format{IncludeJSON: true})
	if err != nil {
		t.Fatal(err)
	}
	run(t, s, testSchema(t), dir, testkit.FoldParquet)
}

func TestReopenCleansUncommittedSegments(t *testing.T) {
	dir := t.TempDir()
	opt := sink.LogOptions{Name: "n", Dir: dir, Document: "stone", RootKey: "stoneid", BaseFileRows: 10}
	if _, err := sink.NewLogSink(opt, sink.NDJSON{}); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "segments", "00000000000000000001-00000000000000000001.ndjson")
	os.WriteFile(orphan, []byte("{}\n"), 0o644)
	os.WriteFile(orphan+".tmp", []byte("{}\n"), 0o644)
	if _, err := sink.NewLogSink(opt, sink.NDJSON{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("a segment not in the manifest must be removed on open")
	}
}
