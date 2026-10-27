package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
	"golang.org/x/oauth2"
)

const agentAuthGrants = `[permissions.defaults]
"auth.login" = true
"auth.remove" = true
"auth.import" = true
"auth.export" = true
`

func snapshotAuthState(t *testing.T) map[string]string {
	t.Helper()
	files := make(map[string]string)
	for _, root := range []string{os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_CONFIG_HOME")} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files[path] = string(data) + " mode=" + info.Mode().String()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func TestAgentLoginRejectsBeforeOAuthOrTokenIO(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	t.Setenv("DOCKET_AGENT_AUTH", "1")
	forbidNetwork(t)
	if err := auth.RegisterAccount("registry_only", ""); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "docket", "accounts")
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(os.Getenv("XDG_STATE_HOME"), "docket", "token.json")
	if err := os.WriteFile(legacy, []byte("synthetic invalid legacy credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotAuthState(t)
	for _, command := range []string{"login", "add"} {
		for _, name := range []string{"", "default", "work", "registry_only", "empty", "dangling", "../escape"} {
			args := []string{"auth", command, "--port=0"}
			if name != "" {
				args = append(args, "--account", name)
			}
			output, code := captureStdout(t, func() int { return run(args) })
			if code != out.ExitUsage || !strings.Contains(output, "ACCOUNT_SELECTION") {
				t.Fatalf("%v: %d %s", args, code, output)
			}
		}
		for _, selector := range [][]string{{"--account", "new", "--account=other"}, {"--account"}, {"--account="}} {
			args := append([]string{"auth", command}, selector...)
			output, code := captureStdout(t, func() int { return run(args) })
			if code != out.ExitUsage || !strings.Contains(output, "USAGE_ERROR") {
				t.Fatalf("%v: %d %s", args, code, output)
			}
		}
		// Selector-looking port data must not become the explicit account.
		output, code := captureStdout(t, func() int { return run([]string{"auth", command, "--port", "--account=new"}) })
		if code != out.ExitUsage || !strings.Contains(output, "ACCOUNT_SELECTION") {
			t.Fatalf("selector stolen: %d %s", code, output)
		}
	}
	if after := snapshotAuthState(t); !reflect.DeepEqual(before, after) {
		t.Fatal("denied enrollment changed protected state")
	}
}

func TestAgentLoginNewProfileAndConcurrentConsent(t *testing.T) {
	for _, command := range []string{"login", "add"} {
		t.Run(command, func(t *testing.T) {
			permissionSetup(t, agentAuthGrants)
			t.Setenv("DOCKET_AGENT_AUTH", "1")
			forbidNetwork(t)
			for _, args := range [][]string{
				{"--account", "new", "auth", command}, {"auth", "--account=new", command}, {"auth", command, "--account", "new"},
			} {
				filtered, requested, err := extractAccountFlag(args)
				if err != nil {
					t.Fatal(err)
				}
				ctx := context.WithValue(t.Context(), accountContextKey{}, requested)
				ctx, code := authorizeCommand(ctx, "auth", filtered[1:])
				if code != out.ExitOK || accountFromContext(ctx) != "new" {
					t.Fatalf("preflight: %d", code)
				}
			}
			ctx := context.WithValue(t.Context(), accountContextKey{}, "new")
			ctx, code := authorizeCommand(ctx, "auth", []string{command})
			if code != out.ExitOK {
				t.Fatal(code)
			}
			calls := 0
			fakeConsent := func(context.Context, auth.Provider, int) (*oauth2.Token, error) {
				calls++
				// Simulate a competing login completing during human consent.
				if err := auth.InstallNewAccount("new", &oauth2.Token{AccessToken: "winner"}); err != nil {
					t.Fatal(err)
				}
				return &oauth2.Token{AccessToken: "loser"}, nil
			}
			output, code := captureStdout(t, func() int { return loginWith(ctx, nil, fakeConsent) })
			if code != out.ExitError || !strings.Contains(output, "ACCOUNT_INSTALL") || calls != 1 {
				t.Fatalf("competing consent: %d %s calls=%d", code, output, calls)
			}
			path, _ := auth.AccountTokenPath("new")
			data, _ := os.ReadFile(path)
			if !strings.Contains(string(data), "winner") || strings.Contains(string(data), "loser") {
				t.Fatal("competing login overwrote winner")
			}
			ctx = context.WithValue(t.Context(), accountContextKey{}, "brand_new")
			ctx, code = authorizeCommand(ctx, "auth", []string{command})
			if code != out.ExitOK {
				t.Fatal(code)
			}
			output, code = captureStdout(t, func() int {
				return loginWith(ctx, nil, func(context.Context, auth.Provider, int) (*oauth2.Token, error) {
					return &oauth2.Token{AccessToken: "synthetic-private-token"}, nil
				})
			})
			if code != 0 || !strings.Contains(output, `"brand_new"`) || strings.Contains(output, "synthetic-private-token") {
				t.Fatalf("new login: %d %s", code, output)
			}
			if resolved, err := auth.ResolveAccount("brand_new"); err != nil || resolved != "brand_new" {
				t.Fatalf("new profile unavailable to normal context: %q %v", resolved, err)
			}
		})
	}
}

func TestAgentAuthImportExportAlwaysDenied(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	t.Setenv("DOCKET_AGENT_AUTH", "1")
	forbidNetwork(t)
	before := snapshotAuthState(t)
	for _, command := range []string{"import", "export"} {
		assertDenial(t, []string{"auth", command, "--account=work"}, "work", "auth."+command, "PERMISSION_DENIED")
	}
	if !reflect.DeepEqual(before, snapshotAuthState(t)) {
		t.Fatal("denied transfer modified state")
	}
}

func TestAgentRemovalRequiresExplicitConfirmationAndPreservesProtectedState(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	t.Setenv("DOCKET_AGENT_AUTH", "1")
	forbidNetwork(t)
	before := snapshotAuthState(t)
	for _, flags := range [][]string{nil, {"--confirm=false"}, {"--confirm=false", "extra"}, {"--unknown"}} {
		args := append([]string{"auth", "remove", "--account=default"}, flags...)
		output, code := captureStdout(t, func() int { return run(args) })
		if code == out.ExitOK {
			t.Fatalf("unconfirmed remove succeeded: %s", output)
		}
		if len(flags) == 0 || reflect.DeepEqual(flags, []string{"--confirm=false"}) {
			if code != out.ExitConfirmMissing || !strings.Contains(output, "CONFIRM_REQUIRED") {
				t.Fatalf("unconfirmed: %d %s", code, output)
			}
		}
		if !reflect.DeepEqual(before, snapshotAuthState(t)) {
			t.Fatalf("unconfirmed removal changed state: %v", flags)
		}
	}
	output, code := captureStdout(t, func() int { return run([]string{"auth", "remove", "--confirm"}) })
	if code != out.ExitUsage || !strings.Contains(output, "USAGE_ERROR") {
		t.Fatalf("implicit remove: %d %s", code, output)
	}
	if !reflect.DeepEqual(before, snapshotAuthState(t)) {
		t.Fatal("implicit removal changed state")
	}
	// A confirmed removal still cannot escape account read-only policy.
	cfgPath, _ := auth.ConfigPath()
	if err := os.WriteFile(cfgPath, []byte("read_only_accounts = [\"work\"]\n[provider]\nclient_id=\"test\"\n"+agentAuthGrants), 0o600); err != nil {
		t.Fatal(err)
	}
	before = snapshotAuthState(t)
	assertDenial(t, []string{"auth", "remove", "--account=work", "--confirm"}, "work", "auth.remove", "WRITES_DISABLED")
	if !reflect.DeepEqual(before, snapshotAuthState(t)) {
		t.Fatal("protected removal changed state")
	}
	output, code = captureStdout(t, func() int { return run([]string{"auth", "remove", "--account=default", "--confirm"}) })
	if code != 0 || !strings.Contains(output, "removed") {
		t.Fatalf("confirmed remove: %d %s", code, output)
	}
	if _, err := auth.ResolveAccount("default"); err == nil {
		t.Fatal("removed profile still registered")
	}
	if _, err := auth.ResolveAccount("work"); err != nil {
		t.Fatal("unrelated protected profile lost")
	}
}

func TestAgentAuthNeverOverridesCeilings(t *testing.T) {
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
		t.Run(env, func(t *testing.T) {
			permissionSetup(t, agentAuthGrants)
			t.Setenv("DOCKET_AGENT_AUTH", "1")
			t.Setenv(env, "1")
			forbidNetwork(t)
			before := snapshotAuthState(t)
			assertDenial(t, []string{"auth", "login", "--account=new", "--port=0"}, "new", "auth.login", "WRITES_DISABLED")
			assertDenial(t, []string{"auth", "remove", "--account=work", "--confirm"}, "work", "auth.remove", "WRITES_DISABLED")
			if !reflect.DeepEqual(before, snapshotAuthState(t)) {
				t.Fatal("ceiling denial modified state")
			}
		})
	}
}

func TestUnguardedHumanReauthenticationAndRemovalCompatible(t *testing.T) {
	permissionSetup(t, agentAuthGrants)
	forbidNetwork(t)
	ctx := context.WithValue(t.Context(), accountContextKey{}, "work")
	ctx, code := authorizeCommand(ctx, "auth", []string{"login"})
	if code != 0 {
		t.Fatal(code)
	}
	output, code := captureStdout(t, func() int {
		return loginWith(ctx, nil, func(context.Context, auth.Provider, int) (*oauth2.Token, error) {
			return &oauth2.Token{AccessToken: "synthetic-replacement"}, nil
		})
	})
	if code != 0 || strings.Contains(output, "synthetic-replacement") {
		t.Fatalf("human reauth: %d %s", code, output)
	}
	output, code = captureStdout(t, func() int { return run([]string{"auth", "remove", "--account=work"}) })
	if code != 0 {
		t.Fatalf("human removal: %d %s", code, output)
	}
}
