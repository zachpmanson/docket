package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"
)

// TokenPath returns the path to the persisted token file, honoring
// XDG_STATE_HOME. Parent directory is 0700, file is 0600.
func TokenPath() (string, error) {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "docket", "token.json"), nil
}

func ensureTokenDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

func readToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func writeToken(path string, tok *oauth2.Token) error {
	if err := ensureTokenDir(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// freshEnough reports whether tok is valid and not within refreshSkew of
// expiring, i.e. safe to hand back to a caller without refreshing.
const refreshSkew = 2 * time.Minute

func freshEnough(tok *oauth2.Token) bool {
	if tok == nil || tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return time.Now().Add(refreshSkew).Before(tok.Expiry)
}

// persistingSource wraps an oauth2.TokenSource so that every refresh is
// flock-guarded and persisted to disk. Several docket invocations can run
// concurrently against the same token file; without the lock, two
// simultaneous refreshes race and one refresh token gets invalidated by
// Google. See docket-design.md §3.
type persistingSource struct {
	src  oauth2.TokenSource
	path string
}

// NewPersistingTokenSource wraps src so Token() re-reads the on-disk token
// under an exclusive flock before deciding whether a refresh is needed, and
// persists any newly minted token before returning it.
func NewPersistingTokenSource(src oauth2.TokenSource, path string) oauth2.TokenSource {
	return &persistingSource{src: src, path: path}
}

// SaveToken persists tok to path, creating parent directories as needed.
// Used after a fresh interactive login, before any refresh has happened.
func SaveToken(tok *oauth2.Token, path string) error {
	return writeToken(path, tok)
}

func (p *persistingSource) Token() (*oauth2.Token, error) {
	if err := ensureTokenDir(p.path); err != nil {
		return nil, err
	}

	lock := flock.New(p.path + ".lock")
	if err := lock.Lock(); err != nil {
		return nil, fmt.Errorf("locking token file: %w", err)
	}
	defer lock.Unlock()

	// Another process may have just refreshed while we waited for the lock.
	if tok, err := readToken(p.path); err == nil && freshEnough(tok) {
		return tok, nil
	}

	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	if err := writeToken(p.path, tok); err != nil {
		return nil, err
	}
	return tok, nil
}
