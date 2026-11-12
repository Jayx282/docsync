package engine

import (
	"time"

	"github.com/Jayx282/docsync/internal/state"
)

// sweep queues the next slice of stored documents for re-assembly at
// source.sweep_per_minute, cycling through all documents forever. A swept document
// that comes out different reveals a missed change (for example in a table marked
// static, or one missing from routing); it is corrected and counted.
func (e *Engine) sweep(now time.Time) error {
	rate := e.cfg.Source.SweepPerMinute
	if rate <= 0 {
		return nil
	}
	if e.lastSweep.IsZero() {
		e.lastSweep = now
		return nil
	}
	e.sweepCarry += float64(rate) * now.Sub(e.lastSweep).Minutes()
	e.lastSweep = now
	n := int(e.sweepCarry)
	if n < 1 {
		return nil
	}
	e.sweepCarry -= float64(n)

	cursor, err := e.store.SweepCursor()
	if err != nil {
		return err
	}
	snap := e.store.DB().NewSnapshot()
	defer snap.Close()
	cur, err := state.NewDocCursorAfter(snap, cursor)
	if err != nil {
		return err
	}
	defer func() { cur.Close() }()
	txn := e.store.Begin()
	last := cursor
	queued, wrapped := 0, cursor == nil
	for queued < n {
		root, _, _, ok, err := cur.Next()
		if err != nil {
			txn.Discard()
			return err
		}
		if !ok {
			if wrapped {
				break // fewer documents than this slice
			}
			cur.Close()
			if cur, err = state.NewDocCursorAfter(snap, nil); err != nil {
				txn.Discard()
				return err
			}
			wrapped, last = true, nil
			continue
		}
		if err := txn.Enqueue(state.ClassSweep, root); err != nil {
			txn.Discard()
			return err
		}
		last = root
		queued++
	}
	if err := txn.Commit(); err != nil {
		return err
	}
	return e.store.SetSweepCursor(last)
}
