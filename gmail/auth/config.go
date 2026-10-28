// Package auth handles OAuth provider configuration, the PKCE login flow,
// and the flock-guarded token store used by mail and calendar commands.
package auth

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/zachpmanson/docket/gmail/config"
)

// Provider describes an OAuth2 client registration. It is read from config
// rather than hardcoded so that swapping clients later is an edit, not a
// refactor. See docket-design.md §3.
type Provider struct {
	ClientID     string   `toml:"client_id"`
	ClientSecret string   `toml:"client_secret"`
	AuthURL      string   `toml:"auth_url"`
	TokenURL     string   `toml:"token_url"`
	RedirectURI  string   `toml:"redirect_uri"`
	Scopes       []string `toml:"scopes"`
	UsePKCE      bool     `toml:"use_pkce"`
}

// Config is the top-level shape of ~/.config/docket/config.toml.
// DefaultCalendar, when set, is the calendar id new/listing operations fall
// back to when no --calendar flag is given ("primary" otherwise).
type Config struct {
	Provider         Provider    `toml:"provider"`
	DefaultCalendar  string      `toml:"default_calendar"`
	ReadOnlyAccounts []string    `toml:"read_only_accounts"`
	Permissions      Permissions `toml:"permissions"`
}

// AccountReadOnly reports whether writes are disabled for the named account.
func (c *Config) AccountReadOnly(account string) bool {
	for _, name := range c.ReadOnlyAccounts {
		if name == account {
			return true
		}
	}
	return false
}

// ConfigPath returns the path to config.toml, honoring XDG_CONFIG_HOME.
func ConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "docket", "config.toml"), nil
}

// ensureConfig seeds path with the embedded default config when it does not
// exist yet, so a fresh install works without a manual copy step. An existing
// file is never touched -- including the read-only store symlink the Nix
// module installs for the beltino service account.
func ensureConfig(path string) error {
	return ensureConfigFrom(path, bytes.NewReader(config.Default))
}

// The reader keeps preparation separate from publication, including on errors.
func ensureConfigFrom(path string, defaults io.Reader) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	// Prepare privately on the same filesystem. Publishing with O_EXCL before
	// writing exposes empty/partial defaults to other goroutines and processes.
	f, err := os.CreateTemp(filepath.Dir(path), ".config.toml-*")
	if err != nil {
		return fmt.Errorf("creating temporary config for %s: %w", path, err)
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, defaults); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	// Link atomically publishes the completed file without replacing any
	// competing winner, operator config, or symlink (even a dangling one).
	// Rename would clobber the destination; a mutex cannot guard other processes.
	if err := os.Link(f.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("publishing %s: %w", path, err)
	}

	// stderr, never stdout: stdout carries the JSON envelope agents parse.
	fmt.Fprintf(os.Stderr, "docket: created %s with default settings\n", path)
	return nil
}

// LoadConfig reads and parses config.toml, creating it from the embedded
// defaults on first run. Client secret and ID may be overridden by
// DOCKET_CLIENT_ID / DOCKET_CLIENT_SECRET env vars so a custom provider
// registration can be swapped in without editing the file.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	if err := ensureConfig(path); err != nil {
		return nil, err
	}

	var cfg Config
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if keys := metadata.Undecoded(); len(keys) != 0 {
		return nil, fmt.Errorf("unknown config keys in %s: %v (check spelling and quote operation names, e.g. \"mail.read\")", path, keys)
	}
	if err := cfg.Permissions.validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", path, err)
	}
	for _, account := range cfg.ReadOnlyAccounts {
		if err := ValidateAccountName(account); err != nil {
			return nil, fmt.Errorf("read_only_accounts in %s: %w", path, err)
		}
	}

	if id := os.Getenv("DOCKET_CLIENT_ID"); id != "" {
		cfg.Provider.ClientID = id
	}
	if secret := os.Getenv("DOCKET_CLIENT_SECRET"); secret != "" {
		cfg.Provider.ClientSecret = secret
	}

	if cfg.Provider.ClientID == "" {
		return nil, fmt.Errorf("provider.client_id is empty in %s", path)
	}

	return &cfg, nil
}
