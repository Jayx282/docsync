// Package assemble merges fragment query results into documents.
package assemble

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
)

// ErrSchemaDrift means a fragment's live result columns no longer match the stored schema.
var ErrSchemaDrift = errors.New("schema drift")

// Input is the raw result of one fragment query.
type Input struct {
	Fragment config.Fragment
	Columns  []schema.Column
	Rows     [][]any
}

// Root is a root id with its encoded map key.
type Root struct {
	Value any
	Key   string
}

// NewRoot builds a Root from a typed key value.
func NewRoot(v any) (Root, error) {
	k, err := routing.TupleKey([]any{v})
	if err != nil {
		return Root{}, err
	}
	return Root{Value: v, Key: k}, nil
}

// Output holds assembled documents and per-root failures.
type Output struct {
	Docs   map[string]doc.Object
	Errors map[string]string
}

type layout struct {
	frag    config.Fragment
	rootIdx int
	colIdx  []int
	fields  []schema.Field
	sortIdx []int
}

// Assemble builds one document per root. A data problem confined to one root
// (for example two rows from a cardinality-one fragment) becomes an entry in
// Output.Errors; a structural problem (schema drift) fails the whole call.
func Assemble(s *schema.Schema, roots []Root, inputs []Input) (*Output, error) {
	layouts := make([]layout, len(inputs))
	for i, in := range inputs {
		l, err := buildLayout(s, in)
		if err != nil {
			return nil, err
		}
		layouts[i] = l
	}
	out := &Output{Docs: make(map[string]doc.Object, len(roots)), Errors: map[string]string{}}
	for _, r := range roots {
		out.Docs[r.Key] = emptyDoc(s, layouts)
	}
	for i, in := range inputs {
		l := layouts[i]
		seenOne := map[string]bool{}
		for _, row := range in.Rows {
			rv, err := schema.Convert(s.RootKey.Type, row[l.rootIdx])
			if err != nil {
				return nil, fmt.Errorf("fragment %s: _root: %w", l.frag.Name, err)
			}
			root, err := NewRoot(rv)
			if err != nil {
				return nil, err
			}
			d, ok := out.Docs[root.Key]
			if !ok {
				continue // root not requested (or already failed)
			}
			obj := make(doc.Object, len(l.fields))
			for j, f := range l.fields {
				v, err := schema.Convert(f.Type, row[l.colIdx[j]])
				if err != nil {
					fail(out, root.Key, fmt.Sprintf("fragment %s, column %s: %v", l.frag.Name, f.Name, err))
					obj = nil
					break
				}
				obj[f.Name] = v
			}
			if obj == nil {
				continue
			}
			switch {
			case l.frag.Cardinality == config.CardinalityMany:
				d[l.frag.Path] = append(d[l.frag.Path].(doc.List), obj)
			default:
				if seenOne[root.Key] {
					fail(out, root.Key, fmt.Sprintf("fragment %s returned more than one row (cardinality one)", l.frag.Name))
					continue
				}
				seenOne[root.Key] = true
				if l.frag.Path != "" {
					d[l.frag.Path] = obj
				} else {
					for k, v := range obj {
						d[k] = v
					}
				}
			}
		}
	}
	for key, d := range out.Docs {
		for _, l := range layouts {
			if l.frag.Cardinality != config.CardinalityMany {
				continue
			}
			sortList(d[l.frag.Path].(doc.List), l)
		}
		out.Docs[key] = d
	}
	return out, nil
}

func fail(out *Output, key, msg string) {
	if _, ok := out.Errors[key]; !ok {
		out.Errors[key] = msg
	}
	delete(out.Docs, key)
}

func emptyDoc(s *schema.Schema, layouts []layout) doc.Object {
	d := make(doc.Object, len(s.Fields))
	for _, f := range s.Fields {
		if f.Type.Kind == schema.List {
			d[f.Name] = doc.List{}
		} else {
			d[f.Name] = nil
		}
	}
	return d
}

func buildLayout(s *schema.Schema, in Input) (layout, error) {
	l := layout{frag: in.Fragment, rootIdx: -1}
	var want []schema.Field
	switch {
	case in.Fragment.Cardinality == config.CardinalityMany:
		f, ok := s.FieldByName(in.Fragment.Path)
		if !ok || f.Type.Kind != schema.List || f.Type.Elem == nil {
			return l, fmt.Errorf("%w: fragment %s: list field %s missing from schema", ErrSchemaDrift, in.Fragment.Name, in.Fragment.Path)
		}
		want = f.Type.Elem.Fields
	case in.Fragment.Path != "":
		f, ok := s.FieldByName(in.Fragment.Path)
		if !ok || f.Type.Kind != schema.Struct {
			return l, fmt.Errorf("%w: fragment %s: struct field %s missing from schema", ErrSchemaDrift, in.Fragment.Name, in.Fragment.Path)
		}
		want = f.Type.Fields
	default:
		for _, c := range in.Columns {
			if strings.EqualFold(c.Name, config.RootColumn) {
				continue
			}
			f, ok := s.FieldByName(c.Name)
			if !ok {
				return l, fmt.Errorf("%w: fragment %s: new column %s", ErrSchemaDrift, in.Fragment.Name, c.Name)
			}
			want = append(want, f)
		}
	}
	idx := 0
	for i, c := range in.Columns {
		if strings.EqualFold(c.Name, config.RootColumn) {
			l.rootIdx = i
			continue
		}
		if idx >= len(want) {
			return l, fmt.Errorf("%w: fragment %s: unexpected column %s", ErrSchemaDrift, in.Fragment.Name, c.Name)
		}
		f := want[idx]
		lt, err := schema.TypeForSQL(c)
		if err != nil {
			return l, fmt.Errorf("%w: fragment %s: %v", ErrSchemaDrift, in.Fragment.Name, err)
		}
		if !strings.EqualFold(f.Name, c.Name) || !sameType(lt, f.Type) {
			return l, fmt.Errorf("%w: fragment %s: column %d is now %s %s, schema has %s %s",
				ErrSchemaDrift, in.Fragment.Name, i, c.Name, lt.SQLType, f.Name, f.Type.SQLType)
		}
		l.colIdx = append(l.colIdx, i)
		l.fields = append(l.fields, f)
		idx++
	}
	if l.rootIdx < 0 {
		return l, fmt.Errorf("%w: fragment %s no longer selects %s", ErrSchemaDrift, in.Fragment.Name, config.RootColumn)
	}
	if idx != len(want) {
		return l, fmt.Errorf("%w: fragment %s returns %d columns, schema expects %d", ErrSchemaDrift, in.Fragment.Name, idx, len(want))
	}
	for _, k := range in.Fragment.SortKey {
		found := false
		for j, f := range l.fields {
			if strings.EqualFold(f.Name, k) {
				l.sortIdx = append(l.sortIdx, j)
				found = true
				break
			}
		}
		if !found {
			return l, fmt.Errorf("%w: fragment %s: sort_key column %s missing", ErrSchemaDrift, in.Fragment.Name, k)
		}
	}
	return l, nil
}

func sameType(a, b schema.Type) bool {
	return a.Kind == b.Kind && a.Precision == b.Precision && a.Scale == b.Scale && a.SQLType == b.SQLType
}

// sortList orders list elements by the sort key, then by full encoding so that
// ties are still deterministic.
func sortList(list doc.List, l layout) {
	if len(list) < 2 {
		return
	}
	encs := make([][]byte, len(list))
	for i, e := range list {
		encs[i] = doc.MustEncode(e)
	}
	idx := make([]int, len(list))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		oa, ob := list[idx[a]].(doc.Object), list[idx[b]].(doc.Object)
		for _, si := range l.sortIdx {
			name := l.fields[si].Name
			if c := doc.Compare(oa[name], ob[name]); c != 0 {
				return c < 0
			}
		}
		return bytes.Compare(encs[idx[a]], encs[idx[b]]) < 0
	})
	sorted := make(doc.List, len(list))
	for i, j := range idx {
		sorted[i] = list[j]
	}
	copy(list, sorted)
}
