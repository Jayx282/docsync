// Package engine runs docsync's capture → route → assemble → commit → publish loop.
package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Jayx282/docsync/internal/config"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/sink"
	"github.com/Jayx282/docsync/internal/sink/parquet"
	"github.com/Jayx282/docsync/internal/source/sqlserver"
	"github.com/Jayx282/docsync/internal/state"
)

// FaultHook lets tests inject failures at named points. Returning an error aborts
// the operation at that point, as a crash would.
type FaultHook func(point string) error

// Options configures an Engine.
type Options struct {
	Config *config.Config
	Logger *slog.Logger
	Faults FaultHook
	// DB, when set, is used instead of opening the configured DSN.
	DB *sql.DB
}

// Engine is a running docsync instance.
type Engine struct {
	cfg    *config.Config
	log    *slog.Logger
	faults FaultHook
	db     *sql.DB
	ownsDB bool
	store  *state.Store
	cat    *sqlserver.Catalog
	plan   *routing.Plan
	sess   *sqlserver.Session
	schema *schema.Schema
	sinks  []sink.Sink

	cmds chan command

	mu           sync.Mutex
	status       Status
	inflight     map[string]uint64 // sink -> generation of a base being written
	baseRequests map[string]string // sink -> why a base was requested
	baseWG       sync.WaitGroup
	lastTrim     time.Time
	lastRetain   time.Time
	lastSweep    time.Time
	sweepCarry   float64
	baseCtx      context.Context
	baseCancel   context.CancelFunc
}

// Open connects to the database, opens state and sinks, and prepares the first cycle.
func Open(ctx context.Context, opt Options) (*Engine, error) {
	cfg := opt.Config
	e := &Engine{
		cfg:      cfg,
		log:      opt.Logger,
		faults:   opt.Faults,
		cmds:     make(chan command, 64),
		inflight: map[string]uint64{},
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	e.baseCtx, e.baseCancel = context.WithCancel(context.Background())
	ok := false
	defer func() {
		if !ok {
			e.Close()
		}
	}()

	if opt.DB != nil {
		e.db = opt.DB
	} else {
		dsn, err := cfg.DSNValue()
		if err != nil {
			return nil, err
		}
		if e.db, err = sqlserver.Open(ctx, dsn); err != nil {
			return nil, err
		}
		e.ownsDB = true
	}
	var err error
	if e.plan, err = routing.Build(cfg); err != nil {
		return nil, err
	}
	tables := []config.TableName{e.plan.Root}
	for _, tp := range e.plan.Tables {
		tables = append(tables, tp.Table)
	}
	if e.cat, err = sqlserver.LoadCatalog(ctx, e.db, tables); err != nil {
		return nil, err
	}
	if err := e.validateCatalog(); err != nil {
		return nil, err
	}
	if e.store, err = state.Open(cfg.ResolvePath(cfg.State.Dir)); err != nil {
		return nil, err
	}
	if e.sess, err = sqlserver.NewSession(ctx, e.db, e.cat, e.plan); err != nil {
		return nil, err
	}
	if err := e.openSinks(); err != nil {
		return nil, err
	}
	if err := e.prepare(ctx); err != nil {
		return nil, err
	}
	ok = true
	return e, nil
}

func (e *Engine) validateCatalog() error {
	var errs []error
	root := e.cat.MustTable(e.plan.Root)
	if err := e.cat.ValidateKeys(e.plan.Root, e.cfg.Document.Root.Key, true); err != nil {
		errs = append(errs, fmt.Errorf("root table: %w", err))
	}
	if _, err := root.Column(e.plan.RootKey); err != nil {
		errs = append(errs, err)
	}
	for _, tp := range e.plan.Tables {
		if tp.IsRoot {
			continue
		}
		if err := e.cat.ValidateKeys(tp.Table, tp.Key, true); err != nil {
			errs = append(errs, err)
		}
		for _, h := range tp.Hops {
			from, to := e.cat.MustTable(h.From), e.cat.MustTable(h.To)
			for i := range h.FromCols {
				fc, err := from.Column(h.FromCols[i])
				if err != nil {
					errs = append(errs, fmt.Errorf("routing %s: %w", tp.Table, err))
					continue
				}
				tc, err := to.Column(h.ToCols[i])
				if err != nil {
					errs = append(errs, fmt.Errorf("routing %s: %w", tp.Table, err))
					continue
				}
				if fc.Logical.Kind == "" || tc.Logical.Kind == "" {
					errs = append(errs, fmt.Errorf("routing %s: join column types %s/%s are not supported", tp.Table, fc.TypeName, tc.TypeName))
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) openSinks() error {
	for _, sc := range e.cfg.Sinks {
		opt := sink.LogOptions{
			Name:            sc.Name,
			Dir:             e.cfg.ResolvePath(sc.Dir),
			Document:        e.cfg.Document.Name,
			RootKey:         e.plan.RootKey,
			SegmentInterval: sc.SegmentInterval.Duration,
			RetainSegments:  sc.RetainSegments.Duration,
			BaseFileRows:    sc.BaseFileRows,
		}
		var f sink.Format
		switch sc.Type {
		case config.SinkParquetLog:
			f = parquet.Format{IncludeJSON: sc.IncludeJSON}
		case config.SinkNDJSON:
			f = sink.NDJSON{}
		default:
			return fmt.Errorf("unknown sink type %s", sc.Type)
		}
		s, err := sink.NewLogSink(opt, f)
		if err != nil {
			return err
		}
		e.sinks = append(e.sinks, s)
	}
	return nil
}

// prepare discovers the schema and decides whether a (re)snapshot is needed.
func (e *Engine) prepare(ctx context.Context) error {
	sch, err := e.discoverSchema(ctx)
	if err != nil {
		return err
	}
	ckpt, err := e.store.Checkpoint()
	if err != nil {
		return err
	}
	stored, err := e.store.Schema()
	if err != nil {
		return err
	}
	configChanged := ckpt.Initialized && ckpt.ConfigHash != e.cfg.ContentHash()
	schemaChanged := ckpt.Initialized && len(stored) > 0 && ckpt.SchemaHash != sch.Hash()
	switch {
	case !ckpt.Initialized:
		e.schema = sch
		return e.resnapshot(ctx, "initial load", true)
	case ckpt.Snapshotting:
		e.schema = sch
		if schemaChanged || configChanged {
			return e.resnapshot(ctx, "configuration or schema changed during an interrupted snapshot", true)
		}
		return e.resnapshot(ctx, "resuming an interrupted snapshot", false)
	case configChanged:
		e.schema = sch
		return e.resnapshot(ctx, "configuration changed", true)
	case schemaChanged:
		// The database changed shape under an unchanged config: keep emitting nothing until
		// an operator starts a new epoch, so consumers never see mixed document shapes.
		var old schema.Schema
		if err := json.Unmarshal(stored, &old); err != nil {
			return err
		}
		e.schema = &old
		e.setPaused(fmt.Sprintf("schema drift: fragment results changed (stored schema %s, live %s); start a new epoch to rebuild all documents",
			short(ckpt.SchemaHash), short(sch.Hash())))
		return nil
	default:
		e.schema = sch
		return nil
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// discoverSchema runs every fragment against an empty #roots to read its result columns.
func (e *Engine) discoverSchema(ctx context.Context) (*schema.Schema, error) {
	cyc, err := e.sess.BeginCycle(ctx)
	if err != nil {
		return nil, wrapSnapshotErr(err)
	}
	defer cyc.Rollback()
	var frags []schema.FragmentColumns
	for _, f := range e.cfg.Document.Fragments {
		res, err := cyc.RunFragment(ctx, f)
		if err != nil {
			return nil, wrapSnapshotErr(err)
		}
		frags = append(frags, schema.FragmentColumns{Fragment: f, Columns: res.Columns})
	}
	if err := cyc.Commit(); err != nil {
		return nil, err
	}
	rc := e.sess.RootColumn()
	return schema.Build(e.cfg.Document.Name, rc.Name, schema.Column{Name: rc.Name, DBType: rc.TypeName, Precision: int64(rc.Precision), Scale: int64(rc.Scale)}, frags)
}

func wrapSnapshotErr(err error) error {
	if sqlserver.IsSnapshotNotAllowed(err) {
		return fmt.Errorf("%w\nenable it with: ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON", err)
	}
	return err
}

// Close stops background work and releases resources.
func (e *Engine) Close() error {
	if e.baseCancel != nil {
		e.baseCancel()
	}
	e.baseWG.Wait()
	var errs []error
	if e.sess != nil {
		errs = append(errs, e.sess.Close())
	}
	for _, s := range e.sinks {
		errs = append(errs, s.Close())
	}
	if e.store != nil {
		errs = append(errs, e.store.Close())
	}
	if e.db != nil && e.ownsDB {
		errs = append(errs, e.db.Close())
	}
	return errors.Join(errs...)
}

// Schema returns the current document schema.
func (e *Engine) Schema() *schema.Schema { return e.schema }

// Store exposes the state store (read-only use).
func (e *Engine) Store() *state.Store { return e.store }

// Sinks returns the configured sinks.
func (e *Engine) Sinks() []sink.Sink { return e.sinks }

// Plan returns the routing plan.
func (e *Engine) Plan() *routing.Plan { return e.plan }

// Run loops until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		e.drainCommands(ctx)
		if e.isPaused() {
			e.publishAll(ctx)
			if !e.wait(ctx, e.cfg.Source.PollInterval.Duration) {
				return nil
			}
			continue
		}
		res, err := e.RunCycle(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e.recordError(err)
			if errors.Is(err, errDrift) {
				continue
			}
			if !e.wait(ctx, backoff) {
				return nil
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		e.publishAll(ctx)
		e.housekeeping(ctx)
		if !res.MoreWork {
			if !e.wait(ctx, e.cfg.Source.PollInterval.Duration) {
				return nil
			}
		}
	}
}

// Step runs one loop iteration (commands, one cycle, publishing, housekeeping) and
// returns the first error. It is meant for tests and tools that drive the engine.
func (e *Engine) Step(ctx context.Context) (CycleResult, error) {
	e.drainCommands(ctx)
	if e.isPaused() {
		return CycleResult{}, e.publishAll(ctx)
	}
	res, err := e.RunCycle(ctx)
	if err != nil {
		return res, err
	}
	if err := e.publishAll(ctx); err != nil {
		return res, err
	}
	e.housekeeping(ctx)
	return res, nil
}

// WaitForBases blocks until background base snapshots finish.
func (e *Engine) WaitForBases() { e.baseWG.Wait() }

// wait sleeps for d, waking early for commands. It returns false if ctx ended.
func (e *Engine) wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	case c := <-e.cmds:
		e.handle(ctx, c)
		return true
	}
}

func (e *Engine) reconnect(ctx context.Context) {
	if e.sess != nil {
		_ = e.sess.Close()
		e.sess = nil
	}
	sess, err := sqlserver.NewSession(ctx, e.db, e.cat, e.plan)
	if err != nil {
		e.recordError(fmt.Errorf("reconnect: %w", err))
		return
	}
	e.sess = sess
	e.log.Info("reconnected to SQL Server")
}

func (e *Engine) housekeeping(ctx context.Context) {
	now := time.Now()
	e.maybeBase(ctx)
	if err := e.sweep(now); err != nil {
		e.recordError(fmt.Errorf("sweeper: %w", err))
	}
	if now.Sub(e.lastTrim) > 30*time.Second {
		e.lastTrim = now
		if err := e.trimOutbox(now); err != nil {
			e.recordError(fmt.Errorf("trim outbox: %w", err))
		}
	}
	if now.Sub(e.lastRetain) > time.Minute {
		e.lastRetain = now
		for _, s := range e.sinks {
			if err := s.Retain(now); err != nil {
				e.recordError(fmt.Errorf("sink %s retention: %w", s.Name(), err))
			}
		}
	}
}

func (e *Engine) fault(point string) error {
	if e.faults == nil {
		return nil
	}
	if err := e.faults(point); err != nil {
		return fmt.Errorf("fault at %s: %w", point, err)
	}
	return nil
}

func lower(s string) string { return strings.ToLower(s) }
