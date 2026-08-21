package out

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// decode round-trips an envelope through the JSON writer, which is where the
// contract is enforced, so these tests exercise the same path a caller reads.
func decode(t *testing.T, env Envelope) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	writeJSON(&buf, env)
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return got
}

func TestFailureAlwaysCarriesAnError(t *testing.T) {
	// A failure envelope built without an error is the shape that leaves a
	// caller unable to tell "rate limited, back off" from "no results".
	got := decode(t, Envelope{OK: false})

	if got["ok"] != false {
		t.Fatalf("ok = %v, want false", got["ok"])
	}
	errObj, ok := got["error"].(map[string]any)
	if !ok {
		t.Fatalf("error = %#v, want a populated object", got["error"])
	}
	if errObj["code"] != unknownErrorCode {
		t.Errorf("error.code = %v, want %q", errObj["code"], unknownErrorCode)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("error.message is empty, want an explanation")
	}
	if _, present := errObj["retryable"]; !present {
		t.Errorf("error.retryable is absent; a client needs it to decide whether to back off")
	}
}

func TestFailurePartialErrorIsCompleted(t *testing.T) {
	got := decode(t, Envelope{OK: false, Error: &Error{Retryable: true}})

	errObj := got["error"].(map[string]any)
	if errObj["code"] != unknownErrorCode {
		t.Errorf("error.code = %v, want %q", errObj["code"], unknownErrorCode)
	}
	if msg, _ := errObj["message"].(string); msg == "" {
		t.Errorf("error.message is empty, want an explanation")
	}
	if errObj["retryable"] != true {
		t.Errorf("error.retryable = %v, want the caller's value preserved", errObj["retryable"])
	}
}

func TestFailureDropsData(t *testing.T) {
	// Partial data alongside ok:false invites a caller to use it.
	got := decode(t, Envelope{OK: false, Data: []string{"partial"}, Error: &Error{Code: "X", Message: "y"}})

	if got["data"] != nil {
		t.Errorf("data = %#v, want null on a failure", got["data"])
	}
}

func TestSuccessCarriesNoError(t *testing.T) {
	got := decode(t, Envelope{OK: true, Data: []string{}, Error: &Error{Code: "X", Message: "y"}})

	if got["error"] != nil {
		t.Errorf("error = %#v, want null on a success", got["error"])
	}
}

func TestFailureExitCodeIsNeverZero(t *testing.T) {
	// Exit code 0 on a failure is invisible to any caller that checks only
	// the process status.
	if got := failureExitCode(ExitOK); got != ExitError {
		t.Errorf("failureExitCode(ExitOK) = %d, want %d", got, ExitError)
	}
	for _, code := range []int{ExitError, ExitUsage, ExitAuthRequired, ExitNotFound, ExitRateLimited, ExitConfirmMissing} {
		if got := failureExitCode(code); got != code {
			t.Errorf("failureExitCode(%d) = %d, want it unchanged", code, got)
		}
	}
}

func TestSuccessCarriesPageWhenPaged(t *testing.T) {
	got := decode(t, Envelope{
		OK:   true,
		Data: []string{"a"},
		Page: &Page{Returned: 1, Limit: 1, HasMore: true, NextPageToken: "tok-1"},
	})

	page, ok := got["page"].(map[string]any)
	if !ok {
		t.Fatalf("page = %#v, want an object", got["page"])
	}
	if page["has_more"] != true {
		t.Errorf("page.has_more = %v, want true", page["has_more"])
	}
	if page["next_page_token"] != "tok-1" {
		t.Errorf("page.next_page_token = %v, want %q", page["next_page_token"], "tok-1")
	}
}

func TestUnpagedResultOmitsPage(t *testing.T) {
	// Commands that cannot be partial (read, thread) must not grow a page
	// object a caller might read as meaningful.
	var buf bytes.Buffer
	writeJSON(&buf, Envelope{OK: true, Data: map[string]string{"id": "m1"}})

	if strings.Contains(buf.String(), "page") {
		t.Errorf("unpaged envelope mentions page:\n%s", buf.String())
	}
}

func TestPageNoteTellsAHumanHowToContinue(t *testing.T) {
	var buf bytes.Buffer
	writePageNote(&buf, &Page{Returned: 500, HasMore: true, NextPageToken: "tok-2"})

	out := buf.String()
	if !strings.Contains(out, "--page-token tok-2") {
		t.Errorf("page note does not say how to continue:\n%s", out)
	}

	buf.Reset()
	writePageNote(&buf, &Page{Returned: 12})
	if buf.Len() != 0 {
		t.Errorf("complete result printed a page note:\n%s", buf.String())
	}
}
