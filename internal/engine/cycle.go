package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Jayx282/docsync/internal/assemble"
	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/keyenc"
	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/source/sqlserver"
	"github.com/Jayx282/docsync/internal/state"
)

var errDrift = errors.New("schema drift; pipeline paused")

// CycleResult summarizes one cycle.
type CycleResult struct {
	Changes int
	Roots   int
	Upserts int
	Deletes int
	Errors  int
	// SweepCorrections counts documents the sweeper found stale and fixed.
	SweepCorrections int
	Generation       uint64 // new generation, or 0 when nothing changed
	MoreWork         bool   // pending work remains; run the next cycle without waiting
	Duration         time.Duration
}

// RunCycle runs one capture cycle, retrying transient SQL errors.
func (e *Engine) RunCycle(ctx context.Context) (CycleResult, error) {
	start := time.Now()
	var res CycleResult
	var err error
	for attempt := 0; ; attempt++ {
		res, err = e.cycleOnce(ctx)
		if err == nil || !sqlserver.IsRetryable(err) || sqlserver.IsConnectionError(err) || attempt >= e.cfg.Source.MaxCycleRetries {
			break
		}
		e.log.Warn("retrying cycle after transient error", "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 200 * time.Millisecond):
		}
	}
	res.Duration = time.Since(start)
	e.recordCycle(res, err)
	if err != nil && sqlserver.IsConnectionError(err) {
		e.reconnect(ctx)
	}
	return res, err
}

// rootSet is an insertion-ordered set of root ids.
type rootSet struct {
	seen map[string]bool
	vals []any
	encs [][]byte
}

func newRootSet() *rootSet { return &rootSet{seen: map[string]bool{}} }

func (s *rootSet) add(v any) error {
	if v == nil {
		return nil
	}
	k, err := routing.TupleKey([]any{v})
	if err != nil {
		return err
	}
	if s.seen[k] {
		return nil
	}
	s.seen[k] = true
	s.vals = append(s.vals, v)
	s.encs = append(s.encs, []byte(k))
	return nil
}

func (s *rootSet) has(enc []byte) bool { return s.seen[string(enc)] }
func (s *rootSet) len() int            { return len(s.vals) }

func (e *Engine) cycleOnce(ctx context.Context) (CycleResult, error) {
	var res CycleResult
	if e.sess == nil {
		e.reconnect(ctx)
		if e.sess == nil {
			return res, fmt.Errorf("no database session")
		}
	}
	ckpt, err := e.store.Checkpoint()
	if err != nil {
		return res, err
	}
	cyc, err := e.sess.BeginCycle(ctx)
	if err != nil {
		return res, wrapSnapshotErr(err)
	}
	sqlDone := false
	defer func() {
		if !sqlDone {
			cyc.Rollback()
		}
	}()

	vers, err := cyc.Versions(ctx)
	if err != nil {
		return res, err
	}
	if err := e.trackingEnabled(vers); err != nil {
		return res, err // wait (with backoff) until change tracking is turned back on
	}
	if reason := e.resnapshotReason(ckpt, vers); reason != "" {
		cyc.Rollback()
		sqlDone = true
		return res, e.resnapshot(ctx, reason, false)
	}

	txn := e.store.Begin()
	defer func() {
		if txn != nil {
			txn.Discard()
		}
	}()

	hot := newRootSet()
	spill := map[byte]*rootSet{}
	spillAdd := func(class byte, v any) error {
		if spill[class] == nil {
			spill[class] = newRootSet()
		}
		return spill[class].add(v)
	}
	from, to := ckpt.CTVersion, vers.Current
	if to > from {
		for _, tp := range e.plan.Tables {
			n, maxVer, err := cyc.Capture(ctx, tp, from)
			if err != nil {
				return res, err
			}
			if maxVer > to {
				return res, fmt.Errorf("change tracking returned version %d for %s, beyond the cycle's snapshot version %d", maxVer, tp.Table, to)
			}
			if n == 0 {
				continue
			}
			res.Changes += int(n)
			rows, err := cyc.ChangedRows(ctx, tp)
			if err != nil {
				return res, err
			}
			roots, err := e.route(ctx, cyc, txn, tp, rows)
			if err != nil {
				return res, fmt.Errorf("route changes of %s: %w", tp.Table, err)
			}
			e.noteTable(tp.Table.String(), int(n), len(roots))
			for i, r := range roots {
				if tp.HotFanoutLimit > 0 && i >= tp.HotFanoutLimit {
					if err := spillAdd(state.ClassFanout, r); err != nil {
						return res, err
					}
					continue
				}
				if err := hot.add(r); err != nil {
					return res, err
				}
			}
		}
	}

	sel, taken, err := e.selectRoots(hot, spillAdd)
	if err != nil {
		return res, err
	}
	// Roots that only the sweeper asked for: if they change, a change was missed.
	swept := map[string]bool{}
	for _, pe := range taken {
		if pe.Live() && pe.Class == state.ClassSweep && !hot.has(pe.Root) {
			swept[string(pe.Root)] = true
		}
	}

	var canonical, missing []any
	var inputs []assemble.Input
	if sel.len() > 0 {
		if canonical, err = cyc.LoadRoots(ctx, sel.vals); err != nil {
			return res, err
		}
		if missing, err = cyc.MissingRoots(ctx); err != nil {
			return res, err
		}
		for _, f := range e.cfg.Document.Fragments {
			fr, err := cyc.RunFragment(ctx, f)
			if err != nil {
				return res, err
			}
			inputs = append(inputs, assemble.Input{Fragment: f, Columns: fr.Columns, Rows: fr.Rows})
		}
	}
	if err := cyc.Commit(); err != nil {
		return res, err
	}
	sqlDone = true
	if err := e.fault("after-sql-commit"); err != nil {
		return res, err
	}

	missingSet := map[string]bool{}
	for _, m := range missing {
		k, err := routing.TupleKey([]any{m})
		if err != nil {
			return res, err
		}
		missingSet[k] = true
	}
	var existing []assemble.Root
	for _, v := range canonical {
		r, err := assemble.NewRoot(v)
		if err != nil {
			return res, err
		}
		if !missingSet[r.Key] {
			existing = append(existing, r)
		}
	}
	out, err := assemble.Assemble(e.schema, existing, inputs)
	if err != nil {
		if errors.Is(err, assemble.ErrSchemaDrift) {
			e.setPaused(err.Error() + " — start a new epoch to rebuild all documents with the new shape")
			return res, errDrift
		}
		return res, err
	}

	type change struct {
		root []byte
		enc  []byte
		del  bool
	}
	var changes []change
	for _, r := range existing {
		rootEnc := []byte(r.Key)
		if msg, bad := out.Errors[r.Key]; bad {
			if err := txn.SetRootError(rootEnc, msg); err != nil {
				return res, err
			}
			res.Errors++
			continue
		}
		enc, err := doc.Encode(out.Docs[r.Key])
		if err != nil {
			return res, err
		}
		if err := txn.ClearRootError(rootEnc); err != nil {
			return res, err
		}
		_, old, found, err := e.store.Doc(rootEnc)
		if err != nil {
			return res, err
		}
		if found && bytes.Equal(old, enc) {
			continue
		}
		if swept[r.Key] {
			res.SweepCorrections++
			if res.SweepCorrections <= 3 {
				e.log.Warn("sweeper corrected a stale document; a source change was not captured (is every table in routing?)", "root", doc.MustJSON(r.Value))
			}
		}
		changes = append(changes, change{root: rootEnc, enc: enc})
	}
	for _, m := range missing {
		k, _ := routing.TupleKey([]any{m})
		rootEnc := []byte(k)
		if err := txn.ClearRootError(rootEnc); err != nil {
			return res, err
		}
		_, _, found, err := e.store.Doc(rootEnc)
		if err != nil {
			return res, err
		}
		if found {
			changes = append(changes, change{root: rootEnc, del: true})
		}
	}

	gen := ckpt.Generation
	if len(changes) > 0 {
		gen++
		entries := make([]state.OutboxEntry, 0, len(changes))
		for _, c := range changes {
			if c.del {
				if err := txn.DeleteDoc(c.root); err != nil {
					return res, err
				}
				res.Deletes++
				entries = append(entries, state.OutboxEntry{Root: c.root, Delete: true})
				continue
			}
			if err := txn.PutDoc(c.root, gen, c.enc); err != nil {
				return res, err
			}
			res.Upserts++
			entries = append(entries, state.OutboxEntry{Root: c.root, Doc: c.enc})
		}
		hdr := state.OutboxHeader{Generation: gen, Epoch: ckpt.Epoch, CTVersion: to, Upserts: res.Upserts, Deletes: res.Deletes, CreatedAt: time.Now().UTC()}
		if err := txn.PutOutbox(hdr, entries); err != nil {
			return res, err
		}
		res.Generation = gen
	}

	// Pending-set bookkeeping: everything assembled this cycle is done; overflow is queued.
	for _, enc := range sel.encs {
		if err := txn.Done(enc); err != nil {
			return res, err
		}
	}
	for _, v := range canonical {
		k, _ := routing.TupleKey([]any{v})
		if err := txn.Done([]byte(k)); err != nil {
			return res, err
		}
	}
	for _, pe := range taken {
		if err := txn.DropQueueEntry(pe); err != nil {
			return res, err
		}
	}
	queued := 0
	for _, class := range []byte{state.ClassOverflow, state.ClassFanout} {
		set := spill[class]
		if set == nil {
			continue
		}
		for _, enc := range set.encs {
			if sel.has(enc) {
				continue
			}
			if err := txn.Enqueue(class, enc); err != nil {
				return res, err
			}
			queued++
		}
	}

	if to > ckpt.CTVersion {
		ckpt.CTVersion = to
	}
	if len(ckpt.TableBegin) < len(e.plan.Tables) {
		e.recordBeginVersions(&ckpt, vers) // checkpoints written before this field existed
	}
	ckpt.Generation = gen
	if vers.ForkGUID != "" {
		ckpt.ForkGUID = vers.ForkGUID
	}
	if err := txn.SetCheckpoint(ckpt); err != nil {
		return res, err
	}
	if err := e.fault("before-state-commit"); err != nil {
		return res, err
	}
	if err := txn.Commit(); err != nil {
		return res, err
	}
	txn = nil
	if err := e.fault("after-state-commit"); err != nil {
		return res, err
	}

	res.Roots = len(existing) + len(missing)
	pending, err := e.store.PendingCount()
	if err != nil {
		return res, err
	}
	res.MoreWork = pending > 0 || queued > 0
	return res, nil
}

// route turns one table's changed rows into the root ids they affect, keeping the
// key map of old routing values up to date in txn.
func (e *Engine) route(ctx context.Context, cyc *sqlserver.Cycle, txn *state.Txn, tp *routing.TablePlan, rows []sqlserver.ChangedRow) ([]any, error) {
	if tp.IsRoot {
		out := make([]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.PK[0])
		}
		return out, nil
	}
	ti := e.cat.MustTable(tp.Table)
	fromCols := tp.FromCols()
	fromTypes := make([]schema.Type, len(fromCols))
	pkIdx := make([]int, len(fromCols))
	for i, c := range fromCols {
		ci, err := ti.Column(c)
		if err != nil {
			return nil, err
		}
		fromTypes[i] = ci.Logical
		pkIdx[i] = -1
		for j, k := range tp.Key {
			if lower(k) == lower(c) {
				pkIdx[i] = j
			}
		}
	}
	table := tp.Table.Key()
	start := routing.NewTupleSet()
	for _, r := range rows {
		if !tp.NeedsKeyMap {
			// The routing columns are part of the primary key, so CT's key carries them even for deletes.
			vals := make([]any, len(fromCols))
			for i, j := range pkIdx {
				vals[i] = r.PK[j]
			}
			if err := start.Add(vals); err != nil {
				return nil, err
			}
			continue
		}
		pkKey, err := routing.TupleKey(r.PK)
		if err != nil {
			return nil, err
		}
		pkEnc := []byte(pkKey)
		old, ok, err := e.store.KeyMap(table, pkEnc)
		if err != nil {
			return nil, err
		}
		if ok {
			vals, err := decodeTuple(old, fromTypes)
			if err != nil {
				return nil, fmt.Errorf("key map entry for %s: %w", tp.Table, err)
			}
			if err := start.Add(vals); err != nil {
				return nil, err
			}
		}
		if r.Exists {
			if err := start.Add(r.From); err != nil {
				return nil, err
			}
			enc, err := routing.TupleKey(r.From)
			if err != nil {
				return nil, err
			}
			if err := txn.PutKeyMap(table, pkEnc, []byte(enc)); err != nil {
				return nil, err
			}
		} else if err := txn.DeleteKeyMap(table, pkEnc); err != nil {
			return nil, err
		}
	}
	return routing.Propagate(ctx, cyc, e.plan, tp, start.Rows())
}

func decodeTuple(enc []byte, types []schema.Type) ([]any, error) {
	prims, err := keyenc.Decode(enc)
	if err != nil {
		return nil, err
	}
	if len(prims) != len(types) {
		return nil, fmt.Errorf("expected %d values, found %d", len(types), len(prims))
	}
	out := make([]any, len(prims))
	for i, p := range prims {
		v, err := schema.FromKeyPrimitive(types[i], p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// decodeRoot turns an encoded root key back into a typed value.
func (e *Engine) decodeRoot(enc []byte) (any, error) {
	vals, err := decodeTuple(enc, []schema.Type{e.schema.RootKey.Type})
	if err != nil {
		return nil, err
	}
	return vals[0], nil
}

// selectRoots picks the roots to assemble this cycle: changed roots first, then the
// pending set by priority, reserving a share of the budget for low-priority work.
func (e *Engine) selectRoots(hot *rootSet, spillAdd func(byte, any) error) (*rootSet, []state.PendingEntry, error) {
	budget := e.cfg.Source.AssembleBudget
	reserve := 0
	for c := state.FirstLowClass; c < state.NumClasses; c++ {
		live, err := e.hasLivePending(c)
		if err != nil {
			return nil, nil, err
		}
		if live {
			reserve = int(float64(budget) * e.cfg.Source.LowPriorityShare)
			if reserve < 1 {
				reserve = 1
			}
			break
		}
	}
	hotCap := budget - reserve
	if hotCap < 1 {
		hotCap = 1
	}
	sel := newRootSet()
	for i, v := range hot.vals {
		if i < hotCap {
			if err := sel.add(v); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err := spillAdd(state.ClassOverflow, v); err != nil {
			return nil, nil, err
		}
	}
	var taken []state.PendingEntry
	take := func(class byte, limit int) (int, error) {
		if limit <= 0 {
			return 0, nil
		}
		entries, err := e.store.TakePending(class, limit, sel.has)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, pe := range entries {
			taken = append(taken, pe)
			if !pe.Live() {
				continue
			}
			v, err := e.decodeRoot(pe.Root)
			if err != nil {
				return n, err
			}
			if err := sel.add(v); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}
	remaining := budget - sel.len()
	high := remaining - reserve
	for c := byte(0); c < state.FirstLowClass; c++ {
		n, err := take(c, high)
		if err != nil {
			return nil, nil, err
		}
		high -= n
		remaining -= n
	}
	for c := state.FirstLowClass; c < state.NumClasses; c++ {
		n, err := take(c, remaining)
		if err != nil {
			return nil, nil, err
		}
		remaining -= n
	}
	return sel, taken, nil
}

func (e *Engine) hasLivePending(class byte) (bool, error) {
	entries, err := e.store.TakePending(class, 1, nil)
	if err != nil {
		return false, err
	}
	for _, pe := range entries {
		if pe.Live() {
			return true, nil
		}
	}
	return false, nil
}

// trackingEnabled fails while change tracking is off on any tracked table. Once it is
// turned back on, resnapshotReason sees the new begin version and rebuilds.
func (e *Engine) trackingEnabled(v sqlserver.Versions) error {
	for _, tp := range e.plan.Tables {
		tv, ok := v.Tables[tp.Table.Key()]
		if !ok || !tv.MinValid.Valid {
			return fmt.Errorf("change tracking is not enabled on %s; waiting for it to be turned back on (ALTER TABLE %s ENABLE CHANGE_TRACKING)", tp.Table, tp.Table.Quoted())
		}
	}
	return nil
}

// resnapshotReason explains why stored state can no longer be trusted, or returns "".
func (e *Engine) resnapshotReason(ckpt state.Checkpoint, v sqlserver.Versions) string {
	if v.Current < ckpt.CTVersion {
		return fmt.Sprintf("the database's change tracking version went backwards (%d < %d); was it restored?", v.Current, ckpt.CTVersion)
	}
	if ckpt.ForkGUID != "" && v.ForkGUID != "" && ckpt.ForkGUID != v.ForkGUID {
		return "the database was restored (its recovery fork changed)"
	}
	for _, tp := range e.plan.Tables {
		tv := v.Tables[tp.Table.Key()]
		if ckpt.CTVersion < tv.MinValid.Int64 {
			return fmt.Sprintf("change tracking history for %s has expired (minimum valid version %d is newer than the checkpoint %d)", tp.Table, tv.MinValid.Int64, ckpt.CTVersion)
		}
		if tv.BeginVersion.Valid && tv.BeginVersion.Int64 > ckpt.CTVersion {
			return fmt.Sprintf("change tracking on %s restarted after the checkpoint (truncated or re-enabled)", tp.Table)
		}
		if prev, ok := ckpt.TableBegin[tp.Table.Key()]; ok && tv.BeginVersion.Valid && tv.BeginVersion.Int64 != prev {
			return fmt.Sprintf("change tracking on %s was switched off and on (begin version %d → %d); changes made meanwhile were not recorded", tp.Table, prev, tv.BeginVersion.Int64)
		}
	}
	return ""
}

// recordBeginVersions stores each table's begin version in the checkpoint.
func (e *Engine) recordBeginVersions(ckpt *state.Checkpoint, v sqlserver.Versions) {
	if ckpt.TableBegin == nil {
		ckpt.TableBegin = map[string]int64{}
	}
	for _, tp := range e.plan.Tables {
		if tv, ok := v.Tables[tp.Table.Key()]; ok && tv.BeginVersion.Valid {
			ckpt.TableBegin[tp.Table.Key()] = tv.BeginVersion.Int64
		}
	}
}
