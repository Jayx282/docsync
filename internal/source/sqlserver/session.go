package sqlserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
)

const (
	rootsTable    = "#roots"
	maxParams     = 2000 // SQL Server allows 2100 parameters per request
	maxValuesRows = 1000 // and 1000 rows per INSERT ... VALUES
)

// Session pins one connection so temp tables and the snapshot transaction share it.
type Session struct {
	conn    *sql.Conn
	cat     *Catalog
	plan    *routing.Plan
	rootCol *ColumnInfo
	chg     map[string]string // table key -> temp table name
}

// NewSession opens a dedicated connection and creates the cycle's temp tables.
// Temp tables are created here, outside any snapshot transaction, because snapshot
// transactions reject some DDL.
func NewSession(ctx context.Context, db *sql.DB, cat *Catalog, plan *routing.Plan) (*Session, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	s := &Session{conn: conn, cat: cat, plan: plan, chg: map[string]string{}}
	rootInfo := cat.MustTable(plan.Root)
	if s.rootCol, err = rootInfo.Column(plan.RootKey); err != nil {
		conn.Close()
		return nil, err
	}
	stmts := []string{fmt.Sprintf("CREATE TABLE %s (id %s NOT NULL PRIMARY KEY WITH (IGNORE_DUP_KEY = ON))", rootsTable, s.rootCol.TypeDef)}
	for i, tp := range plan.Tables {
		ti := cat.MustTable(tp.Table)
		name := fmt.Sprintf("#chg_%d", i)
		s.chg[tp.Table.Key()] = name
		cols := []string{"op nchar(1) NOT NULL", "ver bigint NOT NULL"}
		var pk []string
		for _, k := range tp.Key {
			ci, err := ti.Column(k)
			if err != nil {
				conn.Close()
				return nil, err
			}
			cols = append(cols, fmt.Sprintf("%s %s NOT NULL", config.QuoteIdent(ci.Name), ci.TypeDef))
			pk = append(pk, config.QuoteIdent(ci.Name))
		}
		cols = append(cols, fmt.Sprintf("PRIMARY KEY (%s)", strings.Join(pk, ", ")))
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s (%s)", name, strings.Join(cols, ", ")))
	}
	for _, st := range stmts {
		if _, err := conn.ExecContext(ctx, st); err != nil {
			conn.Close()
			return nil, fmt.Errorf("create temp table: %w", err)
		}
	}
	return s, nil
}

// Close releases the connection (and its temp tables).
func (s *Session) Close() error { return s.conn.Close() }

// RootColumn returns the root key column metadata.
func (s *Session) RootColumn() *ColumnInfo { return s.rootCol }

// Cycle is one snapshot transaction.
type Cycle struct {
	s  *Session
	tx *sql.Tx
}

// BeginCycle empties the temp tables and starts a SNAPSHOT transaction.
func (s *Session) BeginCycle(ctx context.Context) (*Cycle, error) {
	stmts := []string{"TRUNCATE TABLE " + rootsTable}
	for _, tp := range s.plan.Tables {
		stmts = append(stmts, "TRUNCATE TABLE "+s.chg[tp.Table.Key()])
	}
	if _, err := s.conn.ExecContext(ctx, strings.Join(stmts, "; ")); err != nil {
		return nil, err
	}
	tx, err := s.conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSnapshot, ReadOnly: false})
	if err != nil {
		return nil, err
	}
	return &Cycle{s: s, tx: tx}, nil
}

// Commit ends the transaction.
func (c *Cycle) Commit() error { return c.tx.Commit() }

// Rollback abandons the transaction.
func (c *Cycle) Rollback() { _ = c.tx.Rollback() }

// TableVersion is change-tracking metadata for one table.
type TableVersion struct {
	MinValid     sql.NullInt64
	BeginVersion sql.NullInt64
}

// Versions are the change-tracking positions read at the start of a cycle.
type Versions struct {
	Current  int64
	ForkGUID string
	Tables   map[string]TableVersion // by table key
}

// Versions reads per-table minimum valid versions, then the current version.
// It must be the first statement of the cycle so the snapshot covers both.
func (c *Cycle) Versions(ctx context.Context) (Versions, error) {
	v := Versions{Tables: map[string]TableVersion{}}
	var vals []string
	var args []any
	byOID := map[int64]string{}
	for i, tp := range c.s.plan.Tables {
		ti := c.s.cat.MustTable(tp.Table)
		vals = append(vals, fmt.Sprintf("(CAST(@p%d AS int))", i+1))
		args = append(args, ti.ObjectID)
		byOID[ti.ObjectID] = tp.Table.Key()
	}
	q := fmt.Sprintf(`
SELECT t.oid, CHANGE_TRACKING_MIN_VALID_VERSION(t.oid), ctt.begin_version
FROM (VALUES %s) AS t(oid)
LEFT JOIN sys.change_tracking_tables ctt ON ctt.object_id = t.oid`, strings.Join(vals, ","))
	rows, err := c.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return v, err
	}
	for rows.Next() {
		var oid int64
		var tv TableVersion
		if err := rows.Scan(&oid, &tv.MinValid, &tv.BeginVersion); err != nil {
			rows.Close()
			return v, err
		}
		v.Tables[byOID[oid]] = tv
	}
	if err := rows.Close(); err != nil {
		return v, err
	}
	var cur sql.NullInt64
	if err := c.tx.QueryRowContext(ctx, `SELECT CHANGE_TRACKING_CURRENT_VERSION()`).Scan(&cur); err != nil {
		return v, err
	}
	if !cur.Valid {
		return v, ErrChangeTrackingOff
	}
	v.Current = cur.Int64
	var fork sql.NullString
	err = c.tx.QueryRowContext(ctx, `SELECT CONVERT(nvarchar(36), recovery_fork_guid) FROM sys.database_recovery_status WHERE database_id = DB_ID()`).Scan(&fork)
	if err == nil && fork.Valid {
		v.ForkGUID = fork.String
	}
	// Without permission to read the fork GUID, restores are still caught by CURRENT_VERSION < @from.
	return v, nil
}

// ErrChangeTrackingOff means change tracking is not enabled on the database.
var ErrChangeTrackingOff = errors.New("change tracking is not enabled on the database (ALTER DATABASE ... SET CHANGE_TRACKING = ON)")

// Capture copies a table's changes since from into its temp table and returns
// the number of changes and the highest change version seen.
func (c *Cycle) Capture(ctx context.Context, tp *routing.TablePlan, from int64) (int64, int64, error) {
	tmp := c.s.chg[tp.Table.Key()]
	var cols, sel []string
	for _, k := range tp.Key {
		cols = append(cols, config.QuoteIdent(k))
		sel = append(sel, "ct."+config.QuoteIdent(k))
	}
	q := fmt.Sprintf(`INSERT INTO %s (op, ver, %s)
SELECT ct.SYS_CHANGE_OPERATION, ct.SYS_CHANGE_VERSION, %s
FROM CHANGETABLE(CHANGES %s, @p1) AS ct;
SELECT COUNT_BIG(*), ISNULL(MAX(ver), 0) FROM %s;`,
		tmp, strings.Join(cols, ", "), strings.Join(sel, ", "), tp.Table.Quoted(), tmp)
	var n, maxVer int64
	if err := c.tx.QueryRowContext(ctx, q, from).Scan(&n, &maxVer); err != nil {
		return 0, 0, fmt.Errorf("capture changes of %s: %w", tp.Table, err)
	}
	return n, maxVer, nil
}

// ChangedRow is one captured change with the row's current routing values.
type ChangedRow struct {
	Op     byte
	PK     []any
	Exists bool
	From   []any // current values of the plan's FromCols (nil when the row no longer exists)
}

// ChangedRows returns captured changes joined to the table's current rows.
func (c *Cycle) ChangedRows(ctx context.Context, tp *routing.TablePlan) ([]ChangedRow, error) {
	ti := c.s.cat.MustTable(tp.Table)
	tmp := c.s.chg[tp.Table.Key()]
	var sel, on []string
	var pkTypes, fromTypes []schema.Type
	for _, k := range tp.Key {
		ci, err := ti.Column(k)
		if err != nil {
			return nil, err
		}
		sel = append(sel, "c."+config.QuoteIdent(ci.Name))
		on = append(on, fmt.Sprintf("t.%s = c.%s", config.QuoteIdent(ci.Name), config.QuoteIdent(ci.Name)))
		pkTypes = append(pkTypes, ci.Logical)
	}
	first := config.QuoteIdent(tp.Key[0])
	sel = append(sel, fmt.Sprintf("CASE WHEN t.%s IS NULL THEN 0 ELSE 1 END", first))
	for _, f := range tp.FromCols() {
		ci, err := ti.Column(f)
		if err != nil {
			return nil, err
		}
		sel = append(sel, "t."+config.QuoteIdent(ci.Name))
		fromTypes = append(fromTypes, ci.Logical)
	}
	q := fmt.Sprintf("SELECT c.op, %s FROM %s c LEFT JOIN %s t ON %s",
		strings.Join(sel, ", "), tmp, tp.Table.Quoted(), strings.Join(on, " AND "))
	rows, err := c.tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("read changed rows of %s: %w", tp.Table, err)
	}
	defer rows.Close()
	n := 1 + len(pkTypes) + 1 + len(fromTypes)
	var out []ChangedRow
	for rows.Next() {
		raw := make([]any, n)
		ptrs := make([]any, n)
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := ChangedRow{}
		switch op := raw[0].(type) {
		case string:
			if len(op) > 0 {
				r.Op = op[0]
			}
		case []byte:
			if len(op) > 0 {
				r.Op = op[0]
			}
		}
		for i, t := range pkTypes {
			v, err := schema.Convert(t, raw[1+i])
			if err != nil {
				return nil, err
			}
			r.PK = append(r.PK, v)
		}
		exists := raw[1+len(pkTypes)]
		r.Exists = exists == int64(1)
		if r.Exists {
			for i, t := range fromTypes {
				v, err := schema.Convert(t, raw[2+len(pkTypes)+i])
				if err != nil {
					return nil, err
				}
				r.From = append(r.From, v)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Lookup implements routing.Querier inside the cycle's snapshot.
func (c *Cycle) Lookup(ctx context.Context, table config.TableName, match []string, tuples [][]any, want []string) ([][]any, error) {
	return lookup(ctx, c.tx, c.s.cat, table, match, tuples, want)
}

func lookup(ctx context.Context, q queryer, cat *Catalog, table config.TableName, match []string, tuples [][]any, want []string) ([][]any, error) {
	ti, ok := cat.Table(table)
	if !ok {
		return nil, fmt.Errorf("table %s not in catalog", table)
	}
	var matchCols, wantCols []*ColumnInfo
	for _, m := range match {
		ci, err := ti.Column(m)
		if err != nil {
			return nil, err
		}
		matchCols = append(matchCols, ci)
	}
	for _, w := range want {
		ci, err := ti.Column(w)
		if err != nil {
			return nil, err
		}
		wantCols = append(wantCols, ci)
	}
	var sel, on, alias []string
	for _, w := range wantCols {
		sel = append(sel, "t."+config.QuoteIdent(w.Name))
	}
	for i, m := range matchCols {
		alias = append(alias, fmt.Sprintf("c%d", i))
		on = append(on, fmt.Sprintf("t.%s = v.c%d", config.QuoteIdent(m.Name), i))
	}
	perChunk := maxParams / len(matchCols)
	if perChunk > maxValuesRows {
		perChunk = maxValuesRows
	}
	set := routing.NewTupleSet()
	for start := 0; start < len(tuples); start += perChunk {
		end := start + perChunk
		if end > len(tuples) {
			end = len(tuples)
		}
		var vals []string
		var args []any
		for _, t := range tuples[start:end] {
			var cells []string
			for i, m := range matchCols {
				args = append(args, paramValue(t[i]))
				cells = append(cells, fmt.Sprintf("CAST(@p%d AS %s)", len(args), castType(m)))
			}
			vals = append(vals, "("+strings.Join(cells, ", ")+")")
		}
		query := fmt.Sprintf("SELECT DISTINCT %s FROM %s t JOIN (VALUES %s) AS v(%s) ON %s",
			strings.Join(sel, ", "), table.Quoted(), strings.Join(vals, ", "), strings.Join(alias, ", "), strings.Join(on, " AND "))
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("routing lookup on %s: %w", table, err)
		}
		for rows.Next() {
			raw := make([]any, len(wantCols))
			ptrs := make([]any, len(raw))
			for i := range raw {
				ptrs[i] = &raw[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, err
			}
			tuple := make([]any, len(raw))
			for i, w := range wantCols {
				v, err := schema.Convert(w.Logical, raw[i])
				if err != nil {
					rows.Close()
					return nil, err
				}
				tuple[i] = v
			}
			if err := set.Add(tuple); err != nil {
				rows.Close()
				return nil, err
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return set.Rows(), nil
}

// castType strips the collation clause, which CAST does not accept.
func castType(c *ColumnInfo) string {
	if i := strings.Index(c.TypeDef, " COLLATE "); i >= 0 {
		return c.TypeDef[:i]
	}
	return c.TypeDef
}

// LoadRoots inserts root ids into #roots (duplicates are dropped by SQL Server using
// the column's own collation) and returns the distinct ids as stored.
func (c *Cycle) LoadRoots(ctx context.Context, roots []any) ([]any, error) {
	td := castType(c.s.rootCol)
	for start := 0; start < len(roots); start += maxValuesRows {
		end := start + maxValuesRows
		if end > len(roots) {
			end = len(roots)
		}
		var vals []string
		var args []any
		for _, r := range roots[start:end] {
			args = append(args, paramValue(r))
			vals = append(vals, fmt.Sprintf("(CAST(@p%d AS %s))", len(args), td))
		}
		if _, err := c.tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id) VALUES %s", rootsTable, strings.Join(vals, ", ")), args...); err != nil {
			return nil, fmt.Errorf("load roots: %w", err)
		}
	}
	return c.queryRoots(ctx, "SELECT id FROM "+rootsTable)
}

// MissingRoots returns ids in #roots that are not documents: their root row no longer
// exists, or it does not meet the root filter.
func (c *Cycle) MissingRoots(ctx context.Context) ([]any, error) {
	q := fmt.Sprintf("SELECT r.id FROM %s r WHERE NOT EXISTS (SELECT 1 FROM %s t WHERE t.%s = r.id%s)",
		rootsTable, c.s.plan.Root.Quoted(), config.QuoteIdent(c.s.rootCol.Name), c.filterClause(" AND "))
	return c.queryRoots(ctx, q)
}

// filterClause returns the root filter joined by sep (" AND " or " WHERE "), or "".
func (c *Cycle) filterClause(sep string) string {
	if c.s.plan.RootFilter == "" {
		return ""
	}
	return sep + "(" + c.s.plan.RootFilter + ")"
}

// ScanRootKeys streams the key of every root row that meets the root filter.
func (c *Cycle) ScanRootKeys(ctx context.Context, fn func(v any) error) error {
	q := fmt.Sprintf("SELECT t.%s FROM %s t%s", config.QuoteIdent(c.s.rootCol.Name), c.s.plan.Root.Quoted(), c.filterClause(" WHERE "))
	rows, err := c.tx.QueryContext(ctx, q)
	if err != nil {
		return fmt.Errorf("scan %s: %w", c.s.plan.Root, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw any
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		v, err := schema.Convert(c.s.rootCol.Logical, raw)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (c *Cycle) queryRoots(ctx context.Context, q string) ([]any, error) {
	rows, err := c.tx.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var raw any
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		v, err := schema.Convert(c.s.rootCol.Logical, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FragmentResult is the raw output of one fragment query.
type FragmentResult struct {
	Columns []schema.Column
	Rows    [][]any
}

// RunFragment executes a fragment against the current #roots.
func (c *Cycle) RunFragment(ctx context.Context, f config.Fragment) (*FragmentResult, error) {
	q := strings.ReplaceAll(f.SQL, config.RootsPlaceholder, rootsTable)
	rows, err := c.tx.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("fragment %s: %w", f.Name, err)
	}
	defer rows.Close()
	cts, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	res := &FragmentResult{}
	for _, ct := range cts {
		col := schema.Column{Name: ct.Name(), DBType: ct.DatabaseTypeName()}
		if p, s, ok := ct.DecimalSize(); ok {
			col.Precision, col.Scale = p, s
		}
		res.Columns = append(res.Columns, col)
	}
	for rows.Next() {
		raw := make([]any, len(cts))
		ptrs := make([]any, len(raw))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, raw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fragment %s: %w", f.Name, err)
	}
	return res, nil
}

// Scan streams typed values of cols for every row of table (used by resnapshots).
func (c *Cycle) Scan(ctx context.Context, table config.TableName, cols []string, fn func([]any) error) error {
	ti := c.s.cat.MustTable(table)
	var sel []string
	var types []schema.Type
	for _, name := range cols {
		ci, err := ti.Column(name)
		if err != nil {
			return err
		}
		sel = append(sel, config.QuoteIdent(ci.Name))
		types = append(types, ci.Logical)
	}
	rows, err := c.tx.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s", strings.Join(sel, ", "), table.Quoted()))
	if err != nil {
		return fmt.Errorf("scan %s: %w", table, err)
	}
	defer rows.Close()
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		vals := make([]any, len(cols))
		for i, t := range types {
			v, err := schema.Convert(t, raw[i])
			if err != nil {
				return err
			}
			vals[i] = v
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// paramValue converts a typed document value into a driver parameter. Every
// parameter is wrapped in CAST(... AS <column type>) in SQL, so plain Go types suffice.
func paramValue(v any) any {
	switch x := v.(type) {
	case doc.UUID:
		return string(x)
	case doc.Decimal:
		return string(x)
	case doc.Date:
		return x.Time()
	case doc.Timestamp:
		return x.Time()
	case doc.TimestampTZ:
		return x.Time()
	case doc.TimeOfDay:
		return time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(x) * time.Microsecond)
	default:
		return v
	}
}

// IsRetryable reports transient errors after which the cycle can simply run again.
func IsRetryable(err error) bool {
	var me mssql.Error
	if errors.As(err, &me) {
		switch me.Number {
		case 1205, // deadlock victim
			1222, // lock request timeout
			3960, // snapshot update conflict
			3961, // snapshot isolation failed: concurrent DDL
			701,  // insufficient memory
			8645: // memory grant timeout
			return true
		}
	}
	return IsConnectionError(err)
}

// IsConnectionError reports errors that require a new session.
func IsConnectionError(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}

// IsSnapshotNotAllowed reports error 3952 (snapshot isolation is not enabled).
func IsSnapshotNotAllowed(err error) bool {
	var me mssql.Error
	return errors.As(err, &me) && me.Number == 3952
}
