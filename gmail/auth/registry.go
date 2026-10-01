package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/gofrs/flock"
)

type accountRegistry struct {
	Accounts map[string]string `json:"accounts"`
}

// RegistryPath returns the XDG config path for account names and verified emails.
func RegistryPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "docket", "accounts.json"), nil
}

func readRegistry(path string) (accountRegistry, error) {
	registry := accountRegistry{Accounts: make(map[string]string)}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return registry, nil
	}
	if err != nil {
		return registry, err
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return registry, fmt.Errorf("parsing account registry: %w", err)
	}
	if registry.Accounts == nil {
		registry.Accounts = make(map[string]string)
	}
	for name := range registry.Accounts {
		if err := ValidateAccountName(name); err != nil {
			return accountRegistry{}, fmt.Errorf("invalid account registry entry: %w", err)
		}
	}
	return registry, nil
}

func updateRegistry(update func(map[string]string) error) error {
	path, err := RegistryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking account registry: %w", err)
	}
	defer lock.Unlock()

	registry, err := readRegistry(path)
	if err != nil {
		return err
	}
	if err := update(registry.Accounts); err != nil {
		return err
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".accounts-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// RegisterAccount adds an account name and its verified email, if known.
func RegisterAccount(name, email string) error {
	if err := ValidateAccountName(name); err != nil {
		return err
	}
	return updateRegistry(func(accounts map[string]string) error {
		if existing, ok := accounts[name]; ok && email == "" {
			email = existing
		}
		accounts[name] = email
		return nil
	})
}

// AccountEmail returns the last verified email recorded for an account.
func AccountEmail(name string) (string, error) {
	if err := ValidateAccountName(name); err != nil {
		return "", err
	}
	path, err := RegistryPath()
	if err != nil {
		return "", err
	}
	registry, err := readRegistry(path)
	if err != nil {
		return "", err
	}
	return registry.Accounts[name], nil
}

// RegisteredAccounts returns the account names stored in the registry.
func RegisteredAccounts() ([]string, error) {
	path, err := RegistryPath()
	if err != nil {
		return nil, err
	}
	registry, err := readRegistry(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(registry.Accounts))
	for name := range registry.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func unregisterAccount(name string) error {
	return updateRegistry(func(accounts map[string]string) error {
		delete(accounts, name)
		return nil
	})
}
