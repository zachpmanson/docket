package cal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-webdav/caldav"
)

func TestCalendarDiscoveryAndQueryFiltering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.URL.Path != "/caldav/v2/reader@example.com/" || r.Header.Get("Depth") != "1" {
			t.Errorf("unexpected request: %s %s depth=%q", r.Method, r.URL, r.Header.Get("Depth"))
		}
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
		for _, item := range []struct{ path, name string }{
			{"/caldav/v2/reader%40example.com/events/", "Personal"},
			{"/caldav/v2/opaque%23group%40group.calendar.google.com/events/", "Shared &amp; work"},
			{"/caldav/v2/holidays%40virtual/events/", "Holidays"},
			{"/unrelated/path/", "Ignore unrelated"},
			{"/caldav/v2/reader%40example.com/other/", "Ignore non-events"},
		} {
			fmt.Fprintf(w, `<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype><c:calendar/></d:resourcetype><d:displayname>%s</d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, item.path, item.name)
		}
		fmt.Fprint(w, `</d:multistatus>`)
	}))
	defer server.Close()
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []Calendar{{"reader@example.com", "Personal"}, {"opaque#group@group.calendar.google.com", "Shared & work"}, {"holidays@virtual", "Holidays"}}
	got, err := DiscoverCalendars(context.Background(), client, "reader@example.com")
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("discovery = %#v, %v; want %#v", got, err, want)
	}
	got, err = ListCalendars(context.Background(), client, "reader@example.com")
	if err != nil || !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("query calendars = %#v, %v; want %#v", got, err, want[:2])
	}
}

func TestCalendarDiscoveryError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	client, err := caldav.NewClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []func(context.Context, *caldav.Client, string) ([]Calendar, error){DiscoverCalendars, ListCalendars} {
		_, err := list(context.Background(), client, "reader@example.com")
		if err == nil || !strings.Contains(err.Error(), `listing calendars under "/caldav/v2/reader@example.com/"`) {
			t.Fatalf("error = %v", err)
		}
	}
}
