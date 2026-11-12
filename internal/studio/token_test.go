package studio

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTokenFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "studio")
	path := filepath.Join(dir, TokenFile)
	gitignore := filepath.Join(dir, ".gitignore")
	unix := runtime.GOOS != "windows"

	tok, created, warn, err := LoadOrCreateToken(dir)
	if err != nil || !created || warn != "" || len(tok) != 48 {
		t.Fatalf("first start: %q created=%v warn=%q err=%v", tok, created, warn, err)
	}
	if info, _ := os.Stat(path); unix && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file must be owner-only, is %v", info.Mode().Perm())
	}
	if b, _ := os.ReadFile(gitignore); !strings.Contains(string(b), "\n"+TokenFile+"\n") {
		t.Fatalf("the studio folder's .gitignore must list the token file:\n%s", b)
	}

	again, created, _, err := LoadOrCreateToken(dir)
	if err != nil || created || again != tok {
		t.Fatalf("restart must reuse the token: %q created=%v err=%v", again, created, err)
	}

	if unix {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, warn, err = LoadOrCreateToken(dir)
		info, _ := os.Stat(path)
		if err != nil || warn == "" || info.Mode().Perm() != 0o600 {
			t.Fatalf("loose permissions are fixed with a warning: warn=%q perm=%v err=%v", warn, info.Mode().Perm(), err)
		}
	}

	// An existing .gitignore gets the entry once, after its own lines.
	if err := os.WriteFile(gitignore, []byte("pipelines/*/out/"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, _, err := LoadOrCreateToken(dir); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(gitignore)
	if !strings.HasPrefix(string(b), "pipelines/*/out/\n") || strings.Count(string(b), "\n"+TokenFile+"\n") != 1 {
		t.Fatalf("existing .gitignore:\n%s", b)
	}

	if err := os.WriteFile(path, []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadOrCreateToken(dir); err == nil || !strings.Contains(err.Error(), "delete the file") {
		t.Fatalf("a too-short token is refused: %v", err)
	}
}
