package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
)

func TestParseLoginPort(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{nil, auth.DefaultLoginPort},
		{[]string{"--port", "8081"}, 8081},
		{[]string{"--port=8081"}, 8081},
		{[]string{"--port", "1"}, 1},
		{[]string{"--port", "65535"}, 65535},
	} {
		got, err := parseLoginPort(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("parseLoginPort(%v) = %d, %v; want %d", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"--port", "0"}, {"--port", "-1"}, {"--port", "65536"},
		{"--port", "999999999999999999999999"}, {"--port", "abc"},
		{"--port", "127.0.0.1:8081"}, {"--port"}, {"--port="},
		{"--unknown"}, {"8081"}, {"--port", "8081", "extra"},
	} {
		if _, err := parseLoginPort(args); err == nil {
			t.Errorf("parseLoginPort(%v) accepted invalid arguments", args)
		}
	}
}

func TestLoginPortAccountExtraction(t *testing.T) {
	for _, command := range []string{"login", "add"} {
		for _, args := range [][]string{
			{"auth", command, "--account", "secondary", "--port", "8081"},
			{"auth", command, "--port=8081", "--account=secondary"},
			{"--account", "secondary", "auth", command, "--port", "8081"},
		} {
			filtered, account, err := extractAccountFlag(args)
			if err != nil || account != "secondary" {
				t.Fatalf("extractAccountFlag(%v) = %v, %q, %v", args, filtered, account, err)
			}
			if port, err := parseLoginPort(filtered[2:]); err != nil || port != 8081 {
				t.Fatalf("port after extraction = %d, %v", port, err)
			}
		}
		// A port value that resembles an account selector remains data, not
		// an authorization target, and is then rejected by the port parser.
		args, account, err := extractAccountFlag([]string{"auth", command, "--port", "--account=work", "--account", "secondary"})
		if err != nil || account != "secondary" || !reflect.DeepEqual(args, []string{"auth", command, "--port", "--account=work"}) {
			t.Fatalf("port value stole account selection: %v %q %v", args, account, err)
		}
		if _, err := parseLoginPort(args[2:]); err == nil {
			t.Fatal("account-like port accepted")
		}
	}
}

func TestLoginAliasesRejectInvalidPortBeforeOAuth(t *testing.T) {
	permissionSetup(t, "[permissions.accounts.secondary]\n\"auth.login\" = true\n")
	forbidNetwork(t)
	for _, command := range []string{"login", "add"} {
		for _, flags := range [][]string{{"--port", "0"}, {"--port", "bad"}, {"--port"}, {"--unknown"}, {"extra"}} {
			args := append([]string{"auth", command, "--account", "secondary"}, flags...)
			output, code := captureStdout(t, func() int { return run(args) })
			if code != out.ExitUsage || !strings.Contains(output, "USAGE_ERROR") {
				t.Fatalf("%v: code=%d output=%s", args, code, output)
			}
		}
	}
}

func TestLoginPortDoesNotBypassPreflight(t *testing.T) {
	permissionSetup(t, "[permissions.accounts.secondary]\n\"auth.login\" = false\n")
	forbidNetwork(t)
	for _, command := range []string{"login", "add"} {
		for _, port := range []string{"8081", "invalid"} {
			assertDenial(t, []string{"auth", command, "--port", port, "--account", "secondary"}, "secondary", "auth.login", "PERMISSION_DENIED")
		}
	}
}

func TestLoginPortCannotEscapeLegacyCeilings(t *testing.T) {
	for _, env := range []string{"DOCKET_READONLY", "DOCKET_MAIL_READONLY", "DOCKET_CAL_READONLY", "DOCKET_CAL_OWN_EVENTS_ONLY"} {
		t.Run(env, func(t *testing.T) {
			permissionSetup(t, "[permissions.accounts.secondary]\n\"auth.login\" = true\n")
			t.Setenv(env, "1")
			forbidNetwork(t)
			for _, command := range []string{"login", "add"} {
				assertDenial(t, []string{"auth", command, "--account", "secondary", "--port=8081"}, "secondary", "auth.login", "WRITES_DISABLED")
			}
		})
	}
}
