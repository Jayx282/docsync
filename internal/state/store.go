// Package state is docsync's durable local state, kept in Pebble.
//
// Key layout:
//
//	c/ckpt                      checkpoint (JSON)
//	m/schema                    current document schema (JSON)
//	m/seq, m/pending            pending-set sequence counter and size
//	d/<root>                    last emitted document: 8-byte generation + encoded doc
//	k/<table>\x00<pk>           key map: old routing values of a source row
//	p/<class><seq>              pending-set queue entry -> root
//	q/<root>                    pending-set index: root -> class
//	o/<gen>/h, o/<gen>/d/<root> outbox header and entries
//	s/<sink>                    last generation written by a sink
//	e/<root>                    last assembly error for a root
package state

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/Jayx282/docsync/internal/keyenc"
)

var (
	keyCheckpoint   = []byte("c/ckpt")
	keySchema       = []byte("m/schema")
	keySeq          = []byte("m/seq")
	keyPendingCount = []byte("m/pending")
	prefixDoc       = []byte("d/")
	prefixKeyMap    = []byte("k/")
	prefixPending   = []byte("p/")
	prefixPendIdx   = []byte("q/")
	prefixOutbox    = []byte("o/")
	prefixSink      = []byte("s/")
	prefixError     = []byte("e/")
)

// Pending-set priority classes, highest priority first.
const (
	ClassOverflow byte = 0 // changed roots that did not fit in their cycle's budget
	ClassFanout   byte = 1 // roots spilled by a table's hot_fanout_limit
	ClassRebuild  byte = 2 // manual rebuild requests
	ClassSnapshot byte = 3 // full load and resnapshot
	ClassSweep    byte = 4 // anti-entropy sweeper
	NumClasses         = 5
	FirstLowClass      = ClassRebuild
)

// ClassName names a pending class.
func ClassName(c byte) string {
	return [...]string{"overflow", "fanout", "rebuild", "snapshot", "sweep"}[c]
}

// Checkpoint is the engine's durable progress marker.
type Checkpoint struct {
	Initialized bool   `json:"initialized"`
	CTVersion   int64  `json:"ct_version"`
	Generation  uint64 `json:"generation"`
	ForkGUID    string `json:"fork_guid,omitempty"`
	ConfigHash  string `json:"config_hash,omitempty"`
	SchemaHash  string `json:"schema_hash,omitempty"`
	Epoch       uint64 `json:"epoch"`
	// Snapshotting is set while a full load or resnapshot is being scanned.
	Snapshotting bool `json:"snapshotting,omitempty"`
	// BaseNeeded asks sinks for a base snapshot once the pending snapshot work drains.
	BaseNeeded bool `json:"base_needed,omitempty"`
	// TableBegin records each table's change-tracking begin version. A different value
	// later means tracking was switched off and on (changes in between were not recorded).
	TableBegin map[string]int64 `json:"table_begin,omitempty"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// Store wraps the Pebble database.
type Store struct {
	db  *pebble.DB
	seq atomic.Uint64
}

// Open opens (or creates) the store in dir.
func Open(dir string) (*Store, error) {
	db, err := pebble.Open(dir, &pebble.Options{Logger: quietLogger{}})
	if err != nil {
		return nil, fmt.Errorf("open state store %s: %w", dir, err)
	}
	s := &Store{db: db}
	if v, err := s.getUint(keySeq); err != nil {
		db.Close()
		return nil, err
	} else {
		s.seq.Store(v)
	}
	return s, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the Pebble handle for snapshots.
func (s *Store) DB() *pebble.DB { return s.db }

func (s *Store) get(key []byte) ([]byte, bool, error) {
	v, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := append([]byte(nil), v...)
	closer.Close()
	return out, true, nil
}

func (s *Store) getUint(key []byte) (uint64, error) {
	v, ok, err := s.get(key)
	if err != nil || !ok {
		return 0, err
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("state: corrupt counter %s", key)
	}
	return binary.BigEndian.Uint64(v), nil
}

// Checkpoint returns the stored checkpoint (zero value if none).
func (s *Store) Checkpoint() (Checkpoint, error) {
	var c Checkpoint
	v, ok, err := s.get(keyCheckpoint)
	if err != nil || !ok {
		return c, err
	}
	return c, json.Unmarshal(v, &c)
}

// Schema returns the stored schema JSON, if any.
func (s *Store) Schema() ([]byte, error) {
	v, _, err := s.get(keySchema)
	return v, err
}

// PendingCount returns the number of roots waiting in the pending set.
func (s *Store) PendingCount() (uint64, error) { return s.getUint(keyPendingCount) }

// Doc returns the last emitted document for a root.
func (s *Store) Doc(root []byte) (gen uint64, enc []byte, found bool, err error) {
	v, ok, err := s.get(docKey(root))
	if err != nil || !ok {
		return 0, nil, false, err
	}
	if len(v) < 8 {
		return 0, nil, false, fmt.Errorf("state: corrupt document record")
	}
	return binary.BigEndian.Uint64(v[:8]), v[8:], true, nil
}

// KeyMap returns the stored routing values for a source row.
func (s *Store) KeyMap(table string, pk []byte) ([]byte, bool, error) {
	return s.get(keyMapKey(table, pk))
}

// RootError returns the last assembly error recorded for a root.
func (s *Store) RootError(root []byte) (string, bool, error) {
	v, ok, err := s.get(append(append([]byte(nil), prefixError...), root...))
	return string(v), ok, err
}

// SinkPosition returns the last generation a sink reported as written.
func (s *Store) SinkPosition(name string) (uint64, error) {
	return s.getUint(append(append([]byte(nil), prefixSink...), name...))
}

// SetSinkPosition records a sink's position.
func (s *Store) SetSinkPosition(name string, gen uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], gen)
	return s.db.Set(append(append([]byte(nil), prefixSink...), name...), b[:], pebble.Sync)
}

// PendingEntry is one root taken from the pending set.
type PendingEntry struct {
	Class byte
	Root  []byte
	key   []byte
	stale bool
}

// TakePending reads up to limit live pending roots of one class, oldest first.
// Nothing is removed until the caller commits a Txn that calls Done.
func (s *Store) TakePending(class byte, limit int, skip func(root []byte) bool) ([]PendingEntry, error) {
	if limit <= 0 {
		return nil, nil
	}
	lo := append(append([]byte(nil), prefixPending...), class)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: keyenc.PrefixEnd(lo)})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var out []PendingEntry
	live := 0
	for valid := it.First(); valid && live < limit; valid = it.Next() {
		root := append([]byte(nil), it.Value()...)
		e := PendingEntry{Class: class, Root: root, key: append([]byte(nil), it.Key()...)}
		cls, ok, err := s.get(pendIdxKey(root))
		if err != nil {
			return nil, err
		}
		if !ok || len(cls) != 1 || cls[0] != class || (skip != nil && skip(root)) {
			e.stale = !ok || len(cls) != 1 || cls[0] != class
			if e.stale {
				out = append(out, e) // caller deletes stale queue entries
			}
			continue
		}
		out = append(out, e)
		live++
	}
	return out, it.Error()
}

// Live reports whether the entry still represents a pending root.
func (e PendingEntry) Live() bool { return !e.stale }

// IterateDocs calls fn for every stored document in root-key order, reading from snap.
func IterateDocs(snap *pebble.Snapshot, fn func(root []byte, gen uint64, enc []byte) error) error {
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: prefixDoc, UpperBound: keyenc.PrefixEnd(prefixDoc)})
	if err != nil {
		return err
	}
	defer it.Close()
	for valid := it.First(); valid; valid = it.Next() {
		v := it.Value()
		if len(v) < 8 {
			return fmt.Errorf("state: corrupt document record")
		}
		if err := fn(it.Key()[len(prefixDoc):], binary.BigEndian.Uint64(v[:8]), v[8:]); err != nil {
			return err
		}
	}
	return it.Error()
}

// DocCursor walks stored documents in root-key order on a snapshot.
type DocCursor struct {
	it      *pebble.Iterator
	started bool
}

// NewDocCursor opens a cursor over the documents in snap.
func NewDocCursor(snap *pebble.Snapshot) (*DocCursor, error) { return NewDocCursorAfter(snap, nil) }

// NewDocCursorAfter opens a cursor over documents whose root sorts after `after`
// (all documents when after is nil).
func NewDocCursorAfter(snap *pebble.Snapshot, after []byte) (*DocCursor, error) {
	lo := prefixDoc
	if after != nil {
		lo = append(docKey(after), 0x00) // smallest key greater than docKey(after)
	}
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: keyenc.PrefixEnd(prefixDoc)})
	if err != nil {
		return nil, err
	}
	return &DocCursor{it: it}, nil
}

var keySweep = []byte("m/sweep")

// SweepCursor returns the last root the sweeper queued (nil at the start).
func (s *Store) SweepCursor() ([]byte, error) {
	v, _, err := s.get(keySweep)
	return v, err
}

// SetSweepCursor records the last root the sweeper queued.
func (s *Store) SetSweepCursor(root []byte) error {
	if root == nil {
		return s.db.Delete(keySweep, pebble.NoSync)
	}
	return s.db.Set(keySweep, root, pebble.NoSync)
}

// Next returns the next document; ok is false at the end.
func (c *DocCursor) Next() (root []byte, gen uint64, enc []byte, ok bool, err error) {
	var valid bool
	if !c.started {
		valid, c.started = c.it.First(), true
	} else {
		valid = c.it.Next()
	}
	if !valid {
		return nil, 0, nil, false, c.it.Error()
	}
	v := c.it.Value()
	if len(v) < 8 {
		return nil, 0, nil, false, fmt.Errorf("state: corrupt document record")
	}
	root = append([]byte(nil), c.it.Key()[len(prefixDoc):]...)
	return root, binary.BigEndian.Uint64(v[:8]), append([]byte(nil), v[8:]...), true, nil
}

// Close releases the cursor.
func (c *DocCursor) Close() error { return c.it.Close() }

// ForEachDocRoot calls fn for every stored document root (used to find vanished roots on resnapshot).
func (s *Store) ForEachDocRoot(fn func(root []byte) error) error {
	snap := s.db.NewSnapshot()
	defer snap.Close()
	return IterateDocs(snap, func(root []byte, _ uint64, _ []byte) error {
		return fn(append([]byte(nil), root...))
	})
}

// OutboxHeader describes one committed generation.
type OutboxHeader struct {
	Generation uint64    `json:"generation"`
	Epoch      uint64    `json:"epoch"`
	CTVersion  int64     `json:"ct_version"`
	Upserts    int       `json:"upserts"`
	Deletes    int       `json:"deletes"`
	CreatedAt  time.Time `json:"created_at"`
}

// OutboxEntry is one document change inside a generation.
type OutboxEntry struct {
	Root   []byte
	Delete bool
	Doc    []byte // encoded document; nil for deletes
}

// ReadOutbox calls fn for each generation in (after, through], in order.
func (s *Store) ReadOutbox(after, through uint64, fn func(OutboxHeader, []OutboxEntry) error) error {
	if through <= after {
		return nil
	}
	lo := outboxGenPrefix(after + 1)
	hi := outboxGenPrefix(through + 1)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return err
	}
	defer it.Close()
	var hdr *OutboxHeader
	var entries []OutboxEntry
	flush := func() error {
		if hdr == nil {
			return nil
		}
		err := fn(*hdr, entries)
		hdr, entries = nil, nil
		return err
	}
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		rest := k[len(prefixOutbox)+8:]
		switch {
		case len(rest) >= 2 && rest[0] == '/' && rest[1] == 'h':
			if err := flush(); err != nil {
				return err
			}
			var h OutboxHeader
			if err := json.Unmarshal(it.Value(), &h); err != nil {
				return err
			}
			hdr = &h
		case len(rest) >= 3 && rest[0] == '/' && rest[1] == 'd' && rest[2] == '/':
			v := it.Value()
			if len(v) < 1 {
				return fmt.Errorf("state: corrupt outbox entry")
			}
			e := OutboxEntry{Root: append([]byte(nil), rest[3:]...), Delete: v[0] == 'D'}
			if !e.Delete {
				e.Doc = append([]byte(nil), v[1:]...)
			}
			entries = append(entries, e)
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	return flush()
}

// OldestOutbox returns the lowest generation still in the outbox, or 0.
func (s *Store) OldestOutbox() (uint64, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefixOutbox, UpperBound: keyenc.PrefixEnd(prefixOutbox)})
	if err != nil {
		return 0, err
	}
	defer it.Close()
	if !it.First() {
		return 0, it.Error()
	}
	return binary.BigEndian.Uint64(it.Key()[len(prefixOutbox) : len(prefixOutbox)+8]), nil
}

// TrimOutbox deletes outbox generations up to and including through.
func (s *Store) TrimOutbox(through uint64) error {
	return s.db.DeleteRange(prefixOutbox, outboxGenPrefix(through+1), pebble.Sync)
}

// OutboxCreatedAt returns when a generation was committed (zero if absent).
func (s *Store) OutboxCreatedAt(gen uint64) (time.Time, error) {
	v, ok, err := s.get(append(outboxGenPrefix(gen), '/', 'h'))
	if err != nil || !ok {
		return time.Time{}, err
	}
	var h OutboxHeader
	if err := json.Unmarshal(v, &h); err != nil {
		return time.Time{}, err
	}
	return h.CreatedAt, nil
}

// Txn batches every write of one cycle so it commits atomically.
type Txn struct {
	s       *Store
	b       *pebble.Batch
	pending int64 // net change to the pending count
	seq     uint64
}

// Begin starts a write transaction.
func (s *Store) Begin() *Txn {
	return &Txn{s: s, b: s.db.NewIndexedBatch(), seq: s.seq.Load()}
}

// Discard abandons the transaction.
func (t *Txn) Discard() { t.b.Close() }

func (t *Txn) get(key []byte) ([]byte, bool, error) {
	v, closer, err := t.b.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	out := append([]byte(nil), v...)
	closer.Close()
	return out, true, nil
}

// PutDoc stores the latest emitted document for root.
func (t *Txn) PutDoc(root []byte, gen uint64, enc []byte) error {
	v := make([]byte, 8, 8+len(enc))
	binary.BigEndian.PutUint64(v, gen)
	return t.b.Set(docKey(root), append(v, enc...), nil)
}

// DeleteDoc removes a root's document.
func (t *Txn) DeleteDoc(root []byte) error { return t.b.Delete(docKey(root), nil) }

// PutKeyMap stores routing values for a source row.
func (t *Txn) PutKeyMap(table string, pk, vals []byte) error {
	return t.b.Set(keyMapKey(table, pk), vals, nil)
}

// DeleteKeyMap removes a source row's routing values.
func (t *Txn) DeleteKeyMap(table string, pk []byte) error {
	return t.b.Delete(keyMapKey(table, pk), nil)
}

// ClearKeyMap removes every entry for a table.
func (t *Txn) ClearKeyMap(table string) error {
	lo := keyMapKey(table, nil)
	return t.b.DeleteRange(lo, keyenc.PrefixEnd(lo), nil)
}

// Enqueue adds root to the pending set unless it is already pending at the same or higher priority.
func (t *Txn) Enqueue(class byte, root []byte) error {
	idx := pendIdxKey(root)
	cur, ok, err := t.get(idx)
	if err != nil {
		return err
	}
	if ok && len(cur) == 1 && cur[0] <= class {
		return nil
	}
	if !ok {
		t.pending++
	}
	t.seq++
	if err := t.b.Set(idx, []byte{class}, nil); err != nil {
		return err
	}
	return t.b.Set(pendingKey(class, t.seq), root, nil)
}

// IsPending reports whether root is in the pending set, including writes in this Txn.
func (t *Txn) IsPending(root []byte) (bool, error) {
	_, ok, err := t.get(pendIdxKey(root))
	return ok, err
}

// Done removes root from the pending set (any class). Safe to call for roots that are not pending.
func (t *Txn) Done(root []byte) error {
	idx := pendIdxKey(root)
	_, ok, err := t.get(idx)
	if err != nil || !ok {
		return err
	}
	t.pending--
	return t.b.Delete(idx, nil)
}

// DropQueueEntry deletes a queue entry returned by TakePending (live or stale).
func (t *Txn) DropQueueEntry(e PendingEntry) error { return t.b.Delete(e.key, nil) }

// ClearPendingClass removes every queue entry of a class. Index entries are
// cleaned lazily: a root whose index still names this class is re-enqueued by the caller.
func (t *Txn) ClearPendingClass(class byte) error {
	lo := append(append([]byte(nil), prefixPending...), class)
	return t.b.DeleteRange(lo, keyenc.PrefixEnd(lo), nil)
}

// PutOutbox writes one generation to the outbox.
func (t *Txn) PutOutbox(h OutboxHeader, entries []OutboxEntry) error {
	hb, err := json.Marshal(h)
	if err != nil {
		return err
	}
	p := outboxGenPrefix(h.Generation)
	if err := t.b.Set(append(append([]byte(nil), p...), '/', 'h'), hb, nil); err != nil {
		return err
	}
	for _, e := range entries {
		k := append(append(append([]byte(nil), p...), '/', 'd', '/'), e.Root...)
		v := []byte{'U'}
		if e.Delete {
			v = []byte{'D'}
		} else {
			v = append(v, e.Doc...)
		}
		if err := t.b.Set(k, v, nil); err != nil {
			return err
		}
	}
	return nil
}

// SetRootError records an assembly error for a root.
func (t *Txn) SetRootError(root []byte, msg string) error {
	return t.b.Set(append(append([]byte(nil), prefixError...), root...), []byte(msg), nil)
}

// ClearRootError removes a root's recorded error.
func (t *Txn) ClearRootError(root []byte) error {
	return t.b.Delete(append(append([]byte(nil), prefixError...), root...), nil)
}

// SetSchema stores the schema JSON.
func (t *Txn) SetSchema(b []byte) error { return t.b.Set(keySchema, b, nil) }

// SetCheckpoint stores the checkpoint.
func (t *Txn) SetCheckpoint(c Checkpoint) error {
	c.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return t.b.Set(keyCheckpoint, b, nil)
}

// Commit writes the batch durably.
func (t *Txn) Commit() error {
	if t.pending != 0 {
		cur, ok, err := t.get(keyPendingCount)
		if err != nil {
			return err
		}
		var n int64
		if ok && len(cur) == 8 {
			n = int64(binary.BigEndian.Uint64(cur))
		}
		n += t.pending
		if n < 0 {
			n = 0
		}
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		if err := t.b.Set(keyPendingCount, b[:], nil); err != nil {
			return err
		}
	}
	if t.seq != t.s.seq.Load() {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], t.seq)
		if err := t.b.Set(keySeq, b[:], nil); err != nil {
			return err
		}
	}
	if err := t.b.Commit(pebble.Sync); err != nil {
		return err
	}
	t.s.seq.Store(t.seq)
	return t.b.Close()
}

func docKey(root []byte) []byte { return append(append([]byte(nil), prefixDoc...), root...) }

func keyMapKey(table string, pk []byte) []byte {
	k := append(append([]byte(nil), prefixKeyMap...), table...)
	k = append(k, 0x00)
	return append(k, pk...)
}

func pendIdxKey(root []byte) []byte { return append(append([]byte(nil), prefixPendIdx...), root...) }

func pendingKey(class byte, seq uint64) []byte {
	k := append(append([]byte(nil), prefixPending...), class)
	return binary.BigEndian.AppendUint64(k, seq)
}

func outboxGenPrefix(gen uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte(nil), prefixOutbox...), gen)
}

type quietLogger struct{}

func (quietLogger) Infof(string, ...interface{})  {}
func (quietLogger) Errorf(string, ...interface{}) {}
func (quietLogger) Fatalf(format string, args ...interface{}) {
	panic(fmt.Sprintf(format, args...))
}

var _ io.Closer = (*Store)(nil)
