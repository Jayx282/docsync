// Package logfile is a size-based rotating log writer, for docsync running as a
// service (where there is no console to log to).
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Writer appends to path and, when the file would grow past max bytes, renames it to
// path.1 (path.1 to path.2, and so on, keeping keep old files) and starts a new one.
type Writer struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

// Open opens (or creates) the log file and its folder.
func Open(path string, max int64, keep int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &Writer{path: path, max: max, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, info.Size()
	return nil
}

// Write writes p, rotating first if p would take the file past its size limit.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, errors.New("log file is closed")
	}
	if w.size > 0 && w.size+int64(len(p)) > w.max {
		w.rotate()
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts the old files along. If a rename fails (on Windows another program
// may hold the file), it keeps appending to the current file rather than losing logs.
func (w *Writer) rotate() {
	w.f.Close()
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.keep))
	for i := w.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if err := w.open(); err != nil {
			w.f = nil
		}
		return
	}
	if err := w.open(); err != nil {
		w.f = nil
	}
}

// Close closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
