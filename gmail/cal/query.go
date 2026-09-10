package cal

import (
	"context"
	"fmt"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"
)

// recurrenceQueryPad widens the CalDAV time-range filter past the caller's
// true end. Google's server gates returning a recurring master VEVENT on
// the query end crossing ~08:15 local on the day of the *following*
// occurrence (verified live, see zachpmanson/docket#3): a window ending
// mid-week silently drops the whole series even when an occurrence falls
// inside the window — e.g. a weekly Sunday 18:15 event is invisible to any
// query ending before the next Sunday ~08:15 local, including one ending
// right after Sunday 22:00. Padding by a week guarantees the exact match
// window never decides inclusion for any recurrence period <= 7 days;
// callers trim occurrences back to the true window via expandEvents, which
// uses the caller's own timeMin/timeMax.
const recurrenceQueryPad = 7 * 24 * time.Hour

// queryEvents fetches every VEVENT (raw — recurring events still carry
// their RRULE, not yet expanded into occurrences) whose recurrence set
// overlaps [timeMin, timeMax] on calendarID. The server-side filter end is
// widened by recurrenceQueryPad so Google returns recurring masters whose
// next occurrence falls just past the window (see recurrenceQueryPad);
// callers must still expand/trim to [timeMin, timeMax] themselves.
func queryEvents(ctx context.Context, client *caldav.Client, calendarID string, timeMin, timeMax time.Time) ([]ical.Event, error) {
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:     "VCALENDAR",
			AllProps: true,
			AllComps: true,
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{
				{
					Name:  "VEVENT",
					Start: timeMin,
					End:   timeMax.Add(recurrenceQueryPad),
				},
			},
		},
	}

	objs, err := client.QueryCalendar(ctx, eventsPath(calendarID), query)
	if err != nil {
		return nil, fmt.Errorf(
			"querying calendar %q: %w (calendar ids other than \"primary\" must be the exact "+
				"email address or id Google Calendar uses for that calendar)", calendarID, err)
	}

	var events []ical.Event
	for _, obj := range objs {
		if obj.Data == nil {
			continue
		}
		events = append(events, obj.Data.Events()...)
	}
	return events, nil
}
