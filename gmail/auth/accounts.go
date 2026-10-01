package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"golang.org/x/oauth2"
)

var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

var ErrAccountRequired = errors.New("--account is required when multiple accounts are configured")

// SelectionError marks a --account value that did not resolve to a configured
// profile (omitted, unknown, or invalid). It is a usage error, never an auth
// failure: the fix is a different account name, not a new login, and a caller
// that misreads it as auth would send a human to re-authenticate for nothing.
type SelectionError struct{ err error }

func (e *SelectionError) Error() string { return e.err.Error() }
func (e *SelectionError) Unwrap() error { return e.err }

// ValidateAccountName rejects names that could escape the profile directory.
func ValidateAccountName(name string) error {
	if !accountNamePattern.MatchString(name) {
		return fmt.Errorf("invalid account name %q (use 1-64 letters, numbers, hyphens, or underscores; start with a letter or number)", name)
	}
	return nil
}

// AccountTokenPath returns the token path for a named account. The default
// account keeps using the legacy token path, so existing installations migrate
// without moving or rewriting credentials.
func AccountTokenPath(name string) (string, error) {
	if err := ValidateAccountName(name); err != nil {
		return "", err
	}
	legacy, err := TokenPath()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(legacy), "accounts", name, "token.json")
	if name == "default" {
		if err := migrateLegacyDefault(legacy, path); err != nil {
			return "", err
		}
	}
	return path, nil
}

// Accounts lists configured profiles, including the legacy token as "default".
func Accounts() ([]string, error) {
	legacy, err := TokenPath()
	if err != nil {
		return nil, err
	}
	defaultToken, err := AccountTokenPath("default")
	if err != nil {
		return nil, err
	}
	namesSet := make(map[string]bool)
	if regularFile(defaultToken) || regularFile(legacy) {
		namesSet["default"] = true
	}
	registered, err := RegisteredAccounts()
	if err != nil {
		return nil, err
	}
	for _, name := range registered {
		namesSet[name] = true
	}
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(legacy), "accounts"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "default" || ValidateAccountName(entry.Name()) != nil {
			continue
		}
		path := filepath.Join(filepath.Dir(legacy), "accounts", entry.Name(), "token.json")
		if regularFile(path) {
			namesSet[entry.Name()] = true
		}
	}
	names := make([]string, 0, len(namesSet))
	for name := range namesSet {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// migrateLegacyDefault copies the legacy single-token file into the default
// profile. The original is only ever read, never moved or rewritten.
//
// The legacy path stays the documented provisioning target (the Nix module's
// setup notes install a rotated token there), so a legacy token that is newer
// than the profile copy is copied across again rather than ignored. Without
// that, rotating the token by reinstalling it at the old path would silently
// keep serving the pre-rotation copy. A normal refresh writes the profile
// copy, so the profile copy is the newer of the two and no re-copy happens.
func migrateLegacyDefault(legacy, target string) error {
	legacyInfo, err := os.Stat(legacy)
	if err != nil || !legacyInfo.Mode().IsRegular() {
		return nil
	}
	if targetInfo, err := os.Stat(target); err == nil && targetInfo.Mode().IsRegular() &&
		!legacyInfo.ModTime().After(targetInfo.ModTime()) {
		return nil
	}
	token, err := readToken(legacy)
	if err != nil {
		return fmt.Errorf("reading legacy default token: %w", err)
	}
	if err := writeToken(target, token); err != nil {
		return fmt.Errorf("copying legacy default token: %w", err)
	}
	return RegisterAccount("default", "")
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ResolveAccount selects a configured account. It never tests connectivity or
// token validity, so command behavior does not depend on network state.
func ResolveAccount(requested string) (string, error) {
	accounts, err := Accounts()
	if err != nil {
		return "", err
	}
	if requested != "" {
		if err := ValidateAccountName(requested); err != nil {
			return "", &SelectionError{err}
		}
		for _, account := range accounts {
			if account == requested {
				return requested, nil
			}
		}
		return "", &SelectionError{fmt.Errorf("account %q is not configured; configured accounts: %v", requested, accounts)}
	}
	switch len(accounts) {
	case 0:
		return "default", nil
	case 1:
		return accounts[0], nil
	default:
		return "", &SelectionError{fmt.Errorf("%w; configured accounts: %v", ErrAccountRequired, accounts)}
	}
}

// LoginAccount chooses the profile to create or replace. With no profiles it
// retains the historical default account name; with existing profiles it
// requires an explicit selection unless there is exactly one.
func LoginAccount(requested string) (string, error) {
	if requested != "" {
		if err := ValidateAccountName(requested); err != nil {
			return "", err
		}
		return requested, nil
	}
	return ResolveAccount("")
}

// RemoveAccount removes one configured account token.
func RemoveAccount(name string) error {
	resolved, err := ResolveAccount(name)
	if err != nil {
		return err
	}
	path, err := AccountTokenPath(resolved)
	if err != nil {
		return err
	}
	if err := unregisterAccount(resolved); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if resolved == "default" {
		legacy, err := TokenPath()
		if err != nil {
			return err
		}
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Remove(path + ".lock"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// TokenSourceForAccount creates a persisting token source for the selected
// account. An empty name is resolved under the explicit-selection policy.
func TokenSourceForAccount(ctx context.Context, cfg *Config, requested string) (oauth2.TokenSource, error) {
	account, err := ResolveAccount(requested)
	if err != nil {
		return nil, err
	}
	path, err := AccountTokenPath(account)
	if err != nil {
		return nil, err
	}
	tok, err := readToken(path)
	if err != nil {
		return nil, fmt.Errorf("no token for account %q (run `docket auth login --account %s`): %w", account, account, err)
	}
	base := oauthConfig(cfg.Provider).TokenSource(ctx, tok)
	return NewPersistingTokenSource(base, path), nil
}
