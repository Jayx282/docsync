package tools

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/routing"
)

// FanoutStat measures how many documents one source row reaches.
type FanoutStat struct {
	Table    string
	Kind     string // "direct", "lookup" or "multi-hop"
	Rows     int64  // source rows that reach at least one document
	Max      int64
	Avg      float64
	P99      float64
	Top      []FanoutKey
	Query    string
	Duration string
}

// FanoutKey is one of the source rows with the widest reach.
type FanoutKey struct {
	Key   string
	Roots int64
}

// Fanout measures, for each routing table, how many root documents a single
// source row affects: the cost of changing that row. Queries scan whole tables;
// run them on a replica or off-peak for large databases.
func (e *Env) Fanout(ctx context.Context, only string) ([]FanoutStat, error) {
	var out []FanoutStat
	for _, tp := range e.Plan.Tables {
		if tp.IsRoot || (only != "" && !strings.EqualFold(tp.Table.String(), only) && !strings.EqualFold(tp.Table.Name, only)) {
			continue
		}
		st, err := e.fanoutOne(ctx, tp)
		if err != nil {
			return out, fmt.Errorf("%s: %w", tp.Table, err)
		}
		out = append(out, st)
	}
	return out, nil
}

func (e *Env) fanoutOne(ctx context.Context, tp *routing.TablePlan) (FanoutStat, error) {
	st := FanoutStat{Table: tp.Table.String(), Kind: "direct"}
	if len(tp.Hops) > 1 {
		st.Kind = "multi-hop"
	} else if tp.Hops[0].Kind == config.KindReferencedBy {
		st.Kind = "lookup"
	}
	from := fmt.Sprintf("%s a0", tp.Table.Quoted())
	for i, h := range tp.Hops {
		var on []string
		for k := range h.FromCols {
			on = append(on, fmt.Sprintf("a%d.%s = a%d.%s", i+1, config.QuoteIdent(h.ToCols[k]), i, config.QuoteIdent(h.FromCols[k])))
		}
		from += fmt.Sprintf(" JOIN %s a%d ON %s", h.To.Quoted(), i+1, strings.Join(on, " AND "))
	}
	last := len(tp.Hops)
	pk := quoteCols("a0", tp.Key)
	grouped := fmt.Sprintf("SELECT %s, COUNT(DISTINCT a%d.%s) AS roots FROM %s GROUP BY %s",
		pk, last, config.QuoteIdent(e.Plan.RootKey), from, pk)
	st.Query = grouped

	stats := fmt.Sprintf(`WITH f AS (%s)
SELECT COUNT_BIG(*), ISNULL(MAX(roots), 0), ISNULL(AVG(CAST(roots AS float)), 0),
       ISNULL((SELECT TOP (1) PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY roots) OVER () FROM f), 0)
FROM f`, grouped)
	if err := e.DB.QueryRowContext(ctx, stats).Scan(&st.Rows, &st.Max, &st.Avg, &st.P99); err != nil {
		return st, err
	}
	rows, err := e.DB.QueryContext(ctx, fmt.Sprintf("SELECT TOP (5) * FROM (%s) f ORDER BY roots DESC", grouped))
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]any, len(tp.Key)+1)
		ptrs := make([]any, len(vals))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return st, err
		}
		var parts []string
		for i, k := range tp.Key {
			parts = append(parts, fmt.Sprintf("%s=%v", k, display(vals[i])))
		}
		n, _ := vals[len(vals)-1].(int64)
		st.Top = append(st.Top, FanoutKey{Key: strings.Join(parts, ","), Roots: n})
	}
	return st, rows.Err()
}

func display(v any) string {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case nil:
		return "NULL"
	case sql.RawBytes:
		return string(x)
	default:
		if s, err := doc.JSON(x); err == nil {
			return string(s)
		}
		return fmt.Sprint(x)
	}
}
