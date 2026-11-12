package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "docsync.log")
	w, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n" // 40 bytes: two lines fit in 100, three do not
	for i := 0; i < 9; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docsync.log", "docsync.log.1", "docsync.log.2"} {
		b, err := os.ReadFile(filepath.Join(filepath.Dir(path), name))
		if err != nil || len(b) == 0 || len(b) > 100 {
			t.Fatalf("%s: %d bytes, %v", name, len(b), err)
		}
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("only keep old files are kept")
	}

	// Reopening appends to the existing file.
	w, err = Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if _, err := w.Write([]byte("y\n")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	after, _ := os.Stat(path)
	if after.Size() != before.Size()+2 {
		t.Fatalf("reopen must append: %d -> %d", before.Size(), after.Size())
	}
	if _, err := w.Write([]byte("z")); err == nil {
		t.Fatal("writing after Close fails")
	}
}
