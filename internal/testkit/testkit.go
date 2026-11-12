// Package testkit provides helpers for docsync's integration tests: scratch
// databases, SQL scripts, and folding sink output back into documents.
package testkit

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/file"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/Jayx282/docsync/internal/sink"
)

// CreateDatabase creates a scratch database and returns its name and a DSN for it.
// The database is dropped when the test ends.
func CreateDatabase(t testing.TB, serverDSN string) (string, string) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlserver", serverDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	name := fmt.Sprintf("docsync_it_%d_%04d", time.Now().Unix(), rand.Intn(10000))
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("sqlserver", serverDSN)
		if err != nil {
			return
		}
		defer db.Close()
		_, _ = db.Exec(fmt.Sprintf("ALTER DATABASE %s SET SINGLE_USER WITH ROLLBACK IMMEDIATE; DROP DATABASE %s", name, name))
	})
	u, err := url.Parse(serverDSN)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("database", name)
	u.RawQuery = q.Encode()
	return name, u.String()
}

var goLine = regexp.MustCompile(`(?im)^\s*GO\s*$`)

// SplitBatches splits a T-SQL script on GO lines.
func SplitBatches(script string) []string {
	var out []string
	for _, b := range goLine.Split(script, -1) {
		if strings.TrimSpace(stripComments(b)) != "" {
			out = append(out, b)
		}
	}
	return out
}

func stripComments(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

// ApplySQLFile runs every batch of a script.
func ApplySQLFile(t testing.TB, db *sql.DB, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range SplitBatches(string(b)) {
		if _, err := db.Exec(batch); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, batch)
		}
	}
}

// Folded is the consumer's view of a sink: root JSON -> document JSON.
type Folded struct {
	Docs   map[string]string
	Latest uint64
	Base   uint64
	Epoch  uint64
}

func readManifest(dir string) (*sink.Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m sink.Manifest
	return &m, json.Unmarshal(b, &m)
}

// FoldNDJSON loads an ndjson log sink: base, then segments, applying only
// generations newer than the base, exactly as a consumer would.
func FoldNDJSON(dir string) (*Folded, error) {
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	f := &Folded{Docs: map[string]string{}, Latest: m.LatestGeneration, Epoch: m.Epoch}
	type line struct {
		Gen  uint64          `json:"gen"`
		Op   string          `json:"op"`
		Root json.RawMessage `json:"root"`
		Doc  json.RawMessage `json:"doc"`
	}
	apply := func(path string, minGen uint64) error {
		fh, err := os.Open(path)
		if err != nil {
			return err
		}
		defer fh.Close()
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var l line
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if l.Gen <= minGen {
				continue
			}
			if l.Op == "delete" {
				delete(f.Docs, string(l.Root))
			} else {
				f.Docs[string(l.Root)] = string(l.Doc)
			}
		}
		return sc.Err()
	}
	if m.Base != nil {
		f.Base = m.Base.Generation
		for _, name := range m.Base.Files {
			if err := apply(filepath.Join(dir, filepath.FromSlash(m.Base.Path), name), 0); err != nil {
				return nil, err
			}
		}
	}
	for _, seg := range m.Segments {
		if err := apply(filepath.Join(dir, filepath.FromSlash(seg.Path)), f.Base); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// FoldParquet loads a parquet_log sink written with include_json, the same way.
func FoldParquet(dir string) (*Folded, error) {
	m, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	f := &Folded{Docs: map[string]string{}, Latest: m.LatestGeneration, Epoch: m.Epoch}
	apply := func(path string, minGen uint64) error {
		rdr, err := file.OpenParquetFile(path, false)
		if err != nil {
			return err
		}
		defer rdr.Close()
		fr, err := pqarrow.NewFileReader(rdr, pqarrow.ArrowReadProperties{BatchSize: 4096}, memory.DefaultAllocator)
		if err != nil {
			return err
		}
		tbl, err := fr.ReadTable(context.Background())
		if err != nil {
			return err
		}
		defer tbl.Release()
		tr := array.NewTableReader(tbl, 4096)
		defer tr.Release()
		for tr.Next() {
			rec := tr.RecordBatch()
			idx := func(name string) int {
				ids := rec.Schema().FieldIndices(name)
				if len(ids) == 0 {
					return -1
				}
				return ids[0]
			}
			gi, oi, ri, ji := idx("_gen"), idx("_op"), idx("_root"), idx("_doc_json")
			if gi < 0 || oi < 0 || ri < 0 || ji < 0 {
				return fmt.Errorf("%s: missing metadata columns (is include_json on?)", path)
			}
			gens := rec.Column(gi).(*array.Int64)
			ops := rec.Column(oi).(*array.String)
			roots := rec.Column(ri)
			docs := rec.Column(ji).(*array.String)
			for i := 0; i < int(rec.NumRows()); i++ {
				if uint64(gens.Value(i)) <= minGen {
					continue
				}
				rootJSON, err := json.Marshal(roots.GetOneForMarshal(i))
				if err != nil {
					return err
				}
				if ops.Value(i) == "delete" {
					delete(f.Docs, string(rootJSON))
				} else {
					f.Docs[string(rootJSON)] = docs.Value(i)
				}
			}
		}
		return tr.Err()
	}
	if m.Base != nil {
		f.Base = m.Base.Generation
		for _, name := range m.Base.Files {
			if err := apply(filepath.Join(dir, filepath.FromSlash(m.Base.Path), name), 0); err != nil {
				return nil, err
			}
		}
	}
	for _, seg := range m.Segments {
		if err := apply(filepath.Join(dir, filepath.FromSlash(seg.Path)), f.Base); err != nil {
			return nil, err
		}
	}
	return f, nil
}

// Diff describes differences between two folded views (at most max lines).
func Diff(want, got map[string]string, max int) []string {
	var out []string
	for k, w := range want {
		g, ok := got[k]
		switch {
		case !ok:
			out = append(out, fmt.Sprintf("missing root %s (want %s)", k, w))
		case g != w:
			out = append(out, fmt.Sprintf("root %s differs:\n  want %s\n  got  %s", k, w, g))
		}
		if len(out) >= max {
			return out
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			out = append(out, fmt.Sprintf("unexpected root %s (got %s)", k, g))
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}

var _ arrow.Array = (*array.Int64)(nil)
