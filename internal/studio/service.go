package studio

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Jayx282/docsync/internal/assemble"
	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/engine"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/tools"
)

// Service holds the database connection and cached schema used by every studio call.
type Service struct {
	DB      *sql.DB
	DSNEnv  string
	BaseDir string // studio directory: designs/, pipelines/

	mu   sync.Mutex
	snap *Snapshot
}

// Snapshot returns the cached schema, loading it on first use.
func (s *Service) Snapshot(ctx context.Context) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snap != nil {
		return s.snap, nil
	}
	snap, err := LoadSnapshot(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	s.snap = snap
	return snap, nil
}

// Refresh reloads the schema.
func (s *Service) Refresh(ctx context.Context) (*Snapshot, error) {
	snap, err := LoadSnapshot(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	return snap, nil
}

// PipelineDir is where a design's deployed state and output live.
func (s *Service) PipelineDir(name string) string {
	return strings.TrimSuffix(s.BaseDir, "/") + "/pipelines/" + name
}

// Compile compiles a design for this studio.
func (s *Service) Compile(ctx context.Context, d *Design) (*Compiled, error) {
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return Compile(d, snap, CompileOptions{DSNEnv: s.DSNEnv, BaseDir: s.PipelineDir(d.Name)})
}

// PreviewDoc is one assembled document.
type PreviewDoc struct {
	Root  string `json:"root"`
	JSON  string `json:"json,omitempty"`
	Error string `json:"error,omitempty"`
	Bytes int    `json:"bytes"`
}

// PreviewResult is what the designer shows next to a design.
type PreviewResult struct {
	Documents  []PreviewDoc       `json:"documents"`
	Fields     []schema.Field     `json:"fields"`
	SQL        []CompiledFragment `json:"sql"`
	YAML       string             `json:"yaml"`
	Errors     []string           `json:"errors"`
	TookMillis int64              `json:"took_ms"`
}

// Preview compiles a design and assembles a few documents from live data. Design
// problems are returned in Errors (not as an error) so the UI can show them inline.
func (s *Service) Preview(ctx context.Context, d *Design, ids []string, limit int) (*PreviewResult, error) {
	start := time.Now()
	res := &PreviewResult{Documents: []PreviewDoc{}, Fields: []schema.Field{}, SQL: []CompiledFragment{}, Errors: []string{}}
	comp, err := s.Compile(ctx, d)
	if err != nil {
		res.Errors = splitErrors(err)
		return res, nil
	}
	res.SQL, res.YAML = comp.SQL, comp.YAML
	env, err := tools.NewEnv(ctx, comp.Config, s.DB)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	sch, _, err := env.DiscoverSchema(ctx)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	res.Fields = sch.Fields
	if limit <= 0 || limit > 50 {
		limit = 3
	}
	var roots []any
	if len(ids) > 0 {
		for _, id := range ids {
			v, err := engine.ParseRootID(sch.RootKey.Type, id)
			if err != nil {
				res.Errors = append(res.Errors, err.Error())
				return res, nil
			}
			roots = append(roots, v)
		}
	} else if roots, err = s.sampleRoots(ctx, comp.Config, sch, limit); err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	if len(roots) == 0 {
		res.Errors = append(res.Errors, "the root table is empty")
		return res, nil
	}
	out, err := env.AssembleRoots(ctx, sch, roots)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	for _, m := range out.Missing {
		msg := "no such root row"
		if comp.Config.Document.Root.Filter != "" {
			msg = "not a document: there is no such row, or the filter leaves it out"
		}
		res.Documents = append(res.Documents, PreviewDoc{Root: doc.MustJSON(m), Error: msg})
	}
	for _, v := range out.Roots {
		k, err := keyOf(v)
		if err != nil {
			return nil, err
		}
		pd := PreviewDoc{Root: doc.MustJSON(v)}
		if msg, bad := out.Errors[k]; bad {
			pd.Error = msg
		} else if d, ok := out.Docs[k]; ok {
			pd.JSON = doc.MustJSON(d)
			pd.Bytes = len(pd.JSON)
		} else {
			continue // missing; reported above
		}
		res.Documents = append(res.Documents, pd)
	}
	res.TookMillis = time.Since(start).Milliseconds()
	return res, nil
}

func (s *Service) sampleRoots(ctx context.Context, cfg *config.Config, sch *schema.Schema, n int) ([]any, error) {
	root := cfg.RootTable()
	key := config.QuoteIdent(cfg.Document.Root.Key[0])
	where := ""
	if f := cfg.Document.Root.Filter; f != "" {
		where = " WHERE (" + f + ")"
	}
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf("SELECT TOP (%d) t.%s FROM %s t%s ORDER BY t.%s", n, key, root.Quoted(), where, key))
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
		v, err := schema.Convert(sch.RootKey.Type, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FilterCount says how many root rows a design's filter keeps.
type FilterCount struct {
	Errors     []string `json:"errors"`
	Total      int64    `json:"total"`
	Matching   int64    `json:"matching"`
	TookMillis int64    `json:"took_ms"`
}

// CountFilter counts the root rows the design's filter keeps (bounded by ctx).
func (s *Service) CountFilter(ctx context.Context, d *Design) (*FilterCount, error) {
	start := time.Now()
	res := &FilterCount{Errors: []string{}}
	comp, err := s.Compile(ctx, d)
	if err != nil {
		res.Errors = splitErrors(err)
		return res, nil
	}
	cond := comp.Config.Document.Root.Filter
	if cond == "" {
		cond = "1 = 1"
	}
	q := fmt.Sprintf("SELECT COUNT_BIG(*), COUNT_BIG(CASE WHEN %s THEN 1 END) FROM %s t", cond, comp.Config.RootTable().Quoted())
	if err := s.DB.QueryRowContext(ctx, q).Scan(&res.Total, &res.Matching); err != nil {
		res.Errors = append(res.Errors, err.Error())
	}
	res.TookMillis = time.Since(start).Milliseconds()
	return res, nil
}

// checkCalculated computes every calculated field for every document once, so data
// that makes a formula fail (an overflow, say) shows up before deployment.
func (s *Service) checkCalculated(ctx context.Context, cfg *config.Config, d *Design) *tools.Finding {
	frag, ok := cfg.Fragment(calculatedFragment)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	key := config.QuoteIdent(cfg.Document.Root.Key[0])
	where := ""
	if f := cfg.Document.Root.Filter; f != "" {
		where = " WHERE (" + f + ")"
	}
	all := fmt.Sprintf("(SELECT t.%s AS id FROM %s t%s)", key, cfg.RootTable().Quoted(), where)
	counts := []string{"COUNT_BIG(*)"}
	for _, c := range d.Calculated {
		counts = append(counts, "COUNT_BIG(c."+config.QuoteIdent(c.Name)+")") // forces each value to be computed
	}
	q := "SELECT " + strings.Join(counts, ", ") + " FROM (" + strings.ReplaceAll(frag.SQL, config.RootsPlaceholder, all) + ") c"
	vals := make([]any, len(counts))
	ptrs := make([]any, len(counts))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err := s.DB.QueryRowContext(ctx, q).Scan(ptrs...)
	switch {
	case ctx.Err() != nil:
		return &tools.Finding{Severity: tools.Warn, Check: "calculated fields", Detail: "computing them for every document took over 60 seconds, so only part of the data was checked"}
	case err != nil:
		return &tools.Finding{Severity: tools.Fail, Check: "calculated fields", Detail: "a formula fails on some rows: " + err.Error()}
	}
	return &tools.Finding{Severity: tools.OK, Check: "calculated fields",
		Detail: fmt.Sprintf("computed for all %v documents without errors (%d ms)", vals[0], time.Since(start).Milliseconds())}
}

// DeployPlan describes what deploying a design would change.
type DeployPlan struct {
	Errors     []string      `json:"errors"`
	Deployed   bool          `json:"deployed"` // a config for this design exists
	DeployedAt time.Time     `json:"deployed_at,omitzero"`
	Current    string        `json:"current"` // deployed config (without its generated header)
	Next       string        `json:"next"`    // config this deployment would write
	Same       bool          `json:"same"`    // nothing would change
	Rebuild    bool          `json:"rebuild"` // the document definition changes: new epoch, full rebuild
	Running    RuntimeStatus `json:"running"`
}

// PlanDeploy compares a design's compiled config with the deployed one.
func (s *Service) PlanDeploy(ctx context.Context, d *Design, rt *Runtime) (*DeployPlan, error) {
	plan := &DeployPlan{Errors: []string{}, Running: rt.Status()}
	comp, err := s.Compile(ctx, d)
	if err != nil {
		plan.Errors = splitErrors(err)
		return plan, nil
	}
	plan.Next = stripHeader(comp.YAML)
	path := filepath.Join(s.PipelineDir(d.Name), "docsync.yaml")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return plan, nil
	}
	if err != nil {
		return nil, err
	}
	plan.Deployed = true
	if info, err := os.Stat(path); err == nil {
		plan.DeployedAt = info.ModTime().UTC()
	}
	plan.Current = stripHeader(string(b))
	plan.Same = plan.Current == plan.Next
	if old, err := config.Parse(b); err != nil || old.ContentHash() != comp.Config.ContentHash() {
		plan.Rebuild = true
	}
	return plan, nil
}

// stripHeader drops the generated comment lines (they carry a timestamp).
func stripHeader(yml string) string {
	lines := strings.Split(yml, "\n")
	for len(lines) > 0 && strings.HasPrefix(lines[0], "#") {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

// ValidateResult lists checks a design must pass before deployment.
type ValidateResult struct {
	OK       bool            `json:"ok"`
	Errors   []string        `json:"errors"`
	Findings []tools.Finding `json:"findings"`
	Script   string          `json:"script"` // SQL for a DBA: change tracking, indexes, permissions
}

// Validate runs the doctor checks for a design and writes the SQL a DBA must run.
func (s *Service) Validate(ctx context.Context, d *Design) (*ValidateResult, error) {
	res := &ValidateResult{Errors: []string{}, Findings: []tools.Finding{}}
	snap, err := s.Refresh(ctx) // validation must see current settings
	if err != nil {
		return nil, err
	}
	comp, err := s.Compile(ctx, d)
	if err != nil {
		res.Errors = splitErrors(err)
		return res, nil
	}
	env, err := tools.NewEnv(ctx, comp.Config, s.DB)
	if err != nil {
		res.Errors = append(res.Errors, err.Error())
		return res, nil
	}
	res.Findings = env.Doctor(ctx)
	for _, r := range d.Related {
		if r.Shape == ShapeFields && r.Via == nil && !snap.IsUnique(r.Table, toCols(r.Join)) {
			res.Findings = append(res.Findings, tools.Finding{Severity: tools.Warn, Check: "relation " + r.ID,
				Detail: fmt.Sprintf("%s.%v is not unique, so several rows may match one document; such documents are reported as errors. Show it as a list, or join on a unique column.", short(r.Table), toCols(r.Join))})
		}
	}
	if f := s.checkCalculated(ctx, comp.Config, d); f != nil {
		res.Findings = append(res.Findings, *f)
	}
	res.Script = dbaScript(snap, comp.Config)
	res.OK = len(res.Errors) == 0
	for _, f := range res.Findings {
		if f.Severity == tools.Fail {
			res.OK = false
		}
	}
	return res, nil
}

// dbaScript writes the statements needed before deployment.
func dbaScript(snap *Snapshot, cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("-- Run as a user allowed to change the database (review first).\n")
	if !snap.Snapshot {
		b.WriteString("ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON;\n")
	}
	if !snap.CT {
		b.WriteString("ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 3 DAYS, AUTO_CLEANUP = ON);\n")
	}
	tables := []string{cfg.Document.Root.Table}
	for _, r := range cfg.Routing {
		if !r.Static {
			tables = append(tables, r.Table)
		}
	}
	var enable, grant []string
	for _, t := range tables {
		tn := config.MustParseTableName(t)
		if tt, ok := snap.Table(t); ok && !tt.CTEnabled {
			enable = append(enable, fmt.Sprintf("ALTER TABLE %s ENABLE CHANGE_TRACKING;", tn.Quoted()))
		}
		grant = append(grant, fmt.Sprintf("GRANT SELECT, VIEW CHANGE TRACKING ON %s TO [docsync_login];", tn.Quoted()))
	}
	for _, e := range enable {
		b.WriteString(e + "\n")
	}
	// Indexes that routing lookups and fragment joins rely on.
	var idx []string
	seen := map[string]bool{}
	want := func(table string, cols []string) {
		if snap.HasLeadingIndex(table, cols) {
			return
		}
		k := strings.ToLower(table + "|" + strings.Join(cols, ","))
		if seen[k] {
			return
		}
		seen[k] = true
		tn := config.MustParseTableName(table)
		idx = append(idx, fmt.Sprintf("CREATE INDEX %s ON %s (%s);", config.QuoteIdent("ix_"+tn.Name+"_"+strings.Join(cols, "_")), tn.Quoted(), strings.Join(quoteAll(cols), ", ")))
	}
	for _, r := range cfg.Routing {
		if r.Next == nil {
			continue
		}
		var from, to []string
		for _, p := range r.Next.Pairs() {
			from, to = append(from, p.From), append(to, p.To)
		}
		want(r.Table, from)
		want(r.Next.Table, to)
	}
	sort.Strings(idx)
	if len(idx) > 0 {
		b.WriteString("\n-- Indexes for change routing and document queries:\n")
		for _, i := range idx {
			b.WriteString(i + "\n")
		}
	}
	b.WriteString("\n-- Permissions for the login docsync uses (rename docsync_login):\n")
	for _, g := range grant {
		b.WriteString(g + "\n")
	}
	return b.String()
}

func quoteAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = config.QuoteIdent(c)
	}
	return out
}

func splitErrors(err error) []string {
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func keyOf(v any) (string, error) {
	r, err := assemble.NewRoot(v)
	if err != nil {
		return "", err
	}
	return r.Key, nil
}
