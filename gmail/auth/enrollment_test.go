package auth

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

func TestNewAccountMetadataOnly(t *testing.T) {
	for _, state := range []string{"registered", "empty-dir", "invalid-token", "dangling-symlink", "parent-symlink", "corrupt-registry", "registry-symlink", "parent-file"} {
		t.Run(state, func(t *testing.T) {
			root := useStateHome(t)
			profile := filepath.Join(root, "docket", "accounts", "new")
			registry, _ := RegistryPath()
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "registered":
				must(RegisterAccount("new", ""))
			case "empty-dir", "invalid-token":
				must(os.MkdirAll(profile, 0o700))
				if state == "invalid-token" {
					// A FIFO would hang if preflight attempted to read credentials.
					must(os.WriteFile(filepath.Join(profile, "token.json"), []byte("not a token"), 0o600))
				}
			case "dangling-symlink":
				must(os.MkdirAll(filepath.Dir(profile), 0o700))
				must(os.Symlink(filepath.Join(root, "missing"), profile))
			case "parent-symlink":
				must(os.Mkdir(filepath.Join(root, "real"), 0o700))
				must(os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "docket")))
			case "parent-file":
				must(os.WriteFile(filepath.Join(root, "docket"), []byte("protected"), 0o600))
			case "corrupt-registry", "registry-symlink":
				must(os.MkdirAll(filepath.Dir(registry), 0o700))
				if state == "corrupt-registry" {
					must(os.WriteFile(registry, []byte("not json"), 0o600))
				} else {
					must(os.Symlink(filepath.Join(root, "missing"), registry))
				}
			}
			if err := CheckNewAccount("new"); err == nil {
				t.Fatal("accepted occupied or unsafe profile")
			}
			if err := InstallNewAccount("new", &oauth2.Token{AccessToken: "synthetic"}); err == nil {
				t.Fatal("installed occupied or unsafe profile")
			}
		})
	}
}

func TestNewAccountExplicitAndReserved(t *testing.T) {
	root := useStateHome(t)
	for _, name := range []string{"", "default", "../escape", "a/b", "-bad"} {
		if err := CheckNewAccount(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := CheckNewAccount("new"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("preflight wrote files: %v %v", entries, err)
	}
}

func TestConcurrentEnrollmentNoClobber(t *testing.T) {
	useStateHome(t)
	const attempts = 12
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- InstallNewAccount("new", &oauth2.Token{AccessToken: "synthetic"})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful competing installs = %d, want 1", winners)
	}
	path, _ := AccountTokenPath("new")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallNewAccount("new", &oauth2.Token{AccessToken: "replacement"}); err == nil {
		t.Fatal("replaced existing token")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("protected token changed")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token permissions: %v", info.Mode())
	}
	info, _ = os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("profile permissions: %v", info.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 || entries[0].Name() != "token.json" {
		t.Fatalf("temporary files remain: %v", entries)
	}
	if names, err := Accounts(); err != nil || !reflect.DeepEqual(names, []string{"new"}) {
		t.Fatalf("discovery: %v %v", names, err)
	}
	if email, err := AccountEmail("new"); err != nil || email != "" {
		t.Fatalf("profile mistaken for identity: %q %v", email, err)
	}
}

func TestConcurrentDistinctEnrollmentPreservesRegistry(t *testing.T) {
	useStateHome(t)
	if err := RegisterAccount("protected", "human@example.test"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, name := range []string{"one", "two", "three"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := InstallNewAccount(name, &oauth2.Token{AccessToken: "synthetic"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	names, err := RegisteredAccounts()
	if err != nil || !reflect.DeepEqual(names, []string{"one", "protected", "three", "two"}) {
		t.Fatalf("registry: %v %v", names, err)
	}
	if email, _ := AccountEmail("protected"); email != "human@example.test" {
		t.Fatal("protected registry entry changed")
	}
}

func TestTokenDestinationAppearingAfterPreflight(t *testing.T) {
	root := useStateHome(t)
	if err := CheckNewAccount("new"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "docket", "accounts", "new", "token.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallNewAccount("new", &oauth2.Token{AccessToken: "synthetic"}); err == nil {
		t.Fatal("accepted profile appearing after consent")
	}
	// Exercise the final publication primitive independently of directory claim.
	if installed, err := installNewToken(path, &oauth2.Token{AccessToken: "synthetic"}); err == nil || installed {
		t.Fatalf("clobbered destination: %v %v", installed, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "protected" {
		t.Fatal("protected destination changed")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestEnrollmentRegistrationFailurePreservesInstalledToken(t *testing.T) {
	useStateHome(t)
	registry, _ := RegistryPath()
	err := installNewAccount("new", &oauth2.Token{AccessToken: "synthetic"}, func(path string, tok *oauth2.Token) (bool, error) {
		installed, err := installNewToken(path, tok)
		if err != nil {
			return installed, err
		}
		// Force registry rename failure after the token has been published.
		if err := os.Mkdir(registry, 0o700); err != nil {
			return installed, err
		}
		return installed, nil
	})
	if err == nil || !strings.Contains(err.Error(), "token installed") || !strings.Contains(err.Error(), "human recovery") {
		t.Fatalf("partial commit not reported: %v", err)
	}
	path, _ := AccountTokenPath("new")
	tok, err := readToken(path)
	if err != nil || tok.AccessToken != "synthetic" {
		t.Fatalf("installed token lost: %v", err)
	}
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}
	if err := InstallNewAccount("new", &oauth2.Token{AccessToken: "replacement"}); err == nil {
		t.Fatal("partial success overwritten on retry")
	}
	registryData, err := readRegistry(registry)
	if err != nil || len(registryData.Accounts) != 0 {
		t.Fatalf("unexpected registry: %v %v", registryData, err)
	}
}

func TestEnrollmentTokenDurabilityFailureReportsPartialCommit(t *testing.T) {
	useStateHome(t)
	err := installNewAccount("new", &oauth2.Token{AccessToken: "synthetic"}, func(path string, tok *oauth2.Token) (bool, error) {
		installed, err := installNewToken(path, tok)
		if err != nil {
			return installed, err
		}
		return installed, errors.New("synthetic directory sync failure")
	})
	if err == nil || !strings.Contains(err.Error(), "token preserved") {
		t.Fatalf("partial commit: %v", err)
	}
	path, _ := AccountTokenPath("new")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("token deleted after durability failure")
	}
}
