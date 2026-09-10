package cal

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-webdav/caldav"
)

// Agenda lists concrete event occurrences on calendarID between timeMin and
// timeMax, with recurring events expanded client-side (see expand.go).
func Agenda(ctx context.Context, client *caldav.Client, calendarID string, timeMin, timeMax time.Time, loc *time.Location) ([]Event, error) {
	raw, err := queryEvents(ctx, client, calendarID, timeMin, timeMax)
	if err != nil {
		return nil, err
	}
	return expandEvents(raw, timeMin, timeMax, loc, calendarID)
}

// Show re-derives a single occurrence from its composite id
// ("<UID>::<occurrence-start-RFC3339>", see Event's doc comment) by
// re-querying a window wide enough to contain any reasonable event
// duration around the embedded timestamp.
func Show(ctx context.Context, client *caldav.Client, calendarID, id string, loc *time.Location) (*Event, error) {
	_, occStart, err := parseEventID(id, loc)
	if err != nil {
		return nil, err
	}

	const window = 25 * time.Hour // covers any single-day event either side of the timestamp
	events, err := Agenda(ctx, client, calendarID, occStart.Add(-window), occStart.Add(window), loc)
	if err != nil {
		return nil, err
	}

	for i := range events {
		if events[i].ID == id {
			return &events[i], nil
		}
	}
	return nil, fmt.Errorf(
		"no event found with id %q on calendar %q — it may have been deleted or moved; "+
			"re-run `cal agenda` to get current ids", id, calendarID)
}

// parseEventID splits a composite "<uid>::<occurrence-start>" id (see
// Event's doc comment) into its parts.
func parseEventID(id string, loc *time.Location) (uid string, occStart time.Time, err error) {
	sep := strings.LastIndex(id, "::")
	if sep < 0 {
		return "", time.Time{}, fmt.Errorf(
			"malformed event id %q — expected \"<uid>::<RFC3339 start time>\", "+
				"the exact id string from `cal agenda`/`cal find-slot` output, not a UID alone", id)
	}
	uid = id[:sep]
	occStart, err = time.Parse(time.RFC3339, id[sep+2:])
	if err != nil {
		occStart, err = time.ParseInLocation("2006-01-02", id[sep+2:], loc)
		if err != nil {
			return "", time.Time{}, fmt.Errorf(
				"malformed event id %q — the portion after \"::\" must be the RFC3339 "+
					"(or, for all-day events, YYYY-MM-DD) timestamp from the original result: %w", id, err)
		}
	}
	return uid, occStart, nil
}
