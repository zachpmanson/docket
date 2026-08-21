package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"

	"google.golang.org/api/googleapi"

	"github.com/zachpmanson/docket/internal/mail"
	"github.com/zachpmanson/docket/internal/out"
)

// captureStdout runs fn with stdout redirected to a pipe and returns what it
// wrote. The pipe is also what makes the JSON path run: out.Emit/out.Fail pick
// their format from whether stdout is a terminal, and a caller parsing docket
// is never one.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating a pipe: %v", err)
	}
	real := os.Stdout
	os.Stdout = w
	code := fn()
	os.Stdout = real
	w.Close()

	captured, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return string(captured), code
}

// TestAttachmentFailuresAreClassifiable walks every failure the attachment
// path can produce and asserts the envelope a caller parses: a distinct code,
// a non-zero exit, and a retryable flag that is true only where trying again
// can help. A bulk fetcher branches on exactly these, and two outcomes sharing
// a code would have it record a rate limit as a permanent gap.
func TestAttachmentFailuresAreClassifiable(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		code      string
		exit      int
		retryable bool
	}{{
		name: "part id not in the message",
		err:  fmt.Errorf("part %q: %w", "7.4", mail.ErrPartNotFound),
		code: "PART_NOT_FOUND",
		exit: out.ExitNotFound,
	}, {
		name: "part carries no content",
		err:  fmt.Errorf("part %q: %w", "1", mail.ErrAttachmentUnavailable),
		code: "ATTACHMENT_UNAVAILABLE",
		exit: out.ExitNotFound,
	}, {
		name: "over the cap",
		err:  fmt.Errorf("part %q: %w", "1", mail.ErrAttachmentTooLarge),
		code: "ATTACHMENT_TOO_LARGE",
		exit: out.ExitError,
	}, {
		name: "local write failed",
		err:  fmt.Errorf("%w: no such directory", mail.ErrOutputWrite),
		code: "OUTPUT_WRITE_FAILED",
		exit: out.ExitError,
	}, {
		name: "message does not exist",
		err:  fmt.Errorf("fetching message: %w", &googleapi.Error{Code: 404, Message: "Not Found"}),
		code: "MESSAGE_NOT_FOUND",
		exit: out.ExitNotFound,
	}, {
		name:      "rate limited",
		err:       fmt.Errorf("fetching part: %w", &googleapi.Error{Code: 429, Message: "Rate Limit Exceeded"}),
		code:      "RATE_LIMITED",
		exit:      out.ExitRateLimited,
		retryable: true,
	}}

	seen := map[string]string{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, exit := captureStdout(t, func() int { return failMailAttachment(tc.err) })

			var env struct {
				OK    bool `json:"ok"`
				Data  any  `json:"data"`
				Error *struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &env); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
			}

			if env.OK {
				t.Errorf("ok = true on a failure")
			}
			if env.Data != nil {
				t.Errorf("data = %#v on a failure, want null", env.Data)
			}
			if env.Error == nil {
				t.Fatalf("error is null; a caller has nothing to branch on\n%s", stdout)
			}
			if env.Error.Code != tc.code {
				t.Errorf("error.code = %q, want %q", env.Error.Code, tc.code)
			}
			if env.Error.Message == "" {
				t.Errorf("error.message is empty, want the cause")
			}
			if env.Error.Retryable != tc.retryable {
				t.Errorf("error.retryable = %v, want %v", env.Error.Retryable, tc.retryable)
			}
			if exit != tc.exit {
				t.Errorf("exit = %d, want %d", exit, tc.exit)
			}
			if exit == out.ExitOK {
				t.Errorf("exit = 0 on a failure; a caller checking only the status would parse null data as empty")
			}
			if prior, ok := seen[env.Error.Code]; ok {
				t.Errorf("code %q is shared with %q; the two demand different responses", env.Error.Code, prior)
			}
			seen[env.Error.Code] = tc.name
		})
	}
}

// TestAttachmentUsageFailuresAreEnvelopes covers the invocations rejected
// before any API call. A usage error printed as bare text would be
// unparseable to the only kind of caller this command has.
func TestAttachmentUsageFailuresAreEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"no part", []string{"--id", "m1", "--out", "/tmp/x.bin"}},
		{"no out path", []string{"--id", "m1", "--part", "1.2"}},
		{"no id", []string{"--part", "1.2", "--out", "/tmp/x.bin"}},
		{"negative cap", []string{"--id", "m1", "--part", "1.2", "--out", "/tmp/x.bin", "--max-bytes", "-1"}},
		{"unknown flag", []string{"--id", "m1", "--part", "1.2", "--out", "/tmp/x.bin", "--thumbnail"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, exit := captureStdout(t, func() int { return runMail(t.Context(), append([]string{"attachment"}, tc.args...)) })

			var env struct {
				OK    bool `json:"ok"`
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stdout), &env); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
			}
			if env.OK || env.Error == nil {
				t.Fatalf("a rejected invocation did not report an error: %s", stdout)
			}
			if env.Error.Code != "USAGE_ERROR" {
				t.Errorf("error.code = %q, want USAGE_ERROR", env.Error.Code)
			}
			if exit != out.ExitUsage {
				t.Errorf("exit = %d, want %d", exit, out.ExitUsage)
			}
		})
	}
}
