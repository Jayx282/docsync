package studio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v2"

	"github.com/Jayx282/docsync/internal/config"
)

// DefaultHotFanoutLimit caps how many documents one cycle rebuilds for changes to a
// shared row (a lookup, or a row reached through a bridge); the rest are rebuilt in
// the background so a popular lookup edit does not delay other changes.
const DefaultHotFanoutLimit = 2000

// CompileOptions sets runtime parameters of compiled configs.
type CompileOptions struct {
	DSNEnv  string // environment variable holding the connection string
	BaseDir string // the pipeline's folder; state/ and out/ in the config are relative to it
}

// Compiled is a design turned into a docsync config.
type Compiled struct {
	Config *config.Config     `json:"-"`
	YAML   string             `json:"yaml"`
	SQL    []CompiledFragment `json:"sql"`
}

// CompiledFragment is one generated fragment query.
type CompiledFragment struct {
	Name        string `json:"name"`
	Cardinality string `json:"cardinality"`
	Path        string `json:"path,omitempty"`
	SQL         string `json:"sql"`
}

// Compile validates a design and generates the docsync config for it. The YAML is
// parsed back through the normal config loader, so a compiled design obeys exactly
// the same rules as a hand-written config.
func Compile(d *Design, s *Snapshot, opt CompileOptions) (*Compiled, error) {
	if err := d.Validate(s); err != nil {
		return nil, err
	}
	if opt.DSNEnv == "" {
		opt.DSNEnv = "DOCSYNC_DSN"
	}
	rootT, _ := s.Table(d.Root.Table)
	root := config.MustParseTableName(rootT.Name)
	rootKey := rootT.PK[0]

	cfg := &config.Config{
		Source:   config.Source{Type: "sqlserver", DSNEnv: opt.DSNEnv},
		Document: config.Document{Name: d.Name, Root: config.Root{Table: root.String(), Key: []string{rootKey}}},
	}

	rootJoin := fmt.Sprintf("JOIN %s t ON t.%s = r.id", root.Quoted(), q(rootKey))
	if len(d.Root.Fields) > 0 {
		cfg.Document.Fragments = append(cfg.Document.Fragments, config.Fragment{
			Name: "root",
			SQL:  fmt.Sprintf("SELECT r.id AS _root, %s\nFROM {{roots}} r\n%s", selectList("t", d.Root.Fields), rootJoin),
		})
	}

	for _, r := range d.Related {
		rt, _ := s.Table(r.Table)
		rel := config.MustParseTableName(rt.Name)
		fields := append([]Field(nil), r.Fields...)
		var from []string
		parentAlias := func(cols []string) (string, bool) {
			// Joining on the root key needs no join to the root table: #roots holds it.
			if len(cols) == 1 && strings.EqualFold(cols[0], rootKey) {
				return "r.id", true
			}
			return "", false
		}
		var joins []string
		needRoot := false
		if r.Via != nil {
			bt, _ := s.Table(r.Via.Table)
			bridge := config.MustParseTableName(bt.Name)
			var on []string
			if ref, ok := parentAlias(fromCols(r.Via.Join)); ok {
				on = append(on, fmt.Sprintf("b.%s = %s", q(r.Via.Join[0].To), ref))
			} else {
				needRoot = true
				for _, j := range r.Via.Join {
					on = append(on, fmt.Sprintf("b.%s = t.%s", q(j.To), q(j.From)))
				}
			}
			joins = append(joins, fmt.Sprintf("JOIN %s b ON %s", bridge.Quoted(), strings.Join(on, " AND ")))
			var on2 []string
			for _, j := range r.Join {
				on2 = append(on2, fmt.Sprintf("x.%s = b.%s", q(j.To), q(j.From)))
			}
			joins = append(joins, fmt.Sprintf("JOIN %s x ON %s", rel.Quoted(), strings.Join(on2, " AND ")))
		} else {
			var on []string
			if ref, ok := parentAlias(fromCols(r.Join)); ok {
				on = append(on, fmt.Sprintf("x.%s = %s", q(r.Join[0].To), ref))
			} else {
				needRoot = true
				for _, j := range r.Join {
					on = append(on, fmt.Sprintf("x.%s = t.%s", q(j.To), q(j.From)))
				}
			}
			joins = append(joins, fmt.Sprintf("JOIN %s x ON %s", rel.Quoted(), strings.Join(on, " AND ")))
		}
		if needRoot {
			joins = append([]string{rootJoin}, joins...)
		}

		frag := config.Fragment{Name: r.ID}
		if r.Hidden {
			frag.Name = "" // not in documents; only its routing is needed
		} else if r.Shape == ShapeList {
			sortBy := r.SortBy
			if len(sortBy) == 0 {
				sortBy = rt.PK
			}
			for _, sc := range sortBy {
				name := ""
				for _, f := range fields {
					if strings.EqualFold(f.Column, sc) {
						name = f.Output()
					}
				}
				if name == "" { // sort by a column that is not shown: include it
					fields = append(fields, Field{Column: sc})
					name = sc
				}
				frag.SortKey = append(frag.SortKey, name)
			}
			frag.Cardinality = config.CardinalityMany
			frag.Path = r.Path
		} else {
			frag.Cardinality = config.CardinalityOne
			frag.Path = r.Path
		}
		frag.SQL = fmt.Sprintf("SELECT r.id AS _root, %s\nFROM {{roots}} r\n%s", selectList("x", fields), strings.Join(joins, "\n"))
		if frag.Name != "" {
			cfg.Document.Fragments = append(cfg.Document.Fragments, frag)
		}

		// Routing: how changes in these tables reach the root.
		if r.Via != nil {
			bt, _ := s.Table(r.Via.Table)
			from = fromCols(r.Via.Join)
			cfg.Routing = append(cfg.Routing, rule(bt.Name, bt.PK, root.String(), joinMap(r.Via.Join), kindFor(from, rootKey)))
			cfg.Routing = append(cfg.Routing, rule(rt.Name, rt.PK, bt.Name, joinMap(r.Join), config.KindReferencedBy))
		} else {
			from = fromCols(r.Join)
			cfg.Routing = append(cfg.Routing, rule(rt.Name, rt.PK, root.String(), joinMap(r.Join), kindFor(from, rootKey)))
		}
	}

	if len(d.Calculated) > 0 || strings.TrimSpace(d.Root.Filter) != "" {
		sc, err := newFormulaScope(d, s)
		if err != nil {
			return nil, err
		}
		if len(d.Calculated) > 0 {
			cols := make([]string, 0, len(d.Calculated))
			for _, c := range d.Calculated {
				ce, err := sc.compileCalc(c)
				if err != nil {
					return nil, fmt.Errorf("calculated field %q: %w", c.Name, err)
				}
				cols = append(cols, ce.sql+" AS "+q(c.Name))
			}
			cfg.Document.Fragments = append(cfg.Document.Fragments, config.Fragment{Name: calculatedFragment, SQL: sc.fragmentSQL(cols)})
		}
		if f := strings.TrimSpace(d.Root.Filter); f != "" {
			sql, err := sc.compileFilter(f)
			if err != nil {
				return nil, fmt.Errorf("filter: %w", err)
			}
			cfg.Document.Root.Filter = sql
		}
	}

	format := d.Output.Format
	if format == "" {
		format = config.SinkParquetLog
	}
	// Paths are relative to the config file, so a pipeline folder (docsync.yaml with
	// its state/ and out/) runs unchanged elsewhere with `docsync run`.
	cfg.Sinks = []config.Sink{{Name: "output", Type: format, Dir: "out", IncludeJSON: d.Output.IncludeJSON && format == config.SinkParquetLog}}
	cfg.State.Dir = "state"

	// Parse once to validate and fill in defaults, then write the effective settings,
	// so the file shows every value the pipeline will use.
	draft, err := orderedYAML(cfg)
	if err != nil {
		return nil, err
	}
	effective, err := config.Parse(draft)
	if err != nil {
		return nil, fmt.Errorf("compiled config is invalid (please report this): %w", err)
	}
	body, err := orderedYAML(effective)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Generated by docsync studio from design %q on %s.\n# Edit the design in Studio, or copy this file and maintain it by hand.\n",
		d.Name, time.Now().UTC().Format(time.RFC3339))
	parsed, err := config.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("compiled config is invalid (please report this): %w", err)
	}
	out := &Compiled{Config: parsed, YAML: header + string(body)}
	for _, f := range parsed.Document.Fragments {
		out.SQL = append(out.SQL, CompiledFragment{Name: f.Name, Cardinality: f.Cardinality, Path: f.Path, SQL: f.SQL})
	}
	return out, nil
}

func q(ident string) string { return config.QuoteIdent(ident) }

// rule builds a routing rule. Rows that other rows point at (referenced_by) can be
// shared by many documents, so they get a hot fan-out limit.
func rule(table string, key []string, next string, join map[string]string, kind string) config.RoutingRule {
	r := config.RoutingRule{Table: table, Key: key, Next: &config.Link{Table: next, Join: join, Kind: kind}}
	if kind == config.KindReferencedBy {
		r.HotFanoutLimit = DefaultHotFanoutLimit
	}
	return r
}

func selectList(alias string, fields []Field) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, fmt.Sprintf("%s.%s AS %s", alias, q(f.Column), q(f.Output())))
	}
	return strings.Join(parts, ", ")
}

func fromCols(j []JoinPair) []string {
	out := make([]string, len(j))
	for i, p := range j {
		out[i] = p.From
	}
	return out
}

// joinMap turns pairs (parent column From, routed table column To) into a routing
// join: routed table column -> next table column.
func joinMap(j []JoinPair) map[string]string {
	m := make(map[string]string, len(j))
	for _, p := range j {
		m[p.To] = p.From
	}
	return m
}

// kindFor: rows that hold the root key reference the root; otherwise the root (or
// bridge) holds this table's columns.
func kindFor(parentCols []string, rootKey string) string {
	if len(parentCols) == 1 && strings.EqualFold(parentCols[0], rootKey) {
		return config.KindReferences
	}
	return config.KindReferencedBy
}

// orderedYAML renders v as YAML through its JSON form (so json tags and omitempty
// apply), keeping keys in struct order rather than sorting them: generated configs
// then read top-down like hand-written ones (source, document, routing, sinks, ...).
func orderedYAML(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	val, err := readOrdered(dec)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(val)
}

func readOrdered(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			m := yaml.MapSlice{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := readOrdered(dec)
				if err != nil {
					return nil, err
				}
				m = append(m, yaml.MapItem{Key: k, Value: val})
			}
			_, err := dec.Token() // }
			return m, err
		}
		list := []any{}
		for dec.More() {
			val, err := readOrdered(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, val)
		}
		_, err := dec.Token() // ]
		return list, err
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i, nil
		}
		return t.Float64()
	}
	return tok, nil // string, bool or nil
}
