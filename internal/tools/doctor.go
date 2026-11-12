package tools

import (
	"context"
	"database/sql"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/schema"
)

// Severity of a doctor finding.
type Severity string

// Severities.
const (
	OK   Severity = "ok"
	Warn Severity = "warn"
	Fail Severity = "FAIL"
)

// Finding is one doctor check result.
type Finding struct {
	Severity Severity `json:"severity"`
	Check    string   `json:"check"`
	Detail   string   `json:"detail"`
}

// Doctor checks everything docsync needs from the database and the config.
func (e *Env) Doctor(ctx context.Context) []Finding {
	var out []Finding
	add := func(s Severity, check, format string, args ...any) {
		out = append(out, Finding{Severity: s, Check: check, Detail: fmt.Sprintf(format, args...)})
	}

	var version, edition, product string
	if err := e.DB.QueryRowContext(ctx, `SELECT @@VERSION, CAST(SERVERPROPERTY('Edition') AS nvarchar(128)), CAST(SERVERPROPERTY('ProductVersion') AS nvarchar(128))`).Scan(&version, &edition, &product); err != nil {
		add(Fail, "connection", "%v", err)
		return out
	}
	add(OK, "server", "%s (%s, %s)", strings.SplitN(version, "\n", 2)[0], edition, product)

	var dbName string
	var snapshot int
	if err := e.DB.QueryRowContext(ctx, `SELECT DB_NAME(), snapshot_isolation_state FROM sys.databases WHERE database_id = DB_ID()`).Scan(&dbName, &snapshot); err != nil {
		add(Fail, "database", "%v", err)
		return out
	}
	if snapshot == 1 {
		add(OK, "snapshot isolation", "allowed on %s", dbName)
	} else {
		add(Fail, "snapshot isolation", "not allowed on %s: ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON", dbName)
	}
	var retention int
	var units, cleanup string
	err := e.DB.QueryRowContext(ctx, `SELECT retention_period, retention_period_units_desc, CASE WHEN is_auto_cleanup_on = 1 THEN 'on' ELSE 'off' END FROM sys.change_tracking_databases WHERE database_id = DB_ID()`).Scan(&retention, &units, &cleanup)
	switch {
	case err == sql.ErrNoRows:
		add(Fail, "change tracking", "not enabled on %s: ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 3 DAYS, AUTO_CLEANUP = ON)", dbName)
	case err != nil:
		add(Fail, "change tracking", "%v", err)
	default:
		sev := OK
		note := ""
		if strings.EqualFold(units, "MINUTES") || (strings.EqualFold(units, "HOURS") && retention < 24) {
			sev, note = Warn, "; docsync must not be stopped longer than this or it will rebuild from scratch"
		}
		add(sev, "change tracking", "enabled, retention %d %s, auto cleanup %s%s", retention, strings.ToLower(units), cleanup, note)
	}

	// Tables, keys, change tracking and permissions.
	for _, tp := range e.Plan.Tables {
		if err := e.Cat.ValidateKeys(tp.Table, tp.Key, true); err != nil {
			add(Fail, "table "+tp.Table.String(), "%v", err)
		} else {
			add(OK, "table "+tp.Table.String(), "primary key %v, change tracking on", tp.Key)
		}
		var perm sql.NullInt64
		if err := e.DB.QueryRowContext(ctx, `SELECT HAS_PERMS_BY_NAME(@p1, 'OBJECT', 'VIEW CHANGE TRACKING')`, tp.Table.String()).Scan(&perm); err == nil && perm.Int64 != 1 {
			add(Fail, "permission "+tp.Table.String(), "the login lacks VIEW CHANGE TRACKING (GRANT VIEW CHANGE TRACKING ON %s TO <login>)", tp.Table.Quoted())
		}
	}
	for _, st := range e.Plan.Static {
		var oid sql.NullInt64
		if err := e.DB.QueryRowContext(ctx, `SELECT OBJECT_ID(@p1)`, st.Quoted()).Scan(&oid); err != nil || !oid.Valid {
			add(Fail, "static table "+st.String(), "does not exist")
		} else {
			add(OK, "static table "+st.String(), "exists (not tracked: rebuild documents yourself after changing it)")
		}
	}

	// Indexes that routing lookups and fragment joins rely on.
	for _, tp := range e.Plan.Tables {
		for _, h := range tp.Hops {
			for _, side := range []struct {
				table config.TableName
				cols  []string
				why   string
			}{
				{h.To, h.ToCols, "routing looks up " + h.To.String() + " by these columns"},
				{h.From, h.FromCols, "fragments usually join " + h.From.String() + " on these columns"},
			} {
				ok, err := hasLeadingIndex(ctx, e.DB, side.table, side.cols)
				if err != nil {
					add(Warn, "index", "%s: %v", side.table, err)
					continue
				}
				if !ok {
					add(Warn, "index "+side.table.String(), "no index starts with (%s); %s. Consider: CREATE INDEX ix_%s_%s ON %s (%s)",
						strings.Join(side.cols, ", "), side.why, side.table.Name, strings.Join(side.cols, "_"), side.table.Quoted(), strings.Join(side.cols, ", "))
				}
			}
		}
	}

	// Fragment result columns.
	sch, frags, err := e.DiscoverSchema(ctx)
	if err != nil {
		add(Fail, "fragments", "%v", err)
	} else {
		add(OK, "fragments", "%d fragments, %d document fields, schema %s", len(frags), len(sch.Fields), sch.Hash()[:12])
		for _, fc := range frags {
			add(OK, "fragment "+fc.Fragment.Name, "%s", describeColumns(fc.Columns))
		}
	}

	// Tables read by fragments but not declared: their changes would be missed.
	declared := map[string]bool{}
	for _, tp := range e.Plan.Tables {
		declared[tp.Table.Key()] = true
	}
	for _, st := range e.Plan.Static {
		declared[st.Key()] = true
	}
	for _, f := range e.Cfg.Document.Fragments {
		tables, err := e.planTables(ctx, f)
		if err != nil {
			add(Warn, "dependencies "+f.Name, "could not read the query plan (needs SHOWPLAN permission): %v", err)
			continue
		}
		var missing []string
		for _, t := range tables {
			if !declared[t.Key()] {
				missing = append(missing, t.String())
			}
		}
		if len(missing) > 0 {
			add(Fail, "dependencies "+f.Name, "reads %s, which %s not in routing; changes there would be missed. Add a routing rule, or mark it static: true",
				strings.Join(missing, ", "), map[bool]string{true: "is", false: "are"}[len(missing) == 1])
		} else {
			add(OK, "dependencies "+f.Name, "reads only declared tables (%d)", len(tables))
		}
	}

	// The root filter: how many rows it keeps, and that it reads only the root table
	// (a change anywhere else would not re-check which rows are documents).
	if filter := e.Plan.RootFilter; filter != "" {
		root := e.Plan.Root
		var total, kept int64
		q := fmt.Sprintf("SELECT COUNT_BIG(*), COUNT_BIG(CASE WHEN %s THEN 1 END) FROM %s t", filter, root.Quoted())
		if err := e.DB.QueryRowContext(ctx, q).Scan(&total, &kept); err != nil {
			add(Fail, "root filter", "the filter does not run: %v", err)
		} else {
			add(OK, "root filter", "%d of %d %s rows become documents", kept, total, root)
		}
		probe := config.Fragment{Name: "root filter", SQL: fmt.Sprintf("SELECT r.id AS _root FROM %s r JOIN %s t ON t.%s = r.id WHERE (%s)",
			config.RootsPlaceholder, root.Quoted(), config.QuoteIdent(e.Plan.RootKey), filter)}
		tables, err := e.planTables(ctx, probe)
		if err != nil {
			add(Warn, "dependencies root filter", "could not read the query plan (needs SHOWPLAN permission): %v", err)
		} else {
			var other []string
			for _, t := range tables {
				if t.Key() != root.Key() {
					other = append(other, t.String())
				}
			}
			if len(other) > 0 {
				add(Fail, "dependencies root filter", "reads %s; a root filter may use only %s's own columns, because changes elsewhere would not re-check which rows are documents",
					strings.Join(other, ", "), root)
			} else {
				add(OK, "dependencies root filter", "reads only %s", root)
			}
		}
	}
	return out
}

func describeColumns(cols []schema.Column) string {
	var parts []string
	for _, c := range cols {
		if strings.EqualFold(c.Name, config.RootColumn) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", c.Name, strings.ToLower(c.DBType)))
	}
	return strings.Join(parts, ", ")
}

func hasLeadingIndex(ctx context.Context, db *sql.DB, t config.TableName, cols []string) (bool, error) {
	rows, err := db.QueryContext(ctx, `
SELECT i.index_id, c.name, ic.key_ordinal
FROM sys.indexes i
JOIN sys.index_columns ic ON ic.object_id = i.object_id AND ic.index_id = i.index_id AND ic.key_ordinal > 0
JOIN sys.columns c ON c.object_id = ic.object_id AND c.column_id = ic.column_id
WHERE i.object_id = OBJECT_ID(@p1)
ORDER BY i.index_id, ic.key_ordinal`, t.Quoted())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	idx := map[int][]string{}
	for rows.Next() {
		var id, ord int
		var name string
		if err := rows.Scan(&id, &name, &ord); err != nil {
			return false, err
		}
		idx[id] = append(idx[id], strings.ToLower(name))
	}
	want := map[string]bool{}
	for _, c := range cols {
		want[strings.ToLower(c)] = true
	}
	for _, keyCols := range idx {
		if len(keyCols) < len(cols) {
			continue
		}
		ok := true
		for _, c := range keyCols[:len(cols)] {
			if !want[c] {
				ok = false
			}
		}
		if ok {
			return true, nil
		}
	}
	return false, rows.Err()
}

type showPlanObject struct {
	Database string `xml:"Database,attr"`
	Schema   string `xml:"Schema,attr"`
	Table    string `xml:"Table,attr"`
}

// planTables compiles a fragment with SHOWPLAN_XML (nothing is executed) and lists
// every base table in the plan. Views and inline functions expand to their tables.
func (e *Env) planTables(ctx context.Context, f config.Fragment) ([]config.TableName, error) {
	conn, err := e.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rc := e.Cat.MustTable(e.Plan.Root)
	col, err := rc.Column(e.Plan.RootKey)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE #roots (id %s NOT NULL PRIMARY KEY)", col.TypeDef)); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "SET SHOWPLAN_XML ON"); err != nil {
		return nil, err
	}
	defer conn.ExecContext(context.Background(), "SET SHOWPLAN_XML OFF")
	var plan string
	if err := conn.QueryRowContext(ctx, strings.ReplaceAll(f.SQL, config.RootsPlaceholder, "#roots")).Scan(&plan); err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(strings.NewReader(plan))
	seen := map[string]config.TableName{}
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Object" {
			continue
		}
		var o showPlanObject
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "Schema":
				o.Schema = a.Value
			case "Table":
				o.Table = a.Value
			case "Database":
				o.Database = a.Value
			}
		}
		if o.Table == "" || strings.HasPrefix(strings.Trim(o.Table, "[]"), "#") || strings.EqualFold(strings.Trim(o.Database, "[]"), "tempdb") {
			continue
		}
		t, err := config.ParseTableName(o.Schema + "." + o.Table)
		if err != nil {
			continue
		}
		seen[t.Key()] = t
	}
	out := make([]config.TableName, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}
