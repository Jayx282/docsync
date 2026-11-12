package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Jayx282/docsync/internal/routing"
	"github.com/Jayx282/docsync/internal/state"
)

const snapshotBatch = 10_000

// resnapshot rebuilds trust in local state from scratch. In one short SQL snapshot it
// reads the current change-tracking version, rescans the key map (key columns only)
// and lists every root id. All roots then go into the pending set, so normal cycles
// re-assemble them; documents whose roots vanished are queued too, so they get deleted.
// With newEpoch the document shape may have changed, so consumers restart from a new base.
func (e *Engine) resnapshot(ctx context.Context, reason string, newEpoch bool) error {
	e.log.Info("snapshot starting", "reason", reason, "new_epoch", newEpoch)
	e.setActivity("snapshot: " + reason)
	defer e.setActivity("")

	ckpt, err := e.store.Checkpoint()
	if err != nil {
		return err
	}
	if newEpoch || ckpt.Epoch == 0 {
		ckpt.Epoch++
	}
	ckpt.Snapshotting = true
	ckpt.ConfigHash = e.cfg.ContentHash()
	ckpt.SchemaHash = e.schema.Hash()
	schemaJSON, err := json.Marshal(e.schema)
	if err != nil {
		return err
	}
	txn := e.store.Begin()
	if err := txn.SetSchema(schemaJSON); err != nil {
		txn.Discard()
		return err
	}
	if err := txn.SetCheckpoint(ckpt); err != nil {
		txn.Discard()
		return err
	}
	if err := txn.Commit(); err != nil {
		return err
	}

	cyc, err := e.sess.BeginCycle(ctx)
	if err != nil {
		return wrapSnapshotErr(err)
	}
	sqlDone := false
	defer func() {
		if !sqlDone {
			cyc.Rollback()
		}
	}()
	vers, err := cyc.Versions(ctx)
	if err != nil {
		return err
	}

	// Key map: old routing values exactly as of vers.Current.
	keyMapRows := 0
	for _, tp := range e.plan.Tables {
		if !tp.NeedsKeyMap {
			continue
		}
		table := tp.Table.Key()
		txn := e.store.Begin()
		if err := txn.ClearKeyMap(table); err != nil {
			txn.Discard()
			return err
		}
		n := 0
		cols := append(append([]string(nil), tp.Key...), tp.FromCols()...)
		err := cyc.Scan(ctx, tp.Table, cols, func(vals []any) error {
			pk, err := routing.TupleKey(vals[:len(tp.Key)])
			if err != nil {
				return err
			}
			from, err := routing.TupleKey(vals[len(tp.Key):])
			if err != nil {
				return err
			}
			if err := txn.PutKeyMap(table, []byte(pk), []byte(from)); err != nil {
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
		})
		if err != nil {
			txn.Discard()
			return fmt.Errorf("scan key map of %s: %w", tp.Table, err)
		}
		if err := txn.Commit(); err != nil {
			return err
		}
		keyMapRows += n
	}

	// Every current root goes into the pending set.
	roots := 0
	txn = e.store.Begin()
	enqueue := func(enc []byte) error {
		if err := txn.Enqueue(state.ClassSnapshot, enc); err != nil {
			return err
		}
		roots++
		if roots%snapshotBatch == 0 {
			if err := txn.Commit(); err != nil {
				return err
			}
			txn = e.store.Begin()
		}
		return nil
	}
	err = cyc.ScanRootKeys(ctx, func(v any) error {
		k, err := routing.TupleKey([]any{v})
		if err != nil {
			return err
		}
		return enqueue([]byte(k))
	})
	if err != nil {
		txn.Discard()
		return fmt.Errorf("scan roots: %w", err)
	}
	if err := cyc.Commit(); err != nil {
		txn.Discard()
		return err
	}
	sqlDone = true
	// Roots that have a stored document but no longer exist must be re-checked (and deleted).
	if err := e.store.ForEachDocRoot(enqueue); err != nil {
		txn.Discard()
		return err
	}

	ckpt.CTVersion = vers.Current
	if vers.ForkGUID != "" {
		ckpt.ForkGUID = vers.ForkGUID
	}
	ckpt.TableBegin = nil
	e.recordBeginVersions(&ckpt, vers)
	ckpt.Snapshotting = false
	ckpt.BaseNeeded = true
	ckpt.Initialized = true
	if err := txn.SetCheckpoint(ckpt); err != nil {
		txn.Discard()
		return err
	}
	if err := txn.Commit(); err != nil {
		return err
	}
	e.mu.Lock()
	e.status.Paused, e.status.PauseReason = false, ""
	e.mu.Unlock()
	e.log.Info("snapshot scanned", "ct_version", vers.Current, "epoch", ckpt.Epoch, "roots_queued", roots, "key_map_rows", keyMapRows)
	return nil
}
