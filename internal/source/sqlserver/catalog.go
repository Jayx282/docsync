// Package sqlserver reads changes and document fragments from SQL Server.
package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/microsoft/go-mssqldb" // registers the "sqlserver" driver

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/schema"
)

// Open connects to SQL Server and checks the connection.
func Open(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlserver", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to SQL Server: %w", err)
	}
	return db, nil
}

// ColumnInfo describes a table column.
type ColumnInfo struct {
	Name      string
	TypeName  string // lower-case system type name
	MaxLength int
	Precision int
	Scale     int
	Collation string
	Nullable  bool
	TypeDef   string // usable in CREATE TABLE and CAST
	Logical   schema.Type
}

// TableInfo describes a table.
type TableInfo struct {
	Name      config.TableName
	ObjectID  int64
	Columns   map[string]*ColumnInfo
	PK        []string
	CTEnabled bool
}

// Column returns a column by name, case-insensitively.
func (t *TableInfo) Column(name string) (*ColumnInfo, error) {
	c, ok := t.Columns[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("column %s not found in %s", name, t.Name)
	}
	return c, nil
}

// Catalog holds metadata for every table docsync touches.
type Catalog struct {
	tables map[string]*TableInfo
}

// Table returns metadata for a table.
func (c *Catalog) Table(t config.TableName) (*TableInfo, bool) {
	ti, ok := c.tables[t.Key()]
	return ti, ok
}

// MustTable returns metadata for a table loaded into the catalog.
func (c *Catalog) MustTable(t config.TableName) *TableInfo {
	ti, ok := c.tables[t.Key()]
	if !ok {
		panic("catalog: table not loaded: " + t.String())
	}
	return ti
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LoadCatalog reads column, primary-key and change-tracking metadata.
func LoadCatalog(ctx context.Context, q queryer, tables []config.TableName) (*Catalog, error) {
	cat := &Catalog{tables: map[string]*TableInfo{}}
	for _, t := range tables {
		if _, done := cat.tables[t.Key()]; done {
			continue
		}
		ti, err := loadTable(ctx, q, t)
		if err != nil {
			return nil, err
		}
		cat.tables[t.Key()] = ti
	}
	return cat, nil
}

func loadTable(ctx context.Context, q queryer, t config.TableName) (*TableInfo, error) {
	var oid sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT OBJECT_ID(@p1, 'U')`, t.Quoted()).Scan(&oid); err != nil {
		return nil, fmt.Errorf("look up %s: %w", t, err)
	}
	if !oid.Valid {
		return nil, fmt.Errorf("table %s does not exist (or the login cannot see it)", t)
	}
	ti := &TableInfo{Name: t, ObjectID: oid.Int64, Columns: map[string]*ColumnInfo{}}
	rows, err := q.QueryContext(ctx, `
SELECT c.name, TYPE_NAME(c.system_type_id), c.max_length, c.precision, c.scale,
       ISNULL(c.collation_name, ''), c.is_nullable
FROM sys.columns c
WHERE c.object_id = @p1
ORDER BY c.column_id`, oid.Int64)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", t, err)
	}
	for rows.Next() {
		c := &ColumnInfo{}
		var maxLen, prec, scale int64
		if err := rows.Scan(&c.Name, &c.TypeName, &maxLen, &prec, &scale, &c.Collation, &c.Nullable); err != nil {
			rows.Close()
			return nil, err
		}
		c.TypeName = strings.ToLower(c.TypeName)
		c.MaxLength, c.Precision, c.Scale = int(maxLen), int(prec), int(scale)
		c.TypeDef = typeDef(c)
		if lt, err := schema.TypeForSQL(schema.Column{Name: c.Name, DBType: c.TypeName, Precision: prec, Scale: scale}); err == nil {
			c.Logical = lt
		}
		ti.Columns[strings.ToLower(c.Name)] = c
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	pkRows, err := q.QueryContext(ctx, `
SELECT c.name
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = @p1 AND i.is_primary_key = 1
ORDER BY ic.key_ordinal`, oid.Int64)
	if err != nil {
		return nil, fmt.Errorf("read primary key of %s: %w", t, err)
	}
	for pkRows.Next() {
		var name string
		if err := pkRows.Scan(&name); err != nil {
			pkRows.Close()
			return nil, err
		}
		ti.PK = append(ti.PK, name)
	}
	if err := pkRows.Close(); err != nil {
		return nil, err
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.change_tracking_tables WHERE object_id = @p1`, oid.Int64).Scan(&n); err != nil {
		return nil, fmt.Errorf("read change tracking state of %s: %w", t, err)
	}
	ti.CTEnabled = n > 0
	return ti, nil
}

func isStringType(name string) bool {
	switch name {
	case "char", "varchar", "nchar", "nvarchar", "text", "ntext", "sysname":
		return true
	}
	return false
}

func typeDef(c *ColumnInfo) string {
	var d string
	switch c.TypeName {
	case "char", "varchar", "binary", "varbinary":
		if c.MaxLength == -1 {
			d = c.TypeName + "(max)"
		} else {
			d = fmt.Sprintf("%s(%d)", c.TypeName, c.MaxLength)
		}
	case "nchar", "nvarchar":
		if c.MaxLength == -1 {
			d = c.TypeName + "(max)"
		} else {
			d = fmt.Sprintf("%s(%d)", c.TypeName, c.MaxLength/2)
		}
	case "decimal", "numeric":
		d = fmt.Sprintf("%s(%d,%d)", c.TypeName, c.Precision, c.Scale)
	case "datetime2", "datetimeoffset", "time":
		d = fmt.Sprintf("%s(%d)", c.TypeName, c.Scale)
	case "sysname":
		d = "nvarchar(128)"
	default:
		d = c.TypeName
	}
	if isStringType(c.TypeName) && c.Collation != "" {
		d += " COLLATE " + c.Collation
	}
	return d
}

// ValidatePlanTables checks that configured keys match each table's primary key
// and that change tracking is enabled where needed.
func (c *Catalog) ValidateKeys(t config.TableName, key []string, needCT bool) error {
	ti, ok := c.Table(t)
	if !ok {
		return fmt.Errorf("table %s not loaded", t)
	}
	if len(ti.PK) == 0 {
		return fmt.Errorf("%s has no primary key; change tracking requires one", t)
	}
	if len(ti.PK) != len(key) {
		return fmt.Errorf("%s: configured key %v does not match the primary key %v", t, key, ti.PK)
	}
	for i := range key {
		if !strings.EqualFold(ti.PK[i], key[i]) {
			return fmt.Errorf("%s: configured key %v does not match the primary key %v (same columns, same order)", t, key, ti.PK)
		}
	}
	if needCT && !ti.CTEnabled {
		return fmt.Errorf("change tracking is not enabled on %s (ALTER TABLE %s ENABLE CHANGE_TRACKING)", t, t.Quoted())
	}
	return nil
}
