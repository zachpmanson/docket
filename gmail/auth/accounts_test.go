package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func useStateHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	return root
}

func TestLegacyTokenIsDefaultAccount(t *testing.T) {
	root := useStateHome(t)
	legacy := filepath.Join(root, "docket", "token.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"access_token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := AccountTokenPath("default")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(root, "docket", "accounts", "default", "token.json")
	if path != wantPath {
		t.Fatalf("default token path = %q, want migrated path %q", path, wantPath)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy token was not preserved after migration: %v", err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(migrated), "old") {
		t.Fatalf("migrated token = %s, %v", migrated, err)
	}
	accounts, err := Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(accounts, []string{"default"}) {
		t.Fatalf("accounts = %v, want [default]", accounts)
	}
	account, err := ResolveAccount("")
	if err != nil || account != "default" {
		t.Fatalf("ResolveAccount() = %q, %v; want default, nil", account, err)
	}
}

func TestResolveAccountSelectionPolicy(t *testing.T) {
	root := useStateHome(t)
	if got, err := ResolveAccount(""); err != nil || got != "default" {
		t.Fatalf("zero profiles: got %q, %v", got, err)
	}
	if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"a"}`), "one"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveAccount(""); err != nil || got != "one" {
		t.Fatalf("one profile: got %q, %v", got, err)
	}
	if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"b"}`), "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveAccount(""); err == nil || !strings.Contains(err.Error(), "--account is required") {
		t.Fatalf("multiple profiles without selection: got %v", err)
	}
	if got, err := ResolveAccount("two"); err != nil || got != "two" {
		t.Fatalf("explicit selection: got %q, %v", got, err)
	}
	if _, err := ResolveAccount("missing"); err == nil {
		t.Fatal("unknown profile accepted")
	}
	if got, err := AccountTokenPath("one"); err != nil || got != filepath.Join(root, "docket", "accounts", "one", "token.json") {
		t.Fatalf("profile path = %q, %v", got, err)
	}
}

func TestLegacyTokenRefreshesDefaultWhenReinstalled(t *testing.T) {
	root := useStateHome(t)
	legacy := filepath.Join(root, "docket", "token.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"access_token":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := AccountTokenPath("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte(`{"access_token":"rotated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A human reinstalling a rotated token at the legacy path makes it newer
	// than the migrated profile copy; the copy must follow.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(legacy, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := AccountTokenPath("default"); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(migrated), "rotated") {
		t.Fatalf("default profile not refreshed from newer legacy token: %s, %v", migrated, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy token removed during migration: %v", err)
	}
}

func TestSelectionErrorsAreTyped(t *testing.T) {
	useStateHome(t)
	if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"a"}`), "one"); err != nil {
		t.Fatal(err)
	}
	if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"b"}`), "two"); err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{"", "missing", "../escape"} {
		_, err := ResolveAccount(requested)
		var selection *SelectionError
		if !errors.As(err, &selection) {
			t.Errorf("ResolveAccount(%q) error = %v, want SelectionError", requested, err)
		}
	}
	_, err := TokenSourceForAccount(context.Background(), &Config{}, "")
	var selection *SelectionError
	if !errors.As(err, &selection) {
		t.Errorf("ambiguous TokenSourceForAccount error = %v, want SelectionError", err)
	}
	if _, err := ResolveAccount("one"); err != nil {
		t.Errorf("explicit selection failed: %v", err)
	}
}

func TestRemoveAccountRemovesNamedProfileOnly(t *testing.T) {
	useStateHome(t)
	for _, account := range []string{"default", "keep", "remove"} {
		if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"token"}`), account); err != nil {
			t.Fatal(err)
		}
	}
	removedPath, err := AccountTokenPath("remove")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(removedPath+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAccount("remove"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(removedPath); !os.IsNotExist(err) {
		t.Fatalf("removed token still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(removedPath)); !os.IsNotExist(err) {
		t.Fatalf("removed profile directory still exists: %v", err)
	}
	accounts, err := Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(accounts, []string{"default", "keep"}) {
		t.Fatalf("accounts after removal = %v", accounts)
	}
	defaultPath, _ := AccountTokenPath("default")
	if _, err := os.Stat(defaultPath); err != nil {
		t.Fatalf("legacy default token removed: %v", err)
	}
}

func TestImportExportAreProfileIsolated(t *testing.T) {
	useStateHome(t)
	first := `{"access_token":"token-one","refresh_token":"refresh-one"}`
	second := `{"access_token":"token-two","refresh_token":"refresh-two"}`
	if err := ImportTokenForAccount(strings.NewReader(first), "one"); err != nil {
		t.Fatal(err)
	}
	if err := ImportTokenForAccount(strings.NewReader(second), "two"); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := ExportTokenForAccount(&got, "one"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.String(), "token-one") || strings.Contains(got.String(), "token-two") {
		t.Fatalf("export for one contains wrong token: %s", got.String())
	}
}

func TestRefreshPersistsOnlySelectedAccount(t *testing.T) {
	useStateHome(t)
	first := `{"access_token":"old-one","refresh_token":"refresh-one","expiry":"2000-01-01T00:00:00Z"}`
	second := `{"access_token":"token-two","refresh_token":"refresh-two"}`
	if err := ImportTokenForAccount(strings.NewReader(first), "one"); err != nil {
		t.Fatal(err)
	}
	if err := ImportTokenForAccount(strings.NewReader(second), "two"); err != nil {
		t.Fatal(err)
	}

	var refreshToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		refreshToken = r.Form.Get("refresh_token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-one","refresh_token":"rotated-one","token_type":"Bearer","expires_in":3600}`))
	}))
	defer server.Close()

	src, err := TokenSourceForAccount(context.Background(), &Config{Provider: Provider{
		ClientID: "client", ClientSecret: "secret", TokenURL: server.URL,
	}}, "one")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "new-one" || tok.RefreshToken != "rotated-one" || tok.Expiry.Before(time.Now()) {
		t.Fatalf("refreshed token = %#v", tok)
	}
	if refreshToken != "refresh-one" {
		t.Fatalf("refresh token sent = %q, want refresh-one", refreshToken)
	}
	var firstAfter, secondAfter bytes.Buffer
	if err := ExportTokenForAccount(&firstAfter, "one"); err != nil {
		t.Fatal(err)
	}
	if err := ExportTokenForAccount(&secondAfter, "two"); err != nil {
		t.Fatal(err)
	}
	var persisted oauth2.Token
	if err := json.Unmarshal(firstAfter.Bytes(), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != "new-one" || persisted.RefreshToken != "rotated-one" {
		t.Fatalf("selected profile did not persist refresh: %#v", persisted)
	}
	if !strings.Contains(secondAfter.String(), "token-two") || !strings.Contains(secondAfter.String(), "refresh-two") {
		t.Fatalf("other profile changed: %s", secondAfter.String())
	}
}

func TestAccountRegistryAtomicMetadata(t *testing.T) {
	useStateHome(t)
	if err := RegisterAccount("work", "work@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := RegisterAccount("work", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := AccountEmail("work"); err != nil || got != "work@example.test" {
		t.Fatalf("AccountEmail = %q, %v", got, err)
	}
	path, err := RegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("registry permissions = %04o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("registry directory permissions = %04o, want 0700", dirInfo.Mode().Perm())
	}
	if err := ImportTokenForAccount(strings.NewReader(`{"access_token":"x"}`), "work"); err != nil {
		t.Fatal(err)
	}
	accounts, err := Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(accounts, []string{"work"}) {
		t.Fatalf("Accounts() = %v", accounts)
	}
}

func TestAccountNameValidation(t *testing.T) {
	for _, name := range []string{"a", "work-account_2", "default"} {
		if err := ValidateAccountName(name); err != nil {
			t.Errorf("ValidateAccountName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "../escape", "a/b", "-bad", strings.Repeat("x", 65)} {
		if err := ValidateAccountName(name); err == nil {
			t.Errorf("ValidateAccountName(%q) accepted invalid name", name)
		}
	}
}
