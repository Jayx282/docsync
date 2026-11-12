// Package routing turns changed source rows into the root documents they affect.
//
// Every tracked table has a path of hops to the root table. A hop says: rows of
// From whose FromCols equal rows of To on ToCols are related. Given the values of
// the first hop's FromCols for the changed rows (old and new), routing walks the
// path hop by hop until it reaches root keys. Both link kinds (references and
// referenced_by) are walked the same way; the kind only documents cardinality.
package routing

import (
	"context"
	"fmt"
	"strings"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/keyenc"
)

// Hop is one step on a table's path to the root.
type Hop struct {
	From     config.TableName
	FromCols []string
	To       config.TableName
	ToCols   []string
	Kind     string
}

// TablePlan is the routing plan for one tracked table.
type TablePlan struct {
	Table          config.TableName
	Key            []string
	IsRoot         bool
	Hops           []Hop
	HotFanoutLimit int
	// NeedsKeyMap is true when the first hop uses columns outside the primary key,
	// so old values must be remembered to route deletes and moves.
	NeedsKeyMap bool
}

// FromCols returns the columns whose values start routing (empty for the root).
func (t *TablePlan) FromCols() []string {
	if len(t.Hops) == 0 {
		return nil
	}
	return t.Hops[0].FromCols
}

// Plan is the routing plan for the whole configuration.
type Plan struct {
	Root    config.TableName
	RootKey string
	// RootFilter is the SQL condition (alias t) a root row must meet to be a document.
	RootFilter string
	Tables     []*TablePlan // root first, then routing rules in config order
	Static     []config.TableName
	byKey      map[string]*TablePlan
}

// Build derives the plan from a validated configuration.
func Build(cfg *config.Config) (*Plan, error) {
	root := cfg.RootTable()
	p := &Plan{Root: root, RootKey: cfg.Document.Root.Key[0], RootFilter: strings.TrimSpace(cfg.Document.Root.Filter), byKey: map[string]*TablePlan{}}
	rootPlan := &TablePlan{Table: root, Key: cfg.Document.Root.Key, IsRoot: true}
	p.Tables = append(p.Tables, rootPlan)
	p.byKey[root.Key()] = rootPlan

	rules := map[string]config.RoutingRule{}
	for _, r := range cfg.Routing {
		t := config.MustParseTableName(r.Table)
		if r.Static {
			p.Static = append(p.Static, t)
			continue
		}
		rules[t.Key()] = r
	}
	for _, r := range cfg.Routing {
		if r.Static {
			continue
		}
		t := config.MustParseTableName(r.Table)
		tp := &TablePlan{Table: t, Key: r.Key, HotFanoutLimit: r.HotFanoutLimit}
		cur := r
		curTable := t
		for {
			next := config.MustParseTableName(cur.Next.Table)
			pairs := cur.Next.Pairs()
			h := Hop{From: curTable, To: next, Kind: cur.Next.Kind}
			for _, pr := range pairs {
				h.FromCols = append(h.FromCols, pr.From)
				h.ToCols = append(h.ToCols, pr.To)
			}
			tp.Hops = append(tp.Hops, h)
			if next.Key() == root.Key() {
				break
			}
			nr, ok := rules[next.Key()]
			if !ok {
				return nil, fmt.Errorf("routing: %s has no rule", next)
			}
			if len(tp.Hops) > 64 {
				return nil, fmt.Errorf("routing: path from %s is too long (cycle?)", t)
			}
			cur, curTable = nr, next
		}
		tp.NeedsKeyMap = !subset(tp.Hops[0].FromCols, tp.Key)
		p.Tables = append(p.Tables, tp)
		p.byKey[t.Key()] = tp
	}
	return p, nil
}

// Table returns the plan for a table.
func (p *Plan) Table(t config.TableName) (*TablePlan, bool) {
	tp, ok := p.byKey[t.Key()]
	return tp, ok
}

func subset(cols, of []string) bool {
	for _, c := range cols {
		found := false
		for _, o := range of {
			if strings.EqualFold(c, o) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sameCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// Querier looks up related rows in the source database (inside the cycle's snapshot).
type Querier interface {
	// Lookup returns the DISTINCT values of want for rows of table whose match columns
	// equal one of the given tuples.
	Lookup(ctx context.Context, table config.TableName, match []string, tuples [][]any, want []string) ([][]any, error)
}

// TupleSet is a set of value tuples with stable insertion order.
type TupleSet struct {
	seen map[string]bool
	rows [][]any
}

// NewTupleSet returns an empty set.
func NewTupleSet() *TupleSet { return &TupleSet{seen: map[string]bool{}} }

// Add inserts a tuple unless it contains NULL (NULL never joins) or is already present.
func (s *TupleSet) Add(t []any) error {
	for _, v := range t {
		if v == nil {
			return nil
		}
	}
	k, err := TupleKey(t)
	if err != nil {
		return err
	}
	if !s.seen[k] {
		s.seen[k] = true
		s.rows = append(s.rows, t)
	}
	return nil
}

// Rows returns the tuples.
func (s *TupleSet) Rows() [][]any { return s.rows }

// Len returns the number of tuples.
func (s *TupleSet) Len() int { return len(s.rows) }

// TupleKey encodes a tuple of typed values as a map key.
func TupleKey(t []any) (string, error) {
	var out []byte
	for _, v := range t {
		p, err := doc.KeyPrimitive(v)
		if err != nil {
			return "", err
		}
		if out, err = keyenc.Append(out, p); err != nil {
			return "", err
		}
	}
	return string(out), nil
}

// Propagate walks tp's hops starting from tuples of tp.FromCols() and returns the
// root key values reached. NULL values never match.
func Propagate(ctx context.Context, q Querier, p *Plan, tp *TablePlan, start [][]any) ([]any, error) {
	if tp.IsRoot {
		return nil, fmt.Errorf("routing: root table changes are their own roots")
	}
	cur := start
	for i, h := range tp.Hops {
		if len(cur) == 0 {
			return nil, nil
		}
		if h.To.Key() == p.Root.Key() {
			if sameCols(h.ToCols, []string{p.RootKey}) {
				return firstColumn(cur), nil
			}
			rows, err := q.Lookup(ctx, h.To, h.ToCols, cur, []string{p.RootKey})
			if err != nil {
				return nil, err
			}
			return firstColumn(dedupe(rows)), nil
		}
		next := tp.Hops[i+1]
		if sameCols(next.FromCols, h.ToCols) {
			continue // the same column values flow straight into the next hop
		}
		rows, err := q.Lookup(ctx, h.To, h.ToCols, cur, next.FromCols)
		if err != nil {
			return nil, err
		}
		cur = dedupe(rows)
	}
	return nil, fmt.Errorf("routing: path from %s does not reach the root", tp.Table)
}

func firstColumn(rows [][]any) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 && r[0] != nil {
			out = append(out, r[0])
		}
	}
	return out
}

func dedupe(rows [][]any) [][]any {
	s := NewTupleSet()
	for _, r := range rows {
		_ = s.Add(r)
	}
	return s.Rows()
}
