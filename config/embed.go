// Package config embeds docket's default configuration so a fresh install can
// seed ~/.config/docket/config.toml on first run instead of failing.
//
// The embedded bytes are config.toml in this directory -- the same file the
// Nix module installs declaratively for the beltino service account -- so the
// shipped defaults and the generated ones cannot drift apart.
package config

import _ "embed"

// Default is the contents of config/config.toml: the Thunderbird-borrowed
// OAuth client registration plus Zach's default calendar. See
// docket-design.md §3 for why the client lives in config rather than in code.
//
//go:embed config.toml
var Default []byte
