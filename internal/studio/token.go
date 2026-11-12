package studio

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TokenFile is the file in the studio directory that holds the sign-in token.
const TokenFile = ".token"

const minTokenLen = 16

// LoadOrCreateToken returns the sign-in token saved in dir, creating one the first
// time. Reusing it keeps sign-in links and browser cookies valid across restarts.
//
// The file is readable only by its owner (on Unix; on Windows the folder's access
// rules apply), and dir's .gitignore lists it so a committed studio folder does not
// leak it. warn describes a problem that was fixed, such as loose permissions.
func LoadOrCreateToken(dir string) (token string, created bool, warn string, err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, "", err
	}
	path := filepath.Join(dir, TokenFile)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		token = strings.TrimSpace(string(b))
		if len(token) < minTokenLen {
			return "", false, "", fmt.Errorf("%s holds a token shorter than %d characters; delete the file to get a new one", path, minTokenLen)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			if err != nil {
				return "", false, "", err
			}
			if info.Mode().Perm()&0o077 != 0 {
				if err := os.Chmod(path, 0o600); err != nil {
					return "", false, "", err
				}
				warn = fmt.Sprintf("%s was readable by other users and is now owner-only; delete it to issue a new token if someone may have read it", path)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return "", false, "", err
		}
		token, created = hex.EncodeToString(raw), true
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", false, "", err
		}
		if _, err := f.WriteString(token + "\n"); err != nil {
			f.Close()
			return "", false, "", err
		}
		if err := f.Close(); err != nil {
			return "", false, "", err
		}
	default:
		return "", false, "", err
	}
	if err := ensureGitignore(dir); err != nil {
		return "", false, "", err
	}
	return token, created, warn, nil
}

// ensureGitignore adds the token file to dir/.gitignore unless it is already listed.
func ensureGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if l := strings.TrimSpace(line); l == TokenFile || l == "/"+TokenFile {
			return nil
		}
	}
	add := "# docsync studio: keep the sign-in token out of git.\n" + TokenFile + "\n"
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		add = "\n" + add
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(add); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
