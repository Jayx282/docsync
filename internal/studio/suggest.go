package studio

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
)

// Suggestion proposes a relation from the root to another table.
type Suggestion struct {
	ID      string     `json:"id"`
	Table   string     `json:"table"`
	Shape   Shape      `json:"shape"`
	Join    []JoinPair `json:"join"`
	Via     *Via       `json:"via,omitempty"`
	Path    string     `json:"path,omitempty"`
	Source  string     `json:"source"`  // where the suggestion comes from
	Guessed bool       `json:"guessed"` // true when inferred from names, not declared keys
	Unique  bool       `json:"unique"`  // at most one related row per document
	Rows    int64      `json:"rows"`
	Reason  string     `json:"reason"`
}

var keySuffix = regexp.MustCompile(`(?i)(_?id|_?code|_?key|_?no|_?num)$`)

// Suggest lists likely relations for a root table from foreign keys, matching column
// names, and bridge tables. Name-based suggestions are marked as guesses.
func Suggest(s *Snapshot, rootName string) ([]Suggestion, error) {
	root, ok := s.Table(rootName)
	if !ok {
		return nil, fmt.Errorf("table %s does not exist", rootName)
	}
	if len(root.PK) != 1 {
		return nil, fmt.Errorf("%s needs a single-column primary key to be a document root (it has %v)", root.Name, root.PK)
	}
	rootKey := root.PK[0]
	var out []Suggestion
	seen := map[string]bool{}
	add := func(sg Suggestion) {
		k := strings.ToLower(sg.Table)
		if sg.Via != nil {
			k += "|via|" + strings.ToLower(sg.Via.Table)
		}
		if seen[k] {
			return
		}
		seen[k] = true
		if t, ok := s.Table(sg.Table); ok {
			sg.Rows = t.Rows
		}
		out = append(out, sg)
	}
	direct := map[string][]JoinPair{} // tables that reference the root key: table -> join

	// 1. Declared foreign keys.
	for _, fk := range s.FKs {
		switch {
		case strings.EqualFold(fk.ToTable, root.Name) && !strings.EqualFold(fk.FromTable, root.Name):
			var join []JoinPair
			for i := range fk.FromCols {
				join = append(join, JoinPair{From: fk.ToCols[i], To: fk.FromCols[i]})
			}
			unique := s.IsUnique(fk.FromTable, fk.FromCols)
			shape, reason := ShapeList, fmt.Sprintf("%s rows point at %s, so each document can have many", short(fk.FromTable), short(root.Name))
			if unique {
				shape, reason = ShapeFields, fmt.Sprintf("each %s has at most one %s row", short(root.Name), short(fk.FromTable))
			}
			add(Suggestion{Table: fk.FromTable, Shape: shape, Join: join, Source: "foreign key " + fk.Name, Unique: unique, Reason: reason})
			if len(join) == 1 && strings.EqualFold(join[0].From, rootKey) {
				direct[strings.ToLower(fk.FromTable)] = join
			}
		case strings.EqualFold(fk.FromTable, root.Name) && !strings.EqualFold(fk.ToTable, root.Name):
			var join []JoinPair
			for i := range fk.FromCols {
				join = append(join, JoinPair{From: fk.FromCols[i], To: fk.ToCols[i]})
			}
			add(Suggestion{Table: fk.ToTable, Shape: ShapeFields, Join: join, Source: "foreign key " + fk.Name, Unique: true,
				Reason: fmt.Sprintf("each %s points at one %s row (a lookup)", short(root.Name), short(fk.ToTable))})
		}
	}

	tables := s.SortedTables()
	// 2. Child tables: another table has a column named like the root key.
	for _, t := range tables {
		if strings.EqualFold(t.Name, root.Name) {
			continue
		}
		c, ok := t.Column(rootKey)
		if !ok {
			continue
		}
		join := []JoinPair{{From: rootKey, To: c.Name}}
		unique := s.IsUnique(t.Name, []string{c.Name})
		shape, reason := ShapeList, fmt.Sprintf("%s has a %s column, so each document can have many %s rows", short(t.Name), c.Name, short(t.Name))
		if unique {
			shape, reason = ShapeFields, fmt.Sprintf("%s.%s is unique, so each document has at most one %s row", short(t.Name), c.Name, short(t.Name))
		}
		add(Suggestion{Table: t.Name, Shape: shape, Join: join, Source: "same column name", Guessed: true, Unique: unique, Reason: reason})
		if _, done := direct[strings.ToLower(t.Name)]; !done {
			direct[strings.ToLower(t.Name)] = join
		}
	}

	// 3. Lookups: a root column that looks like another table's key.
	for _, c := range root.Columns {
		if strings.EqualFold(c.Name, rootKey) {
			continue
		}
		if t, col, ok := lookupTarget(s, root, c.Name); ok {
			add(Suggestion{Table: t.Name, Shape: ShapeFields, Join: []JoinPair{{From: c.Name, To: col}}, Source: "column name", Guessed: true, Unique: true,
				Reason: fmt.Sprintf("%s.%s looks like it points at %s.%s (a lookup)", short(root.Name), c.Name, short(t.Name), col)})
		}
	}

	// 4. Bridges: a table that references the root and also another table.
	for bkey, bjoin := range direct {
		b, ok := s.Table(bkey)
		if !ok {
			continue
		}
		bridgeCol := bjoin[0].To
		for _, fk := range s.FKs {
			if !strings.EqualFold(fk.FromTable, b.Name) || strings.EqualFold(fk.ToTable, root.Name) || strings.EqualFold(fk.ToTable, b.Name) {
				continue
			}
			var join []JoinPair
			for i := range fk.FromCols {
				join = append(join, JoinPair{From: fk.FromCols[i], To: fk.ToCols[i]})
			}
			add(Suggestion{Table: fk.ToTable, Shape: ShapeList, Join: join, Via: &Via{Table: b.Name, Join: bjoin}, Source: "foreign keys through " + short(b.Name),
				Reason: fmt.Sprintf("%s links %s to %s (many-to-many)", short(b.Name), short(root.Name), short(fk.ToTable))})
		}
		for _, c := range b.Columns {
			if strings.EqualFold(c.Name, bridgeCol) {
				continue
			}
			if t, col, ok := lookupTarget(s, b, c.Name); ok && !strings.EqualFold(t.Name, root.Name) {
				add(Suggestion{Table: t.Name, Shape: ShapeList, Join: []JoinPair{{From: c.Name, To: col}}, Via: &Via{Table: b.Name, Join: bjoin},
					Source: "column names through " + short(b.Name), Guessed: true,
					Reason: fmt.Sprintf("%s links %s to %s (many-to-many)", short(b.Name), short(root.Name), short(t.Name))})
			}
		}
	}

	used := map[string]bool{"root": true} // the root fragment's name
	for i := range out {
		out[i].ID = uniqueID(short(out[i].Table), used)
		if out[i].Shape == ShapeList {
			out[i].Path = plural(out[i].ID)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Guessed != out[j].Guessed {
			return !out[i].Guessed
		}
		return strings.ToLower(out[i].Table) < strings.ToLower(out[j].Table)
	})
	return out, nil
}

// lookupTarget finds a table whose key a column (of table from) seems to hold:
// another table's single-column primary key with the same name, or a table named
// like the column's prefix ("lab_code" -> table lab) with a unique column matching
// the suffix or the full name.
func lookupTarget(s *Snapshot, from *Table, col string) (*Table, string, bool) {
	for _, t := range s.SortedTables() {
		if strings.EqualFold(t.Name, from.Name) {
			continue
		}
		if len(t.PK) == 1 && strings.EqualFold(t.PK[0], col) && !strings.EqualFold(col, "id") {
			return t, t.PK[0], true
		}
	}
	base := strings.ToLower(keySuffix.ReplaceAllString(col, ""))
	suffix := strings.TrimPrefix(strings.ToLower(col[len(base):]), "_")
	if base == "" || base == strings.ToLower(col) {
		return nil, "", false
	}
	for _, t := range s.SortedTables() {
		if strings.EqualFold(t.Name, from.Name) {
			continue
		}
		name := strings.ToLower(short(t.Name))
		if name != base && name != base+"s" && name != base+"es" {
			continue
		}
		for _, cand := range []string{col, suffix, "id"} {
			if c, ok := t.Column(cand); ok && s.IsUnique(t.Name, []string{c.Name}) {
				return t, c.Name, true
			}
		}
	}
	return nil, "", false
}

func short(table string) string {
	if tn, err := config.ParseTableName(table); err == nil {
		return tn.Name
	}
	return table
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_]+`)

func uniqueID(base string, used map[string]bool) string {
	id := nonIdent.ReplaceAllString(strings.ToLower(base), "_")
	id = strings.Trim(id, "_")
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		id = "t_" + id
	}
	if len(id) > 50 {
		id = id[:50]
	}
	cand := id
	for i := 2; used[cand]; i++ {
		cand = fmt.Sprintf("%s_%d", id, i)
	}
	used[cand] = true
	return cand
}

func plural(s string) string {
	switch {
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"):
		return s + "es"
	case strings.HasSuffix(s, "y") && len(s) > 1 && !strings.ContainsAny(s[len(s)-2:len(s)-1], "aeiou"):
		return s[:len(s)-1] + "ies"
	default:
		return s + "s"
	}
}

// Profile measures a relation's shape on the live data.
type Profile struct {
	RelatedRows int64   `json:"related_rows"`
	MaxPerDoc   int64   `json:"max_per_document"`
	AvgPerDoc   float64 `json:"avg_per_document"`
	DocsWithAny int64   `json:"documents_with_any"`
	FanoutMax   int64   `json:"fanout_max"` // documents one related row affects
	FanoutAvg   float64 `json:"fanout_avg"`
	TookMillis  int64   `json:"took_ms"`
	Explanation string  `json:"explanation"`
}

// ProfileRelation runs counting queries for one relation (bounded by ctx).
func ProfileRelation(ctx context.Context, db *sql.DB, s *Snapshot, rootName string, r Relation) (*Profile, error) {
	start := time.Now()
	root, ok := s.Table(rootName)
	if !ok {
		return nil, fmt.Errorf("table %s does not exist", rootName)
	}
	rt, ok := s.Table(r.Table)
	if !ok {
		return nil, fmt.Errorf("table %s does not exist", r.Table)
	}
	p := &Profile{RelatedRows: rt.Rows}
	rootQ := config.MustParseTableName(root.Name).Quoted()
	relQ := config.MustParseTableName(rt.Name).Quoted()
	var perDoc, fanout string
	if r.Via != nil {
		bt, ok := s.Table(r.Via.Table)
		if !ok {
			return nil, fmt.Errorf("table %s does not exist", r.Via.Table)
		}
		bQ := config.MustParseTableName(bt.Name).Quoted()
		on := joinOn("x", "b", r.Join)
		perDoc = fmt.Sprintf("SELECT %s, COUNT(*) AS c FROM %s b JOIN %s x ON %s GROUP BY %s", cols("b", toCols(r.Via.Join)), bQ, relQ, on, cols("b", toCols(r.Via.Join)))
		fanout = fmt.Sprintf("SELECT %s, COUNT(DISTINCT %s) AS c FROM %s b JOIN %s x ON %s GROUP BY %s", cols("x", toCols(r.Join)), cols("b", toCols(r.Via.Join)), bQ, relQ, on, cols("x", toCols(r.Join)))
	} else {
		perDoc = fmt.Sprintf("SELECT %s, COUNT(*) AS c FROM %s x GROUP BY %s", cols("x", toCols(r.Join)), relQ, cols("x", toCols(r.Join)))
		fanout = fmt.Sprintf("SELECT %s, COUNT(*) AS c FROM %s t GROUP BY %s", cols("t", fromCols(r.Join)), rootQ, cols("t", fromCols(r.Join)))
	}
	stat := func(inner string) (n, max int64, avg float64, err error) {
		err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT_BIG(*), ISNULL(MAX(c), 0), ISNULL(AVG(CAST(c AS float)), 0) FROM (%s) g", inner)).Scan(&n, &max, &avg)
		return
	}
	var err error
	if p.DocsWithAny, p.MaxPerDoc, p.AvgPerDoc, err = stat(perDoc); err != nil {
		return nil, err
	}
	var keys int64
	if keys, p.FanoutMax, p.FanoutAvg, err = stat(fanout); err != nil {
		return nil, err
	}
	_ = keys
	p.TookMillis = time.Since(start).Milliseconds()
	switch {
	case r.Shape == ShapeList:
		p.Explanation = fmt.Sprintf("%s %s rows: %.1f on average, %d at most.", count(p.DocsWithAny, "document has", "documents have"), short(rt.Name), p.AvgPerDoc, p.MaxPerDoc)
	default:
		p.Explanation = fmt.Sprintf("One %s row is shared by up to %s (%.1f on average); changing it rebuilds them all.", short(rt.Name), count(p.FanoutMax, "document", "documents"), p.FanoutAvg)
		if p.MaxPerDoc > 1 && r.Via == nil && !s.IsUnique(rt.Name, toCols(r.Join)) {
			p.Explanation += fmt.Sprintf(" Warning: up to %d %s rows match one document; show it as a list instead.", p.MaxPerDoc, short(rt.Name))
		}
	}
	return p, nil
}

// count formats n with the singular or plural phrase.
func count(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func cols(alias string, names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = alias + "." + q(n)
	}
	return strings.Join(parts, ", ")
}

func toCols(j []JoinPair) []string {
	out := make([]string, len(j))
	for i, p := range j {
		out[i] = p.To
	}
	return out
}

func joinOn(childAlias, parentAlias string, j []JoinPair) string {
	parts := make([]string, len(j))
	for i, p := range j {
		parts[i] = fmt.Sprintf("%s.%s = %s.%s", childAlias, q(p.To), parentAlias, q(p.From))
	}
	return strings.Join(parts, " AND ")
}
