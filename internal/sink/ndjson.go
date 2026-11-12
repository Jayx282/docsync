package sink

import (
	"bufio"
	"fmt"
	"path/filepath"

	"github.com/Jayx282/docsync/internal/doc"
	"github.com/Jayx282/docsync/internal/schema"
)

// NDJSON writes one JSON object per line: {"gen":N,"op":"upsert"|"delete","root":...,"doc":{...}}.
// It is meant for debugging and tests.
type NDJSON struct{}

// Name implements Format.
func (NDJSON) Name() string { return "docsync-ndjson-log" }

// Ext implements Format.
func (NDJSON) Ext() string { return "ndjson" }

// WriteSegment implements Format.
func (NDJSON) WriteSegment(path string, _ *schema.Schema, gens []Generation) (int, int, error) {
	f, err := CreateAtomic(path)
	if err != nil {
		return 0, 0, err
	}
	w := bufio.NewWriter(f)
	ups, dels := 0, 0
	for _, g := range gens {
		for _, c := range g.Changes {
			line, err := ndjsonLine(g.Gen, c.Root, c.Delete, c.Doc)
			if err != nil {
				f.Abort()
				return 0, 0, err
			}
			w.WriteString(line)
			if c.Delete {
				dels++
			} else {
				ups++
			}
		}
	}
	if err := w.Flush(); err != nil {
		f.Abort()
		return 0, 0, err
	}
	return ups, dels, f.Finish()
}

// WriteBase implements Format.
func (NDJSON) WriteBase(dir string, _ *schema.Schema, next func() (BaseDoc, bool, error), rowsPerFile int) ([]string, int64, error) {
	var files []string
	var total int64
	var f *AtomicFile
	var w *bufio.Writer
	rows := 0
	finish := func() error {
		if f == nil {
			return nil
		}
		if err := w.Flush(); err != nil {
			f.Abort()
			return err
		}
		err := f.Finish()
		f = nil
		return err
	}
	for {
		d, ok, err := next()
		if err != nil {
			if f != nil {
				f.Abort()
			}
			return nil, 0, err
		}
		if !ok {
			break
		}
		if f == nil || rows >= rowsPerFile {
			if err := finish(); err != nil {
				return nil, 0, err
			}
			name := fmt.Sprintf("part-%05d.ndjson", len(files))
			if f, err = CreateAtomic(filepath.Join(dir, name)); err != nil {
				return nil, 0, err
			}
			w = bufio.NewWriter(f)
			files = append(files, name)
			rows = 0
		}
		line, err := ndjsonLine(d.Gen, d.Root, false, d.Doc)
		if err != nil {
			f.Abort()
			return nil, 0, err
		}
		w.WriteString(line)
		rows++
		total++
	}
	if err := finish(); err != nil {
		return nil, 0, err
	}
	return files, total, nil
}

func ndjsonLine(gen uint64, root any, del bool, d doc.Object) (string, error) {
	op := "upsert"
	var body any = d
	if del {
		op, body = "delete", nil
	}
	rootJSON, err := doc.JSON(root)
	if err != nil {
		return "", err
	}
	docJSON, err := doc.JSON(body)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("{\"gen\":%d,\"op\":%q,\"root\":%s,\"doc\":%s}\n", gen, op, rootJSON, docJSON), nil
}
