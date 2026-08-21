package mail

import (
	"context"
	"errors"
	"net"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"

	"github.com/zachpmanson/docket/internal/out"
)

// Failure is an API error classified into the three things a caller acts on:
// a code it can branch on, an exit status, and whether trying again can
// possibly help. A backfill needs "rate limited, back off" to be a different
// answer from "that message is gone" and from "your token is dead"; a single
// GMAIL_API_ERROR with retryable=true says none of those.
type Failure struct {
	Code      string
	Exit      int
	Retryable bool
}

// Error codes Classify can return. They are part of the CLI contract — a
// caller branches on them, so they are stable strings, not derived text.
const (
	CodeRateLimited   = "RATE_LIMITED"
	CodeAuthExpired   = "AUTH_EXPIRED"
	CodeAuthRevoked   = "AUTH_REVOKED"
	CodePermission    = "PERMISSION_DENIED"
	CodeNotFound      = "NOT_FOUND"
	CodeServerError   = "GMAIL_SERVER_ERROR"
	CodeNetworkError  = "NETWORK_ERROR"
	CodeTimeout       = "TIMEOUT"
	CodeGmailAPIError = "GMAIL_API_ERROR"
)

// Classify maps an error from the Gmail client to a Failure.
//
// Retryable is claimed only for causes that are known to be transient. An
// unrecognised error is reported as not retryable: a caller that retries a
// deterministic failure burns quota and eventually gets rate limited for it,
// whereas one that declines to retry a transient failure loses a window it
// can re-run deliberately. Under the opposite default, a malformed query or a
// deleted message reads to a backfill exactly like a rate limit, and it
// retries against the quota that is already the constraint.
func Classify(err error) Failure {
	if err == nil {
		return Failure{Code: CodeGmailAPIError, Exit: out.ExitError}
	}

	// Token refresh happens inside the transport, so a refresh failure
	// surfaces here rather than at login. invalid_grant means the refresh
	// token is dead and only a human re-running `docket auth login` fixes it;
	// anything else (a 5xx from the token endpoint, a network blip) is worth
	// another attempt.
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if strings.Contains(string(retrieve.Body), "invalid_grant") ||
			retrieve.ErrorCode == "invalid_grant" {
			return Failure{Code: CodeAuthRevoked, Exit: out.ExitAuthRequired, Retryable: false}
		}
		return Failure{Code: CodeAuthExpired, Exit: out.ExitAuthRequired, Retryable: true}
	}

	var apiErr *googleapi.Error
	if errors.As(err, &apiErr) {
		return classifyHTTP(apiErr)
	}

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Failure{Code: CodeTimeout, Exit: out.ExitRateLimited, Retryable: true}
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return Failure{Code: CodeNetworkError, Exit: out.ExitRateLimited, Retryable: true}
	}

	return Failure{Code: CodeGmailAPIError, Exit: out.ExitError, Retryable: false}
}

// rateLimitReasons are the googleapi error reasons Gmail returns under a 403
// for quota exhaustion rather than a genuine authorisation problem. The two
// share a status code, so the reason is the only thing separating "wait" from
// "this will never work".
var rateLimitReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"quotaExceeded":         true,
	"dailyLimitExceeded":    true,
	"backendError":          true,
}

func classifyHTTP(err *googleapi.Error) Failure {
	switch {
	case err.Code == 429:
		return Failure{Code: CodeRateLimited, Exit: out.ExitRateLimited, Retryable: true}
	case err.Code == 401:
		return Failure{Code: CodeAuthExpired, Exit: out.ExitAuthRequired, Retryable: true}
	case err.Code == 403 && hasRateLimitReason(err):
		return Failure{Code: CodeRateLimited, Exit: out.ExitRateLimited, Retryable: true}
	case err.Code == 403:
		return Failure{Code: CodePermission, Exit: out.ExitError, Retryable: false}
	case err.Code == 404:
		return Failure{Code: CodeNotFound, Exit: out.ExitNotFound, Retryable: false}
	case err.Code >= 500:
		return Failure{Code: CodeServerError, Exit: out.ExitRateLimited, Retryable: true}
	default:
		return Failure{Code: CodeGmailAPIError, Exit: out.ExitError, Retryable: false}
	}
}

func hasRateLimitReason(err *googleapi.Error) bool {
	for _, e := range err.Errors {
		if rateLimitReasons[e.Reason] {
			return true
		}
	}
	// Gmail does not always populate errors[].reason; the top-level message
	// carries the same wording, so fall back to it rather than mistaking an
	// exhausted quota for a permission failure a caller must not retry.
	msg := strings.ToLower(err.Message)
	return strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota")
}
