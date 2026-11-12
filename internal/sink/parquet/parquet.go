// Package parquet writes the parquet_log sink's files with Apache Arrow.
package parquet

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/decimal128"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"

	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/sink"
)

// Metadata columns written before the document fields.
const (
	ColGen     = "_gen"
	ColOp      = "_op"
	ColRoot    = "_root"
	ColDocJSON = "_doc_json"
)

const batchRows = 16_384

// Format implements sink.Format for Parquet.
type Format struct {
	IncludeJSON bool
}

// Name implements sink.Format.
func (Format) Name() string { return "docsync-parquet-log" }

// Ext implements sink.Format.
func (Format) Ext() string { return "parquet" }

// ArrowSchema returns the Arrow schema of segment and base files.
func ArrowSchema(s *schema.Schema, includeJSON bool) (*arrow.Schema, error) {
	rootType, err := ArrowType(s.RootKey.Type)
	if err != nil {
		return nil, fmt.Errorf("root key: %w", err)
	}
	fields := []arrow.Field{
		{Name: ColGen, Type: arrow.PrimitiveTypes.Int64},
		{Name: ColOp, Type: arrow.BinaryTypes.String},
		{Name: ColRoot, Type: rootType},
	}
	if includeJSON {
		fields = append(fields, arrow.Field{Name: ColDocJSON, Type: arrow.BinaryTypes.String, Nullable: true})
	}
	for _, f := range s.Fields {
		t, err := ArrowType(f.Type)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", f.Name, err)
		}
		fields = append(fields, arrow.Field{Name: f.Name, Type: t, Nullable: true})
	}
	md := arrow.NewMetadata([]string{"docsync.document", "docsync.root_key", "docsync.schema_hash"},
		[]string{s.Document, s.RootKey.Name, s.Hash()})
	return arrow.NewSchema(fields, &md), nil
}

// ArrowType maps a logical type to an Arrow type.
func ArrowType(t schema.Type) (arrow.DataType, error) {
	switch t.Kind {
	case schema.Bool:
		return arrow.FixedWidthTypes.Boolean, nil
	case schema.UInt8:
		return arrow.PrimitiveTypes.Uint8, nil
	case schema.Int16:
		return arrow.PrimitiveTypes.Int16, nil
	case schema.Int32:
		return arrow.PrimitiveTypes.Int32, nil
	case schema.Int64:
		return arrow.PrimitiveTypes.Int64, nil
	case schema.Float32:
		return arrow.PrimitiveTypes.Float32, nil
	case schema.Float64:
		return arrow.PrimitiveTypes.Float64, nil
	case schema.Decimal:
		return &arrow.Decimal128Type{Precision: t.Precision, Scale: t.Scale}, nil
	case schema.String, schema.UUID:
		return arrow.BinaryTypes.String, nil
	case schema.Date:
		return arrow.FixedWidthTypes.Date32, nil
	case schema.Timestamp:
		return &arrow.TimestampType{Unit: arrow.Microsecond}, nil
	case schema.TimestampTZ:
		return &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}, nil
	case schema.Time:
		return arrow.FixedWidthTypes.Time64us, nil
	case schema.Binary:
		return arrow.BinaryTypes.Binary, nil
	case schema.Struct:
		fs := make([]arrow.Field, 0, len(t.Fields))
		for _, f := range t.Fields {
			ft, err := ArrowType(f.Type)
			if err != nil {
				return nil, err
			}
			fs = append(fs, arrow.Field{Name: f.Name, Type: ft, Nullable: true})
		}
		return arrow.StructOf(fs...), nil
	case schema.List:
		if t.Elem == nil {
			return nil, fmt.Errorf("list without element type")
		}
		et, err := ArrowType(*t.Elem)
		if err != nil {
			return nil, err
		}
		return arrow.ListOf(et), nil
	}
	return nil, fmt.Errorf("unsupported kind %s", t.Kind)
}

type row struct {
	gen  uint64
	op   string
	root any
	doc  doc.Object
}

type writer struct {
	s           *schema.Schema
	includeJSON bool
	as          *arrow.Schema
	f           *sink.AtomicFile
	fw          *pqarrow.FileWriter
	rb          *array.RecordBuilder
	n           int
}

func newWriter(path string, s *schema.Schema, includeJSON bool) (*writer, error) {
	as, err := ArrowSchema(s, includeJSON)
	if err != nil {
		return nil, err
	}
	f, err := sink.CreateAtomic(path)
	if err != nil {
		return nil, err
	}
	props := parquet.NewWriterProperties(
		parquet.WithCompression(compress.Codecs.Zstd),
		parquet.WithMaxRowGroupLength(128*1024),
		parquet.WithCreatedBy("docsync"),
	)
	// writeOnly hides Close so the Parquet writer does not close the file; the
	// sink syncs, closes and renames it in Finish.
	fw, err := pqarrow.NewFileWriter(as, writeOnly{f.File}, props, pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()))
	if err != nil {
		f.Abort()
		return nil, err
	}
	return &writer{s: s, includeJSON: includeJSON, as: as, f: f, fw: fw, rb: array.NewRecordBuilder(memory.DefaultAllocator, as)}, nil
}

func (w *writer) add(r row) error {
	b := w.rb
	b.Field(0).(*array.Int64Builder).Append(int64(r.gen))
	b.Field(1).(*array.StringBuilder).Append(r.op)
	if err := appendValue(b.Field(2), w.s.RootKey.Type, r.root); err != nil {
		return fmt.Errorf("_root: %w", err)
	}
	i := 3
	if w.includeJSON {
		if r.doc == nil {
			b.Field(i).AppendNull()
		} else {
			j, err := doc.JSON(r.doc)
			if err != nil {
				return err
			}
			b.Field(i).(*array.StringBuilder).Append(string(j))
		}
		i++
	}
	for _, f := range w.s.Fields {
		var v any
		if r.doc != nil {
			v = r.doc[f.Name]
		}
		if err := appendValue(b.Field(i), f.Type, v); err != nil {
			return fmt.Errorf("field %s: %w", f.Name, err)
		}
		i++
	}
	w.n++
	if w.n >= batchRows {
		return w.flush()
	}
	return nil
}

func (w *writer) flush() error {
	if w.n == 0 {
		return nil
	}
	rec := w.rb.NewRecordBatch()
	defer rec.Release()
	w.n = 0
	return w.fw.WriteBuffered(rec)
}

func (w *writer) finish() error {
	if err := w.flush(); err != nil {
		w.abort()
		return err
	}
	w.rb.Release()
	if err := w.fw.Close(); err != nil { // writes the footer; the file stays open (see writeOnly)
		w.f.Abort()
		return err
	}
	return w.f.Finish()
}

type writeOnly struct{ io.Writer }

func (w *writer) abort() {
	w.rb.Release()
	w.f.Abort()
}

// WriteSegment implements sink.Format.
func (f Format) WriteSegment(path string, s *schema.Schema, gens []sink.Generation) (int, int, error) {
	w, err := newWriter(path, s, f.IncludeJSON)
	if err != nil {
		return 0, 0, err
	}
	ups, dels := 0, 0
	for _, g := range gens {
		for _, c := range g.Changes {
			r := row{gen: g.Gen, op: "upsert", root: c.Root, doc: c.Doc}
			if c.Delete {
				r.op, r.doc = "delete", nil
				dels++
			} else {
				ups++
			}
			if err := w.add(r); err != nil {
				w.abort()
				return 0, 0, err
			}
		}
	}
	return ups, dels, w.finish()
}

// WriteBase implements sink.Format.
func (f Format) WriteBase(dir string, s *schema.Schema, next func() (sink.BaseDoc, bool, error), rowsPerFile int) ([]string, int64, error) {
	var files []string
	var total int64
	var w *writer
	rows := 0
	for {
		d, ok, err := next()
		if err != nil {
			if w != nil {
				w.abort()
			}
			return nil, 0, err
		}
		if !ok {
			break
		}
		if w == nil || rows >= rowsPerFile {
			if w != nil {
				if err := w.finish(); err != nil {
					return nil, 0, err
				}
			}
			name := fmt.Sprintf("part-%05d.parquet", len(files))
			if w, err = newWriter(filepath.Join(dir, name), s, f.IncludeJSON); err != nil {
				return nil, 0, err
			}
			files = append(files, name)
			rows = 0
		}
		if err := w.add(row{gen: d.Gen, op: "upsert", root: d.Root, doc: d.Doc}); err != nil {
			w.abort()
			return nil, 0, err
		}
		rows++
		total++
	}
	if w != nil {
		if err := w.finish(); err != nil {
			return nil, 0, err
		}
	}
	return files, total, nil
}

func appendValue(b array.Builder, t schema.Type, v any) error {
	if v == nil {
		b.AppendNull()
		return nil
	}
	mismatch := func() error { return fmt.Errorf("value of type %s does not match %s", doc.TypeName(v), t.Kind) }
	switch t.Kind {
	case schema.Bool:
		x, ok := v.(bool)
		if !ok {
			return mismatch()
		}
		b.(*array.BooleanBuilder).Append(x)
	case schema.UInt8, schema.Int16, schema.Int32, schema.Int64:
		x, ok := v.(int64)
		if !ok {
			return mismatch()
		}
		switch t.Kind {
		case schema.UInt8:
			b.(*array.Uint8Builder).Append(uint8(x))
		case schema.Int16:
			b.(*array.Int16Builder).Append(int16(x))
		case schema.Int32:
			b.(*array.Int32Builder).Append(int32(x))
		default:
			b.(*array.Int64Builder).Append(x)
		}
	case schema.Float32:
		x, ok := v.(float64)
		if !ok {
			return mismatch()
		}
		b.(*array.Float32Builder).Append(float32(x))
	case schema.Float64:
		x, ok := v.(float64)
		if !ok {
			return mismatch()
		}
		b.(*array.Float64Builder).Append(x)
	case schema.Decimal:
		x, ok := v.(doc.Decimal)
		if !ok {
			return mismatch()
		}
		n, err := decimal128.FromString(string(x), t.Precision, t.Scale)
		if err != nil {
			return err
		}
		b.(*array.Decimal128Builder).Append(n)
	case schema.String:
		x, ok := v.(string)
		if !ok {
			return mismatch()
		}
		b.(*array.StringBuilder).Append(x)
	case schema.UUID:
		x, ok := v.(doc.UUID)
		if !ok {
			return mismatch()
		}
		b.(*array.StringBuilder).Append(string(x))
	case schema.Date:
		x, ok := v.(doc.Date)
		if !ok {
			return mismatch()
		}
		b.(*array.Date32Builder).Append(arrow.Date32(x))
	case schema.Timestamp:
		x, ok := v.(doc.Timestamp)
		if !ok {
			return mismatch()
		}
		b.(*array.TimestampBuilder).Append(arrow.Timestamp(x))
	case schema.TimestampTZ:
		x, ok := v.(doc.TimestampTZ)
		if !ok {
			return mismatch()
		}
		b.(*array.TimestampBuilder).Append(arrow.Timestamp(x))
	case schema.Time:
		x, ok := v.(doc.TimeOfDay)
		if !ok {
			return mismatch()
		}
		b.(*array.Time64Builder).Append(arrow.Time64(x))
	case schema.Binary:
		x, ok := v.([]byte)
		if !ok {
			return mismatch()
		}
		b.(*array.BinaryBuilder).Append(x)
	case schema.Struct:
		x, ok := v.(doc.Object)
		if !ok {
			return mismatch()
		}
		sb := b.(*array.StructBuilder)
		sb.Append(true)
		for i, f := range t.Fields {
			if err := appendValue(sb.FieldBuilder(i), f.Type, x[f.Name]); err != nil {
				return fmt.Errorf("%s: %w", f.Name, err)
			}
		}
	case schema.List:
		x, ok := v.(doc.List)
		if !ok {
			return mismatch()
		}
		lb := b.(*array.ListBuilder)
		lb.Append(true)
		vb := lb.ValueBuilder()
		for _, e := range x {
			if err := appendValue(vb, *t.Elem, e); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported kind %s", t.Kind)
	}
	return nil
}
