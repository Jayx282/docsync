// Package sink writes committed document generations to output destinations.
//
// Both built-in sinks share one on-disk layout (see docs/consumer-protocol.md):
//
//	<dir>/manifest.json                    commit point, replaced atomically
//	<dir>/base/<gen>/part-NNNNN.<ext>      full snapshot at a generation
//	<dir>/segments/<from>-<to>.<ext>       changes for a range of generations
package sink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
)

// Change is one document change.
type Change struct {
	Root   any
	Delete bool
	Doc    doc.Object // nil for deletes
}

// Generation is one committed cycle's changes. Generations are atomic.
type Generation struct {
	Gen       uint64
	Epoch     uint64
	CTVersion int64
	CreatedAt time.Time
	Changes   []Change
}

// BaseDoc is one document in a base snapshot.
type BaseDoc struct {
	Root any
	Gen  uint64
	Doc  doc.Object
}

// Sink is an output destination.
type Sink interface {
	Name() string
	// Position returns the newest generation durably written and its epoch.
	Position() (gen, epoch uint64)
	// HasBase reports whether a base exists for epoch.
	HasBase(epoch uint64) bool
	// BaseAge returns when the newest base was written.
	BaseAge() (time.Time, bool)
	// Due reports whether enough time has passed to write another segment.
	Due(now time.Time) bool
	// Write durably appends generations (ascending, contiguous, all after Position).
	Write(ctx context.Context, s *schema.Schema, gens []Generation) error
	// WriteBase writes a full snapshot at generation gen and makes it the epoch's base.
	WriteBase(ctx context.Context, s *schema.Schema, epoch, gen uint64, next func() (BaseDoc, bool, error)) error
	// Retain removes segments and bases that are no longer needed.
	Retain(now time.Time) error
	Close() error
}

// Manifest is the sink's commit record.
type Manifest struct {
	Format           string         `json:"format"`
	FormatVersion    int            `json:"format_version"`
	Document         string         `json:"document"`
	RootKey          string         `json:"root_key"`
	Epoch            uint64         `json:"epoch"`
	SchemaHash       string         `json:"schema_hash,omitempty"`
	Base             *BaseEntry     `json:"base,omitempty"`
	Segments         []SegmentEntry `json:"segments"`
	LatestGeneration uint64         `json:"latest_generation"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// BaseEntry describes a base snapshot.
type BaseEntry struct {
	Generation uint64    `json:"generation"`
	Path       string    `json:"path"`
	Files      []string  `json:"files"`
	Documents  int64     `json:"documents"`
	CreatedAt  time.Time `json:"created_at"`
}

// SegmentEntry describes a segment file.
type SegmentEntry struct {
	From      uint64    `json:"from"`
	To        uint64    `json:"to"`
	Path      string    `json:"path"`
	Upserts   int       `json:"upserts"`
	Deletes   int       `json:"deletes"`
	CreatedAt time.Time `json:"created_at"`
}

// Format writes the files of a log sink.
type Format interface {
	Name() string // manifest format name, e.g. "docsync-parquet-log"
	Ext() string
	WriteSegment(path string, s *schema.Schema, gens []Generation) (upserts, deletes int, err error)
	WriteBase(dir string, s *schema.Schema, next func() (BaseDoc, bool, error), rowsPerFile int) (files []string, docs int64, err error)
}

// LogOptions configures a log sink.
type LogOptions struct {
	Name            string
	Dir             string
	Document        string
	RootKey         string
	SegmentInterval time.Duration
	RetainSegments  time.Duration
	BaseFileRows    int
}

// LogSink implements Sink on a directory using a Format.
type LogSink struct {
	opt       LogOptions
	format    Format
	mu        sync.Mutex
	man       Manifest
	lastWrite time.Time
}

// NewLogSink opens (or initializes) a log sink directory.
func NewLogSink(opt LogOptions, f Format) (*LogSink, error) {
	for _, d := range []string{opt.Dir, filepath.Join(opt.Dir, "segments"), filepath.Join(opt.Dir, "base")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	s := &LogSink{opt: opt, format: f}
	b, err := os.ReadFile(filepath.Join(opt.Dir, "manifest.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.man = Manifest{Format: f.Name(), FormatVersion: 1, Document: opt.Document, RootKey: opt.RootKey}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.man); err != nil {
			return nil, fmt.Errorf("sink %s: corrupt manifest: %w", opt.Name, err)
		}
		if s.man.Format != f.Name() {
			return nil, fmt.Errorf("sink %s: %s holds %q output, not %q", opt.Name, opt.Dir, s.man.Format, f.Name())
		}
	}
	s.cleanTemp()
	return s, nil
}

// Name implements Sink.
func (s *LogSink) Name() string { return s.opt.Name }

// Manifest returns a copy of the current manifest.
func (s *LogSink) Manifest() Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.man
	m.Segments = append([]SegmentEntry(nil), s.man.Segments...)
	return m
}

// Position implements Sink.
func (s *LogSink) Position() (uint64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.man.LatestGeneration, s.man.Epoch
}

// HasBase implements Sink.
func (s *LogSink) HasBase(epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.man.Epoch == epoch && s.man.Base != nil
}

// BaseAge implements Sink.
func (s *LogSink) BaseAge() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.man.Base == nil {
		return time.Time{}, false
	}
	return s.man.Base.CreatedAt, true
}

// Due implements Sink.
func (s *LogSink) Due(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Sub(s.lastWrite) >= s.opt.SegmentInterval
}

// Write implements Sink.
func (s *LogSink) Write(ctx context.Context, sch *schema.Schema, gens []Generation) error {
	if len(gens) == 0 {
		return nil
	}
	s.mu.Lock()
	latest, epoch := s.man.LatestGeneration, s.man.Epoch
	s.mu.Unlock()
	if gens[0].Epoch != epoch {
		return fmt.Errorf("sink %s: generation %d belongs to epoch %d but the sink is at epoch %d (write a base first)", s.opt.Name, gens[0].Gen, gens[0].Epoch, epoch)
	}
	// Skip generations already written (idempotent replays after a crash).
	for len(gens) > 0 && gens[0].Gen <= latest {
		gens = gens[1:]
	}
	if len(gens) == 0 {
		return nil
	}
	if gens[0].Gen != latest+1 {
		return fmt.Errorf("sink %s: gap between generation %d and %d", s.opt.Name, latest, gens[0].Gen)
	}
	from, to := gens[0].Gen, gens[len(gens)-1].Gen
	rel := filepath.Join("segments", fmt.Sprintf("%020d-%020d.%s", from, to, s.format.Ext()))
	ups, dels, err := s.format.WriteSegment(filepath.Join(s.opt.Dir, rel), sch, gens)
	if err != nil {
		return fmt.Errorf("sink %s: write segment: %w", s.opt.Name, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.man
	m.Segments = append(append([]SegmentEntry(nil), s.man.Segments...), SegmentEntry{
		From: from, To: to, Path: filepath.ToSlash(rel), Upserts: ups, Deletes: dels, CreatedAt: time.Now().UTC(),
	})
	m.LatestGeneration = to
	m.SchemaHash = sch.Hash()
	if err := s.commit(m); err != nil {
		return err
	}
	s.lastWrite = time.Now()
	return nil
}

// WriteBase implements Sink.
func (s *LogSink) WriteBase(ctx context.Context, sch *schema.Schema, epoch, gen uint64, next func() (BaseDoc, bool, error)) error {
	relDir := filepath.Join("base", fmt.Sprintf("%020d", gen))
	tmpDir := filepath.Join(s.opt.Dir, relDir+".tmp")
	finalDir := filepath.Join(s.opt.Dir, relDir)
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	files, docs, err := s.format.WriteBase(tmpDir, sch, next, s.opt.BaseFileRows)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("sink %s: write base: %w", s.opt.Name, err)
	}
	_ = os.RemoveAll(finalDir)
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return err
	}
	syncDir(filepath.Join(s.opt.Dir, "base"))

	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.man
	m.Base = &BaseEntry{Generation: gen, Path: filepath.ToSlash(relDir), Files: files, Documents: docs, CreatedAt: time.Now().UTC()}
	m.SchemaHash = sch.Hash()
	if m.Epoch != epoch {
		// New epoch: older segments describe documents of another shape; consumers restart from this base.
		m.Epoch = epoch
		m.Segments = nil
		m.LatestGeneration = gen
	} else if gen > m.LatestGeneration {
		m.LatestGeneration = gen
	}
	return s.commit(m)
}

// Retain implements Sink: drops segments fully covered by the base once older than
// the retention period, and bases other than the newest.
func (s *LogSink) Retain(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.man.Base == nil {
		return nil
	}
	m := s.man
	var keep []SegmentEntry
	var drop []string
	for _, seg := range s.man.Segments {
		if seg.To <= m.Base.Generation && now.Sub(seg.CreatedAt) > s.opt.RetainSegments {
			drop = append(drop, filepath.Join(s.opt.Dir, filepath.FromSlash(seg.Path)))
			continue
		}
		keep = append(keep, seg)
	}
	if len(drop) > 0 {
		m.Segments = keep
		if err := s.commit(m); err != nil {
			return err
		}
		for _, p := range drop {
			_ = os.Remove(p)
		}
	}
	// Remove bases other than the current one once they are past retention.
	entries, err := os.ReadDir(filepath.Join(s.opt.Dir, "base"))
	if err != nil {
		return err
	}
	current := filepath.Base(m.Base.Path)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == current {
			continue
		}
		info, err := e.Info()
		if err == nil && now.Sub(info.ModTime()) > s.opt.RetainSegments {
			_ = os.RemoveAll(filepath.Join(s.opt.Dir, "base", e.Name()))
		}
	}
	return nil
}

// Close implements Sink.
func (s *LogSink) Close() error { return nil }

// commit replaces the manifest atomically. Caller holds s.mu.
func (s *LogSink) commit(m Manifest) error {
	m.Format, m.FormatVersion = s.format.Name(), 1
	m.Document, m.RootKey = s.opt.Document, s.opt.RootKey
	m.UpdatedAt = time.Now().UTC()
	sort.Slice(m.Segments, func(i, j int) bool { return m.Segments[i].From < m.Segments[j].From })
	if m.Segments == nil {
		m.Segments = []SegmentEntry{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(s.opt.Dir, "manifest.json"), b); err != nil {
		return err
	}
	s.man = m
	return nil
}

// cleanTemp removes leftovers of interrupted writes.
func (s *LogSink) cleanTemp() {
	for _, sub := range []string{"segments", "base", "."} {
		entries, _ := os.ReadDir(filepath.Join(s.opt.Dir, sub))
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				_ = os.RemoveAll(filepath.Join(s.opt.Dir, sub, e.Name()))
			}
		}
	}
	// Segment files not referenced by the manifest were written but never committed.
	known := map[string]bool{}
	for _, seg := range s.man.Segments {
		known[filepath.Base(seg.Path)] = true
	}
	entries, _ := os.ReadDir(filepath.Join(s.opt.Dir, "segments"))
	for _, e := range entries {
		if !known[e.Name()] {
			_ = os.Remove(filepath.Join(s.opt.Dir, "segments", e.Name()))
		}
	}
}

// WriteFileAtomic writes data to path via a synced temp file and rename.
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// CreateAtomic returns a file that becomes path only when Finish is called.
func CreateAtomic(path string) (*AtomicFile, error) {
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return nil, err
	}
	return &AtomicFile{File: f, path: path}, nil
}

// AtomicFile is a temp file renamed into place on Finish.
type AtomicFile struct {
	*os.File
	path string
}

// Finish syncs, closes and renames the file into place.
func (a *AtomicFile) Finish() error {
	if err := a.File.Sync(); err != nil {
		a.File.Close()
		return err
	}
	if err := a.File.Close(); err != nil {
		return err
	}
	if err := os.Rename(a.path+".tmp", a.path); err != nil {
		return err
	}
	syncDir(filepath.Dir(a.path))
	return nil
}

// Abort discards the temp file.
func (a *AtomicFile) Abort() {
	a.File.Close()
	_ = os.Remove(a.path + ".tmp")
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
