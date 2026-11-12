package studio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
)

// Design is what the designer edits: one document per root row, plus related data
// pulled in as fields (lookups) or nested lists. It compiles to a docsync config.
type Design struct {
	Version int        `json:"version"`
	Name    string     `json:"name"`
	Root    Root       `json:"root"`
	Related []Relation `json:"related"`
	// Calculated fields are computed with formulas from the root, its lookups and
	// summaries of its lists (see formula.go and docs/formulas.md).
	Calculated []Calc    `json:"calculated,omitempty"`
	Output     Output    `json:"output"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// Calc is a calculated field: a formula and the field name it is stored under.
type Calc struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

// Root is the table whose rows become documents.
type Root struct {
	Table  string  `json:"table"`
	Key    string  `json:"key"`
	Fields []Field `json:"fields"`
	// Filter is a formula condition on the root table's columns: only matching rows
	// become documents (for example: status <> "sold").
	Filter string `json:"filter,omitempty"`
}

// Field is a selected column and its output name.
type Field struct {
	Column string `json:"column"`
	As     string `json:"as,omitempty"`
}

// Output returns the field's name in documents.
func (f Field) Output() string {
	if f.As != "" {
		return f.As
	}
	return f.Column
}

// Shape says how related rows appear in a document.
type Shape string

// Shapes.
const (
	// ShapeFields: at most one related row per document; its columns become fields
	// (at the top level, or inside an object named by Path).
	ShapeFields Shape = "fields"
	// ShapeList: any number of related rows; they become a list named by Path.
	ShapeList Shape = "list"
)

// Relation pulls one related table into the document.
type Relation struct {
	ID    string     `json:"id"`
	Table string     `json:"table"`
	Shape Shape      `json:"shape"`
	Join  []JoinPair `json:"join"`
	// Via, when set, reaches Table through a bridge table (many-to-many). Join then
	// pairs bridge columns (From) with Table's columns (To).
	Via    *Via     `json:"via,omitempty"`
	Fields []Field  `json:"fields"`
	Path   string   `json:"path,omitempty"`
	SortBy []string `json:"sort_by,omitempty"`
	// Hidden relations are left out of documents but can be used by calculated fields
	// (for example to count rates without listing them). Their changes still rebuild
	// the documents that use them.
	Hidden bool `json:"hidden,omitempty"`
}

// JoinPair equates a column of the parent side (the root, or the bridge) with a column
// of the related table.
type JoinPair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Via is a bridge table between the root and a related table.
type Via struct {
	Table string     `json:"table"`
	Join  []JoinPair `json:"join"` // root column (From) = bridge column (To)
}

// Output configures where the compiled pipeline writes.
type Output struct {
	Format      string `json:"format,omitempty"` // parquet_log (default) or ndjson
	IncludeJSON bool   `json:"include_json"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Validate checks the design against the schema snapshot (when given).
func (d *Design) Validate(s *Snapshot) error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if d.Version != 1 {
		add("version must be 1")
	}
	if !nameRe.MatchString(d.Name) {
		add("name %q must start with a lowercase letter and use only a-z, 0-9 and _", d.Name)
	}
	if d.Root.Table == "" || d.Root.Key == "" {
		add("choose a root table and its key")
		return errors.Join(errs...)
	}
	used := map[string]string{} // table key -> where
	use := func(table, where string) {
		tn, err := config.ParseTableName(table)
		if err != nil {
			add("%s: %v", where, err)
			return
		}
		if prev, ok := used[tn.Key()]; ok {
			add("%s uses %s, which %s already uses; a table can appear only once per design for now", where, table, prev)
			return
		}
		used[tn.Key()] = where
	}
	use(d.Root.Table, "the root")

	var root *Table
	if s != nil {
		var ok bool
		if root, ok = s.Table(d.Root.Table); !ok {
			add("root table %s does not exist", d.Root.Table)
		} else {
			if len(root.PK) != 1 || !strings.EqualFold(root.PK[0], d.Root.Key) {
				add("the root key must be the single-column primary key of %s (it is %v)", d.Root.Table, root.PK)
			}
		}
	}
	names := map[string]string{} // output field name -> owner
	claim := func(name, owner string) {
		if !identRe.MatchString(name) {
			add("%s: field name %q must be a simple identifier", owner, name)
			return
		}
		k := strings.ToLower(name)
		if prev, ok := names[k]; ok {
			add("field %q is produced by both %s and %s; rename one", name, prev, owner)
			return
		}
		names[k] = owner
	}
	checkFields := func(owner, table string, fields []Field, t *Table) map[string]bool {
		local := map[string]bool{}
		for _, f := range fields {
			if t != nil {
				if _, ok := t.Column(f.Column); !ok {
					add("%s: column %s does not exist in %s", owner, f.Column, table)
				}
			}
			if !identRe.MatchString(f.Output()) {
				add("%s: field name %q must be a simple identifier", owner, f.Output())
			}
			if local[strings.ToLower(f.Output())] {
				add("%s: duplicate field %q", owner, f.Output())
			}
			local[strings.ToLower(f.Output())] = true
		}
		return local
	}
	for k := range checkFields("the root", d.Root.Table, d.Root.Fields, root) {
		_ = k
	}
	for _, f := range d.Root.Fields {
		claim(f.Output(), "the root")
	}

	ids := map[string]bool{}
	total := len(d.Root.Fields)
	for i, r := range d.Related {
		owner := fmt.Sprintf("related[%d] (%s)", i, r.Table)
		if !nameRe.MatchString(r.ID) {
			add("%s: id %q must start with a lowercase letter and use only a-z, 0-9 and _", owner, r.ID)
		} else if r.ID == "root" || r.ID == calculatedFragment {
			add("%s: id %q is reserved", owner, r.ID)
		} else if ids[r.ID] {
			add("%s: duplicate id %q", owner, r.ID)
		}
		ids[r.ID] = true
		use(r.Table, owner)
		if r.Via != nil {
			use(r.Via.Table, owner+" (bridge)")
		}
		if r.Shape != ShapeFields && r.Shape != ShapeList {
			add("%s: shape must be %q or %q", owner, ShapeFields, ShapeList)
		}
		if r.Via != nil && r.Shape != ShapeList {
			add("%s: a relation through a bridge table must be a list", owner)
		}
		if len(r.Join) == 0 {
			add("%s: add at least one join column pair", owner)
		}
		if r.Via != nil && len(r.Via.Join) == 0 {
			add("%s: add at least one join column pair for the bridge", owner)
		}
		if len(r.Fields) == 0 && !r.Hidden {
			add("%s: select at least one field", owner)
		}
		total += len(r.Fields)
		var rt *Table
		if s != nil {
			var ok bool
			if rt, ok = s.Table(r.Table); !ok {
				add("%s: table does not exist", owner)
			} else if len(rt.PK) == 0 {
				add("%s: %s has no primary key; change tracking needs one", owner, r.Table)
			}
			parentTable := d.Root.Table
			if r.Via != nil {
				parentTable = r.Via.Table
				if bt, ok := s.Table(r.Via.Table); !ok {
					add("%s: bridge table %s does not exist", owner, r.Via.Table)
				} else {
					if len(bt.PK) == 0 {
						add("%s: bridge table %s has no primary key", owner, r.Via.Table)
					}
					for _, j := range r.Via.Join {
						if root != nil {
							if _, ok := root.Column(j.From); !ok {
								add("%s: root column %s does not exist", owner, j.From)
							}
						}
						if _, ok := bt.Column(j.To); !ok {
							add("%s: bridge column %s does not exist", owner, j.To)
						}
					}
				}
			}
			if pt, ok := s.Table(parentTable); ok {
				for _, j := range r.Join {
					if _, ok := pt.Column(j.From); !ok {
						add("%s: column %s does not exist in %s", owner, j.From, parentTable)
					}
				}
			}
			if rt != nil {
				for _, j := range r.Join {
					if _, ok := rt.Column(j.To); !ok {
						add("%s: column %s does not exist in %s", owner, j.To, r.Table)
					}
				}
			}
		}
		local := checkFields(owner, r.Table, r.Fields, rt)
		switch {
		case r.Hidden:
			// not in documents: no names to claim
		case r.Shape == ShapeList:
			if r.Path == "" {
				add("%s: a list needs a name (path)", owner)
			} else {
				claim(r.Path, owner)
			}
			for _, sb := range r.SortBy {
				found := false
				for _, f := range r.Fields {
					if strings.EqualFold(f.Column, sb) {
						found = true
					}
				}
				if !found && rt != nil && !containsAll(rt.PK, []string{sb}) {
					add("%s: sort column %s must be a selected field", owner, sb)
				}
			}
		case r.Path != "":
			claim(r.Path, owner)
		default:
			for k := range local {
				for _, f := range r.Fields {
					if strings.EqualFold(f.Output(), k) {
						claim(f.Output(), owner)
					}
				}
			}
		}
	}
	calcNames := map[string]bool{}
	for i, c := range d.Calculated {
		owner := fmt.Sprintf("calculated field %q", c.Name)
		if c.Name == "" {
			add("calculated field %d needs a name", i+1)
			continue
		}
		if calcNames[strings.ToLower(c.Name)] {
			add("%s: the name is used twice", owner)
		}
		calcNames[strings.ToLower(c.Name)] = true
		claim(c.Name, owner)
		if strings.TrimSpace(c.Expr) == "" {
			add("%s: write a formula", owner)
		}
	}
	total += len(d.Calculated)
	if total == 0 {
		add("select at least one field")
	}
	if s != nil && root != nil && len(errs) == 0 {
		if sc, err := newFormulaScope(d, s); err == nil {
			for _, c := range d.Calculated {
				if _, err := sc.compileCalc(c); err != nil {
					add("calculated field %q: %v", c.Name, err)
				}
			}
			if strings.TrimSpace(d.Root.Filter) != "" {
				if _, err := sc.compileFilter(d.Root.Filter); err != nil {
					add("filter: %v", err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// ---- persistence ----------------------------------------------------------------

// Store keeps designs as JSON files in a directory.
type Store struct{ dir string }

// NewStore opens (and creates) the designs directory.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("invalid design name %q", name)
	}
	return filepath.Join(s.dir, name+".json"), nil
}

// List returns all designs, newest first.
func (s *Store) List() ([]*Design, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Design
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		d, err := s.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Get loads one design.
func (s *Store) Get(name string) (*Design, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var d Design
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("design %s: %w", name, err)
	}
	return &d, nil
}

// Put saves a design (atomically).
func (s *Store) Put(d *Design) error {
	p, err := s.path(d.Name)
	if err != nil {
		return err
	}
	d.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Delete removes a design.
func (s *Store) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
