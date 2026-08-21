package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/zachpmanson/docket/internal/out"
)

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "dial tcp: i/o timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		code      string
		exit      int
		retryable bool
	}{{
		name:      "429 is the rate limit a backfill must back off from",
		err:       &googleapi.Error{Code: 429, Message: "Too Many Requests"},
		code:      CodeRateLimited,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "403 with a quota reason is a rate limit, not a permission failure",
		err:       &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}},
		code:      CodeRateLimited,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "403 with no reason but rate-limit wording is still a rate limit",
		err:       &googleapi.Error{Code: 403, Message: "User-rate limit exceeded"},
		code:      CodeRateLimited,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name: "403 on a missing scope will never succeed on retry",
		err: &googleapi.Error{Code: 403, Message: "Request had insufficient authentication scopes",
			Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}},
		code:      CodePermission,
		exit:      out.ExitError,
		retryable: false,
	}, {
		name:      "401 is worth one more attempt because the transport may refresh",
		err:       &googleapi.Error{Code: 401, Message: "Invalid Credentials"},
		code:      CodeAuthExpired,
		exit:      out.ExitAuthRequired,
		retryable: true,
	}, {
		name:      "404 is a gone message",
		err:       &googleapi.Error{Code: 404, Message: "Not Found"},
		code:      CodeNotFound,
		exit:      out.ExitNotFound,
		retryable: false,
	}, {
		name:      "5xx is transient",
		err:       &googleapi.Error{Code: 503, Message: "Backend Error"},
		code:      CodeServerError,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "a wrapped API error is still classified",
		err:       fmt.Errorf("listing messages: %w", &googleapi.Error{Code: 429}),
		code:      CodeRateLimited,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "a dead refresh token needs a human, not a retry",
		err:       &oauth2.RetrieveError{ErrorCode: "invalid_grant", Body: []byte(`{"error":"invalid_grant"}`)},
		code:      CodeAuthRevoked,
		exit:      out.ExitAuthRequired,
		retryable: false,
	}, {
		name:      "a failed refresh for any other reason is retryable",
		err:       &oauth2.RetrieveError{Body: []byte(`{"error":"temporarily_unavailable"}`)},
		code:      CodeAuthExpired,
		exit:      out.ExitAuthRequired,
		retryable: true,
	}, {
		name:      "a network timeout is transient",
		err:       fmt.Errorf("Get https://gmail.googleapis.com: %w", net.Error(fakeTimeout{})),
		code:      CodeNetworkError,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "a cancelled context is transient",
		err:       fmt.Errorf("fetching message: %w", context.DeadlineExceeded),
		code:      CodeTimeout,
		exit:      out.ExitRateLimited,
		retryable: true,
	}, {
		name:      "an unrecognised error does not claim to be retryable",
		err:       errors.New("something we have never seen"),
		code:      CodeGmailAPIError,
		exit:      out.ExitError,
		retryable: false,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			if got.Code != tt.code || got.Exit != tt.exit || got.Retryable != tt.retryable {
				t.Errorf("Classify() = %+v, want {Code:%s Exit:%d Retryable:%v}",
					got, tt.code, tt.exit, tt.retryable)
			}
		})
	}
}

func TestClassifyNeverReturnsAnEmptyCode(t *testing.T) {
	// The envelope contract requires a populated code on every failure, and
	// Classify is the only thing standing between an API error and that field.
	for _, err := range []error{nil, errors.New("x"), &googleapi.Error{Code: 418}} {
		if got := Classify(err); got.Code == "" {
			t.Errorf("Classify(%v) returned an empty code", err)
		}
	}
}
