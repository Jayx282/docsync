// Package studio implements docsync Studio: a visual designer that browses a SQL
// Server schema, builds document designs, previews and validates them, compiles them
// to docsync configs and deploys them.
package studio

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/config"
)

// Column is a table column as shown in the designer.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // e.g. "varchar(10)", "decimal(10,3)"
	Nullable bool   `json:"nullable"`
	Identity bool   `json:"identity,omitempty"`
	Computed bool   `json:"computed,omitempty"`
}

// Index is a table index.
type Index struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique"`
	Primary bool     `json:"primary"`
}

// ForeignKey is a declared foreign key: FromTable.FromCols references ToTable.ToCols.
type ForeignKey struct {
	Name      string   `json:"name"`
	FromTable string   `json:"from_table"`
	FromCols  []string `json:"from_columns"`
	ToTable   string   `json:"to_table"`
	ToCols    []string `json:"to_columns"`
}

// Table describes one user table.
type Table struct {
	Name      string   `json:"name"` // schema.table
	Rows      int64    `json:"rows"`
	CTEnabled bool     `json:"change_tracking"`
	Columns   []Column `json:"columns"`
	PK        []string `json:"primary_key"`
	Indexes   []Index  `json:"indexes"`
}

// Column returns a column by name, case-insensitively.
func (t *Table) Column(name string) (Column, bool) {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return Column{}, false
}

// Snapshot is the schema of one database, loaded in a few queries.
type Snapshot struct {
	Server   string            `json:"server"`
	Database string            `json:"database"`
	Edition  string            `json:"edition"`
	Version  string            `json:"version"`
	Snapshot bool              `json:"snapshot_isolation"`
	CT       bool              `json:"change_tracking"`
	LoadedAt time.Time         `json:"loaded_at"`
	Tables   map[string]*Table `json:"-"`
	FKs      []ForeignKey      `json:"-"`
	byLower  map[string]*Table
}

// NewSnapshot builds a snapshot from tables and foreign keys (used by tests and tools).
func NewSnapshot(tables []*Table, fks []ForeignKey) *Snapshot {
	s := &Snapshot{Tables: map[string]*Table{}, byLower: map[string]*Table{}, FKs: fks, LoadedAt: time.Now().UTC(), Snapshot: true, CT: true}
	for _, t := range tables {
		s.Tables[t.Name] = t
		s.byLower[strings.ToLower(t.Name)] = t
	}
	return s
}

// Table looks a table up by name ("t", "schema.t" or "[schema].[t]").
func (s *Snapshot) Table(name string) (*Table, bool) {
	tn, err := config.ParseTableName(name)
	if err != nil {
		return nil, false
	}
	t, ok := s.byLower[tn.Key()]
	return t, ok
}

// SortedTables returns every table ordered by name.
func (s *Snapshot) SortedTables() []*Table {
	out := make([]*Table, 0, len(s.Tables))
	for _, t := range s.Tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

// IsUnique reports whether cols identify at most one row of the table: they cover the
// primary key or exactly match a unique index.
func (s *Snapshot) IsUnique(table string, cols []string) bool {
	t, ok := s.Table(table)
	if !ok || len(cols) == 0 {
		return false
	}
	if len(t.PK) > 0 && containsAll(cols, t.PK) {
		return true
	}
	for _, ix := range t.Indexes {
		if ix.Unique && sameSet(ix.Columns, cols) {
			return true
		}
	}
	return false
}

// HasLeadingIndex reports whether some index starts with cols (in any order).
func (s *Snapshot) HasLeadingIndex(table string, cols []string) bool {
	t, ok := s.Table(table)
	if !ok {
		return false
	}
	for _, ix := range t.Indexes {
		if len(ix.Columns) >= len(cols) && sameSet(ix.Columns[:len(cols)], cols) {
			return true
		}
	}
	return false
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if strings.EqualFold(h, w) {
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

func sameSet(a, b []string) bool { return len(a) == len(b) && containsAll(a, b) && containsAll(b, a) }

// LoadSnapshot reads tables, columns, keys, indexes and foreign keys.
func LoadSnapshot(ctx context.Context, db *sql.DB) (*Snapshot, error) {
	s := &Snapshot{Tables: map[string]*Table{}, byLower: map[string]*Table{}, LoadedAt: time.Now().UTC()}
	var snapState int
	var ctOn sql.NullInt64
	err := db.QueryRowContext(ctx, `
SELECT CAST(SERVERPROPERTY('ServerName') AS nvarchar(256)), DB_NAME(),
       CAST(SERVERPROPERTY('Edition') AS nvarchar(256)), CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(64)),
       (SELECT snapshot_isolation_state FROM sys.databases WHERE database_id = DB_ID()),
       (SELECT COUNT(*) FROM sys.change_tracking_databases WHERE database_id = DB_ID())`).
		Scan(&s.Server, &s.Database, &s.Edition, &s.Version, &snapState, &ctOn)
	if err != nil {
		return nil, fmt.Errorf("read server info: %w", err)
	}
	s.Snapshot, s.CT = snapState == 1, ctOn.Int64 > 0

	byID := map[int64]*Table{}
	rows, err := db.QueryContext(ctx, `
SELECT t.object_id, SCHEMA_NAME(t.schema_id), t.name,
       ISNULL((SELECT SUM(p.rows) FROM sys.partitions p WHERE p.object_id = t.object_id AND p.index_id IN (0, 1)), 0),
       CASE WHEN ct.object_id IS NULL THEN 0 ELSE 1 END
FROM sys.tables t
LEFT JOIN sys.change_tracking_tables ct ON ct.object_id = t.object_id
WHERE t.is_ms_shipped = 0`)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	for rows.Next() {
		var id, n int64
		var schemaName, name string
		var ct int
		if err := rows.Scan(&id, &schemaName, &name, &n, &ct); err != nil {
			rows.Close()
			return nil, err
		}
		t := &Table{Name: schemaName + "." + name, Rows: n, CTEnabled: ct == 1, Columns: []Column{}, PK: []string{}, Indexes: []Index{}}
		byID[id] = t
		s.Tables[t.Name] = t
		s.byLower[strings.ToLower(t.Name)] = t
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `
SELECT c.object_id, c.name, TYPE_NAME(c.system_type_id), c.max_length, c.precision, c.scale,
       c.is_nullable, c.is_identity, c.is_computed
FROM sys.columns c JOIN sys.tables t ON t.object_id = c.object_id
WHERE t.is_ms_shipped = 0
ORDER BY c.object_id, c.column_id`)
	if err != nil {
		return nil, fmt.Errorf("list columns: %w", err)
	}
	for rows.Next() {
		var id, maxLen, prec, scale int64
		var c Column
		var typ string
		if err := rows.Scan(&id, &c.Name, &typ, &maxLen, &prec, &scale, &c.Nullable, &c.Identity, &c.Computed); err != nil {
			rows.Close()
			return nil, err
		}
		c.Type = displayType(strings.ToLower(typ), maxLen, prec, scale)
		if t := byID[id]; t != nil {
			t.Columns = append(t.Columns, c)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	rows, err = db.QueryContext(ctx, `
SELECT i.object_id, i.name, i.is_unique, i.is_primary_key, c.name
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.key_ordinal > 0
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
JOIN sys.tables t ON t.object_id = i.object_id
WHERE t.is_ms_shipped = 0 AND i.name IS NOT NULL
ORDER BY i.object_id, i.index_id, ic.key_ordinal`)
	if err != nil {
		return nil, fmt.Errorf("list indexes: %w", err)
	}
	type ixKey struct {
		id   int64
		name string
	}
	order := []ixKey{}
	idx := map[ixKey]*Index{}
	for rows.Next() {
		var id int64
		var name, col string
		var unique, primary bool
		if err := rows.Scan(&id, &name, &unique, &primary, &col); err != nil {
			rows.Close()
			return nil, err
		}
		k := ixKey{id, name}
		ix := idx[k]
		if ix == nil {
			ix = &Index{Name: name, Unique: unique, Primary: primary}
			idx[k] = ix
			order = append(order, k)
		}
		ix.Columns = append(ix.Columns, col)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, k := range order {
		t := byID[k.id]
		if t == nil {
			continue
		}
		ix := idx[k]
		t.Indexes = append(t.Indexes, *ix)
		if ix.Primary {
			t.PK = append([]string(nil), ix.Columns...)
		}
	}

	rows, err = db.QueryContext(ctx, `
SELECT fk.object_id, fk.name, fk.parent_object_id, pc.name, fk.referenced_object_id, rc.name
FROM sys.foreign_keys fk
JOIN sys.foreign_key_columns fkc ON fkc.constraint_object_id = fk.object_id
JOIN sys.columns pc ON pc.object_id = fkc.parent_object_id AND pc.column_id = fkc.parent_column_id
JOIN sys.columns rc ON rc.object_id = fkc.referenced_object_id AND rc.column_id = fkc.referenced_column_id
ORDER BY fk.object_id, fkc.constraint_column_id`)
	if err != nil {
		return nil, fmt.Errorf("list foreign keys: %w", err)
	}
	fkByID := map[int64]*ForeignKey{}
	var fkOrder []int64
	for rows.Next() {
		var id, from, to int64
		var name, fc, tc string
		if err := rows.Scan(&id, &name, &from, &fc, &to, &tc); err != nil {
			rows.Close()
			return nil, err
		}
		ft, tt := byID[from], byID[to]
		if ft == nil || tt == nil {
			continue
		}
		fk := fkByID[id]
		if fk == nil {
			fk = &ForeignKey{Name: name, FromTable: ft.Name, ToTable: tt.Name}
			fkByID[id] = fk
			fkOrder = append(fkOrder, id)
		}
		fk.FromCols = append(fk.FromCols, fc)
		fk.ToCols = append(fk.ToCols, tc)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range fkOrder {
		s.FKs = append(s.FKs, *fkByID[id])
	}
	return s, nil
}

func displayType(t string, maxLen, prec, scale int64) string {
	switch t {
	case "char", "varchar", "binary", "varbinary":
		if maxLen == -1 {
			return t + "(max)"
		}
		return fmt.Sprintf("%s(%d)", t, maxLen)
	case "nchar", "nvarchar":
		if maxLen == -1 {
			return t + "(max)"
		}
		return fmt.Sprintf("%s(%d)", t, maxLen/2)
	case "decimal", "numeric":
		return fmt.Sprintf("%s(%d,%d)", t, prec, scale)
	case "datetime2", "datetimeoffset", "time":
		return fmt.Sprintf("%s(%d)", t, scale)
	}
	return t
}
