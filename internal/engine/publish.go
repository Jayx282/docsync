package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
	"github.com/Jayx282/docsync/internal/sink"
	"github.com/Jayx282/docsync/internal/state"
)

const maxChangesPerWrite = 50_000

// publishAll writes committed generations to every sink that is due. It returns the
// first error after attempting every sink.
func (e *Engine) publishAll(ctx context.Context) error {
	ckpt, err := e.store.Checkpoint()
	if err != nil {
		e.recordError(err)
		return err
	}
	now := time.Now()
	var first error
	for _, s := range e.sinks {
		if err := e.publish(ctx, s, ckpt, now); err != nil {
			err = fmt.Errorf("sink %s: %w", s.Name(), err)
			e.recordError(err)
			if first == nil {
				first = err
			}
		}
	}
	return first
}

func (e *Engine) publish(ctx context.Context, s sink.Sink, ckpt state.Checkpoint, now time.Time) error {
	pos, epoch := s.Position()
	if epoch != ckpt.Epoch || !s.HasBase(ckpt.Epoch) {
		return nil // waiting for this epoch's base snapshot
	}
	if pos >= ckpt.Generation || !s.Due(now) {
		return nil
	}
	oldest, err := e.store.OldestOutbox()
	if err != nil {
		return err
	}
	if oldest == 0 || oldest > pos+1 {
		// The outbox no longer holds what this sink needs; a new base will catch it up.
		e.requestBase(s.Name(), fmt.Sprintf("outbox starts at generation %d but the sink is at %d", oldest, pos))
		return nil
	}
	for pos < ckpt.Generation {
		var gens []sink.Generation
		changes := 0
		var last uint64
		err := e.store.ReadOutbox(pos, ckpt.Generation, func(h state.OutboxHeader, entries []state.OutboxEntry) error {
			if changes >= maxChangesPerWrite {
				return errStop
			}
			g := sink.Generation{Gen: h.Generation, Epoch: h.Epoch, CTVersion: h.CTVersion, CreatedAt: h.CreatedAt}
			for _, en := range entries {
				root, err := e.decodeRoot(en.Root)
				if err != nil {
					return err
				}
				c := sink.Change{Root: root, Delete: en.Delete}
				if !en.Delete {
					if c.Doc, err = doc.DecodeObject(en.Doc); err != nil {
						return err
					}
				}
				g.Changes = append(g.Changes, c)
			}
			changes += len(entries)
			gens = append(gens, g)
			last = h.Generation
			return nil
		})
		if err != nil && err != errStop {
			return err
		}
		if len(gens) == 0 {
			return nil
		}
		if err := e.fault("before-sink-write"); err != nil {
			return err
		}
		if err := s.Write(ctx, e.schema, gens); err != nil {
			return err
		}
		if err := e.store.SetSinkPosition(s.Name(), last); err != nil {
			return err
		}
		if err := e.fault("after-sink-write"); err != nil {
			return err
		}
		pos = last
	}
	return nil
}

type stopErr struct{}

func (stopErr) Error() string { return "stop" }

var errStop error = stopErr{}

// requestBase asks for a base snapshot for one sink at the next opportunity.
func (e *Engine) requestBase(name, why string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baseRequests == nil {
		e.baseRequests = map[string]string{}
	}
	if _, ok := e.baseRequests[name]; !ok {
		e.log.Warn("base snapshot requested", "sink", name, "reason", why)
	}
	e.baseRequests[name] = why
}

// maybeBase starts background base snapshots where needed:
//   - a sink has no base for the current epoch (after the epoch's snapshot work drains),
//   - a resnapshot finished draining (BaseNeeded),
//   - an explicit request (compact, or a sink fell behind the outbox),
//   - or the sink's base is older than base_every.
func (e *Engine) maybeBase(ctx context.Context) {
	ckpt, err := e.store.Checkpoint()
	if err != nil {
		e.recordError(err)
		return
	}
	if ckpt.Snapshotting {
		return
	}
	snapshotPending, err := e.hasLivePending(state.ClassSnapshot)
	if err != nil {
		e.recordError(err)
		return
	}
	if snapshotPending {
		return
	}
	for i, s := range e.sinks {
		name := s.Name()
		e.mu.Lock()
		_, busy := e.inflight[name]
		_, requested := e.baseRequests[name]
		e.mu.Unlock()
		if busy {
			if ckpt.BaseNeeded {
				// The running base predates the request; write another one after it.
				e.requestBase(name, "resnapshot finished while a base was being written")
			}
			continue
		}
		due := false
		switch {
		case !s.HasBase(ckpt.Epoch), ckpt.BaseNeeded, requested:
			due = true
		default:
			if at, ok := s.BaseAge(); ok {
				pos, _ := s.Position()
				due = time.Since(at) >= e.cfg.Sinks[i].BaseEvery.Duration && pos == ckpt.Generation
			}
		}
		if due {
			e.startBase(s, ckpt)
		}
	}
	if ckpt.BaseNeeded {
		// Every sink now has a base started or requested; a failed attempt re-requests itself.
		txn := e.store.Begin()
		ckpt.BaseNeeded = false
		if err := txn.SetCheckpoint(ckpt); err != nil {
			txn.Discard()
			e.recordError(err)
			return
		}
		if err := txn.Commit(); err != nil {
			e.recordError(err)
		}
	}
}

// startBase writes a base snapshot in the background from a Pebble snapshot taken now,
// between cycles, so it matches generation ckpt.Generation exactly.
func (e *Engine) startBase(s sink.Sink, ckpt state.Checkpoint) {
	name := s.Name()
	snap := e.store.DB().NewSnapshot()
	gen, epoch := ckpt.Generation, ckpt.Epoch
	sch := e.schema
	e.mu.Lock()
	e.inflight[name] = gen
	delete(e.baseRequests, name)
	e.mu.Unlock()
	e.baseWG.Add(1)
	go func() {
		defer e.baseWG.Done()
		defer snap.Close()
		start := time.Now()
		err := e.writeBase(s, sch, snap, epoch, gen)
		e.mu.Lock()
		delete(e.inflight, name)
		e.mu.Unlock()
		if err != nil {
			e.recordError(fmt.Errorf("base snapshot for sink %s: %w", name, err))
			e.requestBase(name, "previous attempt failed")
			return
		}
		e.log.Info("base snapshot written", "sink", name, "generation", gen, "epoch", epoch, "took", time.Since(start).Round(time.Millisecond))
	}()
}

func (e *Engine) writeBase(s sink.Sink, sch *schema.Schema, snap *pebble.Snapshot, epoch, gen uint64) error {
	cur, err := state.NewDocCursor(snap)
	if err != nil {
		return err
	}
	defer cur.Close()
	rootType := []schema.Type{sch.RootKey.Type}
	next := func() (sink.BaseDoc, bool, error) {
		if err := e.baseCtx.Err(); err != nil {
			return sink.BaseDoc{}, false, err
		}
		rootEnc, g, enc, ok, err := cur.Next()
		if err != nil || !ok {
			return sink.BaseDoc{}, false, err
		}
		vals, err := decodeTuple(rootEnc, rootType)
		if err != nil {
			return sink.BaseDoc{}, false, err
		}
		d, err := doc.DecodeObject(enc)
		if err != nil {
			return sink.BaseDoc{}, false, err
		}
		return sink.BaseDoc{Root: vals[0], Gen: g, Doc: d}, true, nil
	}
	return s.WriteBase(e.baseCtx, sch, epoch, gen, next)
}

// trimOutbox drops generations every current-epoch sink has written, once they are
// older than outbox_retention. While no sink is at the current epoch (a new epoch is
// still draining), nothing in the outbox can be used, so it is trimmed regardless of age.
func (e *Engine) trimOutbox(now time.Time) error {
	ckpt, err := e.store.Checkpoint()
	if err != nil {
		return err
	}
	through := ckpt.Generation
	anyCurrent := false
	e.mu.Lock()
	inflight := make(map[string]uint64, len(e.inflight))
	for k, v := range e.inflight {
		inflight[k] = v
	}
	e.mu.Unlock()
	for _, s := range e.sinks {
		if g, busy := inflight[s.Name()]; busy {
			if g < through {
				through = g
			}
			continue
		}
		pos, epoch := s.Position()
		if epoch == ckpt.Epoch && s.HasBase(ckpt.Epoch) {
			anyCurrent = true
			if pos < through {
				through = pos
			}
		}
	}
	cutoff := now.Add(-e.cfg.State.OutboxRetention.Duration)
	g, err := e.store.OldestOutbox()
	if err != nil || g == 0 {
		return err
	}
	var last uint64
	for ; g <= through; g++ {
		created, err := e.store.OutboxCreatedAt(g)
		if err != nil {
			return err
		}
		if created.IsZero() || (anyCurrent && created.After(cutoff)) {
			break
		}
		last = g
	}
	if last == 0 {
		return nil
	}
	return e.store.TrimOutbox(last)
}
