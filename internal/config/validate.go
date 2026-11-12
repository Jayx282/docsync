package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RootsPlaceholder is replaced with the temp table holding the root ids of a cycle.
const RootsPlaceholder = "{{roots}}"

// RootColumn is the column every fragment must select to identify its document.
const RootColumn = "_root"

var (
	// A bare * right after SELECT (or a comma in the select list) and before a comma,
	// FROM, INTO or the end. COUNT(*) and multiplication do not match.
	selectStar  = regexp.MustCompile(`(?is)(\bselect\b|,)\s*(all\s+|distinct\s+)?(top\s*\(?\s*[^\s)]+\s*\)?\s*(percent\s+)?(with\s+ties\s+)?)?\*\s*(,|\bfrom\b|\binto\b|$)`)
	dottedStar  = regexp.MustCompile(`(?s)[\w\]"]\s*\.\s*\*`)
	identRegexp = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks the configuration for mistakes that can be found without a database.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Source.Type != "sqlserver" {
		add("source.type must be \"sqlserver\" (got %q)", c.Source.Type)
	}
	if c.Source.DSN == "" && c.Source.DSNEnv == "" {
		add("source: set dsn_env (recommended) or dsn")
	}
	if c.Source.PollInterval.Duration < 50_000_000 {
		add("source.poll_interval must be at least 50ms")
	}
	if c.Source.AssembleBudget < 1 {
		add("source.assemble_budget must be at least 1")
	}
	if c.Source.LowPriorityShare < 0 || c.Source.LowPriorityShare >= 1 {
		add("source.low_priority_share must be between 0 and 1")
	}
	if c.Source.SweepPerMinute < 0 {
		add("source.sweep_per_minute must be zero (off) or positive")
	}

	// Document.
	d := c.Document
	if d.Name == "" {
		add("document.name is required")
	}
	root, rootErr := ParseTableName(d.Root.Table)
	if rootErr != nil {
		add("document.root.table: %v", rootErr)
	}
	if len(d.Root.Key) != 1 {
		add("document.root.key must list exactly one column (composite root keys are not supported yet)")
	}
	if strings.Contains(d.Root.Filter, RootsPlaceholder) {
		add("document.root.filter is a condition on the root table (alias t); it cannot use %s", RootsPlaceholder)
	}
	if len(d.Fragments) == 0 {
		add("document.fragments: at least one fragment is required")
	}
	fragNames := map[string]bool{}
	paths := map[string]bool{}
	for i, f := range d.Fragments {
		where := fmt.Sprintf("document.fragments[%d] (%s)", i, f.Name)
		if f.Name == "" {
			add("document.fragments[%d].name is required", i)
		} else if fragNames[f.Name] {
			add("%s: duplicate fragment name", where)
		}
		fragNames[f.Name] = true
		switch f.Cardinality {
		case CardinalityOne:
			if len(f.SortKey) > 0 {
				add("%s: sort_key only applies to cardinality many", where)
			}
		case CardinalityMany:
			if f.Path == "" {
				add("%s: cardinality many requires path (the list field name)", where)
			}
			if len(f.SortKey) == 0 {
				add("%s: cardinality many requires sort_key so list order is deterministic", where)
			}
		default:
			add("%s: cardinality must be %q or %q", where, CardinalityOne, CardinalityMany)
		}
		if f.Path != "" {
			if !identRegexp.MatchString(f.Path) {
				add("%s: path %q must be a simple identifier", where, f.Path)
			}
			if paths[strings.ToLower(f.Path)] {
				add("%s: path %q is used by another fragment", where, f.Path)
			}
			paths[strings.ToLower(f.Path)] = true
		}
		if strings.TrimSpace(f.SQL) == "" {
			add("%s: sql is required", where)
			continue
		}
		if !strings.Contains(f.SQL, RootsPlaceholder) {
			add("%s: sql must join %s and select its id column AS %s", where, RootsPlaceholder, RootColumn)
		}
		if selectStar.MatchString(f.SQL) || dottedStar.MatchString(f.SQL) {
			add("%s: SELECT * is not allowed; list columns explicitly so schema changes are detected", where)
		}
	}

	// Routing.
	rules := map[string]RoutingRule{}
	for i, r := range c.Routing {
		t, err := ParseTableName(r.Table)
		if err != nil {
			add("routing[%d].table: %v", i, err)
			continue
		}
		where := fmt.Sprintf("routing[%d] (%s)", i, r.Table)
		if rootErr == nil && t.Key() == root.Key() {
			add("%s: the root table routes to itself automatically; remove this rule", where)
			continue
		}
		if _, dup := rules[t.Key()]; dup {
			add("%s: duplicate routing rule", where)
			continue
		}
		rules[t.Key()] = r
		if r.Static {
			if r.Next != nil || len(r.Key) > 0 || r.HotFanoutLimit != 0 {
				add("%s: static tables are not tracked; remove key, next and hot_fanout_limit", where)
			}
			continue
		}
		if len(r.Key) == 0 {
			add("%s: key (the table's primary key columns) is required", where)
		}
		if r.HotFanoutLimit < 0 {
			add("%s: hot_fanout_limit must be zero (unlimited) or positive", where)
		}
		if r.Next == nil {
			add("%s: next is required (or mark the table static)", where)
			continue
		}
		if _, err := ParseTableName(r.Next.Table); err != nil {
			add("%s: next.table: %v", where, err)
		}
		if len(r.Next.Join) == 0 {
			add("%s: next.join must map at least one column", where)
		}
		if r.Next.Kind != KindReferences && r.Next.Kind != KindReferencedBy {
			add("%s: next.kind must be %q or %q", where, KindReferences, KindReferencedBy)
		}
	}
	// Every non-static rule must reach the root through other non-static rules, without cycles.
	if rootErr == nil {
		for _, r := range c.Routing {
			if r.Static || r.Next == nil {
				continue
			}
			t, err := ParseTableName(r.Table)
			if err != nil {
				continue
			}
			seen := map[string]bool{t.Key(): true}
			cur := r
			for {
				next, err := ParseTableName(cur.Next.Table)
				if err != nil {
					break
				}
				if next.Key() == root.Key() {
					break
				}
				nr, ok := rules[next.Key()]
				if !ok {
					add("routing (%s): next table %s has no routing rule, so changes cannot reach the root", r.Table, cur.Next.Table)
					break
				}
				if nr.Static || nr.Next == nil {
					add("routing (%s): path to the root passes through %s, which is static or has no next", r.Table, cur.Next.Table)
					break
				}
				if seen[next.Key()] {
					add("routing (%s): cycle detected at %s", r.Table, cur.Next.Table)
					break
				}
				seen[next.Key()] = true
				cur = nr
			}
		}
	}

	// Sinks.
	if len(c.Sinks) == 0 {
		add("sinks: at least one sink is required")
	}
	sinkNames := map[string]bool{}
	for i, s := range c.Sinks {
		where := fmt.Sprintf("sinks[%d] (%s)", i, s.Name)
		if sinkNames[s.Name] {
			add("%s: duplicate sink name", where)
		}
		sinkNames[s.Name] = true
		if s.Type != SinkParquetLog && s.Type != SinkNDJSON {
			add("%s: type must be %q or %q", where, SinkParquetLog, SinkNDJSON)
		}
		if s.Dir == "" {
			add("%s: dir is required", where)
		}
		if s.BaseFileRows < 1 {
			add("%s: base_file_rows must be positive", where)
		}
	}
	if c.State.Dir == "" {
		add("state.dir is required")
	}
	if c.Dashboard.BasicAuthUser != "" && c.Dashboard.BasicAuthPasswordEnv == "" {
		add("dashboard: basic_auth_user requires basic_auth_password_env")
	}
	return errors.Join(errs...)
}

// JoinPair is one column pair of a link, in deterministic order.
type JoinPair struct {
	From string // column on the routing table
	To   string // column on the next table
}

// Pairs returns the join columns sorted by the routing table's column name.
func (l Link) Pairs() []JoinPair {
	out := make([]JoinPair, 0, len(l.Join))
	for k, v := range l.Join {
		out = append(out, JoinPair{From: k, To: v})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].From) < strings.ToLower(out[j].From) })
	return out
}
