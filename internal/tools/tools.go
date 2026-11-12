// Package tools implements docsync's diagnostic commands: doctor, introspect,
// fanout and assemble. They connect to the database but never touch engine state.
package tools

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/Jayx282/docsync/internal/assemble"
	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/engine"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/source/sqlserver"
)

// Env is a database connection plus everything derived from the config.
type Env struct {
	Cfg  *config.Config
	DB   *sql.DB
	Plan *routing.Plan
	Cat  *sqlserver.Catalog
}

// Connect opens the database and loads metadata for every configured table.
func Connect(ctx context.Context, cfg *config.Config) (*Env, error) {
	dsn, err := cfg.DSNValue()
	if err != nil {
		return nil, err
	}
	db, err := sqlserver.Open(ctx, dsn)
	if err != nil {
		return nil, err
	}
	plan, err := routing.Build(cfg)
	if err != nil {
		db.Close()
		return nil, err
	}
	env := &Env{Cfg: cfg, DB: db, Plan: plan}
	env.Cat, err = sqlserver.LoadCatalog(ctx, db, env.tables())
	if err != nil {
		db.Close()
		return nil, err
	}
	return env, nil
}

// NewEnv builds an Env on an existing connection pool (which the caller keeps owning).
func NewEnv(ctx context.Context, cfg *config.Config, db *sql.DB) (*Env, error) {
	plan, err := routing.Build(cfg)
	if err != nil {
		return nil, err
	}
	env := &Env{Cfg: cfg, DB: db, Plan: plan}
	if env.Cat, err = sqlserver.LoadCatalog(ctx, db, env.tables()); err != nil {
		return nil, err
	}
	return env, nil
}

// Close releases the connection.
func (e *Env) Close() error { return e.DB.Close() }

// Assembled is the outcome of assembling a set of roots.
type Assembled struct {
	Docs    map[string]doc.Object // root key -> document
	Errors  map[string]string     // root key -> per-root assembly error
	Missing []any                 // requested roots that do not exist
	Roots   []any                 // canonical root values, in request order where possible
}

// AssembleRoots builds documents for the given root values with schema sch.
func (e *Env) AssembleRoots(ctx context.Context, sch *schema.Schema, values []any) (*Assembled, error) {
	sess, err := sqlserver.NewSession(ctx, e.DB, e.Cat, e.Plan)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	cyc, err := sess.BeginCycle(ctx)
	if err != nil {
		return nil, err
	}
	defer cyc.Rollback()
	canonical, err := cyc.LoadRoots(ctx, values)
	if err != nil {
		return nil, err
	}
	missing, err := cyc.MissingRoots(ctx)
	if err != nil {
		return nil, err
	}
	miss := map[string]bool{}
	for _, m := range missing {
		r, err := assemble.NewRoot(m)
		if err != nil {
			return nil, err
		}
		miss[r.Key] = true
	}
	var roots []assemble.Root
	for _, v := range canonical {
		r, err := assemble.NewRoot(v)
		if err != nil {
			return nil, err
		}
		if !miss[r.Key] {
			roots = append(roots, r)
		}
	}
	var inputs []assemble.Input
	for _, f := range e.Cfg.Document.Fragments {
		fr, err := cyc.RunFragment(ctx, f)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, assemble.Input{Fragment: f, Columns: fr.Columns, Rows: fr.Rows})
	}
	out, err := assemble.Assemble(sch, roots, inputs)
	if err != nil {
		return nil, err
	}
	return &Assembled{Docs: out.Docs, Errors: out.Errors, Missing: missing, Roots: canonical}, nil
}

func (e *Env) tables() []config.TableName {
	var out []config.TableName
	for _, tp := range e.Plan.Tables {
		out = append(out, tp.Table)
	}
	return out
}

// DiscoverSchema runs every fragment with no roots to read its result columns.
func (e *Env) DiscoverSchema(ctx context.Context) (*schema.Schema, []schema.FragmentColumns, error) {
	sess, err := sqlserver.NewSession(ctx, e.DB, e.Cat, e.Plan)
	if err != nil {
		return nil, nil, err
	}
	defer sess.Close()
	cyc, err := sess.BeginCycle(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer cyc.Rollback()
	var frags []schema.FragmentColumns
	for _, f := range e.Cfg.Document.Fragments {
		res, err := cyc.RunFragment(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		frags = append(frags, schema.FragmentColumns{Fragment: f, Columns: res.Columns})
	}
	rc := sess.RootColumn()
	s, err := schema.Build(e.Cfg.Document.Name, rc.Name, schema.Column{Name: rc.Name, DBType: rc.TypeName, Precision: int64(rc.Precision), Scale: int64(rc.Scale)}, frags)
	return s, frags, err
}

// AssembleOne builds the current document for one root id, without touching state.
func (e *Env) AssembleOne(ctx context.Context, id string) (doc.Object, error) {
	sch, _, err := e.DiscoverSchema(ctx)
	if err != nil {
		return nil, err
	}
	v, err := engine.ParseRootID(sch.RootKey.Type, id)
	if err != nil {
		return nil, err
	}
	res, err := e.AssembleRoots(ctx, sch, []any{v})
	if err != nil {
		return nil, err
	}
	if len(res.Missing) > 0 || len(res.Roots) == 0 {
		return nil, fmt.Errorf("root %s does not exist in %s", id, e.Plan.Root)
	}
	r, err := assemble.NewRoot(res.Roots[0])
	if err != nil {
		return nil, err
	}
	if msg, bad := res.Errors[r.Key]; bad {
		return nil, fmt.Errorf("%s", msg)
	}
	return res.Docs[r.Key], nil
}

func quoteCols(alias string, cols []string) string {
	var out []string
	for _, c := range cols {
		out = append(out, alias+"."+config.QuoteIdent(c))
	}
	return strings.Join(out, ", ")
}
