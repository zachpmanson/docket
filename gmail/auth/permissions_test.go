package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func noEnv(string) string { return "" }

func TestEveryOperationIndependentPerAccount(t *testing.T) {
	for _, name := range OperationNames() {
		for _, account := range []string{"default", "work"} {
			t.Run(account+"/"+name, func(t *testing.T) {
				// An explicit account grant/denial changes exactly one bit, not
				// the other account or any other operation (including other reads).
				for _, allow := range []bool{false, true} {
					defaults := make(map[string]bool)
					for _, other := range OperationNames() {
						defaults[other] = !allow
					}
					cfg := &Config{Permissions: Permissions{Defaults: defaults,
						Accounts: map[string]map[string]bool{account: {name: allow}}}}
					for _, target := range []string{"default", "work"} {
						for _, other := range OperationNames() {
							want := !allow
							if target == account && other == name {
								want = allow
							}
							if err := cfg.CheckPermission(target, other, noEnv); (err == nil) != want {
								t.Fatalf("%s %s allowed=%v want=%v", target, other, err == nil, want)
							}
						}
					}
				}
			})
		}
	}
}

func TestPermissionSecureDefaultsAndUpperBounds(t *testing.T) {
	for _, name := range OperationNames() {
		op := operations[name]
		cfg := &Config{}
		want := !op.write && !op.credential
		if err := cfg.CheckPermission("default", name, noEnv); (err == nil) != want {
			t.Errorf("legacy default %s: %v", name, err)
		}
		cfg.Permissions.Defaults = map[string]bool{name: true}
		for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY"} {
			getenv := func(key string) string {
				if key == env {
					return "0" // any nonempty value is a kill switch
				}
				return ""
			}
			blocked := (op.write && (env == "DOCKET_READONLY" ||
				(env == "DOCKET_MAIL_READONLY" && strings.HasPrefix(name, "mail.")) ||
				(env == "DOCKET_CAL_READONLY" && strings.HasPrefix(name, "cal.")))) || op.credential
			err := cfg.CheckPermission("default", name, getenv)
			if (err != nil) != blocked {
				t.Errorf("%s %s blocked=%v, want=%v", env, name, err != nil, blocked)
			}
			if blocked && err.(*PermissionError).Code != "WRITES_DISABLED" {
				t.Errorf("legacy error code changed: %v", err)
			}
		}
		ownedOnly := func(key string) string {
			if key == "DOCKET_CAL_OWN_EVENTS_ONLY" {
				return "1"
			}
			return ""
		}
		if err := cfg.CheckPermission("default", name, ownedOnly); (err != nil) != op.credential {
			t.Errorf("owned-events credential ceiling %s: %v", name, err)
		}
		cfg.ReadOnlyAccounts = []string{"work"}
		for _, account := range []string{"default", "work"} {
			blocked := account == "work" && (op.write || op.credential)
			if err := cfg.CheckPermission(account, name, noEnv); (err != nil) != blocked {
				t.Errorf("read_only_accounts %s %s: %v", account, name, err)
			}
		}
	}
	if err := (&Config{}).CheckPermission("default", "mail.future", noEnv); err == nil {
		t.Fatal("unknown operation allowed")
	}
}

func TestReceiptPermissionExample(t *testing.T) {
	cfg := &Config{ReadOnlyAccounts: []string{"work"}, Permissions: Permissions{
		Accounts: map[string]map[string]bool{"default": {"mail.label": true}},
	}}
	for _, account := range []string{"default", "work"} {
		for _, command := range []string{"search", "list", "read", "thread", "attachment", "label", "send", "reply"} {
			want := command != "send" && command != "reply" && (command != "label" || account == "default")
			if err := cfg.CheckPermission(account, "mail."+command, noEnv); (err == nil) != want {
				t.Errorf("%s %s: %v", account, command, err)
			}
		}
	}
}

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	useStateHome(t)
	path, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPermissionConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"unknown operation", "[permissions.defaults]\n\"mail.reed\" = true"},
		{"unknown section", "[permissions.default]\n\"mail.read\" = false"},
		{"unquoted operation", "[permissions.defaults]\nmail.read = true"},
		{"bad bool", "[permissions.defaults]\n\"mail.read\" = \"true\""},
		{"bad bool integer", "[permissions.defaults]\n\"mail.read\" = 1"},
		{"bad account", "[permissions.accounts.\"../escape\"]\n\"mail.read\" = true"},
		{"bad readonly account", "read_only_accounts = [\"../escape\"]"},
		{"duplicate key", "[permissions.defaults]\n\"mail.read\" = true\n\"mail.read\" = false"},
		{"malformed", "[permissions"},
		{"unknown provider", "[provider]\nclient_id = \"test\"\nclient_secert = \"typo\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeTestConfig(t, tc.text)
			if _, err := LoadConfig(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestLegacyConfigPreservedAndJSONNotUsed(t *testing.T) {
	text := `read_only_accounts = ["work"]
default_calendar = "destination"
[provider]
client_id = "legacy"
client_secret = "legacy-secret"
scopes = ["email"]
`
	path := writeTestConfig(t, text)
	// No format migration: config.json cannot silently override the immutable
	// TOML policy, nor is it imported or rewritten by this release.
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), []byte(`{"permissions":{"defaults":{"mail.send":true}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultCalendar != "destination" || cfg.Provider.ClientID != "legacy" || cfg.Provider.ClientSecret != "legacy-secret" || len(cfg.Provider.Scopes) != 1 || !cfg.AccountReadOnly("work") {
		t.Fatalf("legacy settings changed: %+v", cfg)
	}
	if err := cfg.CheckPermission("default", "mail.send", noEnv); err == nil {
		t.Fatal("legacy config or stray JSON enabled writes")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != text {
		t.Fatalf("legacy file rewritten: %s %v", got, err)
	}
}

func TestConfigPermissionInheritance(t *testing.T) {
	writeTestConfig(t, `[provider]
client_id = "test"
[permissions.defaults]
"mail.read" = false
"mail.label" = true
[permissions.accounts.default]
"mail.read" = true
[permissions.accounts.work]
"mail.label" = false
`)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		account, operation string
		allowed            bool
	}{
		{"default", "mail.read", true}, {"default", "mail.label", true},
		{"work", "mail.read", false}, {"work", "mail.label", false},
		{"other", "mail.read", false}, {"other", "mail.label", true},
	} {
		if err := cfg.CheckPermission(tc.account, tc.operation, noEnv); (err == nil) != tc.allowed {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestAccountDiscoveryDoesNotMigrateCredentials(t *testing.T) {
	root := useStateHome(t)
	legacy := filepath.Join(root, "docket", "token.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		t.Fatal(err)
	}
	// Invalid JSON proves discovery never parses the legacy credential.
	if err := os.WriteFile(legacy, []byte("not a token"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveAccount(""); err != nil || got != "default" {
		t.Fatalf("discovery read token: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "docket", "accounts")); !os.IsNotExist(err) {
		t.Fatalf("discovery migrated token: %v", err)
	}
}
