package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
)

func permissionSetup(t *testing.T, policy string) {
	t.Helper()
	setupCalendarListAccounts(t, "default", "work")
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
		t.Setenv(env, "")
	}
	path, err := auth.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[provider]\nclient_id = \"test\"\n"+policy), 0600); err != nil {
		t.Fatal(err)
	}
}

func forbidNetwork(t *testing.T) {
	t.Helper()
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	// Google clones *http.Transport and silently replaces other RoundTripper
	// implementations. Block at dialing so both Gmail and CalDAV are covered.
	http.DefaultTransport = &http.Transport{DialContext: func(_ context.Context, _, address string) (net.Conn, error) {
		t.Errorf("denied command attempted network connection: %s", address)
		return nil, fmt.Errorf("network forbidden")
	}}
}

func assertDenial(t *testing.T, args []string, account, operation, errorCode string) {
	t.Helper()
	output, code := captureStdout(t, func() int { return run(args) })
	var env out.Envelope
	if err := json.Unmarshal([]byte(output), &env); err != nil {
		t.Fatalf("invalid JSON: %s %v", output, err)
	}
	if code != out.ExitConfirmMissing || env.OK || env.Data != nil || env.Error == nil || env.Error.Code != errorCode || env.Error.Retryable || !strings.Contains(env.Error.Message, account) || !strings.Contains(env.Error.Message, operation) {
		t.Fatalf("%v: code=%d output=%s", args, code, output)
	}
}

func TestEveryCommandDeniedBeforeSensitiveIO(t *testing.T) {
	for _, operation := range auth.OperationNames() {
		for _, account := range []string{"default", "work"} {
			t.Run(account+"/"+operation, func(t *testing.T) {
				permissionSetup(t, fmt.Sprintf("[permissions.accounts.%s]\n%q = false\n", account, operation))
				forbidNetwork(t)
				// Poison credentials: reading them would fail before the policy
				// denial. No valid token, real mailbox or outbound request is used.
				path, err := auth.AccountTokenPath(account)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("invalid credential"), 0600); err != nil {
					t.Fatal(err)
				}
				parts := strings.SplitN(operation, ".", 2)
				args := []string{"--account", account, parts[0], parts[1]}
				// A nonexistent --body-file would report usage before permission
				// if any handler were allowed to read/prepare before preflight.
				if operation == "mail.send" {
					args = append(args, "--to", "nobody@example.test", "--subject", "x", "--body-file", "/does-not-exist", "--dry-run")
				} else if operation == "mail.reply" {
					args = append(args, "--id", "m1", "--body-file", "/does-not-exist", "--confirm")
				}
				assertDenial(t, args, account, operation, "PERMISSION_DENIED")
			})
		}
	}
}

func TestLegacyKillSwitchDeniesBeforeFileReadOrPreview(t *testing.T) {
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY"} {
		for _, mode := range []string{"--dry-run", "--confirm"} {
			t.Run(env+mode, func(t *testing.T) {
				permissionSetup(t, "[permissions.accounts.default]\n\"mail.send\" = true\n\"mail.reply\" = true\n")
				t.Setenv(env, "1")
				forbidNetwork(t)
				for _, file := range []string{"/does-not-exist", "-"} {
					assertDenial(t, []string{"mail", "send", "--account", "default", "--to", "nobody@example.test", "--subject", "x", "--body-file", file, mode}, "default", "mail.send", "WRITES_DISABLED")
					assertDenial(t, []string{"mail", "reply", "--account", "default", "--id", "m1", "--body-file", file, mode}, "default", "mail.reply", "WRITES_DISABLED")
				}
			})
		}
	}
}

func TestCredentialManagementCannotEscapeLegacyPolicy(t *testing.T) {
	for _, operation := range []string{"auth.login", "auth.import", "auth.export", "auth.remove"} {
		for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
			t.Run(operation+"/"+env, func(t *testing.T) {
				permissionSetup(t, fmt.Sprintf("[permissions.accounts.default]\n%q = true\n", operation))
				t.Setenv(env, "1")
				forbidNetwork(t)
				parts := strings.SplitN(operation, ".", 2)
				assertDenial(t, []string{"auth", parts[1], "--account", "default"}, "default", operation, "WRITES_DISABLED")
			})
		}
	}
}

func TestInvalidConfigDeniesBeforeProtectedIO(t *testing.T) {
	permissionSetup(t, "[permissions.defaults]\n\"mail.sned\" = true\n")
	forbidNetwork(t)
	for _, args := range [][]string{
		{"mail", "send", "--body-file", "/does-not-exist", "--dry-run"},
		{"auth", "export"}, {"auth", "import"}, {"cal", "list"},
	} {
		output, code := captureStdout(t, func() int { return run(args) })
		if code != out.ExitUsage || !strings.Contains(output, "CONFIG_ERROR") {
			t.Fatalf("%v: %d %s", args, code, output)
		}
	}
}

func TestUnknownOperationsNeverDispatch(t *testing.T) {
	permissionSetup(t, "")
	forbidNetwork(t)
	for _, domain := range []string{"mail", "cal", "auth"} {
		output, code := captureStdout(t, func() int { return run([]string{domain, "future"}) })
		if code != out.ExitUsage || !strings.Contains(output, "unknown") {
			t.Fatalf("%s: %d %s", domain, code, output)
		}
	}
}

func TestDeniedLegacyTokenNotMigrated(t *testing.T) {
	permissionSetup(t, "[permissions.defaults]\n\"mail.read\" = false\n")
	legacy := filepath.Join(os.Getenv("XDG_STATE_HOME"), "docket", "token.json")
	if err := os.WriteFile(legacy, []byte("invalid token"), 0600); err != nil {
		t.Fatal(err)
	}
	defaultPath := filepath.Join(filepath.Dir(legacy), "accounts", "default", "token.json")
	if err := os.Remove(defaultPath); err != nil {
		t.Fatal(err)
	}
	forbidNetwork(t)
	assertDenial(t, []string{"mail", "read", "--account", "default", "--id", "m1"}, "default", "mail.read", "PERMISSION_DENIED")
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("denial migrated legacy credential: %v", err)
	}
}

func TestCLIAccountPlacementAndAmbiguity(t *testing.T) {
	permissionSetup(t, "[permissions.accounts.work]\n\"mail.read\" = false\n")
	forbidNetwork(t)
	for _, args := range [][]string{
		{"--account", "work", "mail", "read"},
		{"mail", "--account", "work", "read"},
		{"mail", "read", "--account", "work"},
		{"mail", "read", "--account=work"},
		{"-account=work", "mail", "read"},
	} {
		assertDenial(t, args, "work", "mail.read", "PERMISSION_DENIED")
	}
	for _, args := range [][]string{
		{"mail", "read"}, {"cal", "list"}, {"auth", "whoami"},
		{"mail", "read", "--account", "missing"},
		{"mail", "read", "--account", "../escape"},
	} {
		output, code := captureStdout(t, func() int { return run(args) })
		if code != out.ExitUsage || !strings.Contains(output, "ACCOUNT_SELECTION") {
			t.Fatalf("%v: %d %s", args, code, output)
		}
	}
}

func TestAccountSelectorIsNotStolenFromFlagValues(t *testing.T) {
	for _, tc := range []struct {
		args, want []string
		account    string
	}{
		{[]string{"mail", "search", "--query", "--account=work"}, []string{"mail", "search", "--query", "--account=work"}, ""},
		{[]string{"mail", "send", "--subject", "--account", "--account", "default"}, []string{"mail", "send", "--subject", "--account"}, "default"},
		{[]string{"mail", "read", "--html", "--account", "work"}, []string{"mail", "read", "--html"}, "work"},
		{[]string{"mail", "read", "--", "--account", "work"}, []string{"mail", "read", "--", "--account", "work"}, ""},
	} {
		args, account, err := extractAccountFlag(tc.args)
		if err != nil || account != tc.account || !reflect.DeepEqual(args, tc.want) {
			t.Fatalf("%v: %v %q %v", tc.args, args, account, err)
		}
	}
}

func TestAuthAliasesAndAccountListPreflight(t *testing.T) {
	permissionSetup(t, "[permissions.accounts.work]\n\"auth.accounts\" = false\n\"auth.login\" = false\n")
	forbidNetwork(t)
	for _, alias := range []string{"list", "accounts"} {
		assertDenial(t, []string{"auth", alias}, "work", "auth.accounts", "PERMISSION_DENIED")
		assertDenial(t, []string{"auth", alias, "--account", "work"}, "work", "auth.accounts", "PERMISSION_DENIED")
	}
	for _, alias := range []string{"login", "add"} {
		assertDenial(t, []string{"auth", alias, "--account", "work"}, "work", "auth.login", "PERMISSION_DENIED")
	}
	// Explicit creation uses the requested new name, never the implicit default.
	assertDenial(t, []string{"auth", "login", "--account", "new"}, "new", "auth.login", "PERMISSION_DENIED")
	assertDenial(t, []string{"auth", "import", "--account", "new"}, "new", "auth.import", "PERMISSION_DENIED")
}

func TestWriteGateConfirmationAndDryRunAllWrites(t *testing.T) {
	for _, operation := range []string{"mail.send", "mail.reply", "mail.label", "cal.create", "cal.update", "cal.delete"} {
		t.Run(operation, func(t *testing.T) {
			permissionSetup(t, fmt.Sprintf("[permissions.accounts.default]\n%q = true\n", operation))
			parts := strings.SplitN(operation, ".", 2)
			ctx := context.WithValue(t.Context(), accountContextKey{}, "default")
			ctx, code := authorizeCommand(ctx, parts[0], parts[1:])
			if code != 0 {
				t.Fatal(code)
			}
			for _, tc := range []struct {
				confirm, dryRun, proceed bool
				code                     int
				contains                 string
			}{
				{false, false, false, out.ExitConfirmMissing, "CONFIRM_REQUIRED"},
				{false, true, false, out.ExitOK, "preview"},
				{true, true, false, out.ExitOK, "preview"},
				{true, false, true, out.ExitOK, ""},
			} {
				var proceed bool
				output, exit := captureStdout(t, func() int {
					var code int
					proceed, code = writeGate(ctx, tc.confirm, tc.dryRun, map[string]string{"safe": "preview"}, "docket "+parts[0]+" "+parts[1]+" --confirm")
					return code
				})
				if proceed != tc.proceed || exit != tc.code || !strings.Contains(output, tc.contains) {
					t.Fatalf("%+v: %v %d %s", tc, proceed, exit, output)
				}
				if !tc.confirm && !tc.dryRun && !strings.Contains(output, "--account") {
					t.Fatal("confirmation hint dropped account")
				}
			}
		})
	}
}

func TestAllowedLabelCLIAndDeniedSend(t *testing.T) {
	permissionSetup(t, "[permissions.accounts.default]\n\"mail.label\" = true\n")
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	mutations := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer default-token" {
			t.Fatalf("wrong account: %v", r.Header)
		}
		body := `{"labels":[{"id":"Label_1","name":"Receipt"}]}`
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/labels"):
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages/m1/modify"):
			mutations++
			body = `{"id":"m1","labelIds":["Label_1"]}`
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/messages/m1"):
			body = `{"id":"m1","labelIds":["Label_1"]}`
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com" // httptest certificate
	// Keep the API hostname while directing every connection to the fake.
	// This also works when the Google client clones the transport.
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	base := []string{"mail", "label", "--account", "default", "--id", "m1", "--add", "Receipt"}
	for _, mode := range []string{"--dry-run", "", "--confirm"} {
		args := append([]string{}, base...)
		if mode != "" {
			args = append(args, mode)
		}
		output, code := captureStdout(t, func() int { return run(args) })
		if mode == "" {
			if code != out.ExitConfirmMissing || !strings.Contains(output, "CONFIRM_REQUIRED") {
				t.Fatalf("%d %s", code, output)
			}
		} else if code != 0 {
			t.Fatalf("%s: %d %s", mode, code, output)
		}
	}
	if mutations != 1 {
		t.Fatalf("mutations=%d want 1", mutations)
	}
	assertDenial(t, []string{"mail", "label", "--account", "work", "--id", "m1", "--add", "Receipt", "--confirm"}, "work", "mail.label", "PERMISSION_DENIED")
	assertDenial(t, []string{"mail", "send", "--account", "default", "--body-file", "/does-not-exist", "--dry-run"}, "default", "mail.send", "PERMISSION_DENIED")
}

func TestCalendarOwnershipStillWinsOverOperationGrant(t *testing.T) {
	for _, command := range []string{"update", "delete"} {
		for _, mode := range []string{"--dry-run", "--confirm"} {
			t.Run(command+mode, func(t *testing.T) {
				permissionSetup(t, fmt.Sprintf("[permissions.accounts.default]\n\"cal.%s\" = true\n", command))
				t.Setenv("DOCKET_CAL_OWN_EVENTS_ONLY", "1")
				old := http.DefaultTransport
				defer func() { http.DefaultTransport = old }()
				http.DefaultTransport = calendarListTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method != "GET" {
						t.Fatalf("ownership refusal performed mutation: %s", r.Method)
					}
					body := `{"email":"reader@example.test"}`
					if strings.HasSuffix(r.URL.Path, ".ics") {
						body = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:test\r\nBEGIN:VEVENT\r\nUID:human\r\nDTSTART:20261022T090000Z\r\nDTEND:20261022T100000Z\r\nSUMMARY:Human\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
					} else if r.URL.Host != "www.googleapis.com" {
						t.Fatalf("unexpected request %s", r.URL)
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/calendar; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})
				args := []string{"cal", command, "--account", "default", "--calendar", "primary", "--id", "human::2026-10-22T09:00:00Z", "--tz", "Australia/Melbourne", mode}
				if command == "update" {
					args = append(args, "--summary", "changed")
				}
				output, code := captureStdout(t, func() int { return run(args) })
				if code != out.ExitConfirmMissing || !strings.Contains(output, "NOT_OWNED") {
					t.Fatalf("%d %s", code, output)
				}
			})
		}
	}
}
