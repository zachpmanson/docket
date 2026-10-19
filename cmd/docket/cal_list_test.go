package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zachpmanson/docket/gmail/auth"
	"github.com/zachpmanson/docket/internal/out"
)

type calendarListTransport func(*http.Request) (*http.Response, error)

func (f calendarListTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func setupCalendarListAccounts(t *testing.T, names ...string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	for _, name := range names {
		if err := auth.ImportTokenForAccount(strings.NewReader(fmt.Sprintf(`{"access_token":%q}`, name+"-token")), name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCalList(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"exact IDs and names", `<d:response><d:href>/caldav/v2/reader%40example.com/events/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Personal &amp; home</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/caldav/v2/opaque%23group%40group.calendar.google.com/events/</d:href><d:propstat><d:prop><d:resourcetype><c:calendar/></d:resourcetype><d:displayname>Shared 工作</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/caldav/v2/holidays%40virtual/events/</d:href><d:propstat><d:prop><d:resourcetype><c:calendar/></d:resourcetype><d:displayname>Holidays</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, 207, `[{"id":"reader@example.com","name":"Personal & home"},{"id":"opaque#group@group.calendar.google.com","name":"Shared 工作"},{"id":"holidays@virtual","name":"Holidays"}]`},
		{"empty", "", 207, `[]`},
		{"API error", "unavailable", 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupCalendarListAccounts(t, "one", "two")
			// Listing is permitted for read-only accounts and must discover the
			// account home, not the configured default destination calendar.
			config := "read_only_accounts = [\"two\"]\ndefault_calendar = \"destination@example.com\"\n[provider]\nclient_id = \"test-client\"\n"
			configPath, err := auth.ConfigPath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			old := http.DefaultTransport
			defer func() { http.DefaultTransport = old }()
			calls := 0
			http.DefaultTransport = calendarListTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				status, body := http.StatusOK, `{"email":"reader@example.com"}`
				switch r.URL.Host {
				case "www.googleapis.com":
					if r.Method != "GET" || r.URL.Query().Get("access_token") != "two-token" {
						t.Fatalf("wrong identity request: %s %s", r.Method, r.URL)
					}
				case "apidata.googleusercontent.com":
					if r.Method != "PROPFIND" || r.URL.Path != "/caldav/v2/reader@example.com/" || r.Header.Get("Depth") != "1" || r.Header.Get("Authorization") != "Bearer two-token" {
						t.Fatalf("wrong discovery request: %s %s %v", r.Method, r.URL, r.Header)
					}
					status, body = tc.status, tc.body
					if status == 207 {
						body = `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">` + body + `</d:multistatus>`
					}
				default:
					t.Fatalf("unexpected network request: %s", r.URL)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			output, code := captureStdout(t, func() int { return run([]string{"cal", "list", "--account", "two"}) })
			var result struct {
				OK    bool            `json:"ok"`
				Data  json.RawMessage `json:"data"`
				Error *out.Error      `json:"error"`
			}
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatalf("invalid envelope %q: %v", output, err)
			}
			if calls != 2 {
				t.Fatalf("requests = %d, want identity + discovery only; output=%s", calls, output)
			}
			if tc.status != 207 {
				if code != out.ExitError || result.OK || result.Error == nil || result.Error.Code != "CALENDAR_API_ERROR" || !result.Error.Retryable {
					t.Fatalf("error result: code=%d %s", code, output)
				}
				return
			}
			var got, want any
			if err := json.Unmarshal(result.Data, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if code != out.ExitOK || !result.OK || result.Error != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("code=%d output=%s want=%s", code, output, tc.want)
			}
			t.Logf("mock calendar-list output: %s", strings.TrimSpace(output))
		})
	}
}

func TestCalListUsage(t *testing.T) {
	setupCalendarListAccounts(t, "one", "two")
	for _, args := range [][]string{{"cal", "list"}, {"cal", "list", "--account", "missing"}, {"cal", "list", "extra"}, {"cal", "list", "--tz", "Australia/Melbourne"}, {"cal", "list", "--confirm"}, {"cal", "list", "--help"}} {
		output, code := captureStdout(t, func() int { return run(args) })
		var env out.Envelope
		if err := json.Unmarshal([]byte(output), &env); err != nil {
			t.Fatal(err)
		}
		if code != out.ExitUsage || env.OK || env.Error == nil {
			t.Fatalf("%v: code=%d %s", args, code, output)
		}
	}
}
