// Package mail wraps the Gmail REST API (not IMAP/SMTP — see
// docket-design.md §4 for why) behind the envelope/message shapes the
// docket CLI hands back to an agent.
package mail

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// NewService builds a Gmail API client authenticated with src.
func NewService(ctx context.Context, src oauth2.TokenSource) (*gmail.Service, error) {
	svc, err := gmail.NewService(ctx, option.WithTokenSource(src))
	if err != nil {
		return nil, fmt.Errorf("building gmail client: %w", err)
	}
	return svc, nil
}

// meUser is the special Gmail API user id meaning "the authenticated user" —
// avoids a separate call to resolve an actual address.
const meUser = "me"
