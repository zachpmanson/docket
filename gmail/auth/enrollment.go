package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"
)

// checkDirectoryChain rejects symlinks and non-directories without creating or
// opening anything. Missing directories are fine; other metadata errors fail shut.
func checkDirectoryChain(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if parent != path {
		if err := checkDirectoryChain(parent); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("enrollment directory %q is not a directory (symlinks are forbidden)", path)
	}
	return nil
}

func newAccountPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("agent auth login/add requires explicit --account <new-profile>")
	}
	if err := ValidateAccountName(name); err != nil {
		return "", err
	}
	// Never resolve or migrate legacy credentials during guarded enrollment.
	if name == "default" {
		return "", fmt.Errorf("account %q is reserved; choose a new profile name", name)
	}
	legacy, err := TokenPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(legacy), "accounts", name, "token.json"), nil
}

func checkNewProfilePath(path string) error {
	dir := filepath.Dir(path)
	if err := checkDirectoryChain(filepath.Dir(dir)); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("profile %q already exists; agent login cannot replace credentials", filepath.Base(dir))
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// CheckNewAccount checks only name, registry metadata and filesystem metadata.
// It never reads tokens, migrates default, creates files or starts OAuth.
func CheckNewAccount(name string) error {
	path, err := newAccountPath(name)
	if err != nil {
		return err
	}
	registryPath, err := RegistryPath()
	if err != nil {
		return err
	}
	if err := checkDirectoryChain(filepath.Dir(registryPath)); err != nil {
		return err
	}
	if info, err := os.Lstat(registryPath); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("account registry must be a regular file, not a symlink")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	registry, err := readRegistry(registryPath)
	if err != nil {
		return err
	}
	if _, exists := registry.Accounts[name]; exists {
		return fmt.Errorf("account %q is already registered; choose a new profile name", name)
	}
	return checkNewProfilePath(path)
}

// InstallNewAccount commits a consented token without replacing credentials.
// Registry serialization covers both the final name check and token publication.
// A token installed before a later registry/durability failure is preserved;
// callers must not retry by replacing it or delete it as a rollback.
func InstallNewAccount(name string, tok *oauth2.Token) error {
	return installNewAccount(name, tok, installNewToken)
}

func installNewAccount(name string, tok *oauth2.Token, install func(string, *oauth2.Token) (bool, error)) error {
	if err := CheckNewAccount(name); err != nil {
		return err
	}
	path, err := newAccountPath(name)
	if err != nil {
		return err
	}
	installed := false
	err = updateRegistry(func(accounts map[string]string) error {
		if _, exists := accounts[name]; exists {
			return fmt.Errorf("account %q is already registered", name)
		}
		if err := checkNewProfilePath(path); err != nil {
			return err
		}
		parent := filepath.Dir(filepath.Dir(path))
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		// Claim the entire profile, including empty or dangling-symlink names.
		if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		defer func() {
			if !installed {
				// Only remove our empty directory, never any contents.
				_ = os.Remove(filepath.Dir(path))
			}
		}()
		// Persist the profile's name in accounts/ before publishing a token
		// whose own directory entry will be synced by installNewToken.
		parentDir, err := os.Open(parent)
		if err != nil {
			return err
		}
		syncErr := parentDir.Sync()
		closeErr := parentDir.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		installed, err = install(path, tok)
		if err != nil {
			return err
		}
		accounts[name] = "" // local profile name is NOT a verified email identity
		return nil
	})
	if err != nil && installed {
		return fmt.Errorf("token installed for account %q but registration/durability failed; token preserved, human recovery required (do not overwrite): %w", name, err)
	}
	return err
}

func installNewToken(path string, tok *oauth2.Token) (bool, error) {
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*.tmp")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	// Unlike Rename, Link fails if ANY destination already exists. Readers
	// see either no token or the complete synced 0600 file, never a partial one.
	if err := os.Link(tmp.Name(), path); err != nil {
		return false, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return true, err
	}
	defer dir.Close()
	return true, dir.Sync()
}
