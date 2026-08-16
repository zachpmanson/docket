// Package auth handles OAuth provider configuration, the PKCE login flow,
// and the flock-guarded token store used by mail and calendar commands.
package auth

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
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
	Provider        Provider `toml:"provider"`
	DefaultCalendar string   `toml:"default_calendar"`
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

// LoadConfig reads and parses config.toml. Client secret and ID may be
// overridden by DOCKET_CLIENT_ID / DOCKET_CLIENT_SECRET env vars so a
// custom provider registration can be swapped in without editing the file.
func LoadConfig() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
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
