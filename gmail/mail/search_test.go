package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// fakeGmail is a stand-in for the Gmail REST API: enough of messages.list,
// messages.get and labels.list to exercise argument handling and response
// plumbing without a network round-trip or a live mailbox. The Gmail client
// is a concrete type, so the seam is its endpoint rather than an interface.
type fakeGmail struct {
	server *httptest.Server

	// requests records the query string of every messages.list call, so a
	// test can assert what was actually sent (maxResults, pageToken, q).
	requests []string

	// pages keyed by the pageToken that requests them; "" is the first page.
	pages map[string]listPage

	// bodies keyed by message id, returned by messages.get for format=full.
	bodies map[string]string

	// payloads keyed by message id, for the multipart shapes a single
	// text/plain body cannot express. Takes precedence over bodies.
	payloads map[string]*gmail.MessagePart

	// threads keyed by thread id, listing the message ids in the thread.
	threads map[string][]string

	// attachments keyed by attachment id, holding the decoded bytes the
	// content endpoint hands back. Values are a handful of bytes: a fixture
	// is here to prove the plumbing, not to carry a real file.
	attachments map[string][]byte

	// attachmentStatus keyed by attachment id, overriding the response with
	// an HTTP status, so a test can drive the failure classes a bulk fetcher
	// must keep apart.
	attachmentStatus map[string]int

	// attachmentRequests records the path of every attachments.get call, so a
	// test can assert that a refused fetch cost no download.
	attachmentRequests []string
}

type listPage struct {
	ids       []string
	nextToken string
}

func newFakeGmail(t *testing.T, pages map[string]listPage) *fakeGmail {
	t.Helper()
	f := &fakeGmail{
		pages:            pages,
		bodies:           map[string]string{},
		payloads:         map[string]*gmail.MessagePart{},
		threads:          map[string][]string{},
		attachments:      map[string][]byte{},
		attachmentStatus: map[string]int{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.URL.RawQuery)
		page, ok := f.pages[r.URL.Query().Get("pageToken")]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"no such page"}}`, http.StatusNotFound)
			return
		}
		resp := gmail.ListMessagesResponse{NextPageToken: page.nextToken}
		for _, id := range page.ids {
			resp.Messages = append(resp.Messages, &gmail.Message{Id: id, ThreadId: "t-" + id})
		}
		writeJSON(t, w, resp)
	})

	mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/")
		msg, ok := f.message(id)
		if !ok && r.URL.Query().Get("format") == "full" {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		writeJSON(t, w, msg)
	})

	// Registered alongside the messages subtree; the more specific pattern
	// wins, so a content fetch does not fall through to messages.get.
	mux.HandleFunc("/gmail/v1/users/me/messages/{id}/attachments/{attachmentID}",
		func(w http.ResponseWriter, r *http.Request) {
			f.attachmentRequests = append(f.attachmentRequests, r.URL.Path)
			attID := r.PathValue("attachmentID")
			if status, ok := f.attachmentStatus[attID]; ok {
				http.Error(w, fmt.Sprintf(`{"error":{"code":%d,"message":"driven by the fixture"}}`, status), status)
				return
			}
			data, ok := f.attachments[attID]
			if !ok {
				http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
				return
			}
			writeJSON(t, w, gmail.MessagePartBody{
				AttachmentId: attID,
				Size:         int64(len(data)),
				Data:         base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(data),
			})
		})

	mux.HandleFunc("/gmail/v1/users/me/threads/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/threads/")
		ids, ok := f.threads[id]
		if !ok {
			http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
			return
		}
		resp := gmail.Thread{Id: id}
		for _, mid := range ids {
			msg, _ := f.message(mid)
			// Gmail returns payload bodies only for format=full; a metadata
			// fetch that leaked them would hide a command asking for the
			// wrong format.
			if r.URL.Query().Get("format") != "full" {
				msg.Payload = &gmail.MessagePart{Headers: msg.Payload.Headers}
			}
			resp.Messages = append(resp.Messages, &msg)
		}
		writeJSON(t, w, resp)
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeGmail) service(t *testing.T) *gmail.Service {
	t.Helper()
	svc, err := gmail.NewService(context.Background(),
		option.WithEndpoint(f.server.URL),
		option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("building test gmail client: %v", err)
	}
	return svc
}

// message builds the messages.get response for one id, from an explicit
// payload when the test set one and from a single text/plain body otherwise.
func (f *fakeGmail) message(id string) (gmail.Message, bool) {
	if payload, ok := f.payloads[id]; ok {
		msg := message(id, "")
		msg.Payload = payload
		msg.Payload.Headers = headers(id)
		return msg, true
	}
	body, ok := f.bodies[id]
	return message(id, body), ok
}

// message builds a plausible messages.get response. Every value is invented:
// no real address, subject or body belongs in a fixture.
func message(id, body string) gmail.Message {
	return gmail.Message{
		Id:       id,
		ThreadId: "t-" + id,
		LabelIds: []string{"INBOX"},
		Snippet:  "snippet for " + id,
		Payload: &gmail.MessagePart{
			MimeType: "text/plain",
			Headers:  headers(id),
			Body:     partBody(body),
		},
	}
}

func headers(id string) []*gmail.MessagePartHeader {
	return []*gmail.MessagePartHeader{
		{Name: "From", Value: "Dana Okafor <dana@example.com>"},
		{Name: "To", Value: "ops@example.org"},
		{Name: "Subject", Value: "Re: quarterly widget audit"},
		{Name: "Date", Value: "Mon, 03 Aug 2026 09:14:00 +1000"},
		{Name: "Message-ID", Value: "<" + id + "@mail.example.com>"},
	}
}

func partBody(data string) *gmail.MessagePartBody {
	return &gmail.MessagePartBody{
		Size: int64(len(data)),
		Data: base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(data)),
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encoding fake response: %v", err)
	}
}

func emptyLabels() *LabelCache {
	return &LabelCache{
		idToName: map[string]string{"INBOX": "INBOX"},
		nameToID: map[string]string{"INBOX": "INBOX"},
	}
}

func TestListReportsTheNextPageToken(t *testing.T) {
	// The token is the whole point: without it a caller cannot tell a capped
	// result from a complete one, nor reach what it did not get.
	f := newFakeGmail(t, map[string]listPage{
		"": {ids: []string{"m1", "m2"}, nextToken: "tok-2"},
	})

	got, err := List(context.Background(), f.service(t), emptyLabels(), ListOptions{Query: "after:2026/07/01", Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got.Envelopes) != 2 {
		t.Fatalf("got %d envelopes, want 2", len(got.Envelopes))
	}
	if got.NextPageToken != "tok-2" {
		t.Errorf("NextPageToken = %q, want %q", got.NextPageToken, "tok-2")
	}
}

func TestListLastPageHasNoToken(t *testing.T) {
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})

	got, err := List(context.Background(), f.service(t), emptyLabels(), ListOptions{Query: "x", Limit: 25})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.NextPageToken != "" {
		t.Errorf("NextPageToken = %q on the last page, want empty", got.NextPageToken)
	}
}

func TestListPageTokenAndLimitCompose(t *testing.T) {
	// --limit is the page size, so it applies to every page of a walk; the
	// second call must carry both the token and the same maxResults.
	f := newFakeGmail(t, map[string]listPage{
		"":      {ids: []string{"m1", "m2"}, nextToken: "tok-2"},
		"tok-2": {ids: []string{"m3"}},
	})
	svc := f.service(t)

	first, err := List(context.Background(), svc, emptyLabels(), ListOptions{Query: "q", Limit: 2})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := List(context.Background(), svc, emptyLabels(), ListOptions{Query: "q", Limit: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}

	if len(second.Envelopes) != 1 || second.Envelopes[0].ID != "m3" {
		t.Errorf("second page = %+v, want the one message the first page did not return", second.Envelopes)
	}
	if len(f.requests) != 2 {
		t.Fatalf("made %d list calls, want 2: %v", len(f.requests), f.requests)
	}
	if strings.Contains(f.requests[0], "pageToken") {
		t.Errorf("first call sent a page token: %s", f.requests[0])
	}
	for i, want := range []string{"maxResults=2", "pageToken=tok-2"} {
		if !strings.Contains(f.requests[1], want) {
			t.Errorf("second call %q missing %q (from assertion %d)", f.requests[1], want, i)
		}
	}
}

func TestListRejectsALimitAboveTheMaximum(t *testing.T) {
	// Clamping instead would hand back MaxLimit results looking exactly like
	// a complete answer, which is the failure this whole page contract exists
	// to prevent.
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})

	_, err := List(context.Background(), f.service(t), emptyLabels(), ListOptions{Query: "q", Limit: MaxLimit + 1})
	if !errors.Is(err, ErrLimitTooLarge) {
		t.Fatalf("err = %v, want ErrLimitTooLarge", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxLimit)) {
		t.Errorf("error %q does not name the maximum", err)
	}
	if len(f.requests) != 0 {
		t.Errorf("made %d API calls for a rejected limit, want 0", len(f.requests))
	}
}

func TestListDefaultsAnUnsetLimit(t *testing.T) {
	f := newFakeGmail(t, map[string]listPage{"": {ids: []string{"m1"}}})

	if _, err := List(context.Background(), f.service(t), emptyLabels(), ListOptions{Query: "q"}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := fmt.Sprintf("maxResults=%d", DefaultLimit); !strings.Contains(f.requests[0], want) {
		t.Errorf("request %q missing %q", f.requests[0], want)
	}
}

func TestListEmptyResultIsNotNil(t *testing.T) {
	// An empty JSON array and a null are different answers to a caller; only
	// one of them survives being unmarshalled into a slice without ambiguity.
	f := newFakeGmail(t, map[string]listPage{"": {}})

	got, err := List(context.Background(), f.service(t), emptyLabels(), ListOptions{Query: "q"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.Envelopes == nil {
		t.Errorf("Envelopes is nil, want an empty slice")
	}
	if got.NextPageToken != "" {
		t.Errorf("NextPageToken = %q on an empty result, want empty", got.NextPageToken)
	}
}
