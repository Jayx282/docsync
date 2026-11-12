package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Jayx282/docsync/internal/assemble"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/source/sqlserver"
	"github.com/Jayx282/docsync/internal/state"
)

// TableStats tracks change activity for one source table.
type TableStats struct {
	Changes      uint64    `json:"changes"`
	LastChangeAt time.Time `json:"last_change_at"`
	LastRoots    int       `json:"last_roots"`
	MaxRoots     int       `json:"max_roots"`
}

// Status is the engine's in-memory activity record.
type Status struct {
	StartedAt         time.Time              `json:"started_at"`
	Activity          string                 `json:"activity,omitempty"`
	Paused            bool                   `json:"paused"`
	PauseReason       string                 `json:"pause_reason,omitempty"`
	LastError         string                 `json:"last_error,omitempty"`
	LastErrorAt       time.Time              `json:"last_error_at,omitempty"`
	Cycles            uint64                 `json:"cycles"`
	LastCycleAt       time.Time              `json:"last_cycle_at"`
	LastCycleDuration time.Duration          `json:"last_cycle_duration_ns"`
	LastChangeAt      time.Time              `json:"last_change_at"`
	Changes           uint64                 `json:"changes"`
	RootsAssembled    uint64                 `json:"roots_assembled"`
	Upserts           uint64                 `json:"upserts"`
	Deletes           uint64                 `json:"deletes"`
	RootErrors        uint64                 `json:"root_errors"`
	SweepCorrections  uint64                 `json:"sweep_corrections"`
	Tables            map[string]*TableStats `json:"tables"`
}

// SinkView describes one sink's progress.
type SinkView struct {
	Name         string    `json:"name"`
	Generation   uint64    `json:"generation"`
	Epoch        uint64    `json:"epoch"`
	HasBase      bool      `json:"has_base"`
	BaseAt       time.Time `json:"base_at,omitempty"`
	Lag          uint64    `json:"lag_generations"`
	BaseInFlight bool      `json:"base_in_flight"`
}

// StatusView is a point-in-time view for the dashboard and CLI.
type StatusView struct {
	Status
	Document   string           `json:"document"`
	SchemaHash string           `json:"schema_hash"`
	Checkpoint state.Checkpoint `json:"checkpoint"`
	Pending    uint64           `json:"pending"`
	Sinks      []SinkView       `json:"sinks"`
}

func (e *Engine) recordCycle(res CycleResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if e.status.StartedAt.IsZero() {
		e.status.StartedAt = now
	}
	e.status.Cycles++
	e.status.LastCycleAt = now
	e.status.LastCycleDuration = res.Duration
	if err != nil {
		return
	}
	e.status.Changes += uint64(res.Changes)
	e.status.RootsAssembled += uint64(res.Roots)
	e.status.Upserts += uint64(res.Upserts)
	e.status.Deletes += uint64(res.Deletes)
	e.status.RootErrors += uint64(res.Errors)
	e.status.SweepCorrections += uint64(res.SweepCorrections)
	if res.Changes > 0 {
		e.status.LastChangeAt = now
	}
	cycleMetrics(res)
}

func (e *Engine) noteTable(name string, changes, roots int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.status.Tables == nil {
		e.status.Tables = map[string]*TableStats{}
	}
	ts := e.status.Tables[name]
	if ts == nil {
		ts = &TableStats{}
		e.status.Tables[name] = ts
	}
	ts.Changes += uint64(changes)
	ts.LastChangeAt = time.Now()
	ts.LastRoots = roots
	if roots > ts.MaxRoots {
		ts.MaxRoots = roots
	}
	tableMetrics(name, changes, roots)
}

func (e *Engine) recordError(err error) {
	if err == nil {
		return
	}
	e.log.Error("docsync error", "error", err)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.LastError = err.Error()
	e.status.LastErrorAt = time.Now()
	errorMetric()
}

func (e *Engine) setPaused(reason string) {
	e.log.Warn("pipeline paused", "reason", reason)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.Paused, e.status.PauseReason = true, reason
}

func (e *Engine) isPaused() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status.Paused
}

func (e *Engine) setActivity(a string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.Activity = a
}

// Status returns a point-in-time view.
func (e *Engine) Status() StatusView {
	e.mu.Lock()
	v := StatusView{Status: e.status}
	v.Tables = map[string]*TableStats{}
	for k, t := range e.status.Tables {
		c := *t
		v.Tables[k] = &c
	}
	inflight := map[string]bool{}
	for k := range e.inflight {
		inflight[k] = true
	}
	e.mu.Unlock()
	v.Document = e.cfg.Document.Name
	if e.schema != nil {
		v.SchemaHash = e.schema.Hash()
	}
	v.Checkpoint, _ = e.store.Checkpoint()
	v.Pending, _ = e.store.PendingCount()
	for _, s := range e.sinks {
		gen, epoch := s.Position()
		sv := SinkView{Name: s.Name(), Generation: gen, Epoch: epoch, HasBase: s.HasBase(v.Checkpoint.Epoch), BaseInFlight: inflight[s.Name()]}
		if at, ok := s.BaseAge(); ok {
			sv.BaseAt = at
		}
		if v.Checkpoint.Generation > gen {
			sv.Lag = v.Checkpoint.Generation - gen
		}
		v.Sinks = append(v.Sinks, sv)
	}
	return v
}

// ---- operator commands -------------------------------------------------------

type command struct {
	kind  string
	ids   []string
	all   bool
	reply chan error
}

// Command kinds accepted by Do.
const (
	CmdPause      = "pause"
	CmdResume     = "resume"
	CmdRebuild    = "rebuild"
	CmdResnapshot = "resnapshot"
	CmdNewEpoch   = "new_epoch"
	CmdCompact    = "compact"
)

// Do queues an operator command for the engine loop and waits for it to be handled.
func (e *Engine) Do(ctx context.Context, kind string, ids []string, all bool) error {
	c := command{kind: kind, ids: ids, all: all, reply: make(chan error, 1)}
	select {
	case e.cmds <- c:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DoNow runs a command synchronously. Only use it from the goroutine that drives
// the engine with Step (never while Run is active).
func (e *Engine) DoNow(ctx context.Context, kind string, ids []string, all bool) error {
	return e.execute(ctx, command{kind: kind, ids: ids, all: all})
}

func (e *Engine) drainCommands(ctx context.Context) {
	for {
		select {
		case c := <-e.cmds:
			e.handle(ctx, c)
		default:
			return
		}
	}
}

func (e *Engine) handle(ctx context.Context, c command) {
	err := e.execute(ctx, c)
	if err != nil {
		e.recordError(fmt.Errorf("%s: %w", c.kind, err))
	} else {
		e.log.Info("operator command done", "command", c.kind)
	}
	c.reply <- err
}

func (e *Engine) execute(ctx context.Context, c command) error {
	switch c.kind {
	case CmdPause:
		e.setPaused("paused by operator")
		return nil
	case CmdResume:
		e.mu.Lock()
		defer e.mu.Unlock()
		if strings.HasPrefix(e.status.PauseReason, "schema drift") {
			return errors.New("the pipeline is paused for schema drift; start a new epoch instead")
		}
		e.status.Paused, e.status.PauseReason = false, ""
		return nil
	case CmdRebuild:
		return e.rebuild(ctx, c.ids, c.all)
	case CmdResnapshot:
		return e.resnapshot(ctx, "requested by operator", false)
	case CmdNewEpoch:
		sch, err := e.discoverSchema(ctx)
		if err != nil {
			return err
		}
		e.schema = sch
		return e.resnapshot(ctx, "new epoch requested by operator", true)
	case CmdCompact:
		for _, s := range e.sinks {
			e.requestBase(s.Name(), "requested by operator")
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", c.kind)
}

func (e *Engine) rebuild(ctx context.Context, ids []string, all bool) error {
	txn := e.store.Begin()
	n := 0
	enqueue := func(enc []byte) error {
		if err := txn.Enqueue(state.ClassRebuild, enc); err != nil {
			return err
		}
		n++
		if n%snapshotBatch == 0 {
			if err := txn.Commit(); err != nil {
				return err
			}
			txn = e.store.Begin()
		}
		return nil
	}
	if all {
		cyc, err := e.sess.BeginCycle(ctx)
		if err != nil {
			txn.Discard()
			return err
		}
		err = cyc.ScanRootKeys(ctx, func(v any) error {
			k, err := routing.TupleKey([]any{v})
			if err != nil {
				return err
			}
			return enqueue([]byte(k))
		})
		cyc.Rollback()
		if err != nil {
			txn.Discard()
			return err
		}
	}
	for _, id := range ids {
		v, err := ParseRootID(e.schema.RootKey.Type, id)
		if err != nil {
			txn.Discard()
			return err
		}
		k, err := routing.TupleKey([]any{v})
		if err != nil {
			txn.Discard()
			return err
		}
		if err := enqueue([]byte(k)); err != nil {
			txn.Discard()
			return err
		}
	}
	return txn.Commit()
}

// ParseRootID parses a root id typed on the command line or in the dashboard.
func ParseRootID(t schema.Type, s string) (any, error) {
	s = strings.TrimSpace(s)
	switch t.Kind {
	case schema.UInt8, schema.Int16, schema.Int32, schema.Int64:
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("root id %q is not an integer", s)
		}
		return v, nil
	case schema.String:
		return s, nil
	case schema.UUID:
		if len(s) != 36 {
			return nil, fmt.Errorf("root id %q is not a GUID", s)
		}
		return doc.UUID(strings.ToUpper(s)), nil
	case schema.Decimal:
		return doc.NormalizeDecimal(s, int(t.Scale))
	case schema.Date:
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			return nil, fmt.Errorf("root id %q is not a date (YYYY-MM-DD)", s)
		}
		return doc.DateOf(d), nil
	}
	return nil, fmt.Errorf("root ids of type %s cannot be parsed", t.Kind)
}

// Inspection compares a root's last emitted document with a fresh assembly.
type Inspection struct {
	Root       string   `json:"root"`
	Exists     bool     `json:"exists"`
	StoredGen  uint64   `json:"stored_generation,omitempty"`
	Stored     string   `json:"stored,omitempty"`
	Live       string   `json:"live,omitempty"`
	Error      string   `json:"error,omitempty"`
	LastError  string   `json:"last_error,omitempty"`
	Pending    bool     `json:"pending"`
	DiffFields []string `json:"diff_fields"`
}

// Inspect assembles one root on a separate connection and compares it with state.
func (e *Engine) Inspect(ctx context.Context, id string) (*Inspection, error) {
	v, err := ParseRootID(e.schema.RootKey.Type, id)
	if err != nil {
		return nil, err
	}
	k, err := routing.TupleKey([]any{v})
	if err != nil {
		return nil, err
	}
	ins := &Inspection{Root: doc.MustJSON(v), DiffFields: []string{}}
	gen, enc, found, err := e.store.Doc([]byte(k))
	if err != nil {
		return nil, err
	}
	var stored doc.Object
	if found {
		ins.StoredGen = gen
		if stored, err = doc.DecodeObject(enc); err != nil {
			return nil, err
		}
		ins.Stored = doc.MustJSON(stored)
	}
	if msg, ok, err := e.store.RootError([]byte(k)); err == nil && ok {
		ins.LastError = msg
	}
	txn := e.store.Begin()
	ins.Pending, _ = txn.IsPending([]byte(k))
	txn.Discard()

	live, exists, assembleErr, err := e.assembleOne(ctx, v)
	if err != nil {
		return nil, err
	}
	ins.Exists = exists
	ins.Error = assembleErr
	if live != nil {
		ins.Live = doc.MustJSON(live)
	}
	keys := map[string]bool{}
	for f := range stored {
		keys[f] = true
	}
	for f := range live {
		keys[f] = true
	}
	for f := range keys {
		if !bytes.Equal(doc.MustEncode(stored[f]), doc.MustEncode(live[f])) {
			ins.DiffFields = append(ins.DiffFields, f)
		}
	}
	sort.Strings(ins.DiffFields)
	return ins, nil
}

func (e *Engine) assembleOne(ctx context.Context, v any) (doc.Object, bool, string, error) {
	sess, err := sqlserver.NewSession(ctx, e.db, e.cat, e.plan)
	if err != nil {
		return nil, false, "", err
	}
	defer sess.Close()
	cyc, err := sess.BeginCycle(ctx)
	if err != nil {
		return nil, false, "", err
	}
	defer cyc.Rollback()
	canonical, err := cyc.LoadRoots(ctx, []any{v})
	if err != nil {
		return nil, false, "", err
	}
	missing, err := cyc.MissingRoots(ctx)
	if err != nil {
		return nil, false, "", err
	}
	if len(missing) > 0 || len(canonical) == 0 {
		return nil, false, "", nil
	}
	var inputs []assemble.Input
	for _, f := range e.cfg.Document.Fragments {
		fr, err := cyc.RunFragment(ctx, f)
		if err != nil {
			return nil, true, "", err
		}
		inputs = append(inputs, assemble.Input{Fragment: f, Columns: fr.Columns, Rows: fr.Rows})
	}
	r, err := assemble.NewRoot(canonical[0])
	if err != nil {
		return nil, true, "", err
	}
	out, err := assemble.Assemble(e.schema, []assemble.Root{r}, inputs)
	if err != nil {
		return nil, true, err.Error(), nil
	}
	if msg, bad := out.Errors[r.Key]; bad {
		return nil, true, msg, nil
	}
	return out.Docs[r.Key], true, "", nil
}
