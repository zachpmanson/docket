// Package cal wraps Google's CalDAV interface, not the Calendar REST v3
// API. See docket-design.md §5 for why: the REST API is disabled on
// Thunderbird's borrowed OAuth project (a Google Cloud project we don't
// own and can't enable services on), while CalDAV is the protocol
// Thunderbird's desktop client actually uses against Google today, so it
// works under the same client and scope with no new registration.
//
// The tradeoff: CalDAV returns raw iCalendar data, so recurring events
// need client-side RRULE expansion (see expand.go) rather than the
// server-side singleEvents=true REST offered. CalDAV also has no
// free-busy-query support, so FreeBusy is derived from expanded events.
package cal

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/emersion/go-webdav/caldav"
)

// caldavBaseURL is Google's fixed CalDAV service root.
// See https://developers.google.com/calendar/caldav/v2/guide.
const caldavBaseURL = "https://apidata.googleusercontent.com"

// PrimaryCalendar is the alias meaning "the authenticated user's own
// calendar". Google's CalDAV interface has no literal "primary" id like
// the REST API does — it uses the account's actual email address — so
// ResolveCalendarID translates this alias before any request.
const PrimaryCalendar = "primary"

// NewClient builds a CalDAV client authenticated with src as an OAuth2
// Bearer token.
func NewClient(ctx context.Context, src oauth2.TokenSource) (*caldav.Client, error) {
	httpClient := oauth2.NewClient(ctx, src)
	c, err := caldav.NewClient(httpClient, caldavBaseURL)
	if err != nil {
		return nil, fmt.Errorf("building caldav client: %w", err)
	}
	return c, nil
}

// ResolveCalendarID translates PrimaryCalendar to accountEmail, passing any
// other id through unchanged.
func ResolveCalendarID(id, accountEmail string) string {
	if id == PrimaryCalendar {
		return accountEmail
	}
	return id
}

func eventsPath(calendarID string) string {
	return fmt.Sprintf("/caldav/v2/%s/events", calendarID)
}
